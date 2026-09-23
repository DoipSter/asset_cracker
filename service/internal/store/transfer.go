package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Moving simulated money by hand: between the money buckets (common pool, profit pool, the tax
// and fee reserves), out of a strategy's bucket, and to or from the owners outside. Every move is
// one ledger transfer with two entries and a memo, created by the service actor, and reads back
// on the buckets page like every other movement. Nothing here places an order.

// transferable are the account kinds a person may move money between. venue and fees are the
// trading counterparties and are written only by fills, fees and settlements.
var transferable = map[string]bool{"bucket": true, "common_pool": true, "profit_pool": true, "tax_reserve": true, "fee_reserve": true, "external": true}

// SimAccount is one simulated ledger account and its balance now.
type SimAccount struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	BalanceCents int64  `json:"balance_cents"`
	// Frozen is set on a bucket's cash account when the bucket is frozen: nothing to move.
	Frozen bool `json:"frozen,omitempty"`
}

// SimAccounts lists the accounts money may be moved between, pools first, then the buckets that
// are not frozen, then the owners.
func (s *Store) SimAccounts(ctx context.Context) ([]SimAccount, error) {
	rows, err := s.pool.Query(ctx, `
		select a.id, a.kind, a.name,
		       coalesce((select sum(e.amount_cents) from ledger_entry e where e.account_id = a.id), 0) as balance,
		       coalesce(b.status = 'frozen', false) as frozen
		  from ledger_account a
		  left join bucket b on b.ledger_account_id = a.id
		 where a.mode = 'sim' and a.kind in ('common_pool', 'profit_pool', 'tax_reserve', 'fee_reserve', 'bucket', 'external')
		 order by case a.kind when 'common_pool' then 1 when 'profit_pool' then 2 when 'tax_reserve' then 3 when 'fee_reserve' then 4 when 'bucket' then 5 else 6 end, a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SimAccount
	for rows.Next() {
		var a SimAccount
		if err := rows.Scan(&a.ID, &a.Kind, &a.Name, &a.BalanceCents, &a.Frozen); err != nil {
			return nil, err
		}
		if a.Kind == "bucket" && a.Frozen {
			continue // reaped already; nothing to move
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ManualTransfer is what the operator asks for.
type ManualTransfer struct {
	From, To int64
	Cents    int64
	Memo     string
}

// Transferred is what was booked.
type Transferred struct {
	ID         int64  `json:"id"`
	Reason     string `json:"reason"`
	From, To   string `json:"-"`
	FromName   string `json:"from"`
	ToName     string `json:"to"`
	Cents      int64  `json:"cents"`
	FromBucket bool   `json:"from_bucket"` // the engine's copy of that bucket's cash is stale until a reload
}

// TransferRefused is a move the rules do not allow; its text is shown to the operator.
type TransferRefused struct{ Why string }

func (e TransferRefused) Error() string { return e.Why }

// Transfer books one move. The rules: a positive amount; two different accounts of the kinds
// above; never INTO a strategy's bucket (the allocator would read the deposit as a gain, and a
// fresh stake is what Reap and restake is for); and the source must hold the amount, except the
// owners' account, which is the outside world and may go further negative. The reason is read
// off the two kinds so the ledger stays sortable: deposit and withdrawal for the owners' side,
// reap for bucket → common pool, take and expansion between the common and profit pools,
// adjustment for the rest (a hand move to the tax reserve among them: 'tax' is retired as a
// reason, see migration 0009). The memo carries the intent, and the account names.
func (s *Store) Transfer(ctx context.Context, t ManualTransfer) (Transferred, error) {
	var out Transferred
	t.Memo = strings.TrimSpace(t.Memo)
	switch {
	case t.Cents <= 0:
		return out, TransferRefused{"The amount must be above zero."}
	case t.Cents > 100_000_000_00:
		return out, TransferRefused{"That is more than the simulation moves in one transfer."}
	case t.From == t.To:
		return out, TransferRefused{"Pick two different accounts."}
	case len(t.Memo) > 300:
		return out, TransferRefused{"The memo is too long (300 characters)."}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	type acct struct {
		kind, name string
		balance    int64
		frozen     bool
	}
	read := func(id int64) (acct, error) {
		var a acct
		err := tx.QueryRow(ctx, `
			select a.kind, a.name,
			       coalesce((select sum(e.amount_cents) from ledger_entry e where e.account_id = a.id), 0),
			       coalesce(b.status = 'frozen', false)
			  from ledger_account a left join bucket b on b.ledger_account_id = a.id
			 where a.id = $1 and a.mode = 'sim'`, id).Scan(&a.kind, &a.name, &a.balance, &a.frozen)
		if errors.Is(err, pgx.ErrNoRows) {
			return a, TransferRefused{fmt.Sprintf("Account %d is not a simulated account.", id)}
		}
		return a, err
	}
	from, err := read(t.From)
	if err != nil {
		return out, err
	}
	to, err := read(t.To)
	if err != nil {
		return out, err
	}
	switch {
	case !transferable[from.kind] || !transferable[to.kind]:
		return out, TransferRefused{"Money moves between the pools, the reserves, the buckets and the owners; the venue and fee accounts are written only by trading."}
	case to.kind == "bucket":
		return out, TransferRefused{"Money is not moved into a bucket by hand: the allocator would read it as a gain. Reap and restake seeds a fresh bucket from the common pool."}
	case from.kind == "bucket" && from.frozen:
		return out, TransferRefused{from.name + " is frozen; it was reaped when it closed."}
	case from.kind != "external" && from.balance < t.Cents:
		return out, TransferRefused{fmt.Sprintf("%s holds $%.2f; the transfer asks for $%.2f.", from.name, float64(from.balance)/100, float64(t.Cents)/100)}
	}
	out.Reason = transferReason(from.kind, to.kind)
	out.FromName, out.ToName, out.Cents, out.FromBucket = from.name, to.name, t.Cents, from.kind == "bucket"
	memo := t.Memo
	if memo == "" {
		memo = "moved by the operator"
	}
	memo = fmt.Sprintf("%s: %s → %s", memo, from.name, to.name)
	var actor int64
	if err := tx.QueryRow(ctx, `select id from actor where handle = 'service'`).Scan(&actor); err != nil {
		return out, fmt.Errorf("actor 'service': %w", err)
	}
	if err := tx.QueryRow(ctx, `insert into ledger_transfer (mode, reason, memo, created_by) values ('sim', $1, $2, $3) returning id`,
		out.Reason, memo, actor).Scan(&out.ID); err != nil {
		return out, err
	}
	if _, err := tx.Exec(ctx, `insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values ($1, $2, 'sim', $3), ($1, $4, 'sim', $5)`,
		out.ID, t.From, -t.Cents, t.To, t.Cents); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

// transferReason names a move by the kinds it joins, within the ledger's fixed list.
func transferReason(from, to string) string {
	switch {
	case from == "external":
		return "deposit"
	case to == "external":
		return "withdrawal"
	case from == "bucket" && to == "common_pool":
		return "reap"
	case from == "common_pool" && to == "profit_pool":
		return "take"
	case from == "profit_pool" && to == "common_pool":
		return "expansion"
	}
	return "adjustment"
}
