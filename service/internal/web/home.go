package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/runner"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// The home page's API: docs/api-home.md is the contract. All of it reads.
//
// /api/home is polled every few seconds by phones, so what changes second by second (values,
// prices, quotes, open bets) comes from memory, and what needs the database (the snapshot a
// range is compared with, the value chart, what each coin realised) is looked up at most once a
// minute per range and kept.

// Asset is one of the five coins the home page always lists, in this order. Signs, colours and
// precision are doipster's (ASSETS in asset_cracker.py), the same table the widget carries.
type Asset struct {
	Coin, Name, Sign, Colour string
	Decimals                 int
}

var assets = []Asset{
	{"BTC", "Bitcoin", "₿", "#F7931A", 2},
	{"ETH", "Ethereum", "Ξ", "#8FA2F2", 2},
	{"SOL", "Solana", "≡", "#14F195", 4},
	{"XRP", "XRP", "✕", "#4FC3F7", 4},
	{"DOGE", "Dogecoin", "Ð", "#E3C044", 6},
}

// Feed says where one coin's price and rounds come from. RoundSeconds is the series' configured
// round length, used to say when the open round opened; it is a setting, not Kalshi's open_time.
type Feed struct {
	Coin, Product, Series string
	RoundSeconds          float64
}

// Sources is the running service as the home page sees it: typed, and all from memory.
type Sources struct {
	Release string
	Healthy func() bool
	Books   func() []runner.Book
	Markers func(coin string, since float64) []runner.Marker
	Capital func() (store.Capital, bool) // ok is false when the ledger's side could not be read
	Feeds   []Feed
	Price   func(product string) (price, ageSeconds float64, ok bool)
	Round   func(series string) (kalshi.Status, bool)
}

func (s Sources) feed(coin string) (Feed, bool) {
	for _, f := range s.Feeds {
		if f.Coin == coin {
			return f, true
		}
	}
	return Feed{}, false
}

// Valuation marks the books to market right now.
func (s Sources) Valuation() (runner.Valuation, []runner.Book, bool) {
	books := s.Books()
	capital, ok := s.Capital()
	coins := make([]string, len(assets))
	for i, a := range assets {
		coins[i] = a.Coin
	}
	return runner.Value(books, capital, coins), books, ok
}

// homeRanges are the spans the balance sheet can be asked for. ALL has no length: it runs from
// the first snapshot ever written.
var homeRanges = map[string]time.Duration{"1H": time.Hour, "24H": 24 * time.Hour, "7D": 7 * 24 * time.Hour, "ALL": 0}

// parseRange reads a range parameter: empty means the fallback, case does not matter, and
// anything not in `allowed` is refused.
func parseRange(raw, fallback string, allowed map[string]time.Duration) (string, time.Duration, bool) {
	key := strings.ToUpper(strings.TrimSpace(raw))
	if key == "" {
		key = fallback
	}
	d, ok := allowed[key]
	return key, d, ok
}

const maxPoints = 300

// window is what the database knows about one range, looked up at most once a minute.
type window struct {
	fetched  time.Time
	then     store.ValueSnapshot            // the total's snapshot the range is compared with
	have     bool                           // false before any snapshot exists
	complete bool                           // false when history is shorter than the range
	groups   map[string]store.ValueSnapshot // the groups, from the same batch as `then`
	series   [][2]int64
	realised map[string]int64 // per coin, on rounds settled since `then`; nil if it could not be read
	err      string
}

type homeReader interface {
	SnapshotAt(ctx context.Context, scope, key string, t time.Time) (store.ValueSnapshot, bool, error)
	FirstSnapshot(ctx context.Context, scope, key string) (store.ValueSnapshot, bool, error)
	SnapshotsTakenAt(ctx context.Context, scope string, at time.Time) (map[string]store.ValueSnapshot, error)
	SnapshotSeries(ctx context.Context, scope, key string, since time.Time, maxPoints int) ([][2]int64, error)
	RealisedByCoin(ctx context.Context, since time.Time) (map[string]int64, error)
}

