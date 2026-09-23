package engine

import (
	"strings"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/broker"
)

// Every preset builds a version that validates, with the label in its name and its four numbers
// as conventions; and the form's refusals are the engine's.
func TestPresetsBuild(t *testing.T) {
	seen := map[string]bool{}
	for _, pr := range Presets() {
		p, err := FromShape(pr.Shape)
		if err != nil {
			t.Errorf("%s: %v", pr.Key, err)
			continue
		}
		if !strings.HasSuffix(p.Name, ConventionSuffix) || p.Basis != BasisConvention || p.Provenance["lambda"].Kind != KindConvention {
			t.Errorf("%s: %+v", pr.Key, p)
		}
		if seen[p.Name] {
			t.Errorf("two presets named %s", p.Name)
		}
		seen[p.Name] = true
	}
	for _, bad := range []Shape{
		{Name: "x", Exit: "hold", Lambda: 0},                                                                                    // R1
		{Name: "", Exit: "hold", Lambda: 0.5},                                                                                   // no name
		{Name: "x", Exit: "maybe", Lambda: 0.5},                                                                                 // exit
		{Name: "x", Exit: "hold", Lambda: 0.5, Side: "both"},                                                                    // side
		{Name: "x", Exit: "hold", Lambda: 0.5, Sizing: SizingMartingale, BaseStakeCents: 50},                                    // under the base minimum
		{Name: "x", Exit: "hold", Lambda: 0.5, Sizing: SizingMartingale, BaseStakeCents: 1000, Multiplier: 9},                   // multiplier
		{Name: "x", Exit: "hold", Lambda: 0.5, Sizing: SizingMartingale, BaseStakeCents: 1000, Multiplier: 2, MaxDoublings: 11}, // doublings
		{Name: "x", Exit: "hold", Lambda: 0.5, StaleCost: 0.00125},                                                              // finer than 0.0001
		{Name: "x", Exit: "hold", Lambda: 1.5},                                                                                  // lambda range
	} {
		if _, err := FromShape(bad); err == nil {
			t.Errorf("must be refused: %+v", bad)
		}
	}
	// A hold shape ignores the ev-only knobs rather than failing on them.
	p, err := FromShape(Shape{Name: "h", Exit: "hold", Lambda: 0.5, MinHold: 10, Capture: 0.8})
	if err != nil || p.MinHold != 0 || p.TakeCapture != 0 {
		t.Errorf("hold with ev knobs: %v %+v", err, p)
	}
}

func TestSideFilter(t *testing.T) {
	// mid 0.60: yes is the favourite, no the longshot
	if !sideAllowed(SideFavourite, broker.Yes, 0.60) || sideAllowed(SideFavourite, broker.No, 0.60) {
		t.Error("favourite at 0.60 is yes")
	}
	if sideAllowed(SideLongshot, broker.Yes, 0.60) || !sideAllowed(SideLongshot, broker.No, 0.60) {
		t.Error("longshot at 0.60 is no")
	}
	if sideAllowed(SideFavourite, broker.Yes, 0.50) || sideAllowed(SideLongshot, broker.Yes, 0.50) {
		t.Error("at one half there is no favourite and no longshot")
	}
	if !sideAllowed("", broker.Yes, 0.1) || !sideAllowed(SideModel, broker.No, 0.9) {
		t.Error("the model rule allows either side")
	}
}

func TestMartingaleStake(t *testing.T) {
	p := Params{Sizing: SizingMartingale, BaseStakeCents: 1000, Multiplier: 2, MaxDoublings: 3, SeedCents: 100000}
	a := &Account{Params: p}
	for streak, want := range map[int]int64{0: 1000, 1: 2000, 2: 4000, 3: 8000, 4: 8000, 9: 8000} {
		a.LossStreak = streak
		if got := a.FixedStake(); got != want {
			t.Errorf("streak %d: stake %d, want %d", streak, got, want)
		}
	}
	a.Params.MaxDoublings = 10
	a.LossStreak = 10
	if got := a.FixedStake(); got != 100000 {
		t.Errorf("the stake never passes the seed: %d", got)
	}
	if (&Account{Params: Params{Sizing: SizingKelly}}).FixedStake() != 0 {
		t.Error("Kelly sizing has no fixed stake")
	}
	// The fixed stake goes through Size, bounded by the room like any stake.
	s := Size(SizeInput{PSide: 0.70, Asks: []broker.Price{6000}, Kappa: 0.25, CapBps: 2500, SeedCents: 100000,
		EquityCents: 100000, CashCents: 100000, FixedStakeCents: 4000})
	if s.BlockedBy != "" || s.Binding != bindingMartingale || s.StakeCents != 4000 {
		t.Errorf("martingale through Size: %+v", s)
	}
	s = Size(SizeInput{PSide: 0.70, Asks: []broker.Price{6000}, Kappa: 0.25, CapBps: 2500, SeedCents: 100000,
		EquityCents: 100000, CashCents: 100000, FixedStakeCents: 40000})
	if s.BlockedBy != "" || s.Binding != bindingCap || s.StakeCents != 25000 {
		t.Errorf("the window cap bounds a martingale stake: %+v", s)
	}
	s = Size(SizeInput{PSide: 0.50, Asks: []broker.Price{6000}, Kappa: 0.25, CapBps: 2500, SeedCents: 100000,
		EquityCents: 100000, CashCents: 100000, FixedStakeCents: 4000})
	if s.BlockedBy != BlockedNoEdge {
		t.Errorf("no edge means no bet, whatever the sizing: %+v", s)
	}
}

// The streak moves only on settlements: a loss adds one, a win resets it.
func TestLossStreakFollowsSettlements(t *testing.T) {
	m := Measured{Lambda: Provenance{KindConvention, 0.5, "t"}, StaleCost: Provenance{KindConvention, 0.0012, "t"},
		StaleCostSell: Provenance{KindConvention, 0, "t"}, DriftTol: Provenance{KindFact, 0, "t"}}
	p, err := ConventionValue(Conventions{Lambda: m.Lambda, StaleCost: m.StaleCost, StaleCostSell: m.StaleCostSell, DriftTol: m.DriftTol})
	if err != nil {
		t.Fatal(err)
	}
	a := NewAccount(p, 1, 100000, true)
	e, err := NewEngine(a)
	if err != nil {
		t.Fatal(err)
	}
	// A position bought for 600 cents; settle it as a loss, then another as a win.
	a.Positions[posKey{"T1", "yes"}] = &Position{Coin: "BTC", Ticker: "T1", Side: "yes", MarketID: 1, Close: 900, Contracts: 10, CostCents: 600}
	e.ApplySettlement("T1", "no")
	if a.LossStreak != 1 {
		t.Fatalf("after a loss the streak is %d", a.LossStreak)
	}
	a.Positions[posKey{"T2", "yes"}] = &Position{Coin: "BTC", Ticker: "T2", Side: "yes", MarketID: 2, Close: 1800, Contracts: 10, CostCents: 600}
	e.ApplySettlement("T2", "no")
	if a.LossStreak != 2 {
		t.Fatalf("after two losses the streak is %d", a.LossStreak)
	}
	a.Positions[posKey{"T3", "yes"}] = &Position{Coin: "BTC", Ticker: "T3", Side: "yes", MarketID: 3, Close: 2700, Contracts: 10, CostCents: 600}
	e.ApplySettlement("T3", "yes")
	if a.LossStreak != 0 {
		t.Fatalf("a win resets the streak: %d", a.LossStreak)
	}
}
