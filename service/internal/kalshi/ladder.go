package kalshi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The ladder recorder follows Kalshi's above/below ladders (KXBTCD and its kin: hourly, daily and
// weekly events in one series) for the long-shot protocol (docs/longshot-protocol.md, section 2).
//
// RECORD ONLY. It writes markets, one evaluation row per quoted market per pass, and results, and
// it can reach nothing else: its LadderSink has no engine behind it, so a result is stored and
// never settled against a bet. It is not part of the health check. A failure is logged and the
// pass is tried again later; nothing else stops.
//
// Its prices come from the market LIST, which Kalshi caches (quotes.go: measured stale once,
// 54/55 shown while the book had moved to 61). How stale is not measured, so every ten minutes
// one two-sided market's order book is fetched right after the list, and the difference is kept
// in that market's row (model.probe).
const (
	ladderEvery        = time.Minute      // [CONVENTION] one list call per series per pass
	ladderProbeEvery   = 10 * time.Minute // [CONVENTION]
	ladderCallTimeout  = 15 * time.Second
	ladderWriteTimeout = 30 * time.Second
	ladderMaxPages     = 20 // 1,000 markets a page; 425 were open in the largest series on 2026-09-21
	ladderResultAsks   = 20 // [CONVENTION] result requests per series per pass
	ladderGiveUpAfter  = 72 * time.Hour
)

// text is a value Kalshi sends as decimal text ("0.4500", "12.00"). A bare JSON number is kept as
// its literal, and null as "": a field that changes shape must not fail the whole list.
type text string

func (t *text) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "null":
		*t = ""
	case strings.HasPrefix(s, `"`):
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*t = text(strings.TrimSpace(v))
	default:
		*t = text(s)
	}
	return nil
}

// LadderMarket is one market of a ladder as the market list returns it (fields read from the
// public API on 2026-09-21).
type LadderMarket struct {
	Ticker       string `json:"ticker"`
	EventTicker  string `json:"event_ticker"`
	StrikeType   string `json:"strike_type"`
	FloorStrike  text   `json:"floor_strike"`
	OpenTime     string `json:"open_time"`
	CloseTime    string `json:"close_time"`
	YesBid       text   `json:"yes_bid_dollars"`
	YesAsk       text   `json:"yes_ask_dollars"`
	NoBid        text   `json:"no_bid_dollars"`
	NoAsk        text   `json:"no_ask_dollars"`
	YesBidSize   text   `json:"yes_bid_size_fp"`
	YesAskSize   text   `json:"yes_ask_size_fp"`
	Volume24h    text   `json:"volume_24h_fp"`
	Volume       text   `json:"volume_fp"`
	OpenInterest text   `json:"open_interest_fp"`
	Liquidity    text   `json:"liquidity_dollars"`
	PrevYesBid   text   `json:"previous_yes_bid_dollars"`
}

// LadderQuotes is what evaluation.quotes holds for a ladder row: the list's top of book as given.
// Keys that mean what they mean in Quotes have Quotes' names, so the 15-minute reads of the
// protocol (yes_bid, yes_ask, yes_bids->0->>1, no_bids->0->>1) read a ladder row the same way.
// no_bids' size is the list's yes_ask_size: buying Yes is filled by the best No bid. A level whose
// size did not come is left out rather than written as zero. There is no depth and no level
// count: the list does not give them.
type LadderQuotes struct {
	YesBid       string      `json:"yes_bid,omitempty"`
	YesAsk       string      `json:"yes_ask,omitempty"`
	NoBid        string      `json:"no_bid,omitempty"`
	NoAsk        string      `json:"no_ask,omitempty"`
	YesBidSize   string      `json:"yes_bid_size,omitempty"`
	YesAskSize   string      `json:"yes_ask_size,omitempty"`
	YesBids      [][2]string `json:"yes_bids"`
	NoBids       [][2]string `json:"no_bids"`
	Volume24h    string      `json:"volume_24h,omitempty"`
	Volume       string      `json:"volume,omitempty"`
	OpenInterest string      `json:"open_interest,omitempty"`
	Liquidity    string      `json:"liquidity,omitempty"`
	PrevYesBid   string      `json:"previous_yes_bid,omitempty"`
}

