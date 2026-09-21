package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file is the THIRD engine's recorder and the reads its runner rebuilds itself from. Like
// sim.go, everything it writes is SIMULATED money: every account it touches has mode 'sim', and
// the database refuses a transfer that would mix sim with real. Nothing here can place an order.
//
// Why it is a file of its own and RecordStep is untouched: the first two engines book one order
// as one fill, always in full. The third engine's orders go through the paper broker
// (service/internal/broker), which fills only what the recorded book displayed. So an order here
// may be filled at several price levels, partly, or not at all, and each level is a fill row
// with a ledger transfer of its own (RealisedByCoin joins a fill to its transfer, so two fills
// may never share one).
//
// Money is whole cents in int64 and a price is text with four decimals ("0.0990"), made from the
// broker's integer price units by PriceText. No float touches a money path in this file.
//
// Design: docs/honest-fills-v3.md, sections 3, 5.3 and 5.4. Every SQL statement here was written
// on a machine with no database and HAS NOT BEEN RUN (2026-09-21); sim3_db_test.go runs them
// against a dev database when AC_TEST_DB_URL is set, and db/tests/0003_client_order_id.sql proves
// the shapes they lean on.

// FillRow is one price level's worth of an order, as the broker reported it.
type FillRow struct {
	Qty          int    // whole contracts, >= 1
	Price        string // the taker's price per contract, four decimals: "0.0990"
	PremiumCents int64  // price x qty in whole cents, already rounded against the bucket by the broker
	FeeCents     int64  // this fill's share of the order's fee
}

// OrderRow is one order the engine sent and what became of it. An order that filled nothing is
// still a row: a failed exit is evidence, and so is a rejection. For such an order Detail should
// carry the broker's reason, and Limit is written as it was sent even when it is not a price
// (planStep says how).
type OrderRow struct {
	DecisionIndex  int // index into StepRecord3.Decisions, or -1
	BucketID       int64
	BucketLedgerID int64  // checked against the bucket row when the fill is written
	ClientID       string // the engine's own id for the order, unique for ever
	Action         string // buy | sell
	Side           string // yes | no
	Qty            int    // contracts REQUESTED; what filled is the sum of Fills
	Limit          string // the true limit, four decimals
	Status         string // filled | partial | cancelled | rejected
	Detail         any
	Fills          []FillRow
}

// StepRecord3 is everything the third engine made of one look at one market. The evaluation row
// already exists (the sink inserts it before any engine steps), so EvaluationID is required, and
// At must be that evaluation's own timestamp: decision rows point at (evaluation id, at).
type StepRecord3 struct {
	EvaluationID int64
	At           time.Time
	MarketID     int64
	Decisions    []DecisionRow
	Orders       []OrderRow
}

// ErrInvalidStep is returned, before anything is sent to the database, for a step that does not
// add up: a status that contradicts its fills, a fill worth nothing, a price that is not four
// decimals. Nothing was written, and that is certain: planStep returns it before a transaction
// is opened, so DefinitelyRolledBack answers TRUE for it. The runner counts it as a refused
// write (Void, failures++) and never suspends on it: a rebuild cannot cure a step the engine
// will form again.
//
// A broker REJECTION is not an invalid step. An order that filled nothing is an outcome to
// journal, whatever the broker's reason (see planStep for the three kinds that cannot be a row).
var ErrInvalidStep = errors.New("store: the step does not add up; nothing was written")

// ErrRefused is returned when this package itself refuses a step INSIDE its transaction: today,
// only when the guarded insert of a bucket's ledger entry writes no row, because the bucket does
// not own the ledger account the runner carried. It is not a *pgconn.PgError (the server
// answered every statement without an error), but the outcome is just as certain:
// RecordOrders returns it without ever calling tx.Commit, and a server cannot commit a
// transaction it was never sent COMMIT for. The deferred Rollback, or failing that the closed
// connection, discards what the transaction had written. DefinitelyRolledBack answers TRUE.
//
// It is wrapped ONLY on a path that ends before tx.Commit. An error returned by tx.Commit
// itself, a context deadline, a cancelled context and any connection error are never wrapped in
// it, whatever this package knows about them: those stay "outcome unknown" (plan 5.2).
var ErrRefused = errors.New("store: the step was refused inside its transaction, before COMMIT was sent; nothing was written")

