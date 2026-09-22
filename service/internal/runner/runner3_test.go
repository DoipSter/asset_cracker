package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/doipster/asset_cracker/service/internal/analysis"
	"github.com/doipster/asset_cracker/service/internal/broker"
	k3 "github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// The numbers are those of docs/honest-fills-v3.md, section 10. Fake store, real Paper, real engine.

// buy100 makes the rig's standing first bet: 100 Yes at 0.60, all the book displays.
func (g *rig) buy100() {
	g.t.Helper()
	g.look(mktA, above, up("100"))
	if n := g.contracts(g.r); n != 100 {
		g.t.Fatalf("the rig's first bet filled %d contracts, want the 100 displayed", n)
	}
}

// 27. Apply happens only after RecordOrders returns nil.
func TestApplyOnlyAfterTheLedgerWrite(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	seen := false
	g.s.on["RecordOrders"] = func(int) behaviour {
		// Called from inside Step, which holds r.mu: memory is read here without it.
		a := r.engine.Accounts[0]
		if a.CashCents != seed3Cents || len(a.Open()) != 0 {
			t.Errorf("memory moved BEFORE the write returned: cash %d, %d positions", a.CashCents, len(a.Open()))
		}
		seen = true
		return behaviour{}
	}
	g.buy100()
	if !seen {
		t.Fatal("RecordOrders was never called")
	}
	g.equalsLedger(r)
	if cash, _, _ := g.memory(r); cash >= seed3Cents {
		t.Fatalf("cash %d did not fall after a buy", cash)
	}
}

// 28. A refused write: Void, engine unchanged, still running; the third in a row pauses, Halted
// stays "", and a successful probe resumes.
func TestRefusedWritesPauseAndAProbeResumes(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	refuse := true
	g.s.on["RecordOrders"] = func(int) behaviour {
		if refuse {
			return behaviour{err: serverSaysNo()}
		}
		return behaviour{}
	}
	for i := 1; i <= 3; i++ {
		g.look(mktA, above, up("100"))
		g.advance(time.Second)
		cash, ps, state := g.memory(r)
		if cash != seed3Cents || len(ps) != 0 {
			t.Fatalf("refusal %d changed memory: cash %d, %d positions", i, cash, len(ps))
		}
		if held := r.paper.TotalHeld(g.bucketID(), "KXBTC15M-A", broker.NoBids); held != 0 {
			t.Fatalf("refusal %d left %d contracts held: Void did not restore the book", i, held)
		}
		if want := map[bool]string{false: stateRunning, true: statePaused}[i == 3]; state != want {
			t.Fatalf("after refusal %d the state is %q, want %q", i, state, want)
		}
	}
	if h := r.Book().Halted; h != "" {
		t.Fatalf("a PAUSED v3 must not block the value snapshots; Halted = %q", h)
	}
	if err := SnapshotRefusal([]Book{r.Book()}, g.now()); err != nil {
		t.Fatalf("a paused v3 refused the snapshot: %v", err)
	}
	// While paused it sends nothing, and a probe that is refused keeps it paused.
	orders := len(g.s.orders)
	g.look(mktA, above, up("100"))
	r.Tick(context.Background())
	g.wantState(statePaused)
	// The database recovers: the next probe resumes, and it wrote decisions only.
	refuse = false
	g.advance(time.Second)
	g.look(mktA, above, up("100"))
	r.Tick(context.Background())
	g.wantState(stateRunning)
	if len(g.s.orders) != orders {
		t.Fatalf("an order was written while paused")
	}
	if g.s.decisions == 0 {
		t.Fatal("the probe wrote no decision row")
	}
	g.advance(time.Second)
	g.buy100()
	g.equalsLedger(r)
}

// 29. A write that ends in a deadline: Void, suspended, no OrdersRecorded from Step, no rebuild
// before healAfter. The commit then LANDS late; the rebuild finds it; no second order was sent.
func TestUnknownOutcomeThenTheCommitLands(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	reads := g.s.count("BucketFills")
	g.s.on["RecordOrders"] = func(n int) behaviour {
		return behaviour{err: context.DeadlineExceeded, later: n == 1}
	}
	g.look(mktA, above, up("100"))
	g.wantState(stateSuspended)
	if g.contracts(r) != 0 {
		t.Fatal("memory took a fill whose write did not return nil")
	}
	if n := g.s.count("OrdersRecorded"); n != 0 {
		t.Fatalf("Step asked OrdersRecorded %d times: asked straight after a timeout it can answer 'none' about a commit that lands later", n)
	}
	if h := r.Book().Halted; h == "" {
		t.Fatal("a SUSPENDED v3 must block the value snapshots")
	}
	// Before healAfter: no rebuild, from Run or from Heal, and steps send nothing.
	g.advance(3 * time.Second)
	g.look(mktA, above, up("100"))
	r.Tick(context.Background())
	r.Heal(context.Background())
	if n := g.s.count("BucketFills"); n != reads {
		t.Fatalf("a rebuild ran %d s after the suspension, before healAfter", 3)
	}
	g.s.landLater() // the server commits after all
	g.advance(healDelay3)
	r.Tick(context.Background())
	g.wantState(stateRunning)
	if g.contracts(r) != 100 {
		t.Fatalf("the rebuild found %d contracts, want the 100 of the late commit", g.contracts(r))
	}
	g.equalsLedger(r)
	if held := r.paper.TotalHeld(g.bucketID(), "KXBTC15M-A", broker.NoBids); held != 100 {
		t.Fatalf("the rebuilt holds are %d, want 100", held)
	}
	if len(g.s.orders) != 1 {
		t.Fatalf("%d orders are in the ledger for one intent", len(g.s.orders))
	}
	if !strings.Contains(r.rebuilt.Note, "1 of its 1 orders") {
		t.Fatalf("the rebuild's report is %q", r.rebuilt.Note)
	}
	// The same display, already taken: the engine may ask again, and the book gives nothing.
	g.advance(9 * time.Second)
	g.s.on["RecordOrders"] = nil
	g.look(mktA, above, up("100"))
	if g.contracts(r) != 100 {
		t.Fatalf("contracts already taken were taken again: %d", g.contracts(r))
	}
}

// 30. The same with the commit never landing: the rebuild finds nothing, v3 resumes.
func TestUnknownOutcomeAndTheCommitNeverLands(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.s.on["RecordOrders"] = func(int) behaviour { return behaviour{err: context.DeadlineExceeded} }
	g.look(mktA, above, up("100"))
	g.wantState(stateSuspended)
	g.advance(healDelay3)
	r.Tick(context.Background())
	g.wantState(stateRunning)
	if cash, ps, _ := g.memory(r); cash != seed3Cents || len(ps) != 0 {
		t.Fatalf("cash %d and %d positions after a write that never landed", cash, len(ps))
	}
	g.equalsLedger(r)
	if held := r.paper.TotalHeld(g.bucketID(), "KXBTC15M-A", broker.NoBids); held != 0 {
		t.Fatalf("%d contracts are held for an order that never existed", held)
	}
}

// 31. The ledger is moved under a running v3: suspended within one sweep, rebuilt, equal again.
func TestCashCheck(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	g.s.adjust(g.bucketID(), 500)
	g.advance(sweepEvery3)
	r.Tick(context.Background())
	g.wantState(stateSuspended)
	g.advance(healDelay3)
	r.Tick(context.Background())
	g.wantState(stateRunning)
	g.equalsLedger(r)
}

