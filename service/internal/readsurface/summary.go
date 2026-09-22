package readsurface

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The summaries: computed in Postgres over the window, small on the way out whatever the window
// holds. Returns are natural-log close-over-previous-close of consecutive stored candles; a
// missing candle makes the next return span the gap (the store has none today: 100% fill).

const descReturnsSummary = `One line of statistics for a spot instrument's candle returns inside a window: bars, mean and ` +
	`sd of the per-bar log return (basis points), annualised volatility, total return, best and worst bar, fraction of ` +
	`up bars, skew. Cheap at any window; the way to characterise a period before reading it.`

type summaryInput struct {
	Window
	GranularityS int `json:"granularity_s,omitempty" jsonschema:"3600 (hourly, the default) or 86400 (daily)"`
}

func (r *Surface) returnsSummary(ctx context.Context, _ *mcp.CallToolRequest, in summaryInput) (*mcp.CallToolResult, store.Table, error) {
	from, to, err := r.bounds(in.Window)
	if err != nil {
		return nil, store.Table{}, err
	}
	g, err := granularity(in.GranularityS)
	if err != nil {
		return nil, store.Table{}, err
	}
	t, err := r.read(ctx, Timeout, func(q store.Querier) (store.Table, error) {
		id, err := lookup(ctx, q, in.Symbol)
		if err != nil {
			return store.Table{}, err
		}
		// One bar before the window so the first bar in it has a return.
		return store.QueryTable(ctx, q, `
			with r as (
			    select at, ln(close::float8 / lag(close::float8) over (order by at)) as r
			      from candle
			     where instrument_id = $1 and granularity_s = $2 and at >= $3::timestamptz - make_interval(secs => $2::int) and at < $4),
			w as (select * from r where at >= $3 and r is not null),
			m as (select count(*) as n, min(at) as first_at, max(at) as last_at,
			             avg(r) as mu, stddev_samp(r) as sd, avg(r*r) as m2, avg(r*r*r) as m3,
			             sum(r) as total, min(r) as lo, max(r) as hi,
			             avg(case when r > 0 then 1.0 else 0.0 end) as up
			        from w)
			select n as bars, first_at, last_at,
			       mu * 1e4                                        as mean_bp,
			       sd * 1e4                                        as sd_bp,
			       sd * sqrt(365.0 * 86400 / $2::int) * 100             as ann_vol_pct,
			       (exp(total) - 1) * 100                          as total_return_pct,
			       lo * 1e4                                        as worst_bar_bp,
			       hi * 1e4                                        as best_bar_bp,
			       up                                              as frac_up,
			       case when sd > 0 then (m3 - 3*mu*m2 + 2*mu*mu*mu) / (sd*sd*sd) end as skew
			  from m`, id.ID, g, from, to)
	})
	if err != nil {
		return nil, store.Table{}, err
	}
	t.Note = fmt.Sprintf("Per-bar natural-log returns of %d-second candles, close over previous close. bp = basis points (1e-4). "+
		"ann_vol_pct = sd × sqrt(bars per year) × 100. skew is the third standardised moment (population).", g)
	return nil, t, nil
}

// ---- vol profile ----------------------------------------------------------------------------

const descVolProfile = `Mean and sd of a spot instrument's per-bar log return by UTC hour of day (granularity_s 3600) or by ` +
	`day of week (0 = Sunday; granularity_s 86400), inside a window. The strongest structure in the stored history is ` +
	`the intraday volatility shape this returns: the hourly sd roughly doubles from 03-11 UTC to 14-15 UTC in every coin.`

type volProfileInput struct {
	Window
	GranularityS int    `json:"granularity_s,omitempty" jsonschema:"3600 (by hour of day, the default) or 86400 (by day of week)"`
	By           string `json:"by,omitempty" jsonschema:"hour or dow; defaults to hour for hourly candles and dow for daily"`
}

