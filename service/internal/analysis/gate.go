package analysis

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
)

// The promotion gate: whether a strategy version has earned capital, decided by rule from the
// settled history and nothing else. The rule is CONFIGURATION (GateSettings, from the
// environment) plus conventions stated here; every decision is stored with the configuration it
// was made under (metric_snapshot), so a later reader can see what the bar was and whether it
// moved.
//
// Everything in this file is a pure function of figures already computed, like the rest of the
// package: nothing here can return NaN or Inf, and an undefined figure is 0.

// The bootstrap is fixed so that a figure reproduces: the same windows give the same interval on
// every refresh and on every machine. CONVENTIONS, stated in the JSON.
const (
	BootstrapResamples = 1000
	BootstrapSeed      = 20260921
)

// GateSettings is the part of the rule an operator sets. The rest of GateConfig is measured
// (trials) or a convention of this package (MinWindows, FamilyAlpha, the bootstrap).
type GateSettings struct {
	// MinEdgePerDollar is the smallest after-fee return per dollar staked worth finding: the power
	// calculation asks how many windows it takes to find an edge this size. 0.02 is two cents on
	// every dollar staked, about what the fee on an even-money contract costs.
	MinEdgePerDollar float64
	// Power is the chance the sample floor gives of finding an edge of MinEdgePerDollar when it is
	// really there. 0.8 is the usual convention.
	Power float64
	// MaxDrawdownCents is the deepest fall of a version's realised P&L from its running peak that
	// the gate allows, over every life. 25,000 is the second engine's "$250 at risk" of its
	// $1,000 bank.
	MaxDrawdownCents int64
}

// DefaultGate stands until AC_GATE_MIN_EDGE, AC_GATE_POWER or AC_GATE_MAX_DRAWDOWN_CENTS says
// otherwise (config.Load).
var DefaultGate = GateSettings{MinEdgePerDollar: 0.02, Power: 0.8, MaxDrawdownCents: 25_000}

// Valid says whether the settings can be applied: each is a number the rule can use. Invalid
// settings are not a looser gate; config.Load falls back to DefaultGate.
func (g GateSettings) Valid() bool {
	return g.MinEdgePerDollar > 0 && g.Power > 0.5 && g.Power < 1 && g.MaxDrawdownCents > 0 &&
		!math.IsInf(g.MinEdgePerDollar, 0) && !math.IsNaN(g.MinEdgePerDollar) && !math.IsNaN(g.Power)
}

// GateConfig is the whole rule as applied to one document, stored beside every decision.
type GateConfig struct {
	Note        string  `json:"note,omitempty"` // always present in the API; left out of the stored copy (Snapshots)
	MinWindows  int     `json:"min_windows"`    // the floor is this or windows_needed, whichever is more
	FamilyAlpha float64 `json:"family_alpha"`   // see Conventions
	Trials      int     `json:"trials"`         // MEASURED: the rows of strategy_version when the decision was made
	// Z is the number of standard errors the return's lower bound sits below its estimate:
	// CorrectedT(trials), the same threshold the leaderboard verdict uses, published to four
	// places. 0 when trials is 0: no bound, and no gate can be evaluated.
	Z                  float64 `json:"z"`
	MinEdgePerDollar   float64 `json:"min_edge_per_dollar"`
	Power              float64 `json:"power"`
	MaxDrawdownCents   int64   `json:"max_drawdown_cents"`
	BootstrapResamples int     `json:"bootstrap_resamples"`
	BootstrapSeed      uint64  `json:"bootstrap_seed"`
}

const gateNote = "The promotion gate, a rule chosen in advance. A strategy version passes when every check passes: " +
	"windows: it has at least min_windows settled windows with a bet, and at least windows_needed, the sample floor from a power calculation " +
	"(the windows at which a true return of min_edge_per_dollar would be found with probability `power` at threshold z, given the spread the bootstrap measured); " +
	"edge: return_lower, the after-fee return per dollar staked less z bootstrap standard errors, is above zero as published (four places); " +
	"drawdown: the deepest fall of its realised P&L from a running peak, over every life, is within max_drawdown_cents; " +
	"calibration: the model it trades on is scored by the scorecard (today: the second engine's originals only) on at least min_windows windows and does not read `model worse`. " +
	"That last check is weak and says so: it is the absence of a finding against the model, not a finding for it; a non-inferiority margin was not stated in advance. " +
	"z is the two-sided Bonferroni cut for `trials` versions, the leaderboard's own threshold, so `edge` passes exactly when the return's bootstrap t reaches what the verdict needs. " +
	"The gate is not evaluated, and nothing is stored, while coverage is partial or trials could not be read. " +
	"min_edge_per_dollar, power and max_drawdown_cents are settings (AC_GATE_MIN_EDGE, AC_GATE_POWER, AC_GATE_MAX_DRAWDOWN_CENTS); the rest is convention or measured."

// Ratio is a ratio of two sums over independent windows (P&L over money staked), with a standard
// error from resampling the windows.
type Ratio struct {
	N     int
	Value float64 // sum(num) / sum(den); 0 when sum(den) is 0
	SE    float64 // the sd of the ratio over BootstrapResamples resamples of the windows, drawn with replacement
}

