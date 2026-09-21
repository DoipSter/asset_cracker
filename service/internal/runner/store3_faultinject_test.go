//go:build faultinject

package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// The dev-only fault injector, against the fake ledger: go test -tags faultinject ./internal/runner/
// What it must get right is the KIND of each failure, because the runner acts on the kind.
func TestFaultInjectorKinds(t *testing.T) {
	step := func(id string) store.StepRecord3 {
		return store.StepRecord3{EvaluationID: 1, At: t0, MarketID: mktA, Decisions: []store.DecisionRow{{BucketID: 1, Action: "none"}},
			Orders: []store.OrderRow{{DecisionIndex: 0, BucketID: 1, BucketLedgerID: 2, ClientID: id, Action: "buy", Side: "yes", Qty: 1, Limit: "0.6000", Status: "cancelled"}}}
	}
	ctx := context.Background()

	t.Run("refuse: certain, nothing written, three in a row then a success", func(t *testing.T) {
		s := newFakeStore()
		db := WrapStore3(s, Faults{Every: 2, Run: 3})
		var got []bool
		for i := 0; i < 6; i++ {
			err := db.RecordOrders(ctx, fakeSetup, step(string(rune('a'+i))))
			if err != nil && !store.DefinitelyRolledBack(err) {
				t.Fatalf("a refusal must read as certainly rolled back: %v", err)
			}
			got = append(got, err == nil)
		}
		if want := []bool{true, false, false, false, true, false}; !equalBools(got, want) || len(s.orders) != 2 {
			t.Fatalf("writes %v (want %v), %d orders landed", got, want, len(s.orders))
		}
	})
	t.Run("unknown: not certain, nothing written", func(t *testing.T) {
		s := newFakeStore()
		err := WrapStore3(s, Faults{Every: 1, Kind: "unknown"}).RecordOrders(ctx, fakeSetup, step("a"))
		if err == nil || store.DefinitelyRolledBack(err) || len(s.orders) != 0 {
			t.Fatalf("err %v, %d orders", err, len(s.orders))
		}
	})
	t.Run("lose: not certain, and it committed", func(t *testing.T) {
		s := newFakeStore()
		err := WrapStore3(s, Faults{Every: 1, Kind: "lose"}).RecordOrders(ctx, fakeSetup, step("a"))
		if err == nil || store.DefinitelyRolledBack(err) || len(s.orders) != 1 {
			t.Fatalf("err %v, %d orders", err, len(s.orders))
		}
	})
	t.Run("delay: the caller's deadline fires, and the commit lands afterwards", func(t *testing.T) {
		s := newFakeStore()
		db := WrapStore3(s, Faults{Every: 1, Kind: "delay", DelayCommit: 150 * time.Millisecond})
		wctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		err := db.RecordOrders(wctx, fakeSetup, step("a"))
		if !errors.Is(err, context.DeadlineExceeded) || store.DefinitelyRolledBack(err) {
			t.Fatalf("err %v", err)
		}
		if n := len(s.ordersNow()); n != 0 {
			t.Fatalf("%d orders landed before the delay was over", n)
		}
		deadline := time.Now().Add(5 * time.Second)
		for len(s.ordersNow()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if len(s.ordersNow()) != 1 {
			t.Fatal("the delayed commit never landed")
		}
	})
	t.Run("reads fail only when asked for", func(t *testing.T) {
		s := newFakeStore()
		if _, err := WrapStore3(s, Faults{Every: 1}).BucketCash(ctx, nil); err != nil {
			t.Fatalf("a read failed although only writes were asked for: %v", err)
		}
		if _, err := WrapStore3(s, Faults{Every: 1, Ops: []string{"reads"}}).BucketCash(ctx, nil); err == nil {
			t.Fatal("the read did not fail")
		}
	})
	t.Run("off means the store itself", func(t *testing.T) {
		s := newFakeStore()
		if WrapStore3(s, Faults{}) != Store3(s) {
			t.Fatal("with nothing asked for the store must be handed back untouched")
		}
	})
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
