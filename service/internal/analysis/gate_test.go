package analysis

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// Three windows: 100 of 1000, -50 of 500, 300 of 1500. The return is the ratio of the sums,
// 350/3000 = 0.1167, NOT the mean of the three windows' own returns (0.1, -0.1, 0.2: 0.0667).
// The same windows give the same standard error every time.
func TestWindowRatioIsTheRatioOfSums(t *testing.T) {
	num, den := []float64{100, -50, 300}, []float64{1000, 500, 1500}
	r := WindowRatio(num, den)
	if r.N != 3 || !near(r.Value, 350.0/3000) || r.SE <= 0 {
		t.Fatalf("got %+v", r)
	}
	if again := WindowRatio(num, den); again != r {
		t.Fatalf("the bootstrap is seeded: %+v then %+v", r, again)
	}
}

// With every window staking the same, the ratio is the mean of the P&L, and the bootstrap's
// standard error of a mean is sd_n/sqrt(n) with the n-divisor sd: for forty windows of 100 and
// 300 alternately, 100/sqrt(40) = 15.81, a little under the plain SE of 16.01. A thousand
// resamples put the estimate within a few percent of that.
func TestWindowRatioSEOfAMean(t *testing.T) {
	var num, den []float64
	for i := range 40 {
		num, den = append(num, float64(100+200*(i%2))), append(den, 1)
	}
	r := WindowRatio(num, den)
	if r.Value != 200 || math.Abs(r.SE-15.81) > 1 {
		t.Fatalf("got %+v, want SE near 15.81", r)
	}
}

func TestWindowRatioEdges(t *testing.T) {
	if r := WindowRatio(nil, nil); r != (Ratio{}) {
		t.Errorf("nothing: %+v", r)
	}
	if r := WindowRatio([]float64{50}, []float64{1000}); r != (Ratio{N: 1, Value: 0.05}) {
		t.Errorf("one window has no spread: %+v", r)
	}
	if r := WindowRatio([]float64{50, 50, 50}, []float64{1000, 1000, 1000}); r != (Ratio{N: 3, Value: 0.05}) {
		t.Errorf("identical windows have no spread: %+v", r)
	}
	if r := WindowRatio([]float64{50, 60}, []float64{0, 0}); r != (Ratio{N: 2}) {
		t.Errorf("nothing staked: %+v", r)
	}
	if r := WindowRatio([]float64{math.NaN(), 50, math.Inf(1)}, []float64{1000, 1000, 1000}); math.IsNaN(r.Value) || math.IsInf(r.Value, 0) || math.IsNaN(r.SE) {
		t.Errorf("never NaN: %+v", r)
	}
	if r := WindowRatio([]float64{1, 2, 3}, []float64{10, 10}); r.N != 2 {
		t.Errorf("mismatched lengths use the shorter: %+v", r)
	}
}

// 100, -300, 50, 400, -100: the running total goes 100, -200, -150, 250, 150 and its peak 100,
// 100, 100, 250, 250. The deepest fall is 300 (at -200 from 100); at the end it is 100 below the
// peak; the longest run under water is the two windows after the first peak; the worst window
// is -300.
func TestDrawdownOf(t *testing.T) {
	if d := DrawdownOf([]float64{100, -300, 50, 400, -100}); d != (Drawdown{MaxCents: 300, NowCents: 100, LongestUnderwater: 2, WorstWindowCents: -300}) {
		t.Errorf("got %+v", d)
	}
	if d := DrawdownOf(nil); d != (Drawdown{}) {
		t.Errorf("nothing: %+v", d)
	}
	// only losses: the peak is the start, and the drawdown is the whole loss
	if d := DrawdownOf([]float64{-10, -20}); d != (Drawdown{MaxCents: 30, NowCents: 30, LongestUnderwater: 2, WorstWindowCents: -20}) {
		t.Errorf("losses: %+v", d)
	}
	// only gains: never under water
	if d := DrawdownOf([]float64{10, 20}); d != (Drawdown{}) {
		t.Errorf("gains: %+v", d)
	}
	// a flat window at the peak is not under water; the order matters
	if d := DrawdownOf([]float64{10, 0, -5, 5}); d != (Drawdown{MaxCents: 5, NowCents: 0, LongestUnderwater: 1, WorstWindowCents: -5}) {
		t.Errorf("flat: %+v", d)
	}
	if d := DrawdownOf([]float64{-5, 5, 10, 0}); d != (Drawdown{MaxCents: 5, NowCents: 0, LongestUnderwater: 1, WorstWindowCents: -5}) {
		t.Errorf("reordered: %+v", d)
	}
}

