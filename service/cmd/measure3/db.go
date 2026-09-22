package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// The reads. Every one runs inside store.ReadOnly: a READ ONLY transaction that, where the
// connected role is a member of assetcracker_ro, takes that role. The M1 query is the protocol's
// text, verbatim (3.1); the others are the queries the protocol gives only in words (section 1,
// 3.2, section 7), written down here as 0.1 says they would be.

const readBudget = 10 * time.Minute

type reader struct{ db *store.Store }

func symbolList() string {
	quoted := make([]string, len(symbols))
	for i, s := range symbols {
		quoted[i] = "'" + s + "'"
	}
	return strings.Join(quoted, ", ")
}

// The M1 query of 3.1, as printed. $1 and $2 are epoch seconds.
var m1SQL = `
select distinct on (e.market_id, floor(extract(epoch from (m.closes_at - e.at)) / 15))
       extract(epoch from m.closes_at)::bigint as w, i.symbol as coin, e.market_id, e.at,
       (e.model->'v3'->>'p_model')::float8 as p,
       ((e.quotes->>'yes_bid')::float8 + (e.quotes->>'yes_ask')::float8) / 2 as q,
       (m.result = 'yes')::int as y,
       (e.quotes->>'yes_ask')::float8 as yes_ask, (e.quotes->>'no_ask')::float8 as no_ask,
       (e.quotes->>'yes_bid')::float8 as yes_bid, (e.quotes->>'no_bid')::float8 as no_bid
  from evaluation e
  join market m on m.id = e.market_id
  join instrument i on i.id = m.instrument_id
 where e.at >= to_timestamp($1) and e.at < to_timestamp($2)
   and i.symbol in (` + symbolList() + `)
   and m.result in ('yes','no')
   and (e.model->'v3'->>'ok')::boolean and e.model->'v3'->>'p_model' is not null
   and (e.quotes->>'yes_bid')::numeric > 0 and (e.quotes->>'no_bid')::numeric > 0
 order by e.market_id, floor(extract(epoch from (m.closes_at - e.at)) / 15), e.at, e.id`

// The existence query of section 1: the M1 WHERE clause with the time bounds replaced by
// e.at >= t0, returning market ids only; never p, a quote or a result as a value.
var existenceSQL = `
select distinct e.market_id
  from evaluation e
  join market m on m.id = e.market_id
  join instrument i on i.id = m.instrument_id
 where e.at >= $1
   and i.symbol in (` + symbolList() + `)
   and m.result in ('yes','no')
   and (e.model->'v3'->>'ok')::boolean and e.model->'v3'->>'p_model' is not null
   and (e.quotes->>'yes_bid')::numeric > 0 and (e.quotes->>'no_bid')::numeric > 0`

// coverage is s_c per coin: the at of its first snapshot carrying the forecast key (section 1).
func (r reader) coverage(ctx context.Context, t0 time.Time) (coverage, error) {
	out := coverage{}
	err := r.db.ReadOnly(ctx, readBudget, func(q store.Querier) error {
		rows, err := q.Query(ctx, `
			select i.symbol, min(e.at)
			  from evaluation e
			  join market m on m.id = e.market_id
			  join instrument i on i.id = m.instrument_id
			 where e.at >= $1 and i.symbol in (`+symbolList()+`) and e.model ? 'v3'
			 group by i.symbol`, t0)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sym string
			var at time.Time
			if err := rows.Scan(&sym, &at); err != nil {
				return err
			}
			out[sym] = at.UTC()
		}
		return rows.Err()
	})
	return out, err
}

// markets is every market of the five series closing after t0, with whether the existence
// query finds a scored row for it.
func (r reader) markets(ctx context.Context, t0 time.Time) ([]marketRow, error) {
	var out []marketRow
	err := r.db.ReadOnly(ctx, readBudget, func(q store.Querier) error {
		scored := map[int64]bool{}
		rows, err := q.Query(ctx, existenceSQL, t0)
		if err != nil {
			return fmt.Errorf("existence query: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			scored[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = q.Query(ctx, `
			select m.id, i.symbol, m.closes_at, coalesce(m.result, '')
			  from market m join instrument i on i.id = m.instrument_id
			 where i.symbol in (`+symbolList()+`) and m.closes_at > $1
			 order by m.closes_at, i.symbol`, t0)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m marketRow
			if err := rows.Scan(&m.ID, &m.Symbol, &m.ClosesAt, &m.Result); err != nil {
				return err
			}
			m.ClosesAt = m.ClosesAt.UTC()
			m.Scored = scored[m.ID]
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// m1Rows runs the M1 query between two epoch seconds.
func (r reader) m1Rows(ctx context.Context, from, to float64) ([]row, error) {
	var out []row
	err := r.db.ReadOnly(ctx, readBudget, func(q store.Querier) error {
		rows, err := q.Query(ctx, m1SQL, from, to)
		if err != nil {
			return fmt.Errorf("M1 query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var x row
			var at time.Time
			if err := rows.Scan(&x.W, &x.Coin, &x.MarketID, &at, &x.P, &x.Q, &x.Y, &x.YesAsk, &x.NoAsk, &x.YesBid, &x.NoBid); err != nil {
				return err
			}
			x.At = at.UTC()
			x.AtUnix = float64(at.UnixNano()) / 1e9
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

type nextKey struct {
	MarketID int64
	AtUnix   float64
}

// nextQuotes finds, for each M1 row, the first evaluation row of the same market with
// row.at < at <= row.at + 3 s, by at then id (3.2), and returns its four top-of-book quotes.
func (r reader) nextQuotes(ctx context.Context, rows []row) (map[nextKey]nextQuote, error) {
	out := map[nextKey]nextQuote{}
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]int64, len(rows))
	ats := make([]time.Time, len(rows))
	for i, x := range rows {
		ids[i], ats[i] = x.MarketID, x.At
	}
	err := r.db.ReadOnly(ctx, readBudget, func(q store.Querier) error {
		res, err := q.Query(ctx, `
			select k.market_id, k.at, n.quotes
			  from unnest($1::bigint[], $2::timestamptz[]) as k(market_id, at)
			  cross join lateral (
			      select e2.quotes
			        from evaluation e2
			       where e2.market_id = k.market_id and e2.at > k.at and e2.at <= k.at + interval '3 seconds'
			       order by e2.at, e2.id
			       limit 1) n`, ids, ats)
		if err != nil {
			return fmt.Errorf("next snapshots: %w", err)
		}
		defer res.Close()
		for res.Next() {
			var id int64
			var at time.Time
			var raw []byte
			if err := res.Scan(&id, &at, &raw); err != nil {
				return err
			}
			var qs map[string]any
			if err := json.Unmarshal(raw, &qs); err != nil {
				return err
			}
			out[nextKey{id, float64(at.UnixNano()) / 1e9}] = nextQuote{Found: true,
				YesAsk: num(qs["yes_ask"]), NoAsk: num(qs["no_ask"]), YesBid: num(qs["yes_bid"]), NoBid: num(qs["no_bid"])}
		}
		return res.Err()
	})
	return out, err
}

func num(v any) float64 {
	switch x := v.(type) {
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case float64:
		return x
	}
	return 0
}

// trials is the registry's row count (section 7).
func (r reader) trials(ctx context.Context) (int, error) {
	var n int
	err := r.db.ReadOnly(ctx, readBudget, func(q store.Querier) error {
		return q.QueryRow(ctx, `select count(*) from strategy_version`).Scan(&n)
	})
	return n, err
}
