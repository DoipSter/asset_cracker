package readsurface

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The windowed reads of what is recorded: no computation beyond binning, every one paged.

const descInstruments = `What is recorded, and how far it reaches: every instrument with the first and last stored ` +
	`candle of each length, the first and last trade print, and its markets (rounds or ladder legs). Call it first, to ` +
	`plan windows that hold data. Takes no input.`

type noInput struct{}

func (r *Surface) instruments(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, store.Table, error) {
	t, err := r.read(ctx, Timeout, func(q store.Querier) (store.Table, error) {
		return store.QueryTable(ctx, q, `
			select i.symbol, i.kind, i.underlying, s.code as source, i.active,
			       (select count(*) from candle c where c.instrument_id = i.id and c.granularity_s = 3600)  as candles_1h,
			       (select min(at)  from candle c where c.instrument_id = i.id and c.granularity_s = 3600)  as candles_1h_first,
			       (select max(at)  from candle c where c.instrument_id = i.id and c.granularity_s = 3600)  as candles_1h_last,
			       (select count(*) from candle c where c.instrument_id = i.id and c.granularity_s = 86400) as candles_1d,
			       (select min(at)  from candle c where c.instrument_id = i.id and c.granularity_s = 86400) as candles_1d_first,
			       (select max(at)  from candle c where c.instrument_id = i.id and c.granularity_s = 86400) as candles_1d_last,
			       (select min(at)  from price_tick t where t.instrument_id = i.id) as ticks_first,
			       (select max(at)  from price_tick t where t.instrument_id = i.id) as ticks_last,
			       (select count(*) from market m where m.instrument_id = i.id) as markets,
			       (select count(*) from market m where m.instrument_id = i.id and m.result is not null) as markets_settled,
			       (select min(closes_at) from market m where m.instrument_id = i.id) as markets_first_close,
			       (select max(closes_at) from market m where m.instrument_id = i.id) as markets_last_close
			  from instrument i join source s on s.id = i.source_id
			 order by i.id`)
	})
	if err != nil {
		return nil, store.Table{}, err
	}
	t.Note = "Times are UTC. Candle times are the bar's START. A candle covers [at, at + granularity_s). " +
		"Ticks are Coinbase trade prints; markets are Kalshi rounds (kind binary_contract) or ladder legs (binary_ladder)."
	return nil, t, nil
}

// ---- candles --------------------------------------------------------------------------------

const descCandles = `Stored Coinbase candles (open, high, low, close, volume) of a spot instrument inside a window, oldest ` +
	`first. granularity_s is 3600 (hourly) or 86400 (daily), the two lengths recorded since 2023-09-21. resample_s, a ` +
	`multiple of granularity_s, bins them on the way out (14400 for 4-hour bars, 604800 for weeks from daily) so no ` +
	`length needs storing. Paged: at most limit rows, and next continues.`

type candlesInput struct {
	Window
	Paged
	GranularityS int `json:"granularity_s,omitempty" jsonschema:"3600 (hourly, the default) or 86400 (daily)"`
	ResampleS    int `json:"resample_s,omitempty" jsonschema:"bin width in seconds, a multiple of granularity_s; 0 returns the stored bars"`
}

