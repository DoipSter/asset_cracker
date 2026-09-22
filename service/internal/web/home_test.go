package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/runner"
	"github.com/doipster/asset_cracker/service/internal/store"
)

func TestParseRange(t *testing.T) {
	cases := []struct {
		raw, key string
		ok       bool
	}{
		{"", "24H", true}, {"1H", "1H", true}, {"24h", "24H", true}, {" 7d ", "7D", true}, {"ALL", "ALL", true},
		{"15M", "15M", false}, {"1Y", "1Y", false}, {"24H; drop table", "24H; DROP TABLE", false},
	}
	for _, c := range cases {
		key, _, ok := parseRange(c.raw, "24H", homeRanges)
		if key != c.key || ok != c.ok {
			t.Errorf("parseRange(%q) = %q, %v; want %q, %v", c.raw, key, ok, c.key, c.ok)
		}
	}
	if key, d, ok := parseRange("", "15M", assetRanges); key != "15M" || d != 15*time.Minute || !ok {
		t.Errorf("the asset chart defaults to %q %v %v", key, d, ok)
	}
	if _, _, ok := parseRange("ALL", "15M", assetRanges); ok {
		t.Error("the asset chart has no ALL")
	}
}

func TestEarned(t *testing.T) {
	now := runner.Line{ValueCents: 2_567_890, ContributedCents: 2_580_000}
	then := store.ValueSnapshot{ValueCents: 2_469_124, ContributedCents: 2_480_000} // a $1,000 restake since
	cents, pct := earned(now, then, true)
	if cents != -1234 {
		t.Errorf("earned %d, want -1234", cents)
	}
	if want := -1234.0 / 2_469_124 * 100; pct < want-1e-12 || pct > want+1e-12 {
		t.Errorf("earned %v%%, want %v%%", pct, want)
	}
	if cents, pct := earned(now, store.ValueSnapshot{}, false); cents != 0 || pct != 0 {
		t.Errorf("before any snapshot: earned %d (%v%%), want nothing", cents, pct)
	}
	if _, pct := earned(now, store.ValueSnapshot{}, true); pct != 0 {
		t.Errorf("a percentage of nothing: %v", pct)
	}
}

// fakeHistory is a value history held in memory: total snapshots oldest first.
type fakeHistory struct {
	totals []store.ValueSnapshot
	fail   error
	groups map[string]store.ValueSnapshot // the group rows of every batch; nil means the one lone row below
}

func (f fakeHistory) SnapshotAt(_ context.Context, _, _ string, t time.Time) (store.ValueSnapshot, bool, error) {
	for i := len(f.totals) - 1; i >= 0; i-- {
		if !f.totals[i].At.After(t) {
			return f.totals[i], true, f.fail
		}
	}
	return store.ValueSnapshot{}, false, f.fail
}

func (f fakeHistory) FirstSnapshot(context.Context, string, string) (store.ValueSnapshot, bool, error) {
	if len(f.totals) == 0 {
		return store.ValueSnapshot{}, false, f.fail
	}
	return f.totals[0], true, f.fail
}

func (f fakeHistory) SnapshotsTakenAt(_ context.Context, _ string, keys []string, at time.Time) (map[string]store.ValueSnapshot, error) {
	if f.groups != nil {
		out := map[string]store.ValueSnapshot{}
		for _, k := range keys { // like the database: only the keys asked for
			if g, ok := f.groups[k]; ok {
				out[k] = g
			}
		}
		return out, nil
	}
	return map[string]store.ValueSnapshot{"strategies": {At: at, ValueCents: 600_000, ContributedCents: 600_000}}, nil
}

func (f fakeHistory) SnapshotSeries(_ context.Context, _, _ string, since time.Time, _ int) ([][2]int64, error) {
	out := [][2]int64{}
	for _, s := range f.totals {
		if !s.At.Before(since) {
			out = append(out, [2]int64{s.At.Unix(), s.ValueCents})
		}
	}
	return out, nil
}

func (f fakeHistory) RealisedByCoin(context.Context, time.Time) (map[string]int64, error) {
	return map[string]int64{"BTC": -420}, nil
}

