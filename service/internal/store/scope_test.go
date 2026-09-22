package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The analysis and the status page's recent rounds read the 15-minute series only. These are
// every query that lists or counts markets across instruments for them; each must join the
// instrument and carry the scope.
func TestAnalysisAndRecentRoundsReadOnlyTheFifteenMinuteSeries(t *testing.T) {
	for name, sql := range map[string]string{
		"AnalysisSettledCount": sqlAnalysisSettledCount,
		"AnalysisMarkets":      sqlAnalysisMarkets,
		"AnalysisOpenWindows":  sqlAnalysisOpenWindows,
		"RecentRounds":         sqlRecentRounds,
	} {
		if !strings.Contains(sql, "join instrument i on i.id = m.instrument_id") {
			t.Errorf("%s does not join the instrument", name)
		}
		if !strings.Contains(sql, FifteenMinuteSeries) {
			t.Errorf("%s is not scoped to the 15-minute series", name)
		}
	}
	// Counted and listed alike: the analysis checks one against the other.
	if !strings.Contains(sqlAnalysisMarkets, "m.result in ('yes', 'no') and m.closes_at is not null") ||
		!strings.Contains(sqlAnalysisSettledCount, "m.result in ('yes', 'no') and m.closes_at is not null") {
		t.Error("AnalysisSettledCount no longer counts what AnalysisMarkets lists")
	}
}

// A 15-minute series switched on from the assets page has round_seconds 900 like the five, and
// must still stay out of the analysis and the home page: the scope names the five series.
func TestFifteenMinuteScopeNamesTheFiveSeries(t *testing.T) {
	if !strings.Contains(FifteenMinuteSeries, `i.spec @> '{"round_seconds": 900}'::jsonb and i.symbol in (`) {
		t.Errorf("scope %q no longer asks for round_seconds 900 AND a named series", FifteenMinuteSeries)
	}
	if len(AnalysisSeries) != 5 {
		t.Fatalf("AnalysisSeries has %d series, want the five", len(AnalysisSeries))
	}
	quoted := make([]string, len(AnalysisSeries))
	for i, s := range AnalysisSeries {
		quoted[i] = "'" + s + "'"
	}
	if !strings.Contains(FifteenMinuteSeries, "i.symbol in ("+strings.Join(quoted, ", ")+"))") {
		t.Errorf("scope %q does not name exactly %v", FifteenMinuteSeries, AnalysisSeries)
	}
	for _, other := range []string{"KXBNB15M", "KXNEAR15M", "KXHYPE15M", "KXBTCD"} {
		if strings.Contains(FifteenMinuteSeries, "'"+other+"'") {
			t.Errorf("scope admits %s", other)
		}
	}
}