// WindowRatio is the ratio of the sums with its bootstrap standard error. The windows are the
// unit resampled, so bets inside one window, and the coins in the same minutes, stay together.
// It is a ratio of sums, not a mean of per-window ratios: a window that staked a dollar and one
// that staked a hundred count by their dollars, which is what "return per dollar staked" means.
//
// With fewer than two windows, or nothing staked, there is no spread to measure and SE is 0,
// never NaN. The bootstrap is seeded from BootstrapSeed and the window count, so the same
// windows give the same SE every time. Identical windows give an SE of 0 exactly (rounding dust
// is not a spread), as WindowStat does.
func WindowRatio(num, den []float64) Ratio {
	n := min(len(num), len(den))
	if n == 0 {
		return Ratio{}
	}
	var sn, sd float64
	for i := range n {
		sn += finite(num[i])
		sd += finite(den[i])
	}
	r := Ratio{N: n}
	if sd == 0 {
		return r
	}
	r.Value = finite(sn / sd)
	if n < 2 {
		return r
	}
	rng := rand.New(rand.NewPCG(BootstrapSeed, uint64(n)))
	draws := make([]float64, 0, BootstrapResamples)
	for range BootstrapResamples {
		var bn, bd float64
		for range n {
			i := rng.IntN(n)
			bn += finite(num[i])
			bd += finite(den[i])
		}
		if bd == 0 { // every window drawn staked nothing: no ratio to take
			continue
		}
		draws = append(draws, bn/bd)
	}
	if len(draws) < 2 {
		return r
	}
	var mean float64
	for _, d := range draws {
		mean += d
	}
	mean /= float64(len(draws))
	var ss float64
	for _, d := range draws {
		ss += (d - mean) * (d - mean)
	}
	r.SE = finite(math.Sqrt(ss / float64(len(draws)-1)))
	if r.SE <= 1e-12*math.Max(1, math.Abs(r.Value)) {
		r.SE = 0
	}
	return r
}

// Drawdown is what the running total of one value per window did on its way: how far it fell
// from its running peak at the worst, where it stands against that peak now, and the worst
// single window. The running total starts at 0 (the first life's seed), so a version that only
// ever lost has a drawdown equal to its loss.
type Drawdown struct {
	MaxCents          int64 `json:"max_cents"`                  // the deepest fall from a running peak
	NowCents          int64 `json:"now_cents"`                  // the fall from the peak at the end: 0 when the last window set a new peak
	LongestUnderwater int   `json:"longest_underwater_windows"` // the most consecutive windows spent below the running peak
	WorstWindowCents  int64 `json:"worst_window_cents"`         // the single worst window; 0 if none lost
}

// DrawdownOf walks perWindow IN THE ORDER GIVEN, which must be time order (by closes): a
// drawdown is a sequence, and shuffled windows give a different one.
func DrawdownOf(perWindow []float64) Drawdown {
	var d Drawdown
	var total, peak, worst float64
	run := 0
	for _, v := range perWindow {
		v = finite(v)
		total += v
		if v < worst {
			worst = v
		}
		if total > peak {
			peak, run = total, 0
		} else if total < peak {
			run++
			if run > d.LongestUnderwater {
				d.LongestUnderwater = run
			}
		}
		if fall := peak - total; fall > float64(d.MaxCents) {
			d.MaxCents = int64(math.Round(fall))
		}
	}
	d.NowCents = int64(math.Round(peak - total))
	d.WorstWindowCents = int64(math.Round(worst))
	return d
}

// WindowsNeeded is the sample floor from a power calculation: the windows at which a true return
// of minEdge per dollar would be found, with probability `power`, by a two-sided test at z
// standard errors, given that one window's contribution to the return has spread sd. It is
// ceil(((z + z_power) x sd / minEdge)^2), with z_power the normal quantile at `power` (0.8416
// for 0.8). The brief's illustration checks it: even-money bets with sd 1, an edge of 0.04 per
// dollar (a 52% win rate), z 1.96 and power 0.5 (z_power 0) need (1.96 / 0.04)^2 = 2401.
//
// sd is the bootstrap SE times sqrt(n), so the floor and the interval rest on one measurement
// of the spread and cannot disagree. 0 when any input makes the question meaningless (no spread
// measured yet, no edge to find, no threshold, a power outside (0, 1)): the caller reads 0 as
// "no floor could be computed", which is not the same as a floor of 0.
func WindowsNeeded(sd, minEdge, z, power float64) int {
	if !(sd > 0) || !(minEdge > 0) || !(z > 0) || !(power > 0 && power < 1) {
		return 0
	}
	zp := math.Sqrt2 * math.Erfinv(2*power-1)
	n := math.Ceil(math.Pow((z+zp)*sd/minEdge, 2))
	if math.IsNaN(n) || math.IsInf(n, 0) || n > math.MaxInt32 {
		return math.MaxInt32 // more windows than there will ever be: the floor is out of reach, not absent
	}
	return int(n)
}