func (r *Surface) volProfile(ctx context.Context, _ *mcp.CallToolRequest, in volProfileInput) (*mcp.CallToolResult, store.Table, error) {
	from, to, err := r.bounds(in.Window)
	if err != nil {
		return nil, store.Table{}, err
	}
	g, err := granularity(in.GranularityS)
	if err != nil {
		return nil, store.Table{}, err
	}
	by := strings.ToLower(strings.TrimSpace(in.By))
	if by == "" {
		by = "hour"
		if g == 86400 {
			by = "dow"
		}
	}
	var field string
	switch by {
	case "hour":
		if g != 3600 {
			return nil, store.Table{}, fmt.Errorf("by hour needs hourly candles (granularity_s 3600)")
		}
		field = "hour"
	case "dow":
		field = "dow"
	default:
		return nil, store.Table{}, fmt.Errorf("by must be hour or dow")
	}
	t, err := r.read(ctx, Timeout, func(q store.Querier) (store.Table, error) {
		id, err := lookup(ctx, q, in.Symbol)
		if err != nil {
			return store.Table{}, err
		}
		// `field` is one of two literals this code chose; it is not a caller's string.
		return store.QueryTable(ctx, q, fmt.Sprintf(`
			with r as (
			    select at, ln(close::float8 / lag(close::float8) over (order by at)) as r
			      from candle
			     where instrument_id = $1 and granularity_s = $2 and at >= $3::timestamptz - make_interval(secs => $2::int) and at < $4)
			select extract(%s from at)::int as bucket, count(r)::int as bars,
			       avg(r) * 1e4 as mean_bp, stddev_samp(r) * 1e4 as sd_bp,
			       case when stddev_samp(r) > 0 then avg(r) / (stddev_samp(r) / sqrt(count(r))) end as t_mean
			  from r where at >= $3 and r is not null
			 group by 1 order by 1`, field), id.ID, g, from, to)
	})
	if err != nil {
		return nil, store.Table{}, err
	}
	unit := "UTC hour in which the bar STARTS"
	if field == "dow" {
		unit = "UTC day of week the bar starts (0 = Sunday)"
	}
	t.Note = fmt.Sprintf("bucket is the %s; the return is that bar's close over the previous close. bp = 1e-4. "+
		"t_mean is mean / (sd / sqrt(bars)) for the bucket alone; buckets are not independent across coins.", unit)
	return nil, t, nil
}

// ---- momentum grid --------------------------------------------------------------------------

const descMomentumGrid = `Does the past predict the future inside a window? For every (lookback, horizon) pair, in bars: the ` +
	`Pearson and Spearman correlation between the trailing lookback-bar return and the forward horizon-bar return, the ` +
	`costless long/short return mean(sign(past) × forward) in basis points, the mean forward return after an up and ` +
	`after a down past, and a naive t. symbol may be empty for all five spot coins pooled (ranks within coin). Signal ` +
	`bars lie in the window; the forward return may reach horizon bars past its end. At most 8 lookbacks × 4 horizons; ` +
	`the pooled three-year hourly grid is the one call that can hit the 30 s timeout: narrow the window or the grid.`

type momentumInput struct {
	Symbol       string `json:"symbol,omitempty" jsonschema:"one spot instrument, or empty for all spot instruments pooled"`
	From         string `json:"from" jsonschema:"start of the signal window, inclusive. RFC 3339, a date, or -90d"`
	To           string `json:"to,omitempty" jsonschema:"end of the signal window, exclusive; default now"`
	GranularityS int    `json:"granularity_s,omitempty" jsonschema:"3600 (hourly, the default) or 86400 (daily)"`
	Lookbacks    []int  `json:"lookbacks,omitempty" jsonschema:"trailing windows in bars; default 1,3,7,14,30 (daily) or 1,4,24,168 (hourly); at most 8"`
	Horizons     []int  `json:"horizons,omitempty" jsonschema:"forward windows in bars; default 1,7 (daily) or 1,4,24 (hourly); at most 4"`
}

