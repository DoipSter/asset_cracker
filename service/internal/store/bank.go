package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Bank is the one open house. The balance sheet is this bank. A reset closes it and opens
// the next one; allocating a bucket draws from its replenishment.
type Bank struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	OpenedAt time.Time `json:"opened_at"`
	Note     string    `json:"note,omitempty"`
}

// Payday is a deposit from the owners into replenishment, on a rhythm, while its bank is open.
type Payday struct {
	ID          int64     `json:"id"`
	BankID      int64     `json:"bank_id"`
	AmountCents int64     `json:"amount_cents"`
	EveryDays   int       `json:"every_days"`
	NextAt      time.Time `json:"next_at"`
	Note        string    `json:"note,omitempty"`
}

// PaydayRefused is a schedule the rules do not allow; its text is shown to the operator.
type PaydayRefused struct{ Why string }

func (e PaydayRefused) Error() string { return e.Why }

// ErrNoOpenBank is a schedule asked for when no bank is open.
var ErrNoOpenBank = errors.New("no bank is open")

// ErrNoPayday is a stop asked for a payday that is not there, or already stopped.
var ErrNoPayday = errors.New("no such payday")

func missingRelation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// OpenBank is the bank whose books are the balance sheet. A zero ID means none is open
// (or the bank table is not in this database yet).
func (s *Store) OpenBank(ctx context.Context) (Bank, error) {
	var b Bank
	err := s.pool.QueryRow(ctx, `
		select id, name, opened_at, note
		  from bank
		 where closed_at is null
		 order by id desc
		 limit 1`).Scan(&b.ID, &b.Name, &b.OpenedAt, &b.Note)
	if errors.Is(err, pgx.ErrNoRows) || missingRelation(err) {
		return Bank{}, nil
	}
	return b, err
}

// ListPaydays is the schedules still paying on the open bank, soonest first.
func (s *Store) ListPaydays(ctx context.Context) ([]Payday, error) {
	rows, err := s.pool.Query(ctx, `
		select e.id, e.bank_id, e.amount_cents, e.every_days, e.next_at, e.note
		  from bank_event e
		  join bank b on b.id = e.bank_id
		 where e.kind = 'payday' and e.enabled and b.closed_at is null
		 order by e.next_at, e.id`)
	if missingRelation(err) {
		return []Payday{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Payday{}
	for rows.Next() {
		var p Payday
		if err := rows.Scan(&p.ID, &p.BankID, &p.AmountCents, &p.EveryDays, &p.NextAt, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SchedulePayday records a deposit into replenishment. The first one lands everyDays after now,
// then on that rhythm. It belongs to the open bank and does not follow the next one.
func (s *Store) SchedulePayday(ctx context.Context, amountCents int64, everyDays int, note string, now time.Time) (Payday, error) {
	note = strings.TrimSpace(note)
	switch {
	case amountCents <= 0:
		return Payday{}, PaydayRefused{"The amount must be above zero."}
	case amountCents > 100_000_000:
		return Payday{}, PaydayRefused{"A payday is at most $1,000,000."}
	case everyDays < 1 || everyDays > 366:
		return Payday{}, PaydayRefused{"The rhythm is a whole number of days, from 1 to 366."}
	case len(note) > 200:
		return Payday{}, PaydayRefused{"The note is too long (200 characters)."}
	}
	bank, err := s.OpenBank(ctx)
	if err != nil {
		return Payday{}, err
	}
	if bank.ID == 0 {
		return Payday{}, ErrNoOpenBank
	}
	if now.IsZero() {
		now = time.Now()
	}
	var p Payday
	err = s.pool.QueryRow(ctx, `
		insert into bank_event (bank_id, kind, amount_cents, every_days, next_at, note)
		values ($1, 'payday', $2, $3, $4, $5)
		returning id, bank_id, amount_cents, every_days, next_at, note`,
		bank.ID, amountCents, everyDays, now.AddDate(0, 0, everyDays), note).
		Scan(&p.ID, &p.BankID, &p.AmountCents, &p.EveryDays, &p.NextAt, &p.Note)
	return p, err
}

// StopPayday turns a schedule off. A stopped payday does not pay again.
func (s *Store) StopPayday(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `
		update bank_event set enabled = false
		 where id = $1 and kind = 'payday' and enabled`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNoPayday
	}
	return nil
}

// ApplyDuePaydays deposits each payday that has come due on the open bank, once, from the
// owners into replenishment. A long gap pays once and the rhythm continues from there; it does
// not dump a backlog. A closed bank's paydays are not selected.
func (s *Store) ApplyDuePaydays(ctx context.Context, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		select e.id, e.amount_cents, e.every_days, e.next_at, e.note
		  from bank_event e
		  join bank b on b.id = e.bank_id
		 where e.kind = 'payday' and e.enabled and e.next_at <= $1 and b.closed_at is null
		 order by e.id
		   for update of e`, now)
	if missingRelation(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	type due struct {
		id, cents int64
		every     int
		at        time.Time
		note      string
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.cents, &d.every, &d.at, &d.note); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(list) == 0 {
		return 0, nil
	}

	var actor, owners, pool int64
	if err := tx.QueryRow(ctx, `select id from actor where handle = 'service'`).Scan(&actor); err != nil {
		return 0, err
	}
	if err := tx.QueryRow(ctx, `select id from ledger_account where mode = 'sim' and name = 'owners (sim)'`).Scan(&owners); err != nil {
		return 0, err
	}
	if err := tx.QueryRow(ctx, `select id from ledger_account where mode = 'sim' and name = 'common pool (sim)'`).Scan(&pool); err != nil {
		return 0, err
	}

	for _, d := range list {
		memo := strings.TrimSpace(d.note)
		if memo == "" {
			memo = "payday"
		}
		var tid int64
		if err := tx.QueryRow(ctx, `
			insert into ledger_transfer (mode, reason, memo, created_by)
			values ('sim', 'deposit', $1, $2) returning id`, memo, actor).Scan(&tid); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			insert into ledger_entry (transfer_id, account_id, mode, amount_cents)
			values ($1, $2, 'sim', $3), ($1, $4, 'sim', $5)`,
			tid, owners, -d.cents, pool, d.cents); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `update bank_event set next_at = $2 where id = $1`, d.id, advancePayday(d.at, now, d.every)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(list), nil
}

// advancePayday is the next time a payday that was due at `due` should pay. It steps forward
// by the rhythm until the result is after now, so a missed stretch pays once.
func advancePayday(due, now time.Time, every int) time.Time {
	if every < 1 {
		every = 1
	}
	next := due
	for i := 0; i < 4000; i++ {
		next = next.AddDate(0, 0, every)
		if next.After(now) {
			return next
		}
	}
	return now.AddDate(0, 0, every)
}