// VersionRow is one registered strategy version, as the runner needs it.
type VersionRow struct {
	Name   string // the strategy's name, e.g. "Scalper"
	ID     int64  // strategy_version.id
	Status string
	Params json.RawMessage
}

// HeldBucket is a bucket the third engine holds, with what the runner needs to run it. It is
// SimBucket plus three things SimBucket has no place for (sim.go is not changed for this).
type HeldBucket struct {
	SimBucket            // CashCents is the ledger sum at the moment of the read
	Strategy      string // the strategy's name, e.g. "Scalper"
	VersionStatus string // draft | probation | bench | active | retired, as read
	Params        json.RawMessage
}

// BucketFill is one recorded fill with everything the rebuild folds it with (plan 5.4).
type BucketFill struct {
	BucketID    int64
	MarketID    int64
	Ticker      string
	Underlying  string    // the coin, e.g. "DOGE"
	ClosesAt    time.Time // zero if the market row has no close
	Strike      *float64  // nil if the market row has none; a model input, not money
	Result      string    // "" while the market has no result
	OrderID     int64
	ClientID    string // "" for an order that has none
	Action      string
	Side        string
	Detail      json.RawMessage
	FillID      int64
	At          time.Time
	Qty         int
	Price       string // as stored, e.g. "0.0990"; ParsePrice4 turns it into broker price units
	FeeCents    int64
	BucketCents int64 // the bucket's entry on this fill's transfer; 0 when the fill has none
}

// querier is what a read needs: the pool in the service, a transaction in the database test, so
// that the test can read back rows it never commits.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DefinitelyRolledBack sorts a write's error into the two kinds the runner acts on. True means
// the transaction CERTAINLY did not commit, so memory still equals the ledger: either the SERVER
// answered with an error, or this package refused the step before COMMIT was sent (ErrRefused,
// inside the transaction; ErrInvalidStep, before one was opened). False means the outcome is
// unknown: a context deadline, a cancelled context, or any connection error is the CLIENT giving
// up, and the server may still commit a moment later. That holds for every error tx.Commit
// returns that is not the server's own ERROR answer.
//
// Why the two sentinels belong here: sorted as "unknown", a step this package refuses would
// make the runner suspend, rebuild, find nothing wrong, form the same step and suspend again,
// for ever, refusing everyone's value snapshots each time. Sorted as "refused" it is counted,
// and the third in a row pauses the engine with its book still true (plan 5.4).
//
// It is narrower than "any *pgconn.PgError" on purpose: only severity ERROR counts. FATAL and
// PANIC arrive as the connection is being torn down (an administrator's shutdown, a crashed
// backend), and what became of a COMMIT in flight at that moment was not checked against a
// server, so they are left as unknown. Being wrong in that direction costs one rebuild; being
// wrong in the other would leave memory behind the ledger with no flag. [INFERRED from the
// protocol, not tested against a server]
func DefinitelyRolledBack(err error) bool { return definitelyRolledBack(err) }