// LadderNew is a market seen for the first time.
type LadderNew struct {
	Ticker string
	Strike *float64
	Opens  time.Time // zero if the list sent none
	Closes time.Time
}

// LadderRow is one evaluation row.
type LadderRow struct {
	At       time.Time
	MarketID int64
	Price    string // the coin's latest price, "" when there is none fresh (stored as NULL)
	Quotes   LadderQuotes
	Model    map[string]any
}

// Quotes is the ladder's top of book in the shape the engine and the Paper read: the same
// prices, the one displayed level per side the list endpoint gives.
func (q LadderQuotes) Quotes() Quotes {
	return Quotes{YesBid: q.YesBid, YesAsk: q.YesAsk, NoBid: q.NoBid, NoAsk: q.NoAsk, YesAskSize: q.YesAskSize,
		YesBids: q.YesBids, NoBids: q.NoBids, YesLevels: len(q.YesBids), NoLevels: len(q.NoBids)}
}

// LadderEngine is a runner that trades this series' legs, in simulation: the third engine's
// runner over the ladder family. The recorder hands it each leg's snapshot after the row is
// written (Inputs for the model's view, then Step with the row's id), and each result after it
// is stored. nil means the series is recorded and nothing trades it, as before 2026-09-23.
type LadderEngine interface {
	Inputs(coin string, info MarketInfo, closes, at time.Time, price string) map[string]any
	Step(ctx context.Context, coin string, evalID int64, at time.Time, marketID int64, info MarketInfo, closes time.Time, q Quotes, price string)
	Settled(ctx context.Context, coin string, marketID int64, info MarketInfo, closes time.Time)
}

// LadderSink is where the recorder writes. It reaches the database and nothing else.
type LadderSink interface {
	// SaveMarkets records markets first seen and returns their ids by ticker.
	SaveMarkets(ctx context.Context, ms []LadderNew) (map[string]int64, error)
	// SaveRows appends evaluation rows and returns their ids, in order.
	SaveRows(ctx context.Context, rows []LadderRow) ([]int64, error)
	// SaveResult stores a result if none is stored yet (first writer wins).
	SaveResult(ctx context.Context, marketID int64, m MarketInfo) (bool, error)
	// Unsettled lists this series' markets that closed in [since, before) with no result.
	Unsettled(ctx context.Context, since, before time.Time) (map[string]Pending, error)
}

// Pacer spaces requests at least gap apart. One is shared by every series' recorder, so that
// together they add a bounded load beside the 15-minute pollers, which share Kalshi's limits.
type Pacer struct {
	mu   sync.Mutex
	gap  time.Duration
	next time.Time
}

// NewPacer returns a pacer allowing one request per gap.
func NewPacer(gap time.Duration) *Pacer { return &Pacer{gap: gap} }

// Wait returns when the caller may make its request. A nil pacer never waits.
func (p *Pacer) Wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	now := time.Now()
	at := p.next
	if at.Before(now) {
		at = now
	}
	p.next = at.Add(p.gap)
	p.mu.Unlock()
	d := time.Until(at)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type ladderPage struct {
	Markets []LadderMarket `json:"markets"`
	Cursor  string         `json:"cursor"`
}

// ladderPage fetches one page of a series' open markets, with their top of book.
func (c *Client) ladderPage(ctx context.Context, series, cursor string) (ladderPage, error) {
	path := "/markets?series_ticker=" + url.QueryEscape(series) + "&status=open&limit=1000"
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	var p ladderPage
	err := c.get(ctx, path, &p)
	return p, err
}

// listAll follows the list's cursor to its end. A market on two pages is kept once. A list that
// repeats a cursor or does not end is an error, never a quiet stop that would drop markets.
func listAll(fetch func(cursor string) (ladderPage, error)) ([]LadderMarket, error) {
	var out []LadderMarket
	seen, cursors := map[string]bool{}, map[string]bool{}
	cursor := ""
	for page := 0; page < ladderMaxPages; page++ {
		p, err := fetch(cursor)
		if err != nil {
			return nil, err
		}
		for _, m := range p.Markets {
			if m.Ticker != "" && !seen[m.Ticker] {
				seen[m.Ticker] = true
				out = append(out, m)
			}
		}
		if p.Cursor == "" || len(p.Markets) == 0 {
			return out, nil
		}
		if cursors[p.Cursor] {
			return nil, fmt.Errorf("market list repeated cursor %q", p.Cursor)
		}
		cursors[p.Cursor] = true
		cursor = p.Cursor
	}
	return nil, fmt.Errorf("market list did not end after %d pages", ladderMaxPages)
}

