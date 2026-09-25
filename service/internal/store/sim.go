package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Everything in this file writes SIMULATED money. Every ledger account it touches is created
// with mode 'sim', and the database refuses any transfer that would mix sim with real.

// SimBucket is one strategy's bucket, as the runner needs it.
type SimBucket struct {
	ID              int64
	Name            string
	LedgerAccountID int64
	VersionID       int64
	CashCents       int64
	Frozen          bool
}

// SimSetup is the simulated world for one series.
type SimSetup struct {
	ActorID        int64
	VenueLedgerID  int64
	FeesLedgerID   int64
	PoolLedgerID   int64 // replenishment (ledger kind common_pool)
	OwnersLedgerID int64
	WinningsID     int64 // the skim that is kept (ledger kind profit_pool)
	TaxReserveID   int64
	FeeReserveID   int64
	Buckets        map[string]SimBucket // by strategy name
	venueAccountID int64
}

// EnsureSimSetup creates, once, the sim ledger accounts, the paper venue account, and one
// bucket per strategy seeded with seedCents from the common pool. It is safe to call on every
// start: what exists is left alone. A bucket is never topped up here.
func (s *Store) EnsureSimSetup(ctx context.Context, prefix, family string, version int, strategies []string, seedCents int64) (SimSetup, error) {
	out := SimSetup{Buckets: map[string]SimBucket{}}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)

	if err := tx.QueryRow(ctx, `select id from actor where handle = 'service'`).Scan(&out.ActorID); err != nil {
		return out, fmt.Errorf("actor 'service': %w", err)
	}
	account := func(kind, name string) (int64, error) {
		var id int64
		_, err := tx.Exec(ctx, `insert into ledger_account (kind, mode, name) values ($1, 'sim', $2)
		                        on conflict (mode, name) do nothing`, kind, name)
		if err != nil {
			return 0, err
		}
		err = tx.QueryRow(ctx, `select id from ledger_account where mode = 'sim' and name = $1`, name).Scan(&id)
		return id, err
	}
	owners, err := account("external", "owners (sim)")
	if err != nil {
		return out, err
	}
	pool, err := account("common_pool", "common pool (sim)")
	if err != nil {
		return out, err
	}
	if out.WinningsID, err = account("profit_pool", "profit pool (sim)"); err != nil {
		return out, err
	}
	if out.TaxReserveID, err = account("tax_reserve", "tax reserve (sim)"); err != nil {
		return out, err
	}
	if out.FeeReserveID, err = account("fee_reserve", "fee reserve (sim)"); err != nil {
		return out, err
	}
	if out.VenueLedgerID, err = account("venue", "kalshi paper venue"); err != nil {
		return out, err
	}
	if out.FeesLedgerID, err = account("fees", "kalshi fees (sim)"); err != nil {
		return out, err
	}

	var venueAccount int64
	_, err = tx.Exec(ctx, `insert into venue_account (source_id, mode, name)
	                       select id, 'sim', 'kalshi paper' from source where code = 'kalshi'
	                       on conflict (source_id, mode, name) do nothing`)
	if err != nil {
		return out, err
	}
	if err = tx.QueryRow(ctx, `select v.id from venue_account v join source s on s.id = v.source_id
	                            where s.code = 'kalshi' and v.mode = 'sim' and v.name = 'kalshi paper'`).Scan(&venueAccount); err != nil {
		return out, err
	}

	transfer := func(reason, memo string, from, to, cents int64) error {
		var id int64
		if err := tx.QueryRow(ctx, `insert into ledger_transfer (mode, reason, memo, created_by)
		                            values ('sim', $1, $2, $3) returning id`, reason, memo, out.ActorID).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into ledger_entry (transfer_id, account_id, mode, amount_cents)
		                        values ($1, $2, 'sim', $3), ($1, $4, 'sim', $5)`, id, from, -cents, to, cents)
		return err
	}

	for _, name := range strategies {
		bucketName := fmt.Sprintf("%s %s v%d", prefix, name, version)
		var b SimBucket
		// The newest bucket of that name: a strategy that ran out is frozen and replaced by
		// "<name> life N", and the replacement is the one that trades.
		// Its name is kept as the database has it, life and all: the value snapshots are keyed by
		// it, and a second life written under the first life's name would join a dead bucket's
		// history to its replacement's in a table that cannot be corrected.
		err := tx.QueryRow(ctx, `select id, name, ledger_account_id, strategy_version_id, status = 'frozen' from bucket
		                          where name = $1 or name like $1 || ' life %' order by id desc limit 1`, bucketName).
			Scan(&b.ID, &b.Name, &b.LedgerAccountID, &b.VersionID, &b.Frozen)
		if errors.Is(err, pgx.ErrNoRows) {
			b.Name = bucketName
			if err = tx.QueryRow(ctx, `select v.id from strategy_version v join strategy st on st.id = v.strategy_id
			                            where st.family = $1 and st.name = $2 and v.version = $3`, family, name, version).Scan(&b.VersionID); err != nil {
				return out, fmt.Errorf("strategy %s/%s v%d is not registered: %w", family, name, version, err)
			}
			if b.LedgerAccountID, err = account("bucket", bucketName+" cash"); err != nil {
				return out, err
			}
			if err = tx.QueryRow(ctx, `insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
			                           values ($1, 'sim', $2, $3, $4, '{}', 0) returning id`,
				bucketName, venueAccount, b.LedgerAccountID, b.VersionID).Scan(&b.ID); err != nil {
				return out, err
			}
			if err = transfer("deposit", "sim funds for "+bucketName, owners, pool, seedCents); err != nil {
				return out, err
			}
			if err = transfer("seed", "seed "+bucketName, pool, b.LedgerAccountID, seedCents); err != nil {
				return out, err
			}
			if _, err = tx.Exec(ctx, `insert into bucket_event (bucket_id, kind, detail, actor_id)
			                          values ($1, 'seeded', jsonb_build_object('cents', $2::bigint), $3)`, b.ID, seedCents, out.ActorID); err != nil {
				return out, err
			}
		} else if err != nil {
			return out, err
		}
		if err = tx.QueryRow(ctx, `select coalesce(sum(amount_cents), 0) from ledger_entry where account_id = $1`, b.LedgerAccountID).Scan(&b.CashCents); err != nil {
			return out, err
		}
		out.Buckets[name] = b
	}
	out.PoolLedgerID, out.OwnersLedgerID, out.venueAccountID = pool, owners, venueAccount
	return out, tx.Commit(ctx)
}

// CloseBucket is what happens when a strategy runs out: whatever cash is left is reaped into the
// common pool and the bucket is frozen, never topped up. If restake is set, a NEW bucket for the
// same strategy version takes its place with a fresh seed, named "<name> life N"; the frozen one
// keeps its whole record. Returns the bucket now in that slot.
func (s *Store) CloseBucket(ctx context.Context, setup SimSetup, b SimBucket, reason string, restake bool, life int, seedCents int64) (SimBucket, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return b, err
	}
	defer tx.Rollback(ctx)
	transfer := func(reason, memo string, from, to, cents int64) error {
		var id int64
		if err := tx.QueryRow(ctx, `insert into ledger_transfer (mode, reason, memo, created_by) values ('sim', $1, $2, $3) returning id`,
			reason, memo, setup.ActorID).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values ($1, $2, 'sim', $3), ($1, $4, 'sim', $5)`,
			id, from, -cents, to, cents)
		return err
	}
	event := func(bucket int64, kind, detail string) error {
		_, err := tx.Exec(ctx, `insert into bucket_event (bucket_id, kind, detail, actor_id) values ($1, $2, jsonb_build_object('note', $3::text), $4)`,
			bucket, kind, detail, setup.ActorID)
		return err
	}
	var left int64
	if err := tx.QueryRow(ctx, `select coalesce(sum(amount_cents), 0) from ledger_entry where account_id = $1`, b.LedgerAccountID).Scan(&left); err != nil {
		return b, err
	}
	if err := event(b.ID, "tripped", reason); err != nil {
		return b, err
	}
	if left > 0 {
		if err := transfer("reap", "reap "+b.Name, b.LedgerAccountID, setup.PoolLedgerID, left); err != nil {
			return b, err
		}
		if err := event(b.ID, "reaped", fmt.Sprintf("%d cents to the common pool", left)); err != nil {
			return b, err
		}
	}
	if _, err := tx.Exec(ctx, `update bucket set status = 'frozen', tripped_at = now(), frozen_at = now(), trip_reason = $2 where id = $1`, b.ID, reason); err != nil {
		return b, err
	}
	if err := event(b.ID, "frozen", reason); err != nil {
		return b, err
	}
	next := b
	next.Frozen, next.CashCents = true, 0
	if restake {
		if next, err = seedLife(ctx, tx, setup, b, life, seedCents, seedPoolThenOwners); err != nil {
			return b, err
		}
	}
	return next, tx.Commit(ctx)
}

