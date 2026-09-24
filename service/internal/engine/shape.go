package engine

import (
	"fmt"
	"sort"
	"strings"
)

// The builder's language: a Shape is what a person fills in on the buckets page, and FromShape
// turns it into a Params with every number labelled, or refuses. The four protocol numbers are
// the owner's conventions here (Basis convention); a version whose four numbers were MEASURED is
// built by Scalper and Value from measure3's result, never from a Shape.
//
// Nothing in a Shape can make the engine buy where the belief has no edge after costs. The
// knobs decide when, which side, how much and how to leave; the belief decides whether.

// Shape is one strategy as the builder describes it. Zero fields take the parent's defaults
// (common, scalper, value); the presets show the usual settings. The jsonschema tags are the
// field descriptions the MCP proposal tools publish (internal/proposals); the engine reads none.
type Shape struct {
	Name       string `json:"name" jsonschema:"the version's name, shown everywhere; \" (conventions)\" is added if absent. One version 3 per name"`
	Blurb      string `json:"blurb,omitempty" jsonschema:"one line about it, for the pages; blank takes the parent's"`
	Hypothesis string `json:"hypothesis,omitempty" jsonschema:"what this version is meant to test; strategy_register requires it, it goes in the trials registry"`

	Family string `json:"family,omitempty" jsonschema:"which markets it trades: kalshi15m (the 15-minute rounds; the default) or kalshiladder (the daily and weekly above/below ladders, many legs per coin at once, hours to days from the close; tau_min and tau_max are then hours-to-days in seconds and tau_max must exceed 3600)"`
	Exit   string `json:"exit" jsonschema:"hold: every position is held to settlement (parent Value). ev: sells on value or once the bid covers take_capture of the way to a dollar (parent Scalper)"`
	Side   string `json:"side,omitempty" jsonschema:"which side it may buy: model (whichever the belief favours; the default), favourite (the side priced above one half only), longshot (under one half only)"`

	Lambda        float64 `json:"lambda" jsonschema:"weight on the model against the market, 0 to 1 exclusive of 0; the owner's convention is 0.5. With lambda_late_tau set, this is the weight OUTSIDE the late window"`
	LambdaLate    float64 `json:"lambda_late,omitempty" jsonschema:"weight on the model inside lambda_late_tau seconds of the close, 0 to 1 (0: the belief is the mid there, and nothing is entered); needs lambda_late_tau. A convention, for testing horizon-dependent trust in the model"`
	LambdaLateTau float64 `json:"lambda_late_tau,omitempty" jsonschema:"seconds before the close from which lambda_late applies (tau at or under it); 0 means one lambda for the whole round"`
	StaleCost     float64 `json:"stale_cost,omitempty" jsonschema:"staleness cost in dollars charged to a buy; convention 0.0012"`
	StaleCostSell float64 `json:"stale_cost_sell,omitempty" jsonschema:"staleness cost in dollars charged to a sale; ev only"`

	TauMin  float64 `json:"tau_min,omitempty" jsonschema:"no entry with fewer seconds than this to the close; 0 inherits the parent's"`
	TauMax  float64 `json:"tau_max,omitempty" jsonschema:"no entry with more seconds than this to the close; 0 inherits the parent's"`
	BandMin float64 `json:"band_min,omitempty" jsonschema:"the lowest ask it buys at, 0 to 1"`
	BandMax float64 `json:"band_max,omitempty" jsonschema:"the highest ask it buys at, 0 to 1"`
	MaxBets int     `json:"max_bets,omitempty" jsonschema:"filled buys per round, at most"`
	MinGap  float64 `json:"min_gap,omitempty" jsonschema:"seconds between filled buys in one market"`
	MinHold float64 `json:"min_hold,omitempty" jsonschema:"seconds after the last buy before it may sell; ev only"`
	Capture float64 `json:"take_capture,omitempty" jsonschema:"sell once the bid covers this share (0 to 1) of the way to a dollar; ev only"`

	Kappa        float64 `json:"kappa,omitempty" jsonschema:"the Kelly fraction staked, 0 to 1; a risk preference"`
	WindowCapBps int64   `json:"window_cap_bps,omitempty" jsonschema:"the most one window may use, in basis points of min(equity, seed); a limit"`
	MinVolRatio  float64 `json:"min_vol_ratio,omitempty" jsonschema:"entries only while the model's volatility is at least this many times the coin's default; 0 means no trigger"`

	Sizing         string  `json:"sizing,omitempty" jsonschema:"kelly (the default: a fraction of the edge) or martingale (a fixed stake multiplied after each settled loss; bounded by the window cap and cash; registered as a negative control)"`
	BaseStakeCents int64   `json:"base_stake_cents,omitempty" jsonschema:"martingale only: the first stake in cents, 100 to the seed"`
	Multiplier     float64 `json:"multiplier,omitempty" jsonschema:"martingale only: the stake is multiplied by this after each settled loss, 1 to 4"`
	MaxDoublings   int     `json:"max_doublings,omitempty" jsonschema:"martingale only: the most consecutive multiplications, 0 to 10"`

	// Control marks a version registered to be caught, not to win: the pages say so.
	Control bool `json:"control,omitempty" jsonschema:"a negative control: registered to show the checks catch it, not to win; the pages say so"`

	// Members, when two or more, make this version a roster: one bucket, one version, a set of
	// named shapes plus an assignment. Member is Shape without a nested roster, so the MCP schema
	// does not cycle. The window owner is chosen from prior settled clocks; that owner gets
	// first refusal, then a later specialist (smaller tau_max) may reserve an unclaimed ticker.
	// Sit out if nobody may claim. Results attach to this version, not to a member.
	Members         []Member `json:"members,omitempty" jsonschema:"a roster of 2 to 8 member shapes; this version assigns a window owner then may reserve leftovers. v1 members all hold, same family. Omit for a single shape"`
	LookbackWindows int      `json:"lookback_windows,omitempty" jsonschema:"prior 15-minute clocks the adaptive window owner needs, 1 to 64; default 16 when members are set. 0 with structural_only: owner is always the first member"`
	StructuralOnly  bool     `json:"structural_only,omitempty" jsonschema:"true: window owner is always the first member, no look at recent clocks. The ablation's dumb owner"`
	Assign          string   `json:"assign,omitempty" jsonschema:"both (default): window owner first, later specialists may take unclaimed seats. window: only the owner may enter. reserve: no owner; sit until the latest specialist's clock, then they may enter"`
}

