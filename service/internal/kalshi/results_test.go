package kalshi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// listedSink is the poller's fake sink with the record's unsettled list set by the test.
type listedSink struct {
	*fakeSink
	listed map[string]Pending
}

func (s *listedSink) Unsettled(context.Context, time.Time) (map[string]Pending, error) {
	out := map[string]Pending{}
	for t, p := range s.listed {
		out[t] = p
	}
	return out, nil
}

// SaveResult stores the result, and the record stops listing the round, as the real one does.
func (s *listedSink) SaveResult(ctx context.Context, id int64, m MarketInfo, closes time.Time) (bool, error) {
	first, err := s.fakeSink.SaveResult(ctx, id, m, closes)
	delete(s.listed, s.ids[id])
	return first, err
}

// resultServer answers one market's result: `result(now)` decides it, and asks are counted.
func resultServer(t *testing.T, ticker string, result func() string, asks *int64) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ticker) {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(asks, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"market": map[string]any{"ticker": ticker, "result": result(), "expiration_value": "84000.00"}})
	}))
	t.Cleanup(srv.Close)
	c := NewClient("test")
	c.base = srv.URL
	return c
}

// A traded round whose result Kalshi publishes twenty minutes late is still settled: the watch
// used to give up after fifteen, and the bets in it stayed open until a restart asked again.
func TestALateResultOfATradedRoundIsStillSettled(t *testing.T) {
	ticker := "KXBTC15M-26SEP250100-00"
	closes := time.Date(2026, 9, 25, 1, 0, 0, 0, time.UTC)
	now := closes
	var asks int64
	client := resultServer(t, ticker, func() string {
		if now.Before(closes.Add(20 * time.Minute)) {
			return ""
		}
		return "yes"
	}, &asks)
	sink := &listedSink{fakeSink: &fakeSink{results: map[string]int{}, ids: map[int64]string{7: ticker}},
		listed: map[string]Pending{ticker: {ID: 7, Closes: closes}}} // traded: listed however old
	p := &Poller{Client: client, Sink: sink, Series: "KXBTC15M"}
	waiting := map[string]*awaiting{ticker: {id: 7, closes: closes}}
	for now = closes.Add(time.Second); now.Before(closes.Add(25 * time.Minute)); now = now.Add(time.Second) {
		if now.Second() == 0 {
			if err := p.resweep(context.Background(), now, waiting); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.askResults(context.Background(), now, waiting); err != nil {
			t.Fatal(err)
		}
	}
	if sink.results[ticker] != 1 || len(waiting) != 0 {
		t.Fatalf("settled %d times, still waiting on %d", sink.results[ticker], len(waiting))
	}
	// The pace: every second for a minute, every ten seconds until the result is late, then once
	// a minute: about 60 + 84 + 5, not one ask a second for twenty minutes.
	if asks > 200 {
		t.Errorf("%d asks in twenty minutes", asks)
	}
}

// A round nobody traded is let go an hour after its close, when the record stops listing it; a
// round closed within the hour is kept whether listed or not.
func TestAnUntradedRoundIsLetGoAfterItsHour(t *testing.T) {
	closes := time.Date(2026, 9, 25, 1, 0, 0, 0, time.UTC)
	sink := &listedSink{fakeSink: &fakeSink{results: map[string]int{}, ids: map[int64]string{}}, listed: map[string]Pending{}}
	p := &Poller{Sink: sink, Series: "KXBTC15M"}
	waiting := map[string]*awaiting{"UNTRADED": {id: 1, closes: closes}}
	if err := p.resweep(context.Background(), closes.Add(30*time.Minute), waiting); err != nil || len(waiting) != 1 {
		t.Fatalf("dropped within its hour: %v %d", err, len(waiting))
	}
	if err := p.resweep(context.Background(), closes.Add(61*time.Minute), waiting); err != nil || len(waiting) != 0 {
		t.Fatalf("kept past its hour: %v %d", err, len(waiting))
	}
	// And the record's list is followed: a round an earlier run left is picked up.
	sink.listed = map[string]Pending{"EARLIER": {ID: 2, Closes: closes.Add(-3 * time.Hour)}}
	if err := p.resweep(context.Background(), closes.Add(62*time.Minute), waiting); err != nil || waiting["EARLIER"] == nil {
		t.Fatalf("the listed round was not taken up: %v %v", err, waiting)
	}
}

// A result that is neither yes nor no settles nothing, is counted once, and is asked about again.
func TestAScalarResultIsReportedNotSettled(t *testing.T) {
	ticker := "KXBTC15M-26SEP250115-15"
	closes := time.Date(2026, 9, 25, 1, 15, 0, 0, time.UTC)
	var asks int64
	client := resultServer(t, ticker, func() string { return "scalar" }, &asks)
	sink := &listedSink{fakeSink: &fakeSink{results: map[string]int{}, ids: map[int64]string{3: ticker}}}
	p := &Poller{Client: client, Sink: sink, Series: "KXBTC15M"}
	waiting := map[string]*awaiting{ticker: {id: 3, closes: closes}}
	for now := closes.Add(time.Second); now.Before(closes.Add(3 * time.Minute)); now = now.Add(time.Second) {
		if err := p.askResults(context.Background(), now, waiting); err != nil {
			t.Fatal(err)
		}
	}
	if sink.results[ticker] != 0 || waiting[ticker] == nil || waiting[ticker].other != "scalar" || asks < 60 {
		t.Fatalf("results %d, waiting %+v, asks %d", sink.results[ticker], waiting[ticker], asks)
	}
}

// listedLadder is the ladder recorder's fake sink with the record's unsettled list set by the test.
type listedLadder struct {
	*ladderFake
	listed map[string]Pending
}

func (f *listedLadder) Unsettled(context.Context, time.Time, time.Time) (map[string]Pending, error) {
	return f.listed, nil
}

// The ladder watch follows the record's list: a traded leg listed however old is kept (it used
// to be given up after 72 hours, and a restart did not ask again either); an unlisted leg past
// 72 hours is dropped; an unlisted recent one is kept.
func TestTheLadderWatchFollowsTheRecord(t *testing.T) {
	now := time.Date(2026, 9, 30, 21, 0, 0, 0, time.UTC)
	old := now.Add(-100 * time.Hour)
	sink := &listedLadder{ladderFake: &ladderFake{}, listed: map[string]Pending{"TRADED-OLD": {ID: 1, Closes: old}}}
	r := &LadderRecorder{Series: "KXBTCD", Sink: sink, Now: func() time.Time { return now }}
	r.waiting = map[string]*awaiting{
		"UNTRADED-OLD":    {id: 2, closes: old},
		"UNTRADED-RECENT": {id: 3, closes: now.Add(-time.Hour)},
	}
	if err := r.loadUnsettled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.waiting["TRADED-OLD"] == nil || r.waiting["UNTRADED-OLD"] != nil || r.waiting["UNTRADED-RECENT"] == nil || !r.merged.Equal(now) {
		t.Fatalf("waiting %v, merged %v", r.waiting, r.merged)
	}
}

// A leg a bucket traded is asked about before untraded ones, however new: at a 5 pm close hundreds
// of legs are due at once and one pass asks about twenty, oldest first, so a traded leg could wait
// behind all of them while its bets stayed open and no value snapshot was written.
func TestTradedLegsAreAskedFirst(t *testing.T) {
	close := time.Date(2026, 9, 25, 21, 0, 0, 0, time.UTC)
	waiting := map[string]*awaiting{}
	for i := 0; i < 300; i++ {
		waiting[fmt.Sprintf("LEG-%03d", i)] = &awaiting{id: int64(i), closes: close.Add(-time.Duration(300-i) * time.Minute)} // LEG-299 is the newest
	}
	fake := &ladderFake{traded: map[int64]bool{299: true, 150: true}}
	r := &LadderRecorder{Series: "KXBTCD", Sink: fake, waiting: waiting}
	now := close.Add(time.Minute)
	got := dueResults(waiting, now, ladderResultAsks, r.tradedDue(context.Background(), now))
	if len(got) != ladderResultAsks || got[0] != "LEG-150" || got[1] != "LEG-299" || got[2] != "LEG-000" || fake.asked != 1 {
		t.Fatalf("asked first: %v (Traded asked %d times)", got[:3], fake.asked)
	}
	// Twenty due or fewer: every one is asked this pass, and the record is not asked which were traded.
	few := map[string]*awaiting{"A": {id: 1, closes: close}, "B": {id: 2, closes: close}}
	r.waiting = few
	if tr := r.tradedDue(context.Background(), now); tr != nil || fake.asked != 1 {
		t.Fatalf("asked with only %d due: %v", len(few), tr)
	}
}
