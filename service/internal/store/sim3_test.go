package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The pure parts of the third engine's recorder: the rows it builds, the cents they carry, and
// how an error is sorted. Nothing here needs a database; sim3_db_test.go is the part that does.

var testSetup = SimSetup{ActorID: 1, VenueLedgerID: 40, FeesLedgerID: 41}

func testStep(orders ...OrderRow) StepRecord3 {
	return StepRecord3{EvaluationID: 71234, At: time.Unix(1_789_976_488, 0).UTC(), MarketID: 102, Orders: orders}
}

// The big sale of plan 2.4 B, as the broker reports it: 348 asked, five levels found.
func bigSale() OrderRow {
	return OrderRow{DecisionIndex: -1, BucketID: 31, BucketLedgerID: 77, ClientID: "v3:31:71234:1", Action: "sell", Side: "yes",
		Qty: 348, Limit: "0.0900", Status: "partial", Fills: []FillRow{
			{43, "0.1000", 430, 28}, {100, "0.0990", 990, 62}, {1, "0.0930", 9, 1}, {23, "0.0920", 211, 13}, {1, "0.0910", 9, 1}}}
}

// Every transfer sums to zero and has at least two entries, which is what the database's own
// triggers demand; and the bucket's amount is the broker's BucketCents rule (buy: -(premium +
// fee), sell: premium - fee), written out here because the store does not import the broker.
func TestFillCentsBalanceAndNeverWriteAnEmptyTransfer(t *testing.T) {
	for _, action := range []string{"buy", "sell"} {
		for premium := int64(0); premium <= 40; premium++ {
			for fee := int64(0); fee <= 40; fee++ {
				if premium == 0 && fee == 0 {
					continue // never made by the broker, refused by planStep (tested below)
				}
				bucket, venue, fees := fillCents(action, FillRow{Qty: 1, Price: "0.5000", PremiumCents: premium, FeeCents: fee})
				if bucket+venue+fees != 0 {
					t.Fatalf("%s premium %d fee %d: entries sum to %d", action, premium, fee, bucket+venue+fees)
				}
				entries := 0
				for _, c := range []int64{bucket, venue, fees} {
					if c != 0 {
						entries++
					}
				}
				if entries < 2 {
					t.Fatalf("%s premium %d fee %d: %d entries, the ledger refuses fewer than two", action, premium, fee, entries)
				}
				want := premium - fee
				if action == "buy" {
					want = -(premium + fee)
				}
				if bucket != want || fees != fee {
					t.Fatalf("%s premium %d fee %d: bucket %d (want %d), fees %d", action, premium, fee, bucket, want, fees)
				}
			}
		}
	}
}

// The plan's worked examples, to the cent.
func TestFillCentsWorkedExamples(t *testing.T) {
	cases := []struct {
		name                string
		action              string
		fill                FillRow
		bucket, venue, fees int64
	}{
		{"2.4 A: sell 9 yes at 0.1000", "sell", FillRow{9, "0.1000", 90, 6}, 84, -90, 6},
		{"2.4 D: buy 161 yes at 0.1200", "buy", FillRow{161, "0.1200", 1932, 120}, -2052, 1932, 120},
		{"2.4 F: premium equals fee, no bucket entry", "sell", FillRow{1, "0.0100", 1, 1}, 0, -1, 1},
		{"2.4 F: one contract at 0.0091, the bucket pays a cent", "sell", FillRow{1, "0.0091", 0, 1}, -1, 0, 1},
	}
	for _, c := range cases {
		b, v, f := fillCents(c.action, c.fill)
		if b != c.bucket || v != c.venue || f != c.fees {
			t.Errorf("%s: bucket %d venue %d fees %d, want %d %d %d", c.name, b, v, f, c.bucket, c.venue, c.fees)
		}
	}
}

