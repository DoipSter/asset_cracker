package engine

import (
	"math"
	"strings"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/broker"
)

// Every preset builds a version that validates, with the label in its name and its four numbers
// as conventions; and the form's refusals are the engine's.
// A version read back into the builder and registered again comes out the same, every number a
// convention now; the name loses its label on the way in and regains it on the way out.
func TestToShapeRoundTrip(t *testing.T) {
	for _, pr := range Presets() {
		p, err := FromShape(pr.Shape)
		if err != nil {
			t.Fatal(err)
		}
		s := ToShape(p)
		if s.Name != strings.TrimSuffix(p.Name, ConventionSuffix) || s.Hypothesis != "" {
			t.Fatalf("%s: shape %+v", pr.Key, s)
		}
		s.Hypothesis = "a remix"
		q, err := FromShape(s)
		if err != nil {
			t.Fatalf("%s remixed: %v", pr.Key, err)
		}
		if q.Name != p.Name || q.Exit != p.Exit || q.Side != p.Side || q.TauMin != p.TauMin || q.TauMax != p.TauMax || q.BandMin != p.BandMin ||
			q.MaxBets != p.MaxBets || q.Kappa != p.Kappa || q.WindowCapBps != p.WindowCapBps || q.MinVolRatio != p.MinVolRatio || q.Sizing != p.Sizing ||
			q.BaseStakeCents != p.BaseStakeCents || q.TakeCapture != p.TakeCapture || q.MinHold != p.MinHold {
			t.Fatalf("%s did not round-trip:\n%+v\n%+v", pr.Key, p, q)
		}
	}
}

// The late window: a second weight on the model inside lambda_late_tau seconds of the close.
// Both numbers are conventions with provenance, the pair round-trips through the builder, the
// half-pair is refused, and a measured version may not carry it.
func TestLateLambdaShape(t *testing.T) {
	s := Shape{Name: "Horizon", Exit: "hold", Lambda: 0.01, LambdaLate: 0.8, LambdaLateTau: 120, StaleCost: 0.0012}
	p, err := FromShape(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.LambdaLate != 0.8 || p.LambdaLateTau != 120 || !p.HasLateLambda() ||
		p.Provenance["lambda_late"].Kind != KindConvention || p.Provenance["lambda_late_tau"].Kind != KindConvention ||
		!strings.Contains(p.Blurb, "inside 120 s of the close the model's weight is 0.8") {
		t.Fatalf("late lambda not carried: %+v", p)
	}
	for tau, want := range map[float64]float64{600: 0.01, 121: 0.01, 120: 0.8, 30: 0.8, 0: 0.8} {
		if got := p.LambdaAt(tau); got != want {
			t.Errorf("lambda at tau %v: %v, want %v", tau, got, want)
		}
	}
	back := ToShape(p)
	if back.LambdaLate != 0.8 || back.LambdaLateTau != 120 {
		t.Fatalf("did not round-trip: %+v", back)
	}
	// A zero weight inside the window is a chosen number, not "unused": it is kept with provenance.
	z, err := FromShape(Shape{Name: "Market late", Exit: "hold", Lambda: 0.5, LambdaLate: 0, LambdaLateTau: 90})
	if err != nil || z.LambdaAt(60) != 0 || z.Provenance["lambda_late"].Kind != KindConvention {
		t.Fatalf("lambda_late 0 with a window: %v %+v", err, z.Provenance["lambda_late"])
	}
	// Without a window a version has one lambda, and LambdaAt never changes.
	one, _ := FromShape(Shape{Name: "One", Exit: "hold", Lambda: 0.5})
	if one.HasLateLambda() || one.LambdaAt(1) != 0.5 || one.LambdaAt(900) != 0.5 {
		t.Fatalf("a version without a late window: %+v", one)
	}
	if _, has := one.Provenance["lambda_late"]; has {
		t.Fatal("an unused shape field carries no provenance")
	}
	for _, bad := range []Shape{
		{Name: "x", Exit: "hold", Lambda: 0.5, LambdaLate: 0.8},                      // no window
		{Name: "x", Exit: "hold", Lambda: 0.5, LambdaLate: 1.5, LambdaLateTau: 120},  // range
		{Name: "x", Exit: "hold", Lambda: 0.5, LambdaLate: -0.1, LambdaLateTau: 120}, // range
		{Name: "x", Exit: "hold", Lambda: 0.5, LambdaLate: 0.8, LambdaLateTau: -5},   // a negative window
		{Name: "x", Exit: "hold", Lambda: 0.5, LambdaLate: math.NaN(), LambdaLateTau: 120},
	} {
		if _, err := FromShape(bad); err == nil {
			t.Errorf("must be refused: %+v", bad)
		}
	}
	// A measured version has one lambda: the same numbers under the measured basis are refused for it.
	m := p
	m.Basis = "measured"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "lambda_late is a builder's shape") {
		t.Fatalf("a measured version with a late window must be refused for that reason: %v", err)
	}
}

