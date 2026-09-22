package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// resetBudget is how long the reset request may take in the database. The page waits as long.
const resetBudget = 5 * time.Minute

// reloadBudget is the engine's start-up budget: the seeding, the reads and the rebuild.
const reloadBudget = 30 * time.Second

// Control is how the buckets page reaches the running engine. A nil func means there is
// no engine in this process: the saved switch still applies at the next start.
type Control struct {
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
}

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
			"orders":    map[string]any{"on": on, "source": source, "placing": placing, "effective": effective},
			"versions":  versions,
			"policy": map[string]any{
				"id": policy.ID, "since_at": policy.EffectiveAt.UTC().Format(time.RFC3339), "note": policy.Note,
				"winnings_bps": policy.Winnings, "replenish_bps": policy.Replenish, "tax_bps": policy.Tax, "fees_bps": policy.Fees,
			},
		})
	})

	mux.HandleFunc("POST /api/controls/orders", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("POST /api/controls/version", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("POST /api/controls/policy", func(w http.ResponseWriter, r *http.Request) {
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
	})

	mux.HandleFunc("POST /api/controls/reset", func(w http.ResponseWriter, r *http.Request) {
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
	})
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
