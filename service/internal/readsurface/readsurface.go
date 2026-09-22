// Package readsurface is the agents' read-only door to the market data and to the analyses
// computed from it: the MCP tools of `assetcracker mcp` (docs/mcp-read-surface.md).
//
// Every tool takes a WINDOW (an instrument, a start and an end) and answers from inside it,
// through the indexes the tables already carry, so no call reads more of a table than the window
// covers. Raw reads are capped and paged; summaries come back small whatever the window; the
// heavy lifting (returns, ranks, correlations) is done in Postgres where the rows are, and only
// the answer travels. Every call runs in a READ ONLY transaction with a statement timeout
// (store.ReadOnly): this package can change nothing, and it cannot hold a connection for long.
//
// Nothing here is money. The ledger, the journal and the strategy registry (TSK-42) are not in
// this package; they will join it under the same server when they are written.
package readsurface

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Caps every tool observes. A caller who wants more pages.
const (
	DefaultLimit = 500
	MaxLimit     = 2000
	// Timeout is the statement timeout of an ordinary read; Long is for the grid tools that
	// rank and correlate whole windows.
	Timeout = 10 * time.Second
	Long    = 30 * time.Second
)

// Surface holds what the tools need.
type Surface struct {
	db  *store.Store
	now func() time.Time
}

// New makes a surface over the store.
func New(db *store.Store) *Surface { return &Surface{db: db, now: time.Now} }

// Register adds every tool to an MCP server.
func Register(s *mcp.Server, db *store.Store) {
	r := New(db)
	r.register(s)
}

func (r *Surface) register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "instruments", Description: descInstruments}, r.instruments)
	mcp.AddTool(s, &mcp.Tool{Name: "candles", Description: descCandles}, r.candles)
	mcp.AddTool(s, &mcp.Tool{Name: "bars", Description: descBars}, r.bars)
	mcp.AddTool(s, &mcp.Tool{Name: "book", Description: descBook}, r.book)
	mcp.AddTool(s, &mcp.Tool{Name: "markets", Description: descMarkets}, r.markets)
	mcp.AddTool(s, &mcp.Tool{Name: "returns_summary", Description: descReturnsSummary}, r.returnsSummary)
	mcp.AddTool(s, &mcp.Tool{Name: "vol_profile", Description: descVolProfile}, r.volProfile)
	mcp.AddTool(s, &mcp.Tool{Name: "momentum_grid", Description: descMomentumGrid}, r.momentumGrid)
	mcp.AddTool(s, &mcp.Tool{Name: "features", Description: descFeatures}, r.features)
	mcp.AddTool(s, &mcp.Tool{Name: "analysis_results", Description: descAnalysisResults}, r.analysisResults)
}

// ---- the window every tool takes -----------------------------------------------------------

// Window is the part of an input every windowed tool shares.
type Window struct {
	Symbol string `json:"symbol" jsonschema:"instrument symbol as the instrument table spells it: BTC-USD, ETH-USD, SOL-USD, XRP-USD, DOGE-USD for spot; KXBTC15M and friends for the 15-minute rounds; KXBTCD and friends for the daily ladders"`
	From   string `json:"from" jsonschema:"start of the window, inclusive. RFC 3339 (2026-09-01T00:00:00Z), a date (2026-09-01), or relative to now: -7d, -36h, -2w"`
	To     string `json:"to,omitempty" jsonschema:"end of the window, exclusive. Same forms as from; default now"`
}

// Paged adds the row cap and the cursor.
type Paged struct {
	Limit int    `json:"limit,omitempty" jsonschema:"rows per call; default 500, at most 2000. When the window holds more, truncated is true and next carries the cursor"`
	After string `json:"after,omitempty" jsonschema:"continue a read: the next value a previous call returned"`
}

