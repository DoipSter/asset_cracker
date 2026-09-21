package store

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/analysis"
)

// Everything in this file READS, for GET /api/analysis. evaluation and decision grow by hundreds
// of thousands of rows a day and are partitioned by month, so every query on them is bounded by
// time AND by market or strategy version: the planner prunes to one partition and walks an
// index. A settled round is read once (AnalysisWindow) and never again; the caller caches it.

// AnalysisMarkets lists the rounds whose result was stored at or after `since`, oldest first.
// The market table is small (a few hundred rows a day) and not partitioned.
func (s *Store) AnalysisMarkets(ctx context.Context, since time.Time) ([]analysis.Market, time.Time, error) {
	latest := since
	rows, err := s.pool.Query(ctx, `
		select m.id, i.underlying, extract(epoch from m.closes_at)::bigint, m.result, m.settled_at
		  from market m join instrument i on i.id = m.instrument_id
		 where m.result in ('yes', 'no') and m.closes_at is not null and m.settled_at >= $1
		 order by m.closes_at, m.id`, since)
	if err != nil {
		return nil, latest, err
	}
	defer rows.Close()
	var out []analysis.Market
	for rows.Next() {
		var m analysis.Market
		var settled time.Time
		if err := rows.Scan(&m.ID, &m.Coin, &m.Closes, &m.Result, &settled); err != nil {
			return nil, latest, err
		}
		if settled.After(latest) {
			latest = settled
		}
		out = append(out, m)
	}
	return out, latest, rows.Err()
}

