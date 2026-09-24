package engine

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
)

// The kinds of number a version may carry (plan section 6.1). Every numeric field of Params must
// say which it is, so that a guess can never travel as a measurement.
const (
	KindMeasured   = "measured"   // from the recorded history, by the written protocol
	KindConvention = "convention" // a chosen number; a risk preference or a comparability choice
	KindInherited  = "inherited"  // taken unchanged from the parent version; UNMEASURED
	KindLimit      = "limit"      // the owner's limit
	KindFact       = "fact"       // a property of the venue or of what is recorded

	// KindPlaceholder is NOT a kind a version may be registered with. It marks a number that
	// stands in for a measurement that has not been made: test fixtures, and the dev-database
	// plumbing versions of step S5. Validate refuses it; only ValidatePlumbing lets it through,
	// and an engine built that way stamps "plumbing": true on every order it forms.
	KindPlaceholder = "placeholder"
)

// Provenance says where one number came from. Value repeats the number so that the params JSON
// stored with the version cannot say one thing in the field and another in its provenance.
type Provenance struct {
	Kind  string  `json:"kind"`
	Value float64 `json:"value"`
	Note  string  `json:"note"`
}

// Params is one version's settings, frozen with where each came from. The JSON keys are the ones
// stored in strategy_version.params, and the keys of Provenance are those same JSON keys.
//
// There is no zero value that works and no default for the four numbers that must be measured
// (Lambda, StaleCost, StaleCostSell, DriftTol): see Scalper and Value.
type Params struct {
	Name  string `json:"name"`
	Blurb string `json:"blurb"`
	// Basis says where the four numbers of Measured come from: "measured" (the protocol's
	// result; the default, and the empty string reads as it) or "convention" (the owner chose
	// them to run the fund before the measurement was complete; 2026-09-22). A convention
	// version must say so in its name, so a page never shows it as a measured one.
	Basis string `json:"basis,omitempty"`

	// Lambda is the weight on the model in p = mid + Lambda * (p_model - mid). One number for
	// every version, because it is a property of the model, not of a strategy. [MEASURED, M1]
	Lambda float64 `json:"lambda"`
	// LambdaLate, with LambdaLateTau above 0, is the weight on the model INSIDE LambdaLateTau
	// seconds of the close (tau <= LambdaLateTau); Lambda applies outside it. Both zero, with no
	// provenance, means one lambda for the whole round, exactly as before they existed. A shape
	// of the builder (2026-09-23), for convention versions only: the protocol measures ONE
	// lambda, so a measured version never carries these. Why a shape at all: on 162 windows
	// the scorecard found the model's skill against the mid to depend on the horizon (worse at
	// 5 to 10 minutes, far better inside 2), which one number cannot express. [CONVENTION when set]
	LambdaLate    float64 `json:"lambda_late,omitempty"`
	LambdaLateTau float64 `json:"lambda_late_tau,omitempty"`
	// StaleCost is what a second-old ask costs a buyer, in dollars per contract, charged in the
	// entry test and in the Kelly fraction, never in the ledger. [MEASURED on v2's buys: a proxy]
	StaleCost float64 `json:"stale_cost"`
	// StaleCostSell is the same for a seller, charged in both exit tests. Zero, with no
	// provenance, for a version that never sells early. [MEASURED on v2's sells: a proxy]
	StaleCostSell float64 `json:"stale_cost_sell"`
	// DriftTol is how far this package's p_model may be from the second engine's before entries
	// stop (plan 4.3). It is handed to NewModel. [MEASURED on dev in S5]
	DriftTol float64 `json:"drift_tol"`

	Kappa          float64 `json:"kappa"`           // the Kelly fraction staked [CONVENTION: a quarter]
	WindowCapBps   int64   `json:"window_cap_bps"`  // most of min(E_w, seed) one window may use [LIMIT]
	SeedCents      int64   `json:"seed_cents"`      // the bucket's starting balance [CONVENTION]; a bucket seeded at another figure sizes off its own (Account.Seed)
	ExhaustedCents int64   `json:"exhausted_cents"` // under this with nothing open, the account has run out [INHERITED]

	TauMin  float64 `json:"tau_min"` // seconds to the close between which it may enter [INHERITED]
	TauMax  float64 `json:"tau_max"`
	BandMin float64 `json:"band_min"` // the ask must lie in this band to be bought [INHERITED]
	BandMax float64 `json:"band_max"`
	MaxBets int     `json:"max_bets"` // buy orders WITH A FILL per market [INHERITED]
	MinGap  float64 `json:"min_gap"`  // seconds between filled buys in one market [INHERITED]
	MinHold float64 `json:"min_hold"` // seconds after the last buy before it may sell [INHERITED]
	MinTau  float64 `json:"min_tau"`  // under this many seconds to the close it sends nothing [INHERITED]

	Exit        string  `json:"exit"`         // "hold": never sells early. "ev": the value and capture rules
	TakeCapture float64 `json:"take_capture"` // sell once the bid covers this share of the way to a dollar [INHERITED]

	// The builder's shapes (2026-09-22). Each is a FILTER or a SIZING on top of the one entry rule;
	// none of them lets an order through that the belief does not favour after costs.
	//
	// Side restricts which side may be bought: "" or "model", whichever the blend favours;
	// "favourite", only the side the market prices above one half (the favourite-longshot bias:
	// backing favourites, which is the same act as fading longshots); "longshot", only the side
	// under one half (the tail shape).
	Side string `json:"side,omitempty"`
	// MinVolRatio, when above 0, blocks entries while the model's volatility is under this many
	// times the coin's calibrated default: the tail shape buys cheap sides only when the market
	// is moving enough to reach them. [CONVENTION when set]
	MinVolRatio float64 `json:"min_vol_ratio,omitempty"`
	// Sizing is "" or "kelly" (the fraction kappa of the edge, plan 4.5) or "martingale": a fixed
	// stake of BaseStakeCents times Multiplier for each consecutive settled loss the bucket has
	// just had, up to MaxDoublings, and back to the base after a win. The window cap and the
	// entry rule still apply, so this is a BOUNDED martingale that only bets where the belief has
	// an edge: it changes how much, never whether. Built as a negative control, to show the
	// drawdown and one-window checks catch what it does to a record. [CONVENTION when set]
	Sizing         string  `json:"sizing,omitempty"`
	BaseStakeCents int64   `json:"base_stake_cents,omitempty"`
	Multiplier     float64 `json:"multiplier,omitempty"`
	MaxDoublings   int     `json:"max_doublings,omitempty"`

	// Family is which markets the version trades: "" or "kalshi15m", the 15-minute rounds;
	// "kalshiladder", the daily and weekly above/below ladders (many legs per coin open at once,
	// hours to days from their close, priced with the coin's long volatility past an hour). The
	// family decides which runner holds the version's bucket; the rules above are the same.
	Family string `json:"family,omitempty"`

	Levels     int  `json:"levels"`       // recorded levels an order may walk [FACT: five are recorded]
	FeePerFill bool `json:"fee_per_fill"` // the pessimistic fee rounding; copied into the Paper by whoever builds it

	// A roster (2026-09-23): Members, when two or more, make this one version that picks among
	// them. LookbackWindows is K for the adaptive pick (0 with StructuralOnly: roster order).
	Members         []Member `json:"members,omitempty"`
	LookbackWindows int64    `json:"lookback_windows,omitempty"`
	StructuralOnly  bool     `json:"structural_only,omitempty"`

	Provenance  map[string]Provenance `json:"provenance"`
	ProtocolSHA string                `json:"protocol_sha"` // sha-256 of docs/v3-measurement-protocol.md
	ResultSHA   string                `json:"result_sha"`   // sha-256 of research/v3/frozen-params.json
}

