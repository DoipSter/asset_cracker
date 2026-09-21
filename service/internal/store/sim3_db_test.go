package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestRecordOrdersOnDevDatabase is test 45 of docs/honest-fills-v3.md: the statements of sim3.go
// against a real Postgres, which the pure tests cannot reach. It runs only when AC_TEST_DB_URL
// names a database whose name ends in _dev, with migration 0012 applied. Everything it writes is
// inside ONE transaction that is rolled back, so it leaves nothing in an append-only ledger.
//
// WRITTEN WITHOUT A DATABASE TO RUN IT ON (2026-09-21). Until it has passed once, a failure here
// may be this test's mistake and not sim3.go's. It needs this month's evaluation and decision
// partitions to exist, which they do on any database the service has started against.
func TestRecordOrdersOnDevDatabase(t *testing.T) {
	url := os.Getenv("AC_TEST_DB_URL")
	if url == "" {
		t.Skip("AC_TEST_DB_URL is not set: the statements of sim3.go were NOT run")
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

	// This test's own world, under names nobody else uses.
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	actor := one(`insert into actor (kind, handle) values ('system', 'sim3test') returning id`)
	cash := one(`insert into ledger_account (kind, mode, name) values ('bucket', 'sim', 'sim3test bucket cash') returning id`)
	other := one(`insert into ledger_account (kind, mode, name) values ('bucket', 'sim', 'sim3test other cash') returning id`)
	setup := SimSetup{ActorID: actor,
		VenueLedgerID: one(`insert into ledger_account (kind, mode, name) values ('venue', 'sim', 'sim3test venue') returning id`),
		FeesLedgerID:  one(`insert into ledger_account (kind, mode, name) values ('fees', 'sim', 'sim3test fees') returning id`)}
	source := one(`insert into source (code, name, has_market_data) values ('sim3test', 'sim3 test venue', true) returning id`)
	venueAccount := one(`insert into venue_account (source_id, mode, name) values ($1, 'sim', 'sim3test paper') returning id`, source)
	strategy := one(`insert into strategy (family, name) values ('sim3test', 'T') returning id`)
	version := one(`insert into strategy_version (strategy_id, version, params, code_ref, created_by) values ($1, 3, '{}', 'test', $2) returning id`, strategy, actor)
	bucket := one(`insert into bucket (name, mode, venue_account_id, ledger_account_id, strategy_version_id, limits, tax_rate_bps)
	               values ('sim3test bucket', 'sim', $1, $2, $3, '{}', 0) returning id`, venueAccount, cash, version)
	instrument := one(`insert into instrument (source_id, kind, symbol, underlying) values ($1, 'binary_contract', 'SIM3TEST', 'DOGE') returning id`, source)
	market := one(`insert into market (instrument_id, ticker, strike) values ($1, 'SIM3TEST-1', 0.089) returning id`, instrument)
	at := time.Now().UTC().Truncate(time.Microsecond) // what a timestamptz can hold: decisions point at (evaluation id, at)
	evaluation := one(`insert into evaluation (at, market_id, underlying_price, quotes) values ($1, $2, 0.0891, '{}') returning id`, at, market)

	size := 200
	step := StepRecord3{EvaluationID: evaluation, At: at, MarketID: market,
		Decisions: []DecisionRow{{BucketID: bucket, VersionID: version, ModelProb: 0.148, MarketProb: 0.11, Side: "yes", Edge: 0.004, Action: "enter", Why: "test", SizeAlone: &size}},
		Orders: []OrderRow{
			{DecisionIndex: 0, BucketID: bucket, BucketLedgerID: cash, ClientID: "v3:sim3test:1", Action: "buy", Side: "yes", Qty: 200, Limit: "0.1300", Status: "partial",
				Detail: map[string]any{"v": 3}, Fills: []FillRow{{161, "0.1200", 1932, 120}, {23, "0.1300", 299, 18}}},
			{DecisionIndex: -1, BucketID: bucket, BucketLedgerID: cash, ClientID: "v3:sim3test:2", Action: "sell", Side: "yes", Qty: 1, Limit: "0.0001", Status: "filled",
				Fills: []FillRow{{1, "0.0100", 1, 1}}}, // premium equals fee: no bucket entry
			{DecisionIndex: -1, BucketID: bucket, BucketLedgerID: cash, ClientID: "v3:sim3test:3", Action: "sell", Side: "no", Qty: 9, Limit: "0.0900", Status: "cancelled"},
		}}
	plan, err := planStep(setup, step)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordOrdersTx(ctx, tx, setup, step, plan); err != nil {
		t.Fatalf("RecordOrders: %v", err)
	}
	// The balance triggers are deferred to commit, and this transaction never commits: run them now.
	if _, err := tx.Exec(ctx, `set constraints all immediate`); err != nil {
		t.Fatalf("the transfers do not balance: %v", err)
	}
	if _, err := tx.Exec(ctx, `set constraints all deferred`); err != nil {
		t.Fatal(err)
	}

	if n := one(`select count(distinct f.transfer_id) from fill f join trade_order o on o.id = f.order_id where o.bucket_id = $1`, bucket); n != 3 {
		t.Errorf("%d transfers for three fills, want one each", n)
	}
	if n := one(`select count(*) from fill f join trade_order o on o.id = f.order_id where o.client_order_id = 'v3:sim3test:3'`); n != 0 {
		t.Errorf("a cancelled order has %d fills", n)
	}
	if n := one(`select coalesce(sum(balance_cents), 0)::bigint from ledger_balance where mode = 'sim'`); n != 0 {
		t.Errorf("the sim ledger sums to %d cents", n)
	}
	got, err := bucketCash(ctx, tx, []int64{bucket})
	if err != nil || got[bucket] != -2369 {
		t.Errorf("bucket cash %v, %v; want -2369", got, err)
	}
	open, err := openQty(ctx, tx, market, []int64{bucket})
	if err != nil || len(open) != 1 || open[LotKey(bucket, "yes")] != 183 {
		t.Errorf("open %v, %v; want 183 yes and nothing else", open, err)
	}
	fills, err := bucketFills(ctx, tx, []int64{bucket})
	if err != nil || len(fills) != 3 {
		t.Fatalf("rebuild read: %d fills, %v; want 3", len(fills), err)
	}
	for i, want := range []struct {
		price string
		cents int64
	}{{"0.1200", -2052}, {"0.1300", -317}, {"0.0100", 0}} {
		if fills[i].Price != want.price || fills[i].BucketCents != want.cents || fills[i].Underlying != "DOGE" || fills[i].Result != "" {
			t.Errorf("fill %d read back as %+v; want price %s, %d cents", i+1, fills[i], want.price, want.cents)
		}
	}

	// A client id that exists is refused, and the step's other rows go with it.
	again := StepRecord3{EvaluationID: evaluation, At: at, MarketID: market, Orders: []OrderRow{
		{DecisionIndex: -1, BucketID: bucket, BucketLedgerID: cash, ClientID: "v3:sim3test:4", Action: "sell", Side: "yes", Qty: 1, Limit: "0.0900", Status: "cancelled"},
		{DecisionIndex: -1, BucketID: bucket, BucketLedgerID: cash, ClientID: "v3:sim3test:1", Action: "sell", Side: "yes", Qty: 1, Limit: "0.0900", Status: "cancelled"}}}
	plan, _ = planStep(setup, again)
	sp, err := tx.Begin(ctx) // a savepoint, standing in for RecordOrders' own transaction
	if err != nil {
		t.Fatal(err)
	}
	err = recordOrdersTx(ctx, sp, setup, again, plan)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || !definitelyRolledBack(err) {
		t.Errorf("a duplicate client id: %v; want a unique violation that is definitely rolled back", err)
	}
	sp.Rollback(ctx)
	recorded, err := ordersRecorded(ctx, tx, []string{"v3:sim3test:1", "v3:sim3test:3", "v3:sim3test:4", "never"})
	if err != nil || !recorded["v3:sim3test:1"] || !recorded["v3:sim3test:3"] || recorded["v3:sim3test:4"] || recorded["never"] {
		t.Errorf("recorded %v, %v", recorded, err)
	}

	// A ledger account that is not the bucket's own moves nothing.
	wrong := StepRecord3{EvaluationID: evaluation, At: at, MarketID: market, Orders: []OrderRow{
		{DecisionIndex: -1, BucketID: bucket, BucketLedgerID: other, ClientID: "v3:sim3test:5", Action: "sell", Side: "yes", Qty: 9, Limit: "0.0900", Status: "filled",
			Fills: []FillRow{{9, "0.1000", 90, 6}}}}}
	plan, _ = planStep(setup, wrong)
	if sp, err = tx.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	// The server answered every statement, so this is not a PgError; it is this package's own
	// refusal before COMMIT, which the runner must be able to tell from "outcome unknown".
	if err := recordOrdersTx(ctx, sp, setup, wrong, plan); err == nil {
		t.Error("a fill was booked to a ledger account the bucket does not own")
	} else if !errors.Is(err, ErrRefused) || !definitelyRolledBack(err) {
		t.Errorf("a ledger account the bucket does not own: %v; want ErrRefused, definitely rolled back", err)
	}
	sp.Rollback(ctx)

	// An order that filled nothing is journaled whatever the broker's reason: a row with the
	// reason in its detail, no fill, no transfer. A limit that is not a price is stored as null
	// and kept in the detail as it was sent.
	const badLimit, noBids = "the limit is outside 0.0001..0.9999", "no bids on this side"
	transfers := one(`select count(*) from ledger_transfer where created_by = $1`, actor)
	nothing := StepRecord3{EvaluationID: evaluation, At: at, MarketID: market, Orders: []OrderRow{
		{DecisionIndex: -1, BucketID: bucket, BucketLedgerID: cash, ClientID: "v3:sim3test:6", Action: "sell", Side: "yes", Qty: 9, Limit: "0.0000", Status: "rejected",
			Detail: map[string]any{"reason": badLimit}},
		{DecisionIndex: -1, BucketID: bucket, BucketLedgerID: cash, ClientID: "v3:sim3test:7", Action: "sell", Side: "yes", Qty: 9, Limit: "0.0900", Status: "cancelled",
			Detail: map[string]any{"reason": noBids}}}}
	if plan, err = planStep(setup, nothing); err != nil {
		t.Fatalf("an order that filled nothing: %v", err)
	}
	if sp, err = tx.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recordOrdersTx(ctx, sp, setup, nothing, plan); err != nil {
		sp.Rollback(ctx)
		t.Errorf("an order that filled nothing was not journaled: %v", err)
	} else {
		if err := sp.Commit(ctx); err != nil { // releases the savepoint; the outer transaction still rolls back
			t.Fatal(err)
		}
		if n := one(`select count(*) from trade_order o
		              where o.client_order_id = 'v3:sim3test:6' and o.status = 'rejected' and o.limit_price is null
		                and o.detail->>'reason' = $1 and o.detail->>'limit_not_stored' = '0.0000'
		                and not exists (select 1 from fill f where f.order_id = o.id)`, badLimit); n != 1 {
			t.Errorf("a rejected order with a limit of 0: %d rows as expected, want 1", n)
		}
		if n := one(`select count(*) from trade_order o
		              where o.client_order_id = 'v3:sim3test:7' and o.status = 'cancelled' and o.limit_price = 0.09
		                and o.detail->>'reason' = $1 and (o.detail->>'limit_not_stored') is null
		                and not exists (select 1 from fill f where f.order_id = o.id)`, noBids); n != 1 {
			t.Errorf("a cancelled order with a good limit: %d rows as expected, want 1", n)
		}
		if n := one(`select count(*) from ledger_transfer where created_by = $1`, actor); n != transfers {
			t.Errorf("orders that filled nothing wrote %d transfers", n-transfers)
		}
	}

	cancelledCtx, stop := context.WithCancel(ctx)
	stop()
	if err := s.RecordOrders(cancelledCtx, setup, wrong); err == nil || definitelyRolledBack(err) {
		t.Errorf("a cancelled context: %v; want an error whose outcome is unknown", err)
	}
}