// AnalysisOpenWindows is the closes_at (unix seconds) of every round that has closed and has no
// result yet. The five coins settle a few seconds apart, and a window is only scored whole.
func (s *Store) AnalysisOpenWindows(ctx context.Context, now time.Time) (map[int64]bool, error) {
	rows, err := s.pool.Query(ctx, `select distinct extract(epoch from closes_at)::bigint from market where result is null and closes_at < $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var w int64
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		out[w] = true
	}
	return out, rows.Err()
}

// AnalysisModelVersions is the strategy versions whose journal carries the model being scored:
// the second engine's six originals. Every one of them journals the same raw model probability
// for a given second (runner2.go: ModelProb is View.PModel, computed once per step), and a twin
// journals nothing.
func (s *Store) AnalysisModelVersions(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		select v.id from strategy_version v join strategy st on st.id = v.strategy_id
		 where st.family = 'kalshi15m' and v.version = 2 and not coalesce((v.params->>'anti')::boolean, false)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AnalysisBuckets lists every sim bucket of the kalshi15m family, live and frozen, with the
// strategy version it belongs to. A twin is reported under its original's name, world "anti".
func (s *Store) AnalysisBuckets(ctx context.Context) ([]analysis.Bucket, map[int64]int64, error) {
	rows, err := s.pool.Query(ctx, `
		select b.id, b.strategy_version_id, st.name, v.version, coalesce((v.params->>'anti')::boolean, false),
		       b.status = 'frozen', b.replaced_by_bucket_id is not null, b.ledger_account_id
		  from bucket b join strategy_version v on v.id = b.strategy_version_id join strategy st on st.id = v.strategy_id
		 where b.mode = 'sim' and st.family = 'kalshi15m' order by b.id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []analysis.Bucket
	ledger := map[int64]int64{} // bucket id -> its ledger account
	for rows.Next() {
		var b analysis.Bucket
		var version int
		var anti bool
		var account int64
		if err := rows.Scan(&b.ID, &b.VersionID, &b.Strategy, &version, &anti, &b.Frozen, &b.Replaced, &account); err != nil {
			return nil, nil, err
		}
		b.Engine, b.World = fmt.Sprintf("v%d", version), "real"
		if anti {
			b.World, b.Strategy = "anti", strings.TrimPrefix(b.Strategy, "Anti ")
		}
		ledger[b.ID] = account
		out = append(out, b)
	}
	return out, ledger, rows.Err()
}

// AnalysisWindow reads one settled window once: every market in `markets` must share one
// closes_at. Three queries, each bounded to the window's own minutes.
func (s *Store) AnalysisWindow(ctx context.Context, modelVersions []int64, markets []analysis.Market) ([]analysis.MarketFacts, error) {
	if len(markets) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(markets))
	for i, m := range markets {
		ids[i] = m.ID
	}
	closes := time.Unix(markets[0].Closes, 0)
	// A round is open for 15 minutes; the margin is for a market listed a little early. Nothing
	// is journaled or traded after the close.
	from, to := closes.Add(-20*time.Minute), closes.Add(time.Minute)

	// 1. The scorecard's sums. One row per evaluation (a second of one market): the six originals
	// journal the same model_prob for it, so the first is taken. decision is reached through
	// (strategy_version_id, at), evaluation through (market_id, at).
	band := "case"
	for i, b := range analysis.Bands[:len(analysis.Bands)-1] {
		band += fmt.Sprintf(" when tau >= %g then %d", b.MinTau, i)
	}
	band += fmt.Sprintf(" else %d end", len(analysis.Bands)-1)
	scores := map[int64]*[5]analysis.BandSum{}
	rows, err := s.pool.Query(ctx, `
		with scored as (
			select distinct on (d.evaluation_id) e.market_id, d.model_prob as p, d.market_prob as q,
			       extract(epoch from (m.closes_at - d.at))::float8 as tau,
			       case when m.result = 'yes' then 1.0::float8 else 0.0::float8 end as y
			  from decision d
			  join evaluation e on e.id = d.evaluation_id and e.at = d.at
			  join market m on m.id = e.market_id
			 where d.strategy_version_id = any($1) and d.at >= $2 and d.at < $3
			   and e.at >= $2 and e.at < $3 and e.market_id = any($4)
			   and d.model_prob is not null and d.market_prob is not null
			 order by d.evaluation_id, d.id)
		select market_id, `+band+` as band, count(*), sum((p - y) * (p - y))::float8, sum((q - y) * (q - y))::float8
		  from scored group by 1, 2`, modelVersions, from, to, ids)
	if err != nil {
		return nil, fmt.Errorf("scorecard sums: %w", err)
	}
	for rows.Next() {
		var market int64
		var b int
		var sum analysis.BandSum
		if err := rows.Scan(&market, &b, &sum.N, &sum.Model, &sum.Market); err != nil {
			rows.Close()
			return nil, err
		}
		if scores[market] == nil {
			scores[market] = &[5]analysis.BandSum{}
		}
		if b >= 0 && b < len(scores[market]) {
			scores[market][b] = sum
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scorecard sums: %w", err)
	}

	// 2. Every fill, and for a SELL the bid ladder on its side in the latest snapshot of that
	// market at or before it (in practice the same instant: the engine sold off that snapshot).
	// has_depth is false when that snapshot predates the recording of depth (release 87a2100) or
	// there is none within five seconds: such a sale is counted and never priced.
	trades := map[int64][]analysis.Trade{}
	rows, err = s.pool.Query(ctx, `
		select o.market_id, o.id, o.bucket_id, o.action = 'sell', o.side, o.qty::float8, extract(epoch from o.placed_at)::float8,
		       coalesce((o.detail->>'cost')::float8, 0), coalesce((o.detail->>'payout')::float8, 0),
		       coalesce(jsonb_typeof(book.bids) = 'array', false), coalesce((book.bids->0->>1)::float8, 0)
		  from trade_order o
		  left join lateral (
			select case o.side when 'yes' then e.quotes->'yes_bids' else e.quotes->'no_bids' end as bids
			  from evaluation e
			 where o.action = 'sell' and e.market_id = o.market_id and e.at <= o.placed_at and e.at >= o.placed_at - interval '5 seconds'
			 order by e.at desc limit 1) book on true
		 where o.market_id = any($1) and o.placed_at >= $2 and o.placed_at < $3 and o.status = 'filled'
		 order by o.id`, ids, from, to)
	if err != nil {
		return nil, fmt.Errorf("fills: %w", err)
	}
	for rows.Next() {
		var market int64
		var t analysis.Trade
		var qty, at, cost, payout float64
		if err := rows.Scan(&market, &t.OrderID, &t.BucketID, &t.Sell, &t.Side, &qty, &at, &cost, &payout, &t.HasDepth, &t.Displayed); err != nil {
			rows.Close()
			return nil, err
		}
		// cents as the runner books them into the ledger: math.Round(dollars * 100)
		t.Qty, t.Second, t.CostCents, t.PayoutCents = int(qty), int64(math.Floor(at)), int64(math.Round(cost*100)), int64(math.Round(payout*100))
		trades[market] = append(trades[market], t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fills: %w", err)
	}

	// 3. What each bucket was paid when the round settled. settlement is not partitioned.
	payouts := map[int64]map[int64]int64{}
	rows, err = s.pool.Query(ctx, `select market_id, bucket_id, sum(payout_cents)::bigint from settlement where market_id = any($1) group by 1, 2`, ids)
	if err != nil {
		return nil, fmt.Errorf("settlements: %w", err)
	}
	for rows.Next() {
		var market, bucket, cents int64
		if err := rows.Scan(&market, &bucket, &cents); err != nil {
			rows.Close()
			return nil, err
		}
		if payouts[market] == nil {
			payouts[market] = map[int64]int64{}
		}
		payouts[market][bucket] = cents
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settlements: %w", err)
	}

	out := make([]analysis.MarketFacts, 0, len(markets))
	for _, m := range markets {
		f := analysis.Settle(m, trades[m.ID], payouts[m.ID])
		if sc := scores[m.ID]; sc != nil {
			f.Score = *sc
		}
		out = append(out, f)
	}
	return out, nil
}

// AnalysisUnsettled is what is still in play, by bucket: early sales in rounds with no result
// yet, and the cost of bets still open (bought, not sold, round not settled). Bounded to orders
// placed since `since`.
func (s *Store) AnalysisUnsettled(ctx context.Context, since time.Time) (sells map[int64]int, openCostCents map[int64]int64, err error) {
	sells, openCostCents = map[int64]int{}, map[int64]int64{}
	rows, err := s.pool.Query(ctx, `
		select o.bucket_id, count(*) filter (where o.action = 'sell'),
		       coalesce(sum(round(coalesce((o.detail->>'cost')::numeric, 0) * 100) * case o.action when 'buy' then 1 else -1 end), 0)::bigint
		  from trade_order o join market m on m.id = o.market_id
		 where o.placed_at >= $1 and o.status = 'filled' and m.result is null
		 group by 1`, since)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket, cost int64
		var n int
		if err := rows.Scan(&bucket, &n, &cost); err != nil {
			return nil, nil, err
		}
		sells[bucket], openCostCents[bucket] = n, cost
	}
	return sells, openCostCents, rows.Err()
}

// LedgerMaxEntryID is the newest ledger entry's id, 0 if there is none.
func (s *Store) LedgerMaxEntryID(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `select coalesce(max(id), 0) from ledger_entry`).Scan(&id)
	return id, err
}

// LedgerSums adds up the entries with after < id <= upto for some accounts. The ledger is only
// ever appended to, so a caller can keep a running balance and ask for the new entries alone.
func (s *Store) LedgerSums(ctx context.Context, accounts []int64, after, upto int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(accounts) == 0 || upto <= after {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `select account_id, sum(amount_cents)::bigint from ledger_entry
	                                 where account_id = any($1) and id > $2 and id <= $3 group by 1`, accounts, after, upto)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var account, cents int64
		if err := rows.Scan(&account, &cents); err != nil {
			return nil, err
		}
		out[account] = cents
	}
	return out, rows.Err()
}