func (r *Surface) momentumGrid(ctx context.Context, _ *mcp.CallToolRequest, in momentumInput) (*mcp.CallToolResult, store.Table, error) {
	from, to, err := r.bounds(Window{Symbol: in.Symbol, From: in.From, To: in.To})
	if err != nil {
		return nil, store.Table{}, err
	}
	g, err := granularity(in.GranularityS)
	if err != nil {
		return nil, store.Table{}, err
	}
	lbs, hzs := in.Lookbacks, in.Horizons
	if len(lbs) == 0 {
		lbs = []int{1, 4, 24, 168}
		if g == 86400 {
			lbs = []int{1, 3, 7, 14, 30}
		}
	}
	if len(hzs) == 0 {
		hzs = []int{1, 4, 24}
		if g == 86400 {
			hzs = []int{1, 7}
		}
	}
	if err := checkBars("lookbacks", lbs, 8); err != nil {
		return nil, store.Table{}, err
	}
	if err := checkBars("horizons", hzs, 4); err != nil {
		return nil, store.Table{}, err
	}
	maxLb, maxHz := maxOf(lbs), maxOf(hzs)
	t, err := r.read(ctx, Long, func(q store.Querier) (store.Table, error) {
		var ids []int64
		if strings.TrimSpace(in.Symbol) == "" {
			rows, err := q.Query(ctx, `select id from instrument where kind = 'spot' and active order by id`)
			if err != nil {
				return store.Table{}, err
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return store.Table{}, err
				}
				ids = append(ids, id)
			}
		} else {
			id, err := lookup(ctx, q, in.Symbol)
			if err != nil {
				return store.Table{}, err
			}
			ids = []int64{id.ID}
		}
		// b: the bars of the padded window, numbered per instrument. u: every (bar, cell) with its
		// past and forward return, by self-join on the row number. r: ranks within instrument and
		// cell, for Spearman.
		return store.QueryTable(ctx, q, `
			with b as (
			    select instrument_id, at, close::float8 as c,
			           row_number() over (partition by instrument_id order by at) as n
			      from candle
			     where instrument_id = any($1::bigint[]) and granularity_s = $2
			       and at >= $3::timestamptz - make_interval(secs => $2::int * $5::int) and at < $4::timestamptz + make_interval(secs => $2::int * $6::int)),
			g as (select l.lb, h.hz from unnest($7::int[]) as l(lb) cross join unnest($8::int[]) as h(hz)),
			u as (
			    select b.instrument_id, b.at, g.lb, g.hz, ln(b.c / p.c) as m, ln(f.c / b.c) as f
			      from b cross join g
			      join b p on p.instrument_id = b.instrument_id and p.n = b.n - g.lb
			      join b f on f.instrument_id = b.instrument_id and f.n = b.n + g.hz
			     where b.at >= $3 and b.at < $4),
			r as (
			    select *, rank() over (partition by instrument_id, lb, hz order by m) as rm,
			              rank() over (partition by instrument_id, lb, hz order by f) as rf
			      from u)
			select lb as lookback_bars, hz as horizon_bars, count(*)::int as n,
			       corr(m, f)                                            as pearson,
			       corr(rm, rf)                                          as spearman,
			       avg(sign(m) * f) * 1e4                                as long_short_bp,
			       (avg(f) filter (where m > 0)) * 1e4                   as fwd_if_up_bp,
			       (avg(f) filter (where m <= 0)) * 1e4                  as fwd_if_down_bp,
			       case when stddev_samp(sign(m) * f) > 0
			            then avg(sign(m) * f) / (stddev_samp(sign(m) * f) / sqrt(count(*)::float8 / hz)) end as t_naive
			  from r group by lb, hz order by hz, lb`,
			ids, g, from, to, maxLb, maxHz, lbs, hzs)
	})
	if err != nil {
		return nil, store.Table{}, err
	}
	t.Note = fmt.Sprintf("Bars of %d s. Past = ln(close_t / close_(t-lookback)); forward = ln(close_(t+horizon) / close_t). "+
		"bp = 1e-4 per horizon. t_naive treats n/horizon overlapping windows as independent and ignores the correlation "+
		"between coins (pairwise ~0.7 daily: five coins are ~1.3 independent series); halve it for pooled reads.", g)
	return nil, t, nil
}

