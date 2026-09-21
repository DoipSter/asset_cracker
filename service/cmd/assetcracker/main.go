// Command assetcracker is the Asset Cracker service.
//
// Today it records market data: Coinbase trade prints and, once a second, Kalshi's live quotes
// for each open 15-minute round, plus how every round settles. It runs no strategies and has
// no order code. See docs/platform-brief.md for where it is going.
package main

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
	"github.com/doipster/asset_cracker/service/internal/health"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/kalshi15m2"
	"github.com/doipster/asset_cracker/service/internal/kalshi15m3"
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
	all, err := db.ActiveInstruments(ctx)
	if err != nil {
		return fmt.Errorf("instruments: %w", err)
	}
	// The ladders are recorded by their own recorder (below) and are in nothing else.
	instruments, ladders := splitLadders(all)

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
	// The buckets the first engine's runners hold. Any other live bucket that the second engine
	// does not hold either has its cash in the ledger and nowhere else, and is valued from there.
	heldByV1 := []int64{}
	for _, r := range runners {
		heldByV1 = append(heldByV1, r.BucketIDs()...)
	}
	// The third engine, BEFORE the second: the buckets it holds must be in the list the second
	// engine's first capital read is given, or a freshly seeded v3 bucket would read as $1,000
	// earned. The list never changes while the service runs. It is built also with AC_V3 off,
	// because a bet v3 already holds must still be valued and settled; with AC_V3 off and no v3
	// bucket there is no third engine (run3 stays nil) and the service behaves as it did before.
	// Not being able to FIND OUT what v3 holds is a failed start (docs/honest-fills-v3.md, 5.4).
	var coins3 []runner.Coin3
	coin3Of := map[string]string{} // series -> coin, for the ones v3 looks at
	for _, in := range instruments {
		if on, _ := in.Spec["v3"].(bool); !on || in.Source != "kalshi" {
			continue
		}
		num := func(key string, fallback float64) float64 {
			if v, ok := in.Spec[key].(float64); ok {
				return v
			}
			return fallback
		}
		priceFrom, _ := in.Spec["price_from"].(string)
		coins3 = append(coins3, runner.Coin3{Coin: in.Underlying, Series: in.Symbol, Product: strings.TrimPrefix(priceFrom, "coinbase:"),
			Cal: kalshi15m3.Calibration{OffsetPct: num("index_offset_pct", 0.000057), SDPct: num("index_sd_pct", 0.000144),
				DefaultSigma: num("default_sigma", 8e-5), Decimals: int(num("decimals", 2))}})
		coin3Of[in.Symbol] = in.Underlying
	}
	faults := runner.Faults{Every: cfg.V3Faults.Every, Run: cfg.V3Faults.Run, Kind: cfg.V3Faults.Kind, DelayCommit: cfg.V3Faults.DelayCommit, Ops: cfg.V3Faults.Ops}
	switch {
	case faults.On() && !runner.FaultInjectionBuilt:
		slog.Warn("AC_V3_FAIL_* is set and IGNORED: this binary was not built with the faultinject tag")
	case faults.On() && !strings.HasSuffix(cfg.DatabaseName(), "_dev"):
		return fmt.Errorf("fault injection is switched on against database %q, which is not a *_dev one: refusing to start", cfg.DatabaseName())
	}
	run3, err := runner.NewRunner3(ctx, runner.WrapStore3(db, faults), coins3, runner.Options3{On: cfg.V3, DatabaseName: cfg.DatabaseName()})
	if err != nil {
		return err
	}
	heldElsewhere := heldByV1
	if run3 != nil {
		heldElsewhere = append(append([]int64{}, heldByV1...), run3.BucketIDs()...)
		run3.Seed(ctx, client, cfg.UserAgent)
		wg.Add(1)
		go func() { defer wg.Done(); run3.Run(ctx) }() // heal, write-probe, sweep, cash check: v3's own goroutine
	}
	var run2 *runner.Runner2
	if len(coins2) > 0 {
		if run2, err = runner.NewRunner2(ctx, db, coins2, heldElsewhere); err != nil {
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
				if run3 != nil {
					// After v1's and v2's. It takes the model's own small mutex only, never the lock v3
					// holds across a database write, so a slow database cannot back this stream up.
					safely("v3 observe", func() { run3.Observe(t) })
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
			Sink: &sink{db: db, instrumentID: in.ID, latest: latest, product: strings.TrimPrefix(priceFrom, "coinbase:"), run: runners[in.Symbol], run2: run2, coin: coinOf[in.Symbol],
				run3: run3, coin3: coin3Of[in.Symbol]},
		}
		pollers[in.Symbol] = p
		wg.Add(1)
		go func() { defer wg.Done(); p.Run(ctx) }()
	}

	// Kalshi's longer ladders: RECORD ONLY, one recorder per series on its own goroutine, started
	// 12 s apart and sharing one pacer (at most five requests a second among them). Not in the
	// health check, the status document or the home page; nothing trades them.
	pace := kalshi.NewPacer(200 * time.Millisecond)
	for i, in := range ladders {
		priceFrom, _ := in.Spec["price_from"].(string) // "coinbase:BTC-USD"
		rec := &kalshi.LadderRecorder{Client: client, Series: in.Symbol, Pace: pace,
			Sink:  &ladderSink{db: db, instrumentID: in.ID},
			Price: freshPrice(latest, strings.TrimPrefix(priceFrom, "coinbase:")),
			Start: 5*time.Second + time.Duration(i)*12*time.Second}
		wg.Add(1)
		go func() { defer wg.Done(); rec.Run(ctx) }()
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

	slog.Info("asset cracker service running", "version", version, "instruments", len(instruments), "ladders_recorded", len(ladders), "health", "http://"+cfg.HTTPAddr+"/healthz")
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
	// What the home page and the value snapshots read: the engines' books, typed, from memory.
	src := web.Sources{Release: version}
	// The ledger's side comes with them. Only the second engine ever changes it while the service
	// runs (it seeds, reaps and takes allocations; the first engine does none of these), so its
	// book and the capital are read under one hold of its lock and can never be from either side
	// of such an event.
	capitalWithoutV2 := ledgerCapital(db, heldElsewhere)
	src.Books = func() ([]runner.Book, store.Capital, bool) {
		books := []runner.Book{}
		for _, in := range instruments { // in instrument order, so the list does not shuffle between calls
			if r := runners[in.Symbol]; r != nil {
				books = append(books, r.Book())
			}
		}
		// The third engine's book is read apart from the capital, and may be: v3 seeds, reaps and
		// allocates nothing while the service runs, so the capital cannot move because of it.
		var capital store.Capital
		var ok bool
		if run2 == nil {
			capital, ok = capitalWithoutV2()
		} else {
			var book runner.Book
			book, capital, ok = run2.BookAndCapital()
			books = append(books, book)
		}
		if run3 != nil {
			// This runs on the snapshot writer's goroutine, which has no recover: the second fence.
			// Book stops its own panics; if one got past it anyway, the book says HALTED, so the
			// minute is refused rather than written without v3's buckets in it.
			book := runner.Book{Engine: "v3", Halted: "a panic escaped v3's Book"}
			safely("v3 book", func() { book = run3.Book() })
			books = append(books, book)
		}
		return books, capital, ok
	}
	src.Markers = func(coin string, since float64) []runner.Marker {
		var out []runner.Marker
		for _, r := range runners {
			out = append(out, r.Markers(coin, since)...)
		}
		if run2 != nil {
			out = append(out, run2.Markers(coin, since)...)
		}
		if run3 != nil {
			safely("v3 markers", func() { out = append(out, run3.Markers(coin, since)...) })
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
			seconds = 900 // the same default the poller is given above
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

	// The health check pings the database, and the home page is polled every few seconds by
	// every phone looking at it, so its answer is kept for five seconds: /api/home is meant to be
	// served from memory, and must not queue for one of the pool's few connections on every poll.
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

	// Once a minute, write down what everything is worth: the history behind "earned over 24H".
	// It waits a minute before the first one, so that every round has quotes to mark bets by. A
	// snapshot that cannot be written is logged and skipped. It takes the engines' locks only to
	// copy what is in memory, never across a database call: trading does not wait on it.
	// The home page says so when the snapshots stop: nothing else would, short of reading the log.
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
				err := snapshotValues(ctx, db, src, run2, run3, now)
				switch {
				case errors.Is(err, runner.ErrAwaitingSettlement): // routine: a minute that fell between a close and its settlement
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
				engines := map[string]any{}
				for series, r := range runners {
					engines[series] = r.Snapshot()
				}
				doc["engines"] = engines
				if run2 != nil {
					doc["v2"] = run2.Snapshot()
				}
				// The third engine: state, what may order per bucket, skipped_busy, the last rebuild,
				// the paper holds, and what is waiting for a restart.
				if run3 != nil {
					sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					doc["v3"] = run3.Snapshot(sctx)
					cancel()
				} else {
					doc["v3"] = map[string]any{"engine": "v3", "state": "absent", "on": cfg.V3, "reason": "AC_V3 is off and no v3 bucket exists"}
				}
				return doc
			}, src)
			web.AnalysisRoutes(ctx, mux, db)
		})
	stop()
	wg.Wait()
	return err
}