// 32. The settlement write's error paths, and that nothing settles while suspended.
func TestSettlementPaths(t *testing.T) {
	t.Run("the write failed but the rows exist", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100()
		g.s.on["RecordSettlements"] = func(int) behaviour { return behaviour{err: context.DeadlineExceeded, land: true} }
		g.advance(10 * time.Minute)
		g.s.setResult(mktA, "yes")
		g.settle(mktA, "yes")
		g.wantState(stateRunning)
		if g.contracts(r) != 0 {
			t.Fatal("the position was not closed although its settlement row exists")
		}
		g.equalsLedger(r)
	})
	t.Run("a retry hits the unique key", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100()
		b := r.bucket(g.bucketID())
		g.s.settlements = append(g.s.settlements, fakeSettlement{marketID: mktA, setup: fakeSetup,
			row: store.SettlementRow{BucketID: b.ID, BucketLedgerID: b.LedgerAccountID, Side: "yes", Qty: 100, PayoutCents: 10000}})
		g.advance(10 * time.Minute)
		g.s.setResult(mktA, "yes")
		g.settle(mktA, "yes")
		g.wantState(stateRunning)
		if g.contracts(r) != 0 || len(g.s.settlements) != 1 {
			t.Fatalf("%d contracts left, %d settlement rows", g.contracts(r), len(g.s.settlements))
		}
		g.equalsLedger(r)
	})
	t.Run("no row exists: the next sweep settles it", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100()
		g.s.on["RecordSettlements"] = func(n int) behaviour {
			if n == 1 {
				return behaviour{err: serverSaysNo()}
			}
			return behaviour{}
		}
		g.advance(10 * time.Minute)
		g.s.setResult(mktA, "no")
		g.settle(mktA, "no")
		g.wantState(stateRunning)
		if g.contracts(r) != 100 {
			t.Fatal("the position was closed with no settlement row written")
		}
		g.advance(sweepEvery3)
		r.Tick(context.Background())
		if g.contracts(r) != 0 || len(g.s.settlements) != 1 {
			t.Fatalf("the sweep left %d contracts and %d settlement rows", g.contracts(r), len(g.s.settlements))
		}
		g.equalsLedger(r)
	})
	t.Run("the lookup fails: suspended", func(t *testing.T) {
		g := newRig(t)
		g.start(true)
		g.buy100()
		g.s.on["RecordSettlements"] = func(int) behaviour { return behaviour{err: context.DeadlineExceeded} }
		g.s.on["SettlementsRecorded"] = func(int) behaviour { return behaviour{err: context.DeadlineExceeded} }
		g.advance(10 * time.Minute)
		g.s.setResult(mktA, "yes")
		g.settle(mktA, "yes")
		g.wantState(stateSuspended)
	})
	// A settlement write that runs out its whole budget: the lookup must not reuse that spent
	// context, or it fails at once (pgx checks ctx first) and every timeout becomes a suspension.
	// The budget is shortened so the real deadline expires in milliseconds.
	t.Run("the write runs out its budget and its rows landed: applied, still running", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		r.writeBudget = 30 * time.Millisecond
		g.buy100()
		g.s.on["RecordSettlements"] = func(n int) behaviour { return behaviour{expire: n == 1, land: n == 1} }
		g.advance(10 * time.Minute)
		g.s.setResult(mktA, "yes")
		g.settle(mktA, "yes")
		g.wantState(stateRunning)
		if g.contracts(r) != 0 || len(g.s.settlements) != 1 {
			t.Fatalf("%d contracts left, %d settlement rows", g.contracts(r), len(g.s.settlements))
		}
		g.equalsLedger(r)
	})
	t.Run("the write runs out its budget and nothing landed: still running, the next sweep settles", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		r.writeBudget = 30 * time.Millisecond
		g.buy100()
		g.s.on["RecordSettlements"] = func(n int) behaviour { return behaviour{expire: n == 1} }
		g.advance(10 * time.Minute)
		g.s.setResult(mktA, "no")
		g.settle(mktA, "no")
		g.wantState(stateRunning)
		if g.contracts(r) != 100 || len(g.s.settlements) != 0 {
			t.Fatalf("%d contracts left, %d settlement rows: want the position kept for the next sweep", g.contracts(r), len(g.s.settlements))
		}
		g.advance(sweepEvery3)
		r.Tick(context.Background())
		if g.contracts(r) != 0 || len(g.s.settlements) != 1 {
			t.Fatalf("the sweep left %d contracts and %d settlement rows", g.contracts(r), len(g.s.settlements))
		}
		g.equalsLedger(r)
	})
	t.Run("suspended: Settled, the sweep and the cash check return at once", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100()
		g.advance(10 * time.Minute)
		g.s.setResult(mktA, "yes")
		r.mu.Lock()
		r.suspendLocked("the test says so")
		gen := r.gen
		r.mu.Unlock()
		before := map[string]int{}
		ops := []string{"RecordSettlements", "OpenQty", "SettlementsRecorded", "MarketResults", "BucketCash"}
		for _, op := range ops {
			before[op] = g.s.count(op)
		}
		g.settle(mktA, "yes")
		r.sweep(context.Background())
		r.cashCheck(context.Background())
		r.Tick(context.Background()) // before healAfter: no rebuild either
		for _, op := range ops {
			if g.s.count(op) != before[op] {
				t.Fatalf("%s reached the store while v3 was suspended", op)
			}
		}
		if r.gen != gen {
			t.Fatal("gen moved while suspended")
		}
	})
}

// reconcile runs analysis.Reconcile on the fake ledger's rows for one market.
func (g *rig) reconcile(marketID int64) []analysis.Mismatch {
	var trades []analysis.Trade
	for _, o := range g.s.orders {
		if o.marketID != marketID {
			continue
		}
		qty := 0
		for _, f := range o.row.Fills {
			qty += f.Qty
		}
		trades = append(trades, analysis.Trade{OrderID: o.id, BucketID: o.row.BucketID, Sell: o.row.Action == "sell", Side: o.row.Side, Qty: qty, DepthPriced: true})
	}
	settled := map[analysis.Holding]analysis.Paid{}
	for _, x := range g.s.settlements {
		if x.marketID == marketID {
			settled[analysis.Holding{BucketID: x.row.BucketID, Side: x.row.Side}] = analysis.Paid{Qty: x.row.Qty, PayoutCents: x.row.PayoutCents}
		}
	}
	return analysis.Reconcile(trades, settled)
}

// lateFifty leaves v3 holding 100 Yes in memory with a further 50 whose write timed out: the
// commit is kept back for the test to land.
func (g *rig) lateFifty() {
	g.t.Helper()
	g.buy100()
	g.advance(9 * time.Second) // past the Scalper's min_gap of 8 s
	g.s.on["RecordOrders"] = func(int) behaviour { return behaviour{err: context.DeadlineExceeded, later: true} }
	g.look(mktA, above, up("150")) // the display rose by 50: only the excess can be had
	g.s.on["RecordOrders"] = nil
	g.wantState(stateSuspended)
	if len(g.s.pending) != 1 {
		g.t.Fatalf("%d writes were kept back, want 1", len(g.s.pending))
	}
}

func (g *rig) wantSettled150() {
	g.t.Helper()
	if len(g.s.settlements) != 1 || g.s.settlements[0].row.Qty != 150 {
		g.t.Fatalf("settlements: %+v, want one row of 150", g.s.settlements)
	}
	if bad := g.reconcile(mktA); len(bad) != 0 {
		g.t.Fatalf("analysis.Reconcile would hold the window back: %+v", bad)
	}
	g.equalsLedger(g.r)
}

// 32a. Settlement never runs on stale memory.
func TestSettlementNeverRunsOnStaleMemory(t *testing.T) {
	t.Run("a: the result arrives before healAfter", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.lateFifty()
		g.s.landLater()
		g.advance(3 * time.Second)
		g.s.setResult(mktA, "yes")
		g.settle(mktA, "yes")
		r.Tick(context.Background())
		r.Heal(context.Background())
		if len(g.s.settlements) != 0 {
			t.Fatalf("a settlement was written from a memory that is 50 short: %+v", g.s.settlements)
		}
		g.advance(16 * time.Minute) // well past healAfter, and past the close
		r.Tick(context.Background())
		g.wantState(stateRunning)
		g.wantSettled150()
	})
	t.Run("b: every rebuild read fails", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.lateFifty()
		g.s.landLater()
		g.s.setResult(mktA, "yes")
		g.advance(16 * time.Minute)
		fail := func(int) behaviour { return behaviour{err: context.DeadlineExceeded} }
		g.s.on["BucketCash"], g.s.on["BucketFills"] = fail, fail
		results := g.s.count("MarketResults")
		g.settle(mktA, "yes")
		r.Tick(context.Background())
		r.Heal(context.Background())
		g.wantState(stateSuspended)
		if len(g.s.settlements) != 0 || g.s.count("MarketResults") != results {
			t.Fatal("Heal swept although its rebuild failed")
		}
		g.s.on["BucketCash"], g.s.on["BucketFills"] = nil, nil
		r.Heal(context.Background()) // rebuild, and in the same call the sweep
		g.wantState(stateRunning)
		g.wantSettled150()
	})
	t.Run("c: the commit lands after a rebuild has succeeded without it", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.lateFifty()
		g.advance(healDelay3)
		r.Tick(context.Background()) // rebuilds WITHOUT the fifty, sweeps and checks the cash: all agree
		g.wantState(stateRunning)
		if g.contracts(r) != 100 {
			t.Fatalf("memory holds %d", g.contracts(r))
		}
		g.s.landLater() // now the server commits
		g.advance(10 * time.Minute)
		g.s.setResult(mktA, "yes")
		g.settle(mktA, "yes") // before the minute's cash check
		g.wantState(stateSuspended)
		if len(g.s.settlements) != 0 {
			t.Fatalf("the last look let a settlement of 100 through against a ledger of 150")
		}
		g.advance(healDelay3)
		r.Tick(context.Background())
		g.wantState(stateRunning)
		g.wantSettled150()
	})
}

