package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// resetBudget is how long the reset request may take in the database. The page waits as long.
const resetBudget = 5 * time.Minute

// operatorHeader carries the passphrase for anything that changes the books or the recording.
const operatorHeader = "X-Operator-Key"

// operator wraps a handler that changes something: with a key set, the request must carry it in
// operatorHeader, compared in constant time; without one it refuses with 401 and the page asks
// the reader for the key. An empty key means no passphrase (localhost, a tailnet). Reads are never
// wrapped: the figures are simulated money and the page is meant to be looked at.
func operator(key string, h http.HandlerFunc) http.HandlerFunc {
	if key == "" {
		return h
	}
	want := []byte(key)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.TrimSpace(r.Header.Get(operatorHeader)))
		if len(got) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			writeErr(w, http.StatusUnauthorized, "The operator key is missing or wrong.")
			return
		}
		h(w, r)
	}
}

// reloadBudget is the engine's start-up budget: the seeding, the reads and the rebuild.
const reloadBudget = 30 * time.Second

// Control is how the buckets page reaches the running engine. A nil func means there is
// no engine in this process: the saved switch still applies at the next start.
type Control struct {
	Key     string                                  // AC_OPERATOR_KEY: the passphrase every POST here needs; empty means none
	Version string                                  // the build stamp, recorded as code_ref on a version the builder registers
	EnvOn   bool                                    // AC_V3, used when the database has no orders row
	Status  func() (placing bool, effective string) // this process, right now
	Apply   func(on bool) string                    // "now" or "next-start"
	Hold    func()                                  // stop writes before a reset
	Release func()                                  // drop the deleted books from memory
	Abort   func()                                  // the reset did not happen; rebuild
	// Reload is a start without a process start: seed the approved versions, read what is
	// held, rebuild. It runs after a reset and after every approval or retirement made here.
	// held is how many buckets the engine now holds, ordering how many of them may order.
	Reload func(ctx context.Context) (held, ordering int, err error)
	// Build turns the builder's shape (engine.Shape as JSON) into a version the engine accepts,
	// and Presets lists the standard shapes (engine.Presets as JSON). Both are the engine's;
	// this package does not import it, so app hands them in. Nil means the page has no builder.
	Build   func(shape []byte) (Built, error)
	Presets func() []byte
}

// Built is a version the engine has built and validated from a shape.
type Built struct {
	Name, Blurb string
	Params      []byte // engine.Params as JSON
	Parent      string // "Scalper" or "Value": whose version 2 it descends from
	Control     bool   // a negative control, registered to be caught
}

// BuildRefused is a shape the engine would not build; its text is shown on the page.
type BuildRefused struct{ Why string }

func (e BuildRefused) Error() string { return e.Why }

// reload runs the engine's reload and reports it for the page. With no engine in this process
// there is nothing to reload: the next start reads the database.
func (c Control) reload(ctx context.Context) map[string]any {
	if c.Reload == nil {
		return map[string]any{"reloaded": false, "effective": "next-start"}
	}
	held, ordering, err := c.Reload(ctx)
	if err != nil {
		slog.Error("controls: reload", "err", err)
		return map[string]any{"reloaded": false, "effective": "heal", "held": held, "ordering": ordering}
	}
	return map[string]any{"reloaded": true, "effective": "now", "held": held, "ordering": ordering}
}

func (c Control) status() (placing bool, effective string) {
	if c.Status == nil {
		return false, "next-start"
	}
	return c.Status()
}

func (c Control) apply(on bool) string {
	if c.Apply == nil {
		return "next-start"
	}
	return c.Apply(on)
}

func (c Control) hold() {
	if c.Hold != nil {
		c.Hold()
	}
}

func (c Control) release() {
	if c.Release != nil {
		c.Release()
	}
}

func (c Control) abort() {
	if c.Abort != nil {
		c.Abort()
	}
}

// controlStore is the database half of the buckets-page controls. *store.Store satisfies it.
type controlStore interface {
	OrdersSetting(ctx context.Context) (on bool, set bool, err error)
	SetOrdersSetting(ctx context.Context, on bool) error
	ListVersion3(ctx context.Context) ([]store.Version3, error)
	SetVersionStatus(ctx context.Context, id int64, status, reason string) error
	CreateVersion3(ctx context.Context, v store.NewVersion3) (int64, error)
	SetSimPolicy(ctx context.Context, winnings, replenish, tax, fees int, note string) error
	CurrentSkimPolicy(ctx context.Context) (store.SkimPolicy, error)
	ResetSim(ctx context.Context) (store.ResetCounts, error)
}

