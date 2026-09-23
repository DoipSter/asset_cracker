package app

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/doipster/asset_cracker/service/internal/catalogue"
	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/config"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/doipster/asset_cracker/service/internal/web"
)

// startAssets is the searchable catalogue and the assets page (GET /assets, migration 0018). It
// mounts the page and its API on mux, reads the catalogue now and once a day, and starts the
// supervisor that records what is switched on there. Run calls it once, where it mounts routes.
//
// RECORD ONLY. A selected instrument is left out of Run's own list (splitLadders), so no runner,
// engine, trade stream, health check, home-page asset or analysis sees it; the supervisor alone
// records it. Its Kalshi requests wait on pace, the ladder recorders' pacer, so the page adds no
// more Kalshi load than that pacer allows. A Coinbase product gets candles only: the trade
// stream's product list is fixed at start and never changes here.
func startAssets(ctx context.Context, wg *sync.WaitGroup, mux *http.ServeMux, db *store.Store, cfg config.Config, pace *kalshi.Pacer, latest *coinbase.Latest) {
	max := cfg.MaxSelected
	if max <= 0 {
		max = catalogue.DefaultMaxSelected
	}
	categories := cfg.CatalogueCategories
	if len(categories) == 0 {
		categories = catalogue.DefaultCategories
	}
	paced := kalshi.NewPacedClient(cfg.UserAgent, pace)
	sup := &supervisor{db: db, max: max, userAgent: cfg.UserAgent, paced: paced, plain: kalshi.NewClient(cfg.UserAgent),
		pace: pace, latest: latest, wg: wg, running: map[int64]*recording{}, nudge: make(chan struct{}, 1)}
	wg.Add(2)
	go func() { defer wg.Done(); refreshCatalogue(ctx, db, paced, cfg.UserAgent, categories) }()
	go func() { defer wg.Done(); sup.run(ctx) }()
	web.CatalogueRoutes(mux, db, web.Catalogue{Key: cfg.OperatorKey, Max: max, Running: sup.status, Changed: sup.poke})
	slog.Info("assets page", "path", "/assets", "kalshi_categories", strings.Join(categories, ","), "max_selected", max)
}

// catalogueFetchers are the venue calls of one refresh. Every Kalshi one goes through client.
func catalogueFetchers(client *kalshi.Client, userAgent string) catalogue.Fetchers {
	return catalogue.Fetchers{
		SeriesList: client.SeriesList,
		OpenSample: func(ctx context.Context, series string) ([]catalogue.Market, error) {
			ms, err := client.OpenSample(ctx, series, catalogue.SampleSize)
			out := make([]catalogue.Market, len(ms))
			for i, m := range ms {
				out[i] = catalogue.Market{StrikeType: m.StrikeType, HasFloor: m.FloorStrike != nil, Closes: m.CloseTime}
			}
			return out, err
		},
		Products: func(ctx context.Context) ([]byte, error) { return coinbase.Products(ctx, userAgent) },
	}
}

// refreshCatalogue reads the catalogue at start and then once a day [CONVENTION].
func refreshCatalogue(ctx context.Context, db *store.Store, client *kalshi.Client, userAgent string, categories []string) {
	f := catalogueFetchers(client, userAgent)
	for {
		refreshOnce(ctx, db, f, categories)
		t := time.NewTimer(catalogue.RefreshEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func refreshOnce(ctx context.Context, db *store.Store, f catalogue.Fetchers, categories []string) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("catalogue: a panic was stopped; the next refresh tries again", "panic", p)
		}
	}()
	began := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	items, problems := catalogue.Build(ctx, f, categories)
	for _, p := range problems {
		slog.Warn("catalogue", "problem", p)
	}
	if len(items) == 0 {
		slog.Error("catalogue: nothing was read; the stored catalogue is unchanged")
		return
	}
	rows := make([]store.CatalogueRow, len(items))
	for i, it := range items {
		rows[i] = it.Row()
	}
	if err := db.UpsertCatalogue(ctx, rows, began); err != nil {
		slog.Error("catalogue: not stored", "err", err)
		return
	}
	recordable := 0
	for _, it := range items {
		if it.Recorder != "" {
			recordable++
		}
	}
	slog.Info("catalogue refreshed", "items", len(items), "recordable", recordable, "problems", len(problems), "took", time.Since(began).Round(time.Second))
}

// isSelected is an instrument switched on from the assets page (migration 0018).
func isSelected(in store.Instrument) bool {
	flag, _ := in.Spec["selected"].(bool)
	return flag
}

// recordOnlySink is the 15-minute poller's sink for a selected series: the database and the
// instrument, and no runner, so nothing it records reaches an engine.
func recordOnlySink(db *store.Store, instrumentID int64, latest *coinbase.Latest, product string) *sink {
	return &sink{db: db, instrumentID: instrumentID, latest: latest, product: product}
}

// recording is one selected instrument the supervisor records.
type recording struct {
	instrument store.Instrument
	recorder   string
	since      time.Time
	cancel     context.CancelFunc // nil for a product: the candle group runs them together
	poller     *kalshi.Poller     // a 15-minute series', for its status
}