// sells lists the fake ledger's sale rows that filled something.
func (g *rig) sells() []*fakeOrder {
	var out []*fakeOrder
	for _, o := range g.s.ordersNow() {
		if o.row.Action == "sell" && len(o.row.Fills) > 0 {
			out = append(out, o)
		}
	}
	return out
}

// forceRebuild suspends v3 and lets Run rebuild it: the rebuild's self-check is the proof that
// every recorded row, sale details included, folds to what the ledger says.
func (g *rig) forceRebuild(r *Runner3) {
	g.t.Helper()
	r.mu.Lock()
	r.suspendLocked("the test forces a rebuild")
	r.mu.Unlock()
	g.advance(healDelay3)
	r.Tick(context.Background())
	g.wantState(stateRunning)
}

// 32a, for sales. A sale is recorded from memory, so it takes the same last look at the ledger
// the settlement path takes. These two cases were reproduced on the fake ledger WITHOUT that look:
// (d) left v3 suspended for good, every rebuild failing its self-check on the sale's cost_cents;
// (e) recorded a sale of 100 contracts that were no longer held (sold 200 of 100).
func TestSaleNeverRunsOnStaleMemory(t *testing.T) {
	ctx := context.Background()
	t.Run("d: a late buy lands after a clean rebuild, then a sale", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.lateFifty()
		g.advance(healDelay3)
		r.Tick(ctx) // rebuilds WITHOUT the fifty; everything agrees
		g.wantState(stateRunning)
		if g.contracts(r) != 100 {
			t.Fatalf("memory holds %d", g.contracts(r))
		}
		g.s.landLater() // now the server commits the fifty
		g.advance(9 * time.Second)
		g.look(mktA, below, up("150")) // the Scalper wants to sell the 100 memory holds
		if n := len(g.sells()); n != 0 {
			t.Fatalf("a sale formed on a memory 50 short was recorded (%d sale rows): its cost_cents can never fold", n)
		}
		g.wantState(stateSuspended)
		g.advance(healDelay3)
		r.Tick(ctx) // heals by itself: the rebuild finds all 150
		g.wantState(stateRunning)
		if g.contracts(r) != 150 {
			t.Fatalf("the rebuild found %d contracts, want 150", g.contracts(r))
		}
		g.equalsLedger(r)
		g.advance(9 * time.Second)
		g.look(mktA, below, up("150")) // the exit again, now on a true memory
		if n := len(g.sells()); n != 1 || g.contracts(r) != 0 {
			t.Fatalf("%d sale rows and %d contracts left, want one sale of all 150", n, g.contracts(r))
		}
		g.forceRebuild(r) // the sale's detail folds
		g.equalsLedger(r)
		if bad := g.reconcile(mktA); len(bad) != 0 {
			t.Fatalf("analysis.Reconcile would hold the window back: %+v", bad)
		}
	})
	t.Run("e: a late sale lands after a clean rebuild, then a second exit", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100()
		g.advance(9 * time.Second)
		g.s.on["RecordOrders"] = func(int) behaviour { return behaviour{err: context.DeadlineExceeded, later: true} }
		g.look(mktA, below, up("100")) // the sale of 100 times out, and will commit later
		g.s.on["RecordOrders"] = nil
		g.wantState(stateSuspended)
		g.advance(healDelay3)
		r.Tick(ctx) // rebuilds with the 100 still held: the sale has not landed
		g.wantState(stateRunning)
		if g.contracts(r) != 100 || !strings.Contains(r.rebuilt.Note, "0 of its 1 orders") {
			t.Fatalf("memory holds %d, note %q", g.contracts(r), r.rebuilt.Note)
		}
		g.s.landLater() // now the first sale commits
		g.advance(time.Second)
		g.look(mktA, below, up("100")) // the exit rule still fires on memory
		if n := len(g.sells()); n != 1 {
			t.Fatalf("%d filled sale rows for one position of 100: a sale of contracts never held was recorded", n)
		}
		g.wantState(stateSuspended)
		g.advance(healDelay3)
		r.Tick(ctx) // heals by itself
		g.wantState(stateRunning)
		if g.contracts(r) != 0 {
			t.Fatalf("%d contracts after the heal, want 0", g.contracts(r))
		}
		g.equalsLedger(r)
		g.s.setResult(mktA, "yes")
		if bad := g.reconcile(mktA); len(bad) != 0 {
			t.Fatalf("analysis.Reconcile would hold the window back: %+v", bad)
		}
	})
	t.Run("the last look cannot be read: nothing written, still running", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100()
		g.advance(9 * time.Second)
		g.s.on["OpenQty"] = func(int) behaviour { return behaviour{err: context.DeadlineExceeded} }
		orders := len(g.s.ordersNow())
		g.look(mktA, below, up("100"))
		g.wantState(stateRunning)
		if len(g.s.ordersNow()) != orders || g.contracts(r) != 100 {
			t.Fatalf("a sale was written or applied without its last look")
		}
		if held := r.paper.TotalHeld(g.bucketID(), "KXBTC15M-A", broker.YesBids); held != 0 {
			t.Fatalf("%d contracts are held for a sale that was not written: Void did not run", held)
		}
		g.s.on["OpenQty"] = nil
		g.advance(time.Second)
		g.look(mktA, below, up("100"))
		if len(g.sells()) != 1 || g.contracts(r) != 0 {
			t.Fatalf("the sale did not go through once the look could be read")
		}
		g.equalsLedger(r)
	})
}

// comparable is an account without its soft state (what a restart evaluates afresh).
type comparable struct {
	Cash      int64
	Bets      int
	Exhausted bool
	Positions []k3.Position
	Windows   map[int64]k3.Window
}

func accountsOf(r *Runner3) map[int64]comparable {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[int64]comparable{}
	for _, a := range r.engine.Accounts {
		c := comparable{Cash: a.CashCents, Bets: a.Bets, Exhausted: a.Exhausted, Windows: map[int64]k3.Window{}}
		for _, p := range a.Open() {
			p.Exiting, p.ExitTries, p.BlockedSeconds = "", 0, 0
			c.Positions = append(c.Positions, p)
		}
		for k, w := range a.Windows {
			c.Windows[k] = *w
		}
		out[a.BucketID] = c
	}
	return out
}

