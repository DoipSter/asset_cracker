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
// index. A settled round is read once its settlement rows are in (AnalysisWindow) and never
// again; the caller caches it.

// The analysis reads ONLY the 15-minute series (FifteenMinuteSeries, in store.go). Its three
// listings of markets are these; AnalysisWindow reads only the markets AnalysisMarkets listed,
// and AnalysisUnsettled starts from trade_order, which a ladder market never has.
const (
	sqlAnalysisSettledCount = `
		select count(*) from market m join instrument i on i.id = m.instrument_id
		 where m.result in ('yes', 'no') and m.closes_at is not null and ` + FifteenMinuteSeries

	sqlAnalysisMarkets = `
		select m.id, i.underlying, extract(epoch from m.closes_at)::bigint, m.result
		  from market m join instrument i on i.id = m.instrument_id
		 where m.result in ('yes', 'no') and m.closes_at is not null and m.closes_at >= $1
		   and ` + FifteenMinuteSeries + `
		 order by m.closes_at, m.id`

	sqlAnalysisOpenWindows = `
		select distinct extract(epoch from m.closes_at)::bigint
		  from market m join instrument i on i.id = m.instrument_id
		 where m.result is null and m.closes_at < $1
		   and (m.closes_at > $1 - interval '1 hour'
		        or exists (select 1 from trade_order o where o.market_id = m.id))
		   and ` + FifteenMinuteSeries
)

// AnalysisSettledCount is how many rounds have a result: the measured figure the caller checks
// its own list of settled markets against, so that a listing bounded by time can never quietly
// fall short. It counts exactly what AnalysisMarkets lists.
func (s *Store) AnalysisSettledCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, sqlAnalysisSettledCount).Scan(&n)
	return n, err
}