// Member is one shape in a roster. It carries the same knobs as Shape except a nested roster.
type Member struct {
	Name           string  `json:"name" jsonschema:"the member's name, shown on by_member and on the decision"`
	Blurb          string  `json:"blurb,omitempty"`
	Family         string  `json:"family,omitempty"`
	Exit           string  `json:"exit" jsonschema:"hold (v1: every member holds)"`
	Side           string  `json:"side,omitempty"`
	Lambda         float64 `json:"lambda"`
	LambdaLate     float64 `json:"lambda_late,omitempty"`
	LambdaLateTau  float64 `json:"lambda_late_tau,omitempty"`
	StaleCost      float64 `json:"stale_cost,omitempty"`
	StaleCostSell  float64 `json:"stale_cost_sell,omitempty"`
	TauMin         float64 `json:"tau_min,omitempty"`
	TauMax         float64 `json:"tau_max,omitempty"`
	BandMin        float64 `json:"band_min,omitempty"`
	BandMax        float64 `json:"band_max,omitempty"`
	MaxBets        int     `json:"max_bets,omitempty"`
	MinGap         float64 `json:"min_gap,omitempty"`
	MinHold        float64 `json:"min_hold,omitempty"`
	Capture        float64 `json:"take_capture,omitempty"`
	Kappa          float64 `json:"kappa,omitempty"`
	WindowCapBps   int64   `json:"window_cap_bps,omitempty"`
	MinVolRatio    float64 `json:"min_vol_ratio,omitempty"`
	Sizing         string  `json:"sizing,omitempty"`
	BaseStakeCents int64   `json:"base_stake_cents,omitempty"`
	Multiplier     float64 `json:"multiplier,omitempty"`
	MaxDoublings   int     `json:"max_doublings,omitempty"`
}