func TestStatusFromFills(t *testing.T) {
	two := []FillRow{{Qty: 43}, {Qty: 100}}
	cases := []struct {
		requested int
		fills     []FillRow
		want      string
		bad       bool
	}{
		{143, two, "filled", false},
		{348, two, "partial", false},
		{9, nil, "cancelled", false},
		{9, []FillRow{}, "cancelled", false},
		{100, two, "", true},               // more filled than asked
		{0, nil, "", true},                 // nothing asked
		{5, []FillRow{{Qty: 0}}, "", true}, // a fill of nothing
	}
	for _, c := range cases {
		got, err := StatusFromFills(c.requested, c.fills)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("requested %d, fills %v: %q, %v; want %q, error %v", c.requested, c.fills, got, err, c.want, c.bad)
		}
	}
}

func TestPlanStepBuildsOneBalancedTransferPerFill(t *testing.T) {
	plan, err := planStep(testSetup, testStep(bigSale()))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || len(plan[0].Fills) != 5 {
		t.Fatalf("plan: %d orders, want 1 with 5 fills", len(plan))
	}
	var toBucket, fee int64
	for _, f := range plan[0].Fills {
		if f.Bucket+f.Venue+f.Fees != 0 {
			t.Errorf("%s: sums to %d", f.Memo, f.Bucket+f.Venue+f.Fees)
		}
		toBucket += f.Bucket
		fee += f.Fees
	}
	// Plan 2.4 B: 168 filled, fee 105c, 1544c to the bucket in five transfers.
	if toBucket != 1544 || fee != 105 {
		t.Errorf("to the bucket %d, fee %d; want 1544 and 105", toBucket, fee)
	}
	if got, want := plan[0].Fills[1].Memo, "sell 100 yes @ 0.0990 (v3:31:71234:1 fill 2/5)"; got != want {
		t.Errorf("memo %q, want %q", got, want)
	}
	if string(plan[0].Detail) != "{}" {
		t.Errorf("an order with no detail is stored as %s, want {}", plan[0].Detail)
	}
}

func TestPlanStepKeepsACancelledOrderAndADecisionOnlyStep(t *testing.T) {
	failedExit := OrderRow{DecisionIndex: 0, BucketID: 31, BucketLedgerID: 77, ClientID: "v3:31:71830:1", Action: "sell", Side: "yes",
		Qty: 9, Limit: "0.0900", Status: "cancelled", Detail: map[string]any{"reason": "no bids on this side"}}
	r := testStep(failedExit)
	r.Decisions = []DecisionRow{{BucketID: 31, VersionID: 19, Action: "exit"}}
	plan, err := planStep(testSetup, r)
	if err != nil || len(plan) != 1 || len(plan[0].Fills) != 0 {
		t.Fatalf("a cancelled order: plan %v, err %v", plan, err)
	}
	rejected := failedExit
	rejected.Status, rejected.DecisionIndex = "rejected", -1
	if _, err := planStep(testSetup, testStep(rejected)); err != nil {
		t.Errorf("a rejected order with no fills: %v", err)
	}
	// Decisions alone need no sim setup: no money is in them. This is the runner's write-probe.
	probe := StepRecord3{EvaluationID: 1, At: time.Now(), MarketID: 1, Decisions: r.Decisions}
	if plan, err := planStep(SimSetup{}, probe); err != nil || len(plan) != 0 {
		t.Errorf("a decision-only step: plan %v, err %v", plan, err)
	}
}

