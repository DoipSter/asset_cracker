package runner

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	k3 "github.com/doipster/asset_cracker/service/internal/engine"
)

// rosterRig is the rig with one roster version on probation and nothing else. Its first member,
// Early, may buy from ten minutes before the close to five; its second, Late, in the last two and
// a half. Window assignment, so a market the owner does not claim is left alone, and a lookback
// of one clock, so a single settled clock ends the warmup.
func rosterRig(t *testing.T) *rig {
	t.Helper()
	g := &rig{t: t, s: newFakeStore(), clock: t0.Add(5*time.Minute + 10*time.Second)}
	p, err := k3.FromShape(k3.Shape{Name: "Dance", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, Assign: k3.AssignWindow, LookbackWindows: 1,
		Members: []k3.Member{
			{Name: "Early", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, TauMin: 300, TauMax: 600},
			{Name: "Late", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, TauMax: 150},
		}})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	g.s.addVersion(p.Name, "probation", blob)
	g.s.addMarket(mktA, "KXBTC15M-A", "BTC", t0.Add(15*time.Minute), strike)
	g.s.addMarket(mktB, "KXETH15M-A", "ETH", t0.Add(15*time.Minute), strike)
	return g
}

func rostersOf(r *Runner3) map[int64]k3.RosterMemory {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.engine.Rosters()
}

// ownerOf is who owns the next clock, and how: "warmup" is the roster starting over.
func ownerOf(r *Runner3, now time.Time) (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.engine.Accounts {
		if a.Composition != nil {
			return a.Composition.WindowOwner(k3.UnixSeconds(t0.Add(30*time.Minute)), k3.UnixSeconds(now))
		}
	}
	return -1, "no roster"
}

func settledShadows(m map[int64]k3.RosterMemory) (n int) {
	for _, mem := range m {
		n += len(mem.Settled)
	}
	return n
}

// A roster's memory, its members' shadows and the tickers it handed out, outlives the engine it
// was learned in. A rebuild, a reload and a restart used to start it empty, and the roster gave
// every clock to its first member ("warmup") until Lookback clocks had settled again. And a shadow
// on a market no bucket held was never scored live: only a held market's result reached it.
func TestRosterMemorySurvivesRebuildAndRestart(t *testing.T) {
	g := rosterRig(t)
	r := g.start(true)
	if len(r.BucketIDs()) != 1 {
		t.Fatalf("held %v", r.BucketIDs())
	}
	g.look(mktA, above, up("100")) // 590 s out: Early owns the clock (warmup) and buys
	if g.contracts(r) == 0 {
		t.Fatal("Early did not buy: the test proves nothing")
	}
	g.advance(8*time.Minute + 10*time.Second) // 100 s out
	g.look(mktA, above, up("100"))            // Late's shadow; the ticker is Early's
	g.look(mktB, above, up("100"))            // Late's shadow; Early cannot buy and window has no leftovers
	held := g.contracts(r)
	g.advance(2 * time.Minute)
	g.s.setResult(mktA, "yes")
	g.s.setResult(mktB, "yes")
	g.settle(mktA, "yes") // held: settles the bet and the shadows on it
	g.settle(mktB, "yes") // held by nobody: only the shadow
	if g.contracts(r) >= held {
		t.Fatal("the held market did not settle")
	}
	live := rostersOf(r)
	if n := settledShadows(live); n != 3 {
		t.Fatalf("%d shadows scored, want Early on A and Late on A and B: %+v", n, live)
	}
	if _, how := ownerOf(r, g.now()); how != k3.PickWindow {
		t.Fatalf("one settled clock ends a one-clock warmup: %s", how)
	}

	// A rebuild: v3 suspends and heals, and the fresh accounts are given the roster's memory.
	r.mu.Lock()
	r.suspendLocked("the test says so")
	r.healAfter = g.now()
	r.mu.Unlock()
	r.Tick(context.Background()) // rebuild, sweep, prune, save
	g.wantState(stateRunning)
	pruned := rostersOf(r)
	if settledShadows(pruned) != 3 {
		t.Fatalf("after the rebuild: %+v", pruned)
	}
	if _, how := ownerOf(r, g.now()); how != k3.PickWindow {
		t.Fatalf("the rebuild sent the roster back to %s", how)
	}

	// A reload (the buckets page, after an approval) swaps in an empty engine and rebuilds it.
	if _, err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rostersOf(r); !reflect.DeepEqual(got, pruned) {
		t.Fatalf("reloaded with\n %+v\nwas\n %+v", got, pruned)
	}

	// A restart: a new process reads the saved memory.
	again, err := NewRunner3(context.Background(), g.s, rigCoins, g.options(true))
	if err != nil {
		t.Fatal(err)
	}
	if got := rostersOf(again); !reflect.DeepEqual(got, pruned) {
		t.Fatalf("restarted with\n %+v\nwas\n %+v", got, pruned)
	}
	if _, how := ownerOf(again, g.now()); how != k3.PickWindow {
		t.Fatalf("the restart sent the roster back to %s", how)
	}
}

// A shadow whose result the poller's one call did not bring (v3 was busy) is scored by the sweep,
// held or not, as a position is.
func TestTheSweepScoresAShadowSettledMissed(t *testing.T) {
	g := rosterRig(t)
	r := g.start(true)
	g.advance(8*time.Minute + 10*time.Second) // 100 s out
	g.look(mktB, above, up("100"))            // Late's shadow on a market nobody holds
	if g.contracts(r) != 0 {
		t.Fatal("the bucket bought: the market is held and the test proves nothing")
	}
	if p := rostersOf(r); len(p) != 1 {
		t.Fatalf("rosters %+v", p)
	}
	g.advance(2 * time.Minute)
	g.s.setResult(mktB, "no")
	r.mu.Lock()
	within(t, "Settled", func() { g.settle(mktB, "no") })
	r.mu.Unlock()
	if settledShadows(rostersOf(r)) != 0 {
		t.Fatal("scored while the lock was busy")
	}
	g.advance(sweepEvery3)
	r.Tick(context.Background())
	for _, mem := range rostersOf(r) {
		if len(mem.Pending) != 0 || len(mem.Settled) != 1 || mem.Settled[0].PnL >= 0 {
			t.Fatalf("the sweep did not score Late's lost shadow: %+v", mem)
		}
	}
}

// What changed since the last save is saved when Run stops (a release restarts the service), and
// nothing is written when nothing changed.
func TestRosterMemoryIsSavedWhenRunStops(t *testing.T) {
	g := rosterRig(t)
	r := g.start(true)
	stop := func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); r.Run(ctx) }()
		cancel()
		<-done
	}
	stop()
	if n := g.s.count("SaveEngineState"); n != 0 {
		t.Fatalf("saved %d times with nothing changed", n)
	}
	g.advance(8*time.Minute + 10*time.Second)
	g.look(mktB, above, up("100")) // Late's shadow, not saved yet: no minute has passed
	stop()
	var saved savedState3
	if found, err := g.s.LoadEngineState(context.Background(), r.opts.prefix(), &saved); err != nil || !found {
		t.Fatalf("nothing saved: %v", err)
	}
	if len(saved.Rosters) != 1 {
		t.Fatalf("saved %+v", saved.Rosters)
	}
	for _, mem := range saved.Rosters {
		if len(mem.Pending) != 1 || mem.Pending[0].MarketID != mktB {
			t.Fatalf("saved %+v", mem)
		}
	}
}
