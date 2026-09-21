package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Everything in this file writes SIMULATED money. Every ledger account it touches is created
// with mode 'sim', and the database refuses any transfer that would mix sim with real.

// SimBucket is one strategy's bucket, as the runner needs it.
type SimBucket struct {
	ID              int64
	LedgerAccountID int64
	VersionID       int64
	CashCents       int64
}

// SimSetup is the simulated world for one series.
type SimSetup struct {
	ActorID       int64
	VenueLedgerID int64
	FeesLedgerID  int64
	Buckets       map[string]SimBucket // by strategy name
}

// EnsureSimSetup creates, once, the sim ledger accounts, the paper venue account, and one
// bucket per strategy seeded with seedCents from the common pool. It is safe to call on every
// start: what exists is left alone. A bucket is never topped up here.
func (s *Store) EnsureSimSetup(ctx context.Context, series, family string, strategies []string, seedCents int64) (SimSetup, error) {
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
	if _, err = account("profit_pool", "profit pool (sim)"); err != nil {
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
		bucketName := series + " " + name + " v1"
		var b SimBucket
		err := tx.QueryRow(ctx, `select id, ledger_account_id, strategy_version_id from bucket where name = $1`, bucketName).
			Scan(&b.ID, &b.LedgerAccountID, &b.VersionID)
		if errors.Is(err, pgx.ErrNoRows) {
			if err = tx.QueryRow(ctx, `select v.id from strategy_version v join strategy st on st.id = v.strategy_id
			                            where st.family = $1 and st.name = $2 and v.version = 1`, family, name).Scan(&b.VersionID); err != nil {
				return out, fmt.Errorf("strategy %s/%s v1 is not registered: %w", family, name, err)
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
	return out, tx.Commit(ctx)
}

// DecisionRow is one strategy's conclusion from one look at the market.
type DecisionRow struct {
	BucketID, VersionID          int64
	ModelProb, MarketProb, Edge  float64
	Side, Action, BlockedBy, Why string
	SizeAlone                    *int
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

	var evalID int64
	if err := tx.QueryRow(ctx, `insert into evaluation (at, market_id, underlying_price, quotes, model)
	                            values ($1, $2, nullif($3, '')::numeric, $4, $5) returning id`,
		r.At, r.MarketID, r.UnderlyingPrice, quotes, model).Scan(&evalID); err != nil {
		return fmt.Errorf("evaluation: %w", err)
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
