package store

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/analysis"
)

// TestAnalysisStatementsOnDevDatabase runs the statements this file adds for the promotion gate
// against a real Postgres, which the pure tests cannot reach: the log-loss and decision-count
// reads of AnalysisWindow on the newest settled window the database has, and the metric_snapshot
// writer and reader inside one transaction that is rolled back, so nothing is left behind. It
// runs only when AC_TEST_DB_URL names a database whose name ends in _dev.
func TestAnalysisStatementsOnDevDatabase(t *testing.T) {
	url := os.Getenv("AC_TEST_DB_URL")
	if url == "" {
		t.Skip("AC_TEST_DB_URL is not set: the statements of analysis.go were NOT run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var db string
	if err := s.pool.QueryRow(ctx, `select current_database()`).Scan(&db); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(db, "_dev") {
		t.Fatalf("refusing: %q is not a _dev database", db)
	}

	// 1. The reads, on what the database holds. Read-only.
	markets, err := s.AnalysisMarkets(ctx, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	modelVersions, err := s.AnalysisModelVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	buckets, _, err := s.AnalysisBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	seen := map[int64]bool{}
	for _, b := range buckets {
		if !seen[b.VersionID] {
			seen[b.VersionID] = true
			versions = append(versions, b.VersionID)
		}
	}
	if len(markets) == 0 {
		t.Log("no settled market on this database: AnalysisWindow's statements were NOT run")
	} else {
		newest := markets[len(markets)-1].Closes
		var window []analysis.Market
		for _, m := range markets {
			if m.Closes == newest {
				window = append(window, m)
			}
		}
		facts, unready, err := s.AnalysisWindow(ctx, modelVersions, versions, window)
		if err != nil {
			t.Fatalf("AnalysisWindow: %v", err)
		}
		var scored, journaled int64
		for _, f := range facts {
			for _, b := range f.Score {
				if math.IsNaN(b.ModelLog) || math.IsInf(b.ModelLog, 0) || math.IsNaN(b.MarketLog) || math.IsInf(b.MarketLog, 0) || b.ModelLog < 0 || b.MarketLog < 0 {
					t.Errorf("market %d: log loss sums %v %v", f.ID, b.ModelLog, b.MarketLog)
				}
				if b.N > 0 && (b.ModelLog == 0 || b.MarketLog == 0) {
					t.Errorf("market %d: %d rows scored and a log loss of exactly 0", f.ID, b.N)
				}
				scored += b.N
			}
			for _, n := range f.Decisions {
				journaled += n
			}
		}
		t.Logf("window %d: %d markets, %d ready, %d held back, %d rows scored, %d journal rows counted over %d versions",
			newest, len(window), len(facts), len(unready), scored, journaled, len(versions))
	}

	// 2. The writes, inside a transaction that never commits.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	actor := one(`insert into actor (kind, handle) values ('system', 'gatetest') returning id`)
	strategy := one(`insert into strategy (family, name) values ('gatetest', 'G') returning id`)
	version := one(`insert into strategy_version (strategy_id, version, params, code_ref, created_by) values ($1, 1, '{}', 'test', $2) returning id`, strategy, actor)

	before, err := metricSnapshotLatest(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := before[version]; has {
		t.Fatalf("a version created just now has a stored decision")
	}
	row, _ := json.Marshal(map[string]any{"strategy_version_id": version, "return_per_dollar": 0.2})
	cfg, _ := json.Marshal(map[string]any{"z": 2.9913, "min_edge_per_dollar": 0.02})
	first := analysis.Snapshot{VersionID: version, FirstClose: 900, LastClose: 36000, Decisions: 2400, Orders: 40, Trials: 18, Metrics: row, GateConfig: cfg, GatePassed: true}
	n, err := insertMetricSnapshots(ctx, tx, []analysis.Snapshot{first})
	if err != nil || n != 1 {
		t.Fatalf("first insert: %d, %v", n, err)
	}
	// the same decision again writes nothing; a later one writes one
	later := first
	later.LastClose, later.GatePassed = 36900, false
	n, err = insertMetricSnapshots(ctx, tx, []analysis.Snapshot{first, later})
	if err != nil || n != 1 {
		t.Fatalf("repeat and later: %d written, %v; want 1", n, err)
	}
	after, err := metricSnapshotLatest(ctx, tx)
	if err != nil || after[version] != 36900 {
		t.Fatalf("latest: %v, %v; want 36900 for version %d", after, err, version)
	}
	var count int
	var lower, upper time.Time
	var passed bool
	var decisions, trades, trials int
	if err := tx.QueryRow(ctx, `select count(*), min(lower(period)), max(upper(period)), bool_and(gate_passed), max(n_decisions), max(n_trades), max(trials_at_the_time)
	                             from metric_snapshot where strategy_version_id = $1`, version).Scan(&count, &lower, &upper, &passed, &decisions, &trades, &trials); err != nil {
		t.Fatal(err)
	}
	if count != 2 || lower.Unix() != 0 || upper.Unix() != 36900 || passed || decisions != 2400 || trades != 40 || trials != 18 {
		t.Errorf("stored: %d rows, period %v to %v, all passed %v, %d decisions, %d trades, %d trials", count, lower.Unix(), upper.Unix(), passed, decisions, trades, trials)
	}
	var back map[string]any
	var raw []byte
	if err := tx.QueryRow(ctx, `select metrics from metric_snapshot where strategy_version_id = $1 order by id limit 1`, version).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &back); err != nil || back["return_per_dollar"] != 0.2 {
		t.Errorf("metrics read back: %v %s", err, raw)
	}
}