func definitelyRolledBack(err error) bool {
	if errors.Is(err, ErrRefused) || errors.Is(err, ErrInvalidStep) {
		return true // this package's own refusals: COMMIT was never sent
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	severity := pgErr.SeverityUnlocalized // what the server calls it whatever its language; 9.6 and later
	if severity == "" {
		severity = pgErr.Severity
	}
	return severity == "ERROR"
}

// PriceText writes a price in ten-thousandths of a dollar (the broker's unit) as the four-decimal
// text the database stores: 990 -> "0.0990". Integer arithmetic only. Negative is refused by
// ParsePrice4 on the way back, so it is written as it is, visibly wrong, not hidden.
func PriceText(units int64) string {
	if units < 0 {
		return fmt.Sprintf("-%d.%04d", -units/10000, -units%10000)
	}
	return fmt.Sprintf("%d.%04d", units/10000, units%10000)
}

// ParsePrice4 reads a stored price back into ten-thousandths of a dollar, exactly. It accepts
// fewer than four decimals ("0.92", as the first two engines wrote) and trailing zeros beyond
// four, and refuses anything finer than a ten-thousandth: a price this code cannot hold exactly
// must not be rounded quietly.
func ParsePrice4(s string) (int64, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || strings.ContainsAny(s, "+-eE ") {
		return 0, fmt.Errorf("price %q is not a plain decimal", s)
	}
	if len(frac) > 4 {
		if strings.Trim(frac[4:], "0") != "" {
			return 0, fmt.Errorf("price %q is finer than a ten-thousandth", s)
		}
		frac = frac[:4]
	}
	frac += strings.Repeat("0", 4-len(frac))
	w, err := strconv.ParseInt(whole, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("price %q: %w", s, err)
	}
	f, err := strconv.ParseInt(frac, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("price %q: %w", s, err)
	}
	return w*10000 + f, nil
}

// tradablePrice checks a price this file is about to WRITE: exactly four decimals, and strictly
// between 0 and 1 dollar, which is every price a binary contract can trade at.
func tradablePrice(s string) error {
	if i := strings.IndexByte(s, '.'); i < 0 || len(s)-i-1 != 4 {
		return fmt.Errorf("price %q is not written with four decimals", s)
	}
	units, err := ParsePrice4(s)
	if err != nil {
		return err
	}
	if units < 1 || units > 9999 {
		return fmt.Errorf("price %q is outside 0.0001..0.9999", s)
	}
	return nil
}

// StatusFromFills is what an order's status must be, given what it asked for and what filled:
// everything -> filled, something -> partial, nothing -> cancelled. ("rejected" also has no
// fills; it means the order never reached a book, which only the broker can say.)
func StatusFromFills(requested int, fills []FillRow) (string, error) {
	if requested < 1 {
		return "", fmt.Errorf("an order for %d contracts", requested)
	}
	filled := 0
	for _, f := range fills {
		if f.Qty < 1 {
			return "", fmt.Errorf("a fill of %d contracts", f.Qty)
		}
		filled += f.Qty
	}
	switch {
	case filled > requested:
		return "", fmt.Errorf("filled %d of %d requested", filled, requested)
	case filled == requested:
		return "filled", nil
	case filled > 0:
		return "partial", nil
	}
	return "cancelled", nil
}

// fillCents is the three ledger amounts of one fill, in cents, positive INTO the account. They
// always sum to zero. The same shape as RecordStep's, so every ledger reader sees the third
// engine as it sees the second:
//
//	buy:  bucket -(premium + fee), venue +premium, fees +fee
//	sell: bucket +(premium - fee), venue -premium, fees +fee
//
// The bucket's amount equals broker.BucketCents for the same fill, which is what the engine
// applies to its memory (tested in the broker's own package for that function, and here for
// this one; the store does not import the broker).
func fillCents(action string, f FillRow) (bucket, venue, fees int64) {
	if action == "buy" {
		return -(f.PremiumCents + f.FeeCents), f.PremiumCents, f.FeeCents
	}
	return f.PremiumCents - f.FeeCents, -f.PremiumCents, f.FeeCents
}

// plannedFill is one fill ready to write: its memo and its three amounts.
type plannedFill struct {
	Row                 FillRow
	Memo                string
	Bucket, Venue, Fees int64
}

// plannedOrder is one order ready to write.
type plannedOrder struct {
	Row    OrderRow
	Limit  *string // what goes into limit_price; nil (SQL null) when Row.Limit is not a tradable price
	Detail []byte
	Fills  []plannedFill
}

// limitNotStoredKey is where an order's limit is kept when limit_price cannot hold it.
const limitNotStoredKey = "limit_not_stored"

// withLimitNotStored adds the limit the order really carried to its detail, for an order whose
// limit_price is written as null. Every other key is kept byte for byte. A detail that is not a
// JSON object (null, a list, a plain value) is kept whole under "detail".
func withLimitNotStored(detail []byte, limit string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(detail, &obj); err != nil || obj == nil {
		obj = map[string]json.RawMessage{"detail": detail}
	}
	raw, err := json.Marshal(limit)
	if err != nil {
		return nil, err
	}
	obj[limitNotStoredKey] = raw
	return json.Marshal(obj)
}

// planStep checks a whole step and builds every row BEFORE a transaction is opened, so that a
// step that does not add up costs the database nothing and is reported, not half-written.
//
// An order that filled NOTHING is an outcome to journal, not a step to refuse: status
// 'cancelled' (it reached a book: no bids on that side, the best price outside the limit, all of
// it already held by this bucket) or 'rejected' (it never reached one: a bad limit, a crossed or
// unreadable book, no book, a stale one, a closed market). It becomes one trade_order row with
// no fill and no transfer, and the broker's reason travels in Detail. Its limit is evidence, not
// a price anything traded at, so it is not held to 0.0001..0.9999: a limit that is not a tradable
// price is written as a NULL limit_price and kept, as it was sent, in detail under
// "limit_not_stored". An order WITH a fill keeps the strict check.
//
// Three kinds of rejection still cannot be a row, and are refused here as ErrInvalidStep: no
// client id (the row could never be recognised again), an action or side that is not buy/sell of
// yes/no (trade_order.action has a check), and fewer than 1 contract (trade_order.qty has
// check (qty > 0)). The runner must journal those as a decision with blocked_by, or log them;
// it must not pass them here.
func planStep(setup SimSetup, r StepRecord3) ([]plannedOrder, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidStep, fmt.Sprintf(format, args...))
	}
	if r.EvaluationID == 0 || r.MarketID == 0 || r.At.IsZero() {
		return nil, bad("evaluation %d, market %d, at %v: all three are required", r.EvaluationID, r.MarketID, r.At)
	}
	if len(r.Decisions) == 0 && len(r.Orders) == 0 {
		// Not a silent success: the runner's write-probe must never take "nothing was sent" for
		// "the database accepted a write".
		return nil, bad("nothing to record")
	}
	if len(r.Orders) > 0 && (setup.ActorID == 0 || setup.VenueLedgerID == 0 || setup.FeesLedgerID == 0) {
		return nil, bad("the sim setup is empty (actor %d, venue %d, fees %d)", setup.ActorID, setup.VenueLedgerID, setup.FeesLedgerID)
	}
	seen := map[string]bool{}
	out := make([]plannedOrder, 0, len(r.Orders))
	for _, o := range r.Orders {
		switch {
		case o.ClientID == "":
			return nil, bad("an order with no client id")
		case seen[o.ClientID]:
			return nil, bad("client id %s twice in one step", o.ClientID)
		case o.Action != "buy" && o.Action != "sell":
			return nil, bad("%s: action %q", o.ClientID, o.Action)
		case o.Side != "yes" && o.Side != "no":
			return nil, bad("%s: side %q", o.ClientID, o.Side)
		case o.BucketID == 0 || o.BucketLedgerID == 0:
			return nil, bad("%s: bucket %d, ledger account %d", o.ClientID, o.BucketID, o.BucketLedgerID)
		case o.DecisionIndex < -1 || o.DecisionIndex >= len(r.Decisions):
			return nil, bad("%s: decision index %d of %d", o.ClientID, o.DecisionIndex, len(r.Decisions))
		}
		seen[o.ClientID] = true
		want, err := StatusFromFills(o.Qty, o.Fills)
		if err != nil {
			return nil, bad("%s: %v", o.ClientID, err)
		}
		if o.Status != want && !(o.Status == "rejected" && want == "cancelled") {
			return nil, bad("%s: status %q, but its fills say %q", o.ClientID, o.Status, want)
		}
		limit := o.Limit // a copy of its own: the pointer below must not follow the loop variable
		p := plannedOrder{Row: o, Limit: &limit, Detail: []byte(`{}`)}
		if o.Detail != nil {
			if p.Detail, err = json.Marshal(o.Detail); err != nil {
				return nil, bad("%s: detail: %v", o.ClientID, err)
			}
		}
		if err := tradablePrice(o.Limit); err != nil {
			if len(o.Fills) > 0 {
				return nil, bad("%s: limit: %v", o.ClientID, err)
			}
			// Nothing filled, so no money rests on this limit: journal the order as it was.
			p.Limit = nil
			if p.Detail, err = withLimitNotStored(p.Detail, o.Limit); err != nil {
				return nil, bad("%s: detail: %v", o.ClientID, err)
			}
		}
		for i, f := range o.Fills {
			if err := tradablePrice(f.Price); err != nil {
				return nil, bad("%s fill %d: %v", o.ClientID, i+1, err)
			}
			if f.PremiumCents < 0 || f.FeeCents < 0 {
				return nil, bad("%s fill %d: premium %d, fee %d", o.ClientID, i+1, f.PremiumCents, f.FeeCents)
			}
			if f.PremiumCents == 0 && f.FeeCents == 0 {
				// A fill row must point at a transfer and an entry may not be zero, so a fill
				// worth nothing cannot be booked. The broker never makes one (plan 2.3).
				return nil, bad("%s fill %d: neither premium nor fee", o.ClientID, i+1)
			}
			pf := plannedFill{Row: f, Memo: fmt.Sprintf("%s %d %s @ %s (%s fill %d/%d)", o.Action, f.Qty, o.Side, f.Price, o.ClientID, i+1, len(o.Fills))}
			pf.Bucket, pf.Venue, pf.Fees = fillCents(o.Action, f)
			p.Fills = append(p.Fills, pf)
		}
		out = append(out, p)
	}
	return out, nil
}

