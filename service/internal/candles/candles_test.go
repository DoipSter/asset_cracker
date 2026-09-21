package candles

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// All candles here are synthetic: the numbers are placeholders, not prices.

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestComplete(t *testing.T) {
	start := at("2026-09-21T09:00:00Z")
	for _, c := range []struct {
		now  string
		want bool
	}{
		{"2026-09-21T09:30:00Z", false}, // running
		{"2026-09-21T10:00:00Z", false}, // just ended, not settled
		{"2026-09-21T10:00:59Z", false},
		{"2026-09-21T10:01:00Z", true}, // ended + Settle
		{"2026-09-22T00:00:00Z", true},
	} {
		if got := Complete(start, time.Hour, at(c.now)); got != c.want {
			t.Errorf("Complete(09:00, 1h, %s) = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestLastComplete(t *testing.T) {
	for _, c := range []struct {
		g         time.Duration
		now, want string
	}{
		{time.Hour, "2026-09-21T10:00:30Z", "2026-09-21T08:00:00Z"},
		{time.Hour, "2026-09-21T10:01:00Z", "2026-09-21T09:00:00Z"},
		{24 * time.Hour, "2026-09-21T00:00:30Z", "2026-09-19T00:00:00Z"},
		{24 * time.Hour, "2026-09-21T15:00:00Z", "2026-09-20T00:00:00Z"},
	} {
		got := LastComplete(c.g, at(c.now))
		if !got.Equal(at(c.want)) {
			t.Errorf("LastComplete(%v, %s) = %s, want %s", c.g, c.now, got, c.want)
		}
		if !Complete(got, c.g, at(c.now)) || Complete(got.Add(c.g), c.g, at(c.now)) {
			t.Errorf("LastComplete(%v, %s) disagrees with Complete", c.g, c.now)
		}
	}
}

// checkWindows asserts the paging rule: newest first, contiguous, no overlap, at most max each,
// covering exactly [first, last].
func checkWindows(t *testing.T, ws []Window, g time.Duration, max int, first, last time.Time) {
	t.Helper()
	if len(ws) == 0 {
		t.Fatal("no windows")
	}
	if !ws[0].Last.Equal(last) || !ws[len(ws)-1].First.Equal(first) {
		t.Fatalf("cover %s..%s, want %s..%s", ws[len(ws)-1].First, ws[0].Last, first, last)
	}
	for i, w := range ws {
		n := int(w.Last.Sub(w.First)/g) + 1
		if n < 1 || n > max {
			t.Fatalf("window %d holds %d candles, max %d", i, n, max)
		}
		if i > 0 && !w.Last.Add(g).Equal(ws[i-1].First) {
			t.Fatalf("window %d ends %s, next starts %s: gap or overlap", i, w.Last, ws[i-1].First)
		}
	}
}

func TestWindowsThreeYearsHourly(t *testing.T) {
	now := at("2026-09-21T12:30:00Z")
	since := at("2023-09-21T00:00:00Z")
	ws := Windows(since, now, time.Hour, MaxPerRequest)
	last := at("2026-09-21T11:00:00Z")
	checkWindows(t, ws, time.Hour, MaxPerRequest, since, last)
	candles := int(last.Sub(since)/time.Hour) + 1 // 1096 days of 24, plus 12 today
	if candles != 1096*24+12 {
		t.Fatalf("candles = %d", candles)
	}
	if want := (candles + MaxPerRequest - 1) / MaxPerRequest; len(ws) != want {
		t.Fatalf("%d windows, want %d", len(ws), want)
	}
}

func TestWindowsDailyUnalignedSince(t *testing.T) {
	now := at("2026-09-21T12:30:00Z")
	ws := Windows(at("2023-09-21T06:00:00Z"), now, 24*time.Hour, MaxPerRequest)
	// The candle starting 2023-09-21 00:00 begins before `since`, so the first is the next day's.
	checkWindows(t, ws, 24*time.Hour, MaxPerRequest, at("2023-09-22T00:00:00Z"), at("2026-09-20T00:00:00Z"))
	if len(ws) != 4 { // 1095 candles
		t.Fatalf("%d windows, want 4", len(ws))
	}
}

func TestWindowsEdges(t *testing.T) {
	now := at("2026-09-21T12:30:00Z")
	if ws := Windows(now, now, time.Hour, MaxPerRequest); len(ws) != 0 {
		t.Fatalf("since after the last complete candle: %d windows, want 0", len(ws))
	}
	// Exactly one candle.
	ws := Windows(at("2026-09-21T11:00:00Z"), now, time.Hour, MaxPerRequest)
	checkWindows(t, ws, time.Hour, MaxPerRequest, at("2026-09-21T11:00:00Z"), at("2026-09-21T11:00:00Z"))
	// max 1: one window per candle.
	ws = Windows(at("2026-09-21T07:00:00Z"), now, time.Hour, 1)
	checkWindows(t, ws, time.Hour, 1, at("2026-09-21T07:00:00Z"), at("2026-09-21T11:00:00Z"))
	if len(ws) != 5 {
		t.Fatalf("%d windows, want 5", len(ws))
	}
}

func TestRecent(t *testing.T) {
	now := at("2026-09-21T12:05:00Z")
	// Nothing stored: one full request ending at the newest complete candle.
	w, ok := Recent(time.Time{}, false, time.Hour, now)
	if !ok || !w.Last.Equal(at("2026-09-21T11:00:00Z")) || int(w.Last.Sub(w.First)/time.Hour)+1 != MaxPerRequest {
		t.Fatalf("nothing stored: %+v %v", w, ok)
	}
	// Up to date: re-fetch the two newest stored, and the new one.
	w, ok = Recent(at("2026-09-21T10:00:00Z"), true, time.Hour, now)
	if !ok || !w.First.Equal(at("2026-09-21T08:00:00Z")) || !w.Last.Equal(at("2026-09-21T11:00:00Z")) {
		t.Fatalf("up to date: %+v %v", w, ok)
	}
	// A long gap is capped at one request, which the caller can see.
	latest := at("2026-08-01T00:00:00Z")
	w, ok = Recent(latest, true, time.Hour, now)
	if !ok || int(w.Last.Sub(w.First)/time.Hour)+1 != MaxPerRequest || !w.First.After(latest.Add(time.Hour)) {
		t.Fatalf("long gap: %+v %v", w, ok)
	}
	// Daily, nothing new yet: the overlap alone is fetched again.
	w, ok = Recent(at("2026-09-20T00:00:00Z"), true, 24*time.Hour, now)
	if !ok || !w.First.Equal(at("2026-09-18T00:00:00Z")) || !w.Last.Equal(at("2026-09-20T00:00:00Z")) {
		t.Fatalf("daily: %+v %v", w, ok)
	}
}

func bar(start string, x string) coinbase.Bar {
	return coinbase.Bar{Start: at(start), Low: x, High: x, Open: x, Close: x, Volume: "1"}
}

func TestKeepDropsIncompleteAndDuplicates(t *testing.T) {
	now := at("2026-09-21T12:00:30Z")
	in := []coinbase.Bar{
		bar("2026-09-21T11:00:00Z", "3"), // ended 12:00, not settled
		bar("2026-09-21T10:00:00Z", "2"),
		bar("2026-09-21T09:00:00Z", "1"),
		bar("2026-09-21T10:00:00Z", "9"), // a second copy: the first wins
		bar("2026-09-21T12:00:00Z", "4"), // running
	}
	got := Keep(in, time.Hour, now)
	if len(got) != 2 || !got[0].Start.Equal(at("2026-09-21T09:00:00Z")) || got[1].Open != "2" {
		t.Fatalf("Keep = %+v", got)
	}
}

type fakeDB struct {
	stored map[time.Time]store.Candle
	fail   error
}

func (f *fakeDB) InsertCandles(_ context.Context, cs []store.Candle) (int, int, error) {
	if f.fail != nil {
		return 0, 0, f.fail
	}
	n, d := 0, 0
	for _, c := range cs {
		old, ok := f.stored[c.At]
		switch {
		case !ok:
			f.stored[c.At] = c
			n++
		case old.Open != c.Open || old.Close != c.Close:
			d++ // never updated, only counted
		}
	}
	return n, d, nil
}

func TestStepStoresOnlyNewCompleteCandles(t *testing.T) {
	now := at("2026-09-21T12:00:30Z")
	clock := func() time.Time { return now }
	fetch := func(_ context.Context, product string, gs int, first, last time.Time) ([]coinbase.Bar, error) {
		if product != "BTC-USD" || gs != 3600 {
			t.Fatalf("fetch %s %d", product, gs)
		}
		return []coinbase.Bar{bar("2026-09-21T09:00:00Z", "1"), bar("2026-09-21T10:00:00Z", "2"), bar("2026-09-21T11:00:00Z", "3")}, nil
	}
	db := &fakeDB{stored: map[time.Time]store.Candle{
		at("2026-09-21T09:00:00Z"): {Open: "1", Close: "1"}, // same as Coinbase's
		at("2026-09-21T10:00:00Z"): {Open: "5", Close: "5"}, // Coinbase has changed it since
	}}
	w := Window{First: at("2026-09-21T09:00:00Z"), Last: at("2026-09-21T11:00:00Z")}
	c, err := Step(context.Background(), fetch, db, clock, 7, "BTC-USD", time.Hour, w)
	if err != nil {
		t.Fatal(err)
	}
	want := Counts{Requests: 1, Fetched: 3, Complete: 2, Inserted: 0, Differing: 1}
	if c != want {
		t.Fatalf("counts %v, want %v", c, want)
	}
	if got := db.stored[at("2026-09-21T10:00:00Z")].Open; got != "5" {
		t.Fatalf("a stored candle was changed to %s", got)
	}
	if _, ok := db.stored[at("2026-09-21T11:00:00Z")]; ok {
		t.Fatal("an incomplete candle was stored")
	}

	// Later, the 11:00 candle is complete, and is stored with the clock's time and the instrument.
	now = at("2026-09-21T12:01:00Z")
	c, err = Step(context.Background(), fetch, db, clock, 7, "BTC-USD", time.Hour, w)
	if err != nil || c.Inserted != 1 {
		t.Fatalf("second step: %v %v", c, err)
	}
	if s := db.stored[at("2026-09-21T11:00:00Z")]; s.InstrumentID != 7 || s.GranularityS != 3600 || !s.FetchedAt.Equal(now) {
		t.Fatalf("stored %+v", s)
	}
}

func TestStepFailuresAreCounted(t *testing.T) {
	clock := func() time.Time { return at("2026-09-21T12:00:30Z") }
	boom := errors.New("boom")
	bad := func(context.Context, string, int, time.Time, time.Time) ([]coinbase.Bar, error) { return nil, boom }
	w := Window{First: at("2026-09-21T09:00:00Z"), Last: at("2026-09-21T09:00:00Z")}
	c, err := Step(context.Background(), bad, &fakeDB{stored: map[time.Time]store.Candle{}}, clock, 1, "ETH-USD", time.Hour, w)
	if !errors.Is(err, boom) || c.Failed != 1 || c.Requests != 0 {
		t.Fatalf("fetch failure: %v %v", c, err)
	}
	good := func(context.Context, string, int, time.Time, time.Time) ([]coinbase.Bar, error) {
		return []coinbase.Bar{bar("2026-09-21T09:00:00Z", "1")}, nil
	}
	c, err = Step(context.Background(), good, &fakeDB{fail: boom}, clock, 1, "ETH-USD", time.Hour, w)
	if !errors.Is(err, boom) || c.Failed != 1 || c.Inserted != 0 {
		t.Fatalf("insert failure: %v %v", c, err)
	}
}

// The service loads the history itself: with nothing stored it starts at HistoryFrom, and with
// something stored just before the newest candle. Oldest window first, contiguous, none beyond
// the newest complete candle, so a round cut short resumes where it stopped.
func TestCatchup(t *testing.T) {
	now := time.Date(2026, 9, 21, 22, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		latest  time.Time
		stored  bool
		g       time.Duration
		first   time.Time
		windows int
	}{
		{"hourly, nothing stored", time.Time{}, false, time.Hour, HistoryFrom, 88},
		{"daily, nothing stored", time.Time{}, false, 24 * time.Hour, HistoryFrom, 4},
		{"hourly, up to date", now.Truncate(time.Hour).Add(-2 * time.Hour), true, time.Hour, now.Truncate(time.Hour).Add(-4 * time.Hour), 1},
	} {
		ws := Catchup(HistoryFrom, c.latest, c.stored, c.g, now)
		if len(ws) != c.windows || !ws[0].First.Equal(c.first) {
			t.Errorf("%s: %d windows from %v; want %d from %v", c.name, len(ws), ws[0].First, c.windows, c.first)
			continue
		}
		for i := 1; i < len(ws); i++ {
			if !ws[i].First.Equal(ws[i-1].Last.Add(c.g)) {
				t.Errorf("%s: window %d starts %v, after %v", c.name, i, ws[i].First, ws[i-1].Last)
			}
		}
		if last := ws[len(ws)-1].Last; !last.Equal(LastComplete(c.g, now)) {
			t.Errorf("%s: ends %v, want %v", c.name, last, LastComplete(c.g, now))
		}
	}
}