// AnalysisMarkets lists the settled rounds that closed at or after `closedSince`, oldest first.
// It is keyed on closes_at, which is ours and fixed when the round is first seen. It used to be
// keyed on settled_at, which is KALSHI'S settlement time and says nothing about when the result
// reached this table: after an outage the poller stores results in no particular order, and one
// stamped over an hour before the newest already seen was never listed. The market table is
// small (a few hundred rows a day) and not partitioned.
func (s *Store) AnalysisMarkets(ctx context.Context, closedSince time.Time) ([]analysis.Market, error) {
	rows, err := s.pool.Query(ctx, sqlAnalysisMarkets, closedSince)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []analysis.Market
	for rows.Next() {
		var m analysis.Market
		if err := rows.Scan(&m.ID, &m.Coin, &m.Closes, &m.Result); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AnalysisTrials is the number of strategy versions ever registered. The schema names this row
// count as the number of trials that every significance figure must be corrected for
// (0001_init.sql, on strategy_version), so it is counted there and not taken from whichever
// buckets happen to be listed.
func (s *Store) AnalysisTrials(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `select count(*) from strategy_version`).Scan(&n)
	return n, err
}

// AnalysisOpenWindows is the closes_at (unix seconds) of every round that has closed and can
// still get a result. The five coins settle a few seconds apart, and a window is only scored
// whole. "Can still get a result" is UnsettledMarkets' own rule, so the two cannot disagree: a
// round nobody bet on is asked about for an hour and then never again, and after that hour it
// no longer keeps the rest of its window out. It is simply missing from the scorecard.
func (s *Store) AnalysisOpenWindows(ctx context.Context, now time.Time) (map[int64]bool, error) {
	rows, err := s.pool.Query(ctx, sqlAnalysisOpenWindows, now)
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

// AnalysisWindow reads the markets of one settled window: every market in `markets` must share
// one closes_at. Three queries, each bounded to the window's own minutes.
//
// It answers with the facts of the markets whose money is all there, and, by market id, why each
// of the others is not ready. A round's result is committed before its settlement rows, in
// separate transactions, and a halted runner or a stop in between never writes them; such a
// market would read as held bets that paid nothing. So each market's fills are reconciled with
// its settlement rows per bucket and side (analysis.Reconcile) before anything is made of it. A
// market that is not ready must NOT be cached: the caller leaves its window out and asks again
// on a later refresh. Nothing here waits or guesses how long the gap is.
func (s *Store) AnalysisWindow(ctx context.Context, modelVersions []int64, markets []analysis.Market) (facts []analysis.MarketFacts, unready map[int64]string, err error) {
	if len(markets) == 0 {
		return nil, nil, nil
	}
	ids := make([]int64, len(markets))
	for i, m := range markets {
		ids[i] = m.ID
	}
	closes := time.Unix(markets[0].Closes, 0)
	// A round is open for 15 minutes; the margin is for a market listed a little early. Nothing
	// is journaled or traded after the close.
	from, to := closes.Add(-20*time.Minute), closes.Add(time.Minute)

	// 1. Every order that filled anything, and for a SELL the bid ladder on its side in the latest
	// snapshot of that market at or before it (in practice the same instant: the engine sold off
	// that snapshot).
	// The contracts are the order's FILL rows added up, never trade_order.qty, and an order is
	// read when it has at least one fill, whatever its status says. trade_order.qty is what was
	// ASKED for. The first two engines always fill the whole order with one fill, so for them the
	// two are the same number; an order that is partly filled (status 'partial', which the schema
	// has allowed since 0001_init.sql) holds fewer contracts than it asked for, and its settlement
	// row covers only those. Read from qty, Reconcile would find more held than settled and hold
	// the market back for good, and one unready market keeps its whole WINDOW out of the evidence
	// for every engine. Read with status = 'filled', the partial order would vanish instead and
	// its settlement would have no fill to account for it: the same ending. An order that filled
	// nothing (cancelled, rejected) moved no contracts and no money, and is not read at all.
	// The money is still detail.cost and detail.payout: they are what actually moved for the
	// contracts that filled (a sale's cost is the basis of the contracts SOLD, which no fill row
	// carries), so they need no scaling. The lateral sum walks fill (order_id), a handful of rows.
	// depth_priced is true for an order of the depth-aware paper broker (detail.v = 3; the older
	// engines write no "v"): see analysis.Trade.DepthPriced.
	// has_depth is false when that snapshot predates the recording of depth (release 87a2100) or
	// there is none within five seconds: such a sale is counted and never priced.
	// money_missing is true when detail has no cost, or a sale's has no payout: the zero that
	// coalesce puts there is then not a figure, and the market is held back (analysis.MoneyMissing).
	trades := map[int64][]analysis.Trade{}
	rows, err := s.pool.Query(ctx, `
		select o.market_id, o.id, o.bucket_id, o.action = 'sell', o.side, f.qty, extract(epoch from o.placed_at)::float8,
		       coalesce((o.detail->>'cost')::float8, 0), coalesce((o.detail->>'payout')::float8, 0),
		       (o.detail->>'cost') is null or (o.action = 'sell' and (o.detail->>'payout') is null),
		       coalesce(jsonb_typeof(book.bids) = 'array', false), coalesce((book.bids->0->>1)::float8, 0),
		       coalesce(o.detail->>'v' = '3', false)
		  from trade_order o
		  join lateral (select sum(x.qty)::float8 as qty from fill x where x.order_id = o.id) f on f.qty > 0
		  left join lateral (
			select case o.side when 'yes' then e.quotes->'yes_bids' else e.quotes->'no_bids' end as bids
			  from evaluation e
			 where o.action = 'sell' and e.market_id = o.market_id and e.at <= o.placed_at and e.at >= o.placed_at - interval '5 seconds'
			 order by e.at desc limit 1) book on true
		 where o.market_id = any($1) and o.placed_at >= $2 and o.placed_at < $3
		 order by o.id`, ids, from, to)
	if err != nil {
		return nil, nil, fmt.Errorf("fills: %w", err)
	}
	for rows.Next() {
		var market int64
		var t analysis.Trade
		var qty, at, cost, payout float64
		if err := rows.Scan(&market, &t.OrderID, &t.BucketID, &t.Sell, &t.Side, &qty, &at, &cost, &payout, &t.MoneyMissing, &t.HasDepth, &t.Displayed, &t.DepthPriced); err != nil {
			rows.Close()
			return nil, nil, err
		}
		// cents as the runner books them into the ledger: math.Round(dollars * 100)
		// Fills are whole contracts, so their sum is a whole number and the rounding changes nothing;
		// it only keeps a float that came out a hair under from losing a contract.
		t.Qty, t.Second, t.CostCents, t.PayoutCents = int(math.Round(qty)), int64(math.Floor(at)), int64(math.Round(cost*100)), int64(math.Round(payout*100))
		trades[market] = append(trades[market], t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("fills: %w", err)
	}

	// 2. What each bucket was paid when the round settled, and for HOW MANY contracts, by side.
	// settlement has one row per (market, bucket, side), a losing hold's included with 0 cents,
	// and its unique constraint's index leads with market_id. It is not partitioned.
	settled := map[int64]map[analysis.Holding]analysis.Paid{}
	rows, err = s.pool.Query(ctx, `select market_id, bucket_id, side, sum(qty)::bigint, sum(payout_cents)::bigint
	                                 from settlement where market_id = any($1) group by 1, 2, 3`, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("settlements: %w", err)
	}
	for rows.Next() {
		var market, qty int64
		var k analysis.Holding
		var p analysis.Paid
		if err := rows.Scan(&market, &k.BucketID, &k.Side, &qty, &p.PayoutCents); err != nil {
			rows.Close()
			return nil, nil, err
		}
		p.Qty = int(qty)
		if settled[market] == nil {
			settled[market] = map[analysis.Holding]analysis.Paid{}
		}
		settled[market][k] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("settlements: %w", err)
	}

	unready = map[int64]string{}
	var ready []int64
	for _, m := range markets {
		if orders := analysis.MoneyMissing(trades[m.ID]); len(orders) > 0 {
			unready[m.ID] = fmt.Sprintf("orders %v have no recorded cost or payout", orders)
		} else if bad := analysis.Reconcile(trades[m.ID], settled[m.ID]); len(bad) > 0 {
			why := make([]string, len(bad))
			for i, b := range bad {
				why[i] = fmt.Sprintf("bucket %d held %d %s at the close and its settlement rows cover %d", b.BucketID, b.Held, b.Side, b.Settled)
			}
			unready[m.ID] = strings.Join(why, "; ")
		} else {
			ready = append(ready, m.ID)
		}
	}

	// 3. The scorecard's sums, for the ready markets only: this is the heavy read, and a market
	// that is not ready will be read again. One row per evaluation (a second of one market): the
	// six originals journal the same model_prob for it, so the first is taken. decision is
	// reached through (strategy_version_id, at), evaluation through (market_id, at).
	scores := map[int64]*[5]analysis.BandSum{}
	if len(ready) > 0 {
		band := "case"
		for i, b := range analysis.Bands[:len(analysis.Bands)-1] {
			band += fmt.Sprintf(" when tau >= %g then %d", b.MinTau, i)
		}
		band += fmt.Sprintf(" else %d end", len(analysis.Bands)-1)
		rows, err = s.pool.Query(ctx, `
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
			  from scored group by 1, 2`, modelVersions, from, to, ready)
		if err != nil {
			return nil, nil, fmt.Errorf("scorecard sums: %w", err)
		}
		for rows.Next() {
			var market int64
			var b int
			var sum analysis.BandSum
			if err := rows.Scan(&market, &b, &sum.N, &sum.Model, &sum.Market); err != nil {
				rows.Close()
				return nil, nil, err
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
			return nil, nil, fmt.Errorf("scorecard sums: %w", err)
		}
	}

	facts = make([]analysis.MarketFacts, 0, len(ready))
	for _, m := range markets {
		if _, held := unready[m.ID]; held {
			continue
		}
		f := analysis.Settle(m, trades[m.ID], settled[m.ID])
		if sc := scores[m.ID]; sc != nil {
			f.Score = *sc
		}
		facts = append(facts, f)
	}
	return facts, unready, nil
}

// AnalysisUnsettled is what is still in play, by bucket: early sales in rounds with no result
// yet, and the cost of bets still open (bought, not sold, round not settled). Bounded to orders
// placed since `since`.
//
// An order counts when it has at least one fill, whatever its status says, exactly as in
// AnalysisWindow: a partly filled sale is a sale, and an order that filled nothing sold nothing
// and cost nothing. detail.cost is what moved for the contracts that did fill (for a sale, the
// basis of the contracts sold), so bought less sold is the cost still open under partial fills too.
func (s *Store) AnalysisUnsettled(ctx context.Context, since time.Time) (sells map[int64]int, openCostCents map[int64]int64, err error) {
	sells, openCostCents = map[int64]int{}, map[int64]int64{}
	rows, err := s.pool.Query(ctx, `
		select o.bucket_id, count(*) filter (where o.action = 'sell'),
		       coalesce(sum(round(coalesce((o.detail->>'cost')::numeric, 0) * 100) * case o.action when 'buy' then 1 else -1 end), 0)::bigint
		  from trade_order o join market m on m.id = o.market_id
		 where o.placed_at >= $1 and m.result is null
		   and exists (select 1 from fill f where f.order_id = o.id)
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
