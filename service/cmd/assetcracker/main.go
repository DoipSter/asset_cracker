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
	client := kalshi.NewClient(cfg.UserAgent)
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
			Sink: &sink{db: db, instrumentID: in.ID, latest: latest, product: strings.TrimPrefix(priceFrom, "coinbase:")},
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
		prices := map[string]any{}
		for p := range products {
			if t, seen := latest.Get(p); seen {
				age := time.Since(t.ReceivedAt)
				prices[p] = map[string]any{"price": t.Price, "age_seconds": age.Seconds()}
				ok = ok && age < time.Minute
			} else {
				ok = false
			}
		}
		rounds := map[string]kalshi.Status{}
		for series, p := range pollers {
			st := p.Status()
			rounds[series] = st
			ok = ok && time.Since(st.LastQuotesAt) < time.Minute
		}
		return map[string]any{
			"ok": ok, "version": version, "uptime_seconds": time.Since(started).Seconds(), "mode": "market data only",
			"ticks_written": ticksWritten.Load(), "prices": prices, "rounds": rounds,
		}, ok
	}
	err = health.Serve(ctx, cfg.HTTPAddr,
		func(hctx context.Context) (any, bool) { return live(hctx) },
		func(mux *http.ServeMux) {
			web.Routes(mux, db, func() map[string]any { doc, _ := live(context.Background()); return doc })
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

// sink connects one Kalshi series' poller to the database.
type sink struct {
	db           *store.Store
	instrumentID int64
	latest       *coinbase.Latest
	product      string
}

func (s *sink) SaveMarket(ctx context.Context, m kalshi.MarketInfo, closes time.Time) (int64, error) {
	mk := store.Market{Ticker: m.Ticker, Strike: m.FloorStrike, ClosesAt: &closes}
	if opens := m.Opens(); !opens.IsZero() {
		mk.OpensAt = &opens
	}
	return s.db.UpsertMarket(ctx, s.instrumentID, mk)
}

func (s *sink) SaveQuotes(ctx context.Context, at time.Time, marketID int64, q kalshi.Quotes) error {
	price := ""
	if t, ok := s.latest.Get(s.product); ok && time.Since(t.ReceivedAt) < 30*time.Second {
		price = t.Price // a stale price is worse than none: leave it null
	}
	return s.db.InsertEvaluation(ctx, at, marketID, price, q)
}

func (s *sink) SaveResult(ctx context.Context, marketID int64, m kalshi.MarketInfo) (bool, error) {
	settled, err := time.Parse(time.RFC3339Nano, m.SettlementTS)
	if err != nil {
		settled = time.Now()
	}
	return s.db.RecordResult(ctx, marketID, m.Result, m.ExpirationValue, settled)
}

func (s *sink) Unsettled(ctx context.Context, before time.Time) (map[string]int64, error) {
	return s.db.UnsettledMarkets(ctx, s.instrumentID, before)
}
