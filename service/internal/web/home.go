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
// round length: a setting, not Kalshi's open_time, and used to say when the open round opened
// only if Kalshi sent no open time for it.
type Feed struct {
	Coin, Product, Series string
	RoundSeconds          float64
}

// Sources is the running service as the home page sees it: typed, and all from memory.
type Sources struct {
	Release string
	Healthy func() bool
	// Books is the live engine's book and the ledger's side of the balance sheet, read together.
	Books   func() ([]runner.Book, store.Capital, bool)
	Markers func(coin string, since float64) []runner.Marker
	Feeds   []Feed
	Price   func(product string) (price, ageSeconds float64, ok bool)
	Round   func(series string) (kalshi.Status, bool)
	// Recording says how the once-a-minute value snapshots are going. Nil means nobody is
	// writing them (a test, or a build without the writer).
	Recording func() Recording
}

// Recording is the state of the value-snapshot writer: when it last wrote, and why it last did not.
type Recording struct {
	Started     time.Time
	LastWritten time.Time // zero until the first snapshot of this run
	LastProblem string    // why the most recent attempt wrote nothing; empty if it wrote
}

// RecordingStaleAfter is how long without a snapshot before the home page says so. A convention:
// the routine gap is one minute (a minute that falls between a round's close and its
// settlement is skipped), so five in a row is not routine.
const RecordingStaleAfter = 5 * time.Minute

// recordingError is the sentence the page shows when the value history has stopped growing.
func recordingError(r Recording, now time.Time) string {
	last := r.LastWritten
	if last.IsZero() {
		last = r.Started
	}
	if last.IsZero() || now.Sub(last) < RecordingStaleAfter {
		return ""
	}
	msg := fmt.Sprintf("No value snapshot has been written for %d minutes, so the earned figures and the value chart are not growing.", int(now.Sub(last).Minutes()))
	if r.LastProblem != "" {
		msg += " The writer's reason: " + r.LastProblem
	}
	return msg
}

func (s Sources) feed(coin string) (Feed, bool) {
	for _, f := range s.Feeds {
		if f.Coin == coin {
			return f, true
		}
	}
	return Feed{}, false
}

// Valuation marks the books to market right now, and gives back what it was made from.
func (s Sources) Valuation() (runner.Valuation, []runner.Book, store.Capital, bool) {
	books, capital, ok := s.Books()
	coins := make([]string, len(assets))
	for i, a := range assets {
		coins[i] = a.Coin
	}
	return runner.Value(books, capital, coins), books, capital, ok
}

