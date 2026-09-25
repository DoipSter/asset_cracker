package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// The reads. Every one runs inside store.ReadOnly: a READ ONLY transaction that, where the
// connected role is a member of assetcracker_ro, takes that role. The queries the protocol gives
// only in words are written here; H15's is the long-shot protocol's printed query, verbatim.
// Nothing before outcomes() selects a result as a value: rounds() and h15Markets() only filter
// on whether a result exists, as cmd/measure3's existence query does.

const readBudget = 10 * time.Minute

// symbols are the five 15-minute series (long-shot protocol, H15; migrations 0002 and 0006).
var symbols = []string{"KXBTC15M", "KXETH15M", "KXSOL15M", "KXXRP15M", "KXDOGE15M"}

func symbolList() string {
	q := make([]string, len(symbols))
	for i, s := range symbols {
		q[i] = "'" + s + "'"
	}
	return strings.Join(q, ", ")
}

// bins are the protocol's τ bins, (lo, hi] in seconds, in its order. Bin 0 is the reference.
var bins = [][2]float64{{600, 900}, {300, 600}, {120, 300}, {60, 120}, {0, 60}}

var binLabels = []string{"(600, 900]", "(300, 600]", "(120, 300]", "(60, 120]", "(0, 60]"}

// binExpr maps tau to its bin's index: lo < tau <= hi.
const binExpr = `case when tau > 600 then 0 when tau > 300 then 1 when tau > 120 then 2 when tau > 60 then 3 else 4 end`

type round struct {
	MarketID int64     `json:"market_id"`
	Coin     string    `json:"coin"`
	Closes   time.Time `json:"closes"`
}

// observation is one round's row in one bin, as read before any outcome.
type observation struct {
	EvaluationID int64     `json:"evaluation_id"`
	MarketID     int64     `json:"market_id"`
	Coin         string    `json:"coin"`
	Closes       time.Time `json:"closes"`
	At           time.Time `json:"at"`
	Bin          int       `json:"bin"`
	Tau          float64   `json:"tau"`
	YesBid       float64   `json:"yes_bid"`
	YesAsk       float64   `json:"yes_ask"`
	PModel       float64   `json:"p_model"`
	VolRatio     float64   `json:"vol_ratio"`
}

// h15Market is one market of H15's five series closing in the span, and whether it has a result.
type h15Market struct {
	MarketID int64     `json:"market_id"`
	Symbol   string    `json:"symbol"`
	Closes   time.Time `json:"closes"`
	Settled  bool      `json:"settled"`
}

// h15Row is one row of the long-shot protocol's printed query.
type h15Row struct {
	MarketID   int64
	Closes     time.Time
	Result     string
	At         time.Time
	YesBid     float64
	YesAsk     float64
	YesBidSize float64
	NoBidSize  float64
}

// source is what the tool reads: the record in production, a fake in the tests.
type source interface {
	rounds(ctx context.Context, s span) (settled, unsettled []round, err error)
	observations(ctx context.Context, s span, ids []int64) ([]observation, error)
	outcomes(ctx context.Context, ids []int64) (map[int64]string, error)
	h15Markets(ctx context.Context, s span) ([]h15Market, error)
	h15Rows(ctx context.Context, s span) ([]h15Row, error)
}

type reader struct{ db *store.Store }

// The population ("Data and the split"): the rounds of the 15-minute series closing in the span,
// with at least one evaluation row carrying a v3 view with p_model. settled have a yes or no
// result; unsettled have none, and are left out and listed.
func roundsSQL(settled bool) string {
	result := "m.result in ('yes', 'no')"
	if !settled {
		result = "(m.result is null or m.result not in ('yes', 'no'))"
	}
	return `
		select m.id, i.underlying, m.closes_at
		  from market m join instrument i on i.id = m.instrument_id
		 where i.symbol in (` + symbolList() + `)
		   and m.closes_at >= $1 and m.closes_at < $2
		   and ` + result + `
		   and exists (select 1 from evaluation e
		                where e.market_id = m.id and e.at >= $1 - interval '20 minutes' and e.at < $2
		                  and (e.model->'v3'->>'ok')::boolean and e.model->'v3'->>'p_model' is not null)
		 order by m.closes_at, m.id`
}