var _ controlStore = (*store.Store)(nil)

func controlRoutes(mux *http.ServeMux, db controlStore, list *bucketList, ctl Control) {
	mux.HandleFunc("GET /api/controls", func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeErr(w, http.StatusServiceUnavailable, "The controls have no database.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		on, set, err := db.OrdersSetting(ctx)
		if err != nil {
			slog.Error("controls: orders switch", "err", err)
			writeErr(w, http.StatusInternalServerError, "The orders switch could not be read.")
			return
		}
		source := "environment"
		if !set {
			on = ctl.EnvOn
		} else {
			source = "database"
		}
		versions, err := db.ListVersion3(ctx)
		if err != nil {
			slog.Error("controls: versions", "err", err)
			writeErr(w, http.StatusInternalServerError, "The version-3 strategies could not be read.")
			return
		}
		policy, err := db.CurrentSkimPolicy(ctx)
		if err != nil {
			slog.Error("controls: policy", "err", err)
			writeErr(w, http.StatusInternalServerError, "The allocation rule could not be read.")
			return
		}
		placing, effective := ctl.status()
		writeJSON(w, map[string]any{
			"simulated": true,
			"locked":    ctl.Key != "", // the page asks for the operator key before its first change
			"orders":    map[string]any{"on": on, "source": source, "placing": placing, "effective": effective},
			"versions":  versions,
			"policy": map[string]any{
				"id": policy.ID, "since_at": policy.EffectiveAt.UTC().Format(time.RFC3339), "note": policy.Note,
				"winnings_bps": policy.Winnings, "replenish_bps": policy.Replenish, "tax_bps": policy.Tax, "fees_bps": policy.Fees,
			},
		})
	})

	mux.HandleFunc("POST /api/controls/orders", operator(ctl.Key, func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeErr(w, http.StatusServiceUnavailable, "The controls have no database.")
			return
		}
		var body struct {
			On bool `json:"on"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		if err := db.SetOrdersSetting(ctx, body.On); err != nil {
			slog.Error("controls: set orders", "err", err)
			writeErr(w, http.StatusInternalServerError, "The orders switch was not saved.")
			return
		}
		writeJSON(w, map[string]any{"on": body.On, "effective": ctl.apply(body.On)})
	}))

	mux.HandleFunc("POST /api/controls/version", operator(ctl.Key, func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeErr(w, http.StatusServiceUnavailable, "The controls have no database.")
			return
		}
		var body struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		switch body.Status {
		case "probation", "active", "retired":
		default:
			writeErr(w, http.StatusBadRequest, "status must be probation, active, or retired")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		if err := db.SetVersionStatus(ctx, body.ID, body.Status, body.Reason); err != nil {
			if errors.Is(err, store.ErrVersionNotFound) {
				writeErr(w, http.StatusNotFound, "That version-3 strategy is not registered.")
				return
			}
			if strings.Contains(err.Error(), "status must be") || strings.Contains(err.Error(), "too long") {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			slog.Error("controls: version status", "err", err)
			writeErr(w, http.StatusInternalServerError, "The version was not updated.")
			return
		}
		// The status is saved. Now the engine loads it: an approval seeds its bucket and it
		// trades on the next look; a retirement leaves its bucket held and settle-only.
		rctx, rcancel := context.WithTimeout(r.Context(), reloadBudget)
		defer rcancel()
		out := ctl.reload(rctx)
		out["id"], out["status"] = body.ID, body.Status
		if list != nil {
			list.drop()
		}
		writeJSON(w, out)
	}))

	mux.HandleFunc("GET /api/controls/presets", func(w http.ResponseWriter, r *http.Request) {
		if ctl.Presets == nil {
			writeErr(w, http.StatusServiceUnavailable, "This process has no builder.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(ctl.Presets())
	})

	// The builder: a shape in, a draft version-3 row out. The engine builds and validates the
	// params (every number labelled; nothing measured), the store registers the row, and the
	// Approve button that already exists seeds it. One more trial in the registry.
	mux.HandleFunc("POST /api/controls/version/new", operator(ctl.Key, func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeErr(w, http.StatusServiceUnavailable, "The controls have no database.")
			return
		}
		if ctl.Build == nil {
			writeErr(w, http.StatusServiceUnavailable, "This process has no builder.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		body, err := io.ReadAll(r.Body)
		if err != nil || !json.Valid(body) {
			writeErr(w, http.StatusBadRequest, "The request could not be read.")
			return
		}
		var hyp struct {
			Hypothesis string `json:"hypothesis"`
			Via        string `json:"via"` // who sent it, when not the page: "mcp"; recorded in code_ref
		}
		_ = json.Unmarshal(body, &hyp)
		if strings.TrimSpace(hyp.Hypothesis) == "" {
			writeErr(w, http.StatusBadRequest, "Say what this version is meant to test: the hypothesis goes in the registry.")
			return
		}
		origin := "built on the buckets page"
		if via := strings.TrimSpace(hyp.Via); via != "" {
			if len(via) > 40 || strings.ContainsAny(via, "\n\r\t") {
				writeErr(w, http.StatusBadRequest, "via must be one short word, such as mcp.")
				return
			}
			origin = "proposed via " + via
		}
		built, err := ctl.Build(body)
		if err != nil {
			var refused BuildRefused
			if errors.As(err, &refused) {
				writeErr(w, http.StatusBadRequest, refused.Why)
				return
			}
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		id, err := db.CreateVersion3(ctx, store.NewVersion3{Name: built.Name, Blurb: built.Blurb, Hypothesis: hyp.Hypothesis,
			Params: built.Params, Parent: built.Parent, CodeRef: origin + "; release " + ctl.Version})
		if err != nil {
			if errors.Is(err, store.ErrVersionExists) {
				writeErr(w, http.StatusConflict, built.Name+" already has a version 3. Give this one a different name.")
				return
			}
			if strings.Contains(err.Error(), "must be") || strings.Contains(err.Error(), "too long") {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			slog.Error("controls: new version", "err", err)
			writeErr(w, http.StatusInternalServerError, "The version was not registered.")
			return
		}
		if list != nil {
			list.drop()
		}
		slog.Info("version registered as draft", "id", id, "name", built.Name, "control", built.Control, "origin", origin)
		writeJSON(w, map[string]any{"id": id, "name": built.Name, "status": "draft", "control": built.Control})
	}))

	mux.HandleFunc("POST /api/controls/policy", operator(ctl.Key, func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeErr(w, http.StatusServiceUnavailable, "The controls have no database.")
			return
		}
		var body struct {
			Winnings  int    `json:"winnings_bps"`
			Replenish int    `json:"replenish_bps"`
			Tax       int    `json:"tax_bps"`
			Fees      int    `json:"fees_bps"`
			Note      string `json:"note"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if err := store.RatesOK(body.Winnings, body.Replenish, body.Tax, body.Fees); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		if err := db.SetSimPolicy(ctx, body.Winnings, body.Replenish, body.Tax, body.Fees, body.Note); err != nil {
			if strings.Contains(err.Error(), "rate") || strings.Contains(err.Error(), "too long") {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			slog.Error("controls: policy", "err", err)
			writeErr(w, http.StatusInternalServerError, "The allocation rule was not saved.")
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}))

	mux.HandleFunc("POST /api/controls/reset", operator(ctl.Key, func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			writeErr(w, http.StatusServiceUnavailable, "The controls have no database.")
			return
		}
		var body struct {
			Confirm string `json:"confirm"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.Confirm) != "reset sim" {
			writeErr(w, http.StatusBadRequest, "Type reset sim to empty the simulated books.")
			return
		}
		ctl.hold()
		released := false
		defer func() {
			if !released {
				ctl.abort()
			}
		}()
		// The record held 1.76 million decisions when the first reset timed out at a minute.
		// The function now truncates the journal, but the budget is generous all the same.
		ctx, cancel := context.WithTimeout(r.Context(), resetBudget)
		defer cancel()
		started := time.Now()
		counts, err := db.ResetSim(ctx)
		if err != nil {
			slog.Error("controls: reset refused or failed", "err", err, "after", time.Since(started).Round(time.Millisecond))
			writeErr(w, http.StatusInternalServerError, "The sim books were not reset.")
			return
		}
		ctl.release()
		released = true
		if list != nil {
			list.drop()
		}
		slog.Info("sim books reset from the buckets page", "buckets", counts.Buckets, "orders", counts.Orders, "transfers", counts.Transfers,
			"db_ms", counts.TookMS, "took", time.Since(started).Round(time.Millisecond))
		// The books are empty. The start: every approved version is seeded and the engine is
		// rebuilt over the new buckets. With no approved version-3 row nothing is seeded, and
		// the page says so.
		rctx, rcancel := context.WithTimeout(r.Context(), reloadBudget)
		defer rcancel()
		out := map[string]any{"cleared": counts}
		for k, v := range ctl.reload(rctx) {
			out[k] = v
		}
		if list != nil {
			list.drop()
		}
		writeJSON(w, out)
	}))
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "The request could not be read.")
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
