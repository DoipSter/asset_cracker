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

func (f fakeHistory) SnapshotsTakenAt(_ context.Context, _ string, at time.Time) (map[string]store.ValueSnapshot, error) {
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
		Books:   func() []runner.Book { return books },
		Markers: func(string, float64) []runner.Marker { return nil },
		Capital: func() (store.Capital, bool) { return capital, true },
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
	if len(got.Composition) != 4 || got.Composition[0].Earned == nil || *got.Composition[0].Earned != 0 {
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
	// The fake history has a "strategies" group at that moment and no other: a group with no
	// snapshot to compare with reports null, not a made-up zero.
	if got.Composition[0].Earned == nil || got.Composition[2].Earned != nil {
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
	raw, _ := json.Marshal(bucketDocs(nil, nil))
	if string(raw) != "[]" {
		t.Errorf("no buckets serialised as %s", raw)
	}
}