// TestScopeOnDevDatabase runs the scoped statements against a real Postgres, inside ONE
// transaction that is rolled back. It runs only when AC_TEST_DB_URL names a *_dev database.
// WRITTEN WITHOUT A DATABASE TO RUN IT ON (2026-09-21): until it has passed once, a failure may be
// this test's mistake. It does not need migration 0014: the ladder here is marked by spec only.
func TestScopeOnDevDatabase(t *testing.T) {
	url := os.Getenv("AC_TEST_DB_URL")
	if url == "" {
		t.Skip("AC_TEST_DB_URL is not set: the scoped statements were NOT run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	count := func() int64 { return one(sqlAnalysisSettledCount) }

	source := one(`insert into source (code, name, has_market_data) values ('scopetest', 'scope test venue', true) returning id`)
	// One of the five names (under the test's own source), as the scope now requires.
	fifteen := one(`insert into instrument (source_id, kind, symbol, underlying, spec)
	                values ($1, 'binary_contract', 'KXBTC15M', 'BTC', '{"round_seconds": 900}') returning id`, source)
	// A 15-minute series switched on from the assets page: round_seconds 900, not one of the five.
	picked := one(`insert into instrument (source_id, kind, symbol, underlying, spec)
	               values ($1, 'binary_contract', 'SCOPETESTSEL15M', 'BTC', '{"round_seconds": 900, "selected": true, "trade": false}') returning id`, source)
	ladder := one(`insert into instrument (source_id, kind, symbol, underlying, spec)
	               values ($1, 'binary_contract', 'SCOPETESTD', 'BTC', '{"ladder": true, "trade": false}') returning id`, source)

	settledAt := time.Date(2001, 2, 3, 4, 0, 0, 0, time.UTC)
	market := func(instrument int64, ticker string, closes time.Time, result any) int64 {
		return one(`insert into market (instrument_id, ticker, closes_at, result) values ($1, $2, $3, $4) returning id`,
			instrument, ticker, closes, result)
	}
	before := count()
	market(ladder, "SCOPETESTD-SETTLED", settledAt, "yes")
	if got := count(); got != before {
		t.Errorf("a settled ladder market changed the settled count from %d to %d", before, got)
	}
	market(picked, "SCOPETESTSEL15M-SETTLED", settledAt, "yes")
	if got := count(); got != before {
		t.Errorf("a settled market of a selected 15-minute series changed the settled count from %d to %d", before, got)
	}
	keep := market(fifteen, "SCOPETEST15M-SETTLED", settledAt, "no")
	if got := count(); got != before+1 {
		t.Errorf("a settled 15-minute market moved the settled count from %d to %d, want +1", before, got)
	}

	rows, err := tx.Query(ctx, sqlAnalysisMarkets, settledAt)
	if err != nil {
		t.Fatal(err)
	}
	var listed []int64
	for rows.Next() {
		var id, closes int64
		var coin, result string
		if err := rows.Scan(&id, &coin, &closes, &result); err != nil {
			t.Fatal(err)
		}
		if closes == settledAt.Unix() {
			listed = append(listed, id)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if len(listed) != 1 || listed[0] != keep {
		t.Errorf("AnalysisMarkets at the shared close listed %v, want only the 15-minute market %d", listed, keep)
	}

	// Two unsettled markets that closed ten minutes ago, at seconds no real round closes on.
	now := time.Now().UTC().Truncate(time.Minute)
	ladderClose, fifteenClose := now.Add(-10*time.Minute+7*time.Second), now.Add(-10*time.Minute+11*time.Second)
	market(ladder, "SCOPETESTD-OPEN", ladderClose, nil)
	market(fifteen, "SCOPETEST15M-OPEN", fifteenClose, nil)
	wrows, err := tx.Query(ctx, sqlAnalysisOpenWindows, now)
	if err != nil {
		t.Fatal(err)
	}
	windows := map[int64]bool{}
	for wrows.Next() {
		var w int64
		if err := wrows.Scan(&w); err != nil {
			t.Fatal(err)
		}
		windows[w] = true
	}
	if wrows.Err() != nil {
		t.Fatal(wrows.Err())
	}
	if windows[ladderClose.Unix()] || !windows[fifteenClose.Unix()] {
		t.Errorf("open windows: ladder's close held out %v (want false), 15-minute's %v (want true)",
			windows[ladderClose.Unix()], windows[fifteenClose.Unix()])
	}

	// A weekly ladder market closing far ahead must not take the recent rounds' first place.
	far := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	market(ladder, "SCOPETESTD-FAR", far, nil)
	market(fifteen, "SCOPETEST15M-FAR", far.Add(-time.Second), nil)
	rrows, err := tx.Query(ctx, sqlRecentRounds, 16)
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for rrows.Next() {
		var r RoundSummary
		if err := rrows.Scan(&r.Series, &r.Ticker, &r.Strike, &r.ClosesAt, &r.Result, &r.SettlementValue, &r.Snapshots); err != nil {
			t.Fatal(err)
		}
		if r.Series == "SCOPETESTD" {
			t.Errorf("recent rounds listed ladder market %s", r.Ticker)
		}
		if first == "" {
			first = r.Ticker
		}
	}
	if rrows.Err() != nil {
		t.Fatal(rrows.Err())
	}
	if first != "SCOPETEST15M-FAR" {
		t.Errorf("recent rounds began with %q, want SCOPETEST15M-FAR", first)
	}
}