// bounds resolves the window's times. from is required; to defaults to now. A window that ends
// before it starts is an error, not an empty answer, because it is always a mistake.
func (r *Surface) bounds(w Window) (from, to time.Time, err error) {
	now := r.now().UTC()
	if strings.TrimSpace(w.From) == "" {
		return from, to, fmt.Errorf("from is required: every read is a window")
	}
	if from, err = parseTime(w.From, now); err != nil {
		return from, to, fmt.Errorf("from: %w", err)
	}
	if strings.TrimSpace(w.To) == "" {
		to = now
	} else if to, err = parseTime(w.To, now); err != nil {
		return from, to, fmt.Errorf("to: %w", err)
	}
	if !to.After(from) {
		return from, to, fmt.Errorf("the window ends (%s) before it starts (%s)", to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	return from, to, nil
}

// parseTime reads the forms the tools accept. Relative forms count back from now: -7d, -36h,
// -2w, -90m; "now" is now. Dates are midnight UTC.
func parseTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "now":
		return now, nil
	case strings.HasPrefix(s, "-") || strings.HasPrefix(s, "now-"):
		d, err := parseSpan(strings.TrimPrefix(strings.TrimPrefix(s, "now"), "-"))
		if err != nil {
			return time.Time{}, err
		}
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time: use RFC 3339, a date, or -7d / -36h", s)
}

// parseSpan reads 7d, 36h, 2w, 90m, 30s: one number, one unit.
func parseSpan(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty span")
	}
	unit := s[len(s)-1]
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a span: use 7d, 36h, 2w, 90m", s)
	}
	switch unit {
	case 's':
		return time.Duration(n * float64(time.Second)), nil
	case 'm':
		return time.Duration(n * float64(time.Minute)), nil
	case 'h':
		return time.Duration(n * float64(time.Hour)), nil
	case 'd':
		return time.Duration(n * 24 * float64(time.Hour)), nil
	case 'w':
		return time.Duration(n * 7 * 24 * float64(time.Hour)), nil
	}
	return 0, fmt.Errorf("%q is not a span: use 7d, 36h, 2w, 90m", s)
}

// limit applies the caps: zero means the default, more than the max is the max.
func limit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	}
	return n
}

// cursor reads a time cursor; empty means none.
func cursor(after string) (time.Time, bool, error) {
	if strings.TrimSpace(after) == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(after))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("after: %q is not a cursor this surface issued", after)
	}
	return t.UTC(), true, nil
}

// page trims a LIMIT+1 read to its page and sets the cursor from the row's first column, which
// every paged query puts its ordering time in.
func page(t store.Table, lim int) store.Table {
	if len(t.Rows) > lim {
		t.Rows = t.Rows[:lim]
		t.Truncated = true
		if at, ok := t.Rows[len(t.Rows)-1][0].(time.Time); ok {
			t.Next = at.Format(time.RFC3339Nano)
		}
	}
	return t
}

// instrument is what the tools need to know about a symbol.
type instrument struct {
	ID   int64
	Kind string
}

func lookup(ctx context.Context, q store.Querier, symbol string) (instrument, error) {
	var in instrument
	err := q.QueryRow(ctx, `select id, kind from instrument where symbol = $1`, strings.TrimSpace(symbol)).Scan(&in.ID, &in.Kind)
	if err != nil {
		return in, fmt.Errorf("no instrument %q: call instruments for the list", symbol)
	}
	return in, nil
}

// granularity checks a candle length: the two stored lengths, and nothing else, because a length
// that is not stored would silently return nothing.
func granularity(s int) (int, error) {
	switch s {
	case 0, 3600:
		return 3600, nil
	case 86400:
		return 86400, nil
	}
	return 0, fmt.Errorf("granularity_s must be 3600 (hourly) or 86400 (daily); %d is not stored", s)
}

// spanOf is a granularity as a duration.
func spanOf(secs int) time.Duration { return time.Duration(secs) * time.Second }

// capWindow shortens a window so that it holds at most lim bins of width w, for reads that
// aggregate before they limit: the scan is bounded by the window, not by the LIMIT. The start
// is first moved down onto the bin grid (bins start on multiples of w since the Unix epoch, as
// Postgres's date_bin with origin epoch has them), so that the window covers whole bins and
// exactly lim of them. It returns the aligned start and the new end, and says whether it cut;
// the caller sets Truncated and Next = the last bin start.
func capWindow(from, to time.Time, w time.Duration, lim int) (start, end time.Time, cut bool) {
	if w <= 0 {
		return from, to, false
	}
	start = alignDown(from, w)
	most := start.Add(w * time.Duration(lim))
	if most.Before(to) {
		return start, most, true
	}
	return start, to, false
}

// alignDown is the start of the bin of width w (from the Unix epoch) that holds t.
func alignDown(t time.Time, w time.Duration) time.Time {
	secs := int64(w / time.Second)
	if secs <= 0 {
		return t
	}
	u := t.Unix()
	return time.Unix(u-((u%secs)+secs)%secs, 0).UTC()
}

// read opens the read-only transaction and hands the table back.
func (r *Surface) read(ctx context.Context, timeout time.Duration, fn func(q store.Querier) (store.Table, error)) (store.Table, error) {
	var t store.Table
	err := r.db.ReadOnly(ctx, timeout, func(q store.Querier) error {
		var err error
		t, err = fn(q)
		return err
	})
	return t, err
}
