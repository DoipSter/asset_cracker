package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRestakeFamilyGateOnDevDatabase runs the restake's family gate (sameFamily, the one statement
// RestakeBucket gained on 2026-09-27) against a real Postgres, which the pure tests cannot reach.
// It is the defect of that day: a reap-and-restake of a ladder version was accepted by the rounds
// runner, because RestakeBucket checked only that the version's newest bucket was frozen and never
// whose family the version was, so the ladder's next life was seeded in the ledger and held by no
// runner until a restart. It runs only when AC_TEST_DB_URL names a database whose name ends in
// _dev. Everything it writes is inside ONE transaction that is rolled back.
//
// It is the gate that is run, not RestakeBucket whole: the restake itself opens its own
// transaction on the pool and COMMITS a seed transfer and a bucket row, which this test could not
// take back. The gate is the only thing the fix added to the statement.
//
// WRITTEN WITHOUT A DATABASE TO RUN IT ON (2026-09-27). Until it has passed once, a failure here
// may be this test's mistake and not sim.go's.
func TestRestakeFamilyGateOnDevDatabase(t *testing.T) {
	url := os.Getenv("AC_TEST_DB_URL")
	if url == "" {
		t.Skip("AC_TEST_DB_URL is not set: the restake's family gate of sim.go was NOT run")
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

	// This test's own world, under names nobody else uses: one strategy in each family a runner
	// holds, with a version 3 each, and one in a family no runner holds.
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	actor := one(`insert into actor (kind, handle) values ('system', 'restaketest') returning id`)
	version := func(family string) int64 {
		strategy := one(`insert into strategy (family, name) values ($1, 'restaketest') returning id`, family)
		return one(`insert into strategy_version (strategy_id, version, params, code_ref, created_by) values ($1, 3, '{}', 'test', $2) returning id`, strategy, actor)
	}
	rounds, ladders, spot := version(FamilyRounds), version(FamilyLadders), version(FamilySpot)

	// The runner's own family passes, and it is the stored text, 'kalshi15m', that the rounds
	// runner passes: the constant and the column agree.
	if err := sameFamily(ctx, tx, rounds, FamilyRounds); err != nil {
		t.Errorf("a rounds version for the rounds runner: %v, want nil", err)
	}
	if err := sameFamily(ctx, tx, ladders, FamilyLadders); err != nil {
		t.Errorf("a ladder version for the ladder runner: %v, want nil", err)
	}
	// Another family's version is refused, whichever runner asks, and so is a version of a
	// family with no runner (app's loop then tells the page that no runner holds it).
	for _, c := range []struct {
		what    string
		version int64
		family  string
	}{
		{"a ladder version for the rounds runner", ladders, FamilyRounds},
		{"a rounds version for the ladder runner", rounds, FamilyLadders},
		{"a spot version for the rounds runner", spot, FamilyRounds},
		{"a spot version for the ladder runner", spot, FamilyLadders},
	} {
		if err := sameFamily(ctx, tx, c.version, c.family); !errors.Is(err, ErrOtherFamily) {
			t.Errorf("%s: %v, want ErrOtherFamily", c.what, err)
		}
	}
	// The empty string is nobody's family in the column: the runner never passes it (Options3
	// makes its default explicit), and if it did, nothing would match rather than everything.
	if err := sameFamily(ctx, tx, rounds, ""); !errors.Is(err, ErrOtherFamily) {
		t.Errorf("an empty family: %v, want ErrOtherFamily", err)
	}
	// A version that is not there is named as such, as DeployBucket names it, not as "another family".
	if err := sameFamily(ctx, tx, -1, FamilyRounds); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("no such version: %v, want ErrVersionNotFound", err)
	}
}