// RecordOrders journals a step and books its fills, all or nothing, in ONE transaction: the
// decision rows (the same insert as RecordStep; size_alone is the contracts requested), then per
// order one trade_order row with its client id and true status, then per fill exactly one ledger
// transfer, its entries, and one fill row pointing at that transfer.
//
// A client id that already exists is refused by the unique index of migration 0012, and the
// whole step rolls back with it: a step is never written twice.
//
// An order that filled nothing, cancelled or rejected, is journaled like any other: one
// trade_order row, no fill, no transfer, the broker's reason in its detail (see planStep).
//
// On an error, ask DefinitelyRolledBack what kind it was. This function never retries. The kinds:
//
//	ErrInvalidStep            the step does not add up; nothing was sent          -> certain
//	ErrRefused                refused inside the transaction; COMMIT never sent   -> certain
//	*pgconn.PgError, ERROR    the server said no, to a statement or to COMMIT     -> certain
//	anything else             a deadline, a cancelled context, a connection error,
//	                          at Begin, at any statement or AT COMMIT             -> unknown
func (s *Store) RecordOrders(ctx context.Context, setup SimSetup, r StepRecord3) error {
	return recordOrders(ctx, s.pool, setup, r)
}

// txBeginner is the one thing RecordOrders needs of the pool, so that its error paths can be
// tested with no database.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