// The brief's illustration: even-money bets (sd 1 per dollar), an edge of 0.04 per dollar (a 52%
// win rate), 95% confidence (z 1.96) and no power beyond the threshold itself (0.5: z_power 0)
// need (1.96/0.04)^2 = 2401. Asking for 80% power raises it to ((1.96 + 0.8416)/0.04)^2 = 4906.
func TestWindowsNeeded(t *testing.T) {
	if n := WindowsNeeded(1, 0.04, 1.96, 0.5); n != 2401 {
		t.Errorf("the illustration: %d", n)
	}
	if n := WindowsNeeded(1, 0.04, 1.96, 0.8); n != 4906 {
		t.Errorf("80%% power: %d", n)
	}
	// a smaller edge needs quadratically more; a smaller spread quadratically fewer
	if a, b := WindowsNeeded(1, 0.02, 1.96, 0.5), WindowsNeeded(0.5, 0.04, 1.96, 0.5); a != 9604 || b != 601 {
		t.Errorf("scaling: %d %d", a, b)
	}
	for name, n := range map[string]int{
		"no spread":  WindowsNeeded(0, 0.02, 2, 0.8),
		"no edge":    WindowsNeeded(1, 0, 2, 0.8),
		"no z":       WindowsNeeded(1, 0.02, 0, 0.8),
		"power 1":    WindowsNeeded(1, 0.02, 2, 1),
		"power 0":    WindowsNeeded(1, 0.02, 2, 0),
		"NaN spread": WindowsNeeded(math.NaN(), 0.02, 2, 0.8),
	} {
		if n != 0 {
			t.Errorf("%s: %d, want 0 (no floor could be computed)", name, n)
		}
	}
	if n := WindowsNeeded(1e9, 1e-9, 2, 0.8); n != math.MaxInt32 {
		t.Errorf("out of reach: %d", n)
	}
}

// A fixture for the gate: forty windows making 100 and 300 cents alternately on a stake of 1000
// each, so the return is 8000/40000 = 0.2 per dollar with a bootstrap SE near 0.0158; the running
// total never falls; and the model is scored equal to the market (diff 0, no spread), which is
// "unresolved" and so not "model worse".
func gated(bucket int64) []MarketFacts {
	var facts []MarketFacts
	for i := range 40 {
		f := round1(int64(i+1), int64(900*(i+1)), bucket, int64(100+200*(i%2)), 1)
		f.Score[2] = BandSum{N: 10, Model: 2.0, Market: 2.0, ModelLog: 5, MarketLog: 5}
		f.Decisions = map[int64]int64{1: 60, 2: 7}
		facts = append(facts, f)
	}
	return facts
}

// With the edge worth finding set at 0.2 per dollar the floor is ((2.9913 + 0.8416) x 0.1 / 0.2)^2
// = 4 windows, under min_windows, so forty windows clear it; the lower bound 0.2 - 2.9913 x 0.0158
// = 0.153 is above zero; nothing fell; the model is not worse. Every check passes.
func TestGatePasses(t *testing.T) {
	buckets := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v3", World: "real"}}
	doc := Build(Inputs{Facts: gated(1), Buckets: buckets, MarketsSettled: 40, Trials: 18, Gate: GateSettings{MinEdgePerDollar: 0.2, Power: 0.8, MaxDrawdownCents: 25_000}})
	r := doc.Leaderboard.Rows[0]
	if r.VersionID != 1 || r.StakedCents != 40000 || r.ReturnPerDollar != 0.2 || math.Abs(r.ReturnSE-0.0158) > 0.001 || r.ReturnT < 10 ||
		r.ReturnLower <= 0.14 || r.ReturnLower >= 0.16 || r.WindowsNeeded != 4 || r.Orders != 40 || r.FirstClose != 900 || r.LastClose != 36000 ||
		r.Drawdown != (Drawdown{}) || r.Decisions != 40*60 {
		t.Fatalf("row: %+v", r)
	}
	if !r.Gate.Evaluated || !r.Gate.Passed || len(r.Gate.Checks) != 4 || r.Gate.Why != "" {
		t.Fatalf("gate: %+v", r.Gate)
	}
	for i, name := range []string{"windows", "edge", "drawdown", "calibration"} {
		if c := r.Gate.Checks[i]; c.Name != name || !c.Passed || c.Why == "" {
			t.Errorf("check %d: %+v", i, c)
		}
	}
	if !strings.Contains(r.Gate.Checks[0].Why, "floor 30") || !strings.Contains(r.Gate.Checks[0].Why, "windows_needed 4") {
		t.Errorf("the windows check says what the floor was: %q", r.Gate.Checks[0].Why)
	}
	g := doc.Gate
	if g.MinWindows != 30 || g.Trials != 18 || g.Z != 2.9913 || g.MinEdgePerDollar != 0.2 || g.Power != 0.8 || g.MaxDrawdownCents != 25_000 ||
		g.BootstrapResamples != 1000 || g.BootstrapSeed != 20260921 || g.Note == "" {
		t.Errorf("config: %+v", g)
	}
}