// Measured is the four numbers this package refuses to invent, each with where it came from, and
// the two hashes that tie them to the protocol and to its result. cmd/measure3 produces it.
type Measured struct {
	Lambda, StaleCost, StaleCostSell, DriftTol Provenance
	ProtocolSHA, ResultSHA                     string
}

// fields that must be "measured" in a version that may be registered.
var measuredFields = []string{"lambda", "stale_cost", "stale_cost_sell", "drift_tol"}

// fields a version that never sells early leaves at zero, with no provenance.
var exitOnlyFields = map[string]bool{"stale_cost_sell": true, "min_hold": true, "take_capture": true}

// The builder's shape fields: zero, with no provenance, means "not used", so a version stored
// before they existed still validates. Set, each needs provenance like every other number.
var shapeFields = map[string]bool{"min_vol_ratio": true, "base_stake_cents": true, "multiplier": true, "max_doublings": true,
	"lambda_late": true, "lambda_late_tau": true, "lookback_windows": true}

// LambdaAt is the weight on the model at tau seconds to the close: LambdaLate inside
// LambdaLateTau (tau at or under it), else Lambda. With no late window it is always Lambda.
func (p Params) LambdaAt(tau float64) float64 {
	if p.LambdaLateTau > 0 && tau <= p.LambdaLateTau {
		return p.LambdaLate
	}
	return p.Lambda
}