// ladderPoint is what one listed market becomes.
type ladderPoint struct {
	mk       LadderNew
	quotes   LadderQuotes
	yesBid   int64 // ten-thousandths; valid when twoSided
	yesAsk   int64
	row      bool // at least one side quoted: it gets an evaluation row
	twoSided bool // 0 < yes_bid < yes_ask < 1: it may be probed
}

// ladderPrice reads one price: present is false for an absent field, and err is set for a field
// that is present and not a price.
func ladderPrice(t text) (v int64, present bool, err error) {
	if t == "" {
		return 0, false, nil
	}
	v, err = parsePrice(string(t))
	return v, err == nil, err
}

// ladderSize keeps a size as given if it is a number, else "".
func ladderSize(t text) string {
	if f, err := strconv.ParseFloat(string(t), 64); err == nil && f >= 0 {
		return string(t)
	}
	return ""
}

// planLadder reads the list as it was at `at`. A market without a ticker or a parseable close,
// already closed, or of a strike type other than "greater" is left out. A market whose prices do
// not parse is kept (so its result is still asked for) but gets no row, and is counted.
func planLadder(ms []LadderMarket, at time.Time) (points []ladderPoint, malformed, otherType int) {
	inside := func(v int64, present bool) bool { return present && v > 0 && v < priceScale }
	for _, m := range ms {
		closes, err := time.Parse(time.RFC3339Nano, m.CloseTime)
		if m.Ticker == "" || err != nil || !at.Before(closes) {
			continue
		}
		if m.StrikeType != "" && m.StrikeType != "greater" {
			otherType++
			continue
		}
		p := ladderPoint{mk: LadderNew{Ticker: m.Ticker, Closes: closes}}
		if opens, err := time.Parse(time.RFC3339Nano, m.OpenTime); err == nil {
			p.mk.Opens = opens
		}
		if f, err := strconv.ParseFloat(string(m.FloorStrike), 64); err == nil {
			p.mk.Strike = &f
		}
		yb, ybOK, e1 := ladderPrice(m.YesBid)
		ya, yaOK, e2 := ladderPrice(m.YesAsk)
		nb, nbOK, e3 := ladderPrice(m.NoBid)
		na, naOK, e4 := ladderPrice(m.NoAsk)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			malformed++
			points = append(points, p)
			continue
		}
		q := LadderQuotes{YesBids: [][2]string{}, NoBids: [][2]string{},
			YesBidSize: ladderSize(m.YesBidSize), YesAskSize: ladderSize(m.YesAskSize),
			Volume24h: ladderSize(m.Volume24h), Volume: ladderSize(m.Volume), OpenInterest: ladderSize(m.OpenInterest),
			Liquidity: ladderSize(m.Liquidity), PrevYesBid: ladderSize(m.PrevYesBid)}
		for _, f := range []struct {
			v   int64
			ok  bool
			out *string
		}{{yb, ybOK, &q.YesBid}, {ya, yaOK, &q.YesAsk}, {nb, nbOK, &q.NoBid}, {na, naOK, &q.NoAsk}} {
			if f.ok {
				*f.out = formatPrice(f.v)
			}
		}
		if inside(yb, ybOK) && q.YesBidSize != "" {
			q.YesBids = [][2]string{{q.YesBid, q.YesBidSize}}
		}
		if inside(nb, nbOK) && q.YesAskSize != "" {
			q.NoBids = [][2]string{{q.NoBid, q.YesAskSize}}
		}
		p.quotes = q
		p.twoSided = inside(yb, ybOK) && inside(ya, yaOK) && yb < ya
		// Only a two-sided book gets a row: it is all the long-shot protocol can use (section 2,
		// "Eligible observation"), and recording every one-sided far strike as well was estimated
		// at up to 3.1M rows a day against about 0.26M for two-sided ones.
		p.row = p.twoSided
		p.yesBid, p.yesAsk = yb, ya
		points = append(points, p)
	}
	return points, malformed, otherType
}