func (r *Surface) candles(ctx context.Context, _ *mcp.CallToolRequest, in candlesInput) (*mcp.CallToolResult, store.Table, error) {
	from, to, err := r.bounds(in.Window)
	if err != nil {
		return nil, store.Table{}, err
	}
	g, err := granularity(in.GranularityS)
	if err != nil {
		return nil, store.Table{}, err
	}
	if in.ResampleS != 0 && (in.ResampleS < g || in.ResampleS%g != 0) {
		return nil, store.Table{}, fmt.Errorf("resample_s must be a multiple of granularity_s (%d); %d is not", g, in.ResampleS)
	}
	lim := limit(in.Limit)
	after, paging, err := cursor(in.After)
	if err != nil {
		return nil, store.Table{}, err
	}
	t, err := r.read(ctx, Timeout, func(q store.Querier) (store.Table, error) {
		id, err := lookup(ctx, q, in.Symbol)
		if err != nil {
			return store.Table{}, err
		}
		if in.ResampleS == 0 {
			if paging {
				from = after.Add(time.Nanosecond) // strictly after the cursor
			}
			t, err := store.QueryTable(ctx, q, `
				select at, open::float8 as open, high::float8 as high, low::float8 as low, close::float8 as close, volume::float8 as volume
				  from candle
				 where instrument_id = $1 and granularity_s = $2 and at >= $3 and at < $4
				 order by at limit $5`, id.ID, g, from, to, lim+1)
			if err != nil {
				return t, err
			}
			t = page(t, lim)
			t.Note = fmt.Sprintf("Stored %d-second candles, oldest first. at is the bar's START (UTC); it covers [at, at+%ds).", g, g)
			return t, nil
		}
		// Resampled: the scan is bounded by the window, so the window is cut to `limit` bins first.
		w := spanOf(in.ResampleS)
		if paging {
			from = after.Add(w) // the cursor is a bin start this surface issued
		}
		from, to2, cut := capWindow(from, to, w, lim)
		t, err := store.QueryTable(ctx, q, `
			select bin as at,
			       (array_agg(open  order by at))[1]::float8      as open,
			       max(high)::float8                              as high,
			       min(low)::float8                               as low,
			       (array_agg(close order by at desc))[1]::float8 as close,
			       sum(volume)::float8                            as volume,
			       count(*)::int                                  as bars
			  from (select date_bin(make_interval(secs => $5::int), at, to_timestamp(0)) as bin, at, open, high, low, close, volume
			          from candle
			         where instrument_id = $1 and granularity_s = $2 and at >= $3 and at < $4) c
			 group by bin order by bin`, id.ID, g, from, to2, in.ResampleS)
		if err != nil {
			return t, err
		}
		if cut {
			t.Truncated = true
			if n := len(t.Rows); n > 0 {
				if at, ok := t.Rows[n-1][0].(time.Time); ok {
					t.Next = at.Format(time.RFC3339Nano)
				}
			}
		}
		t.Note = fmt.Sprintf("%d-second candles binned to %d seconds from the Unix epoch, oldest first. at is the bin's START (UTC); "+
			"bars is how many stored candles fell in the bin (fewer than %d at the window's edges or across a gap).", g, in.ResampleS, in.ResampleS/g)
		return t, nil
	})
	return nil, t, err
}

// ---- bars from trade prints -----------------------------------------------------------------

const descBars = `Bars built from the recorded Coinbase trade prints of a spot instrument: open, high, low, close, volume ` +
	`and trade count per bucket_s seconds (1 for one-second bars, 60 for minutes, 300 ...). The window is cut so that at ` +
	`most limit buckets are scanned; next continues. Prints exist only from the day the service started recording ` +
	`(see instruments: ticks_first), not from history.`

type barsInput struct {
	Window
	Paged
	BucketS int `json:"bucket_s" jsonschema:"bucket width in seconds, at least 1"`
}

func (r *Surface) bars(ctx context.Context, _ *mcp.CallToolRequest, in barsInput) (*mcp.CallToolResult, store.Table, error) {
	from, to, err := r.bounds(in.Window)
	if err != nil {
		return nil, store.Table{}, err
	}
	if in.BucketS < 1 {
		return nil, store.Table{}, fmt.Errorf("bucket_s must be at least 1 second")
	}
	lim := limit(in.Limit)
	after, paging, err := cursor(in.After)
	if err != nil {
		return nil, store.Table{}, err
	}
	w := spanOf(in.BucketS)
	if paging {
		from = after.Add(w)
	}
	from, to2, cut := capWindow(from, to, w, lim)
	t, err := r.read(ctx, Timeout, func(q store.Querier) (store.Table, error) {
		id, err := lookup(ctx, q, in.Symbol)
		if err != nil {
			return store.Table{}, err
		}
		if id.Kind != "spot" {
			return store.Table{}, fmt.Errorf("%s is %s; trade prints are recorded for spot instruments only", in.Symbol, id.Kind)
		}
		return store.QueryTable(ctx, q, `
			select bin as at,
			       (array_agg(price order by at, received_at))[1]::float8           as open,
			       max(price)::float8                                                as high,
			       min(price)::float8                                                as low,
			       (array_agg(price order by at desc, received_at desc))[1]::float8 as close,
			       coalesce(sum(size), 0)::float8                                    as volume,
			       count(*)::int                                                     as trades
			  from (select date_bin(make_interval(secs => $4::int), at, to_timestamp(0)) as bin, at, received_at, price, size
			          from price_tick
			         where instrument_id = $1 and at >= $2 and at < $3) t
			 group by bin order by bin`, id.ID, from, to2, in.BucketS)
	})
	if err != nil {
		return nil, store.Table{}, err
	}
	if cut {
		t.Truncated = true
		t.Next = to2.Add(-w).Format(time.RFC3339Nano)
	}
	t.Note = fmt.Sprintf("Buckets of %d s from the Unix epoch, oldest first; at is the bucket's START (UTC). A bucket with no "+
		"trade is absent, not zero. Prices are the exchange's trade prices; volume is the summed size in the base coin.", in.BucketS)
	return nil, t, nil
}