// loadWindow finds the snapshot a range starts from and everything that hangs off it. When the
// history is shorter than the range it falls back to the earliest snapshot and says so.
func loadWindow(ctx context.Context, db homeReader, length time.Duration, now time.Time) (window, error) {
	w := window{fetched: now, series: [][2]int64{}}
	var err error
	if length > 0 {
		if w.then, w.have, err = db.SnapshotAt(ctx, "total", "all", now.Add(-length)); err != nil {
			return w, err
		}
		w.complete = w.have
	}
	if !w.have {
		if w.then, w.have, err = db.FirstSnapshot(ctx, "total", "all"); err != nil {
			return w, err
		}
		w.complete = w.have && length == 0 // ALL is, by definition, everything there is
	}
	if !w.have {
		return w, nil
	}
	if w.groups, err = db.SnapshotsTakenAt(ctx, "group", w.then.At); err != nil {
		return w, err
	}
	// One point is left for the value right now, which the handler adds.
	if w.series, err = db.SnapshotSeries(ctx, "total", "all", w.then.At, maxPoints-1); err != nil {
		return w, err
	}
	if w.realised, err = db.RealisedByCoin(ctx, w.then.At); err != nil {
		w.realised = nil // the balance sheet still stands; each asset's earned is reported as unknown
		slog.Warn("home: could not read what each coin realised", "err", err)
	}
	return w, nil
}

// windows caches one window per range. A failed lookup keeps the last good one and tries again
// in ten seconds; the error is reported in the document, not hidden.
type windows struct {
	mu sync.Mutex
	by map[string]window
}

func (c *windows) get(ctx context.Context, db homeReader, key string, length time.Duration) window {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if w, ok := c.by[key]; ok && now.Sub(w.fetched) < time.Minute {
		return w
	}
	w, err := loadWindow(ctx, db, length, now)
	if err != nil {
		slog.Warn("home: value history lookup failed", "range", key, "err", err)
		w = c.by[key] // the last good one, or nothing
		if w.series == nil {
			w.series = [][2]int64{}
		}
		w.err, w.fetched = "value history could not be read; earned figures may be out of date", now.Add(-50*time.Second)
	}
	c.by[key] = w
	return w
}

// earned is the pure arithmetic of a range: what was earned since `then`, and as a percentage
// of what everything was worth then. Before any snapshot exists nothing can be said to have
// been earned: zero, since now, window incomplete.
func earned(now runner.Line, then store.ValueSnapshot, have bool) (cents int64, pct float64) {
	if !have {
		return 0, 0
	}
	cents = store.EarnedBetween(then, store.ValueSnapshot{ValueCents: now.ValueCents, ContributedCents: now.ContributedCents})
	if then.ValueCents > 0 {
		pct = float64(cents) / float64(then.ValueCents) * 100
	}
	return cents, pct
}

var groupLabels = map[string]string{"strategies": "Strategies", "anti": "Anti-world twins", "v1": "First engine", "money": "Money buckets"}

