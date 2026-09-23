// Package app is the live service: ingest, one engine runner, HTTP. cmd/assetcracker is a thin
// main over this package. Simulated money only; no real orders.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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

// refuseScratchDatabase stops the service from recording into a scratch database. A name
// ending in _dev is assetcracker_dev: migration rehearsal and constraint tests, not a second
// market. The tape, the registry and the paper ledger live in assetcracker.
func refuseScratchDatabase(name string) error {
	if strings.HasSuffix(name, "_dev") {
		return fmt.Errorf("database %q is scratch; the service records into assetcracker", name)
	}
	return nil
}

// Run is the service. version is the build stamp (set by deploy/pi/deploy.sh).
func Run(version string) error {
	cfg := config.Load()
	if err := refuseScratchDatabase(cfg.DatabaseName()); err != nil {
		return err
	}
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
	ordersOn := cfg.V3
	if saved, set, err := db.OrdersSetting(ctx); err != nil {
		return fmt.Errorf("orders switch: %w", err)
	} else if set {
		ordersOn = saved
	}
	// One runner per market family, always: with nothing held a runner is observe-only, and the
	// buckets page can reload it into a trial without a process start. run3 holds the
	// 15-minute rounds; runL the daily and weekly ladders (2026-09-23), the same code over
	// store.FamilyLadders, fed by the ladder recorders below instead of the round pollers.
	run3, err := runner.NewRunner3(ctx, runner.WrapStore3(db, faults), coins, runner.Options3{On: ordersOn, DatabaseName: cfg.DatabaseName()})
	if err != nil {
		return err
	}
	run3.Seed(ctx, client, cfg.UserAgent)
	wg.Add(1)
	go func() { defer wg.Done(); run3.Run(ctx) }()
	runL, err := runner.NewRunner3(ctx, runner.WrapStore3(db, faults), coins,
		runner.Options3{On: ordersOn, DatabaseName: cfg.DatabaseName(), Family: store.FamilyLadders, Prefix: ladderPrefix})
	if err != nil {
		return fmt.Errorf("ladder runner: %w", err)
	}
	// The ladder model's long volatility, from the last sixty daily candles of each coin: what a
	// leg hours to days from its close is priced with. A coin without enough candles keeps the
	// fast estimate, and the log says so.
	for _, c := range coins {
		closes, err := db.DailyCloses(ctx, c.Product, 61)
		if sigma := engine.LongSigmaFromDaily(closes); err == nil && sigma > 0 {
			runL.SetLongSigma(c.Coin, sigma)
			slog.Info("ladder model: long volatility set", "coin", c.Coin, "daily_closes", len(closes), "sigma_per_sqrt_s", sigma, "daily_pct", sigma*math.Sqrt(86400)*100)
		} else {
			slog.Warn("ladder model: no long volatility; long legs are priced with the fast estimate", "coin", c.Coin, "daily_closes", len(closes), "err", err)
		}
	}
	runners := []*runner.Runner3{run3, runL}
	wg.Add(1)
	go func() { defer wg.Done(); runL.Run(ctx) }()
	// The volatility forecast (HAR-RV on the hourly candles) replaces the daily-closes sigma
	// above as soon as it is fitted, and every six hours after; it also watches the ladders.
	vol := newVolModel(db, coins, runL, version)
	wg.Add(1)
	go func() { defer wg.Done(); vol.run(ctx) }()

	// The tracked assets: what the home page lists and the trade stream follows, from the
	// instrument table, seeded and switched-on alike. Prints are RECORDED (price_tick) only
	// for the seeded products in `products`; a switched-on coin's prints give it a live price
	// and nothing else, as the assets page says (its candles are recorded by the supervisor).
	assets := newTracked(db, all)
	var ticksWritten atomic.Int64
	if len(assets.Products()) > 0 {
		trades := make(chan coinbase.Trade, 4096)
		wg.Add(2)
		go func() {
			defer wg.Done()
			coinbase.Stream(ctx, cfg.UserAgent, assets.Products, latest, func(t coinbase.Trade) {
				for _, r := range runners {
					safely("observe", func() { r.Observe(t) })
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
			writeTicks(ctx, db.InsertTicks, products, trades, &ticksWritten)
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
		// A ladder series whose coin the runners know is traded by the ladder runner in
		// simulation; one whose coin they do not (DOGE, switched off) is recorded only.
		var eng kalshi.LadderEngine
		if _, known := coinOf[in.Symbol]; known || knownCoin(coins, in.Underlying) {
			eng = runL
		}
		rec := &kalshi.LadderRecorder{Client: client, Series: in.Symbol, Pace: pace,
			Sink:   &ladderSink{db: db, instrumentID: in.ID},
			Price:  freshPrice(latest, strings.TrimPrefix(priceFrom, "coinbase:")),
			Start:  5*time.Second + time.Duration(i)*12*time.Second,
			Coin:   in.Underlying,
			Engine: eng,
			Watch:  vol}
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

	src := web.Sources{Release: version, Assets: assets.Assets}
	heldIDs := func() []int64 { // every bucket some runner holds; a reload changes the set
		var ids []int64
		for _, r := range runners {
			ids = append(ids, r.BucketIDs()...)
		}
		return ids
	}
	capital, dropCapital := ledgerCapital(db, heldIDs) // asked each time: a reload changes the held set
	src.Books = func() ([]runner.Book, store.Capital, bool) {
		cap, ok := capital()
		books := make([]runner.Book, 0, len(runners))
		for _, r := range runners {
			book := runner.Book{Engine: "v3", Halted: "a panic escaped the engine's Book"}
			safely("book", func() { book = r.Book() })
			books = append(books, book)
		}
		return books, cap, ok
	}
	src.Markers = func(coin string, since float64) []runner.Marker {
		var out []runner.Marker
		safely("markers", func() { out = run3.Markers(coin, since) }) // the home page charts the rounds
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
				if n, err := db.ApplyDuePaydays(ctx, now); err != nil {
					slog.Error("payday not applied", "err", err)
				} else if n > 0 {
					dropCapital()
					slog.Info("payday applied", "count", n)
				}
				err := snapshotValues(ctx, db, src, runners, now)
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
				sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				doc["engine"] = run3.Snapshot(sctx)
				doc["ladder_engine"] = runL.Snapshot(sctx)
				cancel()
				return doc
			}, src, web.Control{
				Key:     cfg.OperatorKey,
				Version: version,
				Build:   buildVersion,
				Presets: presetsJSON,
				Shape:   shapeOf,
				// A version lives in one family, so one runner holds its bucket: Reap asks each in
				// turn and the one that knows the version answers. Both refusing by name means no
				// runner holds it and no restake was asked for.
				Drop: dropCapital,
				Reap: func(rctx context.Context, versionID int64, restake bool) (web.Reaped, error) {
					var last error
					for _, r := range runners {
						rep, err := r.Reap(rctx, versionID, restake)
						if err == nil {
							dropCapital() // a bucket closed or opened: the next snapshot reads the ledger
							return web.Reaped{Bucket: rep.Bucket, ReapedCents: rep.ReapedCents, Next: rep.Next, Held: rep.Held, Ordering: rep.MayOrder}, nil
						}
						last = err
						var open runner.OpenPositions
						if errors.As(err, &open) {
							return web.Reaped{}, web.ReapRefused{Why: err.Error()}
						}
						if !errors.Is(err, runner.ErrNoHeldBucket) && !errors.Is(err, store.ErrNoBucketEver) && !errors.Is(err, store.ErrBucketHeld) {
							return web.Reaped{}, err
						}
					}
					if errors.Is(last, runner.ErrNoHeldBucket) {
						return web.Reaped{}, web.ReapRefused{Why: last.Error()}
					}
					return web.Reaped{}, last
				},
				// CloseWhenFlat asks each runner like Reap does; the one that holds the version
				// either reaps now or marks the bucket and says so.
				CloseWhenFlat: func(rctx context.Context, versionID int64) (web.Reaped, *web.Closing, error) {
					var last error
					for _, r := range runners {
						rep, plan, err := r.CloseWhenFlat(rctx, versionID)
						if err == nil {
							dropCapital() // a bucket closed, or will be: the next snapshot reads the ledger
							if plan != nil {
								return web.Reaped{}, &web.Closing{Bucket: plan.Bucket, Open: plan.Open}, nil
							}
							return web.Reaped{Bucket: rep.Bucket, ReapedCents: rep.ReapedCents, Held: rep.Held, Ordering: rep.MayOrder}, nil, nil
						}
						last = err
						if !errors.Is(err, runner.ErrNoHeldBucket) && !errors.Is(err, store.ErrNoBucketEver) && !errors.Is(err, store.ErrBucketHeld) {
							return web.Reaped{}, nil, err
						}
					}
					if errors.Is(last, runner.ErrNoHeldBucket) {
						return web.Reaped{}, nil, web.ReapRefused{Why: last.Error()}
					}
					return web.Reaped{}, nil, last
				},
				// Deploy knows the version's family from the registry, so only the runner that
				// holds that family is suspended for it.
				Deploy: func(rctx context.Context, family string, d store.Deploy) (web.Deployed, error) {
					for _, r := range runners {
						if r.Family() != family {
							continue
						}
						rep, err := r.Deploy(rctx, d)
						if err != nil {
							return web.Deployed{}, err
						}
						dropCapital() // a bucket opened: the next snapshot reads the ledger
						return web.Deployed{Bucket: rep.Bucket, SeedCents: rep.SeedCents, Source: d.Source, Held: rep.Held, Ordering: rep.MayOrder}, nil
					}
					return web.Deployed{}, store.ErrOtherFamily
				},
				EnvOn:  cfg.V3,
				Status: run3.OrdersStatus, // the switch is one row; both runners read it, and both apply it below
				Apply: func(on bool) string {
					eff := run3.SetOrders(on)
					runL.SetOrders(on)
					return eff
				},
				Hold: func() {
					for _, r := range runners {
						r.HoldForReset()
					}
				},
				Release: func() {
					for _, r := range runners {
						r.ReleaseAfterReset()
					}
					dropCapital() // the books are gone: the next snapshot reads the ledger, not last minute's figure
				},
				Abort: func() {
					for _, r := range runners {
						r.AbortReset()
					}
				},
				Reload: func(rctx context.Context) (int, int, error) {
					var held, ordering int
					var firstErr error
					for _, r := range runners {
						report, err := r.Reload(rctx)
						held, ordering = held+report.Held, ordering+report.MayOrder
						if err != nil && firstErr == nil {
							firstErr = err
						}
					}
					dropCapital() // the held set changed: likewise
					return held, ordering, firstErr
				},
			})
			web.AnalysisRoutes(ctx, mux, db, gate)
			startAssets(ctx, &wg, mux, db, cfg, pace, latest) // the searchable catalogue, /assets and its supervisor (assets.go)
		})
	stop()
	wg.Wait()
	return err
}