// probeDiff compares a market's list quotes with its order book, fetched right after the list.
// Differences are book minus list, in dollars (prices) and contracts (sizes); a side the book
// does not have is null, never zero.
func probeDiff(p ladderPoint, book Quotes, listAt, bookAt time.Time) map[string]any {
	bk := map[string]any{"yes_bid": nil, "yes_ask": nil, "yes_bid_size": nil, "yes_ask_size": nil}
	diff := map[string]any{"yes_bid": nil, "yes_ask": nil, "yes_bid_size": nil, "yes_ask_size": nil}
	sizeDiff := func(bookSize, listSize string) any {
		b, err1 := strconv.ParseFloat(bookSize, 64)
		l, err2 := strconv.ParseFloat(listSize, 64)
		if err1 != nil || err2 != nil {
			return nil
		}
		return b - l
	}
	same := true
	if v, err := parsePrice(book.YesBid); err == nil && book.YesLevels > 0 {
		bk["yes_bid"], diff["yes_bid"] = book.YesBid, float64(v-p.yesBid)/priceScale
		same = same && v == p.yesBid
		if len(book.YesBids) > 0 {
			bk["yes_bid_size"] = book.YesBids[0][1]
			diff["yes_bid_size"] = sizeDiff(book.YesBids[0][1], p.quotes.YesBidSize)
		}
	} else {
		same = false
	}
	if v, err := parsePrice(book.YesAsk); err == nil && book.NoLevels > 0 {
		bk["yes_ask"], diff["yes_ask"] = book.YesAsk, float64(v-p.yesAsk)/priceScale
		same = same && v == p.yesAsk
		bk["yes_ask_size"] = book.YesAskSize
		diff["yes_ask_size"] = sizeDiff(book.YesAskSize, p.quotes.YesAskSize)
	} else {
		same = false
	}
	return map[string]any{"book_after_list_ms": bookAt.Sub(listAt).Milliseconds(), "book": bk, "diff": diff, "same_prices": same}
}

// pickProbe chooses the two-sided market to probe: they are taken in turn, by ticker, so over a
// day every part of the ladder is probed. -1 if none is two-sided.
func pickProbe(points []ladderPoint, n int) int {
	var two []int
	for i, p := range points {
		if p.twoSided && p.row {
			two = append(two, i)
		}
	}
	if len(two) == 0 {
		return -1
	}
	sort.Slice(two, func(a, b int) bool { return points[two[a]].mk.Ticker < points[two[b]].mk.Ticker })
	return two[n%len(two)]
}

// resultGap is how long to wait before asking again for a result that has not come. [CONVENTION]
func resultGap(age time.Duration) time.Duration {
	switch {
	case age < 15*time.Minute:
		return 0 // every pass
	case age < 6*time.Hour:
		return 10 * time.Minute
	default:
		return time.Hour
	}
}

// dueResults lists the closed markets to ask about now, oldest close first, at most n.
func dueResults(waiting map[string]*awaiting, now time.Time, n int) []string {
	var due []string
	for t, w := range waiting {
		if !now.Before(w.closes.Add(time.Second)) && !now.Before(w.nextTry) {
			due = append(due, t)
		}
	}
	sort.Slice(due, func(a, b int) bool {
		wa, wb := waiting[due[a]], waiting[due[b]]
		if !wa.closes.Equal(wb.closes) {
			return wa.closes.Before(wb.closes)
		}
		return due[a] < due[b]
	})
	if len(due) > n {
		due = due[:n]
	}
	return due
}

type ladderKnown struct {
	id     int64
	closes time.Time
}

// LadderRecorder records one ladder series. Run it on its own goroutine.
type LadderRecorder struct {
	Client *Client
	Sink   LadderSink
	Series string
	Price  func() string    // the coin's latest price, "" when none is fresh
	Pace   *Pacer           // shared by every recorder; nil never waits
	Start  time.Duration    // delay before the first pass, to stagger the series
	Now    func() time.Time // for tests
	// Coin is the series' underlying as the engine names it ("BTC"), and Engine the runner that
	// trades its legs. Both empty or nil: record only.
	Coin   string
	Engine LadderEngine

	open      map[string]ladderKnown // markets seen open, by ticker
	waiting   map[string]*awaiting   // closed, result not stored yet
	loaded    bool                   // the unsettled markets of an earlier run have been read
	nextProbe time.Time
	probes    int
	fails     int
	reported  bool // the first good pass has been logged
}

