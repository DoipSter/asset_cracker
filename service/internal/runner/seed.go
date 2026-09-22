package runner

import (
	"context"
	"log/slog"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// seedData fetches what an engine is primed with at start: the last fifty finished one-minute
// closes, and for each of the last ten settled rounds how far Kalshi's settled value sat from
// our exchange's average over that round's final minute.
func seedData(ctx context.Context, client *kalshi.Client, userAgent, series, product string) (closes []float64, measured [][2]float64) {
	now := time.Now()
	if candles, err := coinbase.Candles(ctx, userAgent, product, 60, time.Time{}, time.Time{}); err == nil {
		if len(candles) > 50 {
			candles = candles[len(candles)-50:]
		}
		for _, c := range candles {
			if !c.Start.Add(time.Minute).After(now) { // drop the minute still in progress
				closes = append(closes, c.Close)
			}
		}
	} else {
		slog.Warn("could not seed volatility", "series", series, "err", err)
	}
	settled, err := client.SettledMarkets(ctx, series, 10)
	if err != nil || len(settled) == 0 {
		return closes, nil
	}
	type done struct {
		closes time.Time
		value  float64
	}
	var rounds []done
	lo, hi := now, time.Time{}
	for _, m := range settled {
		v, ok := engine.ParseAmount(m.ExpirationValue)
		c, cerr := m.Closes()
		if !ok || v <= 0 || cerr != nil {
			continue
		}
		rounds = append(rounds, done{c, v})
		if c.Before(lo) {
			lo = c
		}
		if c.After(hi) {
			hi = c
		}
	}
	if len(rounds) == 0 {
		return closes, nil
	}
	candles, err := coinbase.Candles(ctx, userAgent, product, 60, lo.Add(-3*time.Minute), hi.Add(2*time.Minute))
	if err != nil {
		return closes, nil
	}
	byStart := map[int64]coinbase.Candle{}
	for _, c := range candles {
		byStart[c.Start.Unix()] = c
	}
	for _, d := range rounds {
		if c, ok := byStart[d.closes.Unix()-60]; ok {
			if ours := (c.Open + c.Close) / 2; ours > 0 {
				measured = append(measured, [2]float64{unix(d.closes), d.value/ours - 1})
			}
		}
	}
	return closes, measured
}