func recordOrders(ctx context.Context, db txBeginner, setup SimSetup, r StepRecord3) error {
	plan, err := planStep(setup, r)
	if err != nil {
		return err // ErrInvalidStep: no transaction was opened
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err // as it is: a connection error or the context, which the plan sorts as unknown
	}
	defer tx.Rollback(ctx)
	if err := recordOrdersTx(ctx, tx, setup, r, plan); err != nil {
		return err // ErrRefused or the server's or the connection's own error; COMMIT is not sent
	}
	// Commit's error is returned exactly as pgx gave it and is NEVER wrapped in ErrRefused: once
	// COMMIT is on the wire only the server's own ERROR answer proves a rollback.
	return tx.Commit(ctx)
}

// recordOrdersTx is RecordOrders inside a transaction the caller owns, so the database test can
// look at the rows and then roll them back.
func recordOrdersTx(ctx context.Context, tx pgx.Tx, setup SimSetup, r StepRecord3, plan []plannedOrder) error {
	decisionIDs := make([]int64, len(r.Decisions))
	for i, d := range r.Decisions {
		// The same statement as RecordStep's. Human weights are not applied yet, so the size
		// after weighting is the strategy's own.
		if err := tx.QueryRow(ctx, `insert into decision (at, evaluation_id, bucket_id, strategy_version_id, model_prob,
		            market_prob, side, edge, action, blocked_by, reason, size_alone, human_weight, size_applied)
		        values ($1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, ''), $11, $12, 1, $12) returning id`,
			r.At, r.EvaluationID, d.BucketID, d.VersionID, d.ModelProb, d.MarketProb, d.Side, d.Edge, d.Action,
			d.BlockedBy, d.Why, d.SizeAlone).Scan(&decisionIDs[i]); err != nil {
			return fmt.Errorf("decision: %w", err)
		}
	}
	for _, p := range plan {
		o := p.Row
		var decisionID *int64
		var decisionAt *time.Time
		if o.DecisionIndex >= 0 {
			decisionID, decisionAt = &decisionIDs[o.DecisionIndex], &r.At
		}
		var orderID int64
		if err := tx.QueryRow(ctx, `insert into trade_order (bucket_id, market_id, decision_id, decision_at, broker, action, side,
		            qty, limit_price, status, placed_at, detail, client_order_id)
		        values ($1, $2, $3, $4, 'paper', $5, $6, $7, $8::text::numeric, $9, $10, $11, $12) returning id`,
			o.BucketID, r.MarketID, decisionID, decisionAt, o.Action, o.Side, o.Qty, p.Limit, o.Status, r.At, p.Detail, o.ClientID).Scan(&orderID); err != nil {
			return fmt.Errorf("order %s: %w", o.ClientID, err)
		}
		for _, f := range p.Fills {
			var transferID int64
			if err := tx.QueryRow(ctx, `insert into ledger_transfer (at, mode, reason, memo, created_by)
			                            values ($1, 'sim', 'fill', $2, $3) returning id`, r.At, f.Memo, setup.ActorID).Scan(&transferID); err != nil {
				return fmt.Errorf("transfer for %s: %w", o.ClientID, err)
			}
			// The entries and the fill row travel together: one round trip per fill, which is
			// what keeps a five-level order inside the runner's two seconds on the Pi.
			batch := &pgx.Batch{}
			var wantOne []string // per queued statement: "" or what it is, when it must write exactly one row
			if f.Bucket != 0 {
				// The bucket's cash account is taken from the bucket row and must equal the id
				// the runner carried: if they ever differ, no row is written and the step is
				// refused, where a plain insert would have moved another bucket's cash.
				batch.Queue(`insert into ledger_entry (transfer_id, account_id, mode, amount_cents)
				             select $1::bigint, b.ledger_account_id, 'sim', $3::bigint
				               from bucket b where b.id = $2 and b.ledger_account_id = $4 and b.mode = 'sim'`,
					transferID, o.BucketID, f.Bucket, o.BucketLedgerID)
				wantOne = append(wantOne, "the bucket's entry")
			}
			for _, leg := range [][2]int64{{setup.VenueLedgerID, f.Venue}, {setup.FeesLedgerID, f.Fees}} {
				if leg[1] != 0 {
					batch.Queue(`insert into ledger_entry (transfer_id, account_id, mode, amount_cents) values ($1, $2, 'sim', $3)`, transferID, leg[0], leg[1])
					wantOne = append(wantOne, "")
				}
			}
			batch.Queue(`insert into fill (order_id, at, qty, price, fee_cents, transfer_id) values ($1, $2, $3, $4::text::numeric, $5, $6)`,
				orderID, r.At, f.Row.Qty, f.Row.Price, f.Row.FeeCents, transferID)
			wantOne = append(wantOne, "")
			br := tx.SendBatch(ctx, batch)
			for _, what := range wantOne {
				tag, err := br.Exec()
				if err != nil {
					br.Close()
					return fmt.Errorf("fill of %s: %w", o.ClientID, err)
				}
				if what != "" && tag.RowsAffected() != 1 {
					br.Close()
					// Not a server error, and still certain: the caller returns without COMMIT.
					return fmt.Errorf("%w: fill of %s: %s was not written: bucket %d does not own sim ledger account %d", ErrRefused, o.ClientID, what, o.BucketID, o.BucketLedgerID)
				}
			}
			if err := br.Close(); err != nil {
				return fmt.Errorf("fill of %s: %w", o.ClientID, err)
			}
		}
	}
	return nil
}