// ledgerCapital is where the snapshots and the home page get the ledger's side of the balance
// sheet when the second engine is not running. (When it is, it keeps the capital cached, re-reads
// it whenever it changes, and hands it over together with its book.) Without that engine nothing
// is seeded, reaped or allocated while the service runs, so it is read from the database at most
// once a minute. held is the ids of the buckets the first engine's runners hold.
func ledgerCapital(db *store.Store, held []int64) func() (store.Capital, bool) {
	var (
		mu   sync.Mutex
		last store.Capital
		at   time.Time
		good bool
	)
	return func() (store.Capital, bool) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) > time.Minute {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, err := db.ReadCapital(ctx, held)
			if at, good = time.Now(), err == nil; good {
				last = c
			} else {
				slog.Warn("could not read the capital behind the value snapshots", "err", err)
			}
		}
		return last, good
	}
}

// snapshotValues appends one minute's value snapshots: the total and the four groups every
// minute, every bucket and coin on each fifth minute of the hour. It refuses to write a row
// that might be wrong, because a wrong row is there for good: the table is append-only, and a
// gap in the chart is honest where a false step is not. So nothing is written while the
// contributed figure might be stale, while an engine is halted, or while an open bet's round
// has closed and not yet settled (runner.SnapshotRefusal says why for the last two).
func snapshotValues(ctx context.Context, db *store.Store, src web.Sources, run2 *runner.Runner2, run3 *runner.Runner3, now time.Time) error {
	if run3 != nil {
		// A suspended v3 blocks every engine's snapshot, so it is given the chance to heal first:
		// rebuild, and only if that succeeded, settle what closed meanwhile. The same 3 s the
		// capital read below gets. Its reads are made outside v3's lock.
		hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		safely("v3 heal", func() { run3.Heal(hctx) })
		cancel()
	}
	if run2 != nil {
		if _, fresh := run2.Capital(); !fresh {
			// Its own short budget: the engine's lock is not held across this read, but a
			// struggling database is not to be leaned on for ten seconds for a display figure.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			run2.RefreshCapital(rctx)
			cancel()
		}
	}
	v, books, _, fresh := src.Valuation()
	// The clock is read AFTER the books, so a round that closed while they were being read is
	// caught: refusing a minute needlessly costs a point on a chart, the other way a false row.
	if err := runner.SnapshotRefusal(books, time.Now()); err != nil {
		return err
	}
	if !fresh {
		return fmt.Errorf("the ledger's side of the balance sheet could not be read")
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	at := now.Truncate(time.Second) // one timestamp for the whole batch, exact in Postgres's microseconds
	return db.InsertSnapshots(wctx, v.Snapshots(at, at.Minute()%5 == 0))
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
	run3         *runner.Runner3 // version 3, or nil; nothing of it is ever returned to the poller
	coin3        string          // set when version 3 looks at this series
}

// safely runs one call into the third engine on someone else's goroutine. The runner's entry
// points recover their own panics and suspend v3; this is the second fence, so that whatever
// goes wrong in v3 the poller, the price stream and the snapshot writer never see it. Nothing in
// the first two engines' paths recovers a panic, so one that escaped would take the service down.
func safely(what string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("a panic escaped the third engine and was stopped here", "in", what, "panic", p)
		}
	}()
	fn()
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
	model := map[string]any{}
	if t, ok := s.latest.Get(s.product); ok {
		model["price_age_s"] = at.Sub(t.At).Seconds() // how old the print the model is pricing off is, by the exchange's clock
	}
	var v2in map[string]any
	if s.run2 != nil && s.coin != "" {
		v2in = s.run2.Inputs(s.coin)
		model["v2"] = v2in
	}
	if s.run3 != nil && s.coin3 != "" {
		// Before the insert, and so before v1 and v2 step: it takes the model's small mutex only. It
		// journals v3's model inputs every second, also when v3 may place no order.
		safely("v3 inputs", func() {
			if in := s.run3.Inputs(s.coin3, m, closes, at, price, v2in); in != nil {
				model["v3"] = in
			}
		})
	}
	evalID, err := s.db.InsertEvaluation(ctx, at, marketID, price, q, model)
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
	if s.run3 != nil && s.coin3 != "" {
		// LAST, whatever v1 and v2 returned, and nothing comes back: it gives up at once if v3's lock
		// is busy (counted as skipped_busy in /api/status).
		safely("v3 step", func() { s.run3.Step(ctx, s.coin3, evalID, at, marketID, m, closes, q, price) })
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
	if s.run3 != nil {
		// LAST. For every series, not only v3's coins: a settle-only bucket may hold a bet on a coin
		// whose flag was since removed. If v3 is busy or suspended this does nothing, and the sweep
		// settles the position from market.result, which RecordResult stored above.
		safely("v3 settled", func() { s.run3.Settled(ctx, s.coin3, marketID, m, closes) })
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
