package runner

import (
	"testing"
	"time"
)

func TestSetOrdersAndResetHold(t *testing.T) {
	r := &Runner3{
		opts:    Options3{On: true},
		buckets: []bucket3{{mayOrder: true}},
		state:   stateRunning,
	}
	r.ordersLive.Store(true)
	if got := r.SetOrders(false); got != "now" {
		t.Fatalf("switch with a bucket that may order: %q", got)
	}
	placing, effective := r.OrdersStatus()
	if placing || effective != "now" {
		t.Fatalf("after off: placing %v effective %q", placing, effective)
	}
	if got := r.SetOrders(true); got != "now" {
		t.Fatalf("back on: %q", got)
	}
	placing, _ = r.OrdersStatus()
	if !placing {
		t.Fatal("orders did not resume")
	}

	r.buckets = nil
	if got := r.SetOrders(true); got != "next-start" {
		t.Fatalf("no bucket that may order: %q", got)
	}

	r.HoldForReset()
	if r.state != stateSuspended || !r.healAfter.After(time.Now().Add(30*time.Minute)) {
		t.Fatalf("hold: state %s heal %s", r.state, r.healAfter)
	}
	r.AbortReset()
	if r.healAfter.After(time.Now().Add(time.Second)) {
		t.Fatal("a failed reset left the rebuild an hour away")
	}
	r.ReleaseAfterReset()
	if r.state != stateRunning || len(r.buckets) != 0 || r.engine == nil {
		t.Fatalf("release: state %s buckets %d engine %v", r.state, len(r.buckets), r.engine)
	}
}