// 33. Rebuild equals live.
func TestRebuildEqualsLive(t *testing.T) {
	g := newRig(t)
	g.s.addVersion("Value", "probation", plumbing(t, "Value"))
	live := g.start(true)
	g.look(mktA, above, up("100"))
	g.advance(9 * time.Second)
	g.look(mktA, above, up("160"))                             // the Scalper adds; the Value has had its one bet
	g.look(mktB, above, book("0.5500", "40", "0.4300", "300")) // a second coin in the same window
	g.advance(9 * time.Second)                                 // past min_hold
	g.look(mktA, below, book("0.5800", "30", "0.4000", "160")) // the Scalper sells what the bid shows: a partial exit at a loss
	g.advance(time.Second)
	g.look(mktB, above, book("0.5500", "40", "0.4300", "300"))
	if len(g.s.orders) < 4 {
		t.Fatalf("the round produced only %d orders", len(g.s.orders))
	}
	sold := false
	for _, o := range g.s.orders {
		sold = sold || (o.row.Action == "sell" && len(o.row.Fills) > 0)
	}
	if !sold {
		t.Fatal("the round has no filled sale in it, so it proves nothing about releasing basis")
	}
	g.equalsLedger(live)

	again, err := NewRunner3(context.Background(), g.s, rigCoins, g.options(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, state := g.memory(again); state != stateRunning {
		t.Fatalf("the rebuilt runner is %s: %s", state, again.reason)
	}
	want, got := accountsOf(live), accountsOf(again)
	for id, w := range want {
		gt := got[id]
		if !reflect.DeepEqual(w, gt) {
			t.Errorf("bucket %d:\n live    %+v\n rebuilt %+v", id, w, gt)
		}
		for _, ticker := range []string{"KXBTC15M-A", "KXETH15M-A"} {
			for _, l := range []broker.Ladder{broker.YesBids, broker.NoBids} {
				if a, b := live.paper.TotalHeld(id, ticker, l), again.paper.TotalHeld(id, ticker, l); b < a {
					t.Errorf("bucket %d %s %s: rebuilt holds %d are below the live %d", id, ticker, l, b, a)
				}
			}
		}
	}
}

// 34. The net rule.
func TestNetRule(t *testing.T) {
	g := newRig(t)
	g.start(true)
	g.buy100()
	g.advance(9 * time.Second)
	g.look(mktA, below, up("100")) // sells all 100 into a bid of 500
	if g.contracts(g.r) != 0 {
		t.Fatalf("%d contracts left after the exit", g.contracts(g.r))
	}
	g.look(mktB, above, book("0.5500", "40", "0.4300", "70")) // a second lot, left open
	if g.contracts(g.r) != 70 {
		t.Fatalf("the second lot is %d contracts", g.contracts(g.r))
	}
	g.s.setResult(mktA, "yes")
	g.s.setResult(mktB, "no")
	g.advance(48 * time.Hour) // the service was down over the close, for two days

	r := g.start(true)
	g.wantState(stateRunning) // an old sold-out lot, and an old open one, are no reason to suspend
	if g.contracts(r) != 0 {
		t.Fatalf("%d contracts are still open after the start-up sweep", g.contracts(r))
	}
	if len(g.s.settlements) != 1 || g.s.settlements[0].marketID != mktB || g.s.settlements[0].row.Qty != 70 {
		t.Fatalf("settlements %+v: want the open lot only; a position sold out early gets no row", g.s.settlements)
	}
	if err := SnapshotRefusal([]Book{r.Book()}, g.now()); err != nil {
		t.Fatalf("the snapshot is refused: %v", err)
	}
	if bad := append(g.reconcile(mktA), g.reconcile(mktB)...); len(bad) != 0 {
		t.Fatalf("Reconcile: %+v", bad)
	}
}

// 35. The sweep settles what Settled never delivered, and what TryLock made it skip.
func TestSweepSettlesWhatSettledMissed(t *testing.T) {
	t.Run("never delivered", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100()
		g.advance(10*time.Minute + time.Second)
		if err := SnapshotRefusal([]Book{r.Book()}, g.now()); !errors.Is(err, ErrAwaitingSettlement) {
			t.Fatalf("an open bet on a closed round: %v", err)
		}
		g.s.setResult(mktA, "no")
		r.Heal(context.Background()) // the snapshot writer's call, just before it reads the books
		if g.contracts(r) != 0 || len(g.s.settlements) != 1 {
			t.Fatal("Heal's sweep did not settle the position")
		}
	})
	t.Run("skipped because the lock was busy", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100()
		g.advance(10*time.Minute + time.Second)
		g.s.setResult(mktA, "yes")
		r.mu.Lock()
		within(t, "Settled", func() { g.settle(mktA, "yes") })
		r.mu.Unlock()
		if r.skippedSettled.Load() != 1 || len(g.s.settlements) != 0 {
			t.Fatalf("skipped %d, settlements %d", r.skippedSettled.Load(), len(g.s.settlements))
		}
		g.advance(sweepEvery3)
		r.Tick(context.Background())
		if g.contracts(r) != 0 || len(g.s.settlements) != 1 {
			t.Fatal("the sweep did not pick the skipped settlement up")
		}
		g.equalsLedger(r)
	})
}

// 36. Settle-only: AC_V3 off, or the version retired, with a bet open.
func TestSettleOnly(t *testing.T) {
	for _, c := range []struct{ name, result string }{{"AC_V3 off, a winner", "yes"}, {"AC_V3 off, a loser", "no"}, {"retired, a winner", "yes"}} {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t)
			g.start(true)
			g.buy100()
			on := true
			if strings.HasPrefix(c.name, "AC_V3 off") {
				on = false
			} else {
				g.s.setStatus("Scalper", "retired")
			}
			r := g.start(on)
			if len(r.BucketIDs()) != 1 || r.buckets[0].mayOrder {
				t.Fatalf("held %v, may order %v: want held and settle-only", r.BucketIDs(), r.buckets[0].mayOrder)
			}
			if r.setup.ActorID == 0 || r.setup.VenueLedgerID == 0 || r.setup.FeesLedgerID == 0 || r.setup.PoolLedgerID == 0 {
				t.Fatalf("a settle-only runner has no sim setup: %+v", r.setup)
			}
			writes := g.s.count("RecordOrders")
			g.advance(9 * time.Second)
			g.look(mktA, above, up("400"))
			if g.s.count("RecordOrders") != writes {
				t.Fatal("a settle-only bucket wrote a step")
			}
			book := r.Book()
			if len(book.Positions) != 1 || book.Positions[0].ValueCents == nil || *book.Positions[0].ValueCents != 5800 || book.Buckets[0].Version != 3 {
				t.Fatalf("the bet is not valued at the bid: %+v", book)
			}
			g.advance(10 * time.Minute)
			g.s.setResult(mktA, c.result)
			r.Tick(context.Background())
			if len(g.s.settlements) != 1 {
				t.Fatalf("the sweep wrote %d settlement rows", len(g.s.settlements))
			}
			x := g.s.settlements[0]
			if x.setup.ActorID != fakeSetup.ActorID || x.setup.VenueLedgerID != fakeSetup.VenueLedgerID {
				t.Fatalf("the settlement does not carry the service actor and the paper venue: %+v", x.setup)
			}
			if want := map[string]int64{"yes": 10000, "no": 0}[c.result]; x.row.PayoutCents != want {
				t.Fatalf("payout %d, want %d", x.row.PayoutCents, want)
			}
			if bad := g.reconcile(mktA); len(bad) != 0 {
				t.Fatalf("Reconcile: %+v", bad)
			}
			g.equalsLedger(r)
		})
	}
}

// 37. The bucket set is fixed, and EnsureSimSetup is called exactly once in every mode.
func TestFixedBucketSet(t *testing.T) {
	t.Run("off, or nothing tradable: no names, nothing created, a full setup", func(t *testing.T) {
		g := newRig(t)
		g.start(true)
		g.buy100()
		made := len(g.s.created)
		for _, on := range []bool{false, true} {
			if on {
				g.s.setStatus("Scalper", "bench")
			}
			calls := g.s.count("EnsureSimSetup")
			r := g.start(on)
			if g.s.count("EnsureSimSetup") != calls+1 {
				t.Fatalf("on=%v: EnsureSimSetup was called %d times", on, g.s.count("EnsureSimSetup")-calls)
			}
			if names := g.s.setupNames[len(g.s.setupNames)-1]; len(names) != 0 {
				t.Fatalf("on=%v: it was given names %v", on, names)
			}
			if len(g.s.created) != made {
				t.Fatalf("on=%v: it created %v", on, g.s.created[made:])
			}
			if s := r.setup; s.ActorID == 0 || s.VenueLedgerID == 0 || s.FeesLedgerID == 0 || s.PoolLedgerID == 0 {
				t.Fatalf("on=%v: setup %+v", on, s)
			}
		}
	})
	t.Run("a status change while running changes nothing until a reload", func(t *testing.T) {
		g := newRig(t)
		g.s.addVersion("Value", "draft", plumbing(t, "Value"))
		r := g.start(true)
		ids := r.BucketIDs()
		if len(ids) != 1 {
			t.Fatalf("held %v: a draft version must not be seeded", ids)
		}
		g.s.setStatus("Value", "probation") // approved while the service runs
		tradable, setups, made := g.s.count("TradableVersions"), g.s.count("EnsureSimSetup"), len(g.s.created)
		r.mu.Lock()
		r.suspendLocked("the test forces a rebuild")
		r.mu.Unlock()
		g.advance(healDelay3)
		r.Tick(context.Background())
		g.wantState(stateRunning)
		if g.s.count("TradableVersions") != tradable || g.s.count("EnsureSimSetup") != setups || len(g.s.created) != made {
			t.Fatal("a rebuild asked for the versions or the setup: only NewRunner3 may")
		}
		if !reflect.DeepEqual(r.BucketIDs(), ids) {
			t.Fatalf("the held set moved from %v to %v", ids, r.BucketIDs())
		}
		notes, _ := r.Snapshot(context.Background())["restart_notes"].([]string)
		if len(notes) != 1 || notes[0] != "Value: approved, waiting for a reload" {
			t.Fatalf("restart notes %v", notes)
		}
		g.s.setStatus("Scalper", "retired")
		g.advance(notesFor3) // the status page reuses its read of the statuses for this long
		notes, _ = r.Snapshot(context.Background())["restart_notes"].([]string)
		if len(notes) != 2 || notes[1] != "Scalper: no longer tradable: settle-only after a reload" {
			t.Fatalf("restart notes %v", notes)
		}
	})
}