// capitalKnown says whether the ledger's side has EVER been read. Until it has, contributed is
// not 0 but unknown, and everything worked out from it (earned, lifetime earned, the money
// buckets) is unknown too: served as 0, the whole balance would read as earnings.
func capitalKnown(c store.Capital, ok bool) bool { return ok || !c.ReadAt.IsZero() }

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
	SnapshotsTakenAt(ctx context.Context, scope string, keys []string, at time.Time) (map[string]store.ValueSnapshot, error)
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
	// Current Groups plus the keys a batch written before the archive rename used, so earned
	// over a range that starts in that era can still be a figure (thenGroup).
	keys := append([]string{}, runner.Groups...)
	keys = append(keys, "strategies", "anti", "v1")
	if w.groups, err = db.SnapshotsTakenAt(ctx, "group", keys, w.then.At); err != nil {
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

// get fills the cache under its own deadline, not the request's: the window is shared by every
// viewer, and one phone hanging up mid-lookup must not fail it for all of them.
func (c *windows) get(db homeReader, key string, length time.Duration) window {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if w, ok := c.by[key]; ok && now.Sub(w.fetched) < time.Minute {
		return w
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w, err := loadWindow(ctx, db, length, now)
	if err != nil {
		slog.Warn("home: value history lookup failed", "range", key, "err", err)
		w = c.by[key] // the last good one, or nothing
		if w.series == nil {
			w.series = [][2]int64{}
		}
		w.err, w.fetched = "value history could not be read; earned figures may be out of date", now.Add(-50*time.Second)
		if !w.have {
			w.err = "value history could not be read; earned figures are unknown"
		}
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

var groupLabels = map[string]string{"v3": "Live engine", "legacy": "Archived engines", "money": "Money buckets"}

// batchAccountsForTotal says whether the group rows found in the `then` batch add up to that
// batch's total, in value and in contributed. It is the test the zero-baseline rule (groupEarned)
// rests on. The writer makes the total BY adding the groups up (runner.Value) and a batch goes in
// all or nothing, so a batch whose groups add up to its total is whole, and a group with no row
// in it was not written because the release that wrote the batch did not have that group: it
// held nothing and had been given nothing. A batch that does NOT add up has lost rows some other
// way, and nothing can be said about a group missing from it. This is a check of the rows, not
// an assumption about which release wrote them.
func batchAccountsForTotal(then store.ValueSnapshot, groups map[string]store.ValueSnapshot) bool {
	if len(groups) == 0 {
		return false
	}
	var value, contributed int64
	for _, g := range groups {
		value, contributed = value+g.ValueCents, contributed+g.ContributedCents
	}
	return value == then.ValueCents && contributed == then.ContributedCents
}

// groupEarned is what one composition group earned over the range, or ok false when that cannot
// be known (the caller then serves null).
//
// The zero-baseline rule: a group with no row in the `then` batch, where that batch is whole
// (batchAccountsForTotal), did not exist then. It is compared with a baseline of value 0 and
// contributed 0, so what it earned over the range is everything it has earned: value less
// contributed now. Without the rule the third engine's line, which no batch written before its
// release has, would read "unknown" for every range that starts before that release (for ALL,
// for ever), and the lines would stop adding up to the total. With it they still add up: the
// total then was the sum of the groups that were there, and the missing one adds 0 to both sides.
func groupEarned(g runner.Line, w window, whole bool) (cents int64, ok bool) {
	then, found := thenGroup(g.Key, w.groups)
	switch {
	case found:
		cents, _ = earned(g, then, true)
		return cents, true
	case whole:
		return g.ValueCents - g.ContributedCents, true
	}
	return 0, false
}

// thenGroup is the snapshot the current group is compared with. "legacy" was stored as three
// keys (strategies, anti, v1) before those engines were archived; a batch that still has those
// rows is summed so earned over a range that starts before the rename is still a figure.
func thenGroup(key string, groups map[string]store.ValueSnapshot) (store.ValueSnapshot, bool) {
	if row, ok := groups[key]; ok {
		return row, true
	}
	if key != "legacy" {
		return store.ValueSnapshot{}, false
	}
	var sum store.ValueSnapshot
	for _, old := range []string{"strategies", "anti", "v1"} {
		row, ok := groups[old]
		if !ok {
			return store.ValueSnapshot{}, false
		}
		sum.ValueCents += row.ValueCents
		sum.ContributedCents += row.ContributedCents
	}
	return sum, true
}

func unixf(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func atof(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

// moneyDoc is where the money sits. Deployed cash comes from the engines' books; the rest is the
// ledger's, and is null, not 0, while the ledger has never been read.
func moneyDoc(m store.MoneyBuckets, deployed int64, known bool) map[string]any {
	doc := map[string]any{"deployed_cents": deployed, "winnings_cents": nil, "replenishment_cents": nil,
		"tax_reserve_cents": nil, "fee_reserve_cents": nil, "venue_fees_paid_cents": nil}
	if known {
		doc["winnings_cents"], doc["replenishment_cents"] = m.Winnings, m.Replenishment
		doc["tax_reserve_cents"], doc["fee_reserve_cents"], doc["venue_fees_paid_cents"] = m.TaxReserve, m.FeeReserve, m.FeesPaid
	}
	return doc
}

// The two things capital_error can say. Stale figures are still the last good ones; figures
// that were never read are not figures at all.
const (
	capitalStale   = "the ledger's side of the balance sheet could not be read; contributed and earned figures may be out of date"
	capitalUnknown = "the ledger's side of the balance sheet has not been read since the service started: contributed, earned and the money buckets are unknown (null), and the total leaves the money buckets out"
)

// capitalError is the capital_error string for a document, or "" when the read is good.
func capitalError(c store.Capital, ok bool) string {
	switch {
	case ok:
		return ""
	case capitalKnown(c, ok):
		return capitalStale
	}
	return capitalUnknown
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
	v, books, capital, capitalOK := src.Valuation()
	known := capitalKnown(capital, capitalOK)
	// A lookup that failed with no earlier good one to fall back on knows nothing: not "no
	// snapshot yet", which is a real answer and reads as nothing earned since now.
	historyKnown := w.have || w.err == ""

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
	total := map[string]any{"value_cents": v.Total.ValueCents, "earned_cents": nil, "earned_pct": nil, "range": key, "since": since,
		"window_complete": w.complete, "at_risk_cents": v.Total.AtRiskCents,
		"unrealized_cents": v.Total.ValueCents - v.Total.CashCents - v.Total.AtRiskCents,
		"unmarked_bets":    v.Total.Unmarked, "contributed_cents": nil, "lifetime_earned_cents": nil}
	if known {
		total["contributed_cents"], total["lifetime_earned_cents"] = v.Total.ContributedCents, v.Total.ValueCents-v.Total.ContributedCents
	}
	if known && historyKnown {
		total["earned_cents"], total["earned_pct"] = cents, pct
	}

	composition := []map[string]any{}
	whole := w.have && batchAccountsForTotal(w.then, w.groups)
	for _, g := range v.Groups {
		row := map[string]any{"key": g.Key, "label": groupLabels[g.Key], "buckets": g.Count, "value_cents": g.ValueCents, "earned_cents": nil}
		switch {
		case !known || !historyKnown:
		case !w.have:
			row["earned_cents"] = int64(0)
		default:
			if cents, ok := groupEarned(g, w, whole); ok {
				row["earned_cents"] = cents
			}
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
		if w.realised != nil || (!w.have && historyKnown) {
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
		"halted": halted, "total": total, "series": series, "money": moneyDoc(capital.Money, deployedCents(v), known),
		"composition": composition, "assets": list}
	if w.err != "" {
		doc["history_error"] = w.err
	}
	if msg := capitalError(capital, capitalOK); msg != "" {
		doc["capital_error"] = msg
	}
	if src.Recording != nil {
		if msg := recordingError(src.Recording(), now); msg != "" {
			doc["recording_error"] = msg
		}
	}
	return doc
}

// changes is each coin's price change over a range, from the cached Coinbase candles. It never
// waits for Coinbase: it answers from what it has (nil until a first fetch lands) and refreshes
// in the background, at most once a minute per coin and range.
type changes struct {
	mu       sync.Mutex
	first    map[string]float64   // product+range -> the first candle's close
	firstAt  map[string]time.Time // when that close was last fetched successfully
	fetched  map[string]time.Time // the last attempt, good or bad
	inflight map[string]bool
}

// changeTooOld says whether a first candle fetched `age` ago can still stand for the start of a
// range made of candles `candleSeconds` wide. While Coinbase's candles cannot be fetched the live
// price keeps moving, and a change worked out against an hours-old candle would still be
// labelled "1H". The allowance is a choice, not a measurement: two candles, and never under five
// minutes, so that the ordinary once-a-minute refresh and one or two failures do not blank it.
func changeTooOld(age time.Duration, candleSeconds int) bool {
	allowed := 2 * time.Duration(candleSeconds) * time.Second
	if allowed < 5*time.Minute {
		allowed = 5 * time.Minute
	}
	return age > allowed
}

func (c *changes) get(userAgent string, src Sources, key string) map[string]*float64 {
	out := map[string]*float64{}
	spec, ok := ranges[key]
	if !ok { // ALL: there is no candle range that means "since the first snapshot"
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range src.Feeds {
		k := f.Product + key
		if first, ok := c.first[k]; ok && first > 0 && !changeTooOld(time.Since(c.firstAt[k]), spec[0]) {
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
				c.first[k], c.firstAt[k] = h.Closes[0], h.fetched
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
	moves := &changes{first: map[string]float64{}, firstAt: map[string]time.Time{}, fetched: map[string]time.Time{}, inflight: map[string]bool{}}
	rounds := &roundPrices{by: map[string]roundPoints{}}

	mux.HandleFunc("GET /api/home", func(w http.ResponseWriter, r *http.Request) {
		key, length, ok := parseRange(r.URL.Query().Get("range"), "24H", homeRanges)
		if !ok {
			http.Error(w, "range must be 1H, 24H, 7D or ALL", http.StatusBadRequest)
			return
		}
		writeJSON(w, homeDoc(src, key, cache.get(db, key, length), time.Now(), moves.get(userAgent, src, key)))
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
		var points [][2]float64
		var round any
		since := unixf(time.Now().Add(-assetRanges[key]))
		switch {
		case !fed:
		case key == "15M":
			st, ok := src.Round(feed.Series)
			if !ok || st.Ticker == "" {
				break
			}
			// When the round opened is Kalshi's own open time if it sent one. Only without it is
			// the series' configured round length used, and the document says which it was.
			opens, measured := unixf(st.Opens), !st.Opens.IsZero()
			if !measured {
				opens = unixf(st.Closes) - feed.RoundSeconds
			}
			round, since = map[string]any{"ticker": st.Ticker, "strike": st.Strike, "opens": opens, "opens_measured": measured, "closes": unixf(st.Closes)}, opens
			var err error
			if points, err = rounds.get(db, st.Ticker, time.Now()); err != nil {
				http.Error(w, "query failed", http.StatusInternalServerError)
				return
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
		if points == nil {
			points = [][2]float64{} // a list, not null, for the page that iterates it
		}

		v, books, _, _ := src.Valuation()
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

	list := &bucketList{}
	mux.HandleFunc("GET /api/buckets", func(w http.ResponseWriter, r *http.Request) {
		l := list.get(db, time.Now())
		if !l.good {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		v, books, capital, capitalOK := src.Valuation()
		p := l.policy
		doc := map[string]any{
			"simulated": true,
			"policy": map[string]any{"id": p.ID, "since_at": p.EffectiveAt.UTC().Format(time.RFC3339), "note": p.Note,
				"winnings_bps": p.Winnings, "replenish_bps": p.Replenish, "tax_bps": p.Tax, "fees_bps": p.Fees},
			"money":   moneyDoc(capital.Money, deployedCents(v), capitalKnown(capital, capitalOK)),
			"buckets": bucketDocs(l.rows, books),
			"events":  l.events,
		}
		if l.err != "" {
			doc["buckets_error"] = l.err
		}
		if msg := capitalError(capital, capitalOK); msg != "" {
			doc["capital_error"] = msg
		}
		writeJSON(w, doc)
	})
}

type roundReader interface {
	RoundSeries(ctx context.Context, ticker string, every time.Duration) ([]store.SeriesPoint, error)
}

// roundPoints is one round's recorded prices as last read, or why they could not be.
type roundPoints struct {
	fetched time.Time
	points  [][2]float64
	err     error
}

// roundPrices keeps each open round's recorded prices for five seconds, which is the width of
// one of its points, so nothing newer could be shown sooner. The read walks up to a whole
// round of once-a-second evaluations, and without this every viewer's every poll ran it, all at
// once, each holding one of the pool's few connections. Reads are one at a time, under their
// own deadline and not the request's, and a failure is kept for the five seconds too.
type roundPrices struct {
	mu sync.Mutex
	by map[string]roundPoints
}

const roundPricesFor = 5 * time.Second

func (c *roundPrices) get(db roundReader, ticker string, now time.Time) ([][2]float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.by[ticker]; ok && now.Sub(p.fetched) < roundPricesFor {
		return p.points, p.err
	}
	for old, p := range c.by { // rounds that are over: there are five new tickers every quarter hour
		if now.Sub(p.fetched) > time.Minute {
			delete(c.by, old)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	recorded, err := db.RoundSeries(ctx, ticker, roundPricesFor)
	p := roundPoints{fetched: now, points: [][2]float64{}, err: err}
	for _, r := range recorded {
		if r.Price != nil {
			p.points = append(p.points, [2]float64{unixf(r.At), *r.Price})
		}
	}
	c.by[ticker] = p
	return p.points, p.err
}

type bucketReader interface {
	Buckets(ctx context.Context) ([]store.BucketRow, error)
	RecentBucketEvents(ctx context.Context, limit int) ([]store.BucketEventRow, error)
	CurrentSkimPolicy(ctx context.Context) (store.SkimPolicy, error)
}

// bucketListing is the database's part of /api/buckets as last read.
type bucketListing struct {
	rows   []store.BucketRow
	events []store.BucketEventRow
	policy store.SkimPolicy
	good   bool   // false until a read has worked
	err    string // set while the newest attempt is a failed one
}

// bucketList keeps that part for a minute. It adds up every bucket's whole ledger and order
// history, which only grows, and what moves second by second (cash, equity, at risk) is laid
// over it from the engines' books on every request anyway. A failed read is not tried again on
// the next poll but a minute later, like a good one, however many pages are polling; meanwhile
// the last good listing is served and the document says so. It reads under its own deadline,
// not the request's, because the result is shared.
type bucketList struct {
	mu    sync.Mutex
	tried time.Time // the last attempt, good or bad
	last  bucketListing
}

const bucketListFor = time.Minute

func (c *bucketList) get(db bucketReader, now time.Time) bucketListing {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tried.IsZero() && now.Sub(c.tried) < bucketListFor {
		return c.last
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	rows, err1 := db.Buckets(ctx)
	events, err2 := db.RecentBucketEvents(ctx, 30)
	policy, err3 := db.CurrentSkimPolicy(ctx)
	c.tried = now
	if err := firstErr(err1, err2, err3); err != nil {
		slog.Warn("buckets: query failed", "err", err)
		c.last.err = "the bucket list could not be read; seed, allocated, bets, status and events are the last good ones"
		return c.last
	}
	c.last = bucketListing{rows: rows, events: events, policy: policy, good: true}
	return c.last
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
