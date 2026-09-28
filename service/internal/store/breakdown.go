package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// VersionBet is one bucket's bet on one settled market: what one strategy version paid there and
// what came back, with the facts the Evidence page's breakdown cuts it by (analysis.Break).
type VersionBet struct {
	MarketID  int64
	Close     time.Time // the market's close: the window the bet belongs to
	Coin      string
	Placed    time.Time // its first buy that filled
	Side      string    // the side of that first buy
	Member    string    // the roster member that fired it, from the order's detail; "" outside a roster
	Qty       int64     // contracts bought
	CostCents float64   // contracts x entry price, fees NOT inside
	FeeCents  int64     // venue fees on every fill of the market, buys and sales
	PnLCents  int64     // what the bucket got back less what it paid there, fees inside
	Won       bool      // a settlement paid it something
	// SideMid is the mid price of the side bought when the order was decided, from the order's
	// detail (which records the YES mid); nil when the order recorded none (the older engines).
	SideMid *float64
	// QuotedSpread is the recorded book's YES ask less YES bid (the NO spread is the same number)
	// in the last snapshot at or before the first buy, looked for up to ten minutes back; nil
	// when there is none, or when a side of the book was empty.
	QuotedSpread *float64
}

// VersionBets reads every bet a strategy version made on a market that has settled, all its
// buckets (lives) together, oldest close first. A market counts only once every bucket of the
// version that held contracts in it to the end has its settlement booked, the rule
// RealisedByCoin uses, so no bet appears with its stake paid and its winnings still to come.
// The money is each fill's own ledger entry on the bucket's account plus the settlement's
// payout: the same figures as the ledger and the leaderboard, fees inside. info says what the
// version is; found is false when there is no such version.
func (s *Store) VersionBets(ctx context.Context, versionID int64) (info VersionInfo, found bool, bets []VersionBet, err error) {
	bets = []VersionBet{}
	err = s.ReadOnly(ctx, 20*time.Second, func(q Querier) error {
		err := q.QueryRow(ctx, `
			select s.family, (case when jsonb_typeof(v.params->'members') = 'array' then jsonb_array_length(v.params->'members') else 0 end) >= 2
			  from strategy_version v join strategy s on s.id = v.strategy_id where v.id = $1`, versionID).Scan(&info.Family, &info.Roster)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		rows, err := q.Query(ctx, `
			with b as (select id, ledger_account_id from bucket where mode = 'sim' and strategy_version_id = $1),
			mk as (select distinct o.market_id as id from trade_order o join b on b.id = o.bucket_id),
			settled as (
			    select m.id, m.closes_at, i.underlying
			      from mk join market m on m.id = mk.id join instrument i on i.id = m.instrument_id
			     where m.settled_at is not null
			       and not exists (
			           select 1 from trade_order o join b on b.id = o.bucket_id join fill f on f.order_id = o.id
			            where o.market_id = m.id
			              and not exists (select 1 from settlement x
			                               where x.market_id = o.market_id and x.bucket_id = o.bucket_id and x.side = o.side)
			            group by o.bucket_id, o.side
			           having sum(case when o.action = 'buy' then f.qty else -f.qty end) > 0)),
			buy as (
			    select o.bucket_id, o.market_id, min(o.placed_at) as placed_at,
			           (array_agg(o.side order by o.placed_at, o.id))[1] as side,
			           (array_agg(o.detail->>'member' order by o.placed_at, o.id))[1] as member,
			           (array_agg(o.detail->>'mid' order by o.placed_at, o.id))[1] as mid
			      from trade_order o join b on b.id = o.bucket_id
			     where o.action = 'buy' and exists (select 1 from fill f where f.order_id = o.id)
			     group by o.bucket_id, o.market_id),
			fl as (
			    select o.bucket_id, o.market_id,
			           coalesce(sum(f.qty) filter (where o.action = 'buy'), 0) as qty,
			           coalesce(sum(f.qty * f.price) filter (where o.action = 'buy'), 0) as notional,
			           sum(f.fee_cents) as fee
			      from trade_order o join b on b.id = o.bucket_id join fill f on f.order_id = o.id
			     group by o.bucket_id, o.market_id),
			cash as (
			    select o.bucket_id, o.market_id, sum(e.amount_cents) as cents
			      from trade_order o join b on b.id = o.bucket_id join fill f on f.order_id = o.id
			      join ledger_entry e on e.transfer_id = f.transfer_id and e.account_id = b.ledger_account_id
			     group by o.bucket_id, o.market_id),
			pay as (select x.bucket_id, x.market_id, sum(x.payout_cents) as payout
			          from settlement x join b on b.id = x.bucket_id group by x.bucket_id, x.market_id)
			select s.id, s.closes_at, s.underlying, buy.placed_at, buy.side, coalesce(buy.member, ''),
			       round(fl.qty)::bigint, (fl.notional * 100)::float8, coalesce(fl.fee, 0)::bigint,
			       (coalesce(cash.cents, 0) + coalesce(pay.payout, 0))::bigint, coalesce(pay.payout, 0) > 0,
			       (case when nullif(buy.mid, '') is null then null
			             when buy.side = 'yes' then buy.mid::numeric else 1 - buy.mid::numeric end)::float8,
			       (select (nullif(ev.quotes->>'yes_ask', '')::numeric - nullif(ev.quotes->>'yes_bid', '')::numeric)::float8
			          from evaluation ev
			         where ev.market_id = s.id and ev.at <= buy.placed_at and ev.at >= buy.placed_at - interval '10 minutes'
			         order by ev.at desc limit 1)
			  from settled s
			  join buy on buy.market_id = s.id
			  join fl on fl.bucket_id = buy.bucket_id and fl.market_id = s.id
			  left join cash on cash.bucket_id = buy.bucket_id and cash.market_id = s.id
			  left join pay on pay.bucket_id = buy.bucket_id and pay.market_id = s.id
			 where fl.qty > 0
			 order by s.closes_at, s.id`, versionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v VersionBet
			if err := rows.Scan(&v.MarketID, &v.Close, &v.Coin, &v.Placed, &v.Side, &v.Member, &v.Qty, &v.CostCents,
				&v.FeeCents, &v.PnLCents, &v.Won, &v.SideMid, &v.QuotedSpread); err != nil {
				return err
			}
			bets = append(bets, v)
		}
		return rows.Err()
	})
	return info, found, bets, err
}