// HasLateLambda reports a version whose weight on the model changes inside the round.
func (p Params) HasLateLambda() bool { return p.LambdaLateTau > 0 }

// The values Side and Sizing may take.
const (
	SideModel     = "model"
	SideFavourite = "favourite"
	SideLongshot  = "longshot"

	SizingKelly      = "kelly"
	SizingMartingale = "martingale"

	// The market families. FamilyRounds is the default and the empty string reads as it.
	FamilyRounds  = "kalshi15m"
	FamilyLadders = "kalshiladder"
)

// FamilyOf is a version's family with the default made explicit.
func (p Params) FamilyOf() string {
	if p.Family == "" {
		return FamilyRounds
	}
	return p.Family
}

func inherited(v float64, note string) Provenance {
	return Provenance{Kind: KindInherited, Value: v, Note: note}
}

// common is everything the two versions share (plan section 6.4). Every number here that is not
// handed in through Measured is a convention, a limit, a fact or inherited, and says so.
func common(m Measured) Params {
	const parent = "unmeasured; taken unchanged from the parent v2 so that parent and child differ only in fills, sizing and belief"
	p := Params{
		Lambda: m.Lambda.Value, StaleCost: m.StaleCost.Value, DriftTol: m.DriftTol.Value,
		Kappa: 0.25, WindowCapBps: 2500, SeedCents: 100000, ExhaustedCents: 100,
		TauMax: 900, BandMin: 0.05, BandMax: 0.95, MinTau: 8, Levels: 5,
		ProtocolSHA: m.ProtocolSHA, ResultSHA: m.ResultSHA,
	}
	p.Provenance = map[string]Provenance{
		"lambda": m.Lambda, "stale_cost": m.StaleCost, "drift_tol": m.DriftTol,
		"kappa":           {KindConvention, 0.25, "quarter Kelly: a risk preference, not measurable"},
		"window_cap_bps":  {KindLimit, 2500, "the owner's limit; v2's TotalCap, on min(E_w, seed)"},
		"seed_cents":      {KindConvention, 100000, "the same $1,000 as v2, so the lines compare"},
		"exhausted_cents": inherited(100, "v2's BankruptAt"),
		"tau_max":         inherited(900, parent),
		"band_min":        inherited(0.05, parent),
		"band_max":        inherited(0.95, parent),
		"min_tau":         inherited(8, parent),
		"levels":          {KindFact, 5, "a snapshot records five bid levels per side"},
	}
	return p
}

func scalper(m Measured) Params {
	const parent = "unmeasured; taken unchanged from Scalper v2"
	p := common(m)
	p.Name, p.Blurb, p.Exit = "Scalper", "Trades often, banks small gains; fills only what the book displayed", "ev"
	p.StaleCostSell = m.StaleCostSell.Value
	p.TauMin, p.MaxBets, p.MinGap, p.MinHold, p.TakeCapture = 25, 25, 8, 5, 0.80
	p.Provenance["stale_cost_sell"] = m.StaleCostSell
	p.Provenance["tau_min"] = inherited(25, parent)
	p.Provenance["max_bets"] = inherited(25, parent+"; counts buy orders with a fill")
	p.Provenance["min_gap"] = inherited(8, parent)
	p.Provenance["min_hold"] = inherited(5, parent)
	p.Provenance["take_capture"] = inherited(0.80, parent+"; the review found its effect unresolved")
	return p
}

func value(m Measured) Params {
	const parent = "unmeasured; taken unchanged from Value v2"
	p := common(m)
	p.Name, p.Blurb, p.Exit = "Value", "Model and market blended at the measured weight; holds to settlement", "hold"
	p.TauMin, p.MaxBets, p.MinGap = 8, 1, 20
	p.Provenance["tau_min"] = inherited(8, parent)
	p.Provenance["max_bets"] = inherited(1, parent+"; counts buy orders with a fill")
	p.Provenance["min_gap"] = inherited(20, parent)
	return p
}