func (m Member) asShape() Shape {
	return Shape{
		Name: m.Name, Blurb: m.Blurb, Family: m.Family, Exit: m.Exit, Side: m.Side,
		Lambda: m.Lambda, LambdaLate: m.LambdaLate, LambdaLateTau: m.LambdaLateTau,
		StaleCost: m.StaleCost, StaleCostSell: m.StaleCostSell,
		TauMin: m.TauMin, TauMax: m.TauMax, BandMin: m.BandMin, BandMax: m.BandMax,
		MaxBets: m.MaxBets, MinGap: m.MinGap, MinHold: m.MinHold, Capture: m.Capture,
		Kappa: m.Kappa, WindowCapBps: m.WindowCapBps, MinVolRatio: m.MinVolRatio,
		Sizing: m.Sizing, BaseStakeCents: m.BaseStakeCents, Multiplier: m.Multiplier, MaxDoublings: m.MaxDoublings,
	}
}

func memberOf(s Shape) Member {
	return Member{
		Name: s.Name, Blurb: s.Blurb, Family: s.Family, Exit: s.Exit, Side: s.Side,
		Lambda: s.Lambda, LambdaLate: s.LambdaLate, LambdaLateTau: s.LambdaLateTau,
		StaleCost: s.StaleCost, StaleCostSell: s.StaleCostSell,
		TauMin: s.TauMin, TauMax: s.TauMax, BandMin: s.BandMin, BandMax: s.BandMax,
		MaxBets: s.MaxBets, MinGap: s.MinGap, MinHold: s.MinHold, Capture: s.Capture,
		Kappa: s.Kappa, WindowCapBps: s.WindowCapBps, MinVolRatio: s.MinVolRatio,
		Sizing: s.Sizing, BaseStakeCents: s.BaseStakeCents, Multiplier: s.Multiplier, MaxDoublings: s.MaxDoublings,
	}
}