// Run records until ctx ends. A panic in a pass is stopped here and counted as a failed pass.
func (r *LadderRecorder) Run(ctx context.Context) {
	timer := time.NewTimer(r.Start)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		began := time.Now()
		err := r.guard(func() error { return r.pass(ctx) })
		wait := ladderEvery - time.Since(began)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.fails++
			wait = ladderEvery << min(r.fails-1, 3) // back off: 1, 2, 4, then 8 minutes
			slog.Warn("ladder recorder: pass failed", "series", r.Series, "err", err, "failures_in_a_row", r.fails, "next_in", wait)
		} else {
			r.fails = 0
		}
		timer.Reset(max(wait, time.Second))
	}
}

func (r *LadderRecorder) guard(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("ladder recorder: a panic was stopped", "series", r.Series, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return fn()
}

func (r *LadderRecorder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// pass is one minute's work: list, probe if due, write markets and rows, ask for results.
func (r *LadderRecorder) pass(ctx context.Context) error {
	if r.open == nil {
		r.open, r.waiting = map[string]ladderKnown{}, map[string]*awaiting{}
	}
	if !r.loaded {
		if err := r.loadUnsettled(ctx); err != nil {
			slog.Warn("ladder recorder: unsettled markets of an earlier run not read; will retry", "series", r.Series, "err", err)
		}
	}
	markets, err := listAll(func(cursor string) (ladderPage, error) {
		if err := r.Pace.Wait(ctx); err != nil {
			return ladderPage{}, err
		}
		cctx, cancel := context.WithTimeout(ctx, ladderCallTimeout)
		defer cancel()
		return r.Client.ladderPage(cctx, r.Series, cursor)
	})
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	at := r.now()
	points, malformed, otherType := planLadder(markets, at)
	if malformed > 0 || otherType > 0 {
		slog.Warn("ladder recorder: markets not recorded", "series", r.Series, "prices_unreadable", malformed, "strike_type_not_greater", otherType)
	}

	probeAt, probe := -1, map[string]any(nil)
	if !at.Before(r.nextProbe) {
		if probeAt = pickProbe(points, r.probes); probeAt >= 0 {
			probe = r.probe(ctx, points[probeAt], at)
			r.probes++
			r.nextProbe = at.Add(ladderProbeEvery)
		}
	}

	var fresh []LadderNew
	for _, p := range points {
		if _, ok := r.open[p.mk.Ticker]; !ok {
			fresh = append(fresh, p.mk)
		}
	}
	if len(fresh) > 0 {
		wctx, cancel := context.WithTimeout(ctx, ladderWriteTimeout)
		ids, err := r.Sink.SaveMarkets(wctx, fresh)
		cancel()
		if err != nil {
			return fmt.Errorf("save markets: %w", err)
		}
		for _, n := range fresh {
			if id, ok := ids[n.Ticker]; ok {
				r.open[n.Ticker] = ladderKnown{id: id, closes: n.Closes}
			}
		}
	}

	price := ""
	if r.Price != nil {
		price = r.Price()
	}
	trading := r.Engine != nil && r.Coin != ""
	rows := make([]LadderRow, 0, len(points))
	var looks []ladderPoint // the points behind rows, in the rows' order, for the engine
	for i, p := range points {
		k, ok := r.open[p.mk.Ticker]
		if !ok || !p.row {
			continue
		}
		model := map[string]any{"source": "list"}
		if i == probeAt {
			model["probe"] = probe
		}
		if trading && p.twoSided {
			// The model's view of this leg, journaled with the row as the rounds' poller does
			// (under "v3"), so any minute's decision can be recomputed from the row.
			if view := r.Engine.Inputs(r.Coin, p.mk.info(), p.mk.Closes, at, price); view != nil {
				model["v3"] = view
			}
		}
		rows = append(rows, LadderRow{At: at, MarketID: k.id, Price: price, Quotes: p.quotes, Model: model})
		looks = append(looks, p)
	}
	wctx, cancel := context.WithTimeout(ctx, ladderWriteTimeout)
	ids, err := r.Sink.SaveRows(wctx, rows)
	cancel()
	if err != nil {
		return fmt.Errorf("save rows: %w", err)
	}
	if trading && len(ids) == len(rows) {
		// One look per two-sided leg, with the row's id behind it. Step holds the runner's lock
		// briefly per leg and gives up when it is busy; a leg skipped this minute is looked at
		// the next.
		for i, p := range looks {
			if !p.twoSided {
				continue
			}
			r.Engine.Step(ctx, r.Coin, ids[i], at, rows[i].MarketID, p.mk.info(), p.mk.Closes, p.quotes.Quotes(), price)
		}
	}
	if !r.reported {
		r.reported = true
		slog.Info("ladder recorder: recording", "series", r.Series, "open_markets", len(points), "rows", len(rows), "trading", trading)
	}
	r.settle(ctx, at)
	return nil
}

// info is the leg as the engine's market: ticker, strike, close.
func (m LadderNew) info() MarketInfo {
	return MarketInfo{Ticker: m.Ticker, FloorStrike: m.Strike, CloseTime: m.Closes.UTC().Format(time.RFC3339Nano)}
}

// probe fetches one market's order book right after the list and compares the two.
func (r *LadderRecorder) probe(ctx context.Context, p ladderPoint, listAt time.Time) map[string]any {
	err := r.Pace.Wait(ctx)
	var book Quotes
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, ladderCallTimeout)
		book, err = r.Client.OrderBook(cctx, p.mk.Ticker)
		cancel()
	}
	bookAt := r.now()
	if err != nil {
		return map[string]any{"error": err.Error(), "book_after_list_ms": bookAt.Sub(listAt).Milliseconds()}
	}
	return probeDiff(p, book, listAt, bookAt)
}

