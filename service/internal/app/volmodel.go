package app

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/runner"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// volModel is the service's volatility forecast and its ladder watcher (2026-09-23).
//
// Every refresh it reads each coin's hourly candles, builds the daily realised variances, fits
// HAR-RV (engine.FitHAR), scores the fit on the last ninety days it did not see against the
// sixty-day mean the ladders used before, and hands the ladder runner the forecast as its long
// volatility. It logs the fit, the holdout and the forecast, so the claim "a better sigma" is
// checked every six hours in the journal rather than asserted once.
//
// As the ladder recorders' watcher it reads each close's legs together: the lognormal the
// market's mids imply (engine.ImpliedFromLadder), set against the forecast, and any crossed pair
// net of fee (engine.Crossings). It writes what it sees to analysis_result under
// ladder.implied, one row per series and close every tenth minute, and logs a crossing that
// beats the fee when it first appears. It places no order: a crossing is two legs, and the
// engine trades one at a time.
type volModel struct {
	db     *store.Store
	coins  []runner.Coin3
	target *runner.Runner3 // the ladder runner: SetLongSigma
	stamp  string          // the release, written as code_sha

	mu        sync.Mutex
	forecast  map[string]float64 // coin -> daily variance forecast
	har       map[string]engine.HAR
	lastWrite map[string]time.Time // series|close -> last analysis_result row
	lastCross map[string]time.Time // series|low|high -> last warning
}

const (
	volHistory   = 3 * 365 * 24 * time.Hour // how far back the hourly candles are read
	volHoldout   = 90                       // days scored out of sample
	volEvery     = 6 * time.Hour
	watchEvery   = 10 * time.Minute
	crossWarnGap = 30 * time.Minute
)

func newVolModel(db *store.Store, coins []runner.Coin3, target *runner.Runner3, stamp string) *volModel {
	return &volModel{db: db, coins: coins, target: target, stamp: stamp,
		forecast: map[string]float64{}, har: map[string]engine.HAR{}, lastWrite: map[string]time.Time{}, lastCross: map[string]time.Time{}}
}

// refresh fits every coin once. A coin without enough history keeps whatever it had (at first,
// nothing: the runner's long sigma stays as set from the daily closes).
func (v *volModel) refresh(ctx context.Context) {
	for _, c := range v.coins {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		closes, err := v.db.HourlyCloses(rctx, c.Product, time.Now().Add(-volHistory))
		cancel()
		if err != nil {
			slog.Warn("vol model: hourly candles not read", "coin", c.Coin, "err", err)
			continue
		}
		hc := make([]engine.HourlyClose, len(closes))
		for i, h := range closes {
			hc[i] = engine.HourlyClose{At: h.At, Close: h.Close}
		}
		days := engine.DailyRealisedVariance(hc)
		rv := make([]float64, len(days))
		for i, d := range days {
			rv[i] = d.RV
		}
		h, err := engine.FitHAR(rv)
		if err != nil {
			slog.Warn("vol model: no HAR fit", "coin", c.Coin, "days", len(rv), "err", err)
			continue
		}
		f, err := h.Forecast(rv)
		if err != nil || !(f > 0) {
			slog.Warn("vol model: no forecast", "coin", c.Coin, "err", err)
			continue
		}
		sigma := engine.SigmaPerSqrtSecond(f)
		v.mu.Lock()
		v.forecast[c.Coin], v.har[c.Coin] = f, h
		v.mu.Unlock()
		if v.target != nil {
			v.target.SetLongSigma(c.Coin, sigma)
		}
		attrs := []any{"coin", c.Coin, "days", len(rv), "har_c", h.C, "har_day", h.Bd, "har_week", h.Bw, "har_month", h.Bm, "r2_in_sample", h.R2,
			"forecast_daily_pct", math.Sqrt(f) * 100, "sigma_per_sqrt_s", sigma}
		if ho, err := engine.HoldoutHAR(rv, volHoldout); err == nil {
			attrs = append(attrs, "holdout_days", ho.Days, "holdout_mae_log_har", ho.HAR, "holdout_mae_log_naive60", ho.Naive)
		}
		slog.Info("vol model: HAR-RV fitted; the ladder runner's long volatility is the forecast", attrs...)
	}
}

// run refreshes at start and every volEvery.
func (v *volModel) run(ctx context.Context) {
	v.refresh(ctx)
	t := time.NewTicker(volEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			v.refresh(ctx)
		}
	}
}

// Watch is the ladder recorders' hook (kalshi.LadderWatcher).
func (v *volModel) Watch(ctx context.Context, series, coin string, at, closes time.Time, in []kalshi.LadderLeg) {
	legs := make([]engine.Leg, len(in))
	for i, l := range in {
		legs[i] = engine.Leg{Ticker: l.Ticker, Strike: l.Strike, Bid: l.Bid, Ask: l.Ask}
	}
	tau := closes.Sub(at).Seconds()
	im := engine.ImpliedFromLadder(legs, tau)
	crossings := engine.Crossings(legs)

	v.mu.Lock()
	forecast := v.forecast[coin]
	key := series + "|" + closes.UTC().Format(time.RFC3339)
	due := at.Sub(v.lastWrite[key]) >= watchEvery
	if due {
		v.lastWrite[key] = at
	}
	var fresh []engine.Crossing
	for _, x := range crossings {
		if x.NetCents <= 0 {
			continue
		}
		k := series + "|" + x.Low.Ticker + "|" + x.High.Ticker
		if at.Sub(v.lastCross[k]) >= crossWarnGap {
			v.lastCross[k] = at
			fresh = append(fresh, x)
		}
	}
	v.mu.Unlock()

	for _, x := range fresh {
		slog.Warn("ladder watcher: crossed legs beat the fee (no order is placed; a pair is not one leg)",
			"series", series, "closes", closes.UTC().Format(time.RFC3339), "low", x.Low.Ticker, "low_ask", x.Low.Ask, "high", x.High.Ticker, "high_bid", x.High.Bid,
			"gap_cents", x.GapCents, "fee_cents", x.FeeCents, "net_cents", x.NetCents)
	}
	if !due {
		return
	}
	result := map[string]any{"legs_quoted": len(legs), "legs_fitted": im.Legs, "implied_ok": im.OK, "tau_s": tau}
	if !im.OK {
		result["why"] = im.Why
	} else {
		result["implied_sigma_per_sqrt_s"] = im.Sigma
		result["implied_daily_pct"] = im.Sigma * math.Sqrt(86400) * 100
		result["implied_median"] = im.Median
		result["fit_rmse"] = im.RMSE
		if forecast > 0 {
			fs := engine.SigmaPerSqrtSecond(forecast)
			result["forecast_daily_pct"] = fs * math.Sqrt(86400) * 100
			result["implied_over_forecast"] = im.Sigma / fs
		}
	}
	xs := make([]map[string]any, 0, len(crossings))
	for _, x := range crossings {
		xs = append(xs, map[string]any{"low": x.Low.Ticker, "low_ask": x.Low.Ask, "high": x.High.Ticker, "high_bid": x.High.Bid,
			"gap_cents": x.GapCents, "fee_cents": x.FeeCents, "net_cents": x.NetCents})
	}
	result["crossings"] = xs
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := v.db.InsertAnalysisResult(wctx, "ladder.implied", "service vol model (ladder watcher)", v.stamp,
		map[string]any{"series": series, "coin": coin, "closes": closes.UTC().Format(time.RFC3339)}, at, at, result)
	if err != nil {
		slog.Warn("ladder watcher: analysis row not written", "series", series, "err", err)
	}
}