func TestPlanStepRefusesWhatDoesNotAddUp(t *testing.T) {
	change := func(f func(*OrderRow)) StepRecord3 {
		o := bigSale()
		o.Fills = append([]FillRow(nil), o.Fills...)
		f(&o)
		return testStep(o)
	}
	cases := map[string]StepRecord3{
		"status says filled, fills say partial": change(func(o *OrderRow) { o.Status = "filled" }),
		"status says cancelled with fills":      change(func(o *OrderRow) { o.Status = "cancelled" }),
		"status says rejected with fills":       change(func(o *OrderRow) { o.Status = "rejected" }),
		"an unknown status":                     change(func(o *OrderRow) { o.Status = "new" }),
		"more filled than asked":                change(func(o *OrderRow) { o.Qty = 100 }),
		"a fill worth nothing":                  change(func(o *OrderRow) { o.Fills[2] = FillRow{1, "0.0001", 0, 0} }),
		"a negative fee":                        change(func(o *OrderRow) { o.Fills[0].FeeCents = -1 }),
		"a price with two decimals":             change(func(o *OrderRow) { o.Fills[0].Price = "0.10" }),
		"a price of a dollar":                   change(func(o *OrderRow) { o.Fills[0].Price = "1.0000" }),
		"a limit of zero":                       change(func(o *OrderRow) { o.Limit = "0.0000" }),
		"no client id":                          change(func(o *OrderRow) { o.ClientID = "" }),
		"an action that is neither":             change(func(o *OrderRow) { o.Action = "short" }),
		"a side that is neither":                change(func(o *OrderRow) { o.Side = "long" }),
		"no ledger account":                     change(func(o *OrderRow) { o.BucketLedgerID = 0 }),
		"a decision that is not in the step":    change(func(o *OrderRow) { o.DecisionIndex = 0 }),
		"the same client id twice":              testStep(bigSale(), bigSale()),
		"no evaluation":                         {At: time.Now(), MarketID: 1, Orders: []OrderRow{bigSale()}},
		"nothing at all":                        {EvaluationID: 1, At: time.Now(), MarketID: 1},
	}
	for name, r := range cases {
		if _, err := planStep(testSetup, r); !errors.Is(err, ErrInvalidStep) {
			t.Errorf("%s: %v, want ErrInvalidStep", name, err)
		}
	}
	// A winning settlement with a zero setup fails a foreign key; an order is refused sooner.
	if _, err := planStep(SimSetup{}, testStep(bigSale())); !errors.Is(err, ErrInvalidStep) {
		t.Errorf("an empty sim setup: %v, want ErrInvalidStep", err)
	}
}

func TestPriceTextAndBack(t *testing.T) {
	for units, text := range map[int64]string{1: "0.0001", 91: "0.0091", 990: "0.0990", 1000: "0.1000", 8800: "0.8800", 9999: "0.9999", 10000: "1.0000"} {
		if got := PriceText(units); got != text {
			t.Errorf("PriceText(%d) = %q, want %q", units, got, text)
		}
		if got, err := ParsePrice4(text); err != nil || got != units {
			t.Errorf("ParsePrice4(%q) = %d, %v; want %d", text, got, err, units)
		}
	}
	for units := int64(1); units <= 9999; units++ {
		if got, err := ParsePrice4(PriceText(units)); err != nil || got != units {
			t.Fatalf("%d did not survive the round trip: %d, %v", units, got, err)
		}
	}
	// What the first two engines stored, and what numeric::text may hand back.
	for text, units := range map[string]int64{"0.92": 9200, "0.1": 1000, "1": 10000, "0.099000": 990} {
		if got, err := ParsePrice4(text); err != nil || got != units {
			t.Errorf("ParsePrice4(%q) = %d, %v; want %d", text, got, err, units)
		}
	}
	for _, text := range []string{"", ".5", "0.09905", "-0.1000", "1e-2", "0.1 ", "abc", "0.0x10"} {
		if got, err := ParsePrice4(text); err == nil {
			t.Errorf("ParsePrice4(%q) = %d, want an error", text, got)
		}
	}
}

func TestLotKey(t *testing.T) {
	if LotKey(31, "yes") != [2]string{"31", "yes"} || LotKey(31, "yes") == LotKey(31, "no") {
		t.Error("a lot is keyed by bucket and side")
	}
}

