package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCloseOutGateOnDevDatabase runs the close-out's gate (stillHolds, the one statement
// CloseOutBucket refuses on) against a real Postgres, which the pure tests cannot reach. It is
// the defect of 2026-09-27: a legacy bucket that ever held a bet to settlement could never be
// closed out, because the gate summed fills without excluding lots that have a settlement row,
// and a settlement writes a settlement row, never a sell fill. It runs only when AC_TEST_DB_URL
// names a database whose name ends in _dev. Everything it writes is inside ONE transaction that
// is rolled back, so it leaves nothing in an append-only ledger.
//
// It is the gate that is run, not CloseOutBucket whole: the close itself (CloseBucket) opens its
// own transaction on the pool and COMMITS a reap, a frozen bucket and three bucket_event rows,
// which this test could not take back. The gate is the only thing the fix changed.
//
// WRITTEN WITHOUT A DATABASE TO RUN IT ON (2026-09-27). Until it has passed once, a failure here
// may be this test's mistake and not sim.go's.
func TestCloseOutGateOnDevDatabase(t *testing.T) {
	url := os.Getenv("AC_TEST_DB_URL")
	if url == "" {
		t.Skip("AC_TEST_DB_URL is not set: the close-out gate of sim.go was NOT run")
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

	// This test's own world, under names nobody else uses. The version is 2: the web handler
	// sends every bucket below version 3 to CloseOutBucket, and those are the hold-to-settlement
	// buckets the defect bit.
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	actor := one(`insert into actor (kind, handle) values ('system', 'closeouttest') returning id`)
	cash := one(`insert into ledger_account (kind, mode, name) values ('bucket', 'sim', 'closeouttest bucket cash') returning id`)
	setup := SimSetup{ActorID: actor,
		VenueLedgerID: one(`insert into ledger_account (kind, mode, name) values ('venue', 'sim', 'closeouttest venue') returning id`),
		FeesLedgerID:  one(`insert into ledger_account (kind, mode, name) values ('fees', 'sim', 'closeouttest fees') returning id`)}
	source := one(`insert into source (code, name, has_market_data) values ('closeouttest', 'close-out test venue', true) returning id`)
	venueAccount := one(`insert into venue_account (source_id, mode, name) values ($1, 'sim', 'closeouttest paper') returning id`, source)
	strategy := one(`insert into strategy (family, name) values ('closeouttest', 'T') returning id`)
	version := one(`insert into strategy_version (strategy_id, version, params, code_ref, created_by) values ($1, 2, '{}', 'test', $2) returning id`, strategy, actor)
	bucket := one(`insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
	               values ('closeouttest bucket', 'sim', $1, $2, $3, '{}', 0) returning id`, venueAccount, cash, version)
	instrument := one(`insert into instrument (source_id, kind, symbol, underlying) values ($1, 'binary_contract', 'CLOSEOUTTEST', 'DOGE') returning id`, source)
	ref := BucketRef{ID: bucket, VersionID: version, Version: 2, Name: "closeouttest bucket", Status: "active"}

	// Three markets. A is held to the close and settled (a loser: payout 0, no transfer). B is
	// held and never settled. C is bought and sold out early: no settlement row, ever, and no
	// contracts either.
	at := time.Now().UTC().Truncate(time.Microsecond)
	market := func(ticker string) (int64, int64) {
		m := one(`insert into market (instrument_id, ticker, strike) values ($1, $2, 0.089) returning id`, instrument, ticker)
		e := one(`insert into evaluation (at, market_id, underlying_price, quotes) values ($1, $2, 0.0891, '{}') returning id`, at, m)
		return m, e
	}
	record := func(marketID, evaluationID int64, orders ...OrderRow) {
		t.Helper()
		step := StepRecord3{EvaluationID: evaluationID, At: at, MarketID: marketID, Orders: orders}
		plan, err := planStep(setup, step)
		if err != nil {
			t.Fatal(err)
		}
		if err := recordOrdersTx(ctx, tx, setup, step, plan); err != nil {
			t.Fatalf("RecordOrders: %v", err)
		}
	}
	buy := func(client, side string, qty int, price string, premium, fee int64) OrderRow {
		return OrderRow{DecisionIndex: -1, BucketID: bucket, BucketLedgerID: cash, ClientID: client, Action: "buy", Side: side,
			Qty: qty, Limit: price, Status: "filled", Fills: []FillRow{{qty, price, premium, fee}}}
	}
	sell := func(client, side string, qty int, price string, premium, fee int64) OrderRow {
		return OrderRow{DecisionIndex: -1, BucketID: bucket, BucketLedgerID: cash, ClientID: client, Action: "sell", Side: side,
			Qty: qty, Limit: price, Status: "filled", Fills: []FillRow{{qty, price, premium, fee}}}
	}
	a, evalA := market("CLOSEOUTTEST-A")
	b, evalB := market("CLOSEOUTTEST-B")
	c, evalC := market("CLOSEOUTTEST-C")
	record(a, evalA, buy("v2:closeouttest:a1", "yes", 5, "0.4000", 200, 10))
	record(b, evalB, buy("v2:closeouttest:b1", "no", 3, "0.3000", 90, 6))
	record(c, evalC, buy("v2:closeouttest:c1", "yes", 2, "0.5000", 100, 4))
	record(c, evalC, sell("v2:closeouttest:c2", "yes", 2, "0.6000", 120, 4))
	// The balance triggers are deferred to commit, and this transaction never commits: run them now.
	if _, err := tx.Exec(ctx, `set constraints all immediate`); err != nil {
		t.Fatalf("the transfers do not balance: %v", err)
	}
	if _, err := tx.Exec(ctx, `set constraints all deferred`); err != nil {
		t.Fatal(err)
	}

	// Nothing settled yet: A and B are open, C nets to nothing. Eight contracts, not ten.
	var open OpenContracts
	if err := stillHolds(ctx, tx, ref); !errors.As(err, &open) || open.Lots != 8 || open.Name != ref.Name {
		t.Fatalf("with A and B held and C sold out: %v; want OpenContracts{closeouttest bucket, 8}", err)
	}

	// A settles. That is a settlement row and no sell fill, exactly as RecordSettlements writes
	// it, so A's fills still net to 5 bought. The gate must not count them any more, and must
	// still count B's: without the settlement exclusion the old gate went on counting A's 5 for
	// ever (8 here, never fewer), which is the defect; nothing was ever hidden, because no lot's
	// net is negative.
	if _, err := tx.Exec(ctx, `insert into settlement (market_id, bucket_id, at, side, qty, payout_cents, transfer_id)
	                           values ($1, $2, $3, 'yes', 5, 0, null)`, a, bucket, at.Add(time.Minute)); err != nil {
		t.Fatalf("settlement of A: %v", err)
	}
	if n := one(`select sum(case when o.action = 'buy' then f.qty else -f.qty end)::bigint
	               from trade_order o join fill f on f.order_id = o.id where o.bucket_id = $1 and o.market_id = $2`, bucket, a); n != 5 {
		t.Fatalf("A's fills net to %d after settlement; the test assumed a settlement writes no sell fill", n)
	}
	open = OpenContracts{}
	if err := stillHolds(ctx, tx, ref); !errors.As(err, &open) || open.Lots != 3 {
		t.Errorf("with A settled and B still held: %v; want OpenContracts with 3 contracts (B's)", err)
	}

	// B settles too (a winner this time, payout with a transfer). The bucket is flat and the
	// close-out may go ahead: the gate answers nil.
	transfer := one(`insert into ledger_transfer (at, mode, reason, memo, created_by) values ($1, 'sim', 'settlement', '3 no settled', $2) returning id`, at.Add(time.Minute), actor)
	if _, err := tx.Exec(ctx, `insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values ($1, $2, 'sim', -300), ($1, $3, 'sim', 300)`,
		transfer, setup.VenueLedgerID, cash); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `insert into settlement (market_id, bucket_id, at, side, qty, payout_cents, transfer_id)
	                           values ($1, $2, $3, 'no', 3, 300, $4)`, b, bucket, at.Add(time.Minute), transfer); err != nil {
		t.Fatalf("settlement of B: %v", err)
	}
	if err := stillHolds(ctx, tx, ref); err != nil {
		t.Errorf("with every lot settled or sold out: %v; want nil, the close-out goes ahead", err)
	}

	// A settlement on ONE side does not clear the other: a bucket that held both sides of a
	// market (a hedge) and was paid on one is still holding the other until it, too, settles.
	d, evalD := market("CLOSEOUTTEST-D")
	record(d, evalD, buy("v2:closeouttest:d1", "yes", 4, "0.5000", 200, 8), buy("v2:closeouttest:d2", "no", 4, "0.5000", 200, 8))
	if _, err := tx.Exec(ctx, `insert into settlement (market_id, bucket_id, at, side, qty, payout_cents, transfer_id)
	                           values ($1, $2, $3, 'yes', 4, 0, null)`, d, bucket, at.Add(time.Minute)); err != nil {
		t.Fatalf("settlement of D yes: %v", err)
	}
	open = OpenContracts{}
	if err := stillHolds(ctx, tx, ref); !errors.As(err, &open) || open.Lots != 4 {
		t.Errorf("with D's yes settled and its no not: %v; want OpenContracts with 4 contracts (D's no)", err)
	}
}
