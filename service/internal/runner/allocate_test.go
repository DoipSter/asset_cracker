package runner

import (
	"context"
	"testing"
	"time"
)

// The sustainment allocation at settlement: a winning window lifts the book above its mark,
// the rates in force split the gain, the skim is one ledger write, and the account's cash goes
// down by exactly what left. Memory and the fake ledger agree afterwards.
func TestAllocationAtSettlement(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100() // 100 Yes at 0.60: 6000 cents plus the fee
	cashBefore, _, _ := g.memory(r)
	g.s.setPolicy(2000, 1000, 0, 0) // 20% winnings, 10% replenishment, set on the page after the bet
	g.advance(10*time.Minute + time.Second)
	g.settle(mktA, "yes") // 100 contracts pay 10000 cents

	book := cashBefore + 10000
	gain := book - seed3Cents
	if gain <= 0 {
		t.Fatalf("the rig's winner did not lift the book above the seed: book %d", book)
	}
	want := gain*2000/10000 + gain*1000/10000
	if len(g.s.skims) != 1 {
		t.Fatalf("%d skims recorded, want 1", len(g.s.skims))
	}
	k := g.s.skims[0]
	if k.BookCents != book || k.HWMBefore != seed3Cents || k.Taken() != want || k.Winnings != gain*2000/10000 || k.Replenish != gain*1000/10000 || k.Tax != 0 || k.Fees != 0 {
		t.Fatalf("skim %+v; want book %d mark %d taken %d", k, book, seed3Cents, want)
	}
	cash, _, _ := g.memory(r)
	if cash != book-want {
		t.Fatalf("cash after the allocation %d, want %d", cash, book-want)
	}
	g.equalsLedger(r)
	id := g.bucketID()
	r.mu.Lock()
	mark := r.hwm[id]
	r.mu.Unlock()
	if mark != book-want {
		t.Fatalf("mark %d, want %d", mark, book-want)
	}
	g.wantState(stateRunning)

	// A second settlement with no new high records nothing.
	g.s.addMarket(503, "KXBTC15M-B", "BTC", t0.Add(30*time.Minute), strike)
	g.settle(503, "no")
	if len(g.s.skims) != 1 {
		t.Fatalf("%d skims after a settlement with no new high", len(g.s.skims))
	}
}

// With every rate at zero the new mark is recorded and no money moves.
func TestZeroRatesRecordTheMarkOnly(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	cashBefore, _, _ := g.memory(r)
	g.advance(10*time.Minute + time.Second)
	g.settle(mktA, "yes")
	if len(g.s.skims) != 1 || g.s.skims[0].Taken() != 0 || g.s.skims[0].BookCents != cashBefore+10000 {
		t.Fatalf("skims %+v", g.s.skims)
	}
	cash, _, _ := g.memory(r)
	if cash != cashBefore+10000 {
		t.Fatalf("cash moved to %d with every rate at zero", cash)
	}
	g.equalsLedger(r)
}

// A loser is never skimmed: the book stays under the mark.
func TestNoAllocationOnALoss(t *testing.T) {
	g := newRig(t)
	g.start(true)
	g.buy100()
	g.s.setPolicy(5000, 0, 0, 0)
	g.advance(10*time.Minute + time.Second)
	g.settle(mktA, "no")
	if len(g.s.skims) != 0 {
		t.Fatalf("a loss was skimmed: %+v", g.s.skims)
	}
}

// The mark survives a restart: the next load reads it from the last skim, not from the seed.
func TestMarkIsReadAtLoad(t *testing.T) {
	g := newRig(t)
	g.start(true)
	g.buy100()
	g.s.setPolicy(2000, 0, 0, 0)
	g.advance(10*time.Minute + time.Second)
	g.settle(mktA, "yes")
	want := g.s.skims[0].BookCents - g.s.skims[0].Taken()
	r := g.start(true)
	id := g.bucketID()
	r.mu.Lock()
	mark := r.hwm[id]
	r.mu.Unlock()
	if mark != want {
		t.Fatalf("mark after a restart %d, want %d", mark, want)
	}
}

// A bucket whose last bet loses with less than the floor left is closed at that settlement:
// reaped into replenishment, frozen, not replaced, and gone from the held set.
func TestRunOutIsClosedAtSettlement(t *testing.T) {
	g := newRig(t)
	r := g.start(true)
	g.buy100()
	// The ledger is drained under the engine while the bet is open, then a rebuild makes
	// memory match: 50 cents and a live position, which is not "ran out" yet.
	g.s.adjust(g.bucketID(), 50-g.s.ledgerCash(g.bucketID()))
	r.mu.Lock()
	r.suspendLocked("the test forces a rebuild")
	r.mu.Unlock()
	g.advance(healDelay3)
	r.Tick(context.Background())
	g.wantState(stateRunning)
	if g.s.count("CloseBucket") != 0 || g.contracts(r) != 100 {
		t.Fatalf("closed %d, contracts %d before the settlement", g.s.count("CloseBucket"), g.contracts(r))
	}
	g.advance(10 * time.Minute)
	g.settle(mktA, "no")
	if len(g.s.closed) != 1 || len(r.BucketIDs()) != 0 || !g.s.buckets[0].Frozen || g.s.ledgerCash(g.s.buckets[0].ID) != 0 {
		t.Fatalf("closed %v, held %v, frozen %v, cash %d", g.s.closed, r.BucketIDs(), g.s.buckets[0].Frozen, g.s.ledgerCash(g.s.buckets[0].ID))
	}
	g.wantState(stateRunning)
	if got := r.Snapshot(context.Background())["mode"]; got != "observe-only" {
		t.Fatalf("mode after the close %v", got)
	}
}