// The server said no, or this package refused the step before COMMIT was sent -> certainly rolled
// back. The client gave up, or the connection went -> unknown. A wrapped error is sorted by what
// is inside it.
func TestDefinitelyRolledBack(t *testing.T) {
	refused := &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint \"trade_order_client_order_id\""}
	localised := &pgconn.PgError{Severity: "FEHLER", SeverityUnlocalized: "ERROR", Code: "23514"}
	old := &pgconn.PgError{Severity: "ERROR", Code: "P0001"} // a server too old to send the unlocalised field
	shutdown := &pgconn.PgError{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01"}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	late, stop := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer stop()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"a constraint violation", refused, true},
		{"the same, wrapped twice", fmt.Errorf("order v3:1:2:3: %w", fmt.Errorf("insert: %w", refused)), true},
		{"a server that answers in German", localised, true},
		{"a server with no unlocalised severity", old, true},
		{"the server going away", shutdown, false},
		{"a cancelled context", cancelled.Err(), false},
		{"a deadline", fmt.Errorf("commit: %w", late.Err()), false},
		{"a connection error", errors.New("write tcp: broken pipe"), false},
		{"a deadline, wrapped twice", fmt.Errorf("order v3:1:2:3: %w", fmt.Errorf("insert: %w", context.DeadlineExceeded)), false},
		{"a step refused before it was sent", fmt.Errorf("%w: x", ErrInvalidStep), true},
		{"a step refused inside the transaction", fmt.Errorf("%w: x", ErrRefused), true},
		{"the same, wrapped again by a caller", fmt.Errorf("step: %w", fmt.Errorf("%w: x", ErrRefused)), true},
		{"text that only LOOKS like a refusal", errors.New(ErrRefused.Error()), false},
		{"no error", nil, false},
	}
	for _, c := range cases {
		if got := DefinitelyRolledBack(c.err); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Every statement in this file that reads by a list guards the list being empty in Go; this
// pins the memo format the ledger readers will see.
func TestMemoNamesTheOrderAndTheFill(t *testing.T) {
	plan, err := planStep(testSetup, testStep(OrderRow{DecisionIndex: -1, BucketID: 31, BucketLedgerID: 77, ClientID: "v3:31:71234:1",
		Action: "buy", Side: "yes", Qty: 200, Limit: "0.1300", Status: "partial",
		Fills: []FillRow{{161, "0.1200", 1932, 120}, {23, "0.1300", 299, 18}}}))
	if err != nil {
		t.Fatal(err)
	}
	if got := plan[0].Fills[0].Memo; got != "buy 161 yes @ 0.1200 (v3:31:71234:1 fill 1/2)" {
		t.Errorf("memo %q", got)
	}
	var paid int64
	for _, f := range plan[0].Fills {
		paid -= f.Bucket
	}
	if paid != 2369 { // plan 2.4 D
		t.Errorf("the bucket pays %d, want 2369", paid)
	}
	if !strings.HasSuffix(plan[0].Fills[1].Memo, "fill 2/2)") {
		t.Errorf("memo %q", plan[0].Fills[1].Memo)
	}
}

// ---------------------------------------------------------------------------------------------
// RecordOrders with no database: a stand-in transaction that remembers what it was sent and
// fails where a test tells it to. It proves how every way RecordOrders can end is SORTED; that
// the statements themselves are right is sim3_db_test.go's job.
// ---------------------------------------------------------------------------------------------

type fakeDB struct {
	beginErr error
	tx       *fakeTx
	begun    int
}

func (d *fakeDB) Begin(context.Context) (pgx.Tx, error) {
	d.begun++
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return d.tx, nil
}

// fakeTx answers the four calls recordOrders makes. Any other method of pgx.Tx is the nil
// interface below and panics, so a new kind of call cannot go unnoticed.
type fakeTx struct {
	pgx.Tx
	failOn     string // a statement containing this text fails with failErr
	failErr    error
	bucketRows int64 // rows the guarded insert of the bucket's entry reports
	commitErr  error

	sql                []string
	args               [][]any
	batches            int
	commits, rollbacks int
	nextID             int64
}

func newFakeTx() *fakeTx { return &fakeTx{bucketRows: 1} }

func (tx *fakeTx) sent(sql string, args []any) error {
	tx.sql, tx.args = append(tx.sql, sql), append(tx.args, args)
	if tx.failOn != "" && strings.Contains(sql, tx.failOn) {
		return tx.failErr
	}
	return nil
}

func (tx *fakeTx) count(text string) (n int) {
	for _, q := range tx.sql {
		if strings.Contains(q, text) {
			n++
		}
	}
	return n
}

func (tx *fakeTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	tx.nextID++
	return fakeRow{err: tx.sent(sql, args), id: tx.nextID}
}

func (tx *fakeTx) SendBatch(_ context.Context, b *pgx.Batch) pgx.BatchResults {
	tx.batches++
	br := &fakeBatch{}
	for _, q := range b.QueuedQueries {
		tag := "INSERT 0 1"
		if strings.Contains(q.SQL, "from bucket b") {
			tag = fmt.Sprintf("INSERT 0 %d", tx.bucketRows)
		}
		br.tags, br.errs = append(br.tags, tag), append(br.errs, tx.sent(q.SQL, q.Arguments))
	}
	return br
}

func (tx *fakeTx) Commit(context.Context) error   { tx.commits++; return tx.commitErr }
func (tx *fakeTx) Rollback(context.Context) error { tx.rollbacks++; return nil }

type fakeRow struct {
	err error
	id  int64
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int64) = r.id
	return nil
}

