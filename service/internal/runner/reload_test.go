package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
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

// Reap by the operator's hand: refused while a bet is open; once it settles the bucket is reaped
// into the pool and frozen; with restake a fresh life takes its place and trades if its version
// is approved. A version without a bucket is refused by name.
func TestReapAndRestake(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	r := g.start(true)
	scalper := g.s.versions[0].ID
	g.buy100()

	if _, err := r.Reap(ctx, scalper, false); err == nil || !strings.Contains(err.Error(), "open position") {
		t.Fatalf("a bucket with a bet on must not be reaped: %v", err)
	}
	if g.s.count("CloseBucket") != 0 || g.contracts(r) != 100 {
		t.Fatalf("the refusal changed something: closes %d contracts %d", g.s.count("CloseBucket"), g.contracts(r))
	}
	g.wantState(stateRunning)

	g.advance(10 * time.Minute)
	g.s.setResult(mktA, "yes") // the record has the result: a rebuild after this settles the lot too
	g.settle(mktA, "yes")
	cash := g.s.ledgerCash(g.bucketID())
	first := g.bucketID()

	// Retired, then reaped: what it held goes to the pool, the bucket is frozen, nothing replaces it.
	g.s.setStatus("Scalper", "retired")
	if _, err := r.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := r.Reap(ctx, scalper, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReapedCents != cash || rep.Next != "" || rep.Held != 0 || len(r.BucketIDs()) != 0 || !g.s.buckets[0].Frozen || g.s.ledgerCash(first) != 0 {
		t.Fatalf("reap: %+v held %v frozen %v cash %d (was %d)", rep, r.BucketIDs(), g.s.buckets[0].Frozen, g.s.ledgerCash(first), cash)
	}
	g.wantState(stateRunning)
	if _, err := r.Reap(ctx, scalper, false); !errors.Is(err, ErrNoHeldBucket) {
		t.Fatalf("a version without a bucket: %v", err)
	}

	// Approving again seeds nothing: the version has a bucket, frozen. Coming back is a restake:
	// life 2 opens from the pool, and trades because the version is approved.
	g.s.setStatus("Scalper", "probation")
	if rep, err := r.Reload(ctx); err != nil || rep.Held != 0 {
		t.Fatalf("re-approval alone: %+v %v", rep, err)
	}
	rep, err = r.Reap(ctx, scalper, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Bucket != "" || rep.ReapedCents != 0 || !strings.HasSuffix(rep.Next, " life 2") || rep.Held != 1 || rep.MayOrder != 1 {
		t.Fatalf("restake with nothing held: %+v", rep)
	}
	if g.s.ledgerCash(g.bucketID()) != seed3Cents {
		t.Fatalf("life 2 holds %d, want the seed", g.s.ledgerCash(g.bucketID()))
	}
	// Reap and restake while held and approved: life 3 replaces life 2 at once.
	rep, err = r.Reap(ctx, scalper, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(rep.Bucket, " life 2") || rep.ReapedCents != seed3Cents || !strings.HasSuffix(rep.Next, " life 3") || rep.Held != 1 || rep.MayOrder != 1 {
		t.Fatalf("reap and restake: %+v", rep)
	}
	if lifeOf(rep.Next) != 3 || lifeOf("Scalper v3") != 1 || lifeOf("x life 7") != 7 {
		t.Fatalf("lifeOf: %d", lifeOf(rep.Next))
	}
}

// Deploy by the operator's hand: a draft is seeded at the figure asked for and put on probation
// in the same write, so it trades on the next look; a version that holds a bucket is refused
// without the books being touched; after a reap, a deploy opens the next life at its own figure,
// and the allocator's mark for that life is that figure, not the convention.
func TestDeployAtAChosenSeed(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	g.s.addVersion("Value", "draft", plumbing(t, "Value"))
	r := g.start(true)
	scalper, value := g.s.versions[0].ID, g.s.versions[1].ID
	if len(r.BucketIDs()) != 1 {
		t.Fatalf("held %v: a draft is not seeded", r.BucketIDs())
	}

	if _, err := r.Deploy(ctx, store.Deploy{VersionID: scalper, SeedCents: 50000, Source: store.SeedFromBank}); !errors.Is(err, store.ErrBucketHeld) {
		t.Fatalf("a version with a bucket held: %v", err)
	}
	if g.s.count("DeployBucket") != 0 || len(r.BucketIDs()) != 1 {
		t.Fatalf("the refusal touched the books: deploys %d held %v", g.s.count("DeployBucket"), r.BucketIDs())
	}
	g.wantState(stateRunning)

	rep, err := r.Deploy(ctx, store.Deploy{VersionID: value, SeedCents: 50000, Source: store.SeedFromReplenishment})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Bucket != "kalshi15m3 Value v3" || rep.SeedCents != 50000 || rep.Held != 2 || rep.MayOrder != 2 {
		t.Fatalf("deploy: %+v", rep)
	}
	if g.s.version(value).Status != "probation" {
		t.Fatalf("the draft was not put on probation: %s", g.s.version(value).Status)
	}
	var valueID int64
	for _, b := range g.s.buckets {
		if b.strategy == "Value" {
			valueID = b.ID
		}
	}
	if g.s.ledgerCash(valueID) != 50000 {
		t.Fatalf("the deployed bucket holds %d, want the figure asked for", g.s.ledgerCash(valueID))
	}
	r.mu.Lock()
	mark, acct := r.hwm[valueID], r.engine.Account(valueID)
	r.mu.Unlock()
	if mark != 50000 {
		t.Fatalf("the allocator's mark is %d, want the bucket's own seed", mark)
	}
	if acct == nil || acct.Seed() != 50000 {
		t.Fatalf("the engine sizes the deployed bucket off %v, want its own seed of 50000", acct)
	}
	g.wantState(stateRunning)

	// Reaped, then deployed again: life 2 at another figure.
	g.s.setStatus("Value", "retired")
	if _, err := r.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reap(ctx, value, false); err != nil {
		t.Fatal(err)
	}
	rep, err = r.Deploy(ctx, store.Deploy{VersionID: value, SeedCents: 25000, Source: store.SeedFromBank})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Bucket != "kalshi15m3 Value v3 life 2" || rep.Held != 2 || rep.MayOrder != 2 || g.s.version(value).Status != "probation" {
		t.Fatalf("redeploy: %+v status %s", rep, g.s.version(value).Status)
	}
	g.equalsLedger(r)
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
