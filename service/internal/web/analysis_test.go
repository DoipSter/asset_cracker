package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/analysis"
)

// The warm loop refreshes at once and then on every tick, does not wait for anybody to ask, and
// ends with its context. A refresh that panics must not end it: an unrecovered panic in a
// goroutine takes the whole process down, and the traders run in that process.
func TestWarmKeepsRefreshingAndSurvivesAPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	ran, done := make(chan int32, 16), make(chan struct{})
	go func() {
		defer close(done)
		warm(ctx, time.Millisecond, func() {
			n := calls.Add(1)
			ran <- n
			if n == 2 {
				panic("a bug in the analysis")
			}
		})
	}()
	for want := int32(1); want <= 4; want++ {
		select {
		case got := <-ran:
			if got != want {
				t.Fatalf("refresh %d came as %d", want, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("refresh %d never ran: the loop stopped", want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop outlived its context")
	}
}

func TestWarmDoesNothingOnceCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	warm(ctx, time.Hour, func() { t.Error("refreshed after its context ended") })
}

// Windows never tried come first, newest first; windows holding a market that was tried and held
// back come after them, so a round whose settlement is never written cannot use up the budget
// every minute and keep the older history from ever being read.
func TestReadOrder(t *testing.T) {
	m := func(id, closes int64) analysis.Market { return analysis.Market{ID: id, Closes: closes} }
	pending := map[int64][]analysis.Market{
		900:  {m(1, 900), m(2, 900)},
		1800: {m(3, 1800)},
		2700: {m(4, 2700), m(5, 2700)}, // market 5 was held back last time
		3600: {m(6, 3600)},
		4500: {m(7, 4500)}, // and so was 7
	}
	got := readOrder(pending, map[int64]string{5: "bucket 22 held 106 yes at the close and its settlement rows cover 0", 7: "the same", 99: "not pending any more"})
	want := []int64{3600, 1800, 900, 4500, 2700}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if got := readOrder(nil, nil); len(got) != 0 {
		t.Fatalf("nothing pending: %v", got)
	}
}

// A decision is new when its last window is past the one stored for its version; a version with
// nothing stored is new at any window. The order is kept.
func TestNewDecisions(t *testing.T) {
	snaps := []analysis.Snapshot{{VersionID: 1, LastClose: 36000}, {VersionID: 2, LastClose: 36000}, {VersionID: 3, LastClose: 35100}}
	got := newDecisions(snaps, map[int64]int64{1: 36000, 3: 34200})
	if len(got) != 2 || got[0].VersionID != 2 || got[1].VersionID != 3 {
		t.Fatalf("got %+v", got)
	}
	if got := newDecisions(snaps, nil); len(got) != 3 {
		t.Fatalf("nothing stored: %+v", got)
	}
	if got := newDecisions(nil, map[int64]int64{1: 1}); len(got) != 0 {
		t.Fatalf("nothing decided: %+v", got)
	}
}

func TestVersionIDs(t *testing.T) {
	got := versionIDs([]analysis.Bucket{{ID: 1, VersionID: 12}, {ID: 2, VersionID: 3}, {ID: 3, VersionID: 12}, {ID: 4, VersionID: 7}})
	if len(got) != 3 || got[0] != 3 || got[1] != 7 || got[2] != 12 {
		t.Fatalf("got %v", got)
	}
	if got := versionIDs(nil); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// A request that finds a fresh document is served it without touching the database (db is nil
// here, so any read would panic), and a stale-marked one is passed on as it is.
func TestGetServesAFreshDocument(t *testing.T) {
	c := newAnalysisCache()
	doc := analysis.Build(analysis.Inputs{MarketsSettled: 5, Trials: 18})
	c.doc, c.at = &doc, time.Now()
	got, reason := c.get(nil)
	if got != &doc || reason != "" {
		t.Fatalf("got %p %q", got, reason)
	}
}

// Before anything has been computed a failed refresh still answers in JSON, in the document's
// own shape, with the reason: a bare "503" leaves the page's panel blank and hides the failure.
func TestFirstFailureIsJSONWithTheReason(t *testing.T) {
	c := newAnalysisCache()
	c.at, c.lastErr = time.Now(), "fills: relation \"trade_order\" does not exist" // a refresh has just failed
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/analysis", c.handle(nil))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/analysis", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var body struct {
		Simulated   bool   `json:"simulated"`
		Stale       bool   `json:"stale"`
		StaleReason string `json:"stale_reason"`
		Leaderboard struct {
			Rows []any `json:"rows"`
		} `json:"leaderboard"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	if !body.Simulated || !body.Stale || !strings.Contains(body.StaleReason, "trade_order") || body.Leaderboard.Rows == nil {
		t.Fatalf("got %s", rec.Body)
	}
}