type fakeBatch struct {
	pgx.BatchResults
	tags []string
	errs []error
	next int
}

func (b *fakeBatch) Exec() (pgconn.CommandTag, error) {
	i := b.next
	b.next++
	return pgconn.NewCommandTag(b.tags[i]), b.errs[i]
}

func (b *fakeBatch) Close() error { return nil }

// Every way RecordOrders can end, and how the runner is told to take it. The two that matter
// most: a step refused INSIDE the transaction is certain (COMMIT was never sent), and nothing
// that comes back from COMMIT is certain unless the server itself said ERROR.
func TestRecordOrdersSortsEveryWayItCanEnd(t *testing.T) {
	duplicate := &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint \"trade_order_client_order_id\""}
	unbalanced := &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "P0001", Message: "transfer does not balance"}
	shutdown := &pgconn.PgError{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01"}
	brokenPipe := errors.New("write tcp 127.0.0.1:5432: broken pipe")
	invalid := bigSale()
	invalid.Status = "filled" // its fills say partial

	cases := []struct {
		name        string
		step        StepRecord3
		db          func(*fakeDB)
		certain     bool  // what DefinitelyRolledBack must answer
		is          error // a sentinel the error must wrap, or nil
		begun       int
		commits     int
		rollbacks   int
		wantNoError bool
	}{
		{name: "it commits", step: testStep(bigSale()), db: func(*fakeDB) {},
			wantNoError: true, begun: 1, commits: 1, rollbacks: 1},
		{name: "the step does not add up: no transaction is opened", step: testStep(invalid), db: func(*fakeDB) {},
			certain: true, is: ErrInvalidStep},
		{name: "Begin: the connection is gone", step: testStep(bigSale()), db: func(d *fakeDB) { d.beginErr = brokenPipe },
			begun: 1},
		{name: "Begin: the deadline passed", step: testStep(bigSale()), db: func(d *fakeDB) { d.beginErr = context.DeadlineExceeded },
			begun: 1},
		{name: "a statement: the server refuses a duplicate client id", step: testStep(bigSale()),
			db:      func(d *fakeDB) { d.tx.failOn, d.tx.failErr = "insert into trade_order", duplicate },
			certain: true, begun: 1, rollbacks: 1},
		{name: "a statement in the batch: the server refuses", step: testStep(bigSale()),
			db:      func(d *fakeDB) { d.tx.failOn, d.tx.failErr = "insert into fill", unbalanced },
			certain: true, begun: 1, rollbacks: 1},
		{name: "a statement: the deadline passed", step: testStep(bigSale()),
			db:    func(d *fakeDB) { d.tx.failOn, d.tx.failErr = "insert into ledger_transfer", context.DeadlineExceeded },
			begun: 1, rollbacks: 1},
		{name: "a statement in the batch: the connection is gone", step: testStep(bigSale()),
			db:    func(d *fakeDB) { d.tx.failOn, d.tx.failErr = "insert into ledger_entry", brokenPipe },
			begun: 1, rollbacks: 1},
		{name: "a statement: the server is going away", step: testStep(bigSale()),
			db:    func(d *fakeDB) { d.tx.failOn, d.tx.failErr = "insert into trade_order", shutdown },
			begun: 1, rollbacks: 1},
		{name: "the bucket does not own the ledger account: refused inside the transaction", step: testStep(bigSale()),
			db:      func(d *fakeDB) { d.tx.bucketRows = 0 },
			certain: true, is: ErrRefused, begun: 1, rollbacks: 1},
		{name: "COMMIT: the deadline passed", step: testStep(bigSale()), db: func(d *fakeDB) { d.tx.commitErr = context.DeadlineExceeded },
			begun: 1, commits: 1, rollbacks: 1},
		{name: "COMMIT: the connection is gone", step: testStep(bigSale()), db: func(d *fakeDB) { d.tx.commitErr = brokenPipe },
			begun: 1, commits: 1, rollbacks: 1},
		{name: "COMMIT: the server is going away", step: testStep(bigSale()), db: func(d *fakeDB) { d.tx.commitErr = shutdown },
			begun: 1, commits: 1, rollbacks: 1},
		{name: "COMMIT: the server answers ERROR (a deferred trigger)", step: testStep(bigSale()), db: func(d *fakeDB) { d.tx.commitErr = unbalanced },
			certain: true, begun: 1, commits: 1, rollbacks: 1},
	}
	for _, c := range cases {
		d := &fakeDB{tx: newFakeTx()}
		c.db(d)
		err := recordOrders(context.Background(), d, testSetup, c.step)
		if (err == nil) != c.wantNoError {
			t.Errorf("%s: error %v", c.name, err)
			continue
		}
		if got := DefinitelyRolledBack(err); got != c.certain {
			t.Errorf("%s: DefinitelyRolledBack(%v) = %v, want %v", c.name, err, got, c.certain)
		}
		for _, sentinel := range []error{ErrRefused, ErrInvalidStep} {
			if got, want := errors.Is(err, sentinel), c.is == sentinel; got != want {
				t.Errorf("%s: errors.Is(%v, %q) = %v, want %v", c.name, err, sentinel, got, want)
			}
		}
		if d.begun != c.begun || d.tx.commits != c.commits || d.tx.rollbacks != c.rollbacks {
			t.Errorf("%s: begun %d, commits %d, rollbacks %d; want %d, %d, %d", c.name,
				d.begun, d.tx.commits, d.tx.rollbacks, c.begun, c.commits, c.rollbacks)
		}
		// The rule ErrRefused rests on: whatever is certain without the server having said so
		// ended BEFORE COMMIT was sent.
		if (errors.Is(err, ErrRefused) || errors.Is(err, ErrInvalidStep)) && d.tx.commits != 0 {
			t.Errorf("%s: a refusal by this package after COMMIT was sent", c.name)
		}
	}
}