// supervisor starts and stops the recorders of the selected instruments. It re-reads them every
// minute, and at once when the page switches one. It manages ONLY those: the migration-seeded
// instruments run as Run starts them. Stopping cancels the recorder; everything it wrote stays.
type supervisor struct {
	db           *store.Store
	max          int
	userAgent    string
	paced, plain *kalshi.Client
	pace         *kalshi.Pacer
	latest       *coinbase.Latest
	wg           *sync.WaitGroup
	nudge        chan struct{}

	mu           sync.Mutex
	running      map[int64]*recording
	candleIDs    []int64
	candleCancel context.CancelFunc
}

func (s *supervisor) poke() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

func (s *supervisor) run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.nudge:
		}
	}
}

func (s *supervisor) tick(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("asset supervisor: a panic was stopped; the next minute tries again", "panic", p)
		}
	}()
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	all, err := s.db.SelectedInstruments(rctx)
	cancel()
	if err != nil {
		slog.Error("asset supervisor: selection not read; what runs keeps running", "err", err)
		return
	}
	byID := map[int64]store.Instrument{}
	wanted := make([]catalogue.Selected, 0, len(all))
	for _, in := range all {
		byID[in.ID] = in
		wanted = append(wanted, catalogue.Selected{ID: in.ID, Source: in.Source, Symbol: in.Symbol, Kind: in.Kind})
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := map[int64]string{}
	for id, r := range s.running {
		now[id] = r.recorder
	}
	start, stop, left := catalogue.Plan(now, wanted, s.max)
	for _, id := range stop { // before start: a recorder whose kind changed is in both
		r := s.running[id]
		if r.cancel != nil {
			r.cancel()
		}
		delete(s.running, id)
		slog.Info("asset supervisor: stopped recording", "symbol", r.instrument.Symbol, "recorder", r.recorder)
	}
	for i, w := range start {
		s.startOne(ctx, byID[w.ID], i)
	}
	for _, w := range left {
		slog.Warn("asset supervisor: selected but not recorded", "symbol", w.Symbol, "kind", w.Kind, "max_selected", s.max)
	}
	s.syncCandles(ctx)
}

// startOne starts one instrument's recorder. The caller holds s.mu.
func (s *supervisor) startOne(ctx context.Context, in store.Instrument, i int) {
	rec := &recording{instrument: in, recorder: catalogue.RecorderFor(in.Kind), since: time.Now()}
	s.running[in.ID] = rec
	slog.Info("asset supervisor: recording", "symbol", in.Symbol, "recorder", rec.recorder)
	if rec.recorder == catalogue.RecorderCandles {
		return // syncCandles runs every product in one group
	}
	priceFrom, _ := in.Spec["price_from"].(string)
	product := strings.TrimPrefix(priceFrom, "coinbase:")
	var run func(context.Context)
	switch rec.recorder {
	case catalogue.RecorderRound:
		round := catalogue.RoundSeconds * time.Second
		if v, ok := in.Spec["round_seconds"].(float64); ok && v > 0 {
			round = time.Duration(v) * time.Second
		}
		p := &kalshi.Poller{Client: s.paced, Series: in.Symbol, Round: round, Sink: recordOnlySink(s.db, in.ID, s.latest, product)}
		rec.poller, run = p, p.Run
	case catalogue.RecorderLadder:
		r := &kalshi.LadderRecorder{Client: s.plain, Series: in.Symbol, Pace: s.pace,
			Sink: &ladderSink{db: s.db, instrumentID: in.ID}, Price: freshPrice(s.latest, product),
			Start: 5*time.Second + time.Duration(i)*12*time.Second}
		run = r.Run
	default:
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	rec.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				slog.Error("asset supervisor: a recorder was stopped by a panic; it starts again within a minute", "symbol", in.Symbol, "panic", p)
				s.mu.Lock()
				if s.running[in.ID] == rec {
					delete(s.running, in.ID)
				}
				s.mu.Unlock()
			}
		}()
		run(rctx)
	}()
}

// syncCandles runs the selected products' candles as one group, one request a second like the
// seeded products' writer, restarted when the set changes (a restart resumes from the newest stored
// candle). The caller holds s.mu.
func (s *supervisor) syncCandles(ctx context.Context) {
	products := map[string]int64{}
	var ids []int64
	for id, r := range s.running {
		if r.recorder == catalogue.RecorderCandles {
			products[r.instrument.Symbol] = id
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if slices.Equal(ids, s.candleIDs) {
		return
	}
	if s.candleCancel != nil {
		s.candleCancel()
		s.candleCancel = nil
	}
	s.candleIDs = ids
	if len(ids) == 0 {
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	s.candleCancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				slog.Error("asset supervisor: the candle group was stopped by a panic; it starts again within a minute", "panic", p)
				s.mu.Lock()
				if slices.Equal(s.candleIDs, ids) {
					s.candleIDs = nil
				}
				s.mu.Unlock()
			}
		}()
		recordCandles(cctx, s.db, s.userAgent, products)
	}()
}

// status is what the page shows of each selected instrument being recorded.
func (s *supervisor) status() map[int64]web.RecorderState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]web.RecorderState, len(s.running))
	for id, r := range s.running {
		st := web.RecorderState{Recorder: r.recorder, Since: r.since}
		if r.poller != nil {
			ps := r.poller.Status()
			st.LastError = ps.LastError
			if !ps.LastQuotesAt.IsZero() {
				at := ps.LastQuotesAt
				st.LastQuotesAt = &at
			}
		}
		out[id] = st
	}
	return out
}
