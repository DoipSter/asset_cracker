package kalshi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var ladderAt = time.Date(2026, 9, 21, 16, 30, 0, 0, time.UTC)

// lm builds one listed market as the list sends it: prices and sizes as fixed-point strings.
func lm(ticker string, closes time.Time, yb, ybs, ya, yas, nb, na string) map[string]any {
	return map[string]any{"ticker": ticker, "event_ticker": "KXBTCD-26SEP2117", "strike_type": "greater",
		"floor_strike": 115999.99, "open_time": closes.Add(-time.Hour).Format(time.RFC3339), "close_time": closes.Format(time.RFC3339),
		"yes_bid_dollars": yb, "yes_bid_size_fp": ybs, "yes_ask_dollars": ya, "yes_ask_size_fp": yas,
		"no_bid_dollars": nb, "no_ask_dollars": na, "volume_24h_fp": "120.00", "volume_fp": "900.00",
		"open_interest_fp": "450.00", "liquidity_dollars": "1234.5600", "previous_yes_bid_dollars": "0.3900"}
}

func decode(t *testing.T, v any) []LadderMarket {
	t.Helper()
	b, err := json.Marshal(map[string]any{"markets": v})
	if err != nil {
		t.Fatal(err)
	}
	var p ladderPage
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p.Markets
}

func TestLadderListParses(t *testing.T) {
	raw := `{"markets":[{"ticker":"KXBTCD-26SEP2117-T115999.99","event_ticker":"KXBTCD-26SEP2117","strike_type":"greater",
	  "floor_strike":115999.99,"open_time":"2026-09-21T16:00:00Z","close_time":"2026-09-21T17:00:00Z",
	  "yes_bid_dollars":"0.4000","yes_ask_dollars":"0.4500","no_bid_dollars":"0.5500","no_ask_dollars":"0.6000",
	  "yes_bid_size_fp":"12.00","yes_ask_size_fp":"3.00","volume_24h_fp":null,"volume_fp":7,"open_interest_fp":"450.00",
	  "liquidity_dollars":"1234.5600","previous_yes_bid_dollars":"0.3900","unknown_field":{"x":1}}],"cursor":"abc"}`
	var p ladderPage
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if p.Cursor != "abc" || len(p.Markets) != 1 {
		t.Fatalf("page = %+v", p)
	}
	m := p.Markets[0]
	if m.YesBid != "0.4000" || m.YesAsk != "0.4500" || m.NoBid != "0.5500" || m.NoAsk != "0.6000" ||
		m.YesBidSize != "12.00" || m.YesAskSize != "3.00" || m.FloorStrike != "115999.99" ||
		m.Volume24h != "" || m.Volume != "7" || m.EventTicker != "KXBTCD-26SEP2117" || m.StrikeType != "greater" {
		t.Errorf("parsed %+v", m)
	}
}