// Each check fails on its own, and one failure fails the gate.
func TestGateFailsOneCheckAtATime(t *testing.T) {
	v2 := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v3", World: "real"}}
	loose := GateSettings{MinEdgePerDollar: 0.2, Power: 0.8, MaxDrawdownCents: 25_000}
	failing := func(name string, in Inputs) (LeaderRow, Check) {
		t.Helper()
		r := Build(in).Leaderboard.Rows[0]
		if !r.Gate.Evaluated || r.Gate.Passed {
			t.Fatalf("%s: gate %+v", name, r.Gate)
		}
		var failed []Check
		for _, c := range r.Gate.Checks {
			if !c.Passed {
				failed = append(failed, c)
			}
		}
		if len(failed) != 1 || failed[0].Name != name {
			t.Fatalf("%s: failed %+v", name, failed)
		}
		return r, failed[0]
	}

	// windows: at the default edge of 0.02 the floor is ((2.9913 + 0.8416) x 0.1 / 0.02)^2, about
	// 367 windows, and the check names the floor it applied
	r, c := failing("windows", Inputs{Facts: gated(1), Buckets: v2, MarketsSettled: 40, Trials: 18})
	if r.WindowsNeeded < 350 || r.WindowsNeeded > 385 || c.Why != fmt.Sprintf("40 windows, floor %d (min_windows 30, windows_needed %d)", r.WindowsNeeded, r.WindowsNeeded) {
		t.Errorf("windows: %d %q", r.WindowsNeeded, c.Why)
	}

	// edge: the same spread around a loss
	losing := gated(1)
	for i := range losing {
		r := losing[i].Rounds[1]
		r.PnLCents -= 400 // -300 and -100 alternately: return -0.2
		losing[i].Rounds[1] = r
	}
	_, c = failing("edge", Inputs{Facts: losing, Buckets: v2, MarketsSettled: 40, Trials: 18, Gate: loose})
	if !strings.Contains(c.Why, "return -0.2000 per dollar, lower bound -0.2") {
		t.Errorf("edge: %q", c.Why)
	}

	// drawdown: one window losing 150 against a limit of 100; the return and its spread barely move
	fell := gated(1)
	fell[20].Rounds[1] = BucketRound{PnLCents: -150, Bets: 1, StakedCents: 1000, Orders: 1}
	_, c = failing("drawdown", Inputs{Facts: fell, Buckets: v2, MarketsSettled: 40, Trials: 18, Gate: GateSettings{MinEdgePerDollar: 0.2, Power: 0.8, MaxDrawdownCents: 100}})
	if !strings.Contains(c.Why, "deepest fall 150 cents, limit 100") {
		t.Errorf("drawdown: %q", c.Why)
	}

	// calibration: the model read as worse (steady's scorecard), on the same money
	worse := gated(1)
	for i := range worse {
		worse[i].Score[2] = BandSum{N: 10, Model: 2.0 + 0.1 + 0.2*float64(i%2), Market: 2.0}
	}
	_, c = failing("calibration", Inputs{Facts: worse, Buckets: v2, MarketsSettled: 40, Trials: 18, Gate: loose})
	if !strings.Contains(c.Why, `"model worse"`) {
		t.Errorf("calibration: %q", c.Why)
	}
	// calibration: a version whose model is not the one scored (the first engine, a twin, the archived second)
	for _, b := range []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v1", World: "real"}, {ID: 1, VersionID: 1, Strategy: "Value", Engine: "v2", World: "anti"},
		{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v2", World: "real"}} {
		_, c = failing("calibration", Inputs{Facts: gated(1), Buckets: []Bucket{b}, MarketsSettled: 40, Trials: 18, Gate: loose})
		if !strings.Contains(c.Why, "not scored") {
			t.Errorf("%s %s: %q", b.Engine, b.World, c.Why)
		}
	}
	// calibration: scored on too few windows
	few := gated(1)
	for i := range few {
		if i >= 20 {
			few[i].Score = [5]BandSum{}
		}
	}
	_, c = failing("calibration", Inputs{Facts: few, Buckets: v2, MarketsSettled: 40, Trials: 18, Gate: loose})
	if !strings.Contains(c.Why, "scored on 20 windows, fewer than min_windows 30") {
		t.Errorf("few: %q", c.Why)
	}
}