// The reasons below are the broker's own text (broker/paper.go), written out because the store
// does not import the broker.
const (
	reasonBadLimit     = "the limit is outside 0.0001..0.9999"
	reasonCrossed      = "the recorded book is crossed: the best yes bid and the best no bid add up to more than 1.0000"
	reasonNoBids       = "no bids on this side"
	reasonAlreadyTaken = "already taken by this bucket"
)

// A broker rejection is an outcome to journal, not an invalid step: one trade_order row with the
// status the broker gave, the reason in its detail, no fill and no transfer. The runner journals
// it and carries on; it has nothing to suspend for.
func TestAnOrderThatFilledNothingIsJournaledWhateverTheReason(t *testing.T) {
	cases := []struct {
		name, status, limit, reason string
		wantLimit                   any // what is sent for limit_price: the text, or nil for SQL null
	}{
		{"a bad limit, rejected", "rejected", "0.0000", reasonBadLimit, nil},
		{"a limit of a dollar, rejected", "rejected", "1.0000", reasonBadLimit, nil},
		{"a negative limit, rejected", "rejected", PriceText(-5), reasonBadLimit, nil},
		{"a bad limit, journaled as cancelled", "cancelled", "0.0000", reasonBadLimit, nil},
		{"a crossed book", "rejected", "0.0900", reasonCrossed, "0.0900"},
		{"a crossed book, journaled as cancelled", "cancelled", "0.0900", reasonCrossed, "0.0900"},
		{"an empty side", "cancelled", "0.0900", reasonNoBids, "0.0900"},
		{"held in full", "cancelled", "0.0900", reasonAlreadyTaken, "0.0900"},
	}
	for _, c := range cases {
		o := OrderRow{DecisionIndex: 0, BucketID: 31, BucketLedgerID: 77, ClientID: "v3:31:71830:1", Action: "sell", Side: "yes",
			Qty: 9, Limit: c.limit, Status: c.status, Detail: map[string]any{"reason": c.reason, "model": "paper-1", "unfilled": 9}}
		step := testStep(o)
		step.Decisions = []DecisionRow{{BucketID: 31, VersionID: 19, Action: "exit"}}
		d := &fakeDB{tx: newFakeTx()}
		if err := recordOrders(context.Background(), d, testSetup, step); err != nil {
			t.Errorf("%s: %v; want it journaled", c.name, err)
			continue
		}
		tx := d.tx
		if tx.count("insert into decision") != 1 || tx.count("insert into trade_order") != 1 || tx.commits != 1 {
			t.Errorf("%s: %d decisions, %d orders, %d commits; want one of each", c.name, tx.count("insert into decision"), tx.count("insert into trade_order"), tx.commits)
		}
		if n := tx.count("ledger_transfer") + tx.count("ledger_entry") + tx.count("insert into fill") + tx.batches; n != 0 {
			t.Errorf("%s: %d statements touched the ledger or the fills; an order that filled nothing moves no money", c.name, n)
		}
		// The trade_order insert's arguments, in the statement's order: $7 qty, $8 limit, $9 status, $11 detail.
		args := tx.args[1]
		if qty, status := args[6], args[8]; qty != 9 || status != c.status {
			t.Errorf("%s: qty %v, status %v", c.name, qty, status)
		}
		limit, _ := args[7].(*string)
		switch {
		case c.wantLimit == nil && limit != nil:
			t.Errorf("%s: limit_price %q, want null", c.name, *limit)
		case c.wantLimit != nil && (limit == nil || *limit != c.wantLimit):
			t.Errorf("%s: limit_price %v, want %v", c.name, limit, c.wantLimit)
		}
		var detail map[string]any
		if err := json.Unmarshal(args[10].([]byte), &detail); err != nil {
			t.Fatalf("%s: detail %s: %v", c.name, args[10], err)
		}
		if detail["reason"] != c.reason || detail["model"] != "paper-1" {
			t.Errorf("%s: detail %v lost the broker's reason", c.name, detail)
		}
		if kept, has := detail[limitNotStoredKey]; has != (c.wantLimit == nil) || (has && kept != c.limit) {
			t.Errorf("%s: detail[%s] = %v, %v; a limit that is not stored as a price is kept as it was sent", c.name, limitNotStoredKey, kept, has)
		}
	}
}