func TestPlanLadderDecidesWhichMarketsGetARow(t *testing.T) {
	soon, gone := ladderAt.Add(30*time.Minute), ladderAt
	ms := decode(t, []any{
		lm("TWO", soon, "0.4000", "12.00", "0.4500", "3.00", "0.5500", "0.6000"),
		lm("YESONLY", soon, "0.9700", "5.00", "1.0000", "0.00", "0.0000", "0.0300"),
		lm("NOONLY", soon, "0.0000", "0.00", "0.0200", "100.00", "0.9800", "1.0000"),
		lm("EMPTY", soon, "0.0000", "0.00", "1.0000", "0.00", "0.0000", "1.0000"),
		lm("EMPTY0", soon, "0.0000", "0.00", "0.0000", "0.00", "0.0000", "0.0000"),
		lm("CROSSED", soon, "0.5000", "1.00", "0.5000", "1.00", "0.5000", "0.5000"),
		lm("CLOSED", gone, "0.4000", "1.00", "0.4500", "1.00", "0.5500", "0.6000"),
		lm("BAD", soon, "abc", "1.00", "0.4500", "1.00", "0.5500", "0.6000"),
	})
	other := lm("RANGE", soon, "0.4000", "1.00", "0.4500", "1.00", "0.5500", "0.6000")
	other["strike_type"] = "between"
	ms = append(ms, decode(t, []any{other})...)

	points, malformed, otherType := planLadder(ms, ladderAt)
	if malformed != 1 || otherType != 1 {
		t.Errorf("malformed %d, other strike type %d; want 1 and 1", malformed, otherType)
	}
	got := map[string]ladderPoint{}
	for _, p := range points {
		got[p.mk.Ticker] = p
	}
	for ticker, want := range map[string]struct{ in, row, two bool }{
		"TWO": {true, true, true}, "YESONLY": {true, false, false}, "NOONLY": {true, false, false}, // one-sided: listed, no row
		"EMPTY": {true, false, false}, "EMPTY0": {true, false, false}, "CROSSED": {true, false, false},
		"CLOSED": {false, false, false}, "BAD": {true, false, false}, "RANGE": {false, false, false},
	} {
		p, in := got[ticker]
		if in != want.in || p.row != want.row || p.twoSided != want.two {
			t.Errorf("%s: listed %v row %v two-sided %v; want %v %v %v", ticker, in, p.row, p.twoSided, want.in, want.row, want.two)
		}
	}
	two := got["TWO"]
	if two.mk.Strike == nil || *two.mk.Strike != 115999.99 || !two.mk.Closes.Equal(soon) || !two.mk.Opens.Equal(soon.Add(-time.Hour)) {
		t.Errorf("TWO market = %+v", two.mk)
	}
	q := two.quotes
	if q.YesBid != "0.4000" || q.YesAsk != "0.4500" || q.YesBidSize != "12.00" || q.YesAskSize != "3.00" ||
		len(q.YesBids) != 1 || q.YesBids[0] != [2]string{"0.4000", "12.00"} ||
		len(q.NoBids) != 1 || q.NoBids[0] != [2]string{"0.5500", "3.00"} || q.OpenInterest != "450.00" {
		t.Errorf("TWO quotes = %+v", q)
	}
	if n := got["YESONLY"].quotes.NoBids; len(n) != 0 {
		t.Errorf("YESONLY has no No bid, but no_bids = %v", n)
	}
	// The protocol's reads work on the stored JSON, and an empty side is an empty list, not null.
	b, _ := json.Marshal(got["YESONLY"].quotes)
	if !strings.Contains(string(b), `"no_bids":[]`) || !strings.Contains(string(b), `"yes_bids":[["0.9700","5.00"]]`) {
		t.Errorf("YESONLY quotes JSON = %s", b)
	}
}

func TestListAllFollowsTheCursor(t *testing.T) {
	page := func(cursor string, tickers ...string) ladderPage {
		p := ladderPage{Cursor: cursor}
		for _, tk := range tickers {
			p.Markets = append(p.Markets, LadderMarket{Ticker: tk})
		}
		return p
	}
	pages := map[string]ladderPage{"": page("c1", "A", "B"), "c1": page("c2", "B", "C"), "c2": page("", "D")}
	var asked []string
	got, err := listAll(func(c string) (ladderPage, error) { asked = append(asked, c); return pages[c], nil })
	if err != nil {
		t.Fatal(err)
	}
	var tickers []string
	for _, m := range got {
		tickers = append(tickers, m.Ticker)
	}
	if strings.Join(tickers, ",") != "A,B,C,D" || strings.Join(asked, ",") != ",c1,c2" {
		t.Errorf("tickers %v after asking %q", tickers, asked)
	}

	// A cursor on an empty page ends the list.
	pages = map[string]ladderPage{"": page("c1", "A"), "c1": page("c2")}
	if got, err := listAll(func(c string) (ladderPage, error) { return pages[c], nil }); err != nil || len(got) != 1 {
		t.Errorf("empty last page: %d markets, err %v", len(got), err)
	}
	// A repeated cursor, and a list that never ends, are errors rather than a short list.
	pages = map[string]ladderPage{"": page("c1", "A"), "c1": page("c1", "B")}
	if _, err := listAll(func(c string) (ladderPage, error) { return pages[c], nil }); err == nil {
		t.Error("a repeated cursor was not an error")
	}
	n := 0
	if _, err := listAll(func(string) (ladderPage, error) { n++; return page(fmt.Sprint(n), fmt.Sprint("M", n)), nil }); err == nil || n != ladderMaxPages {
		t.Errorf("an endless list: err %v after %d pages", err, n)
	}
	// A failed page fails the whole list.
	if _, err := listAll(func(string) (ladderPage, error) { return ladderPage{}, fmt.Errorf("boom") }); err == nil {
		t.Error("a failed page was not an error")
	}
}