func (r *LadderRecorder) loadUnsettled(ctx context.Context) error {
	now := r.now()
	wctx, cancel := context.WithTimeout(ctx, ladderWriteTimeout)
	defer cancel()
	old, err := r.Sink.Unsettled(wctx, now.Add(-ladderGiveUpAfter), now)
	if err != nil {
		return err
	}
	for t, p := range old {
		if _, ok := r.waiting[t]; !ok {
			r.waiting[t] = &awaiting{id: p.ID, closes: p.Closes}
		}
	}
	r.loaded = true
	return nil
}

// settle asks for the results of closed markets, as the 15-minute poller does, and stores each
// with the first-writer-wins RecordResult behind the sink. Nothing is settled against a bet: no
// engine trades these markets. A failure here is logged and left to the next pass.
func (r *LadderRecorder) settle(ctx context.Context, now time.Time) {
	for t, k := range r.open {
		if !now.Before(k.closes) {
			r.waiting[t] = &awaiting{id: k.id, closes: k.closes}
			delete(r.open, t)
		}
	}
	for _, t := range dueResults(r.waiting, now, ladderResultAsks) {
		w := r.waiting[t]
		if err := r.Pace.Wait(ctx); err != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, ladderCallTimeout)
		m, err := r.Client.Market(cctx, t)
		cancel()
		if err != nil && !errors.Is(err, ErrNotFound) {
			slog.Warn("ladder recorder: result not read", "series", r.Series, "ticker", t, "err", err)
			return
		}
		if err == nil && (m.Result == "yes" || m.Result == "no") {
			wctx, cancel := context.WithTimeout(ctx, ladderWriteTimeout)
			_, err := r.Sink.SaveResult(wctx, w.id, m)
			cancel()
			if err != nil {
				slog.Warn("ladder recorder: result not stored", "series", r.Series, "ticker", t, "err", err)
				return
			}
			if r.Engine != nil && r.Coin != "" {
				// The result is stored: now the engine settles what its buckets hold in this leg.
				r.Engine.Settled(ctx, r.Coin, w.id, m, w.closes)
			}
			delete(r.waiting, t)
			continue
		}
		if age := now.Sub(w.closes); age > ladderGiveUpAfter {
			slog.Warn("ladder recorder: no result; giving up", "series", r.Series, "ticker", t, "closed", w.closes.UTC().Format(time.RFC3339), "result", m.Result)
			delete(r.waiting, t)
		} else {
			w.nextTry = now.Add(resultGap(age))
		}
	}
}
