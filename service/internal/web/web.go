// Package web serves the home page, the phone widget, and the JSON they poll.
//
// The pages read, and post a few controls, all for simulated money. The home page holds the
// bank: from an account's own row it moves money, schedules a payday, sets the allocation
// rates, and closes the bank for the next one (the reset). The buckets page deploys strategy
// accounts: a version, a seed, and where the seed is drawn from; it also retires a version,
// closes a bucket out, and switches new orders. The server listens on localhost only.
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
// ranges are the Coinbase candle sizes and counts behind each chart span. 15M is for a coin with
// no Kalshi series: its quarter hour is fifteen one-minute candles, where a series coin's is the
// open round from recorded quotes.
var ranges = map[string][2]int{"15M": {60, 15}, "1H": {60, 60}, "24H": {300, 288}, "7D": {3600, 168}}

type history struct {
	Closes    []float64 `json:"closes"`
	Times     []float64 // when each close was struck, unix seconds: the candle's end, or for the one still forming, when it was fetched
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
		h.Times = append(h.Times, closeTime(c.Start, spec[0], h.fetched))
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

// closeTime is when a candle's close was struck. Coinbase's newest candle is still forming (seen
// 2026-09-21: an hourly candle whose end was 1,478 s ahead of the clock): its close is the latest
// trade, so it is timed at the fetch, not at an end that has not happened yet and would put the
// chart's last point after "now" and after the bets drawn on it.
func closeTime(start time.Time, granularity int, fetched time.Time) float64 {
	end := start.Add(time.Duration(granularity) * time.Second)
	if end.After(fetched) {
		end = fetched
	}
	return float64(end.UnixNano()) / 1e9
}

//go:embed home.html
var homePage []byte

//go:embed index.html
var widgetPage []byte // the phone widget, which was the whole site before there was a home page

// Live is what the running service knows without asking the database.
type Live func() map[string]any

// Routes mounts the pages and their API on mux: the home page at /, the phone widget at /widget.
// The widget asks for /api/... by absolute path, so it works from either address.
func Routes(mux *http.ServeMux, db *store.Store, userAgent string, live Live, src Sources, ctl Control) {
	serve := func(path string, body []byte) {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(body)
		})
	}
	serve("/{$}", homePage)
	serve("/widget", widgetPage)
	homeRoutes(mux, db, userAgent, src, ctl)
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