func TestProbeDiffIsBookMinusList(t *testing.T) {
	points, _, _ := planLadder(decode(t, []any{lm("TWO", ladderAt.Add(time.Hour), "0.4000", "12.00", "0.4500", "3.00", "0.5500", "0.6000")}), ladderAt)
	p := points[0]
	book, err := BookQuotes([][]string{{"0.4100", "7"}}, [][]string{{"0.5500", "3.00"}})
	if err != nil {
		t.Fatal(err)
	}
	d := probeDiff(p, book, ladderAt, ladderAt.Add(180*time.Millisecond))
	diff := d["diff"].(map[string]any)
	if diff["yes_bid"] != 0.01 || diff["yes_ask"] != 0.0 || diff["yes_bid_size"] != -5.0 || diff["yes_ask_size"] != 0.0 ||
		d["same_prices"] != false || d["book_after_list_ms"] != int64(180) {
		t.Errorf("probe = %v", d)
	}
	same, _ := BookQuotes([][]string{{"0.4000", "12"}}, [][]string{{"0.5500", "3"}})
	if d := probeDiff(p, same, ladderAt, ladderAt); d["same_prices"] != true {
		t.Errorf("identical book: %v", d)
	}
	// A side missing from the book is null, never a difference against zero.
	oneSided, _ := BookQuotes([][]string{{"0.4000", "12"}}, nil)
	d = probeDiff(p, oneSided, ladderAt, ladderAt)
	if d["diff"].(map[string]any)["yes_ask"] != nil || d["book"].(map[string]any)["yes_ask"] != nil || d["same_prices"] != false {
		t.Errorf("one-sided book: %v", d)
	}
}

func TestPickProbeTakesTwoSidedMarketsInTurn(t *testing.T) {
	soon := ladderAt.Add(time.Hour)
	points, _, _ := planLadder(decode(t, []any{
		lm("B", soon, "0.4000", "1", "0.4500", "1", "0.5500", "0.6000"),
		lm("ONE", soon, "0.9700", "1", "1.0000", "0", "0.0000", "0.0300"),
		lm("A", soon, "0.4000", "1", "0.4500", "1", "0.5500", "0.6000"),
		lm("C", soon, "0.4000", "1", "0.4500", "1", "0.5500", "0.6000"),
	}), ladderAt)
	var got []string
	for n := 0; n < 4; n++ {
		got = append(got, points[pickProbe(points, n)].mk.Ticker)
	}
	if strings.Join(got, "") != "ABCA" {
		t.Errorf("probed %v, want A B C A", got)
	}
	if i := pickProbe(points[1:2], 0); i != -1 {
		t.Errorf("no two-sided market, picked %d", i)
	}
}

func TestDueResultsOldestFirstAndCapped(t *testing.T) {
	c := ladderAt
	w := map[string]*awaiting{
		"NEW": {closes: c}, "OLD": {closes: c.Add(-time.Hour)}, "LATER": {closes: c.Add(-time.Hour), nextTry: c.Add(time.Minute)},
		"OPEN": {closes: c.Add(time.Minute)}, "OLDB": {closes: c.Add(-time.Hour)},
	}
	if got := dueResults(w, c.Add(2*time.Second), 2, nil); strings.Join(got, ",") != "OLD,OLDB" {
		t.Errorf("due = %v", got)
	}
	if resultGap(time.Minute) != 0 || resultGap(time.Hour) != 10*time.Minute || resultGap(24*time.Hour) != time.Hour {
		t.Error("result retry gaps changed")
	}
}

type ladderFake struct {
	saves   [][]LadderNew
	rows    [][]LadderRow
	results map[int64]string
	next    int64
	traded  map[int64]bool // the markets a bucket has an order on
	asked   int            // Traded calls
}