// 38. Locks. Run under -race.
func TestNothingOnThePollersPathWaits(t *testing.T) {
	ctx := context.Background()
	t.Run("the store blocks inside RecordOrders", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100() // a position for Settled to find
		g.advance(9 * time.Second)
		entered, release := make(chan struct{}, 1), make(chan struct{})
		g.s.on["RecordOrders"] = func(int) behaviour { return behaviour{enter: entered, block: release} }
		done := make(chan struct{})
		go func() { defer close(done); g.look(mktA, above, up("150")) }()
		<-entered // Step now holds r.mu across its write
		infoB, closesB, _ := g.info(mktB)
		within(t, "Inputs", func() { r.Inputs("ETH", infoB, closesB, g.now(), above) })
		within(t, "Observe", func() { r.Observe(trade("ETH-USD", above, g.now())) })
		within(t, "Step for another coin", func() {
			r.Step(ctx, "ETH", 9001, g.now(), mktB, infoB, closesB, book("0.5500", "40", "0.4300", "70"), above)
		})
		within(t, "Settled", func() { g.settle(mktA, "yes") })
		if r.skippedStep.Load() != 1 || r.skippedSettled.Load() != 1 {
			t.Fatalf("skipped_busy: step %d, settled %d", r.skippedStep.Load(), r.skippedSettled.Load())
		}
		if len(g.s.settlements) != 0 {
			t.Fatal("the skipped Settled wrote something")
		}
		close(release)
		<-done
		if g.contracts(r) != 150 {
			t.Fatalf("the blocked step ended with %d contracts", g.contracts(r))
		}
	})
	t.Run("the store blocks inside a rebuild read", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100()
		r.mu.Lock()
		r.suspendLocked("the test forces a rebuild")
		r.mu.Unlock()
		g.advance(healDelay3)
		entered, release := make(chan struct{}, 1), make(chan struct{})
		g.s.on["BucketFills"] = func(int) behaviour { return behaviour{enter: entered, block: release} }
		done := make(chan struct{})
		go func() { defer close(done); r.Tick(ctx) }()
		<-entered
		info, closes, _ := g.info(mktA)
		within(t, "Inputs", func() { r.Inputs("BTC", info, closes, g.now(), above) })
		within(t, "Observe", func() { r.Observe(trade("BTC-USD", above, g.now())) })
		within(t, "Step", func() { g.look(mktA, above, up("150")) })
		within(t, "Settled", func() { g.settle(mktA, "yes") })
		within(t, "Book", func() { r.Book() })
		close(release)
		<-done
		g.wantState(stateRunning)
	})
}

// 39. The rebuild's swap.
func TestRebuildSwap(t *testing.T) {
	ctx := context.Background()
	t.Run("gen moved during the reads: thrown away, and the next attempt succeeds", func(t *testing.T) {
		for _, mover := range []string{"a second rebuild", "a recovered panic"} {
			g := newRig(t)
			r := g.start(true)
			g.buy100()
			r.mu.Lock()
			r.suspendLocked("the test forces a rebuild")
			r.mu.Unlock()
			entered, release := make(chan struct{}, 1), make(chan struct{})
			g.s.on["BucketFills"] = func(n int) behaviour {
				if n == 2 { // 1 was NewRunner3's
					return behaviour{enter: entered, block: release}
				}
				return behaviour{}
			}
			first := make(chan error, 1)
			go func() { first <- r.rebuild(ctx) }()
			<-entered
			if mover == "a second rebuild" { // Run's and Heal's at once
				if err := r.rebuild(ctx); err != nil {
					t.Fatalf("%s: the second rebuild failed: %v", mover, err)
				}
			} else {
				func() {
					defer func() { r.caught("a test", recover()) }()
					panic("boom")
				}()
			}
			close(release)
			err := <-first
			switch {
			case mover == "a second rebuild" && err != nil:
				// The second healed v3: the first's reads are thrown away, and that is no failure.
				t.Fatalf("%s: the overtaken rebuild returned %v after v3 was healed; want nil so the caller sweeps", mover, err)
			case mover == "a second rebuild" && (!r.rebuilt.OK || r.rebuilt.Reason != ""):
				t.Fatalf("%s: the overtaken rebuild overwrote the successful report: %+v", mover, r.rebuilt)
			case mover == "a recovered panic" && !errors.Is(err, errGenMoved):
				t.Fatalf("%s: the overtaken rebuild returned %v, want it thrown away", mover, err)
			}
			if mover == "a recovered panic" {
				g.wantState(stateSuspended)
				g.advance(healDelay3)
				r.Tick(ctx)
			}
			g.wantState(stateRunning)
			if g.contracts(r) != 100 {
				t.Fatalf("%s: %d contracts", mover, g.contracts(r))
			}
			g.equalsLedger(r)
		}
	})
	t.Run("a Settled that arrives during the reads writes nothing; the sweep after the swap settles", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		g.buy100()
		g.advance(10*time.Minute + time.Second)
		g.s.setResult(mktA, "yes")
		r.mu.Lock()
		r.suspendLocked("the test forces a rebuild")
		r.mu.Unlock()
		g.advance(healDelay3)
		entered, release := make(chan struct{}, 1), make(chan struct{})
		g.s.on["BucketFills"] = func(int) behaviour { return behaviour{enter: entered, block: release} }
		done := make(chan struct{})
		go func() { defer close(done); r.Tick(ctx) }()
		<-entered
		g.settle(mktA, "yes")
		if len(g.s.settlements) != 0 {
			t.Fatal("Settled wrote during a rebuild's reads")
		}
		close(release)
		<-done // the same tick: the rebuild succeeded, so the sweep followed it
		if len(g.s.settlements) != 1 || g.contracts(r) != 0 {
			t.Fatalf("%d settlements, %d contracts", len(g.s.settlements), g.contracts(r))
		}
		g.equalsLedger(r)
	})
}

// 40. A panic in Inputs, Step, Settled or Observe is recovered, v3 suspended, nothing reaches the caller.
func TestPanicsAreRecovered(t *testing.T) {
	for _, where := range []string{"Inputs", "Observe", "Step", "Settled"} {
		t.Run(where, func(t *testing.T) {
			g := newRig(t)
			r := g.start(true)
			g.buy100()
			info, closes, _ := g.info(mktA)
			switch where { // break something the entry point cannot do without
			case "Inputs":
				r.model = nil
				if out := r.Inputs("BTC", info, closes, g.now(), above); out != nil {
					t.Fatalf("Inputs returned %v after a panic", out)
				}
			case "Observe":
				r.model = nil
				r.Observe(trade("BTC-USD", above, g.now()))
			case "Step": // with r.mu held and an order pending at the broker
				g.advance(9 * time.Second)
				g.s.on["RecordOrders"] = func(int) behaviour { panic("boom") }
				g.look(mktA, above, up("150"))
			case "Settled":
				g.s.on["OpenQty"] = func(int) behaviour { panic("boom") }
				g.settle(mktA, "yes")
			}
			if h := r.Book().Halted; !strings.Contains(h, "panic") {
				t.Fatalf("Halted = %q after a panic in %s", h, where)
			}
			r.mu.Lock()
			state := r.state
			r.mu.Unlock()
			if state != stateSuspended {
				t.Fatalf("state %q after a panic in %s", state, where)
			}
			if where == "Step" || where == "Settled" { // and it heals: the lock was released, the pending order forgotten
				g.s.on["RecordOrders"], g.s.on["OpenQty"] = nil, nil
				g.advance(healDelay3)
				r.Tick(context.Background())
				g.wantState(stateRunning)
				g.equalsLedger(r)
			}
		})
	}
}

