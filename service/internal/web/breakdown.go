package web

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/doipster/asset_cracker/service/internal/analysis"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// GET /api/analysis/breakdown?version=<strategy_version_id>: one version's results cut every way
// the Evidence page draws them (analysis.Break), read from the record in a READ ONLY transaction.
// It is one read of that version's bets, made when asked and kept a minute per version, so a page
// left open costs one read a minute for the version it shows and nothing for the rest.

// breakdownDoc is the body: the breakdown, and what it is of.
type breakdownDoc struct {
	Simulated  bool    `json:"simulated"`
	Version    int64   `json:"strategy_version_id"`
	Family     string  `json:"family"`
	Roster     bool    `json:"roster"`
	ComputedAt float64 `json:"computed_at"`
	analysis.Breakdown
	// Dancer is a roster's owner election measured on its members' recorded shadows; absent for a
	// version that is not a roster, and for a roster with no shadow scored yet.
	Dancer *analysis.Dancer `json:"dancer,omitempty"`
}

type breakdowns struct {
	mu sync.Mutex
	by map[int64]breakdownDoc
}

const breakdownEvery = time.Minute

func breakdownRoute(mux *http.ServeMux, db *store.Store) {
	c := &breakdowns{by: map[int64]breakdownDoc{}}
	mux.HandleFunc("GET /api/analysis/breakdown", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "version must be a strategy version's id", http.StatusBadRequest)
			return
		}
		if db == nil {
			http.Error(w, "no database behind this service", http.StatusServiceUnavailable)
			return
		}
		doc, found, err := c.get(db, id)
		switch {
		case err != nil:
			http.Error(w, "the breakdown could not be read", http.StatusServiceUnavailable)
		case !found:
			http.Error(w, "no such strategy version", http.StatusNotFound)
		default:
			writeJSON(w, doc)
		}
	})
}

// get answers from the cache when it is under a minute old. The read runs under its own deadline,
// not the request's, as the home page's windows do: a viewer hanging up must not fail it.
func (c *breakdowns) get(db *store.Store, id int64) (breakdownDoc, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.by[id]; ok && time.Since(time.Unix(int64(d.ComputedAt), 0)) < breakdownEvery {
		return d, true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	info, found, rows, err := db.VersionBets(ctx, id)
	if err != nil {
		slog.Warn("analysis: breakdown read failed", "version", id, "err", err)
		return breakdownDoc{}, false, err
	}
	if !found {
		return breakdownDoc{}, false, nil
	}
	d := breakdownDoc{Simulated: true, Version: id, Family: info.Family, Roster: info.Roster, ComputedAt: unixf(time.Now()),
		Breakdown: analysis.Break(betsOf(rows), info.Family == store.FamilyLadders)}
	if info.Roster {
		shadows, err := db.VersionShadows(ctx, id)
		if err != nil {
			slog.Warn("analysis: roster shadows read failed", "version", id, "err", err)
			return breakdownDoc{}, false, err
		}
		d.Dancer = analysis.Dance(shadowsOf(shadows))
	}
	c.by[id] = d
	return d, true, nil
}

// shadowsOf turns the record's shadows into the analysis's.
func shadowsOf(rows []store.ShadowRow) []analysis.Shadow {
	out := make([]analysis.Shadow, len(rows))
	for i, r := range rows {
		seen := 0.0
		if !r.Seen.IsZero() {
			seen = unixf(r.Seen)
		}
		out[i] = analysis.Shadow{Member: r.Member, Close: r.Close.Unix(), Seen: seen, Owner: r.Owner, OwnerHow: r.OwnerHow, Cost: r.Cost, PnL: r.PnL}
	}
	return out
}

// betsOf turns the record's rows into the analysis's bets.
func betsOf(rows []store.VersionBet) []analysis.Bet {
	out := make([]analysis.Bet, len(rows))
	for i, r := range rows {
		out[i] = analysis.Bet{Close: r.Close.Unix(), Coin: r.Coin, Tau: r.Close.Sub(r.Placed).Seconds(), Member: r.Member,
			Qty: r.Qty, CostCents: r.CostCents, FeeCents: r.FeeCents, PnLCents: r.PnLCents, Won: r.Won,
			SideMid: r.SideMid, QuotedSpread: r.QuotedSpread}
	}
	return out
}