func TestLoadWindow(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	var five fakeHistory // five hours of history, one snapshot a minute
	for m := 300; m >= 1; m-- {
		five.totals = append(five.totals, store.ValueSnapshot{At: now.Add(-time.Duration(m) * time.Minute), ValueCents: int64(1_380_000 + m)})
	}
	first := five.totals[0].At

	cases := []struct {
		name     string
		db       fakeHistory
		length   time.Duration
		have     bool
		complete bool
		since    time.Time
	}{
		{"1H inside five hours", five, time.Hour, true, true, now.Add(-time.Hour)},
		{"24H with only five hours", five, 24 * time.Hour, true, false, first},
		{"ALL", five, 0, true, true, first},
		{"1H before any snapshot", fakeHistory{}, time.Hour, false, false, time.Time{}},
		{"ALL before any snapshot", fakeHistory{}, 0, false, false, time.Time{}},
	}
	for _, c := range cases {
		w, err := loadWindow(context.Background(), c.db, c.length, now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if w.have != c.have || w.complete != c.complete || !w.then.At.Equal(c.since) {
			t.Errorf("%s: have %v complete %v since %v; want %v %v %v", c.name, w.have, w.complete, w.then.At, c.have, c.complete, c.since)
		}
		if w.series == nil {
			t.Errorf("%s: series is nil and would serialise as null", c.name)
		}
	}
	if _, err := loadWindow(context.Background(), fakeHistory{fail: errors.New("down")}, time.Hour, now); err == nil {
		t.Error("a failed lookup was not reported")
	}
}

func testSources(books []runner.Book, capital store.Capital) Sources {
	return Sources{Release: "test", Healthy: func() bool { return true },
		Books:   func() ([]runner.Book, store.Capital, bool) { return books, capital, true },
		Markers: func(string, float64) []runner.Marker { return nil },
		Feeds:   []Feed{{Coin: "BTC", Product: "BTC-USD", Series: "KXBTC15M", RoundSeconds: 900}},
		Price:   func(string) (float64, float64, bool) { return 81234.56, 0.2, true },
		Round: func(string) (kalshi.Status, bool) {
			return kalshi.Status{Ticker: "KXBTC15M-26SEP210345-45", Strike: 81300.12, Closes: time.Unix(1_790_000_700, 0),
				Quotes: kalshi.Quotes{YesBid: "0.4100", YesAsk: "0.4300"}}, true
		}}
}

// Before anything has happened the home page must still get a whole document: every list a
// list, five assets, earned nothing since now.
func TestHomeBeforeAnythingExists(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	w, _ := loadWindow(context.Background(), fakeHistory{}, 24*time.Hour, now)
	doc := homeDoc(testSources(nil, store.Capital{}), "24H", w, now, nil)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range []string{"halted", "series", "composition", "assets"} {
		if strings.Contains(string(raw), `"`+list+`":null`) {
			t.Errorf("%s serialised as null: the page iterates it", list)
		}
	}
	var got struct {
		Simulated bool
		Total     struct {
			Earned   int64   `json:"earned_cents"`
			Since    float64 `json:"since"`
			Complete bool    `json:"window_complete"`
		}
		Series      [][2]int64
		Composition []struct {
			Key    string
			Earned *int64 `json:"earned_cents"`
		}
		Assets []struct {
			Coin   string
			Price  *float64
			Earned *int64 `json:"earned_cents"`
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Simulated || got.Total.Earned != 0 || got.Total.Complete || got.Total.Since != 1_790_000_000 {
		t.Errorf("total: %+v", got.Total)
	}
	if len(got.Series) != 1 || got.Series[0] != [2]int64{1_790_000_000, 0} {
		t.Errorf("series %v, want only the value right now", got.Series)
	}
	if len(got.Composition) != 3 || got.Composition[0].Key != "v3" || got.Composition[0].Earned == nil || *got.Composition[0].Earned != 0 {
		t.Errorf("composition: %+v", got.Composition)
	}
	var order []string
	for _, a := range got.Assets {
		order = append(order, a.Coin)
		if a.Earned == nil || *a.Earned != 0 {
			t.Errorf("%s earned %v before anything settled", a.Coin, a.Earned)
		}
	}
	if strings.Join(order, " ") != "BTC ETH SOL XRP DOGE" {
		t.Errorf("assets are %v", order)
	}
	if got.Assets[0].Price == nil || got.Assets[1].Price != nil {
		t.Error("BTC has a feed and a price; ETH has neither and its price must be null, not zero")
	}
}

func TestHomeAddsUp(t *testing.T) {
	v := int64(620)
	books := []runner.Book{{Engine: "v1", Series: "KXBTC15M", Halted: "engine says 1 cents, ledger says 2",
		Buckets:   []runner.BucketBook{{BucketID: 1, Name: "KXBTC15M Value v1", Engine: "v1", World: "real", CashCents: 14_000, AtRiskCents: 500, MarkedCents: 620}},
		Positions: []runner.Position{{Coin: "BTC", CostCents: 500, ValueCents: &v}}}}
	capital := store.Capital{Money: store.MoneyBuckets{Winnings: 100, External: 15_100}, Buckets: []store.BucketCapital{{ID: 1, Version: 1, ContributedCents: 15_000}}}
	now := time.Unix(1_790_000_000, 0)
	hist := fakeHistory{totals: []store.ValueSnapshot{{At: now.Add(-2 * time.Hour), ValueCents: 15_000, ContributedCents: 15_000}}}
	w, _ := loadWindow(context.Background(), hist, time.Hour, now)
	raw, _ := json.Marshal(homeDoc(testSources(books, capital), "1H", w, now, nil))
	var got struct {
		Halted []string
		Total  struct {
			Value      int64 `json:"value_cents"`
			Earned     int64 `json:"earned_cents"`
			AtRisk     int64 `json:"at_risk_cents"`
			Unrealized int64 `json:"unrealized_cents"`
		}
		Money       map[string]int64
		Composition []struct {
			Key    string
			Value  int64  `json:"value_cents"`
			Earned *int64 `json:"earned_cents"`
		}
		Assets []struct {
			Stake  int64  `json:"stake_cents"`
			Value  *int64 `json:"stake_value_cents"`
			Earned *int64 `json:"earned_cents"`
			Round  *struct {
				YesBid float64 `json:"yes_bid"`
			}
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, c := range got.Composition {
		sum += c.Value
	}
	if sum != got.Total.Value || got.Total.Value != 14_720 {
		t.Errorf("composition adds up to %d, total is %d, want 14720", sum, got.Total.Value)
	}
	if got.Total.Earned != -380 || got.Total.AtRisk != 500 || got.Total.Unrealized != 120 {
		t.Errorf("total: %+v", got.Total)
	}
	if got.Money["deployed_cents"] != 14_000 || got.Money["winnings_cents"] != 100 {
		t.Errorf("money: %v", got.Money)
	}
	if len(got.Halted) != 1 || !strings.HasPrefix(got.Halted[0], "v1 KXBTC15M: ") {
		t.Errorf("halted: %v", got.Halted)
	}
	// The fake history has a "strategies" group at that moment and no other, and that lone row
	// does not add up to the batch's total: the batch is not whole, so a group with no snapshot
	// to compare with reports null, not a made-up zero. The zero-baseline rule does not apply.
	if got.Composition[0].Earned != nil || got.Composition[1].Earned != nil || got.Composition[2].Earned != nil {
		t.Errorf("composition earned: %+v", got.Composition)
	}
	btc := got.Assets[0]
	if btc.Stake != 500 || btc.Value == nil || *btc.Value != 620 || btc.Earned == nil || *btc.Earned != -420 || btc.Round == nil || btc.Round.YesBid != 0.41 {
		t.Errorf("BTC: %+v", btc)
	}
}

// The widget used to be the whole site at /. It moved to /widget and asks for its API by
// absolute path, so it must work from there unchanged; / is the home page.
func TestPagesAndBadRequests(t *testing.T) {
	mux := http.NewServeMux()
	Routes(mux, nil, "test", func() map[string]any { return map[string]any{} }, testSources(nil, store.Capital{}))
	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := get("/"); code != 200 || !strings.Contains(body, `href="widget"`) {
		t.Errorf("/ answered %d", code)
	}
	if code, body := get("/widget"); code != 200 || !strings.Contains(body, `fetch("/api/status"`) {
		t.Errorf("/widget answered %d, or no longer asks for /api/status by absolute path", code)
	}
	for _, path := range []string{"/api/home?range=1Y", "/api/asset?coin=PEPE", "/api/asset?coin=BTC&range=ALL"} {
		if code, _ := get(path); code != 400 {
			t.Errorf("%s answered %d, want 400", path, code)
		}
	}
	if code, _ := get("/api/status"); code != 200 {
		t.Errorf("/api/status answered %d", code)
	}
}

func TestBucketDocs(t *testing.T) {
	mark := int64(100_000)
	rows := []store.BucketRow{
		{ID: 30, Name: "kalshi15m2 Anti Model v2 life 2", Status: "active", Strategy: "Anti Model", Version: 2, Anti: true, Life: 2, CashCents: 99_000, SeedCents: 100_000, Bets: 3},
		{ID: 10, Name: "kalshi15m2 Anti Model v2", Status: "frozen", Strategy: "Anti Model", Version: 2, Anti: true, Life: 1, SeedCents: 100_000, Bets: 40},
	}
	books := []runner.Book{{Engine: "v2", Buckets: []runner.BucketBook{{BucketID: 30, CashCents: 97_000, AtRiskCents: 2_000, MarkedCents: 2_400, HighWaterCents: &mark}}}}
	docs := bucketDocs(rows, books)
	live, frozen := docs[0], docs[1]
	if live["strategy"] != "Model" || live["world"] != "anti" || live["engine"] != "v2" || live["equity_cents"] != int64(99_400) || live["cash_cents"] != int64(97_000) {
		t.Errorf("live: %v", live)
	}
	if frozen["equity_cents"] != int64(0) || frozen["status"] != "frozen" || frozen["life"] != 1 {
		t.Errorf("frozen: %v", frozen)
	}
	third := bucketDocs([]store.BucketRow{{ID: 50, Name: "kalshi15m3 Scalper v3", Status: "active", Strategy: "Scalper", Version: 3, Life: 1, CashCents: 100_000, SeedCents: 100_000}},
		[]runner.Book{{Engine: "v3", Buckets: []runner.BucketBook{{BucketID: 50, Engine: "v3", World: "real", Version: 3, CashCents: 97_400, AtRiskCents: 2_600, MarkedCents: 2_450}}}})[0]
	if third["engine"] != "v3" || third["world"] != "real" || third["equity_cents"] != int64(99_850) || third["high_water_cents"] != (*int64)(nil) {
		t.Errorf("third engine: %v", third)
	}
	raw, _ := json.Marshal(bucketDocs(nil, nil))
	if string(raw) != "[]" {
		t.Errorf("no buckets serialised as %s", raw)
	}
}

// homeWith composes a home document from one bucket's book and whatever the ledger's side and
// the value history are said to be, and decodes it loosely: null and 0 must be told apart.
func homeWith(t *testing.T, capital store.Capital, capitalOK bool, w window) map[string]any {
	t.Helper()
	books := []runner.Book{{Engine: "v1", Series: "KXBTC15M", Buckets: []runner.BucketBook{{BucketID: 1, Name: "KXBTC15M Value v1", Engine: "v1", World: "real", CashCents: 14_000}}}}
	src := testSources(books, capital)
	src.Books = func() ([]runner.Book, store.Capital, bool) { return books, capital, capitalOK }
	raw, err := json.Marshal(homeDoc(src, "1H", w, time.Unix(1_790_000_000, 0), nil))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// A ledger that has never been read is not a ledger with nothing in it. Served as zeros, the
// whole balance would read as lifetime earnings.
func TestHomeWhenTheLedgerWasNeverRead(t *testing.T) {
	then := store.ValueSnapshot{At: time.Unix(1_789_996_400, 0), ValueCents: 15_000, ContributedCents: 15_000}
	w := window{have: true, complete: true, then: then, series: [][2]int64{}, realised: map[string]int64{},
		groups: map[string]store.ValueSnapshot{"v1": then}}

	doc := homeWith(t, store.Capital{}, false, w)
	total := doc["total"].(map[string]any)
	for _, field := range []string{"contributed_cents", "lifetime_earned_cents", "earned_cents", "earned_pct"} {
		if v, present := total[field]; !present || v != nil {
			t.Errorf("total.%s is %v, want null: the ledger has never been read", field, v)
		}
	}
	if total["value_cents"] != 14_000.0 {
		t.Errorf("the books are still known: value %v", total["value_cents"])
	}
	for _, row := range doc["composition"].([]any) {
		if g := row.(map[string]any); g["earned_cents"] != nil {
			t.Errorf("%s earned %v, want null", g["key"], g["earned_cents"])
		}
	}
	money := doc["money"].(map[string]any)
	if money["deployed_cents"] != 14_000.0 || money["winnings_cents"] != nil || money["venue_fees_paid_cents"] != nil {
		t.Errorf("money: %v; deployed cash is the books', the rest is the ledger's and unknown", money)
	}
	if doc["capital_error"] != capitalUnknown || doc["healthy"] != false {
		t.Errorf("capital_error %q, healthy %v", doc["capital_error"], doc["healthy"])
	}

	// A read that worked once and fails now: the last good figures are served, and said to be old.
	read := store.Capital{Money: store.MoneyBuckets{Winnings: 100, External: 15_100}, ReadAt: time.Unix(1_789_999_000, 0),
		Buckets: []store.BucketCapital{{ID: 1, Version: 1, ContributedCents: 15_000}}}
	doc = homeWith(t, read, false, w)
	total = doc["total"].(map[string]any)
	if total["contributed_cents"] != 15_100.0 || total["lifetime_earned_cents"] != -1_000.0 || total["earned_cents"] != -1_000.0 {
		t.Errorf("stale but known: %v", total)
	}
	if doc["capital_error"] != capitalStale || doc["money"].(map[string]any)["winnings_cents"] != 100.0 {
		t.Errorf("stale but known: capital_error %q, money %v", doc["capital_error"], doc["money"])
	}
	if _, present := homeWith(t, read, true, w)["capital_error"]; present {
		t.Error("capital_error on a good read")
	}
}

// A history lookup that failed with nothing earlier to fall back on knows nothing, which is not
// the same as there being no snapshot yet.
func TestHomeWhenTheHistoryWasNeverRead(t *testing.T) {
	cache := &windows{by: map[string]window{}}
	w := cache.get(fakeHistory{fail: errors.New("relation value_snapshot does not exist")}, "1H", time.Hour)
	if w.have || !strings.Contains(w.err, "unknown") {
		t.Fatalf("window: have %v, err %q", w.have, w.err)
	}
	capital := store.Capital{Money: store.MoneyBuckets{External: 15_000}, Buckets: []store.BucketCapital{{ID: 1, Version: 1, ContributedCents: 15_000}}}
	doc := homeWith(t, capital, true, w)
	total := doc["total"].(map[string]any)
	if total["earned_cents"] != nil || total["earned_pct"] != nil || total["contributed_cents"] != 15_000.0 || total["window_complete"] != false {
		t.Errorf("total: %v", total)
	}
	for _, row := range doc["composition"].([]any) {
		if g := row.(map[string]any); g["earned_cents"] != nil {
			t.Errorf("%s earned %v, want null", g["key"], g["earned_cents"])
		}
	}
	if btc := doc["assets"].([]any)[0].(map[string]any); btc["earned_cents"] != nil {
		t.Errorf("BTC earned %v, want null", btc["earned_cents"])
	}
	if doc["history_error"] != w.err {
		t.Errorf("history_error %q", doc["history_error"])
	}
}

// fakeBuckets is the database behind /api/buckets: it counts how often it is asked.
type fakeBuckets struct {
	asked int
	fail  error
	name  string
}

func (f *fakeBuckets) Buckets(context.Context) ([]store.BucketRow, error) {
	f.asked++
	return []store.BucketRow{{ID: 1, Name: f.name}}, f.fail
}

func (f *fakeBuckets) RecentBucketEvents(context.Context, int) ([]store.BucketEventRow, error) {
	return []store.BucketEventRow{}, nil
}

func (f *fakeBuckets) CurrentSkimPolicy(context.Context) (store.SkimPolicy, error) {
	return store.SkimPolicy{ID: 1}, nil
}

// The bucket list is read at most once a minute whether the read works or not, and a failure
// serves the last good list and says so.
func TestBucketListBacksOff(t *testing.T) {
	start := time.Unix(1_790_000_000, 0)
	db := &fakeBuckets{fail: errors.New("timeout"), name: "first"}
	list := &bucketList{}

	if l := list.get(db, start); l.good || db.asked != 1 {
		t.Fatalf("a first read that failed: good %v after %d reads", l.good, db.asked)
	}
	if list.get(db, start.Add(10*time.Second)); db.asked != 1 {
		t.Errorf("a failed read was tried again after 10 s: %d reads", db.asked)
	}
	db.fail = nil
	if l := list.get(db, start.Add(61*time.Second)); !l.good || l.err != "" || db.asked != 2 || l.rows[0].Name != "first" {
		t.Errorf("a minute later: %+v after %d reads", l, db.asked)
	}
	if list.get(db, start.Add(100*time.Second)); db.asked != 2 {
		t.Errorf("a good list was read again inside its minute: %d reads", db.asked)
	}
	db.fail, db.name = errors.New("timeout"), "second"
	l := list.get(db, start.Add(125*time.Second))
	if !l.good || l.err == "" || db.asked != 3 || len(l.rows) != 1 || l.rows[0].Name != "first" {
		t.Errorf("a failure after a good read must serve the good one and say so: %+v after %d reads", l, db.asked)
	}
	if list.get(db, start.Add(130*time.Second)); db.asked != 3 {
		t.Errorf("no backing off after a failure: %d reads", db.asked)
	}
	db.fail = nil
	if l := list.get(db, start.Add(190*time.Second)); l.err != "" || l.rows[0].Name != "second" {
		t.Errorf("recovered: %+v", l)
	}
}

// A price change is only "over the range" while the candle it is measured from is recent.
func TestChangeTooOld(t *testing.T) {
	cases := []struct {
		age    time.Duration
		candle int
		old    bool
	}{
		{70 * time.Second, 60, false}, // the ordinary once-a-minute refresh
		{5 * time.Minute, 60, false},
		{6 * time.Minute, 60, true},
		{9 * time.Minute, 300, false},
		{11 * time.Minute, 300, true},
		{90 * time.Minute, 3600, false},
		{3 * time.Hour, 3600, true},
	}
	for _, c := range cases {
		if got := changeTooOld(c.age, c.candle); got != c.old {
			t.Errorf("a first candle %v old, %d s candles: too old %v, want %v", c.age, c.candle, got, c.old)
		}
	}
}

// A finished candle's close is timed at its end; the one still forming at the fetch, never later.
func TestCloseTime(t *testing.T) {
	fetched := time.Unix(1_789_979_722, 0)
	if got := closeTime(time.Unix(1_789_974_000, 0), 3600, fetched); got != 1_789_977_600 {
		t.Errorf("a finished candle closed at %v", got)
	}
	if got := closeTime(time.Unix(1_789_977_600, 0), 3600, fetched); got != 1_789_979_722 {
		t.Errorf("the forming candle is timed at %v, want the fetch, not an end %d s in the future", got, 1_789_981_200-1_789_979_722)
	}
}

// fakeRounds is the recorded evaluations behind the 15M chart: it counts how often it is asked.
type fakeRounds struct{ asked int }

func (f *fakeRounds) RoundSeries(_ context.Context, _ string, _ time.Duration) ([]store.SeriesPoint, error) {
	f.asked++
	price := 81_234.5
	return []store.SeriesPoint{{At: time.Unix(1_790_000_000, 0), Price: &price}, {At: time.Unix(1_790_000_005, 0)}}, nil
}

// However many pages poll the 15M chart, a round's recorded prices are read once per point width.
func TestRoundPricesAreKeptForOnePoint(t *testing.T) {
	db, cache, start := &fakeRounds{}, &roundPrices{by: map[string]roundPoints{}}, time.Unix(1_790_000_010, 0)
	for i := 0; i < 3; i++ {
		points, err := cache.get(db, "KXBTC15M-A", start.Add(time.Duration(i)*time.Second))
		if err != nil || len(points) != 1 || points[0] != [2]float64{1_790_000_000, 81_234.5} {
			t.Fatalf("points %v, err %v: a moment with no price is left out", points, err)
		}
	}
	if db.asked != 1 {
		t.Errorf("three polls inside five seconds read the database %d times", db.asked)
	}
	if cache.get(db, "KXETH15M-A", start); db.asked != 2 {
		t.Errorf("another round's prices came from the first's: %d reads", db.asked)
	}
	if cache.get(db, "KXBTC15M-A", start.Add(6*time.Second)); db.asked != 3 {
		t.Errorf("not read again after five seconds: %d reads", db.asked)
	}
	if cache.get(db, "KXBTC15M-B", start.Add(16*time.Minute)); len(cache.by) != 1 {
		t.Errorf("%d rounds kept, want only the open one: finished rounds must not pile up", len(cache.by))
	}
}

// The page only hears that the value history stopped if the API says so: one skipped minute is
// routine, five are not, and before the first snapshot the clock runs from the service's start.
func TestRecordingError(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	for _, c := range []struct {
		name string
		r    Recording
		want string
	}{
		{"just started", Recording{Started: now.Add(-2 * time.Minute)}, ""},
		{"one routine skip", Recording{Started: now.Add(-time.Hour), LastWritten: now.Add(-2 * time.Minute), LastProblem: "between close and settlement"}, ""},
		{"stopped", Recording{Started: now.Add(-time.Hour), LastWritten: now.Add(-7 * time.Minute), LastProblem: "v2 is halted"}, "for 7 minutes"},
		{"never wrote", Recording{Started: now.Add(-10 * time.Minute), LastProblem: "no table"}, "for 10 minutes"},
		{"nobody writing", Recording{}, ""},
	} {
		got := recordingError(c.r, now)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to contain %q", c.name, got, c.want)
		}
		if c.want != "" && c.r.LastProblem != "" && !strings.Contains(got, c.r.LastProblem) {
			t.Errorf("%s: the writer's reason is missing from %q", c.name, got)
		}
	}
}

// fourGroupWorld is a service with no third-engine bucket anywhere: one first-engine bucket, a
// second-engine original and its twin, and the money buckets; and a value history whose batch of
// two hours ago has the four groups a release before the third engine wrote.
func fourGroupWorld(now time.Time) ([]runner.Book, store.Capital, fakeHistory) {
	v, e := int64(620), int64(900)
	books := []runner.Book{
		{Engine: "v1", Series: "KXBTC15M", Buckets: []runner.BucketBook{{BucketID: 1, Name: "KXBTC15M Value v1", Strategy: "Value", Engine: "v1", World: "real", CashCents: 14_000, AtRiskCents: 500, MarkedCents: 620}},
			Positions: []runner.Position{{Strategy: "Value", Engine: "v1", World: "real", Coin: "BTC", CostCents: 500, ValueCents: &v}}},
		{Engine: "v2", Buckets: []runner.BucketBook{
			{BucketID: 30, Name: "kalshi15m2 Model v2", Strategy: "Model", Engine: "v2", World: "real", CashCents: 98_000, AtRiskCents: 1_200, MarkedCents: 900},
			{BucketID: 20, Name: "kalshi15m2 Anti Model v2", Strategy: "Model", Engine: "v2", World: "anti", CashCents: 101_000},
		}, Positions: []runner.Position{{Strategy: "Model", Engine: "v2", World: "real", Coin: "ETH", CostCents: 1_200, ValueCents: &e}}},
	}
	capital := store.Capital{ReadAt: now, Money: store.MoneyBuckets{Winnings: 1_000, Replenishment: 41, TaxReserve: 300, External: 216_341},
		Buckets: []store.BucketCapital{{ID: 1, Version: 1, ContributedCents: 15_000}, {ID: 30, Version: 2, ContributedCents: 100_000}, {ID: 20, Version: 2, Anti: true, ContributedCents: 100_000}}}
	at := now.Add(-2 * time.Hour)
	hist := fakeHistory{totals: []store.ValueSnapshot{{At: at, ValueCents: 216_000, ContributedCents: 216_341}},
		groups: map[string]store.ValueSnapshot{
			"strategies": {At: at, ValueCents: 99_500, ContributedCents: 100_000},
			"anti":       {At: at, ValueCents: 100_200, ContributedCents: 100_000},
			"v1":         {At: at, ValueCents: 14_959, ContributedCents: 15_000},
			"money":      {At: at, ValueCents: 1_341, ContributedCents: 1_341}}}
	return books, capital, hist
}

// withThirdEngine stakes the third engine's two buckets in that world ($1,000 each, from
// outside) and gives one of them a bet that is 150 cents under water at the bid.
func withThirdEngine(books []runner.Book, capital store.Capital) ([]runner.Book, store.Capital) {
	b := int64(2_450)
	books = append(books, runner.Book{Engine: "v3", Buckets: []runner.BucketBook{
		{BucketID: 50, Name: "kalshi15m3 Scalper v3", Strategy: "Scalper", Engine: "v3", World: "real", Version: 3, CashCents: 97_400, AtRiskCents: 2_600, MarkedCents: 2_450},
		{BucketID: 51, Name: "kalshi15m3 Value v3", Strategy: "Value", Engine: "v3", World: "real", Version: 3, CashCents: 100_000},
	}, Positions: []runner.Position{{Strategy: "Scalper", Engine: "v3", World: "real", Coin: "BTC", CostCents: 2_600, ValueCents: &b}}})
	capital.Buckets = append(capital.Buckets, store.BucketCapital{ID: 50, Version: 3, ContributedCents: 100_000}, store.BucketCapital{ID: 51, Version: 3, ContributedCents: 100_000})
	capital.Money.External += 200_000
	return books, capital
}

type compositionDoc struct {
	Total struct {
		Value  int64  `json:"value_cents"`
		Earned *int64 `json:"earned_cents"`
	}
	Composition []struct {
		Key, Label string
		Buckets    int
		Value      int64  `json:"value_cents"`
		Earned     *int64 `json:"earned_cents"`
	}
}

func composed(t *testing.T, books []runner.Book, capital store.Capital, hist fakeHistory, now time.Time) (compositionDoc, []byte) {
	t.Helper()
	w, err := loadWindow(context.Background(), hist, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(homeDoc(testSources(books, capital), "1H", w, now, nil))
	if err != nil {
		t.Fatal(err)
	}
	var doc compositionDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc, raw
}

func TestThreeGroupsAddUp(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	books, capital, hist := fourGroupWorld(now)
	doc, raw := composed(t, books, capital, hist, now)
	if keys := compositionKeys(doc); strings.Join(keys, " ") != "v3 legacy money" {
		t.Fatalf("composition order: %v\n%s", keys, raw)
	}
	if live := doc.Composition[0]; live.Key != "v3" || live.Label != "Live engine" || live.Buckets != 0 || live.Value != 0 || live.Earned == nil || *live.Earned != 0 {
		t.Errorf("empty live engine line: %+v", live)
	}
	if arch := doc.Composition[1]; arch.Key != "legacy" || arch.Label != "Archived engines" || arch.Buckets != 3 || arch.Value != 214_520 || arch.Earned == nil || *arch.Earned != -139 {
		t.Errorf("archived engines line: %+v", arch)
	}
	if money := doc.Composition[2]; money.Key != "money" || money.Label != "Money buckets" || money.Buckets != 4 || money.Earned == nil || *money.Earned != 0 {
		t.Errorf("money line: %+v", money)
	}
	if doc.Total.Earned == nil || *doc.Total.Earned != -139 {
		t.Errorf("total earned %v, want -139", doc.Total.Earned)
	}
}

func TestLiveEngineGroupAddsUpToTheTotal(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	books, capital, hist := fourGroupWorld(now)
	books, capital = withThirdEngine(books, capital)
	hist.totals[0].ValueCents, hist.totals[0].ContributedCents = 216_000+200_000, 216_341+200_000
	hist.groups["v3"] = store.ValueSnapshot{At: hist.totals[0].At, ValueCents: 200_000, ContributedCents: 200_000}

	doc, raw := composed(t, books, capital, hist, now)
	var keys []string
	var value, earnedSum int64
	for _, c := range doc.Composition {
		keys = append(keys, c.Key)
		value += c.Value
		if c.Earned == nil {
			t.Fatalf("%s earned null: %s", c.Key, raw)
		}
		earnedSum += *c.Earned
	}
	if strings.Join(keys, " ") != "v3 legacy money" {
		t.Errorf("composition order: %v", keys)
	}
	if value != doc.Total.Value || doc.Total.Value != 215_861+199_850 {
		t.Errorf("composition adds up to %d, total is %d", value, doc.Total.Value)
	}
	if doc.Total.Earned == nil || earnedSum != *doc.Total.Earned || earnedSum != -139-150 {
		t.Errorf("composition earned adds up to %d, total earned %v, want -289", earnedSum, doc.Total.Earned)
	}
	live := doc.Composition[0]
	if live.Label != "Live engine" || live.Buckets != 2 || live.Value != 199_850 || *live.Earned != -150 {
		t.Errorf("live engine line: %+v", live)
	}
}

// The zero-baseline rule. A range that starts before the live engine existed is compared with
// a batch that has no row for it. Its earned figure is then everything it has earned (value less
// contributed). Archived v1/v2 rows stored under the old keys still feed the legacy line.
func TestARangeFromBeforeTheLiveEngineShowsFigures(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	books, capital, hist := fourGroupWorld(now)
	books, capital = withThirdEngine(books, capital)

	doc, raw := composed(t, books, capital, hist, now)
	var earnedSum int64
	for _, c := range doc.Composition {
		if c.Earned == nil {
			t.Fatalf("%s earned null over a range that starts before the live engine: %s", c.Key, raw)
		}
		earnedSum += *c.Earned
	}
	if live := doc.Composition[0]; live.Key != "v3" || *live.Earned != -150 {
		t.Errorf("live engine earned %d, want -150 (199,850 marked less 200,000 put in), not its stake", *live.Earned)
	}
	if arch := doc.Composition[1]; arch.Key != "legacy" || *arch.Earned != -139 {
		t.Errorf("legacy earned %d, want -139 (the old strategies/anti/v1 rows summed)", *arch.Earned)
	}
	if doc.Total.Earned == nil || *doc.Total.Earned != -139-150 || earnedSum != *doc.Total.Earned {
		t.Errorf("total earned %v, composition adds up to %d, want -289", doc.Total.Earned, earnedSum)
	}
	if money := doc.Composition[2]; money.Key != "money" || *money.Earned != 0 {
		t.Errorf("money earned %d: the live engine's seed came from outside and must not read as the money buckets' loss", *money.Earned)
	}

	delete(hist.groups, "anti")
	doc, _ = composed(t, books, capital, hist, now)
	if doc.Composition[0].Earned != nil || doc.Composition[1].Earned != nil || doc.Composition[2].Earned == nil {
		t.Errorf("a batch that does not add up: %+v", doc.Composition)
	}
}

func compositionKeys(doc compositionDoc) []string {
	keys := make([]string, len(doc.Composition))
	for i, c := range doc.Composition {
		keys[i] = c.Key
	}
	return keys
}