// 41. Nothing tradable and nothing held.
func TestObserveOnlyAndFailedStarts(t *testing.T) {
	t.Run("observe-only", func(t *testing.T) {
		g := newRig(t)
		g.s.setStatus("Scalper", "draft") // a draft version is not traded, and not seeded
		r := g.start(true)
		if len(g.s.setupNames) != 1 || len(g.s.setupNames[0]) != 0 || len(g.s.created) != 0 || len(r.BucketIDs()) != 0 {
			t.Fatalf("names %v, created %v, held %v", g.s.setupNames, g.s.created, r.BucketIDs())
		}
		info, closes, _ := g.info(mktA)
		j := r.Inputs("BTC", info, closes, g.now(), above)
		if p, ok := j["p_model"].(float64); !ok || !(p > 0.5) {
			t.Fatalf("p_model is not journaled in observe-only mode: %v", j)
		}
		g.look(mktA, above, up("100"))
		if g.s.count("RecordOrders") != 0 {
			t.Fatal("observe-only wrote a step")
		}
		if got := r.Snapshot(context.Background())["mode"]; got != "observe-only" {
			t.Fatalf("mode %v", got)
		}
	})
	t.Run("orders off and nothing held is an empty engine, not an absent one", func(t *testing.T) {
		g := newRig(t)
		r, err := NewRunner3(context.Background(), g.s, rigCoins, g.options(false))
		if r == nil || err != nil {
			t.Fatalf("orders off and no bucket: got %v, %v; want an observe-only runner the page can reload", r, err)
		}
		if len(g.s.setupNames) != 1 || len(g.s.setupNames[0]) != 0 || len(g.s.created) != 0 || len(r.BucketIDs()) != 0 {
			t.Fatalf("names %v, created %v, held %v", g.s.setupNames, g.s.created, r.BucketIDs())
		}
		if got := r.Snapshot(context.Background())["mode"]; got != "observe-only" {
			t.Fatalf("mode %v", got)
		}
	})
	for _, op := range []string{"HeldBuckets", "EnsureSimSetup", "TradableVersions"} {
		t.Run(op+" failing is a failed start", func(t *testing.T) {
			g := newRig(t)
			g.s.on[op] = func(int) behaviour { return behaviour{err: context.DeadlineExceeded} }
			if r, err := NewRunner3(context.Background(), g.s, rigCoins, g.options(true)); err == nil {
				t.Fatalf("got runner %v and no error", r)
			}
		})
	}
	t.Run("placeholders are refused anywhere but a _dev database, and without the mark", func(t *testing.T) {
		g := newRig(t)
		opts := g.options(true)
		opts.DatabaseName = "assetcracker"
		r, err := NewRunner3(context.Background(), g.s, rigCoins, opts)
		if err != nil || len(g.s.created) != 0 || len(r.BucketIDs()) != 0 {
			t.Fatalf("a plumbing version was seeded on a database that is not _dev: %v %v", g.s.created, err)
		}
		g = newRig(t)
		var unmarked map[string]any
		_ = json.Unmarshal(g.s.versions[0].Params, &unmarked)
		delete(unmarked, DevPlumbingKey)
		g.s.versions[0].Params, _ = json.Marshal(unmarked)
		if r := g.start(true); len(g.s.created) != 0 || len(r.BucketIDs()) != 0 {
			t.Fatalf("placeholder numbers without the mark were seeded: %v", g.s.created)
		}
	})
}

// 42. Running out.
func TestExhaustion(t *testing.T) {
	// drain leaves the bucket with 50 cents of ledger cash.
	drain := func(g *rig) { g.s.adjust(g.bucketID(), 50-g.s.ledgerCash(g.bucketID())) }

	t.Run("an exhausted account stops ordering and moves no capital while running", func(t *testing.T) {
		g := newRig(t)
		r := g.start(true)
		drain(g)
		g.advance(sweepEvery3)
		r.Tick(context.Background()) // the cash check suspends
		g.advance(healDelay3)
		r.Tick(context.Background()) // the rebuild derives Exhausted
		g.wantState(stateRunning)
		if !r.engine.Accounts[0].Exhausted {
			t.Fatal("the rebuild did not derive Exhausted")
		}
		g.look(mktA, above, up("100"))
		if len(g.s.orders) != 0 || g.s.count("CloseBucket") != 0 {
			t.Fatalf("%d orders, %d closes while running", len(g.s.orders), g.s.count("CloseBucket"))
		}
	})
	for _, on := range []bool{true, false} {
		t.Run("the next start closes it without restake", func(t *testing.T) {
			g := newRig(t)
			g.start(true)
			drain(g)
			r := g.start(on)
			if len(g.s.closed) != 1 || len(r.BucketIDs()) != 0 || !g.s.buckets[0].Frozen || g.s.ledgerCash(g.s.buckets[0].ID) != 0 {
				t.Fatalf("on=%v: closed %v, held %v", on, g.s.closed, r.BucketIDs())
			}
			if again := g.start(on); again != nil && len(again.BucketIDs()) != 0 {
				t.Fatal("a frozen bucket came back")
			}
		})
	}
	t.Run("under the floor with a bet OPEN: not closed; a winner lifts it; a loser is closed after the sweep", func(t *testing.T) {
		for _, result := range []string{"yes", "no"} {
			g := newRig(t)
			g.start(true)
			g.buy100()
			drain(g)
			r := g.start(true)
			if g.s.count("CloseBucket") != 0 || len(r.BucketIDs()) != 1 || g.contracts(r) != 100 {
				t.Fatalf("a bucket with a bet open was closed: %v", g.s.closed)
			}
			if r.engine.Accounts[0].Exhausted {
				t.Fatal("Exhausted was derived from cash alone")
			}
			g.advance(10*time.Minute + time.Second)
			g.s.setResult(mktA, result)
			r = g.start(true) // the start-up sweep settles the old lot, THEN the test for running out
			if len(g.s.settlements) != 1 {
				t.Fatalf("%s: the start-up sweep wrote %d settlements", result, len(g.s.settlements))
			}
			if result == "yes" && (g.s.count("CloseBucket") != 0 || len(r.BucketIDs()) != 1) {
				t.Fatalf("a bucket whose winner lifted it to %d cents was closed", g.s.ledgerCash(g.s.buckets[0].ID))
			}
			if result == "no" && (len(g.s.closed) != 1 || len(r.BucketIDs()) != 0) {
				t.Fatalf("a bucket that lost and has 50 cents was not closed: closed %v, held %v", g.s.closed, r.BucketIDs())
			}
		}
	})
	t.Run("the first rebuild fails: nothing swept, nothing closed, ids known", func(t *testing.T) {
		g := newRig(t)
		g.start(true)
		drain(g)
		g.s.on["BucketFills"] = func(int) behaviour { return behaviour{err: context.DeadlineExceeded} }
		r := g.start(true)
		g.wantState(stateSuspended)
		if g.s.count("CloseBucket") != 0 || g.s.count("MarketResults") != 0 || len(r.BucketIDs()) != 1 {
			t.Fatalf("closes %d, sweeps %d, held %v", g.s.count("CloseBucket"), g.s.count("MarketResults"), r.BucketIDs())
		}
		book := r.Book()
		if book.Halted == "" {
			t.Fatal("a runner that has never rebuilt must block the snapshots")
		}
		// Still listed, at its ledger cash: left out, Value would price it at the zero the capital
		// query gives a bucket held elsewhere.
		if len(book.Buckets) != 1 || book.Buckets[0].CashCents != 50 || book.Buckets[0].Version != 3 {
			t.Fatalf("the held bucket is not in the halted book at its ledger cash: %+v", book.Buckets)
		}
	})
}

