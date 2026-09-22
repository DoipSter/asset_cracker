package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/catalogue"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// The assets dialog on the home page: search the catalogue of what the venues list (migration
// 0018), and switch recording of an item on or off. It writes one thing, the Record switch, which
// adds, activates or deactivates an instrument: no strategy or engine ever sees a switched-on
// one; a spot product is listed on the home page with its live price. Everything recorded stays
// when it is switched off. GET /assets, the page this once was, sends the reader to the dialog.

// catalogueStore is the database half of the assets page. *store.Store satisfies it.
type catalogueStore interface {
	SearchCatalogue(ctx context.Context, q, source, frequency string, limit int) ([]store.CatalogueHit, error)
	CatalogueSummary(ctx context.Context) (store.CatalogueSummary, error)
	RecordedInstruments(ctx context.Context) ([]store.Recorded, error)
	SetAssetSelection(ctx context.Context, c store.AssetChange, decide func(store.AssetState) (store.AssetPlan, error)) (store.AssetResult, error)
}

var _ catalogueStore = (*store.Store)(nil)

// RecorderState is how one selected instrument's recorder is doing in this process.
type RecorderState struct {
	Recorder     string     `json:"recorder"` // round, ladder or candles
	Since        time.Time  `json:"since"`
	LastError    string     `json:"last_error,omitempty"`
	LastQuotesAt *time.Time `json:"last_quotes_at,omitempty"` // round pollers only
}

// Catalogue is how the assets page reaches the supervisor (app/assets.go). A nil func is a
// process without one: a switch is saved and applies at the next start.
type Catalogue struct {
	Max     int                            // the most selected at once
	Running func() map[int64]RecorderState // the selected instruments being recorded now, by id
	Changed func()                         // re-read the selection now, not at the next minute
}

var catalogueFrequencies = map[string]bool{
	"": true, "fifteen_min": true, "hourly": true, "daily": true, "weekly": true, "monthly": true,
	"annual": true, "one_off": true, "custom": true, "continuous": true,
}

// CatalogueRoutes mounts the assets page and its API on mux.
func CatalogueRoutes(mux *http.ServeMux, db catalogueStore, c Catalogue) {
	mux.HandleFunc("GET /assets", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/#assets", http.StatusFound) // the dialog on the home page
	})

	mux.HandleFunc("GET /api/catalogue", func(w http.ResponseWriter, r *http.Request) {
		qs := r.URL.Query()
		q, source, frequency := strings.TrimSpace(qs.Get("q")), qs.Get("source"), qs.Get("frequency")
		if len(q) > 64 {
			catalogueErr(w, http.StatusBadRequest, "The search is longer than 64 characters.")
			return
		}
		if source != "" && source != catalogue.Kalshi && source != catalogue.Coinbase {
			catalogueErr(w, http.StatusBadRequest, "source must be kalshi or coinbase")
			return
		}
		if !catalogueFrequencies[frequency] {
			catalogueErr(w, http.StatusBadRequest, "That frequency is not one the catalogue uses.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		hits, err := db.SearchCatalogue(ctx, q, source, frequency, catalogue.SearchLimit)
		if err != nil {
			slog.Error("catalogue: search", "err", err)
			catalogueErr(w, http.StatusInternalServerError, "The catalogue could not be searched.")
			return
		}
		summary, err := db.CatalogueSummary(ctx)
		if err != nil {
			slog.Error("catalogue: summary", "err", err)
			catalogueErr(w, http.StatusInternalServerError, "The catalogue could not be read.")
			return
		}
		recorded, err := db.RecordedInstruments(ctx)
		if err != nil {
			slog.Error("catalogue: recorded instruments", "err", err)
			catalogueErr(w, http.StatusInternalServerError, "The recorded instruments could not be read.")
			return
		}
		writeJSON(w, catalogueDoc(hits, summary, recorded, c))
	})

	mux.HandleFunc("POST /api/controls/asset", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Source string `json:"source"`
			Code   string `json:"code"`
			Record *bool  `json:"record"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			catalogueErr(w, http.StatusBadRequest, "The request could not be read.")
			return
		}
		body.Code = strings.TrimSpace(body.Code)
		switch {
		case body.Source != catalogue.Kalshi && body.Source != catalogue.Coinbase:
			catalogueErr(w, http.StatusBadRequest, "source must be kalshi or coinbase")
			return
		case body.Code == "" || len(body.Code) > 64:
			catalogueErr(w, http.StatusBadRequest, "code must be a catalogue code")
			return
		case body.Record == nil:
			catalogueErr(w, http.StatusBadRequest, "record must be true or false")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		res, err := db.SetAssetSelection(ctx, store.AssetChange{Source: body.Source, Code: body.Code, Record: *body.Record,
			Via: "assets page, POST /api/controls/asset"}, catalogue.Rules(c.Max))
		var refused catalogue.Refused
		switch {
		case errors.As(err, &refused):
			catalogueErr(w, http.StatusConflict, refused.Why)
			return
		case errors.Is(err, store.ErrNotCatalogued):
			catalogueErr(w, http.StatusNotFound, body.Code+" is not in the catalogue.")
			return
		case err != nil:
			slog.Error("catalogue: switch", "source", body.Source, "code", body.Code, "err", err)
			catalogueErr(w, http.StatusInternalServerError, "The switch was not saved.")
			return
		}
		// When it takes effect. A switched-on row is the supervisor's: now, if this process has
		// one. A seeded row's recorders were started with the process: a spot product's price
		// feed and its place on the home page follow the table within a minute ("soon"); a
		// series' poller runs until the next start.
		effective := "unchanged"
		if res.Changed {
			switch {
			case res.Seeded && res.Kind == "spot":
				effective = "soon"
			case res.Seeded:
				effective = "next-start"
			case c.Changed != nil:
				c.Changed()
				effective = "now"
			default:
				effective = "next-start"
			}
			slog.Info("assets page: recording switched", "source", body.Source, "code", body.Code, "record", res.Record, "seeded", res.Seeded, "selected", res.Selected, "effective", effective)
		}
		writeJSON(w, map[string]any{"instrument_id": res.InstrumentID, "record": res.Record, "changed": res.Changed,
			"selected": res.Selected, "max": c.Max, "effective": effective, "seeded": res.Seeded})
	})
}

type catalogueRecorded struct {
	store.Recorded
	Running *RecorderState `json:"running,omitempty"` // a selected one this process is recording now
}

func catalogueDoc(hits []store.CatalogueHit, summary store.CatalogueSummary, recorded []store.Recorded, c Catalogue) map[string]any {
	running := map[int64]RecorderState{}
	if c.Running != nil {
		running = c.Running()
	}
	list := make([]catalogueRecorded, 0, len(recorded))
	selected := 0
	for _, r := range recorded {
		out := catalogueRecorded{Recorded: r}
		if r.Selected {
			selected++
			if st, ok := running[r.ID]; ok {
				out.Running = &st
			}
		}
		list = append(list, out)
	}
	return map[string]any{
		"results": hits, "limit": catalogue.SearchLimit, "catalogue": summary,
		"recording": list, "selected": selected, "max": c.Max,
		"supervised": c.Running != nil,
		"note": "Recording only: no strategy or engine uses a switched-on instrument. A Coinbase product is listed on the home page " +
			"with its live price and gets daily, hourly and minute candles; its prints are not recorded. Every row is yours to switch " +
			"except what the live engine trades or prices from, which says so. Switching off keeps everything recorded; a seeded " +
			"series stops at the next start of the service, a seeded product within a minute.",
	}
}

func catalogueErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
