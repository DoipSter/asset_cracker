package runner

import (
	"context"
	"testing"
	"time"
)

// A reset followed by a reload is a start without a process start: the approved version is
// seeded again, the engine is rebuilt over the new bucket, and the next look may order.
func TestReloadAfterResetSeedsAndTrades(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	r := g.start(true)
	first := g.bucketID()
	g.look(mktA, above, up("100"))
	if g.s.count("RecordOrders") == 0 {
		t.Fatal("the rig did not trade before the reset")
	}

	r.HoldForReset()
	g.s.wipe()
	r.ReleaseAfterReset()
	if len(r.BucketIDs()) != 0 {
		t.Fatalf("released with buckets %v", r.BucketIDs())
	}
	report, err := r.Reload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Held != 1 || report.MayOrder != 1 {
		t.Fatalf("report %+v", report)
	}
	g.wantState(stateRunning)
	ids := r.BucketIDs()
	if len(ids) != 1 || ids[0] == first {
		t.Fatalf("held %v after the reset; the first bucket was %d", ids, first)
	}
	if g.s.ledgerCash(ids[0]) != seed3Cents {
		t.Fatalf("the new bucket has %d cents, not the seed", g.s.ledgerCash(ids[0]))
	}
	cash, positions, _ := g.memory(r)
	if cash != seed3Cents || len(positions) != 0 {
		t.Fatalf("memory after the reload: cash %d, positions %d", cash, len(positions))
	}
	before := g.s.count("RecordOrders")
	g.look(mktA, above, up("100"))
	if g.s.count("RecordOrders") != before+1 {
		t.Fatal("the reloaded engine did not order on the next look")
	}
	if o := g.s.ordersNow(); len(o) == 0 || o[len(o)-1].row.BucketID != ids[0] {
		t.Fatal("the order went to a bucket that is not the new one")
	}
	g.equalsLedger(r)
}

// Approval and retirement take effect at a reload, not at a process start.
func TestReloadSeedsAnApprovalAndBenchesARetirement(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	g.s.addVersion("Value", "draft", plumbing(t, "Value"))
	r := g.start(true)
	if len(r.BucketIDs()) != 1 {
		t.Fatalf("held %v: a draft is not seeded", r.BucketIDs())
	}

	g.s.setStatus("Value", "probation")
	report, err := r.Reload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Held != 2 || report.MayOrder != 2 || len(r.BucketIDs()) != 2 {
		t.Fatalf("after approving Value: %+v, held %v", report, r.BucketIDs())
	}
	if notes, _ := r.Snapshot(ctx)["restart_notes"].([]string); len(notes) != 0 {
		t.Fatalf("notes after the reload: %v", notes)
	}

	g.s.setStatus("Scalper", "retired")
	report, err = r.Reload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Held != 2 || report.MayOrder != 1 {
		t.Fatalf("after retiring Scalper: %+v", report)
	}
	r.mu.Lock()
	var settleOnly string
	for _, b := range r.buckets {
		if b.Strategy == "Scalper" {
			settleOnly = b.whyNot
		}
	}
	r.mu.Unlock()
	if settleOnly != "its version is retired: settle-only" {
		t.Fatalf("Scalper's bucket: %q", settleOnly)
	}
}

// A load that cannot read leaves the old books in place, and the runner heals from them.
func TestReloadFailureKeepsTheOldBooks(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	r := g.start(true)
	ids := r.BucketIDs()
	g.look(mktA, above, up("100"))

	g.s.on["HeldBuckets"] = func(int) behaviour { return behaviour{err: context.DeadlineExceeded} }
	if _, err := r.Reload(ctx); err == nil {
		t.Fatal("a reload whose read failed returned no error")
	}
	delete(g.s.on, "HeldBuckets")
	if got := r.BucketIDs(); len(got) != 1 || got[0] != ids[0] {
		t.Fatalf("held %v after the failed reload; want %v", got, ids)
	}
	g.wantState(stateSuspended)
	r.mu.Lock()
	due := !g.now().Before(r.healAfter)
	r.mu.Unlock()
	if !due {
		t.Fatal("the failed reload left the heal an hour out")
	}
	r.Tick(ctx)
	g.wantState(stateRunning)
	if g.contracts(r) != 100 {
		t.Fatalf("the healed engine holds %d contracts, not the 100 it bought", g.contracts(r))
	}
	g.equalsLedger(r)
}

// While a reload is between two loads, a rebuild started by Tick cannot put the old ids back:
// the heal is pushed an hour out, and a Tick in that window does nothing.
func TestReloadHoldsOffTheHealer(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	r.mu.Lock()
	r.suspendLocked("reloading the books")
	r.healAfter = g.now().Add(time.Hour)
	r.mu.Unlock()
	reads := g.s.count("BucketCash")
	r.Tick(context.Background())
	if g.s.count("BucketCash") != reads {
		t.Fatal("Tick rebuilt while the reload held the heal off")
	}
	g.wantState(stateSuspended)
}