// FromShape builds the version. Every number the shape sets is a convention that names the
// builder as its source; every number it leaves at zero is inherited from the parent, as the
// parent's provenance says. The result passes Validate or an error says which field did not.
func FromShape(s Shape) (Params, error) {
	if strings.TrimSpace(s.Name) == "" {
		return Params{}, fmt.Errorf("the version has no name")
	}
	if s.Exit != "hold" && s.Exit != "ev" {
		return Params{}, fmt.Errorf("exit must be hold or ev")
	}
	switch s.Family {
	case "", FamilyRounds, FamilyLadders:
	default:
		return Params{}, fmt.Errorf("family must be %s or %s", FamilyRounds, FamilyLadders)
	}
	if !(s.Lambda > 0) {
		return Params{}, fmt.Errorf("lambda must be above 0 (the protocol's R1: a version at 0 is not registered)")
	}
	conv := func(v float64, what string) Provenance {
		return Provenance{Kind: KindConvention, Value: v, Note: "the builder's setting, chosen by the owner: " + what}
	}
	m := Measured{
		Lambda:        conv(s.Lambda, "the weight on the model against the market"),
		StaleCost:     conv(s.StaleCost, "the staleness cost charged to a buy; not a frozen measurement"),
		StaleCostSell: conv(s.StaleCostSell, "the staleness cost charged to a sale; not a frozen measurement"),
		DriftTol:      Provenance{Kind: KindFact, Value: 0, Note: "no reference engine runs beside the third; the drift gate is open and the number decides nothing"},
	}
	var p Params
	if s.Exit == "ev" {
		p = scalper(m)
	} else {
		p = value(m)
	}
	p.Basis = BasisConvention
	name := strings.TrimSpace(s.Name)
	if !strings.HasSuffix(name, ConventionSuffix) {
		name += ConventionSuffix
	}
	p.Name = name
	if b := strings.TrimSpace(s.Blurb); b != "" {
		p.Blurb = b
	} else {
		p.Blurb = strings.Replace(p.Blurb, "the measured weight", "the owner's weight", 1)
	}
	p.Blurb += "; built on the buckets page, every number a convention"
	if s.Family == FamilyLadders {
		p.Family = FamilyLadders
		p.Blurb += "; trades the daily and weekly ladders"
	}
	if s.Control {
		p.Blurb += "; a NEGATIVE CONTROL, registered to be caught"
	}

	set := func(key string, v float64, what string) { p.Provenance[key] = conv(v, what) }
	if s.LambdaLateTau > 0 || s.LambdaLate != 0 {
		// Both are recorded, lambda_late even at 0: a zero with provenance is a chosen number,
		// a zero without one is "not used". Validate refuses the pair when tau is missing.
		p.LambdaLate, p.LambdaLateTau = s.LambdaLate, s.LambdaLateTau
		set("lambda_late", s.LambdaLate, "the weight on the model inside the late window")
		set("lambda_late_tau", s.LambdaLateTau, "seconds before the close from which lambda_late applies")
		p.Blurb += fmt.Sprintf("; inside %v s of the close the model's weight is %v", s.LambdaLateTau, s.LambdaLate)
	}
	if s.TauMin > 0 {
		p.TauMin = s.TauMin
		set("tau_min", s.TauMin, "no entry with fewer seconds than this to the close")
	}
	if s.TauMax > 0 {
		p.TauMax = s.TauMax
		set("tau_max", s.TauMax, "no entry with more seconds than this to the close")
	}
	if s.BandMin > 0 || s.BandMax > 0 {
		if s.BandMax <= 0 {
			s.BandMax = p.BandMax
		}
		p.BandMin, p.BandMax = s.BandMin, s.BandMax
		set("band_min", s.BandMin, "the lowest ask it buys at")
		set("band_max", s.BandMax, "the highest ask it buys at")
	}
	if s.MaxBets > 0 {
		p.MaxBets = s.MaxBets
		set("max_bets", float64(s.MaxBets), "filled buys per round, at most")
	}
	if s.MinGap > 0 {
		p.MinGap = s.MinGap
		set("min_gap", s.MinGap, "seconds between filled buys in one market")
	}
	if s.Exit == "ev" {
		if s.MinHold > 0 {
			p.MinHold = s.MinHold
			set("min_hold", s.MinHold, "seconds after the last buy before it may sell")
		}
		if s.Capture > 0 {
			p.TakeCapture = s.Capture
			set("take_capture", s.Capture, "sell once the bid covers this share of the way to a dollar")
		}
	}
	if s.Kappa > 0 {
		p.Kappa = s.Kappa
		set("kappa", s.Kappa, "the Kelly fraction staked; a risk preference")
	}
	if s.WindowCapBps > 0 {
		p.WindowCapBps = s.WindowCapBps
		p.Provenance["window_cap_bps"] = Provenance{Kind: KindLimit, Value: float64(s.WindowCapBps), Note: "the owner's limit on one window, in basis points of min(equity, seed)"}
	}
	if s.Side != "" && s.Side != SideModel {
		p.Side = s.Side
	}
	if s.MinVolRatio > 0 {
		p.MinVolRatio = s.MinVolRatio
		set("min_vol_ratio", s.MinVolRatio, "entries only while the model's volatility is at least this many times the coin's default")
	}
	if s.Sizing == SizingMartingale {
		p.Sizing = SizingMartingale
		p.BaseStakeCents, p.Multiplier, p.MaxDoublings = s.BaseStakeCents, s.Multiplier, s.MaxDoublings
		set("base_stake_cents", float64(s.BaseStakeCents), "the martingale's first stake")
		set("multiplier", s.Multiplier, "the stake is multiplied by this after each settled loss")
		set("max_doublings", float64(s.MaxDoublings), "the most consecutive multiplications; the window cap bounds it too")
	}
	if len(s.Members) > 0 {
		if err := attachMembers(&p, s, set); err != nil {
			return Params{}, err
		}
	}
	return p, p.Validate()
}