// Where a seed is drawn from. The page names two: replenishment (the common pool) and the bank
// (the owners, outside). The engine's own restake has a third way, the pool with the owners
// covering a shortfall, which is what the pool is for when a bucket runs out on its own.
const (
	SeedFromReplenishment = "replenishment"
	SeedFromBank          = "bank"
	seedPoolThenOwners    = "pool-then-owners" // not a choice on the page
)

// SeedSources are the choices the page offers, in order.
var SeedSources = []string{SeedFromReplenishment, SeedFromBank}

// fundSeed books the cash of one seed into a bucket's account, by source, and says in the memo
// where it came from: the memo is what the bucket list reads the source back from (Buckets).
// Replenishment alone is refused when it is short; the bank puts the whole seed into the pool
// first, so the pool's books still show every dollar that ever went into a bucket passing through.
func fundSeed(ctx context.Context, tx pgx.Tx, setup SimSetup, name string, account, seedCents int64, source string) error {
	transfer := func(reason, memo string, from, to, cents int64) error {
		var id int64
		if err := tx.QueryRow(ctx, `insert into ledger_transfer (mode, reason, memo, created_by) values ('sim', $1, $2, $3) returning id`,
			reason, memo, setup.ActorID).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values ($1, $2, 'sim', $3), ($1, $4, 'sim', $5)`,
			id, from, -cents, to, cents)
		return err
	}
	var inPool int64
	if err := tx.QueryRow(ctx, `select coalesce(sum(amount_cents), 0) from ledger_entry where account_id = $1`, setup.PoolLedgerID).Scan(&inPool); err != nil {
		return err
	}
	switch source {
	case SeedFromBank:
		if err := transfer("deposit", "bank funds for "+name, setup.OwnersLedgerID, setup.PoolLedgerID, seedCents); err != nil {
			return err
		}
		return transfer("seed", "seed "+name+" from the bank", setup.PoolLedgerID, account, seedCents)
	case SeedFromReplenishment:
		if inPool < seedCents {
			return DeployRefused{fmt.Sprintf("Replenishment holds $%.2f; the deploy asks for $%.2f. Pull from the bank instead, or deploy less.", float64(inPool)/100, float64(seedCents)/100)}
		}
		return transfer("seed", "seed "+name+" from replenishment", setup.PoolLedgerID, account, seedCents)
	case seedPoolThenOwners:
		if short := seedCents - inPool; short > 0 {
			if err := transfer("deposit", fmt.Sprintf("replenishment short by %d cents for %s", short, name), setup.OwnersLedgerID, setup.PoolLedgerID, short); err != nil {
				return err
			}
		}
		return transfer("seed", "seed "+name+" from replenishment", setup.PoolLedgerID, account, seedCents)
	}
	return DeployRefused{"Pull the seed from replenishment or from the bank."}
}

// seedLife opens "<base> life N" for the same version as the frozen bucket `prev`, seeded by
// `source` (fundSeed), and marks prev as replaced by it. The engine's own restake draws on the
// pool with the owners covering a shortfall: restarting a dead bucket is what the pool is for,
// and only what it cannot cover is brought in from outside, recorded as its own deposit. Inside
// the caller's transaction.
func seedLife(ctx context.Context, tx pgx.Tx, setup SimSetup, prev SimBucket, life int, seedCents int64, source string) (SimBucket, error) {
	base := prev.Name
	if i := strings.Index(base, " life "); i >= 0 {
		base = base[:i]
	}
	next := SimBucket{Name: fmt.Sprintf("%s life %d", base, life), VersionID: prev.VersionID, CashCents: seedCents}
	if err := tx.QueryRow(ctx, `insert into ledger_account (kind, mode, name) values ('bucket', 'sim', $1) returning id`, next.Name+" cash").Scan(&next.LedgerAccountID); err != nil {
		return next, err
	}
	if err := tx.QueryRow(ctx, `insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
	                           values ($1, 'sim', $2, $3, $4, '{}', 0) returning id`, next.Name, setup.venueAccountID, next.LedgerAccountID, next.VersionID).Scan(&next.ID); err != nil {
		return next, err
	}
	if err := fundSeed(ctx, tx, setup, next.Name, next.LedgerAccountID, seedCents, source); err != nil {
		return next, err
	}
	if _, err := tx.Exec(ctx, `insert into bucket_event (bucket_id, kind, detail, actor_id) values ($1, 'seeded', jsonb_build_object('note', $2::text), $3)`,
		next.ID, fmt.Sprintf("replaces %s", prev.Name), setup.ActorID); err != nil {
		return next, err
	}
	if _, err := tx.Exec(ctx, `update bucket set replaced_by_bucket_id = $2 where id = $1`, prev.ID, next.ID); err != nil {
		return next, err
	}
	return next, nil
}

// Deploy is what the operator asks for on the buckets page: a bucket for a version, seeded
// with SeedCents drawn from Source.
type Deploy struct {
	VersionID int64
	SeedCents int64
	Source    string // SeedFromReplenishment or SeedFromBank
}

// DeployRefused is a deploy the rules do not allow; its text is shown to the operator.
type DeployRefused struct{ Why string }

func (e DeployRefused) Error() string { return e.Why }

// ErrOtherFamily is a deploy of a version that belongs to another market family, so another
// runner's. app asks each runner in turn; the one whose family it is answers.
var ErrOtherFamily = errors.New("that version belongs to another market family")

// DeployBucket opens a bucket for a version by the operator's hand: the first life, named as
// EnsureSimSetup names it, or the next life of a version whose newest bucket is frozen. The seed
// is the amount asked for, from the source asked for, and a draft or retired version is put on
// probation in the same transaction, so the reload that follows finds a bucket already seeded
// and a version that may order; EnsureSimSetup, finding the bucket, seeds nothing more. Refused
// while the version holds a bucket (close it first), and when replenishment is asked for more
// than it has.
func (s *Store) DeployBucket(ctx context.Context, setup SimSetup, prefix, family string, version int, d Deploy) (SimBucket, error) {
	switch {
	case d.SeedCents <= 0:
		return SimBucket{}, DeployRefused{"The seed must be above zero."}
	case d.SeedCents > 100_000_000:
		return SimBucket{}, DeployRefused{"A seed is at most $1,000,000."}
	case d.Source != SeedFromReplenishment && d.Source != SeedFromBank:
		return SimBucket{}, DeployRefused{"Pull the seed from replenishment or from the bank."}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SimBucket{}, err
	}
	defer tx.Rollback(ctx)
	var name, vFamily, status string
	var vVersion int
	err = tx.QueryRow(ctx, `select st.name, st.family, v.version, v.status from strategy_version v join strategy st on st.id = v.strategy_id where v.id = $1`, d.VersionID).
		Scan(&name, &vFamily, &vVersion, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return SimBucket{}, ErrVersionNotFound
	}
	if err != nil {
		return SimBucket{}, err
	}
	if vFamily != family || vVersion != version {
		return SimBucket{}, ErrOtherFamily
	}
	var prev SimBucket
	err = tx.QueryRow(ctx, `select id, name, ledger_account_id, strategy_version_id, status = 'frozen' from bucket
	                          where strategy_version_id = $1 and mode = 'sim' order by id desc limit 1`, d.VersionID).
		Scan(&prev.ID, &prev.Name, &prev.LedgerAccountID, &prev.VersionID, &prev.Frozen)
	var next SimBucket
	switch {
	case err == nil && !prev.Frozen:
		return SimBucket{}, ErrBucketHeld
	case err == nil:
		if next, err = seedLife(ctx, tx, setup, prev, LifeOf(prev.Name)+1, d.SeedCents, d.Source); err != nil {
			return SimBucket{}, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		next = SimBucket{Name: fmt.Sprintf("%s %s v%d", prefix, name, version), VersionID: d.VersionID, CashCents: d.SeedCents}
		if _, err = tx.Exec(ctx, `insert into ledger_account (kind, mode, name) values ('bucket', 'sim', $1) on conflict (mode, name) do nothing`, next.Name+" cash"); err != nil {
			return SimBucket{}, err
		}
		if err = tx.QueryRow(ctx, `select id from ledger_account where mode = 'sim' and name = $1`, next.Name+" cash").Scan(&next.LedgerAccountID); err != nil {
			return SimBucket{}, err
		}
		if err = tx.QueryRow(ctx, `insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
		                           values ($1, 'sim', $2, $3, $4, '{}', 0) returning id`, next.Name, setup.venueAccountID, next.LedgerAccountID, next.VersionID).Scan(&next.ID); err != nil {
			return SimBucket{}, err
		}
		if err = fundSeed(ctx, tx, setup, next.Name, next.LedgerAccountID, d.SeedCents, d.Source); err != nil {
			return SimBucket{}, err
		}
		if _, err = tx.Exec(ctx, `insert into bucket_event (bucket_id, kind, detail, actor_id) values ($1, 'seeded', jsonb_build_object('cents', $2::bigint, 'source', $3::text), $4)`,
			next.ID, d.SeedCents, d.Source, setup.ActorID); err != nil {
			return SimBucket{}, err
		}
	default:
		return SimBucket{}, err
	}
	if status == "draft" || status == "retired" {
		if _, err = tx.Exec(ctx, `update strategy_version set status = 'probation', retired_at = null, retired_reason = '' where id = $1`, d.VersionID); err != nil {
			return SimBucket{}, err
		}
	}
	return next, tx.Commit(ctx)
}

// ErrBucketHeld is a RestakeBucket of a version whose newest bucket is not frozen.
var ErrBucketHeld = errors.New("that version's bucket is still held; reap it first")

// ErrNoBucketEver is a RestakeBucket of a version that never had a bucket: approval seeds those.
var ErrNoBucketEver = errors.New("that version has never had a bucket; approving it seeds one")

// ErrNoSuchBucket is a close-out of an id that is not a simulated bucket.
var ErrNoSuchBucket = errors.New("no such simulated bucket")

// ErrAlreadyClosed is a close-out of a bucket that is already frozen. Its record is kept either way.
var ErrAlreadyClosed = errors.New("that bucket is already closed")

// OpenContracts is a close-out refused because the bucket still holds contracts. A settlement
// into a frozen bucket has nowhere to go.
type OpenContracts struct {
	Name string
	Lots int
}

func (e OpenContracts) Error() string {
	return fmt.Sprintf("%s still holds %d contract(s); close it once they settle", e.Name, e.Lots)
}

// BucketRef is enough to decide how a bucket is closed: the live engine's own reap when it is
// version 3 and the engine holds it, otherwise a close-out in the ledger alone.
type BucketRef struct {
	ID, VersionID int64
	Version       int
	Name, Status  string
}

// ClosedOut is a bucket taken off the books without a reset: frozen, its cash in replenishment,
// its bets and fills and decisions still there.
type ClosedOut struct {
	Name        string
	ReapedCents int64
}

// LookupBucket is the simulated bucket of that id.
func (s *Store) LookupBucket(ctx context.Context, bucketID int64) (BucketRef, error) {
	var b BucketRef
	err := s.pool.QueryRow(ctx, `select b.id, b.strategy_version_id, v.version, b.name, b.status
	                               from bucket b join strategy_version v on v.id = b.strategy_version_id
	                              where b.id = $1 and b.mode = 'sim'`, bucketID).
		Scan(&b.ID, &b.VersionID, &b.Version, &b.Name, &b.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return BucketRef{}, ErrNoSuchBucket
	}
	return b, err
}

// CloseOutBucket reaps one simulated bucket into replenishment and freezes it. Nothing it did is
// deleted, and nothing is seeded in its place. It is how an old strategy leaves the books without
// a reset. Refused while it still holds contracts, and when it is already frozen.
func (s *Store) CloseOutBucket(ctx context.Context, bucketID int64) (ClosedOut, error) {
	b, err := s.LookupBucket(ctx, bucketID)
	if err != nil {
		return ClosedOut{}, err
	}
	if b.Status == "frozen" {
		return ClosedOut{}, ErrAlreadyClosed
	}
	var lots int
	// A scalar subquery: no open side is NULL, and coalesce makes that zero. Summing the outer
	// query would return no row at all when nothing is open.
	err = s.pool.QueryRow(ctx, `
		select coalesce((
		    select sum(q) from (
		        select sum(case when o.action = 'buy' then f.qty else -f.qty end) q
		          from trade_order o join fill f on f.order_id = o.id
		         where o.bucket_id = $1
		         group by o.side
		        having sum(case when o.action = 'buy' then f.qty else -f.qty end) > 0
		    ) open_sides
		), 0)::int`, bucketID).Scan(&lots)
	if err != nil {
		return ClosedOut{}, err
	}
	if lots > 0 {
		return ClosedOut{}, OpenContracts{Name: b.Name, Lots: lots}
	}
	var actor, pool, ledger, cash int64
	if err = s.pool.QueryRow(ctx, `select id from actor where handle = 'service'`).Scan(&actor); err != nil {
		return ClosedOut{}, err
	}
	if err = s.pool.QueryRow(ctx, `select id from ledger_account where mode = 'sim' and name = 'common pool (sim)'`).Scan(&pool); err != nil {
		return ClosedOut{}, fmt.Errorf("replenishment pool: %w", err)
	}
	if err = s.pool.QueryRow(ctx, `select ledger_account_id from bucket where id = $1`, b.ID).Scan(&ledger); err != nil {
		return ClosedOut{}, err
	}
	if err = s.pool.QueryRow(ctx, `select coalesce(sum(amount_cents), 0) from ledger_entry where account_id = $1`, ledger).Scan(&cash); err != nil {
		return ClosedOut{}, err
	}
	held := SimBucket{ID: b.ID, Name: b.Name, LedgerAccountID: ledger, VersionID: b.VersionID}
	if _, err = s.CloseBucket(ctx, SimSetup{ActorID: actor, PoolLedgerID: pool}, held, "closed out by the operator", false, 0, 0); err != nil {
		return ClosedOut{}, err
	}
	return ClosedOut{Name: b.Name, ReapedCents: cash}, nil
}

// RestakeBucket opens the next life of a version whose newest bucket is frozen (reaped by hand,
// or run out), seeded from the replenishment pool. It is how a version comes back after a Reap
// without a restake. The bucket trades if the version is approved, else it is held settle-only.
func (s *Store) RestakeBucket(ctx context.Context, setup SimSetup, versionID int64, seedCents int64) (SimBucket, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SimBucket{}, err
	}
	defer tx.Rollback(ctx)
	var prev SimBucket
	err = tx.QueryRow(ctx, `select id, name, ledger_account_id, strategy_version_id, status = 'frozen' from bucket
	                          where strategy_version_id = $1 and mode = 'sim' order by id desc limit 1`, versionID).
		Scan(&prev.ID, &prev.Name, &prev.LedgerAccountID, &prev.VersionID, &prev.Frozen)
	if errors.Is(err, pgx.ErrNoRows) {
		return SimBucket{}, ErrNoBucketEver
	}
	if err != nil {
		return SimBucket{}, err
	}
	if !prev.Frozen {
		return SimBucket{}, ErrBucketHeld
	}
	life := 1
	if i := strings.LastIndex(prev.Name, " life "); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(prev.Name[i+len(" life "):])); err == nil && n > 0 {
			life = n
		}
	}
	next, err := seedLife(ctx, tx, setup, prev, life+1, seedCents, seedPoolThenOwners)
	if err != nil {
		return SimBucket{}, err
	}
	return next, tx.Commit(ctx)
}

// DecisionRow is one strategy's conclusion from one look at the market.
type DecisionRow struct {
	BucketID, VersionID          int64
	ModelProb, MarketProb, Edge  float64
	Side, Action, BlockedBy, Why string
	SizeAlone                    *int
	// NoModelProb and NoMarketProb say the engine formed no such figure (no model view; no
	// two-sided book), and the third engine's insert stores NULL for it rather than a 0 the
	// scorecard would read as a price. The frozen engines never set them.
	NoModelProb, NoMarketProb bool
}

// probOrNull is a decision's probability as stored: NULL when the engine formed none.
func probOrNull(missing bool, p float64) any {
	if missing {
		return nil
	}
	return p
}

// TradeRow is a simulated fill: a buy that opened a bet, or a sell that closed one early.
type TradeRow struct {
	DecisionIndex  int // index into StepRecord.Decisions, or -1
	BucketID       int64
	BucketLedgerID int64
	Action         string // buy or sell
	Side           string // yes or no
	Qty            int
	Price          float64
	FeeCents       int64
	CashCents      int64 // what leaves the bucket on a buy, or arrives on a sell; fee included
	Detail         any
}

// StepRecord is everything one look at one market produced.
type StepRecord struct {
	EvaluationID    int64 // if set, decisions hang off this existing row and none is inserted
	At              time.Time
	MarketID        int64
	UnderlyingPrice string
	Quotes, Model   any
	Decisions       []DecisionRow
	Trades          []TradeRow
}

// RecordStep journals a step and books its trades, all or nothing.
func (s *Store) RecordStep(ctx context.Context, setup SimSetup, r StepRecord) error {
	quotes, err := json.Marshal(r.Quotes)
	if err != nil {
		return err
	}
	model, err := json.Marshal(r.Model)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	evalID := r.EvaluationID
	if evalID == 0 {
		if err := tx.QueryRow(ctx, `insert into evaluation (at, market_id, underlying_price, quotes, model)
		                            values ($1, $2, nullif($3, '')::numeric, $4, $5) returning id`,
			r.At, r.MarketID, r.UnderlyingPrice, quotes, model).Scan(&evalID); err != nil {
			return fmt.Errorf("evaluation: %w", err)
		}
	}
	decisionIDs := make([]int64, len(r.Decisions))
	for i, d := range r.Decisions {
		// Human weights are not applied yet, so the size after weighting is the strategy's own.
		if err := tx.QueryRow(ctx, `insert into decision (at, evaluation_id, bucket_id, strategy_version_id, model_prob,
		            market_prob, side, edge, action, blocked_by, reason, size_alone, human_weight, size_applied)
		        values ($1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, ''), $11, $12, 1, $12) returning id`,
			r.At, evalID, d.BucketID, d.VersionID, d.ModelProb, d.MarketProb, d.Side, d.Edge, d.Action,
			d.BlockedBy, d.Why, d.SizeAlone).Scan(&decisionIDs[i]); err != nil {
			return fmt.Errorf("decision: %w", err)
		}
	}
	for _, t := range r.Trades {
		premium := t.CashCents - t.FeeCents // buy: paid to the venue. sell: see below
		bucket, venue := -t.CashCents, premium
		if t.Action == "sell" {
			// The venue pays price x qty; the fee comes out of that; the bucket gets the rest.
			bucket, venue = t.CashCents, -(t.CashCents + t.FeeCents)
		}
		var transferID int64
		if err := tx.QueryRow(ctx, `insert into ledger_transfer (at, mode, reason, memo, created_by)
		                            values ($1, 'sim', 'fill', $2, $3) returning id`,
			r.At, fmt.Sprintf("%s %d %s @ %.4f", t.Action, t.Qty, t.Side, t.Price), setup.ActorID).Scan(&transferID); err != nil {
			return err
		}
		batch := &pgx.Batch{}
		add := func(account, cents int64) {
			if cents != 0 {
				batch.Queue(`insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values ($1, $2, 'sim', $3)`, transferID, account, cents)
			}
		}
		add(t.BucketLedgerID, bucket)
		add(setup.VenueLedgerID, venue)
		add(setup.FeesLedgerID, t.FeeCents)
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("ledger entries: %w", err)
		}
		detail, _ := json.Marshal(t.Detail)
		var decisionID *int64
		var decisionAt *time.Time
		if t.DecisionIndex >= 0 {
			decisionID, decisionAt = &decisionIDs[t.DecisionIndex], &r.At
		}
		var orderID int64
		if err := tx.QueryRow(ctx, `insert into trade_order (bucket_id, market_id, decision_id, decision_at, broker, action, side,
		            qty, limit_price, status, placed_at, detail)
		        values ($1, $2, $3, $4, 'paper', $5, $6, $7, $8, 'filled', $9, $10) returning id`,
			t.BucketID, r.MarketID, decisionID, decisionAt, t.Action, t.Side, t.Qty, t.Price, r.At, detail).Scan(&orderID); err != nil {
			return fmt.Errorf("order: %w", err)
		}
		if _, err := tx.Exec(ctx, `insert into fill (order_id, at, qty, price, fee_cents, transfer_id) values ($1, $2, $3, $4, $5, $6)`,
			orderID, r.At, t.Qty, t.Price, t.FeeCents, transferID); err != nil {
			return fmt.Errorf("fill: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// SettlementRow is what one bucket held on one side of a settled market, and what it was paid.
type SettlementRow struct {
	BucketID, BucketLedgerID int64
	Side                     string
	Qty                      int
	PayoutCents              int64
}

// RecordSettlements books the payouts for a settled market, all or nothing.
func (s *Store) RecordSettlements(ctx context.Context, setup SimSetup, marketID int64, at time.Time, rows []SettlementRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, r := range rows {
		var transferID *int64
		if r.PayoutCents > 0 {
			var id int64
			if err := tx.QueryRow(ctx, `insert into ledger_transfer (at, mode, reason, memo, created_by)
			                            values ($1, 'sim', 'settlement', $2, $3) returning id`,
				at, fmt.Sprintf("%d %s settled", r.Qty, r.Side), setup.ActorID).Scan(&id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `insert into ledger_entry (transfer_id, account_id, mode, amount_cents)
			                           values ($1, $2, 'sim', $3), ($1, $4, 'sim', $5)`,
				id, setup.VenueLedgerID, -r.PayoutCents, r.BucketLedgerID, r.PayoutCents); err != nil {
				return err
			}
			transferID = &id
		}
		if _, err := tx.Exec(ctx, `insert into settlement (market_id, bucket_id, at, side, qty, payout_cents, transfer_id)
		                           values ($1, $2, $3, $4, $5, $6, $7)`,
			marketID, r.BucketID, at, r.Side, r.Qty, r.PayoutCents, transferID); err != nil {
			return fmt.Errorf("settlement: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// SaveEngineState stores what a strategy family must remember across a restart.
func (s *Store) SaveEngineState(ctx context.Context, series string, state any) error {
	blob, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `insert into engine_state (series, state) values ($1, $2)
	                           on conflict (series) do update set state = excluded.state, saved_at = now()`, series, blob)
	return err
}

// LoadEngineState reads it back. ok is false when nothing has been saved yet.
func (s *Store) LoadEngineState(ctx context.Context, series string, into any) (bool, error) {
	var blob []byte
	err := s.pool.QueryRow(ctx, `select state from engine_state where series = $1`, series).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(blob, into)
}

// SkimPolicy is the SUSTAINMENT ALLOCATION: the platform's own share of a bucket's gain above its
// high-water mark, split between the money buckets, in basis points. It is ours, not the
// government's: Tax here is only the part set aside, into the tax reserve, for real taxes.
type SkimPolicy struct {
	ID                             int64
	Winnings, Replenish, Tax, Fees int64
	EffectiveAt                    time.Time
	Note                           string
}

// CurrentSkimPolicy is the newest policy for simulated money.
func (s *Store) CurrentSkimPolicy(ctx context.Context) (SkimPolicy, error) {
	var p SkimPolicy
	err := s.pool.QueryRow(ctx, `select id, winnings_bps, replenish_bps, tax_bps, fees_bps, effective_at, note
	                               from skim_policy where mode = 'sim' order by id desc limit 1`).
		Scan(&p.ID, &p.Winnings, &p.Replenish, &p.Tax, &p.Fees, &p.EffectiveAt, &p.Note)
	return p, err
}

// HighWaterMark is the allocator's mark for a bucket: its last recorded high (the mark after its
// newest skim), else seedCents, LESS whatever has been moved out of it by hand since (every
// transfer out that is not a fill, a fee, a settlement, the seed or the allocation itself).
// Money the operator takes out is not a loss the strategy must earn back before its next gain
// counts; the mark comes down with it.
func (s *Store) HighWaterMark(ctx context.Context, bucketID int64, seedCents int64) (int64, error) {
	var mark int64
	err := s.pool.QueryRow(ctx, `
		with last as (select at, hwm_after_cents from bucket_skim where bucket_id = $1 order by id desc limit 1),
		     acct as (select ledger_account_id from bucket where id = $1),
		     taken as (select coalesce(sum(-e.amount_cents), 0) as cents
		                 from ledger_entry e
		                 join ledger_transfer t on t.id = e.transfer_id
		                where e.account_id = (select ledger_account_id from acct) and e.amount_cents < 0
		                  and t.reason not in ('fill', 'fee', 'settlement', 'sustainment', 'seed')
		                  and t.at > coalesce((select at from last), '-infinity'::timestamptz))
		select coalesce((select hwm_after_cents from last), $2::bigint) - (select cents from taken)`, bucketID, seedCents).Scan(&mark)
	return mark, err
}

// Skim is one bucket reaching a new high, and what was taken from the gain.
type Skim struct {
	Bucket                         SimBucket
	Policy                         SkimPolicy
	BookCents, HWMBefore           int64
	Winnings, Replenish, Tax, Fees int64
}

// Taken is everything the skim removes from the bucket.
func (k Skim) Taken() int64 { return k.Winnings + k.Replenish + k.Tax + k.Fees }

// RecordSkim books a skim as one transfer out of the bucket into the money buckets, and records
// the new high-water mark. With every rate at zero it records the mark and moves nothing.
func (s *Store) RecordSkim(ctx context.Context, setup SimSetup, k Skim) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var transferID *int64
	if k.Taken() > 0 {
		var id int64
		if err := tx.QueryRow(ctx, `insert into ledger_transfer (mode, reason, memo, created_by) values ('sim', 'sustainment', $1, $2) returning id`,
			fmt.Sprintf("sustainment allocation from %s under policy %d", k.Bucket.Name, k.Policy.ID), setup.ActorID).Scan(&id); err != nil {
			return err
		}
		batch := &pgx.Batch{}
		add := func(account, cents int64) {
			if cents != 0 {
				batch.Queue(`insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values ($1, $2, 'sim', $3)`, id, account, cents)
			}
		}
		add(k.Bucket.LedgerAccountID, -k.Taken())
		add(setup.WinningsID, k.Winnings)
		add(setup.PoolLedgerID, k.Replenish)
		add(setup.TaxReserveID, k.Tax)
		add(setup.FeeReserveID, k.Fees)
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("skim entries: %w", err)
		}
		transferID = &id
		if _, err := tx.Exec(ctx, `insert into bucket_event (bucket_id, kind, detail, actor_id)
		        values ($1, 'allocated', jsonb_build_object('winnings', $2::bigint, 'replenishment', $3::bigint, 'tax_reserve', $4::bigint, 'fee_reserve', $5::bigint), $6)`,
			k.Bucket.ID, k.Winnings, k.Replenish, k.Tax, k.Fees, setup.ActorID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `insert into bucket_skim (bucket_id, policy_id, book_cents, hwm_before_cents, gain_cents, winnings_cents,
	            replenish_cents, tax_cents, fees_cents, hwm_after_cents, transfer_id)
	        values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		k.Bucket.ID, k.Policy.ID, k.BookCents, k.HWMBefore, k.BookCents-k.HWMBefore, k.Winnings, k.Replenish, k.Tax, k.Fees,
		k.BookCents-k.Taken(), transferID); err != nil {
		return fmt.Errorf("skim record: %w", err)
	}
	return tx.Commit(ctx)
}

