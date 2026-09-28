package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// RosterShadow is one roster member's would-be unit entry on one market, as the record keeps it
// (roster_shadow, migration 0023): what the member would have bought, when, who owned the clock
// then, and what it came to. The member that fired is on the order; the shadows are every member,
// fired or not, so the roster's picks can be set against what it passed over.
type RosterShadow struct {
	BucketID  int64
	Member    string
	MarketID  int64
	Side      string    // yes or no
	Seen      time.Time // zero when the engine did not keep it (memory saved before 2026-09-27): stored as NULL
	Closes    time.Time
	Owner     string // the clock's owner when it was seen; "" when none or not kept
	OwnerHow  string // window (elected from prior clocks) or warmup (the first member, too few clocks yet); "" with no owner
	CostCents int64  // one contract
	Result    string // yes or no; "" when dropped unscored: result and pnl are then NULL
	PnLCents  int64
}

// InsertRosterShadows appends shadows, all or nothing. A row the table already has (the same
// bucket, member and market) is passed over, so writing a batch again after an answer was lost
// adds nothing. The version is the bucket's.
func (s *Store) InsertRosterShadows(ctx context.Context, rows []RosterShadow) error {
	if len(rows) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for _, r := range rows {
			var seen *time.Time
			if !r.Seen.IsZero() {
				seen = &r.Seen
			}
			var result *string
			var pnl *int64
			if r.Result != "" {
				result, pnl = &r.Result, &r.PnLCents
			}
			batch.Queue(`
				insert into roster_shadow (bucket_id, strategy_version_id, member, market_id, side, seen_at, closes_at, owner, owner_how, cost_cents, result, pnl_cents)
				select b.id, b.strategy_version_id, $2::text, $3::bigint, $4::text, $5::timestamptz, $6::timestamptz, $7::text, $8::text, $9::integer, $10::text, $11::integer
				  from bucket b where b.id = $1::bigint
				on conflict (bucket_id, member, market_id) do nothing`,
				r.BucketID, r.Member, r.MarketID, r.Side, seen, r.Closes, r.Owner, r.OwnerHow, r.CostCents, result, pnl)
		}
		return tx.SendBatch(ctx, batch).Close()
	})
}
