package app

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/doipster/asset_cracker/service/internal/candles"
	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// candleMinute is when, past each hour, the writer runs [CONVENTION]: after the hourly candle
// has ended and candles.Settle has passed.
const candleMinute = 5 * time.Minute

// recordCandles stores Coinbase's complete daily and hourly candles for the spot products, once
// at start and then once an hour (migration 0015). Each round catches up from the newest stored
// candle, or from candles.HistoryFrom when none is stored, so the service loads the history
// itself; cmd/candles does the same by hand where it is allowed to write. RECORD ONLY: nothing reads
// it but research. Not in the health check, the status document or the home page. A failure is
// logged and the next hour tries again; a panic is recovered here and never reaches anything else.
func recordCandles(ctx context.Context, db *store.Store, userAgent string, products map[string]int64) {
	fetch := func(ctx context.Context, product string, g int, first, last time.Time) ([]coinbase.Bar, error) {
		return coinbase.Bars(ctx, userAgent, product, g, first, last)
	}
	names := make([]string, 0, len(products))
	for p := range products {
		names = append(names, p)
	}
	sort.Strings(names)
	candleRound(ctx, db, fetch, names, products)
	for {
		now := time.Now()
		next := now.Truncate(time.Hour).Add(candleMinute)
		if !next.After(now) {
			next = next.Add(time.Hour)
		}
		t := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		candleRound(ctx, db, fetch, names, products)
	}
}

// candleRound is one hour's fetch: one request per product and granularity, a second apart.
func candleRound(ctx context.Context, db *store.Store, fetch candles.Fetch, names []string, products map[string]int64) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("candles: a panic was stopped; the next hour tries again", "panic", p)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute) // [CONVENTION] a round cut short resumes next hour: the minutes' first load takes several
	defer cancel()
	var total candles.Counts
	for _, g := range candles.Granularities { // coarsest first, for every product, then the next length
		for _, p := range names {
			gs := int(g / time.Second)
			latest, stored, err := db.LatestCandle(ctx, products[p], gs)
			if err != nil {
				slog.Error("candles: newest stored", "product", p, "granularity_s", gs, "err", err)
				total.Failed++
				continue
			}
			for _, w := range candles.Catchup(candles.HistoryFrom, latest, stored, g, time.Now()) {
				c, err := candles.Step(ctx, fetch, db, time.Now, products[p], p, g, w)
				total.Add(c)
				if err != nil {
					slog.Error("candles", "err", err)
					break // oldest first: stopping here keeps the stored history contiguous
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
	}
	slog.Info("candles", "requests", total.Requests, "inserted", total.Inserted, "differing", total.Differing, "failed", total.Failed)
	if total.Differing > 0 {
		slog.Warn("candles: Coinbase has changed candles already stored (they were kept as first stored); candles.Settle may be too short",
			"differing", total.Differing)
	}
}