// ---- the Kalshi book, sampled ---------------------------------------------------------------

const descBook = `The recorded Kalshi order book of an instrument's rounds inside a window, thinned to the last snapshot in ` +
	`every step_s seconds per round: the spot price the model saw, best yes/no bid and ask, cumulative bid size within ` +
	`1c, 3c and 5c of the best on each side (recorded from 2026-09-21 07:29 UTC; null before), and the v3 model's ` +
	`probability and variance. One row per round per step, so several rows can share a time. Window cut to limit steps.`

type bookInput struct {
	Window
	Paged
	StepS int `json:"step_s" jsonschema:"sampling step in seconds, at least 1; the service records one snapshot a second"`
}

func (r *Surface) book(ctx context.Context, _ *mcp.CallToolRequest, in bookInput) (*mcp.CallToolResult, store.Table, error) {
	from, to, err := r.bounds(in.Window)
	if err != nil {
		return nil, store.Table{}, err
	}
	if in.StepS < 1 {
		return nil, store.Table{}, fmt.Errorf("step_s must be at least 1 second")
	}
	lim := limit(in.Limit)
	after, paging, err := cursor(in.After)
	if err != nil {
		return nil, store.Table{}, err
	}
	w := spanOf(in.StepS)
	if paging {
		from = after.Add(w)
	}
	from, to2, cut := capWindow(from, to, w, lim)
	t, err := r.read(ctx, Timeout, func(q store.Querier) (store.Table, error) {
		id, err := lookup(ctx, q, in.Symbol)
		if err != nil {
			return store.Table{}, err
		}
		if id.Kind == "spot" {
			return store.Table{}, fmt.Errorf("%s is a spot instrument and has no book here; name its Kalshi series (e.g. KXBTC15M)", in.Symbol)
		}
		return store.QueryTable(ctx, q, `
			with e as (
			    select e.at, e.market_id, e.underlying_price, e.quotes, e.model,
			           date_bin(make_interval(secs => $4::int), e.at, to_timestamp(0)) as bin,
			           row_number() over (partition by e.market_id, date_bin(make_interval(secs => $4::int), e.at, to_timestamp(0))
			                              order by e.at desc) as rn
			      from evaluation e
			      join market m on m.id = e.market_id
			     where m.instrument_id = $1 and m.closes_at >= $2 and e.at >= $2 and e.at < $3)
			select e.bin as at, m.ticker, m.strike::float8 as strike, m.closes_at, m.result,
			       e.underlying_price::float8                     as price,
			       (e.quotes->>'yes_bid')::float8                 as yes_bid,
			       (e.quotes->>'yes_ask')::float8                 as yes_ask,
			       (e.quotes->>'no_bid')::float8                  as no_bid,
			       (e.quotes->>'no_ask')::float8                  as no_ask,
			       (e.quotes->>'yes_bid_size')::float8            as yes_bid_size,
			       (e.quotes->>'no_bid_size')::float8             as no_bid_size,
			       (e.quotes->'yes_bid_depth'->>'1c')::float8     as yes_depth_1c,
			       (e.quotes->'yes_bid_depth'->>'3c')::float8     as yes_depth_3c,
			       (e.quotes->'yes_bid_depth'->>'5c')::float8     as yes_depth_5c,
			       (e.quotes->'no_bid_depth'->>'1c')::float8      as no_depth_1c,
			       (e.quotes->'no_bid_depth'->>'3c')::float8      as no_depth_3c,
			       (e.quotes->'no_bid_depth'->>'5c')::float8      as no_depth_5c,
			       (e.model->'v3'->>'p_model')::float8            as p_model_v3,
			       (e.model->'v3'->>'sigma2')::float8             as sigma2,
			       (e.model->>'price_age_s')::float8              as price_age_s
			  from e join market m on m.id = e.market_id
			 where e.rn = 1
			 order by e.bin, m.ticker`, id.ID, from, to2, in.StepS)
	})
	if err != nil {
		return nil, store.Table{}, err
	}
	if cut {
		t.Truncated = true
		t.Next = to2.Add(-w).Format(time.RFC3339Nano)
	}
	t.Note = fmt.Sprintf("Last snapshot per round in each %d s step, oldest first; at is the step's START (UTC). Prices are dollars "+
		"per contract (0.62 = 62c). Depth fields are contracts bid within 1c/3c/5c of the best bid on that side; null where not yet "+
		"recorded. sigma2 is the model's per-second log-return variance.", in.StepS)
	return nil, t, nil
}

