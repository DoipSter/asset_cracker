// Package candles fetches Coinbase's daily and hourly candles and stores the complete ones
// (migration 0015). RECORD ONLY. It never prints or logs a price: only counts.
package candles

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// MaxPerRequest is the most candles Coinbase returns for one request [READ: its documentation,
// quoted in the brief; not probed].
const MaxPerRequest = 300

// Granularities are the candle lengths recorded: daily, hourly and one minute, the finest Coinbase
// serves. Every other length (5, 10, 15, 30 minutes, 6 hours) is built from the minutes exactly:
// first open, highest high, lowest low, last close, summed volume. Coinbase serves 60, 300, 900,
// 3600, 21600 and 86400 seconds and refuses 600 and 1800 [READ: its API, 2026-09-21].
// Coarsest first: each round brings every product's daily and hourly candles up to date before
// it spends its time on minutes, of which the first load is about 5,300 requests a product.
var Granularities = []time.Duration{24 * time.Hour, time.Hour, time.Minute}

// Settle is how long after a candle's end it is first treated as complete. [ASSUMPTION, not
// measured: how late Coinbase can still change a finished candle is unknown.] It is checked, not
// trusted: the hourly writer re-fetches the newest stored candles, and InsertCandles counts every
// stored one that Coinbase has since changed, which the writer logs.
const Settle = time.Minute

// Complete reports whether the candle starting at `start` had ended, and settled, by `now`.
func Complete(start time.Time, g time.Duration, now time.Time) bool {
	return !start.Add(g + Settle).After(now)
}

// LastComplete is the start of the newest candle that is complete at `now`. Candles start on
// multiples of their length since the Unix epoch.
func LastComplete(g time.Duration, now time.Time) time.Time {
	return now.Add(-Settle).Truncate(g).Add(-g).UTC()
}

// Window is one request: the candles whose START is in [First, Last], both ends included, as
// Coinbase reads a range.
type Window struct{ First, Last time.Time }

// Windows pages backwards from the newest complete candle at `now` to the first candle starting
// at or after `since`, newest window first. Each window holds at most `max` candle starts, and
// consecutive windows neither overlap nor leave a gap.
func Windows(since, now time.Time, g time.Duration, max int) []Window {
	if max < 1 {
		max = 1
	}
	first := since.Truncate(g)
	if first.Before(since) {
		first = first.Add(g)
	}
	var out []Window
	for last := LastComplete(g, now); !last.Before(first); {
		start := last.Add(-time.Duration(max-1) * g)
		if start.Before(first) {
			start = first
		}
		out = append(out, Window{First: start.UTC(), Last: last.UTC()})
		last = start.Add(-g)
	}
	return out
}

// Overlap is how many of the newest stored candles the hourly writer fetches again, so that one
// Coinbase changed after it was stored is counted (Counts.Differing) rather than never seen.
const Overlap = 2

// Recent is the one request the hourly writer makes for a product and granularity: from Overlap
// candles before the newest stored one (`latest`, if `stored`) to the newest complete one, and
// never more than MaxPerRequest candles. ok is false when there is nothing to fetch. A gap older
// than that one request is left for cmd/candles; the caller can tell by w.First > latest + g.
func Recent(latest time.Time, stored bool, g time.Duration, now time.Time) (w Window, ok bool) {
	last := LastComplete(g, now)
	first := last.Add(-time.Duration(MaxPerRequest-1) * g)
	if stored {
		if f := latest.Add(-Overlap * g).UTC(); f.After(first) {
			first = f
		}
	}
	if first.After(last) {
		return Window{}, false
	}
	return Window{First: first, Last: last}, true
}

// HistoryFrom is where the recorded history starts: the warm-up of the spot protocol's draft
// begins here. A fixed date, not "three years before today", so every run agrees on it.
var HistoryFrom = time.Date(2023, 9, 21, 0, 0, 0, 0, time.UTC)

// Catchup is every request needed to bring one product and granularity up to date: from Overlap
// candles before the newest stored one (or from `since` when none is stored) to the newest
// complete candle, OLDEST window first. Oldest first matters: a round cut short leaves the
// stored history contiguous, and the next round resumes from its newest candle. This is how the
// service fills the history itself, as the same role that writes everything else.
func Catchup(since, latest time.Time, stored bool, g time.Duration, now time.Time) []Window {
	from := since
	if stored {
		if f := latest.Add(-Overlap * g); f.After(from) {
			from = f
		}
	}
	ws := Windows(from, now, g, MaxPerRequest)
	for i, j := 0, len(ws)-1; i < j; i, j = i+1, j-1 {
		ws[i], ws[j] = ws[j], ws[i]
	}
	return ws
}

// Keep returns the bars worth storing: complete at `now`, one per start (the first copy wins),
// oldest first.
func Keep(bars []coinbase.Bar, g time.Duration, now time.Time) []coinbase.Bar {
	seen := map[int64]bool{}
	out := make([]coinbase.Bar, 0, len(bars))
	for _, b := range bars {
		if !Complete(b.Start, g, now) || seen[b.Start.Unix()] {
			continue
		}
		seen[b.Start.Unix()] = true
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// Fetch is one candles request (coinbase.Bars, with the user agent bound).
type Fetch func(ctx context.Context, product string, granularityS int, first, last time.Time) ([]coinbase.Bar, error)

// Inserter stores candles (store.Store).
type Inserter interface {
	InsertCandles(ctx context.Context, cs []store.Candle) (inserted, differing int, err error)
}

// Counts are what a run reports. Never a price.
type Counts struct {
	Requests  int // requests that answered
	Fetched   int // bars returned
	Complete  int // of those, complete and distinct
	Inserted  int // of those, new
	Differing int // already stored with values Coinbase has since changed
	Failed    int // requests or inserts that failed
}

// Add sums counts.
func (c *Counts) Add(o Counts) {
	c.Requests += o.Requests
	c.Fetched += o.Fetched
	c.Complete += o.Complete
	c.Inserted += o.Inserted
	c.Differing += o.Differing
	c.Failed += o.Failed
}

func (c Counts) String() string {
	return fmt.Sprintf("requests=%d fetched=%d complete=%d inserted=%d differing=%d failed=%d",
		c.Requests, c.Fetched, c.Complete, c.Inserted, c.Differing, c.Failed)
}

// Step fetches one window of one product and stores its complete candles, skipping stored ones.
// Completeness is judged, and fetched_at stamped, at the clock's time when the answer arrived.
func Step(ctx context.Context, fetch Fetch, db Inserter, clock func() time.Time, instrumentID int64, product string, g time.Duration, w Window) (Counts, error) {
	var c Counts
	gs := int(g / time.Second)
	bars, err := fetch(ctx, product, gs, w.First, w.Last)
	now := clock()
	if err != nil {
		c.Failed++
		return c, fmt.Errorf("%s %ds %s..%s: %w", product, gs, w.First.Format(time.RFC3339), w.Last.Format(time.RFC3339), err)
	}
	c.Requests++
	c.Fetched = len(bars)
	keep := Keep(bars, g, now)
	c.Complete = len(keep)
	rows := make([]store.Candle, len(keep))
	for i, b := range keep {
		rows[i] = store.Candle{InstrumentID: instrumentID, GranularityS: gs, At: b.Start,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, FetchedAt: now}
	}
	if c.Inserted, c.Differing, err = db.InsertCandles(ctx, rows); err != nil {
		c.Failed++
		return c, fmt.Errorf("%s %ds: insert: %w", product, gs, err)
	}
	return c, nil
}