// Scalper is Scalper v3. It cannot be had without the measured numbers: a Measured whose lambda,
// staleness costs or drift tolerance is not of kind "measured" is refused.
func Scalper(m Measured) (Params, error) { p := scalper(m); return p, p.Validate() }

// Value is Value v3, under the same rule.
func Value(m Measured) (Params, error) { p := value(m); return p, p.Validate() }

// PlumbingScalper and PlumbingValue are the same two versions with placeholder numbers let
// through. They exist for tests and for the dev database's plumbing versions (step S5), whose
// results mean nothing and are labelled so. They must never be registered on prod.
func PlumbingScalper(m Measured) (Params, error) { p := scalper(m); return p, p.ValidatePlumbing() }
func PlumbingValue(m Measured) (Params, error)   { p := value(m); return p, p.ValidatePlumbing() }

// BasisConvention marks a version whose four numbers the owner chose (Conventions).
const BasisConvention = "convention"

// ConventionSuffix is what a convention version's name must end with.
const ConventionSuffix = " (conventions)"

// Conventions are the four numbers as the owner chose them, each of kind "convention" with a
// note saying why, for a version that runs the fund BEFORE the protocol's measurement is
// complete (the owner's decision of 2026-09-22 00:48 PT). Such a version is its own trial: it is
// registered under its own name, "Scalper (conventions)", so that the measured Scalper v3 keeps
// its slot, and the trials count moves for both. Nothing about it is a measurement, and its
// params say so in every field.
type Conventions struct {
	Lambda, StaleCost, StaleCostSell Provenance // kind "convention"
	DriftTol                         Provenance // kind "fact", 0: the gate is open (third amendment)
}

// ConventionScalper is Scalper with the owner's four numbers; ConventionValue likewise.
func ConventionScalper(c Conventions) (Params, error) { return convention(scalper, c) }
func ConventionValue(c Conventions) (Params, error)   { return convention(value, c) }

func convention(build func(Measured) Params, c Conventions) (Params, error) {
	p := build(Measured{Lambda: c.Lambda, StaleCost: c.StaleCost, StaleCostSell: c.StaleCostSell, DriftTol: c.DriftTol})
	p.Basis = BasisConvention
	p.Name += ConventionSuffix
	p.Blurb = strings.Replace(p.Blurb, "the measured weight", "the owner's weight", 1) + "; the four numbers are the owner's conventions, not the protocol's measurement"
	return p, p.Validate()
}

// Validate refuses a version that may not be registered: a numeric field without provenance, a
// provenance that disagrees with its field, a placeholder anywhere, or one of the four measured
// numbers not marked measured and tied to a protocol and a result.
func (p Params) Validate() error { return p.validate(false) }

// ValidatePlumbing is Validate with placeholders allowed. See KindPlaceholder.
func (p Params) ValidatePlumbing() error { return p.validate(true) }

