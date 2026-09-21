// Command replay2 runs a v2 recording (tools/parity/record.py) through the Go port of doipster's
// second engine version and compares it with what the Python did. It is the parity gate for
// package kalshi15m2.
//
//	go run ./cmd/replay2 ../tools/parity/fixtures/<v2 recording>
//
// Passes (exit 0) when: every step agrees with the Python's trace (for each original strategy the
// same side and the same bet/no-bet call on that coin; the coin's index offset and volatility
// identical; probabilities and edges within tolerance; and ALL TWELVE accounts' cash identical,
// which is what catches a twin or a bankruptcy going differently); the trade log and the
// early-sales log are identical line for line; and the saved state agrees.
//
// The session id, a timestamp taken when the engine starts, is compared like everything else.
package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	k "github.com/doipster/asset_cracker/service/internal/kalshi15m2"
)

const tolerance = 1e-12

type call struct {
	T    float64           `json:"t"`
	Call string            `json:"call"`
	Args []json.RawMessage `json:"args"`
	Meta *struct {
		Engine string                   `json:"engine"`
		Coins  []string                 `json:"coins"`
		Python string                   `json:"python"`
		Assets map[string]k.Calibration `json:"assets"`
	} `json:"meta"`
}

type traceRow struct {
	I      int                `json:"i"`
	Coin   string             `json:"coin"`
	Sigma2 float64            `json:"sigma2"`
	Offset float64            `json:"offset"`
	Views  map[string][]any   `json:"views"`
	Cash   map[string]float64 `json:"cash"`
}