// VersionInfo is what a version is, as the breakdown needs it.
type VersionInfo struct {
	Family string // FamilyRounds or FamilyLadders
	Roster bool   // its params carry two or more members: it picks among them, and its shadows are recorded
}

// ShadowRow is one scored member shadow (roster_shadow), as the dancer's measurement reads it.
type ShadowRow struct {
	Member   string
	Close    time.Time
	Seen     time.Time // zero when it was not kept
	Owner    string
	OwnerHow string
	Cost     int64
	PnL      int64
}

// VersionShadows reads a roster version's scored shadows, every bucket of it, oldest clock first.
// A shadow dropped unscored has no result to measure and is left out.
func (s *Store) VersionShadows(ctx context.Context, versionID int64) ([]ShadowRow, error) {
	out := []ShadowRow{}
	err := s.ReadOnly(ctx, 20*time.Second, func(q Querier) error {
		rows, err := q.Query(ctx, `
			select member, closes_at, seen_at, owner, owner_how, cost_cents, pnl_cents
			  from roster_shadow
			 where strategy_version_id = $1 and result is not null
			 order by closes_at, member, market_id`, versionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ShadowRow
			var seen *time.Time
			if err := rows.Scan(&r.Member, &r.Close, &seen, &r.Owner, &r.OwnerHow, &r.Cost, &r.PnL); err != nil {
				return err
			}
			if seen != nil {
				r.Seen = *seen
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