// Placeholders lists the fields that stand in for a measurement, sorted. Empty for a real version.
func (p Params) Placeholders() []string {
	var out []string
	for key, pr := range p.Provenance {
		if pr.Kind == KindPlaceholder {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// numericFields is every numeric field of Params by its JSON key. Reflection, so that a field
// added later cannot escape the provenance rule by being forgotten here.
func (p Params) numericFields() map[string]float64 {
	out := map[string]float64{}
	v, t := reflect.ValueOf(p), reflect.TypeOf(p)
	for i := 0; i < t.NumField(); i++ {
		key := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		switch f := v.Field(i); f.Kind() {
		case reflect.Float64, reflect.Float32:
			out[key] = f.Float()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			out[key] = float64(f.Int())
		}
	}
	return out
}

func (p Params) validate(plumbing bool) error {
	var bad []string
	fail := func(format string, a ...any) { bad = append(bad, fmt.Sprintf(format, a...)) }

	if p.Name == "" {
		fail("the version has no name")
	}
	if p.Exit != "hold" && p.Exit != "ev" {
		fail("exit is %q, not hold or ev", p.Exit)
	}
	kinds := map[string]bool{KindMeasured: true, KindConvention: true, KindInherited: true, KindLimit: true, KindFact: true}
	fields := p.numericFields()
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	usesMeasured := false
	for _, key := range keys {
		val := fields[key]
		if math.IsNaN(val) || math.IsInf(val, 0) {
			fail("%s is not a number", key)
			continue
		}
		pr, has := p.Provenance[key]
		if p.Exit == "hold" && exitOnlyFields[key] {
			if val != 0 || has {
				fail("%s is set, but a version that never sells early has no use for it", key)
			}
			continue
		}
		if shapeFields[key] && val == 0 && !has {
			continue // the shape is not used
		}
		switch {
		case !has:
			fail("%s has no provenance", key)
			continue
		case pr.Value != val:
			fail("%s is %v but its provenance says %v", key, val, pr.Value)
		case pr.Note == "":
			fail("%s: the provenance has no note", key)
		case pr.Kind == KindPlaceholder && !plumbing:
			fail("%s is a placeholder, not a measurement: this version may not be constructed", key)
		case pr.Kind != KindPlaceholder && !kinds[pr.Kind]:
			fail("%s: %q is not a kind of provenance", key, pr.Kind)
		}
		usesMeasured = usesMeasured || pr.Kind == KindMeasured
	}
	for key := range p.Provenance {
		if _, ok := fields[key]; !ok {
			fail("provenance for %s, which is not a numeric field", key)
		}
	}
	switch p.Basis {
	case "", "measured":
		for _, key := range measuredFields {
			pr, has := p.Provenance[key]
			if !has || pr.Kind == KindMeasured || pr.Kind == KindPlaceholder {
				continue
			}
			// The protocol's third amendment (2026-09-22): drift_tol has no reference engine to be
			// measured against, so it is a fact, 0, and the gate it fed is open. That one field, and
			// only as a fact of exactly 0; the other three stay measured.
			if key == "drift_tol" && pr.Kind == KindFact && pr.Value == 0 {
				continue
			}
			fail("%s must be measured; it is labelled %q", key, pr.Kind)
		}
		if usesMeasured && (p.ProtocolSHA == "" || p.ResultSHA == "") {
			fail("measured numbers need the protocol's and the result's sha")
		}
	case BasisConvention:
		// The owner's numbers: each of the three is a convention that says so, drift_tol is the
		// fact 0, nothing is called measured, and the name carries the label.
		if !strings.HasSuffix(p.Name, ConventionSuffix) {
			fail("a convention version's name must end with %q", ConventionSuffix)
		}
		if usesMeasured {
			fail("a convention version may not call any number measured")
		}
		for _, key := range []string{"lambda", "stale_cost", "stale_cost_sell"} {
			if pr, has := p.Provenance[key]; has && pr.Kind != KindConvention && pr.Kind != KindPlaceholder {
				fail("%s must be a convention in a convention version; it is labelled %q", key, pr.Kind)
			}
		}
		if pr := p.Provenance["drift_tol"]; !(pr.Kind == KindFact && pr.Value == 0) && pr.Kind != KindPlaceholder {
			fail("drift_tol must be the fact 0 (the gate is open); it is %q %v", pr.Kind, pr.Value)
		}
	default:
		fail("basis %q is neither measured nor convention", p.Basis)
	}

	// Ranges. Written as !(ok) so that a NaN fails.
	if !(p.Lambda >= 0 && p.Lambda <= 1) {
		fail("lambda %v is outside 0..1", p.Lambda)
	}
	if p.Lambda == 0 && !plumbing {
		fail("lambda is 0: the protocol's rule R1 says no version is registered")
	}
	// The late window: both numbers or neither, the weight in 0..1 (0 is allowed: inside the
	// window the belief is the mid, and nothing is entered there), and only in a convention
	// version, since the protocol freezes one lambda.
	switch {
	case p.LambdaLateTau < 0 || math.IsNaN(p.LambdaLateTau):
		fail("lambda_late_tau %v is negative", p.LambdaLateTau)
	case p.LambdaLateTau == 0 && p.LambdaLate != 0:
		fail("lambda_late is set without lambda_late_tau: say how many seconds before the close it applies")
	case p.LambdaLateTau > 0:
		if !(p.LambdaLate >= 0 && p.LambdaLate <= 1) {
			fail("lambda_late %v is outside 0..1", p.LambdaLate)
		}
		if p.Basis != BasisConvention && !plumbing {
			fail("lambda_late is a builder's shape for convention versions; a measured version has one lambda")
		}
	}
	for key, v := range map[string]float64{"stale_cost": p.StaleCost, "stale_cost_sell": p.StaleCostSell} {
		if _, ok := priceUnits(v); !ok || !(v >= 0 && v < 1) {
			fail("%s %v is not a whole number of ten-thousandths of a dollar in 0..1", key, v)
		}
	}
	if !(p.DriftTol >= 0 && p.DriftTol <= 1) {
		fail("drift_tol %v is outside 0..1", p.DriftTol)
	}
	// The shapes.
	switch p.Side {
	case "", SideModel, SideFavourite, SideLongshot:
	default:
		fail("side %q is not model, favourite or longshot", p.Side)
	}
	switch p.Family {
	case "", FamilyRounds:
	case FamilyLadders:
		// A ladder leg is open for days; the round's default gate (900 s) would let a ladder
		// version enter only in the last fifteen minutes of a week-long market, which is not
		// what anyone building one means. The shape must say its window.
		if p.TauMax <= LongHorizon {
			fail("a %s version needs tau_max above %v seconds (an hour): its markets close hours to days out", FamilyLadders, LongHorizon)
		}
	default:
		fail("family %q is not %s or %s", p.Family, FamilyRounds, FamilyLadders)
	}
	if !(p.MinVolRatio >= 0 && p.MinVolRatio <= 100) {
		fail("min_vol_ratio %v is outside 0..100", p.MinVolRatio)
	}
	switch p.Sizing {
	case "", SizingKelly:
		if p.BaseStakeCents != 0 || p.Multiplier != 0 || p.MaxDoublings != 0 {
			fail("base_stake_cents, multiplier and max_doublings are for martingale sizing only")
		}
	case SizingMartingale:
		if p.BaseStakeCents < 100 || p.BaseStakeCents > p.SeedCents {
			fail("base_stake_cents %d must be at least 100 and at most the seed", p.BaseStakeCents)
		}
		if !(p.Multiplier >= 1 && p.Multiplier <= 4) {
			fail("multiplier %v is outside 1..4", p.Multiplier)
		}
		if p.MaxDoublings < 0 || p.MaxDoublings > 10 {
			fail("max_doublings %d is outside 0..10", p.MaxDoublings)
		}
	default:
		fail("sizing %q is neither kelly nor martingale", p.Sizing)
	}
	if !(p.Kappa > 0 && p.Kappa <= 1) {
		fail("kappa %v is outside 0..1", p.Kappa)
	}
	if p.WindowCapBps < 0 || p.WindowCapBps > 10000 {
		fail("window_cap_bps %d is outside 0..10000", p.WindowCapBps)
	}
	if p.SeedCents <= 0 || p.ExhaustedCents < 0 {
		fail("seed_cents must be positive and exhausted_cents not negative")
	}
	if !(p.MinTau >= 0 && p.TauMin >= 0 && p.TauMin <= p.TauMax) {
		fail("the time gates are out of order: min_tau %v, tau_min %v, tau_max %v", p.MinTau, p.TauMin, p.TauMax)
	}
	if !(p.BandMin >= 0 && p.BandMin <= p.BandMax && p.BandMax <= 1) {
		fail("the price band %v..%v is out of order", p.BandMin, p.BandMax)
	}
	if p.MaxBets < 1 || !(p.MinGap >= 0) || !(p.MinHold >= 0) {
		fail("max_bets must be at least 1, min_gap and min_hold not negative")
	}
	if !(p.TakeCapture >= 0 && p.TakeCapture < 1) {
		fail("take_capture %v is outside 0..1", p.TakeCapture)
	}
	if p.Levels < 1 || p.Levels > 5 {
		fail("levels %d is outside 1..5, and only five are recorded", p.Levels)
	}
	if err := validateComposition(p); err != nil {
		fail("%s", err.Error())
	}
	if len(bad) > 0 {
		return errors.New("engine params " + p.Name + ": " + strings.Join(bad, "; "))
	}
	return nil
}

// priceUnits turns a dollar figure that is a whole number of ten-thousandths (the staleness costs
// are frozen rounded to 0.0001) into the broker's price units. ok is false for anything finer,
// so that the integer arithmetic of prob.go never quietly rounds a parameter.
func priceUnits(dollars float64) (int64, bool) {
	scaled := dollars * 10000
	n := math.Round(scaled)
	if math.IsNaN(scaled) || math.Abs(scaled-n) > 1e-6 || math.Abs(n) > 10000 {
		return 0, false
	}
	return int64(n), true
}