func unixf(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func atof(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

func moneyDoc(m store.MoneyBuckets, deployed int64) map[string]any {
	return map[string]any{"deployed_cents": deployed, "winnings_cents": m.Winnings, "replenishment_cents": m.Replenishment,
		"tax_reserve_cents": m.TaxReserve, "fee_reserve_cents": m.FeeReserve, "venue_fees_paid_cents": m.FeesPaid}
}

// deployedCents is the cash in every live bucket right now, from the books.
func deployedCents(v runner.Valuation) int64 {
	var sum int64
	for _, g := range v.Groups {
		if g.Key != "money" {
			sum += g.CashCents
		}
	}
	return sum
}

// stake is what is open on one coin. value is nil when there are bets and none has a bid.
func stake(l runner.Line) (cost int64, value *int64, open, unmarked int) {
	if l.Count == 0 || l.Unmarked < l.Count {
		v := l.ValueCents
		value = &v
	}
	return l.AtRiskCents, value, l.Count, l.Unmarked
}

// homeDoc composes the balance sheet from a valuation taken now and a window from the database.
func homeDoc(src Sources, key string, w window, now time.Time, changes map[string]*float64) map[string]any {
	v, books, capitalOK := src.Valuation()
	capital, _ := src.Capital()

	halted := []string{}
	for _, b := range books {
		if b.Halted != "" {
			halted = append(halted, strings.TrimSpace(b.Engine+" "+b.Series)+": "+b.Halted)
		}
	}
	cents, pct := earned(v.Total, w.then, w.have)
	since := unixf(now)
	if w.have {
		since = unixf(w.then.At)
	}
	total := map[string]any{"value_cents": v.Total.ValueCents, "earned_cents": cents, "earned_pct": pct, "range": key, "since": since,
		"window_complete": w.complete, "at_risk_cents": v.Total.AtRiskCents,
		"unrealized_cents": v.Total.ValueCents - v.Total.CashCents - v.Total.AtRiskCents,
		"unmarked_bets":    v.Total.Unmarked, "contributed_cents": v.Total.ContributedCents,
		"lifetime_earned_cents": v.Total.ValueCents - v.Total.ContributedCents}

	composition := []map[string]any{}
	for _, g := range v.Groups {
		row := map[string]any{"key": g.Key, "label": groupLabels[g.Key], "buckets": g.Count, "value_cents": g.ValueCents, "earned_cents": nil}
		if !w.have {
			row["earned_cents"] = int64(0)
		} else if then, ok := w.groups[g.Key]; ok {
			row["earned_cents"], _ = earned(g, then, true)
		}
		composition = append(composition, row)
	}

	byCoin := map[string]runner.Line{}
	for _, c := range v.Coins {
		byCoin[c.Key] = c
	}
	list := []map[string]any{}
	for _, a := range assets {
		cost, value, open, unmarked := stake(byCoin[a.Coin])
		row := map[string]any{"coin": a.Coin, "name": a.Name, "sign": a.Sign, "colour": a.Colour, "decimals": a.Decimals,
			"price": nil, "price_age_s": nil, "change_pct": changes[a.Coin], "stake_cents": cost, "stake_value_cents": value,
			"open_bets": open, "unmarked_bets": unmarked, "earned_cents": nil, "round": nil}
		if w.realised != nil || !w.have {
			row["earned_cents"] = w.realised[a.Coin]
		}
		if f, ok := src.feed(a.Coin); ok {
			if p, age, ok := src.Price(f.Product); ok {
				row["price"], row["price_age_s"] = p, age
			}
			if st, ok := src.Round(f.Series); ok && st.Ticker != "" {
				row["round"] = map[string]any{"ticker": st.Ticker, "strike": st.Strike, "closes": unixf(st.Closes),
					"yes_bid": atof(st.Quotes.YesBid), "yes_ask": atof(st.Quotes.YesAsk)}
			}
		}
		list = append(list, row)
	}

	series := append([][2]int64{}, w.series...)
	series = append(series, [2]int64{now.Unix(), v.Total.ValueCents})
	doc := map[string]any{"simulated": true, "release": src.Release, "as_of": unixf(now), "healthy": src.Healthy() && capitalOK,
		"halted": halted, "total": total, "series": series, "money": moneyDoc(capital.Money, deployedCents(v)),
		"composition": composition, "assets": list}
	if w.err != "" {
		doc["history_error"] = w.err
	}
	if !capitalOK {
		doc["capital_error"] = "the ledger's side of the balance sheet could not be read; contributed and earned figures may be out of date"
	}
	return doc
}

// changes is each coin's price change over a range, from the cached Coinbase candles. It never
// waits for Coinbase: it answers from what it has (nil until a first fetch lands) and refreshes
// in the background, at most once a minute per coin and range.
type changes struct {
	mu       sync.Mutex
	first    map[string]float64 // product+range -> the first candle's close
	fetched  map[string]time.Time
	inflight map[string]bool
}

func (c *changes) get(userAgent string, src Sources, key string) map[string]*float64 {
	out := map[string]*float64{}
	if _, ok := ranges[key]; !ok { // ALL: there is no candle range that means "since the first snapshot"
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range src.Feeds {
		k := f.Product + key
		if first, ok := c.first[k]; ok && first > 0 {
			if p, _, ok := src.Price(f.Product); ok {
				pct := (p/first - 1) * 100
				out[f.Coin] = &pct
			}
		}
		if time.Since(c.fetched[k]) < time.Minute || c.inflight[k] {
			continue
		}
		c.inflight[k] = true
		go func(product string) {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			h, err := priceHistory(ctx, userAgent, product, key)
			c.mu.Lock()
			defer c.mu.Unlock()
			c.inflight[k], c.fetched[k] = false, time.Now()
			if err == nil && len(h.Closes) > 0 {
				c.first[k] = h.Closes[0]
			}
		}(f.Product)
	}
	return out
}

// assetRanges are the chart spans for one asset. 15M is the open round, from recorded
// evaluations; the rest are Coinbase candles, the same ones the widget draws.
var assetRanges = map[string]time.Duration{"15M": 15 * time.Minute, "1H": time.Hour, "24H": 24 * time.Hour, "7D": 7 * 24 * time.Hour}

const maxMarkers = 500

// homeRoutes mounts the three routes of docs/api-home.md that this file serves.
func homeRoutes(mux *http.ServeMux, db *store.Store, userAgent string, src Sources) {
	cache := &windows{by: map[string]window{}}
	moves := &changes{first: map[string]float64{}, fetched: map[string]time.Time{}, inflight: map[string]bool{}}

	mux.HandleFunc("GET /api/home", func(w http.ResponseWriter, r *http.Request) {
		key, length, ok := parseRange(r.URL.Query().Get("range"), "24H", homeRanges)
		if !ok {
			http.Error(w, "range must be 1H, 24H, 7D or ALL", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		writeJSON(w, homeDoc(src, key, cache.get(ctx, db, key, length), time.Now(), moves.get(userAgent, src, key)))
	})

	mux.HandleFunc("GET /api/asset", func(w http.ResponseWriter, r *http.Request) {
		coin := strings.ToUpper(r.URL.Query().Get("coin"))
		var asset *Asset
		for i := range assets {
			if assets[i].Coin == coin {
				asset = &assets[i]
			}
		}
		key, _, ok := parseRange(r.URL.Query().Get("range"), "15M", assetRanges)
		if asset == nil || !ok {
			http.Error(w, "coin must be one of BTC, ETH, SOL, XRP, DOGE and range one of 15M, 1H, 24H, 7D", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		feed, fed := src.feed(coin)
		points := [][2]float64{}
		var round any
		since := unixf(time.Now().Add(-assetRanges[key]))
		switch {
		case !fed:
		case key == "15M":
			st, ok := src.Round(feed.Series)
			if !ok || st.Ticker == "" {
				break
			}
			opens := unixf(st.Closes) - feed.RoundSeconds
			round, since = map[string]any{"ticker": st.Ticker, "strike": st.Strike, "opens": opens, "closes": unixf(st.Closes)}, opens
			recorded, err := db.RoundSeries(ctx, st.Ticker, 5*time.Second)
			if err != nil {
				http.Error(w, "query failed", http.StatusInternalServerError)
				return
			}
			for _, p := range recorded {
				if p.Price != nil {
					points = append(points, [2]float64{unixf(p.At), *p.Price})
				}
			}
		default:
			h, err := priceHistory(ctx, userAgent, feed.Product, key)
			if err != nil {
				http.Error(w, "history unavailable", http.StatusBadGateway)
				return
			}
			for i, c := range h.Closes {
				points = append(points, [2]float64{h.Times[i], c})
			}
			if len(points) > 0 {
				since = points[0][0]
			}
		}
		if len(points) > maxPoints {
			points = points[len(points)-maxPoints:]
		}

		v, books, _ := src.Valuation()
		var line runner.Line
		for _, c := range v.Coins {
			if c.Key == coin {
				line = c
			}
		}
		cost, value, _, unmarked := stake(line)
		positions := []map[string]any{}
		for _, b := range books {
			for _, p := range b.Positions {
				if p.Coin != coin {
					continue
				}
				positions = append(positions, map[string]any{"strategy": p.Strategy, "engine": p.Engine, "world": p.World, "side": p.Side,
					"contracts": p.Contracts, "entry_price": p.EntryPrice, "cost_cents": p.CostCents, "value_cents": p.ValueCents,
					"placed": p.Placed, "underlying_at_entry": p.Underlying, "ticker": p.Ticker})
			}
		}
		found := src.Markers(coin, since)
		sort.Slice(found, func(i, j int) bool { return found[i].T < found[j].T })
		truncated := len(found) > maxMarkers
		if truncated {
			found = found[len(found)-maxMarkers:] // the newest
		}
		markers := []map[string]any{}
		for _, m := range found {
			markers = append(markers, map[string]any{"t": m.T, "price": m.Price, "side": m.Side, "strategy": m.Strategy,
				"engine": m.Engine, "world": m.World, "kind": m.Kind})
		}
		writeJSON(w, map[string]any{"simulated": true, "coin": coin, "range": key, "decimals": asset.Decimals, "points": points, "round": round,
			"stake_cents": cost, "stake_value_cents": value, "unmarked_bets": unmarked, "positions": positions,
			"markers": markers, "markers_truncated": truncated})
	})

	// The buckets page asks the database, so its answer is kept for ten seconds.
	var (
		listMu   sync.Mutex
		listed   time.Time
		listRows []store.BucketRow
		listEvts []store.BucketEventRow
		listPol  store.SkimPolicy
	)
	mux.HandleFunc("GET /api/buckets", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		listMu.Lock()
		if time.Since(listed) > 10*time.Second {
			rows, err1 := db.Buckets(ctx)
			events, err2 := db.RecentBucketEvents(ctx, 30)
			policy, err3 := db.CurrentSkimPolicy(ctx)
			if err := firstErr(err1, err2, err3); err != nil {
				listMu.Unlock()
				slog.Warn("buckets: query failed", "err", err)
				http.Error(w, "query failed", http.StatusInternalServerError)
				return
			}
			listRows, listEvts, listPol, listed = rows, events, policy, time.Now()
		}
		rows, events, p := listRows, listEvts, listPol
		listMu.Unlock()

		v, books, _ := src.Valuation()
		capital, _ := src.Capital()
		writeJSON(w, map[string]any{
			"simulated": true,
			"policy": map[string]any{"id": p.ID, "since_at": p.EffectiveAt.UTC().Format(time.RFC3339), "note": p.Note,
				"winnings_bps": p.Winnings, "replenish_bps": p.Replenish, "tax_bps": p.Tax, "fees_bps": p.Fees},
			"money":   moneyDoc(capital.Money, deployedCents(v)),
			"buckets": bucketDocs(rows, books),
			"events":  events,
		})
	})
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// bucketDocs lays what the database lists beside what the engines hold right now. A bucket no
// engine holds (a frozen one) is worth the cash the ledger says it has, which after reaping is
// nothing. The order is the database's: live buckets first, then frozen.
func bucketDocs(rows []store.BucketRow, books []runner.Book) []map[string]any {
	held := map[int64]runner.BucketBook{}
	for _, book := range books {
		for _, b := range book.Buckets {
			if !b.Retired {
				held[b.BucketID] = b
			}
		}
	}
	out := []map[string]any{}
	for _, row := range rows {
		doc := map[string]any{"name": row.Name, "engine": fmt.Sprintf("v%d", row.Version), "strategy": strings.TrimPrefix(row.Strategy, "Anti "),
			"world": "real", "status": row.Status, "life": row.Life, "seed_cents": row.SeedCents, "equity_cents": row.CashCents,
			"cash_cents": row.CashCents, "at_risk_cents": int64(0), "high_water_cents": nil, "allocated_cents": row.AllocatedCents,
			"bets": row.Bets, "unmarked_bets": 0}
		if row.Anti {
			doc["world"] = "anti"
		}
		if b, ok := held[row.ID]; ok {
			doc["equity_cents"], doc["cash_cents"], doc["at_risk_cents"] = b.ValueCents(), b.CashCents, b.AtRiskCents
			doc["high_water_cents"], doc["unmarked_bets"] = b.HighWaterCents, b.Unmarked
		}
		out = append(out, doc)
	}
	return out
}
