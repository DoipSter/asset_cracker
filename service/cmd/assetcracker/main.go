// Command assetcracker is the Asset Cracker service.
//
// Today it records market data: Coinbase trade prints and, once a second, Kalshi's live quotes
// for each open 15-minute round, plus how every round settles. It runs no strategies and has
// no order code. See docs/platform-brief.md for where it is going.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/config"
	"github.com/doipster/asset_cracker/service/internal/health"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/kalshi15m2"
	"github.com/doipster/asset_cracker/service/internal/runner"
	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/doipster/asset_cracker/service/internal/web"
)

var version = "dev" // set at build time by deploy/pi/deploy.sh

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
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
	instruments, err := db.ActiveInstruments(ctx)
	if err != nil {
		return fmt.Errorf("instruments: %w", err)
	}

	started := time.Now()
	latest := coinbase.NewLatest()
	var wg sync.WaitGroup

	// Coinbase: one connection for every spot instrument.
	products := map[string]int64{}
	for _, in := range instruments {
		if in.Source == "coinbase" && in.Kind == "spot" {
			products[in.Symbol] = in.ID
		}
	}
	// One runner per Kalshi series: the six strategies, live, in simulation.
	client := kalshi.NewClient(cfg.UserAgent)
	runners := map[string]*runner.Runner{} // by series
	byProduct := map[string][]*runner.Runner{}
	for _, in := range instruments {
		if in.Source != "kalshi" || in.Kind != "binary_contract" {
			continue
		}
		if trade, set := in.Spec["trade"].(bool); set && !trade {
			slog.Info("recording only, no strategies", "series", in.Symbol)
			continue // market data is still captured: the poller's sink works without a runner
		}
		priceFrom, _ := in.Spec["price_from"].(string) // "coinbase:BTC-USD"
		product := strings.TrimPrefix(priceFrom, "coinbase:")
		num := func(key string, fallback float64) float64 {
			if v, ok := in.Spec[key].(float64); ok {
				return v
			}
			return fallback
		}
		r, err := runner.New(ctx, db, in.Symbol, in.Underlying, product,
			num("index_offset_pct", 0.000057), num("index_sd_pct", 0.000144), num("default_sigma", 8e-5))
		if err != nil {
			return err
		}
		r.Seed(ctx, client, cfg.UserAgent)
		runners[in.Symbol] = r
		byProduct[product] = append(byProduct[product], r)
	}

	// The second engine version: one runner for every coin flagged "v2", sharing twelve balances.
	var coins2 []runner.Coin2
	coinOf := map[string]string{} // series -> coin, for the ones v2 trades
	for _, in := range instruments {
		if on, _ := in.Spec["v2"].(bool); !on || in.Source != "kalshi" {
			continue
		}
		num := func(key string, fallback float64) float64 {
			if v, ok := in.Spec[key].(float64); ok {
				return v
			}
			return fallback
		}
		priceFrom, _ := in.Spec["price_from"].(string)
		coins2 = append(coins2, runner.Coin2{Coin: in.Underlying, Series: in.Symbol, Product: strings.TrimPrefix(priceFrom, "coinbase:"),
			Cal: kalshi15m2.Calibration{OffsetPct: num("index_offset_pct", 0.000057), SDPct: num("index_sd_pct", 0.000144),
				DefaultSigma: num("default_sigma", 8e-5), Decimals: int(num("decimals", 2))}})
		coinOf[in.Symbol] = in.Underlying
	}
	var run2 *runner.Runner2
	if len(coins2) > 0 {
		if run2, err = runner.NewRunner2(ctx, db, coins2); err != nil {
			return err
		}
		run2.Seed(ctx, client, cfg.UserAgent)
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
				for _, r := range byProduct[t.Product] {
					r.Observe(t)
				}
				if run2 != nil {
					run2.Observe(t)
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

	// Kalshi: one poller per series.
	pollers := map[string]*kalshi.Poller{}
	for _, in := range instruments {
		if in.Source != "kalshi" || in.Kind != "binary_contract" {
			continue
		}
		round := 900 * time.Second
		if s, ok := in.Spec["round_seconds"].(float64); ok && s > 0 {
			round = time.Duration(s) * time.Second
		}
		priceFrom, _ := in.Spec["price_from"].(string) // "coinbase:BTC-USD"
		p := &kalshi.Poller{
			Client: client, Series: in.Symbol, Round: round,
			Sink: &sink{db: db, instrumentID: in.ID, latest: latest, product: strings.TrimPrefix(priceFrom, "coinbase:"), run: runners[in.Symbol], run2: run2, coin: coinOf[in.Symbol]},
		}
		pollers[in.Symbol] = p
		wg.Add(1)
		go func() { defer wg.Done(); p.Run(ctx) }()
	}

	// Keep next month's partitions ahead of need.
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

	slog.Info("asset cracker service running", "version", version, "instruments", len(instruments), "health", "http://"+cfg.HTTPAddr+"/healthz")
	// What the running service knows right now. The health check and the status page share it.
	live := func(hctx context.Context) (map[string]any, bool) {
		ok := db.Ping(hctx) == nil
		// The feed is one connection for every product, so it is healthy if ANY product traded in
		// the last minute. A quiet coin is not a fault: DOGE went 50 seconds with three trades.
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
			"ok": ok, "version": version, "uptime_seconds": time.Since(started).Seconds(), "mode": "simulation on live data, no real orders: engine v2 (five coins, shared balances, anti-world) beside v1",
			"ticks_written": ticksWritten.Load(), "prices": prices, "rounds": rounds,
		}, ok
	}
	err = health.Serve(ctx, cfg.HTTPAddr,
		func(hctx context.Context) (any, bool) { return live(hctx) },
		func(mux *http.ServeMux) {
			web.Routes(mux, db, cfg.UserAgent, func() map[string]any {
				doc, _ := live(context.Background())
				engines := map[string]any{}
				for series, r := range runners {
					engines[series] = r.Snapshot()
				}
				doc["engines"] = engines
				if run2 != nil {
					doc["v2"] = run2.Snapshot()
				}
				return doc
			})
		})
	stop()
	wg.Wait()
	return err
}