// attachMembers builds each member and labels the pick. A member Shape may not itself carry members.
func attachMembers(p *Params, s Shape, set func(string, float64, string)) error {
	p.Members = make([]Member, 0, len(s.Members))
	for i, m := range s.Members {
		if strings.TrimSpace(m.Name) == "" {
			m.Name = fmt.Sprintf("member %d", i+1)
		}
		if _, err := FromShape(m.asShape()); err != nil {
			return fmt.Errorf("member %q: %w", m.Name, err)
		}
		p.Members = append(p.Members, m)
	}
	p.StructuralOnly = s.StructuralOnly
	p.Assign = s.Assign
	if p.Assign == "" {
		p.Assign = AssignBoth
	}
	switch {
	case p.Assign == AssignReserve:
		if s.LookbackWindows != 0 {
			return fmt.Errorf("assign=reserve does not use lookback_windows")
		}
		if s.StructuralOnly {
			return fmt.Errorf("assign=reserve has no window owner; structural_only is for window or both")
		}
		p.LookbackWindows = 0
		p.StructuralOnly = false
		p.Blurb += fmt.Sprintf("; a roster of %d, reservation only: sit until the latest specialist's clock", len(p.Members))
	case s.StructuralOnly:
		p.LookbackWindows = 0
		p.Blurb += fmt.Sprintf("; a roster of %d, window owner always the first member", len(p.Members))
		if p.Assign == AssignBoth {
			p.Blurb += ", later specialists may take unclaimed seats"
		}
	default:
		k := s.LookbackWindows
		if k == 0 {
			k = DefaultLookback
		}
		p.LookbackWindows = int64(k)
		set("lookback_windows", float64(k), "prior 15-minute clocks the adaptive window owner needs")
		switch p.Assign {
		case AssignWindow:
			p.Blurb += fmt.Sprintf("; a roster of %d, one window owner from the last %d clocks, no leftovers", len(p.Members), k)
		default:
			p.Blurb += fmt.Sprintf("; a roster of %d, window owner from the last %d clocks then later specialists on unclaimed seats", len(p.Members), k)
		}
	}
	return nil
}

// ToShape is the builder's form filled from a registered version: every knob as the version
// has it, so a remix starts from what actually traded, not from a preset. The name loses its
// label (the builder adds it back); the hypothesis is left blank, because a remix tests something
// new and the registry wants that said. Registered again through FromShape, every number becomes
// the owner's convention, which is what choosing to keep it means.
func ToShape(p Params) Shape {
	s := Shape{
		Name:            strings.TrimSuffix(p.Name, ConventionSuffix),
		Blurb:           "",
		Family:          p.Family,
		Exit:            p.Exit,
		Side:            p.Side,
		Lambda:          p.Lambda,
		LambdaLate:      p.LambdaLate,
		LambdaLateTau:   p.LambdaLateTau,
		StaleCost:       p.StaleCost,
		StaleCostSell:   p.StaleCostSell,
		TauMin:          p.TauMin,
		TauMax:          p.TauMax,
		BandMin:         p.BandMin,
		BandMax:         p.BandMax,
		MaxBets:         p.MaxBets,
		MinGap:          p.MinGap,
		MinHold:         p.MinHold,
		Capture:         p.TakeCapture,
		Kappa:           p.Kappa,
		WindowCapBps:    p.WindowCapBps,
		MinVolRatio:     p.MinVolRatio,
		Sizing:          p.Sizing,
		BaseStakeCents:  p.BaseStakeCents,
		Multiplier:      p.Multiplier,
		MaxDoublings:    p.MaxDoublings,
		LookbackWindows: int(p.LookbackWindows),
		StructuralOnly:  p.StructuralOnly,
		Assign:          p.Assign,
	}
	s.Members = append([]Member(nil), p.Members...)
	return s
}

// Preset is a named Shape with a sentence about what it tests.
type Preset struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	What  string `json:"what"`
	Shape Shape  `json:"shape"`
}