// ---- markets --------------------------------------------------------------------------------

const descMarkets = `The markets of an instrument closing inside a window, oldest close first: ticker, strike, open and close ` +
	`times, and for settled ones the result (yes/no), the settlement value and when it settled. For the 15-minute series ` +
	`a market is one round; for a ladder (KXBTCD ...) one leg, several per expiry. Paged on (closes_at, ticker).`

type marketsInput struct {
	Window
	Paged
	SettledOnly bool `json:"settled_only,omitempty" jsonschema:"only markets with a result"`
}

func (r *Surface) markets(ctx context.Context, _ *mcp.CallToolRequest, in marketsInput) (*mcp.CallToolResult, store.Table, error) {
	from, to, err := r.bounds(in.Window)
	if err != nil {
		return nil, store.Table{}, err
	}
	lim := limit(in.Limit)
	// The cursor is "<closes_at RFC3339>|<ticker>": many ladder legs share one close.
	var afterAt time.Time
	afterTicker := ""
	if s := strings.TrimSpace(in.After); s != "" {
		at, ticker, ok := strings.Cut(s, "|")
		t, err := time.Parse(time.RFC3339Nano, at)
		if !ok || err != nil {
			return nil, store.Table{}, fmt.Errorf("after: %q is not a cursor this surface issued", in.After)
		}
		afterAt, afterTicker = t, ticker
	}
	t, err := r.read(ctx, Timeout, func(q store.Querier) (store.Table, error) {
		id, err := lookup(ctx, q, in.Symbol)
		if err != nil {
			return store.Table{}, err
		}
		if id.Kind == "spot" {
			return store.Table{}, fmt.Errorf("%s is a spot instrument and has no markets; name a Kalshi series", in.Symbol)
		}
		return store.QueryTable(ctx, q, `
			select m.closes_at, m.ticker, m.strike::float8 as strike, m.opens_at, m.result,
			       m.settlement_value::float8 as settlement_value, m.settled_at
			  from market m
			 where m.instrument_id = $1 and m.closes_at >= $2 and m.closes_at < $3
			   and ($4 or m.result is not null)
			   and ($5::timestamptz is null or (m.closes_at, m.ticker) > ($5::timestamptz, $6::text))
			 order by m.closes_at, m.ticker limit $7`,
			id.ID, from, to, !in.SettledOnly, nullTime(afterAt), afterTicker, lim+1)
	})
	if err != nil {
		return nil, store.Table{}, err
	}
	if len(t.Rows) > lim {
		t.Rows = t.Rows[:lim]
		t.Truncated = true
		last := t.Rows[len(t.Rows)-1]
		if at, ok := last[0].(time.Time); ok {
			t.Next = at.Format(time.RFC3339Nano) + "|" + fmt.Sprint(last[1])
		}
	}
	t.Note = "Ordered by (closes_at, ticker). strike is on Kalshi's index (CF Benchmarks), which runs slightly above Coinbase. " +
		"settlement_value is the index value the result was decided on. Unsettled markets have null result."
	return nil, t, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