// The engine forms its belief with the weight of the moment: outside the window the model barely
// counts and there is no edge; inside it the model is trusted and the same book gets an entry,
// whose stored detail says which lambda it was formed with.
func TestLateLambdaDecides(t *testing.T) {
	p, err := FromShape(Shape{Name: "Horizon", Exit: "hold", Lambda: 0.01, LambdaLate: 1, LambdaLateTau: 120, StaleCost: 0.0012})
	if err != nil {
		t.Fatal(err)
	}
	a := NewAccount(p, 1, 100000, true)
	e, err := NewEngine(a)
	if err != nil {
		t.Fatal(err)
	}
	// A mid of exactly one half (yes bid 0.49, yes ask 0.51) and a model at 0.95.
	q := book([][2]string{{"0.4900", "100"}}, [][2]string{{"0.4900", "100"}})
	m := Market{Ticker: "T", MarketID: 1, EvaluationID: 1, Strike: 100, Close: 1000}
	v := view("BTC", 0.95)

	ds, ins := e.Decide("BTC", m, q, v, 1000-600) // ten minutes out: lambda 0.01
	if len(ds) != 1 || len(ins) != 0 || ds[0].BlockedBy != BlockedNoEdge || math.Abs(ds[0].P-0.5045) > 1e-9 {
		t.Fatalf("outside the window: %+v %d intents", ds, len(ins))
	}
	m.EvaluationID = 2
	ds, ins = e.Decide("BTC", m, q, v, 1000-60) // a minute out: lambda 1
	if len(ds) != 1 || len(ins) != 1 || ds[0].Action != "enter" || ds[0].Side != "yes" || math.Abs(ds[0].P-0.95) > 1e-9 {
		t.Fatalf("inside the window: %+v %d intents", ds, len(ins))
	}
	if got := ins[0].Detail["lambda"]; got != 1.0 {
		t.Fatalf("the order's detail must carry the weight it was formed with: %v", got)
	}
	// The same book and view through a version with one lambda of 0.01 never enters.
	one, _ := FromShape(Shape{Name: "One", Exit: "hold", Lambda: 0.01, StaleCost: 0.0012})
	e1, _ := NewEngine(NewAccount(one, 2, 100000, true))
	if ds, ins := e1.Decide("BTC", m, q, v, 1000-60); len(ins) != 0 || ds[0].BlockedBy != BlockedNoEdge {
		t.Fatalf("one lambda is unchanged: %+v", ds)
	}
}

// A ladder version must say its window; past an hour the model prices with the long sigma.
func TestLadderFamily(t *testing.T) {
	s := Shape{Name: "Day", Exit: "hold", Lambda: 0.5, Family: FamilyLadders}
	if _, err := FromShape(s); err == nil || !strings.Contains(err.Error(), "tau_max") {
		t.Fatalf("a ladder shape without tau_max must be refused: %v", err)
	}
	s.TauMin, s.TauMax = 10800, 108000
	p, err := FromShape(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.Family != FamilyLadders || p.FamilyOf() != FamilyLadders || (Params{}).FamilyOf() != FamilyRounds || ToShape(p).Family != FamilyLadders {
		t.Fatalf("family: %+v", p)
	}
	if _, err := FromShape(Shape{Name: "x", Exit: "hold", Lambda: 0.5, Family: "spot"}); err == nil {
		t.Fatal("an unknown family must be refused")
	}

	m, _ := NewModel([]string{"BTC"}, map[string]Calibration{"BTC": {DefaultSigma: 1e-4, Decimals: 2}}, 0)
	m.Observe("BTC", 100000, 1000)
	day := Market{Ticker: "D", Strike: 101000, Close: 1000 + 86400}
	v := m.View("BTC", day, 100000, 1000, nil)
	if v.Horizon != "fast" || v.Sigma2 != 1e-8 {
		t.Fatalf("without a long sigma the fast one prices every horizon: %+v", v)
	}
	closes := make([]float64, 0, 31)
	for i := 0; i < 31; i++ { // alternating ±2% days: a sd of about 2.8% a day
		closes = append(closes, 100000*math.Pow(1.02, float64(i%2)))
	}
	long := LongSigmaFromDaily(closes)
	if !(long > 5e-5 && long < 1.5e-4) {
		t.Fatalf("long sigma per sqrt-second %v", long)
	}
	m.SetLongSigma("BTC", long)
	v = m.View("BTC", day, 100000, 1000, nil)
	if v.Horizon != "long" || math.Abs(v.Sigma2-long*long) > 1e-15 || v.Journal()["horizon"] != "long" {
		t.Fatalf("a day out prices with the long sigma: %+v", v)
	}
	if v.PModel <= 0.2 || v.PModel >= 0.5 {
		t.Fatalf("1%% above the strike a day out with ~2.8%% daily vol: p = %v", v.PModel)
	}
	near := m.View("BTC", Market{Ticker: "R", Strike: 101000, Close: 1000 + 600}, 100000, 1000, nil)
	if near.Horizon != "fast" || near.Sigma2 != 1e-8 {
		t.Fatalf("ten minutes out stays with the fast sigma: %+v", near)
	}
	if LongSigmaFromDaily(closes[:5]) != 0 {
		t.Fatal("fewer than ten closes give no long sigma")
	}
}

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
	// A bucket seeded at its own figure is bounded by that figure, not the params' convention;
	// with no figure read from the ledger the convention stands.
	a.SeedCents = 5000
	if got := a.FixedStake(); got != 5000 || a.Seed() != 5000 {
		t.Errorf("the stake never passes the bucket's own seed: %d (seed %d)", got, a.Seed())
	}
	a.SeedCents = 0
	if a.Seed() != 100000 {
		t.Errorf("no seed on record: the convention stands, got %d", a.Seed())
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