// The "v3" block of /api/status is served as JSON and carries what the S5 checklist reads.
func TestStatusBlock(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	doc := r.Snapshot(context.Background())
	blob, err := json.Marshal(doc) // it is served as JSON
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"state":"running"`, `"may_order":true`, `"skipped_busy"`, `"last_rebuild"`, `"holds":{"KXBTC15M-A no_bids":100}`, `"plumbing":true`} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("the status block lacks %s:\n%s", want, blob)
		}
	}
}

// tools/dev-v3-plumbing.sql carries the params as JSON text. They must be what the engine's
// Plumbing constructors make of the script's test values, or the runner refuses the version.
func TestDevPlumbingScriptMatchesTheConstructors(t *testing.T) {
	blob, err := os.ReadFile("../../../tools/dev-v3-plumbing.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(blob)
	if !regexp.MustCompile(`(?s)current_database\(\).*?raise exception`).MatchString(sql[:strings.Index(sql, "insert into")]) {
		t.Fatal("the script does not refuse a database that is not *_dev before its first insert")
	}
	if !strings.Contains(sql, "'"+DevPlumbingMark+"'") {
		t.Fatal("the hypothesis text is not the mark the runner looks for")
	}
	// Strategy names of their own, so that migration 0013's (Scalper, 3) and (Value, 3) never meet
	// a plumbing row or bucket; Value a draft, so the checklist can approve one while running.
	for _, want := range []string{"('Scalper', 'Scalper (dev plumbing)', 'probation',", "('Value', 'Value (dev plumbing)', 'draft',"} {
		if !strings.Contains(sql, want) {
			t.Errorf("the script does not register %s", want)
		}
	}
	ph := func(v float64, what string) k3.Provenance {
		return k3.Provenance{Kind: k3.KindPlaceholder, Value: v, Note: "TEST VALUE for dev plumbing, not a measurement: " + what}
	}
	m := k3.Measured{Lambda: ph(0.5, "lambda"), StaleCost: ph(0.007, "stale_cost"), StaleCostSell: ph(0.007, "stale_cost_sell"), DriftTol: ph(0.02, "drift_tol")}
	for name, build := range map[string]func(k3.Measured) (k3.Params, error){"Scalper": k3.PlumbingScalper, "Value": k3.PlumbingValue} {
		p, err := build(m)
		if err != nil {
			t.Fatal(err)
		}
		found := regexp.MustCompile(`(?s)-- BEGIN ` + name + `\n\s*'(.*?)'::jsonb`).FindStringSubmatch(sql)
		if found == nil {
			t.Fatalf("no params block for %s", name)
		}
		var got, want any
		if err := json.Unmarshal([]byte(strings.ReplaceAll(found[1], "''", "'")), &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = json.Unmarshal(marked(t, p), &want)
		if !reflect.DeepEqual(got, want) {
			wantText, _ := json.Marshal(want)
			t.Errorf("%s: the script's params differ from the constructor's. The constructor makes:\n%s", name, wantText)
		}
		opts := Options3{On: true, DatabaseName: "assetcracker_dev"}
		if _, plumbing, err := opts.vet(json.RawMessage(strings.ReplaceAll(found[1], "''", "'"))); err != nil || !plumbing {
			t.Errorf("%s: the runner would not accept the script's version on a _dev database: %v", name, err)
		}
		opts.DatabaseName = "assetcracker"
		if _, _, err := opts.vet(json.RawMessage(strings.ReplaceAll(found[1], "''", "'"))); err == nil {
			t.Errorf("%s: the runner would accept the plumbing version on prod", name)
		}
	}
}

// 43a. EnsureSimSetup with NO names against a real dev database: it creates nothing, and returns
// the ids v2's own call returns. This is the measurement behind "an empty list creates nothing"
// (plan 5.4); until it has run, that statement is a reading of sim.go.
//
// It runs only when AC_TEST_DB_URL names a database whose name ends in _dev. NOT RUN when this
// was written (2026-09-21: no database on the machine it was written on). What it writes: the
// "on conflict do nothing" inserts of the seven sim accounts and the paper venue account, which
// are no-ops on any database v2 has started against.
func TestEnsureSimSetupWithNoNamesOnDevDatabase(t *testing.T) {
	url := os.Getenv("AC_TEST_DB_URL")
	if url == "" {
		t.Skip("AC_TEST_DB_URL is not set: EnsureSimSetup with no names was NOT run against a database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var name string
	if err := pool.QueryRow(ctx, `select current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, "_dev") {
		t.Fatalf("refusing: %q is not a _dev database", name)
	}
	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// v2's own call first, so the shared accounts exist. With no names it creates no bucket either.
	v2, err := db.EnsureSimSetup(ctx, "kalshi15m2", "kalshi15m", 2, nil, 100000)
	if err != nil {
		t.Fatal(err)
	}
	tables := []string{"bucket", "ledger_transfer", "ledger_entry", "bucket_event", "ledger_account", "venue_account"}
	counts := func() map[string]int64 {
		out := map[string]int64{}
		for _, table := range tables {
			var n int64
			if err := pool.QueryRow(ctx, `select count(*) from `+table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[table] = n
		}
		return out
	}
	before := counts()
	v3, err := db.EnsureSimSetup(ctx, engine3, family3, version3, nil, seed3Cents)
	if err != nil {
		t.Fatal(err)
	}
	// The service may be trading on this database while the test runs, so the ledger tables can
	// grow by themselves: they are compared only if nothing else grew them. bucket, ledger_account
	// and venue_account never grow while a service runs (v2 restakes aside), so they must be equal.
	after := counts()
	for _, table := range []string{"bucket", "ledger_account", "venue_account"} {
		if before[table] != after[table] {
			t.Errorf("%s went from %d to %d rows", table, before[table], after[table])
		}
	}
	for _, table := range []string{"ledger_transfer", "ledger_entry", "bucket_event"} {
		if before[table] != after[table] {
			t.Logf("%s went from %d to %d rows: if a service is trading on this database that is its own work; stop it and run again to be sure", table, before[table], after[table])
		}
	}
	if len(v3.Buckets) != 0 {
		t.Errorf("an empty name list returned buckets: %v", v3.Buckets)
	}
	if v3.ActorID == 0 || v3.ActorID != v2.ActorID || v3.VenueLedgerID != v2.VenueLedgerID || v3.FeesLedgerID != v2.FeesLedgerID || v3.PoolLedgerID != v2.PoolLedgerID {
		t.Errorf("v3's setup %+v differs from v2's %+v", v3, v2)
	}
}

// What kalshi15m3/doc.go asks of its runner beyond the plan's numbered tests.

// AfterDecide runs once after EVERY Decide, also while v3 is paused and sends nothing: a pending
// exit whose rule has stopped firing is dropped then too, and not offered again after the resume.
func TestAfterDecideRunsWhilePaused(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	g.advance(9 * time.Second)                                 // past min_hold
	g.look(mktA, below, book("0.5800", "30", "0.4000", "100")) // a partial exit: 30 sold, the rest still wanted out
	exiting := func() string {
		r.mu.Lock()
		defer r.mu.Unlock()
		if p := r.engine.Accounts[0].Position("KXBTC15M-A", "yes"); p != nil {
			return p.Exiting
		}
		return "no position"
	}
	if g.contracts(r) != 70 || exiting() != "value" {
		t.Fatalf("%d contracts, exiting %q: want 70 left and the exit still wanted", g.contracts(r), exiting())
	}
	g.s.on["RecordOrders"] = func(int) behaviour { return behaviour{err: serverSaysNo()} }
	for i := 0; i < pauseAfter3; i++ { // the same 30 are displayed and already taken: each exit comes back empty, and its write is refused
		g.advance(time.Second)
		g.look(mktA, below, book("0.5800", "30", "0.4000", "100"))
	}
	g.wantState(statePaused)
	if g.contracts(r) != 70 || exiting() != "value" {
		t.Fatalf("refused writes changed memory: %d contracts, exiting %q", g.contracts(r), exiting())
	}
	g.advance(time.Second)
	g.look(mktA, above, up("100")) // the price is back above the strike: the value rule no longer fires
	g.wantState(statePaused)
	if got := exiting(); got != "" {
		t.Fatalf("a paused v3 did not call AfterDecide: the exit is still wanted (%q) although its rule stopped firing", got)
	}
}

// An "inconsistent" event from the engine suspends: memory may now be anything, and only a
// rebuild from the database says what is true.
func TestInconsistentEventSuspends(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	r.mu.Lock()
	r.noteEvents(r.engine.Apply(nil, []broker.Report{{}}), 0, 0) // one report and no intent: the engine says "inconsistent"
	r.mu.Unlock()
	g.wantState(stateSuspended)
	if h := r.Book().Halted; !strings.Contains(h, "could not fold") {
		t.Fatalf("Halted = %q", h)
	}
	g.advance(healDelay3)
	r.Tick(context.Background())
	g.wantState(stateRunning)
	g.equalsLedger(r)
}

// A recorded order the rebuild cannot fold leaves v3 SUSPENDED with the reason, and nothing is
// swept: what is open is not known.
func TestRebuildThatCannotFoldStaysSuspended(t *testing.T) {
	g := newRig(t)
	g.start(true)
	g.buy100()
	g.s.mu.Lock()
	g.s.orders[0].detail = []byte(`{"v":2}`) // not a v3 order's detail
	g.s.mu.Unlock()
	g.advance(16 * time.Minute)
	g.s.setResult(mktA, "yes")
	r := g.start(true)
	g.wantState(stateSuspended)
	r.Tick(context.Background())
	r.Heal(context.Background())
	g.wantState(stateSuspended)
	if len(g.s.settlements) != 0 || g.s.count("MarketResults") != 0 || g.s.count("CloseBucket") != 0 {
		t.Fatalf("a runner that could not rebuild swept or closed: %d settlements", len(g.s.settlements))
	}
	if doc := r.Snapshot(context.Background()); !strings.Contains(fmt.Sprint(doc["last_rebuild"]), "not version 3") {
		t.Fatalf("the status block does not say why the rebuild fails: %v", doc["last_rebuild"])
	}
}

// Prune runs with the once-a-minute work: a window whose positions were all sold early never sees
// a settlement, and is forgotten once it has closed.
func TestPruneOnceAMinute(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	g.advance(9 * time.Second)
	g.look(mktA, below, up("100")) // sells all 100
	windows := func() int {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.engine.Accounts[0].Windows)
	}
	if g.contracts(r) != 0 || windows() != 1 {
		t.Fatalf("%d contracts, %d windows: want the sold-out window still in memory", g.contracts(r), windows())
	}
	g.advance(sweepEvery3)
	r.Tick(context.Background())
	if windows() != 1 {
		t.Fatal("a window that has not closed yet was pruned: its realised losses still count against the budget")
	}
	g.advance(15 * time.Minute)
	r.Tick(context.Background())
	if windows() != 0 {
		t.Fatal("the closed, empty window was not pruned by the minute's tick")
	}
	// The sold-out round is never settled, so only prune can forget its Paper market and orders.
	id := g.bucketID()
	if r.paper.TotalHeld(id, "KXBTC15M-A", broker.YesBids) == 0 {
		t.Fatal("the rig's sale left no hold, so this proves nothing about forgetting the round")
	}
	g.advance(time.Hour)
	r.Tick(context.Background())
	r.mu.Lock()
	_, quoted := r.quotes["KXBTC15M-A"]
	r.mu.Unlock()
	if quoted || r.paper.TotalHeld(id, "KXBTC15M-A", broker.YesBids) != 0 {
		t.Fatal("a round closed an hour ago with no position is still in memory: the Paper grows every round")
	}
}