func checkBars(what string, ns []int, most int) error {
	if len(ns) > most {
		return fmt.Errorf("%s: at most %d", what, most)
	}
	seen := map[int]bool{}
	for _, n := range ns {
		if n < 1 || n > 2000 {
			return fmt.Errorf("%s: %d bars is out of range (1..2000)", what, n)
		}
		if seen[n] {
			return fmt.Errorf("%s: %d repeats", what, n)
		}
		seen[n] = true
	}
	return nil
}

func maxOf(ns []int) int {
	m := 0
	for _, n := range ns {
		if n > m {
			m = n
		}
	}
	return m
}

// ---- features -------------------------------------------------------------------------------

const descFeatures = `The per-bar feature vector a multi-horizon model would consume, for a spot instrument inside a window: ` +
	`the close, the trailing log return over each lookback in bars, the sd of one-bar returns over the last 7 and 30 bars ` +
	`and their ratio, the volume z-score against the previous 30 bars, and the bar's UTC hour and day of week. The ` +
	`lookback padding is read by the server, so the rows returned are exactly the window. Paged.`

type featuresInput struct {
	Window
	Paged
	GranularityS int   `json:"granularity_s,omitempty" jsonschema:"3600 (hourly, the default) or 86400 (daily)"`
	Lookbacks    []int `json:"lookbacks,omitempty" jsonschema:"trailing return windows in bars; default 1,3,7,14,30,60,90; at most 8"`
}

func (r *Surface) features(ctx context.Context, _ *mcp.CallToolRequest, in featuresInput) (*mcp.CallToolResult, store.Table, error) {
	from, to, err := r.bounds(in.Window)
	if err != nil {
		return nil, store.Table{}, err
	}
	g, err := granularity(in.GranularityS)
	if err != nil {
		return nil, store.Table{}, err
	}
	lbs := in.Lookbacks
	if len(lbs) == 0 {
		lbs = []int{1, 3, 7, 14, 30, 60, 90}
	}
	if err := checkBars("lookbacks", lbs, 8); err != nil {
		return nil, store.Table{}, err
	}
	sort.Ints(lbs)
	pad := maxOf(lbs)
	if pad < 30 {
		pad = 30 // the volume z and the 30-bar vol look back 30 bars
	}
	lim := limit(in.Limit)
	after, paging, err := cursor(in.After)
	if err != nil {
		return nil, store.Table{}, err
	}
	start := from
	if paging {
		start = after.Add(time.Nanosecond)
	}
	// The lookbacks are validated integers, so they may be spliced into the statement as literals:
	// lag() takes its offset per call and a window per column.
	var cols []string
	for _, lb := range lbs {
		cols = append(cols, fmt.Sprintf("ln(c / lag(c, %d) over w) as ret_%d", lb, lb))
	}
	sql := fmt.Sprintf(`
		with b as (
		    select at, close::float8 as c, volume::float8 as v
		      from candle
		     where instrument_id = $1 and granularity_s = $2 and at >= $3::timestamptz - make_interval(secs => $2::int * $4::int) and at < $5),
		w as (
		    select at, c, v, ln(c / lag(c, 1) over w) as r1, %s
		      from b window w as (order by at)),
		x as (
		    select *,
		           stddev_samp(r1) over (order by at rows between 6 preceding and current row)  as vol_7,
		           stddev_samp(r1) over (order by at rows between 29 preceding and current row) as vol_30,
		           (v - avg(v) over (order by at rows between 30 preceding and 1 preceding))
		             / nullif(stddev_samp(v) over (order by at rows between 30 preceding and 1 preceding), 0) as volume_z_30
		      from w)
		select at, c as close, %s, vol_7, vol_30, vol_7 / nullif(vol_30, 0) as vol_ratio_7_30, volume_z_30,
		       extract(hour from at)::int as hour_utc, extract(dow from at)::int as dow
		  from x
		 where at >= $6 and at < $5
		 order by at limit $7`, strings.Join(cols, ", "), retNames(lbs))
	t, err := r.read(ctx, Timeout, func(q store.Querier) (store.Table, error) {
		id, err := lookup(ctx, q, in.Symbol)
		if err != nil {
			return store.Table{}, err
		}
		return store.QueryTable(ctx, q, sql, id.ID, g, from, pad, to, start, lim+1)
	})
	if err != nil {
		return nil, store.Table{}, err
	}
	t = page(t, lim)
	t.Note = fmt.Sprintf("Bars of %d s; at is the bar's START (UTC). ret_N = ln(close / close N bars earlier). vol_N = sample sd of "+
		"the one-bar log return over the last N bars including this one. volume_z_30 = (volume - mean of previous 30) / their sd. "+
		"Nulls where the stored history does not reach back far enough.", g)
	return nil, t, nil
}