// Check is one of the gate's tests on one version, with the figure it looked at in words.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Why    string `json:"why"`
}

// Gate is the gate's decision on one version.
type Gate struct {
	// Evaluated is false when no decision could be made: coverage is partial, trials could not be
	// read, or the version has no settled window with a bet. Then Passed is false, Checks is
	// empty, Why says so, and nothing is stored.
	Evaluated bool    `json:"evaluated"`
	Passed    bool    `json:"passed"`
	Checks    []Check `json:"checks"`
	Why       string  `json:"why,omitempty"`
}

// gateInputs is what one row brings to the gate.
type gateInputs struct {
	windows, needed int
	ret             Ratio
	lower           float64 // as published
	drawdown        Drawdown
	// calibration: whether the model this version trades on is the one the scorecard scores,
	// and the scorecard's overall row
	modelScored bool
	overall     ScoreRow
}

// evaluateGate applies the rule. Every check is decided on the figures AS PUBLISHED (the caller
// passes them rounded), so a row can never show a number on one side of the bar and a verdict on
// the other.
func evaluateGate(cfg GateConfig, in gateInputs) Gate {
	floor := max(cfg.MinWindows, in.needed)
	g := Gate{Evaluated: true, Checks: []Check{}}
	add := func(name string, passed bool, why string) {
		g.Checks = append(g.Checks, Check{name, passed, why})
	}
	switch {
	case in.needed == 0:
		add("windows", false, fmt.Sprintf("%d windows; the sample floor could not be computed (no spread measured yet)", in.windows))
	case in.windows >= floor:
		add("windows", true, fmt.Sprintf("%d windows, floor %d (min_windows %d, windows_needed %d)", in.windows, floor, cfg.MinWindows, in.needed))
	default:
		add("windows", false, fmt.Sprintf("%d windows, floor %d (min_windows %d, windows_needed %d)", in.windows, floor, cfg.MinWindows, in.needed))
	}
	switch {
	case in.ret.SE == 0:
		add("edge", false, fmt.Sprintf("return %.4f per dollar with no standard error measured", in.ret.Value))
	default:
		add("edge", in.lower > 0, fmt.Sprintf("return %.4f per dollar, lower bound %.4f at z %.4f", in.ret.Value, in.lower, cfg.Z))
	}
	add("drawdown", in.drawdown.MaxCents <= cfg.MaxDrawdownCents, fmt.Sprintf("deepest fall %d cents, limit %d", in.drawdown.MaxCents, cfg.MaxDrawdownCents))
	switch {
	case !in.modelScored:
		add("calibration", false, "the model this version trades on is not scored; only the second engine's originals are")
	case in.overall.NWindows < cfg.MinWindows:
		add("calibration", false, fmt.Sprintf("the model is scored on %d windows, fewer than min_windows %d", in.overall.NWindows, cfg.MinWindows))
	default:
		add("calibration", in.overall.Verdict != "model worse", fmt.Sprintf("the scorecard's overall verdict is %q on %d windows", in.overall.Verdict, in.overall.NWindows))
	}
	g.Passed = true
	for _, c := range g.Checks {
		g.Passed = g.Passed && c.Passed
	}
	return g
}

// notEvaluated is the gate when no decision can be made.
func notEvaluated(why string) Gate { return Gate{Checks: []Check{}, Why: why} }

// Snapshot is one row for metric_snapshot: a gate decision on one version, with everything it
// was decided from.
type Snapshot struct {
	VersionID  int64
	FirstClose int64 // unix s: the close of the earliest window with a bet; the period opens 900 s before it
	LastClose  int64 // unix s: the close of the latest
	Decisions  int64 // journal rows of this version on the settled markets in those windows
	Orders     int   // orders that filled anything, buys and sales
	Trials     int
	Metrics    json.RawMessage // the leaderboard row, whole
	GateConfig json.RawMessage
	GatePassed bool
}

// Snapshots is what a document has to store: one Snapshot per leaderboard row whose gate was
// evaluated. Rows without a settled window with a bet, and every row of a partial document, have
// no decision to store. The caller decides which of these are new (by LastClose) and writes them.
// The stored configuration is the numbers without the note: the note is prose that lives in this
// package's history, and a kilobyte of it on every row would be most of the table.
func Snapshots(doc Document) []Snapshot {
	var out []Snapshot
	stored := doc.Gate
	stored.Note = ""
	cfg, err := json.Marshal(stored)
	if err != nil {
		return nil
	}
	for _, r := range doc.Leaderboard.Rows {
		if !r.Gate.Evaluated {
			continue
		}
		row, err := json.Marshal(r)
		if err != nil {
			continue
		}
		out = append(out, Snapshot{VersionID: r.VersionID, FirstClose: r.FirstClose, LastClose: r.LastClose, Decisions: r.Decisions, Orders: r.Orders,
			Trials: doc.Conventions.Trials, Metrics: row, GateConfig: cfg, GatePassed: r.Gate.Passed})
	}
	return out
}
