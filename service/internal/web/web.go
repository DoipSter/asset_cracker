// Package web serves the read-only status page: one embedded HTML file and the JSON it polls.
//
// Everything here reads. There is no route that changes anything, and the server it is mounted
// on listens on localhost only; it is viewed from another machine through an SSH tunnel.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

//go:embed index.html
var page []byte

// Live is what the running service knows without asking the database.
type Live func() map[string]any

// Routes mounts the page and its API on mux.
func Routes(mux *http.ServeMux, db *store.Store, live Live) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		doc := live()
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