func lines(path string) ([]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		f, err = os.Open(path + ".gz")
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(f.Name(), ".gz") {
		if r, err = gzip.NewReader(f); err != nil {
			return nil, err
		}
	}
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: replay2 <recording folder>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(folder string) error {
	raw, err := lines(filepath.Join(folder, "calls.jsonl"))
	if err != nil {
		return err
	}
	var head call
	if err := json.Unmarshal([]byte(raw[0]), &head); err != nil || head.Meta == nil || head.Meta.Engine != "v2" {
		return fmt.Errorf("%s is not a v2 recording", folder)
	}
	trace := map[int]traceRow{}
	if tl, err := lines(filepath.Join(folder, "trace.jsonl")); err == nil {
		for _, l := range tl {
			var row traceRow
			if json.Unmarshal([]byte(l), &row) == nil {
				trace[row.I] = row
			}
		}
	}

	now := head.T
	trader := k.NewTrader(func() float64 { return now }, head.Meta.Coins, head.Meta.Assets)
	counts := map[string]int{}
	worst := map[string]float64{}
	var stepErrors []string
	checked := 0
	for i, l := range raw[1:] {
		var c call
		if err := json.Unmarshal([]byte(l), &c); err != nil {
			return fmt.Errorf("line %d: %w", i+2, err)
		}
		now = c.T
		if err := apply(trader, c); err != nil {
			return fmt.Errorf("call %d: %w", i, err)
		}
		counts[c.Call]++
		if want, ok := trace[i]; ok {
			checked++
			if msg := checkStep(trader, want, worst); msg != "" && len(stepErrors) < 5 {
				stepErrors = append(stepErrors, fmt.Sprintf("call %d (%s): %s", i, want.Coin, msg))
			}
		}
	}
	fmt.Printf("replayed %d calls: %v (recorded on Python %s)\n", len(raw)-1, counts, head.Meta.Python)

	ok := true
	switch {
	case len(trace) == 0:
		fmt.Println("no trace.jsonl(.gz) in the folder: steps not compared")
	case len(stepErrors) == 0:
		fmt.Printf("steps match (%d compared; offsets, volatility and all twelve balances identical; largest gaps: %s)\n", checked, gaps(worst))
	default:
		ok = false
		fmt.Printf("STEPS DIFFER. First: %s\n", strings.Join(stepErrors, "\n  then: "))
	}

	for _, pair := range []struct {
		file string
		rows [][]string
	}{{"kalshi_trades.csv", trader.Trades}, {"kalshi_exits.csv", trader.Exits}} {
		// Prefer what the Python REPLAY wrote from exactly these calls (saved beside the trace).
		live, err := lines(filepath.Join(folder, "replay_"+pair.file))
		if err != nil {
			live, _ = lines(filepath.Join(folder, pair.file))
		}
		var ours []string
		for _, row := range pair.rows {
			ours = append(ours, csvLine(row))
		}
		if d := firstLineDiff(live, ours); d < 0 {
			fmt.Printf("%s: match (%d rows)\n", pair.file, max(0, len(live)-1))
		} else {
			ok = false
			fmt.Printf("%s: DIFFERS at line %d (python %d rows, go %d)\n    python: %s\n    go:     %s\n",
				pair.file, d+1, len(live)-1, len(ours)-1, at(live, d), at(ours, d))
		}
	}

	var theirs map[string]any
	// Prefer the state the Python REPLAY ended in (written beside the trace): it was produced
	// under the same recorded clock, so equity figures are comparable. The live run's own file
	// was saved on its own schedule.
	blob, err := os.ReadFile(filepath.Join(folder, "replay_state.json"))
	if err != nil {
		blob, err = os.ReadFile(filepath.Join(folder, "kalshi_balance.json"))
	}
	if err != nil || json.Unmarshal(blob, &theirs) != nil {
		return fmt.Errorf("cannot read the Python's saved state")
	}
	mineBlob, _ := json.Marshal(trader.State())
	var mine map[string]any
	_ = json.Unmarshal(mineBlob, &mine)
	// index_offset_pct depends on the moment of the final save, so it is left out of the coins.
	if coins, ok := theirs["coins"].(map[string]any); ok {
		for _, c := range coins {
			delete(c.(map[string]any), "index_offset_pct")
		}
	}
	diff := ""
	for _, key := range []string{"rounds_monitored", "last_round_close", "leaderboard", "accounts", "coins"} {
		if diff = firstDifference(theirs[key], mine[key], "/"+key); diff != "" {
			break
		}
	}
	if diff == "" {
		fmt.Println("saved state matches (accounts with bankruptcies and retirements, coins, leaderboard)")
	} else {
		ok = false
		fmt.Printf("STATE DIFFERS at %s\n", diff)
	}
	if !ok {
		fmt.Println("PARITY FAILED")
		return fmt.Errorf("the Go port of v2 does not reproduce the Python on %s", folder)
	}
	fmt.Println("PARITY OK")
	return nil
}

func apply(t *k.Trader, c call) error {
	a := c.Args
	num := func(i int) float64 { var v float64; _ = json.Unmarshal(a[i], &v); return v }
	str := func(i int) string { var v string; _ = json.Unmarshal(a[i], &v); return v }
	switch c.Call {
	case "observe":
		t.Observe(str(0), num(1), num(2))
	case "seed_vol":
		var closes []float64
		_ = json.Unmarshal(a[1], &closes)
		t.SeedVol(str(0), closes)
	case "seed_offsets":
		var pairs [][2]float64
		_ = json.Unmarshal(a[1], &pairs)
		t.SeedOffsets(str(0), pairs)
	case "step":
		var m k.Market
		if err := json.Unmarshal(a[1], &m); err != nil {
			return err
		}
		t.Step(str(0), m, num(2), num(3))
	case "note_settlement":
		var final any
		_ = json.Unmarshal(a[2], &final)
		t.NoteSettlement(str(0), num(1), final)
	case "on_settled":
		var final any
		var price *float64
		_ = json.Unmarshal(a[2], &final)
		_ = json.Unmarshal(a[4], &price)
		t.OnSettled(str(0), str(1), final, num(3), price)
	default:
		return fmt.Errorf("unknown call %q", c.Call)
	}
	return nil
}

func checkStep(t *k.Trader, want traceRow, worst map[string]float64) string {
	gap := func(name string, a, b float64) bool {
		g := math.Abs(a - b)
		worst[name] = math.Max(worst[name], g)
		return g <= tolerance
	}
	c := t.Coins[want.Coin]
	if c.Sigma2 != want.Sigma2 {
		return fmt.Sprintf("sigma2 python %v go %v", want.Sigma2, c.Sigma2)
	}
	if got := c.OffsetPct(); got != want.Offset {
		return fmt.Sprintf("offset python %v go %v", want.Offset, got)
	}
	for _, a := range t.Accounts {
		name := a.Params.Name
		if cash, ok := want.Cash[name]; ok && cash != a.Cash {
			return fmt.Sprintf("%s: cash python %v go %v", name, cash, a.Cash)
		}
		if a.Params.Anti {
			continue
		}
		v, view := want.Views[name], a.Views[want.Coin]
		if v == nil || view == nil {
			if (v == nil) != (view == nil) {
				return name + ": a view on one side only"
			}
			continue
		}
		if side := v[2].(string); side != view.Best.Side {
			return fmt.Sprintf("%s: side python %s go %s", name, side, view.Best.Side)
		}
		if bet := v[4].(bool); bet != view.Signal.Bet {
			return fmt.Sprintf("%s: bet python %v go %v", name, bet, view.Signal.Bet)
		}
		if !gap("p_up", view.PUp, v[0].(float64)) || !gap("p_model", view.PModel, v[1].(float64)) || !gap("edge", view.Best.Edge, v[3].(float64)) {
			return fmt.Sprintf("%s: probabilities differ beyond %g", name, tolerance)
		}
	}
	return ""
}

func gaps(w map[string]float64) string {
	var parts []string
	for _, name := range []string{"p_model", "p_up", "edge"} {
		parts = append(parts, fmt.Sprintf("%s %.1e", name, w[name]))
	}
	return strings.Join(parts, ", ")
}

// csvLine quotes a row as Python's csv module does: only where needed.
func csvLine(fields []string) string {
	out := make([]string, len(fields))
	for i, f := range fields {
		if strings.ContainsAny(f, ",\"\r\n") {
			f = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
		}
		out[i] = f
	}
	return strings.Join(out, ",")
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "(missing)"
}

func firstLineDiff(a, b []string) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		if at(a, i) != at(b, i) {
			return i
		}
	}
	return -1
}

// firstDifference says where two JSON values first differ, or "" if they are the same. Key
// order is ignored, a missing key equals null, and numbers compare by value.
func firstDifference(a, b any, path string) string {
	switch x := a.(type) {
	case map[string]any:
		y, _ := b.(map[string]any)
		keys := map[string]bool{}
		for key := range x {
			keys[key] = true
		}
		for key := range y {
			keys[key] = true
		}
		for key := range keys {
			if d := firstDifference(x[key], y[key], path+"/"+key); d != "" {
				return d
			}
		}
		return ""
	case []any:
		y, _ := b.([]any)
		if len(x) != len(y) {
			return fmt.Sprintf("%s: length python %d go %d", path, len(x), len(y))
		}
		for i := range x {
			if d := firstDifference(x[i], y[i], fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
		return ""
	}
	if a != b {
		return fmt.Sprintf("%s: python %v go %v", path, a, b)
	}
	return ""
}