// The review's own case: two accounts act in one step. One buys and fills; the other's exit
// carries a limit of 0 because of an engine bug, and the broker rejects it. The good fill is
// booked and the rejection is journaled beside it; before, the whole step was refused.
func TestARejectionDoesNotTakeAGoodFillDownWithIt(t *testing.T) {
	buy := OrderRow{DecisionIndex: -1, BucketID: 31, BucketLedgerID: 77, ClientID: "v3:31:71234:1", Action: "buy", Side: "yes",
		Qty: 58, Limit: "0.1200", Status: "filled", Fills: []FillRow{{58, "0.1200", 696, 43}}}
	exit := OrderRow{DecisionIndex: -1, BucketID: 32, BucketLedgerID: 78, ClientID: "v3:32:71234:1", Action: "sell", Side: "yes",
		Qty: 9, Limit: PriceText(0), Status: "rejected", Detail: map[string]any{"reason": reasonBadLimit}}
	d := &fakeDB{tx: newFakeTx()}
	if err := recordOrders(context.Background(), d, testSetup, testStep(buy, exit)); err != nil {
		t.Fatal(err)
	}
	if o, tr, f := d.tx.count("insert into trade_order"), d.tx.count("insert into ledger_transfer"), d.tx.count("insert into fill"); o != 2 || tr != 1 || f != 1 || d.tx.commits != 1 {
		t.Errorf("%d orders, %d transfers, %d fills, %d commits; want 2, 1, 1, 1", o, tr, f, d.tx.commits)
	}
}