// MoneyBuckets is where the simulated money sits right now, in cents.
type MoneyBuckets struct {
	Deployed      int64 `json:"deployed"` // cash in the strategies' buckets that are still trading
	Winnings      int64 `json:"winnings"`
	Replenishment int64 `json:"replenishment"`
	TaxReserve    int64 `json:"tax_reserve"`
	FeeReserve    int64 `json:"fee_reserve"`
	FeesPaid      int64 `json:"fees_paid"` // to the venue, on every fill so far
	// External is everything the owners have put in from outside, to date. It is not a bucket, so
	// it is left out of the status document; the value snapshots use it to tell earning from funding.
	External int64 `json:"-"`
}

// MoneyBucketBalances reads them from the ledger.
func (s *Store) MoneyBucketBalances(ctx context.Context) (MoneyBuckets, error) {
	var m MoneyBuckets
	rows, err := s.pool.Query(ctx, `
		select a.kind, coalesce(sum(e.amount_cents), 0)::bigint
		  from ledger_account a left join ledger_entry e on e.account_id = a.id
		  left join bucket b on b.ledger_account_id = a.id
		 where a.mode = 'sim' and (a.kind <> 'bucket' or b.status <> 'frozen')
		 group by a.kind`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var cents int64
		if err := rows.Scan(&kind, &cents); err != nil {
			return m, err
		}
		switch kind {
		case "bucket":
			m.Deployed = cents
		case "profit_pool":
			m.Winnings = cents
		case "common_pool":
			m.Replenishment = cents
		case "tax_reserve":
			m.TaxReserve = cents
		case "fee_reserve":
			m.FeeReserve = cents
		case "fees":
			m.FeesPaid = cents
		case "external":
			m.External = -cents // the outside world's balance goes down as money comes in
		}
	}
	return m, rows.Err()
}
