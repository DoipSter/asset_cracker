// Package app is the live service: ingest, one engine runner, HTTP. cmd/assetcracker is a thin
// main over this package. Simulated money only; no real orders.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/config"
	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/health"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/runner"
	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/doipster/asset_cracker/service/internal/web"
)

// Run is the service. version is the build stamp (set by deploy/pi/deploy.sh).
func Run(version string) error {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()
	if err := db.EnsurePartitions(ctx, time.Now()); err != nil {
		return err
	}
	all, err := db.ActiveInstruments(ctx)
	if err != nil {
		return fmt.Errorf("instruments: %w", err)
	}
	instruments, ladders := splitLadders(all)

	started := time.Now()
	latest := coinbase.NewLatest()
	var wg sync.WaitGroup

	products := map[string]int64{}
	for _, in := range instruments {
		if in.Source == "coinbase" && in.Kind == "spot" {
			products[in.Symbol] = in.ID
		}
	}
	client := kalshi.NewClient(cfg.UserAgent)

	var coins []runner.Coin3
	coinOf := map[string]string{}
	for _, in := range instruments {
		if in.Source != "kalshi" || in.Kind != "binary_contract" {
			continue
		}
		if trade, set := in.Spec["trade"].(bool); set && !trade {
			slog.Info("recording only, no strategies", "series", in.Symbol)
			continue
		}
		num := func(key string, fallback float64) float64 {
			if v, ok := in.Spec[key].(float64); ok {
				return v
			}
			return fallback
		}
		priceFrom, _ := in.Spec["price_from"].(string)
		coins = append(coins, runner.Coin3{Coin: in.Underlying, Series: in.Symbol, Product: strings.TrimPrefix(priceFrom, "coinbase:"),
			Cal: engine.Calibration{OffsetPct: num("index_offset_pct", 0.000057), SDPct: num("index_sd_pct", 0.000144),
				DefaultSigma: num("default_sigma", 8e-5), Decimals: int(num("decimals", 2))}})
		coinOf[in.Symbol] = in.Underlying
	}
	faults := runner.Faults{Every: cfg.V3Faults.Every, Run: cfg.V3Faults.Run, Kind: cfg.V3Faults.Kind, DelayCommit: cfg.V3Faults.DelayCommit, Ops: cfg.V3Faults.Ops}
	switch {
	case faults.On() && !runner.FaultInjectionBuilt:
		slog.Warn("AC_V3_FAIL_* is set and IGNORED: this binary was not built with the faultinject tag")
	case faults.On() && !strings.HasSuffix(cfg.DatabaseName(), "_dev"):
		return fmt.Errorf("fault injection is switched on against database %q, which is not a *_dev one: refusing to start", cfg.DatabaseName())
	}
	run3, err := runner.NewRunner3(ctx, runner.WrapStore3(db, faults), coins, runner.Options3{On: cfg.V3, DatabaseName: cfg.DatabaseName()})
	if err != nil {
		return err
	}
	held := []int64{}
	if run3 != nil {
		held = run3.BucketIDs()
		run3.Seed(ctx, client, cfg.UserAgent)
		wg.Add(1)
		go func() { defer wg.Done(); run3.Run(ctx) }()
	}

	var ticksWritten atomic.Int64
	if len(products) > 0 {
		trades := make(chan coinbase.Trade, 4096)
		names := make([]string, 0, len(products))
		for p := range products {
			names = append(names, p)
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			coinbase.Stream(ctx, cfg.UserAgent, names, latest, func(t coinbase.Trade) {
				if run3 != nil {
					safely("observe", func() { run3.Observe(t) })
				}
				select {
				case trades <- t:
				default:
					slog.Warn("tick buffer full; dropping a print", "product", t.Product)
				}
			})
		}()
		go func() {
			defer wg.Done()
			writeTicks(ctx, db, products, trades, &ticksWritten)
		}()
	}

	pollers := map[string]*kalshi.Poller{}
	for _, in := range instruments {
		if in.Source != "kalshi" || in.Kind != "binary_contract" {
			continue
		}
		round := 900 * time.Second
		if s, ok := in.Spec["round_seconds"].(float64); ok && s > 0 {
			round = time.Duration(s) * time.Second
		}
		priceFrom, _ := in.Spec["price_from"].(string)
		p := &kalshi.Poller{
			Client: client, Series: in.Symbol, Round: round,
			Sink: &sink{db: db, instrumentID: in.ID, latest: latest, product: strings.TrimPrefix(priceFrom, "coinbase:"),
				run: run3, coin: coinOf[in.Symbol]},
		}
		pollers[in.Symbol] = p
		wg.Add(1)
		go func() { defer wg.Done(); p.Run(ctx) }()
	}

	pace := kalshi.NewPacer(200 * time.Millisecond)
	for i, in := range ladders {
		priceFrom, _ := in.Spec["price_from"].(string)
		rec := &kalshi.LadderRecorder{Client: client, Series: in.Symbol, Pace: pace,
			Sink:  &ladderSink{db: db, instrumentID: in.ID},
			Price: freshPrice(latest, strings.TrimPrefix(priceFrom, "coinbase:")),
			Start: 5*time.Second + time.Duration(i)*12*time.Second}
		wg.Add(1)
		go func() { defer wg.Done(); rec.Run(ctx) }()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				if err := db.EnsurePartitions(ctx, now); err != nil {
					slog.Error("partitions", "err", err)
				}
			}
		}
	}()

	if len(products) > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); recordCandles(ctx, db, cfg.UserAgent, products) }()
	}

	gate := gateSettings(cfg.Gate)
	slog.Info("asset cracker service running", "version", version, "instruments", len(instruments), "ladders_recorded", len(ladders), "health", "http://"+cfg.HTTPAddr+"/healthz")
	slog.Info("promotion gate", "min_edge_per_dollar", gate.MinEdgePerDollar, "power", gate.Power, "max_drawdown_cents", gate.MaxDrawdownCents)

	live := func(hctx context.Context) (map[string]any, bool) {
		ok := db.Ping(hctx) == nil
		prices := map[string]any{}
		feedFresh := len(products) == 0
		for p := range products {
			if t, seen := latest.Get(p); seen {
				age := time.Since(t.ReceivedAt)
				prices[p] = map[string]any{"price": t.Price, "age_seconds": age.Seconds()}
				feedFresh = feedFresh || age < time.Minute
			}
		}
		ok = ok && feedFresh
		rounds := map[string]kalshi.Status{}
		for series, p := range pollers {
			st := p.Status()
			rounds[series] = st
			ok = ok && time.Since(st.LastQuotesAt) < time.Minute
		}
		return map[string]any{
			"ok": ok, "version": version, "uptime_seconds": time.Since(started).Seconds(),
			"mode":          "simulation on live data, no real orders: one engine",
			"ticks_written": ticksWritten.Load(), "prices": prices, "rounds": rounds,
		}, ok
	}

	src := web.Sources{Release: version}
	capital := ledgerCapital(db, held)
	src.Books = func() ([]runner.Book, store.Capital, bool) {
		cap, ok := capital()
		if run3 == nil {
			return nil, cap, ok
		}
		book := runner.Book{Engine: "v3", Halted: "a panic escaped the engine's Book"}
		safely("book", func() { book = run3.Book() })
		return []runner.Book{book}, cap, ok
	}
	src.Markers = func(coin string, since float64) []runner.Marker {
		var out []runner.Marker
		if run3 != nil {
			safely("markers", func() { out = run3.Markers(coin, since) })
		}
		return out
	}
	for _, in := range instruments {
		if in.Source != "kalshi" || in.Kind != "binary_contract" {
			continue
		}
		priceFrom, _ := in.Spec["price_from"].(string)
		seconds, ok := in.Spec["round_seconds"].(float64)
		if !ok || seconds <= 0 {
			seconds = 900
		}
		src.Feeds = append(src.Feeds, web.Feed{Coin: in.Underlying, Product: strings.TrimPrefix(priceFrom, "coinbase:"), Series: in.Symbol, RoundSeconds: seconds})
	}
	src.Price = func(product string) (float64, float64, bool) {
		t, seen := latest.Get(product)
		p, err := strconv.ParseFloat(t.Price, 64)
		return p, time.Since(t.ReceivedAt).Seconds(), seen && err == nil
	}
	src.Round = func(series string) (kalshi.Status, bool) {
		p, ok := pollers[series]
		if !ok {
			return kalshi.Status{}, false
		}
		return p.Status(), true
	}

	var (
		healthMu sync.Mutex
		healthAt time.Time
		healthOK bool
	)
	src.Healthy = func() bool {
		healthMu.Lock()
		defer healthMu.Unlock()
		if time.Since(healthAt) > 5*time.Second {
			hctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, healthOK = live(hctx)
			cancel()
			healthAt = time.Now()
		}
		return healthOK
	}

	var recordingMu sync.Mutex
	recording := web.Recording{Started: time.Now()}
	src.Recording = func() web.Recording {
		recordingMu.Lock()
		defer recordingMu.Unlock()
		return recording
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				err := snapshotValues(ctx, db, src, run3, now)
				switch {
				case errors.Is(err, runner.ErrAwaitingSettlement):
					slog.Warn("value snapshot skipped", "why", err)
				case err != nil:
					slog.Error("value snapshot not written", "err", err)
				}
				recordingMu.Lock()
				if err == nil {
					recording.LastWritten, recording.LastProblem = now, ""
				} else {
					recording.LastProblem = err.Error()
				}
				recordingMu.Unlock()
			}
		}
	}()

	err = health.Serve(ctx, cfg.HTTPAddr,
		func(hctx context.Context) (any, bool) { return live(hctx) },
		func(mux *http.ServeMux) {
			web.Routes(mux, db, cfg.UserAgent, func() map[string]any {
				doc, _ := live(context.Background())
				if run3 != nil {
					sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					doc["engine"] = run3.Snapshot(sctx)
					cancel()
				} else {
					doc["engine"] = map[string]any{"state": "absent", "on": cfg.V3, "reason": "no tradable series"}
				}
				return doc
			}, src)
			web.AnalysisRoutes(ctx, mux, db, gate)
		})
	stop()
	wg.Wait()
	return err
}
