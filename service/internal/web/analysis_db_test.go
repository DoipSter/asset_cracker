package web

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/analysis"
	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestAnalysisRefreshOnDevDatabase runs the refresh as the service does, against a real database,
// until the history is read or a minute is up, and looks at the gate decisions the document
// makes. It writes nothing: every version is marked as already stored, so storeDecisions has
// nothing new to write. It runs only when AC_TEST_DB_URL names a database whose name ends in _dev.
func TestAnalysisRefreshOnDevDatabase(t *testing.T) {
	url := os.Getenv("AC_TEST_DB_URL")
	if url == "" {
		t.Skip("AC_TEST_DB_URL is not set: the refresh was NOT run against a database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if cfg, err := pgconn.ParseConfig(url); err != nil || !strings.HasSuffix(cfg.Database, "_dev") {
		t.Fatalf("refusing: %q does not name a _dev database (%v)", url, err)
	}
	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	c := newAnalysisCache()
	c.ctx, c.gate = ctx, analysis.DefaultGate
	c.snappedKnown = true
	stored := func(doc analysis.Document) { // mark everything as stored, so nothing is written
		for _, r := range doc.Leaderboard.Rows {
			c.snapped[r.VersionID] = math.MaxInt64
		}
	}
	var doc analysis.Document
	for i := 0; i < 60; i++ {
		rctx, rcancel := context.WithTimeout(ctx, analysisTimeout)
		doc, err = c.compute(rctx, db)
		rcancel()
		if err != nil {
			t.Fatalf("refresh %d: %v", i+1, err)
		}
		stored(doc)
		if doc.Coverage.Complete {
			break
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(raw); strings.Contains(s, "NaN") || strings.Contains(s, "Inf") || strings.Contains(s, "null") {
		t.Errorf("the document is not clean JSON: %s", s)
	}
	t.Logf("coverage %+v, %d windows, trials %d, z %.4f", doc.Coverage, doc.WindowsRecorded, doc.Conventions.Trials, doc.Gate.Z)
	o := doc.Scorecard.Overall
	t.Logf("scorecard overall: %d windows, brier %.4f vs %.4f (t %.2f, %s), log loss %.4f vs %.4f (diff %.4f, se %.4f)",
		o.NWindows, o.BrierModel, o.BrierMarket, o.T, o.Verdict, o.LogLossModel, o.LogLossMarket, o.LogLossDiff, o.LogLossSE)
	for _, r := range doc.Leaderboard.Rows {
		if r.Windows == 0 {
			continue
		}
		t.Logf("%-9s %s %-4s windows %3d bets %4d staked %8d pnl %7d return %+.4f se %.4f lower %+.4f needed %5d drawdown max %6d now %6d | gate evaluated %v passed %v",
			r.Strategy, r.Engine, r.World, r.Windows, r.Bets, r.StakedCents, r.LifetimePnLCents, r.ReturnPerDollar, r.ReturnSE, r.ReturnLower, r.WindowsNeeded,
			r.Drawdown.MaxCents, r.Drawdown.NowCents, r.Gate.Evaluated, r.Gate.Passed)
		for _, ch := range r.Gate.Checks {
			t.Logf("            %-11s %-5v %s", ch.Name, ch.Passed, ch.Why)
		}
		if r.Gate.Why != "" {
			t.Logf("            not evaluated: %s", r.Gate.Why)
		}
	}
	if snaps := analysis.Snapshots(doc); doc.Coverage.Complete && doc.Conventions.Trials > 0 && len(snaps) == 0 {
		t.Errorf("a complete document with versions that bet must have decisions to store")
	}
}