// writeTicks batches trade prints into the database: every second, or sooner when busy.
func writeTicks(ctx context.Context, db *store.Store, products map[string]int64, in <-chan coinbase.Trade, written *atomic.Int64) {
	var batch []store.Tick
	flush := func() {
		if len(batch) == 0 {
			return
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := db.InsertTicks(wctx, batch); err != nil {
			slog.Error("writing ticks", "n", len(batch), "err", err)
		} else {
			written.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case <-t.C:
			flush()
		case tr := <-in:
			batch = append(batch, store.Tick{InstrumentID: products[tr.Product], At: tr.At, ReceivedAt: tr.ReceivedAt, Price: tr.Price, Size: tr.Size})
			if len(batch) >= 500 {
				flush()
			}
		}
	}
}

// sink connects one Kalshi series' poller to the database and to the series' strategies.
type sink struct {
	db           *store.Store
	instrumentID int64
	latest       *coinbase.Latest
	product      string
	run          *runner.Runner  // version 1 on this series, or nil
	run2         *runner.Runner2 // version 2, shared by every coin it trades
	coin         string          // set when version 2 trades this series
}

// price is the latest trade price, or "" if it is stale: a stale price is worse than none.
func (s *sink) price() string {
	if t, ok := s.latest.Get(s.product); ok && time.Since(t.ReceivedAt) < 30*time.Second {
		return t.Price
	}
	return ""
}

func (s *sink) SaveMarket(ctx context.Context, m kalshi.MarketInfo, closes time.Time) (int64, error) {
	mk := store.Market{Ticker: m.Ticker, Strike: m.FloorStrike, ClosesAt: &closes}
	if opens := m.Opens(); !opens.IsZero() {
		mk.OpensAt = &opens
	}
	return s.db.UpsertMarket(ctx, s.instrumentID, mk)
}

func (s *sink) SaveQuotes(ctx context.Context, at time.Time, marketID int64, m kalshi.MarketInfo, closes time.Time, q kalshi.Quotes) error {
	// One snapshot row per second per market; every engine version hangs its decisions off it.
	price := s.price()
	evalID, err := s.db.InsertEvaluation(ctx, at, marketID, price, q)
	if err != nil {
		return err
	}
	var first error
	if s.run != nil {
		first = s.run.Step(ctx, evalID, at, marketID, m, closes, q, price)
	}
	if s.run2 != nil && s.coin != "" {
		if err := s.run2.Step(ctx, s.coin, evalID, at, marketID, m, closes, q, price); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *sink) SaveResult(ctx context.Context, marketID int64, m kalshi.MarketInfo, closes time.Time) (bool, error) {
	settled, err := time.Parse(time.RFC3339Nano, m.SettlementTS)
	if err != nil {
		settled = time.Now()
	}
	first, err := s.db.RecordResult(ctx, marketID, m.Result, m.ExpirationValue, settled)
	if err != nil || !first {
		return first, err
	}
	if s.run != nil {
		err = s.run.Settled(ctx, marketID, m, closes, s.price())
	}
	if s.run2 != nil && s.coin != "" {
		if err2 := s.run2.Settled(ctx, s.coin, marketID, m, closes, s.price()); err == nil {
			err = err2
		}
	}
	return true, err
}

func (s *sink) Unsettled(ctx context.Context, before time.Time) (map[string]kalshi.Pending, error) {
	found, err := s.db.UnsettledMarkets(ctx, s.instrumentID, before)
	out := map[string]kalshi.Pending{}
	for ticker, m := range found {
		out[ticker] = kalshi.Pending{ID: m.ID, Closes: m.Closes}
	}
	return out, err
}