func (r reader) rounds(ctx context.Context, s span) (settled, unsettled []round, err error) {
	err = r.db.ReadOnly(ctx, readBudget, func(q store.Querier) error {
		for _, want := range []bool{true, false} {
			rows, err := q.Query(ctx, roundsSQL(want), s.From, s.To)
			if err != nil {
				return fmt.Errorf("rounds: %w", err)
			}
			for rows.Next() {
				var x round
				if err := rows.Scan(&x.MarketID, &x.Coin, &x.Closes); err != nil {
					rows.Close()
					return err
				}
				x.Closes = x.Closes.UTC()
				if want {
					settled = append(settled, x)
				} else {
					unsettled = append(unsettled, x)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("rounds: %w", err)
			}
		}
		return nil
	})
	return settled, unsettled, err
}

// observationsSQL is "Rows": per round and bin, the LAST evaluation row with a two-sided book and
// a v3 view. The e.at bounds hold every row of a round closing in the span (a round opens 15
// minutes before its close), so they prune evaluation's partitions and change no row.
const observationsSQL = `
	select distinct on (market_id, bin) id, market_id, coin, closes_at, at, bin, tau, yes_bid, yes_ask, p_model, vol_ratio
	  from (select e.id, e.market_id, i.underlying as coin, m.closes_at, e.at,
	               extract(epoch from (m.closes_at - e.at))::float8 as tau,
	               (e.quotes->>'yes_bid')::float8 as yes_bid, (e.quotes->>'yes_ask')::float8 as yes_ask,
	               (e.model->'v3'->>'p_model')::float8 as p_model, (e.model->'v3'->>'vol_ratio')::float8 as vol_ratio
	          from evaluation e
	          join market m on m.id = e.market_id
	          join instrument i on i.id = m.instrument_id
	         where m.id = any($1)
	           and e.at >= $2 - interval '20 minutes' and e.at < $3
	           and e.at < m.closes_at and e.at >= m.closes_at - interval '900 seconds'
	           and (e.model->'v3'->>'ok')::boolean and e.model->'v3'->>'p_model' is not null
	           and (e.quotes->>'yes_bid')::numeric > 0
	           and (e.quotes->>'yes_bid')::numeric < (e.quotes->>'yes_ask')::numeric
	           and (e.quotes->>'yes_ask')::numeric < 1) x
	  cross join lateral (select ` + binExpr + ` as bin) b
	 order by market_id, bin, at desc, id desc`

func (r reader) observations(ctx context.Context, s span, ids []int64) ([]observation, error) {
	var out []observation
	err := r.db.ReadOnly(ctx, readBudget, func(q store.Querier) error {
		rows, err := q.Query(ctx, observationsSQL, ids, s.From, s.To)
		if err != nil {
			return fmt.Errorf("observations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var o observation
			var vol *float64
			if err := rows.Scan(&o.EvaluationID, &o.MarketID, &o.Coin, &o.Closes, &o.At, &o.Bin, &o.Tau, &o.YesBid, &o.YesAsk, &o.PModel, &vol); err != nil {
				return err
			}
			if vol == nil {
				// The journal writes vol_ratio with every view that is ok (engine.View.Journal).
				// A row without it is not the record the protocol describes: stop, do not skip.
				return fmt.Errorf("observations: evaluation %d has a v3 view with no vol_ratio", o.EvaluationID)
			}
			o.VolRatio, o.Closes, o.At = *vol, o.Closes.UTC(), o.At.UTC()
			out = append(out, o)
		}
		return rows.Err()
	})
	return out, err
}

// outcomes reads the results, and is called only once the attempt is committed.
func (r reader) outcomes(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	err := r.db.ReadOnly(ctx, readBudget, func(q store.Querier) error {
		rows, err := q.Query(ctx, `select id, result from market where id = any($1) and result in ('yes', 'no')`, ids)
		if err != nil {
			return fmt.Errorf("outcomes: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var result string
			if err := rows.Scan(&id, &result); err != nil {
				return err
			}
			out[id] = result
		}
		return rows.Err()
	})
	return out, err
}

// h15Markets is every market of H15's five series closing in the span, and whether it has a
// result: a window counts only when its markets all have one (long-shot protocol, section 4).
func (r reader) h15Markets(ctx context.Context, s span) ([]h15Market, error) {
	var out []h15Market
	err := r.db.ReadOnly(ctx, readBudget, func(q store.Querier) error {
		rows, err := q.Query(ctx, `
			select m.id, i.symbol, m.closes_at, coalesce(m.result in ('yes', 'no'), false)
			  from market m join instrument i on i.id = m.instrument_id
			 where i.symbol in (`+symbolList()+`) and m.closes_at >= $1 and m.closes_at < $2
			 order by m.closes_at, m.id`, s.From, s.To)
		if err != nil {
			return fmt.Errorf("h15 markets: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m h15Market
			if err := rows.Scan(&m.MarketID, &m.Symbol, &m.Closes, &m.Settled); err != nil {
				return err
			}
			m.Closes = m.Closes.UTC()
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// h15SQL is the query printed in section 2 of docs/longshot-protocol.md, verbatim. Its bounds
// are "closes_at > $1 and closes_at <= $2"; the span is [From, To), and closes are whole seconds,
// so $1 is From less a microsecond and $2 is To less a microsecond (h15Rows).
const h15SQL = `
select distinct on (e.market_id) e.market_id, m.closes_at, m.result, e.at,
       (e.quotes->>'yes_bid')::numeric as yes_bid, (e.quotes->>'yes_ask')::numeric as yes_ask,
       coalesce((e.quotes->'yes_bids'->0->>1)::numeric, 0) as yes_bid_size,
       coalesce((e.quotes->'no_bids'->0->>1)::numeric, 0) as no_bid_size
  from evaluation e
  join market m on m.id = e.market_id
  join instrument i on i.id = m.instrument_id
 where i.symbol in ('KXBTC15M','KXETH15M','KXSOL15M','KXXRP15M','KXDOGE15M')
   and m.result in ('yes','no')
   and m.closes_at > $1 and m.closes_at <= $2
   and e.at >= m.closes_at - interval '300 seconds' and e.at < m.closes_at - interval '240 seconds'
 order by e.market_id, e.at;`

func (r reader) h15Rows(ctx context.Context, s span) ([]h15Row, error) {
	var out []h15Row
	err := r.db.ReadOnly(ctx, readBudget, func(q store.Querier) error {
		rows, err := q.Query(ctx, strings.TrimSuffix(h15SQL, ";"), s.From.Add(-time.Microsecond), s.To.Add(-time.Microsecond))
		if err != nil {
			return fmt.Errorf("h15 rows: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var x h15Row
			var yb, ya, ybs, nbs *float64
			if err := rows.Scan(&x.MarketID, &x.Closes, &x.Result, &x.At, &yb, &ya, &ybs, &nbs); err != nil {
				return err
			}
			x.Closes, x.At = x.Closes.UTC(), x.At.UTC()
			x.YesBid, x.YesAsk, x.YesBidSize, x.NoBidSize = orZero(yb), orZero(ya), orZero(ybs), orZero(nbs)
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

// orZero reads a numeric the query may leave NULL (a snapshot without that key): 0 fails every
// eligibility test that reads it, which is what "not in the sample" means.
func orZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
