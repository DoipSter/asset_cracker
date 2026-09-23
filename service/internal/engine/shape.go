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

	Exit string `json:"exit" jsonschema:"hold: every position is held to settlement (parent Value). ev: sells on value or once the bid covers take_capture of the way to a dollar (parent Scalper)"`
	Side string `json:"side,omitempty" jsonschema:"which side it may buy: model (whichever the belief favours; the default), favourite (the side priced above one half only), longshot (under one half only)"`

	Lambda        float64 `json:"lambda" jsonschema:"weight on the model against the market, 0 to 1 exclusive of 0; the owner's convention is 0.5"`
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
	if s.Control {
		p.Blurb += "; a NEGATIVE CONTROL, registered to be caught"
	}

	set := func(key string, v float64, what string) { p.Provenance[key] = conv(v, what) }
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
	return p, p.Validate()
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
	}
	sort.SliceStable(list, func(i, j int) bool { return i < j })
	return list
}