// What is still refused, and why it no longer suspends anyone: an order WITH a fill keeps the
// strict limit, and a rejection the table cannot hold stays ErrInvalidStep, which
// DefinitelyRolledBack now sorts as certain.
func TestWhatARejectionStillCannotBe(t *testing.T) {
	rejected := func(f func(*OrderRow)) StepRecord3 {
		o := OrderRow{DecisionIndex: -1, BucketID: 31, BucketLedgerID: 77, ClientID: "v3:31:71830:1", Action: "sell", Side: "yes",
			Qty: 9, Limit: "0.0900", Status: "rejected", Detail: map[string]any{"reason": "x"}}
		f(&o)
		return testStep(o)
	}
	filledAtABadLimit := bigSale()
	filledAtABadLimit.Limit = "0.0000"
	cases := map[string]StepRecord3{
		"a fill under a limit that is not a price":   testStep(filledAtABadLimit),
		"no contracts (trade_order.qty must be > 0)": rejected(func(o *OrderRow) { o.Qty = 0 }),
		"no client id":                                 rejected(func(o *OrderRow) { o.ClientID = "" }),
		"an action the table's check refuses":          rejected(func(o *OrderRow) { o.Action = "short" }),
		"a detail that cannot be written as JSON":      rejected(func(o *OrderRow) { o.Detail = map[string]any{"f": func() {}} }),
		"the same, with a limit that is not a price":   rejected(func(o *OrderRow) { o.Limit, o.Detail = "0.0000", map[string]any{"f": func() {}} }),
		"a rejected order that nevertheless has fills": rejected(func(o *OrderRow) { o.Fills = []FillRow{{9, "0.1000", 90, 6}} }),
	}
	for name, r := range cases {
		d := &fakeDB{tx: newFakeTx()}
		err := recordOrders(context.Background(), d, testSetup, r)
		if !errors.Is(err, ErrInvalidStep) || !DefinitelyRolledBack(err) || d.begun != 0 {
			t.Errorf("%s: %v (transactions begun: %d); want ErrInvalidStep, certain, nothing sent", name, err, d.begun)
		}
	}
}

func TestWithLimitNotStoredKeepsTheDetailAsItWas(t *testing.T) {
	cases := []struct{ name, detail, want string }{
		{"no detail", `{}`, `{"limit_not_stored":"0.0000"}`},
		{"an object: every key kept, a big number not rounded", `{"reason":"x","n":12345678901234567890}`, `{"limit_not_stored":"0.0000","n":12345678901234567890,"reason":"x"}`},
		{"a JSON null", `null`, `{"detail":null,"limit_not_stored":"0.0000"}`},
		{"a list", `["a",1]`, `{"detail":["a",1],"limit_not_stored":"0.0000"}`},
		{"a plain string", `"why"`, `{"detail":"why","limit_not_stored":"0.0000"}`},
	}
	for _, c := range cases {
		got, err := withLimitNotStored([]byte(c.detail), "0.0000")
		if err != nil || string(got) != c.want {
			t.Errorf("%s: %s, %v; want %s", c.name, got, err, c.want)
		}
	}
	// A limit that is not even a number is kept as text, quoted.
	if got, err := withLimitNotStored([]byte(`{}`), "a \"limit\""); err != nil || string(got) != `{"limit_not_stored":"a \"limit\""}` {
		t.Errorf("an unreadable limit: %s, %v", got, err)
	}
}