func retNames(lbs []int) string {
	names := make([]string, len(lbs))
	for i, lb := range lbs {
		names[i] = fmt.Sprintf("ret_%d", lb)
	}
	return strings.Join(names, ", ")
}

// ---- analysis results -----------------------------------------------------------------------

const descAnalysisResults = `Analyses already computed and written down (the analysis_result table, migration 0016): ` +
	`what was computed (key), by what (source, code_sha), over which window and parameters, when, and, with ` +
	`include_result, the result itself as JSON. Newest first. Read this before recomputing: a grid over three years of ` +
	`hourly bars that someone already ran is here. Keys are dotted paths, e.g. candle.momentum_grid.daily.`

type analysisResultsInput struct {
	Key           string `json:"key,omitempty" jsonschema:"key or key prefix to match, e.g. candle. or candle.vol_profile"`
	Since         string `json:"since,omitempty" jsonschema:"only results computed at or after this time; RFC 3339, a date, or -30d"`
	ID            int64  `json:"id,omitempty" jsonschema:"one result by id; then include_result defaults to true"`
	IncludeResult bool   `json:"include_result,omitempty" jsonschema:"return the result JSON, not only its size; default false unless id is given"`
	Limit         int    `json:"limit,omitempty" jsonschema:"at most this many; default 20, at most 100"`
}

func (r *Surface) analysisResults(ctx context.Context, _ *mcp.CallToolRequest, in analysisResultsInput) (*mcp.CallToolResult, store.Table, error) {
	since := time.Time{}
	if strings.TrimSpace(in.Since) != "" {
		t, err := parseTime(in.Since, r.now().UTC())
		if err != nil {
			return nil, store.Table{}, fmt.Errorf("since: %w", err)
		}
		since = t
	}
	lim := in.Limit
	switch {
	case lim <= 0:
		lim = 20
	case lim > 100:
		lim = 100
	}
	full := in.IncludeResult || in.ID != 0
	t, err := r.read(ctx, Timeout, func(q store.Querier) (store.Table, error) {
		return store.QueryTable(ctx, q, `
			select id, key, source, code_sha, computed_at, window_from, window_to, params,
			       pg_column_size(result) as result_bytes,
			       case when $5 then result end as result
			  from analysis_result
			 where ($1 = '' or key like $1 || '%')
			   and computed_at >= $2
			   and ($3 = 0 or id = $3)
			 order by computed_at desc, id desc limit $4`,
			strings.TrimSpace(in.Key), since, in.ID, lim, full)
	})
	if err != nil {
		return nil, store.Table{}, err
	}
	t.Note = "Append-only: a result is never edited, a rerun is a new row. result is null unless include_result (or id) was given; " +
		"result_bytes says how large it is before you ask for it."
	return nil, t, nil
}
