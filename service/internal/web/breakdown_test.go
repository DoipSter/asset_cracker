package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

func TestBreakdownRoute(t *testing.T) {
	mux := http.NewServeMux()
	breakdownRoute(mux, nil)
	for path, want := range map[string]int{
		"/api/analysis/breakdown":            400,
		"/api/analysis/breakdown?version=0":  400,
		"/api/analysis/breakdown?version=x1": 400,
		"/api/analysis/breakdown?version=25": 503, // no database behind this one
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s answered %d, want %d", path, rec.Code, want)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/analysis/breakdown?version=25", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("a POST answered %d: the route only reads", rec.Code)
	}
}

// The record's rows become the analysis's bets: the window is the close, tau the seconds from
// the first buy to it.
func TestBetsOf(t *testing.T) {
	closes := time.Unix(1_790_000_900, 0)
	mid := 0.7
	got := betsOf([]store.VersionBet{{Close: closes, Placed: closes.Add(-412 * time.Second), Coin: "ETH", Member: "Favourite",
		Qty: 5, CostCents: 350, FeeCents: 8, PnLCents: 142, Won: true, SideMid: &mid}})
	if len(got) != 1 || got[0].Close != 1_790_000_900 || got[0].Tau != 412 || got[0].Member != "Favourite" || *got[0].SideMid != 0.7 || got[0].QuotedSpread != nil {
		t.Errorf("%+v", got)
	}
}
