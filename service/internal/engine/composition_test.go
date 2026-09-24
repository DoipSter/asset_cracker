package engine

import (
	"strings"
	"testing"
)

func testMember(t *testing.T, name string, tauMin, tauMax, bandMin, lambda float64) Params {
	t.Helper()
	p, err := FromShape(Shape{Name: name, Exit: "hold", Lambda: lambda, StaleCost: 0.0012,
		TauMin: tauMin, TauMax: tauMax, BandMin: bandMin})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPickSitOutWarmupAdaptiveAndNoLookAhead(t *testing.T) {
	a := testMember(t, "A", 300, 600, 0.4, 0.5)
	b := testMember(t, "B", 300, 600, 0.4, 0.5)
	c := &Composition{Name: "Roster", Members: []Params{a, b}, Lookback: 16}

	if idx, how := c.Pick(1000, nil); idx != -1 || how != PickSitOut {
		t.Fatalf("nobody eligible: %d %s", idx, how)
	}
	if idx, how := c.Pick(1000, []int{1}); idx != 1 || how != PickRoster {
		t.Fatalf("one eligible is roster: %d %s", idx, how)
	}

	// Fifteen settled shadows each: not yet K, so warmup is the first eligible (A).
	for i := 0; i < 15; i++ {
		close := float64(100 + i)
		c.settled = append(c.settled,
			shadowSettled{Member: a.Name, Close: close, Cost: 70, PnL: -70},
			shadowSettled{Member: b.Name, Close: close, Cost: 70, PnL: 30},
		)
	}
	if idx, how := c.Pick(1000, []int{0, 1}); idx != 0 || how != PickWarmup {
		t.Fatalf("warmup: %d %s", idx, how)
	}

	// The 16th: B has been winning, A losing. Adaptive picks B.
	c.settled = append(c.settled,
		shadowSettled{Member: a.Name, Close: 200, Cost: 70, PnL: -70},
		shadowSettled{Member: b.Name, Close: 200, Cost: 70, PnL: 30},
	)
	if idx, how := c.Pick(1000, []int{0, 1}); idx != 1 || how != PickAdaptive {
		t.Fatalf("adaptive should pick B: %d %s", idx, how)
	}

	// A future settlement that would flip A into the lead must not be visible at 1000.
	c.settled = append(c.settled, shadowSettled{Member: a.Name, Close: 2000, Cost: 70, PnL: 30})
	if idx, how := c.Pick(1000, []int{0, 1}); idx != 1 || how != PickAdaptive {
		t.Fatalf("a settlement after now must not elect A: %d %s", idx, how)
	}

	// Structural-only ignores the shadows.
	c.StructuralOnly = true
	if idx, how := c.Pick(1000, []int{0, 1}); idx != 0 || how != PickRoster {
		t.Fatalf("structural only: %d %s", idx, how)
	}
}

func TestFromShapeRoster(t *testing.T) {
	p, err := FromShape(Shape{
		Name: "Dance", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012,
		Members: []Member{
			{Name: "Favourite", Exit: "hold", Lambda: 1, StaleCost: 0.0012, Side: SideFavourite, BandMin: 0.70, BandMax: 0.90, TauMin: 300, TauMax: 600},
			{Name: "Value no-longshot", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, BandMin: 0.40, TauMax: 600},
			{Name: "Late model", Exit: "hold", Lambda: 0.8, StaleCost: 0.0012, BandMin: 0.40, TauMax: 150},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasComposition() || p.LookbackWindows != DefaultLookback || p.StructuralOnly || len(p.Members) != 3 || p.Members[0].Side != SideFavourite {
		t.Fatalf("roster: %+v lookback %d", p.Members, p.LookbackWindows)
	}
	if p.Provenance["lookback_windows"].Kind != KindConvention || !strings.Contains(p.Blurb, "roster of 3") {
		t.Fatalf("labelled: %q %+v", p.Blurb, p.Provenance["lookback_windows"])
	}
	back := ToShape(p)
	if len(back.Members) != 3 || back.LookbackWindows != DefaultLookback || back.StructuralOnly {
		t.Fatalf("ToShape: %+v", back)
	}
	q, err := FromShape(back)
	if err != nil || !q.HasComposition() || q.Members[0].Side != SideFavourite || q.Members[2].TauMax != 150 {
		t.Fatalf("round-trip: %v %+v", err, q.Members)
	}

	only, err := FromShape(Shape{
		Name: "Dance structural", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012, StructuralOnly: true,
		Members: []Member{
			{Name: "Favourite", Exit: "hold", Lambda: 1, StaleCost: 0.0012, TauMin: 300, TauMax: 600},
			{Name: "Late", Exit: "hold", Lambda: 0.8, StaleCost: 0.0012, TauMax: 150},
		},
	})
	if err != nil || !only.StructuralOnly || only.LookbackWindows != 0 {
		t.Fatalf("structural only: %v %+v", err, only)
	}

	for _, bad := range []Shape{
		{Name: "x", Exit: "hold", Lambda: 0.5, Members: []Member{{Name: "only", Exit: "hold", Lambda: 0.5}}},
		{Name: "x", Exit: "ev", Lambda: 0.5, StaleCost: 0.0012, StaleCostSell: 0.0012,
			Members: []Member{
				{Name: "a", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012},
				{Name: "b", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012},
			}},
		{Name: "x", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012,
			Members: []Member{
				{Name: "a", Exit: "ev", Lambda: 0.5, StaleCost: 0.0012, StaleCostSell: 0.0012},
				{Name: "b", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012},
			}},
	} {
		if _, err := FromShape(bad); err == nil {
			t.Fatalf("must refuse %+v", bad)
		}
	}
}

func TestShadowSettleIsCausal(t *testing.T) {
	c := &Composition{Lookback: 2, Members: []Params{{Name: "A"}, {Name: "B"}}}
	c.Observe("A", "T1", "yes", 100, 70)
	c.Observe("B", "T1", "yes", 100, 70)
	if _, n := c.score("A", 99); n != 0 {
		t.Fatal("pending is not a score")
	}
	c.Settle("T1", "yes")
	if r, n := c.score("A", 100); n != 1 || r <= 0 {
		t.Fatalf("A won at the close: r=%v n=%d", r, n)
	}
	if _, n := c.score("A", 99); n != 0 {
		t.Fatal("a settlement whose close is after now is invisible")
	}
}