// The normal build has no fault injector in it: whatever AC_V3_FAIL_* asked for, the store comes
// back untouched (main logs that it was asked and ignored it).
func TestNormalBuildIgnoresFaultSettings(t *testing.T) {
	if FaultInjectionBuilt {
		t.Skip("this is the faultinject build")
	}
	s := newFakeStore()
	if got := WrapStore3(s, Faults{Every: 1, Run: 3, Kind: "unknown", Ops: []string{"orders", "settlements", "reads"}}); got != Store3(s) {
		t.Fatalf("a normal build wrapped the store: %T", got)
	}
}

// Beside 42's "the first rebuild fails": a PANIC in the first rebuild, the start-up sweep or the
// close is a suspended start, not a crash. The same rows would only suspend v3 at run time (40);
// at start they must not put v1 and v2 into a restart loop, with AC_V3 off too.
func TestStartUpPanicSuspends(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("the rebuild's fold panics, on=%v", on), func(t *testing.T) {
			g := newRig(t)
			g.start(true)
			g.buy100()
			g.s.on["BucketFills"] = func(int) behaviour { panic("boom") }
			results, closes := g.s.count("MarketResults"), g.s.count("CloseBucket")
			r, err := NewRunner3(context.Background(), g.s, rigCoins, g.options(on))
			if err != nil || r == nil {
				t.Fatalf("a panicking first rebuild failed the start: %v, %v", r, err)
			}
			g.r = r
			g.wantState(stateSuspended)
			if h := r.Book().Halted; !strings.Contains(h, "panic") {
				t.Fatalf("Halted = %q", h)
			}
			if len(r.BucketIDs()) != 1 || g.s.count("MarketResults") != results || g.s.count("CloseBucket") != closes {
				t.Fatalf("held %v; it swept or closed after the panic", r.BucketIDs())
			}
			g.s.on["BucketFills"] = nil
			g.advance(healDelay3)
			r.Tick(context.Background())
			g.wantState(stateRunning)
			if g.contracts(r) != 100 {
				t.Fatalf("%d contracts after the heal", g.contracts(r))
			}
		})
	}
	t.Run("the start-up close panics under the lock", func(t *testing.T) {
		g := newRig(t)
		g.start(true)
		g.s.adjust(g.bucketID(), 50-g.s.ledgerCash(g.bucketID()))
		g.s.on["CloseBucket"] = func(int) behaviour { panic("boom") }
		r, err := NewRunner3(context.Background(), g.s, rigCoins, g.options(true))
		if err != nil || r == nil {
			t.Fatalf("a panicking close failed the start: %v, %v", r, err)
		}
		g.r = r
		g.wantState(stateSuspended)
		if len(r.BucketIDs()) != 1 || !strings.Contains(r.Book().Halted, "panic") {
			t.Fatalf("held %v, halted %q", r.BucketIDs(), r.Book().Halted)
		}
	})
}

// /api/status says WHY an approved version is not trading, from what the start recorded.
func TestRestartNotesSayWhy(t *testing.T) {
	notes := func(r *Runner3) []string {
		n, _ := r.Snapshot(context.Background())["restart_notes"].([]string)
		return n
	}
	t.Run("refused by vet at start", func(t *testing.T) {
		g := newRig(t)
		opts := g.options(true)
		opts.DatabaseName = "assetcracker" // a plumbing version, on a database that is not _dev
		r, err := NewRunner3(context.Background(), g.s, rigCoins, opts)
		if err != nil {
			t.Fatal(err)
		}
		n := notes(r)
		if len(n) != 1 || !strings.HasPrefix(n[0], "Scalper: refused at the last load: it is a dev plumbing version") || strings.Contains(n[0], "waiting for a reload") {
			t.Fatalf("restart notes %v", n)
		}
	})
	t.Run("its bucket ran out and was frozen", func(t *testing.T) {
		g := newRig(t)
		g.start(true)
		g.s.adjust(g.bucketID(), 50-g.s.ledgerCash(g.bucketID()))
		want := "Scalper: its bucket ran out and is frozen: not traded, not replaced"
		for i, what := range []string{"the start that closes it", "the start after it"} {
			r := g.start(true)
			if n := notes(r); len(n) != 1 || n[0] != want {
				t.Fatalf("%s: restart notes %v", what, n)
			}
			if len(g.s.closed) != 1 || len(r.BucketIDs()) != 0 {
				t.Fatalf("%d: closed %v, held %v", i, g.s.closed, r.BucketIDs())
			}
		}
	})
	t.Run("approved while running with params the next start will refuse", func(t *testing.T) {
		g := newRig(t)
		var unmarked map[string]any
		_ = json.Unmarshal(plumbing(t, "Value"), &unmarked)
		delete(unmarked, DevPlumbingKey) // placeholder numbers without the mark
		params, _ := json.Marshal(unmarked)
		g.s.addVersion("Value", "draft", params)
		r := g.start(true)
		g.s.setStatus("Value", "probation")
		n := notes(r)
		if len(n) != 1 || !strings.HasPrefix(n[0], "Value: approved, but the next load will refuse it: ") {
			t.Fatalf("restart notes %v", n)
		}
	})
}

// Two rebuilds at once, Heal's overtaken by Run's: Heal still sweeps, and the report stays the
// successful one.
func TestHealSweepsWhenItsRebuildIsOvertaken(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	g.advance(10*time.Minute + time.Second)
	g.s.setResult(mktA, "yes")
	r.mu.Lock()
	r.suspendLocked("the test forces a rebuild")
	r.mu.Unlock()
	g.advance(healDelay3)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	g.s.on["BucketFills"] = func(n int) behaviour {
		if n == 2 { // 1 was NewRunner3's
			return behaviour{enter: entered, block: release}
		}
		return behaviour{}
	}
	done := make(chan struct{})
	go func() { defer close(done); r.Heal(ctx) }()
	<-entered
	if err := r.rebuild(ctx); err != nil { // Run's, which wins
		t.Fatal(err)
	}
	close(release)
	<-done
	if len(g.s.settlements) != 1 || g.contracts(r) != 0 {
		t.Fatalf("Heal did not sweep after its rebuild was overtaken by one that healed v3: %d settlements", len(g.s.settlements))
	}
	if !r.rebuilt.OK {
		t.Fatalf("the report was overwritten: %+v", r.rebuilt)
	}
}

// A panic under r.mu is recorded as a suspension BEFORE the lock is released: whoever takes the
// lock next sees "suspended", never half-changed memory reported as running.
func TestPanicUnderTheLockIsNotedBeforeRelease(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	g.advance(9 * time.Second)
	next := make(chan string, 1)
	g.s.on["RecordOrders"] = func(int) behaviour {
		go func() { // waits on r.mu, which Step holds: it gets the lock the moment Step lets go
			r.mu.Lock()
			next <- r.state
			r.mu.Unlock()
		}()
		panic("boom")
	}
	g.look(mktA, above, up("150"))
	if got := <-next; got != stateSuspended {
		t.Fatalf("the next holder of the lock saw state %q after a panic under it", got)
	}
}

// Book stops its own panic under the lock, suspends v3, and returns a HALTED book.
func TestBookPanicIsHalted(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	r.mu.Lock()
	r.engine = nil // Book cannot read an engine that is not there
	r.mu.Unlock()
	var book Book
	within(t, "Book", func() { book = r.Book() })
	if !strings.Contains(book.Halted, "panic") || len(book.Buckets) != 0 {
		t.Fatalf("book %+v", book)
	}
	if err := SnapshotRefusal([]Book{book}, g.now()); err == nil {
		t.Fatal("a book that panicked did not refuse the snapshot")
	}
	g.advance(healDelay3)
	r.Tick(context.Background())
	g.wantState(stateRunning)
	g.equalsLedger(r)
}

// A further suspension does not push healAfter: a fault that recurs faster than healDelay3 must
// not keep v3 from ever trying to rebuild.
func TestRepeatedSuspensionsDoNotPostponeTheHeal(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	for i := 0; i < 3; i++ {
		r.mu.Lock()
		r.suspendLocked(fmt.Sprintf("fault %d", i))
		r.mu.Unlock()
		g.advance(healDelay3 / 3)
	}
	g.advance(time.Second)
	r.Tick(context.Background())
	g.wantState(stateRunning)
}