// No decision is made where no verdict could be: a partial document, an unknown trials count, or
// a version with nothing settled. Such a row is not evaluated, says why, and is not stored.
func TestGateIsNotEvaluatedWithoutTheEvidence(t *testing.T) {
	v2 := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v3", World: "real"}, {ID: 2, VersionID: 2, Strategy: "Late", Engine: "v3", World: "real"}}
	loose := GateSettings{MinEdgePerDollar: 0.2, Power: 0.8, MaxDrawdownCents: 25_000}
	for name, c := range map[string]struct {
		in  Inputs
		why string
	}{
		"partial":   {Inputs{Facts: gated(1), Buckets: v2, MarketsSettled: 3360, Trials: 18, Gate: loose}, "partial: only 40 of 3360 settled markets read; no gate decision until all are read"},
		"no trials": {Inputs{Facts: gated(1), Buckets: v2, MarketsSettled: 40, Trials: 0, Gate: loose}, "the number of strategy versions could not be read, so there is no threshold to decide against"},
	} {
		doc := Build(c.in)
		for _, r := range doc.Leaderboard.Rows {
			if r.Gate.Evaluated || r.Gate.Passed || r.Gate.Why != c.why || r.Gate.Checks == nil || len(r.Gate.Checks) != 0 {
				t.Errorf("%s: %s: %+v", name, r.Strategy, r.Gate)
			}
			// The figures stay, as t does: a partial document shows the lower bound for the windows
			// read. With no trials count there is no z, and so no bound at all.
			if hasBound := r.ReturnLower != 0; r.Strategy == "Value" && hasBound != (c.in.Trials > 0) {
				t.Errorf("%s: lower bound %v", name, r.ReturnLower)
			}
		}
		if snaps := Snapshots(doc); len(snaps) != 0 {
			t.Errorf("%s: nothing to store: %+v", name, snaps)
		}
	}
	doc := Build(Inputs{Facts: gated(1), Buckets: v2, MarketsSettled: 40, Trials: 18, Gate: loose})
	if late := doc.Leaderboard.Rows[1]; late.Strategy != "Late" || late.Gate.Evaluated || late.Gate.Why != "no settled window with a bet" || late.Windows != 0 {
		t.Errorf("never bet: %+v", late.Gate)
	}
	if value := doc.Leaderboard.Rows[0]; !value.Gate.Evaluated {
		t.Errorf("the other row is decided: %+v", value.Gate)
	}
}

// Settings that cannot be applied are not a looser gate: the defaults stand, and the document
// says which numbers were used.
func TestInvalidGateSettingsFallBackToTheDefaults(t *testing.T) {
	for name, s := range map[string]GateSettings{"zero": {}, "negative edge": {-0.02, 0.8, 25_000}, "power 1": {0.02, 1, 25_000}, "power half": {0.02, 0.5, 25_000},
		"no drawdown": {0.02, 0.8, 0}, "NaN": {math.NaN(), 0.8, 25_000}, "Inf": {math.Inf(1), 0.8, 25_000}} {
		if s.Valid() {
			t.Errorf("%s: valid", name)
		}
		g := Build(Inputs{Gate: s}).Gate
		if g.MinEdgePerDollar != 0.02 || g.Power != 0.8 || g.MaxDrawdownCents != 25_000 {
			t.Errorf("%s: %+v", name, g)
		}
	}
	if !DefaultGate.Valid() || !(GateSettings{0.05, 0.9, 10_000}).Valid() {
		t.Error("good settings are valid")
	}
}