// OrdersRecorded says which client ids have a trade_order row; an id that has none is simply
// absent from the map. The rebuild uses it to REPORT whether the step that suspended it had
// committed. Step never decides anything by it: asked straight after a timeout it can answer
// "none" about a commit that lands a moment later (plan 5.2).
func (s *Store) OrdersRecorded(ctx context.Context, clientIDs []string) (map[string]bool, error) {
	return ordersRecorded(ctx, s.pool, clientIDs)
}

func ordersRecorded(ctx context.Context, q querier, clientIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(clientIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `select client_order_id from trade_order where client_order_id = any($1::text[])`, clientIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// LotKey is the key of the maps OpenQty and SettlementsRecorded return: one bucket's holding on
// one side of a market.
func LotKey(bucketID int64, side string) [2]string {
	return [2]string{strconv.FormatInt(bucketID, 10), side}
}

// SettlementsRecorded says which (bucket, side) of a market already have a settlement row: the
// same question as OrdersRecorded, for a payout. Here the answer MAY be acted on, because a
// settlement is idempotent by unique (market_id, bucket_id, side): a late commit makes the retry
// find the rows (plan 5.3). Keys are LotKey.
func (s *Store) SettlementsRecorded(ctx context.Context, marketID int64, bucketIDs []int64) (map[[2]string]bool, error) {
	out := map[[2]string]bool{}
	if len(bucketIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `select bucket_id, side from settlement where market_id = $1 and bucket_id = any($2::bigint[])`, marketID, bucketIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket int64
		var side string
		if err := rows.Scan(&bucket, &side); err != nil {
			return nil, err
		}
		out[LotKey(bucket, side)] = true
	}
	return out, rows.Err()
}

// TradableVersions is the versions of a family a person has approved: status probation or
// active. The runner calls it ONCE, at start; /api/status calls it again only to show "approved,
// waiting for restart". It creates nothing.
func (s *Store) TradableVersions(ctx context.Context, family string, version int) ([]VersionRow, error) {
	rows, err := s.pool.Query(ctx, `
		select st.name, v.id, v.status, v.params
		  from strategy_version v join strategy st on st.id = v.strategy_id
		 where st.family = $1 and v.version = $2 and v.status in ('probation', 'active')
		 order by st.name`, family, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionRow{}
	for rows.Next() {
		var v VersionRow
		var params []byte
		if err := rows.Scan(&v.Name, &v.ID, &v.Status, &params); err != nil {
			return nil, err
		}
		v.Params = params
		out = append(out, v)
	}
	return out, rows.Err()
}

// HeldBuckets is every sim bucket of that family and version that is not frozen, WHATEVER its
// version's status: what the third engine holds comes from here, so a bucket whose version was
// benched or retired with a bet open is still valued and settled. It creates nothing.
func (s *Store) HeldBuckets(ctx context.Context, family string, version int) ([]HeldBucket, error) {
	rows, err := s.pool.Query(ctx, `
		select b.id, b.name, b.ledger_account_id, b.strategy_version_id, st.name, v.status, v.params,
		       coalesce((select sum(e.amount_cents) from ledger_entry e where e.account_id = b.ledger_account_id), 0)::bigint
		  from bucket b
		  join strategy_version v on v.id = b.strategy_version_id
		  join strategy st        on st.id = v.strategy_id
		 where b.mode = 'sim' and b.status <> 'frozen' and st.family = $1 and v.version = $2
		 order by b.id`, family, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HeldBucket{}
	for rows.Next() {
		var h HeldBucket
		var params []byte
		if err := rows.Scan(&h.ID, &h.Name, &h.LedgerAccountID, &h.VersionID, &h.Strategy, &h.VersionStatus, &params, &h.CashCents); err != nil {
			return nil, err
		}
		h.Params = params
		out = append(out, h)
	}
	return out, rows.Err()
}

// BucketCash is each bucket's cash as the ledger has it: the sum of the entries on its own
// account. For the third engine this IS the cash; there is no saved figure to disagree with.
// A bucket id that does not exist is an error, not zero cents: a cash check that compared
// memory with a made-up zero would "find" a difference that is not one.
func (s *Store) BucketCash(ctx context.Context, bucketIDs []int64) (map[int64]int64, error) {
	return bucketCash(ctx, s.pool, bucketIDs)
}

func bucketCash(ctx context.Context, q querier, bucketIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(bucketIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		select b.id, coalesce(sum(e.amount_cents), 0)::bigint
		  from bucket b left join ledger_entry e on e.account_id = b.ledger_account_id
		 where b.id = any($1::bigint[])
		 group by b.id`, bucketIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, cents int64
		if err := rows.Scan(&id, &cents); err != nil {
			return nil, err
		}
		out[id] = cents
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range bucketIDs {
		if _, ok := out[id]; !ok {
			return nil, fmt.Errorf("bucket %d does not exist", id)
		}
	}
	return out, nil
}

// OpenQty is the ledger's net-open contracts per (bucket, side) of one market: fills added up,
// bought less sold, never trade_order.qty and never a status. It is the settlement path's last
// look before it writes (plan 5.3): a quantity that differs from memory means memory is behind,
// and nothing is written. A lot that nets to nothing is absent from the map. Keys are LotKey.
func (s *Store) OpenQty(ctx context.Context, marketID int64, bucketIDs []int64) (map[[2]string]int, error) {
	return openQty(ctx, s.pool, marketID, bucketIDs)
}

func openQty(ctx context.Context, q querier, marketID int64, bucketIDs []int64) (map[[2]string]int, error) {
	out := map[[2]string]int{}
	if len(bucketIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		select o.bucket_id, o.side, sum(case when o.action = 'buy' then f.qty else -f.qty end)::int
		  from trade_order o join fill f on f.order_id = o.id
		 where o.market_id = $1 and o.bucket_id = any($2::bigint[])
		 group by o.bucket_id, o.side
		having sum(case when o.action = 'buy' then f.qty else -f.qty end) > 0`, marketID, bucketIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket int64
		var side string
		var qty int
		if err := rows.Scan(&bucket, &side, &qty); err != nil {
			return nil, err
		}
		out[LotKey(bucket, side)] = qty
	}
	return out, rows.Err()
}

// MarketResults is the sweep's read: the result of each of those markets that has one. A market
// with no result yet is absent from the map. The poller tells an engine about a result exactly
// once, so the third engine reads it from the table instead of depending on that call.
func (s *Store) MarketResults(ctx context.Context, marketIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(marketIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `select id, result from market where id = any($1::bigint[]) and result is not null`, marketIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var result string
		if err := rows.Scan(&id, &result); err != nil {
			return nil, err
		}
		out[id] = result
	}
	return out, rows.Err()
}

// BucketFills is the rebuild read (plan 5.4): every fill of those buckets that is either in a
// market with no result yet, or belongs to a lot that is NET open (bought more than sold) and
// has no settlement row. Oldest first, which is the order the engine folds them in.
//
// Net, exactly as RealisedByCoin decides it: a position sold out before the close has no
// settlement row and never will, and must not read as an open bet. There is no time bound: a
// net-open lot whose market already has a result, however old, is handed to the sweep.
//
// The bucket's entry is LEFT-joined: a sale whose premium equalled its fee share has none, and
// reads as 0 cents.
func (s *Store) BucketFills(ctx context.Context, bucketIDs []int64) ([]BucketFill, error) {
	return bucketFills(ctx, s.pool, bucketIDs)
}

func bucketFills(ctx context.Context, q querier, bucketIDs []int64) ([]BucketFill, error) {
	out := []BucketFill{}
	if len(bucketIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		with open_lot as (
		    select o.bucket_id, o.market_id, o.side
		      from trade_order o join fill f on f.order_id = o.id
		     where o.bucket_id = any($1::bigint[])
		       and not exists (select 1 from settlement x
		                        where x.market_id = o.market_id and x.bucket_id = o.bucket_id and x.side = o.side)
		     group by o.bucket_id, o.market_id, o.side
		    having sum(case when o.action = 'buy' then f.qty else -f.qty end) > 0)
		select o.bucket_id, o.market_id, m.ticker, i.underlying, m.closes_at, m.strike::float8, coalesce(m.result, ''),
		       o.id, coalesce(o.client_order_id, ''), o.action, o.side, o.detail,
		       f.id, f.at, f.qty::int, f.price::text, f.fee_cents, coalesce(e.amount_cents, 0)
		  from trade_order o
		  join fill f              on f.order_id = o.id
		  join bucket b            on b.id = o.bucket_id
		  left join ledger_entry e on e.transfer_id = f.transfer_id and e.account_id = b.ledger_account_id
		  join market m            on m.id = o.market_id
		  join instrument i        on i.id = m.instrument_id
		 where o.bucket_id = any($1::bigint[])
		   and (m.result is null
		        or exists (select 1 from open_lot l
		                    where l.bucket_id = o.bucket_id and l.market_id = o.market_id and l.side = o.side))
		 order by f.id`, bucketIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f BucketFill
		var closes *time.Time
		var detail []byte
		if err := rows.Scan(&f.BucketID, &f.MarketID, &f.Ticker, &f.Underlying, &closes, &f.Strike, &f.Result,
			&f.OrderID, &f.ClientID, &f.Action, &f.Side, &detail,
			&f.FillID, &f.At, &f.Qty, &f.Price, &f.FeeCents, &f.BucketCents); err != nil {
			return nil, err
		}
		if closes != nil {
			f.ClosesAt = *closes
		}
		f.Detail = detail
		out = append(out, f)
	}
	return out, rows.Err()
}
