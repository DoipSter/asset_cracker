// Package web serves the read-only pages: the home page, the phone widget, and the JSON they poll.
//
// Everything here reads. There is no route that changes anything, and the server it is mounted
// on listens on localhost only; it is viewed from another machine through an SSH tunnel.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// ranges are the chart spans the Python app offers: candle size in seconds, and how many.
var ranges = map[string][2]int{"1H": {60, 60}, "24H": {300, 288}, "7D": {3600, 168}}

type history struct {
	Closes    []float64 `json:"closes"`
	Times     []float64 // when each close was struck: the candle's start plus its length, unix seconds
	High, Low float64
	fetched   time.Time
}

var (
	historyMu    sync.Mutex
	historyCache = map[string]history{}
)

// priceHistory fetches a product's candles for a range, at most once a minute per range.
func priceHistory(ctx context.Context, userAgent, product, key string) (history, error) {
	spec, ok := ranges[key]
	if !ok {
		return history{}, fmt.Errorf("unknown range %q", key)
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	if h, ok := historyCache[product+key]; ok && time.Since(h.fetched) < time.Minute {
		return h, nil
	}
	candles, err := coinbase.Candles(ctx, userAgent, product, spec[0], time.Time{}, time.Time{})
	if err != nil {
		return history{}, err
	}
	if len(candles) > spec[1] {
		candles = candles[len(candles)-spec[1]:]
	}
	h := history{fetched: time.Now()}
	for i, c := range candles {
		h.Closes = append(h.Closes, c.Close)
		h.Times = append(h.Times, float64(c.Start.Unix()+int64(spec[0])))
		if i == 0 || c.High > h.High {
			h.High = c.High
		}
		if i == 0 || c.Low < h.Low {
			h.Low = c.Low
		}
	}
	historyCache[product+key] = h
	return h, nil
}

//go:embed home.html
var homePage []byte

//go:embed index.html
var widgetPage []byte // the phone widget, which was the whole site before there was a home page

// Live is what the running service knows without asking the database.
type Live func() map[string]any

// Routes mounts the pages and their API on mux: the home page at /, the phone widget at /widget.
// The widget asks for /api/... by absolute path, so it works from either address.
func Routes(mux *http.ServeMux, db *store.Store, userAgent string, live Live, src Sources) {
	serve := func(path string, body []byte) {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(body)
		})
	}
	serve("/{$}", homePage)
	serve("/widget", widgetPage)
	homeRoutes(mux, db, userAgent, src)
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		doc := live()
		if r.URL.Query().Get("full") == "" { // the page polls this every second: keep it off the database
			writeJSON(w, doc)
			return
		}
		if rounds, err := db.RecentRounds(ctx, 16); err == nil {
			doc["recent_rounds"] = rounds
		}
		if versions, err := db.StrategyVersions(ctx); err == nil {
			doc["strategies"] = versions
		}
		if n, err := db.LedgerEntryCount(ctx); err == nil {
			doc["ledger_entries"] = n
		}
		writeJSON(w, doc)
	})
	mux.HandleFunc("GET /api/minute", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		points, err := db.LastMinute(ctx, r.URL.Query().Get("product"))
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, points)
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		h, err := priceHistory(ctx, userAgent, r.URL.Query().Get("product"), r.URL.Query().Get("range"))
		if err != nil {
			http.Error(w, "history unavailable", http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{"closes": h.Closes, "high": h.High, "low": h.Low})
	})
	mux.HandleFunc("GET /api/round", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		points, err := db.RoundSeries(ctx, r.URL.Query().Get("ticker"), 5*time.Second)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, points)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