// One snapshot per row decided: the version, the period's bounds, the counts, the row as JSON,
// and the configuration without its note.
func TestSnapshots(t *testing.T) {
	v2 := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v3", World: "real"}, {ID: 2, VersionID: 2, Strategy: "Late", Engine: "v3", World: "real"}}
	doc := Build(Inputs{Facts: gated(1), Buckets: v2, MarketsSettled: 40, Trials: 18, Gate: GateSettings{MinEdgePerDollar: 0.2, Power: 0.8, MaxDrawdownCents: 25_000}})
	snaps := Snapshots(doc)
	if len(snaps) != 1 {
		t.Fatalf("got %+v", snaps)
	}
	s := snaps[0]
	if s.VersionID != 1 || s.FirstClose != 900 || s.LastClose != 36000 || s.Decisions != 2400 || s.Orders != 40 || s.Trials != 18 || !s.GatePassed {
		t.Errorf("got %+v", s)
	}
	var row map[string]any
	if err := json.Unmarshal(s.Metrics, &row); err != nil || row["strategy_version_id"] != float64(1) || row["return_per_dollar"] != 0.2 || row["gate"] == nil {
		t.Errorf("metrics: %v %s", err, s.Metrics)
	}
	var cfg map[string]any
	if err := json.Unmarshal(s.GateConfig, &cfg); err != nil || cfg["z"] != 2.9913 || cfg["min_edge_per_dollar"] != 0.2 || cfg["trials"] != float64(18) {
		t.Errorf("config: %v %s", err, s.GateConfig)
	}
	if _, has := cfg["note"]; has {
		t.Errorf("the stored configuration is the numbers, not the prose: %s", s.GateConfig)
	}
	if len(doc.Gate.Note) < 200 {
		t.Errorf("the API carries the note: %q", doc.Gate.Note)
	}
}

// The journal rows counted are the version's own, on the settled markets inside its period. Value
// (version 1) bet in windows 900 to 36000 and journaled 60 rows on each market: 2400. Version 2
// journaled 7 a market but never bet: no period, so 0, and nothing is stored for it.
func TestDecisionsAreCountedInsideThePeriod(t *testing.T) {
	facts := gated(1)
	facts = append(facts, round1(41, 36900, 2, 50, 1)) // Late bets once, in a later window, where Value journaled 60 more
	facts[40].Decisions = map[int64]int64{1: 60, 2: 7}
	v2 := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v3", World: "real"}, {ID: 2, VersionID: 2, Strategy: "Late", Engine: "v3", World: "real"}}
	rows := Build(Inputs{Facts: facts, Buckets: v2, MarketsSettled: 41, Trials: 18}).Leaderboard.Rows
	byName := map[string]LeaderRow{}
	for _, r := range rows {
		byName[r.Strategy] = r
	}
	if v := byName["Value"]; v.Decisions != 2400 || v.LastClose != 36000 {
		t.Errorf("Value: %d rows to %d", v.Decisions, v.LastClose)
	}
	if l := byName["Late"]; l.Decisions != 7 || l.FirstClose != 36900 || l.LastClose != 36900 || l.Windows != 1 {
		t.Errorf("Late: %+v", l)
	}
}

// Log loss is made exactly as Brier is: per window from the summed rows, then a mean over windows.
// Window 900: 2 rows, model 1.0 and market 0.5 summed: 0.5 and 0.25. Window 1800: 4 rows, 2.0 and
// 2.0: 0.5 and 0.5. Means 0.5 and 0.375, diff {0.25, 0} mean 0.125, SE 0.1768/sqrt(2) = 0.125.
func TestScorecardLogLoss(t *testing.T) {
	a, b := score(1, "BTC", 900, 0, 2, 0.5, 0.3), score(2, "BTC", 1800, 0, 4, 1.0, 0.6)
	a.Score[0].ModelLog, a.Score[0].MarketLog = 1.0, 0.5
	b.Score[0].ModelLog, b.Score[0].MarketLog = 2.0, 2.0
	o := Build(Inputs{Facts: []MarketFacts{a, b}}).Scorecard.Overall
	if o.LogLossModel != 0.5 || o.LogLossMarket != 0.375 || o.LogLossDiff != 0.125 || o.LogLossSE != 0.125 {
		t.Errorf("got %+v", o)
	}
	// the Brier figures are untouched by it (0.25 and 0.15 in both windows), and the verdict is theirs
	if o.BrierModel != 0.25 || o.BrierMarket != 0.15 || o.Diff != 0.1 || o.Verdict != "unresolved" {
		t.Errorf("brier: %+v", o)
	}
	// a NaN log sum is a 0, never a NaN on the page
	a.Score[0].ModelLog = math.NaN()
	if o := Build(Inputs{Facts: []MarketFacts{a, b}}).Scorecard.Overall; math.IsNaN(o.LogLossModel) || o.LogLossModel != 0.25 {
		t.Errorf("NaN: %+v", o)
	}
}