// Presets are the standard shapes the builder offers, filled with the parents' settings and the
// owner's conventions of 2026-09-22 (lambda 0.5, staleness 0.0012). Every one is a starting
// point for the form, not a recommendation; each registered from it is its own trial.
func Presets() []Preset {
	base := func(name, exit string) Shape {
		return Shape{Name: name, Exit: exit, Lambda: 0.5, StaleCost: 0.0012}
	}
	list := []Preset{
		{"value", "Value", "Belief beats price after costs; one bet a round, held to settlement. The parent shape.",
			base("Value", "hold")},
		{"late", "Late", "The market lags spot in the last minutes: the same rule, entries only inside 150 seconds of the close.",
			func() Shape { s := base("Late", "hold"); s.TauMax = 150; s.MaxBets = 2; return s }()},
		{"favourite", "Favourite", "The favourite-longshot bias: back only the side priced above one half, in the 0.62 to 0.88 band, inside 240 seconds.",
			func() Shape {
				s := base("Favourite", "hold")
				s.Side = SideFavourite
				s.BandMin, s.BandMax = 0.62, 0.88
				s.TauMax = 240
				return s
			}()},
		{"model", "Model", "Trusts the model more: three bets a round, held.",
			func() Shape { s := base("Model", "hold"); s.MaxBets = 3; return s }()},
		{"scalper", "Scalper", "Trades often and banks small gains: sells on value or once the bid covers 80% of the way to a dollar.",
			func() Shape {
				s := base("Scalper", "ev")
				s.StaleCostSell, s.TauMin, s.MaxBets, s.MinGap, s.MinHold, s.Capture = 0.0012, 25, 25, 8, 5, 0.80
				return s
			}()},
		{"calm-scalper", "Calm Scalper", "Scalper with a quarter of the appetite: five bets a round and a longer gap, so fees bite less.",
			func() Shape {
				s := base("Calm Scalper", "ev")
				s.StaleCostSell, s.TauMin, s.MaxBets, s.MinGap, s.MinHold, s.Capture = 0.0012, 25, 5, 30, 10, 0.80
				return s
			}()},
		{"tail", "Tail", "Cheap sides when the market is moving: longshots under 0.20, only while volatility is at least 1.5 times the coin's default.",
			func() Shape {
				s := base("Tail", "hold")
				s.Side, s.BandMin, s.BandMax, s.MinVolRatio, s.TauMin = SideLongshot, 0.02, 0.20, 1.5, 120
				return s
			}()},
		{"martingale-control", "Martingale control", "A NEGATIVE CONTROL: Value's entries with a fixed $10 stake doubled after each settled loss, up to five times. Registered to show the drawdown and one-window checks catch it, not to make money.",
			func() Shape {
				s := base("Martingale control", "hold")
				s.Sizing, s.BaseStakeCents, s.Multiplier, s.MaxDoublings, s.Control = SizingMartingale, 1000, 2, 5, true
				return s
			}()},
		// The ladders: the same belief against the ask, priced with the coin's realised daily
		// volatility, on markets that close hours to days out. tau is in seconds: 3 h = 10800,
		// 30 h = 108000, 7 d = 604800. One bet per leg; the window cap is per close date.
		{"day-value", "Day Value (ladders)", "Value's rule on the daily ladders: buys a leg where the belief beats the ask after costs, between 30 and 3 hours before its 5 pm ET close, one bet a leg, held to settlement.",
			func() Shape {
				s := base("Day Value", "hold")
				s.Family, s.TauMin, s.TauMax, s.BandMin, s.BandMax = FamilyLadders, 10800, 108000, 0.05, 0.95
				return s
			}()},
		{"day-favourite", "Day Favourite (ladders)", "The favourite-longshot bias on the daily ladders: the side priced 0.60 to 0.90 only, between 30 and 3 hours before the close, one bet a leg, held.",
			func() Shape {
				s := base("Day Favourite", "hold")
				s.Family, s.Side, s.TauMin, s.TauMax, s.BandMin, s.BandMax = FamilyLadders, SideFavourite, 10800, 108000, 0.60, 0.90
				return s
			}()},
		{"week-value", "Week Value (ladders)", "Value's rule on the weekly ladders: legs one to seven days from the close, priced with the coin's realised daily volatility, one bet a leg, held.",
			func() Shape {
				s := base("Week Value", "hold")
				s.Family, s.TauMin, s.TauMax, s.BandMin, s.BandMax = FamilyLadders, 86400, 604800, 0.05, 0.95
				return s
			}()},
	}
	sort.SliceStable(list, func(i, j int) bool { return i < j })
	return list
}