func (f *ladderFake) Traded(_ context.Context, ids []int64) (map[int64]bool, error) {
	f.asked++
	out := map[int64]bool{}
	for _, id := range ids {
		if f.traded[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (f *ladderFake) SaveMarkets(_ context.Context, ms []LadderNew) (map[string]int64, error) {
	f.saves = append(f.saves, ms)
	out := map[string]int64{}
	for _, m := range ms {
		f.next++
		out[m.Ticker] = f.next
	}
	return out, nil
}
func (f *ladderFake) SaveRows(_ context.Context, rows []LadderRow) ([]int64, error) {
	f.rows = append(f.rows, rows)
	ids := make([]int64, len(rows))
	for i := range rows {
		f.next++
		ids[i] = f.next
	}
	return ids, nil
}

type watchFake struct{ calls []string }

func (w *watchFake) Watch(_ context.Context, series, coin string, at, closes time.Time, legs []LadderLeg) {
	w.calls = append(w.calls, fmt.Sprintf("%s|%s|%s|%d", series, coin, closes.UTC().Format("15:04"), len(legs)))
}

// The watcher gets each close's two-sided legs together, three or more, closes in order.
func TestWatchGroupsLegsByClose(t *testing.T) {
	c1, c2 := ladderAt.Add(time.Hour), ladderAt.Add(25*time.Hour)
	pt := func(ticker string, closes time.Time, strike float64, bid, ask int64, two bool) ladderPoint {
		return ladderPoint{mk: LadderNew{Ticker: ticker, Closes: closes, Strike: &strike}, yesBid: bid, yesAsk: ask, twoSided: two}
	}
	points := []ladderPoint{
		pt("B1", c2, 100, 4000, 4500, true), pt("B2", c2, 101, 3000, 3500, true), pt("B3", c2, 102, 2000, 2500, true), pt("B4", c2, 103, 0, 100, false),
		pt("A1", c1, 100, 6000, 6500, true), pt("A2", c1, 101, 5000, 5500, true), // two legs: not a distribution
	}
	w := &watchFake{}
	r := &LadderRecorder{Series: "KXBTCD", Coin: "BTC", Watch: w}
	r.watch(context.Background(), points, ladderAt)
	if len(w.calls) != 1 || w.calls[0] != "KXBTCD|BTC|"+c2.UTC().Format("15:04")+"|3" {
		t.Fatalf("watch calls %v", w.calls)
	}
}

// engineFake records what the recorder hands an engine: one Inputs and one Step per two-sided
// leg with the row's id, and a Settled per stored result.
type engineFake struct {
	inputs, steps []string
	stepIDs       []int64
	settled       []string
}

func (e *engineFake) Inputs(coin string, info MarketInfo, closes, at time.Time, price string) map[string]any {
	e.inputs = append(e.inputs, coin+"|"+info.Ticker+"|"+price)
	return map[string]any{"v": 3, "ok": true, "p_model": 0.5}
}
func (e *engineFake) Step(_ context.Context, coin string, evalID int64, at time.Time, marketID int64, info MarketInfo, closes time.Time, q Quotes, price string) {
	e.steps = append(e.steps, coin+"|"+info.Ticker+"|"+q.YesBid+"/"+q.NoBid)
	e.stepIDs = append(e.stepIDs, evalID)
}
func (e *engineFake) Settled(_ context.Context, coin string, marketID int64, info MarketInfo, closes time.Time) {
	e.settled = append(e.settled, coin+"|"+info.Ticker+"|"+info.Result)
}
func (f *ladderFake) SaveResult(_ context.Context, id int64, m MarketInfo) (bool, error) {
	_, had := f.results[id]
	if !had {
		f.results[id] = m.Result
	}
	return !had, nil
}
func (f *ladderFake) Unsettled(_ context.Context, since, before time.Time) (map[string]Pending, error) {
	return map[string]Pending{"OLD": {ID: 99, Closes: before.Add(-2 * time.Hour)}}, nil
}

// One recorder, three passes against a fake Kalshi: two while the markets are open, one after
// they close.
func TestLadderPassesRecordProbeAndSettle(t *testing.T) {
	closes := ladderAt.Add(30 * time.Minute)
	now := ladderAt
	var bookCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.Path; {
		case path == "/markets":
			if r.URL.Query().Get("series_ticker") != "KXBTCD" || r.URL.Query().Get("status") != "open" || r.URL.Query().Get("limit") != "1000" {
				t.Errorf("list query %q", r.URL.RawQuery)
			}
			if r.URL.Query().Get("cursor") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"cursor": "p2", "markets": []any{
					lm("TWO", closes, "0.4000", "12.00", "0.4500", "3.00", "0.5500", "0.6000"),
					lm("YESONLY", closes, "0.9700", "5.00", "1.0000", "0.00", "0.0000", "0.0300"),
					lm("EMPTY", closes, "0.0000", "0.00", "1.0000", "0.00", "0.0000", "1.0000")}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"cursor": "", "markets": []any{
				lm("NOONLY", closes, "0.0000", "0.00", "0.0200", "100.00", "0.9800", "1.0000")}})
		case strings.HasSuffix(path, "/orderbook"):
			bookCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"orderbook_fp": map[string]any{
				"yes_dollars": [][]string{{"0.4100", "7"}}, "no_dollars": [][]string{{"0.5500", "3.00"}}}})
		case strings.HasPrefix(path, "/markets/"):
			ticker := strings.TrimPrefix(path, "/markets/")
			_ = json.NewEncoder(w).Encode(map[string]any{"market": map[string]any{"ticker": ticker, "result": "yes",
				"expiration_value": "116000.01", "settlement_ts": closes.Add(time.Minute).Format(time.RFC3339)}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := NewClient("test")
	client.base = srv.URL
	sink := &ladderFake{results: map[int64]string{}}
	eng := &engineFake{}
	r := &LadderRecorder{Client: client, Sink: sink, Series: "KXBTCD", Price: func() string { return "115000.5" },
		Now: func() time.Time { return now }, Coin: "BTC", Engine: eng}
	ctx := context.Background()

	if err := r.pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.saves) != 1 || len(sink.saves[0]) != 4 {
		t.Fatalf("first pass saved %v, want the four listed markets once", sink.saves)
	}
	if len(sink.rows) != 1 || len(sink.rows[0]) != 1 {
		t.Fatalf("first pass wrote %d rows, want 1 (TWO: only a two-sided book gets a row)", len(sink.rows[0]))
	}
	// The engine saw the one two-sided leg: its view is journaled with the row, and it was
	// stepped with that row's id and the top of book in the engine's shape.
	if len(eng.inputs) != 1 || eng.inputs[0] != "BTC|TWO|115000.5" || len(eng.steps) != 1 || eng.steps[0] != "BTC|TWO|0.4000/0.5500" {
		t.Fatalf("engine inputs %v steps %v", eng.inputs, eng.steps)
	}
	if v, ok := sink.rows[0][0].Model["v3"].(map[string]any); !ok || v["p_model"] != 0.5 || eng.stepIDs[0] != sink.next {
		t.Fatalf("row model %v, step id %v (rows' last id %d)", sink.rows[0][0].Model, eng.stepIDs, sink.next)
	}
	probes := 0
	for _, row := range sink.rows[0] {
		if row.Model["source"] != "list" || row.Price != "115000.5" || !row.At.Equal(ladderAt) {
			t.Errorf("row %+v", row)
		}
		if pr, ok := row.Model["probe"].(map[string]any); ok {
			probes++
			if pr["same_prices"] != false || row.Quotes.YesBid != "0.4000" {
				t.Errorf("probe on %+v: %v", row.Quotes, pr)
			}
		}
	}
	if probes != 1 || bookCalls != 1 {
		t.Errorf("%d probes and %d book calls, want 1 and 1", probes, bookCalls)
	}
	if sink.results[99] != "yes" {
		t.Errorf("the earlier run's unsettled market got %q", sink.results[99])
	}

	now = now.Add(time.Minute)
	if err := r.pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.saves) != 1 || len(sink.rows) != 2 || len(sink.rows[1]) != 1 || bookCalls != 1 {
		t.Errorf("second pass: %d saves, %d row batches, %d book calls; want no new markets, 1 row, no probe",
			len(sink.saves), len(sink.rows), bookCalls)
	}
	for _, row := range sink.rows[1] {
		if _, ok := row.Model["probe"]; ok {
			t.Error("probed again within ten minutes")
		}
	}

	now = closes.Add(time.Minute) // the list still shows them, closed; results are out
	if err := r.pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.rows[2]) != 0 {
		t.Errorf("wrote %d rows for closed markets", len(sink.rows[2]))
	}
	if len(eng.settled) != 5 || eng.settled[0][:4] != "BTC|" || !strings.HasSuffix(eng.settled[0], "|yes") {
		t.Errorf("the engine settled %v, want the five results", eng.settled)
	}
	if len(sink.results) != 5 || len(r.waiting) != 0 || len(r.open) != 0 {
		t.Errorf("results %v, still waiting on %d, open %d; want all five stored", sink.results, len(r.waiting), len(r.open))
	}
}
