// Command replay runs a recording made by tools/parity/record.py through the Go port of the
// six strategies and compares it with what the Python did. It is the parity gate for the port.
//
//	go run ./cmd/replay ../tools/parity/fixtures/2026-09-20_40min
//
// Passes (exit 0) when, for every coin: each step agrees with the Python's trace (same side and
// the same bet/no-bet call for every strategy; index offset identical; volatility,
// probabilities and edges within tolerance); the trade log is identical line for line; and the
// saved state agrees.
//
// Why a tolerance at all: CPython takes erf, log and pow from the platform C library, and Go
// has its own. Measured in internal/pyfloat's tests, they differ by at most a few units in the
// last place, which is about 1e-16 here. Anything that is pure arithmetic is compared exactly.
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

	k "github.com/doipster/asset_cracker/service/internal/kalshi15m"
)

const tolerance = 1e-12

type call struct {
	T    float64         `json:"t"`
	Coin string          `json:"coin"`
	Call string          `json:"call"`
	Args json.RawMessage `json:"args"`
	Meta *struct {
		Coins  []string `json:"coins"`
		Python string   `json:"python"`
		Assets map[string]struct {
			Suffix       string  `json:"suffix"`
			OffsetPct    float64 `json:"index_offset_pct"`
			SDPct        float64 `json:"index_sd_pct"`
			DefaultSigma float64 `json:"default_sigma"`
		} `json:"assets"`
	} `json:"meta"`
}

type traceRow struct {
	I      int              `json:"i"`
	Coin   string           `json:"coin"`
	Sigma2 float64          `json:"sigma2"`
	Offset float64          `json:"offset"`
	Views  map[string][]any `json:"views"`
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
		fmt.Fprintln(os.Stderr, "usage: replay <recording folder>")
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
	if err := json.Unmarshal([]byte(raw[0]), &head); err != nil || head.Meta == nil {
		return fmt.Errorf("first line is not a meta row")
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
	traders := map[string]*k.Trader{}
	for _, coin := range head.Meta.Coins {
		a := head.Meta.Assets[coin]
		traders[coin] = k.NewTrader(func() float64 { return now }, a.OffsetPct, a.SDPct, a.DefaultSigma)
	}

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
		if err := apply(traders[c.Coin], c); err != nil {
			return fmt.Errorf("call %d: %w", i, err)
		}
		counts[c.Call]++
		if want, ok := trace[i]; ok {
			checked++
			if msg := checkStep(traders[c.Coin], want, worst); msg != "" {
				stepErrors = append(stepErrors, fmt.Sprintf("call %d (%s): %s", i, c.Coin, msg))
			}
		}
	}
	fmt.Printf("replayed %d calls: %v (recorded on Python %s)\n", len(raw)-1, counts, head.Meta.Python)

	ok := true
	switch {
	case len(trace) == 0:
		fmt.Println("no trace.jsonl(.gz) in the folder: steps not compared")
	case len(stepErrors) == 0:
		fmt.Printf("steps match (%d compared; index offset identical; largest gaps: %s)\n", checked, gaps(worst))
	default:
		ok = false
		fmt.Printf("STEPS DIFFER in %d of %d. First: %s\n", len(stepErrors), checked, stepErrors[0])
	}

	for _, coin := range head.Meta.Coins {
		suffix := head.Meta.Assets[coin].Suffix
		live, _ := lines(filepath.Join(folder, "kalshi_trades"+suffix+".csv"))
		var ours []string
		for _, row := range traders[coin].Trades {
			ours = append(ours, csvLine(row))
		}
		if d := firstLineDiff(live, ours); d < 0 {
			fmt.Printf("%s: trades match (%d rows)\n", coin, max(0, len(live)-1))
		} else {
			ok = false
			fmt.Printf("%s: TRADES DIFFER at line %d (python %d rows, go %d)\n    python: %s\n    go:     %s\n",
				coin, d+1, len(live)-1, len(ours)-1, at(live, d), at(ours, d))
		}

		var theirs map[string]any
		blob, err := os.ReadFile(filepath.Join(folder, "kalshi_balance"+suffix+".json"))
		if err != nil || json.Unmarshal(blob, &theirs) != nil {
			return fmt.Errorf("%s: cannot read the Python's saved state", coin)
		}
		mineBlob, _ := json.Marshal(traders[coin].State()) // through text, so both sides are plain JSON
		var mine map[string]any
		_ = json.Unmarshal(mineBlob, &mine)
		diff := ""
		for _, key := range []string{"rounds_monitored", "last_round_ticker", "index_offsets", "leaderboard", "accounts"} {
			if diff = firstDifference(theirs[key], mine[key], "/"+key); diff != "" {
				break
			}
		}
		if diff == "" {
			fmt.Printf("%s: saved state matches\n", coin)
		} else {
			ok = false
			fmt.Printf("%s: STATE DIFFERS at %s\n", coin, diff)
		}
	}
	if !ok {
		fmt.Println("PARITY FAILED")
		return fmt.Errorf("the Go port does not reproduce the Python on %s", folder)
	}
	fmt.Println("PARITY OK")
	return nil
}

func apply(t *k.Trader, c call) error {
	var args []json.RawMessage
	if err := json.Unmarshal(c.Args, &args); err != nil {
		return err
	}
	num := func(i int) float64 { var v float64; _ = json.Unmarshal(args[i], &v); return v }
	switch c.Call {
	case "observe":
		t.Observe(num(0), num(1))
	case "seed_vol":
		var closes []float64
		_ = json.Unmarshal(args[0], &closes)
		t.SeedVol(closes)
	case "seed_offsets":
		var pairs [][2]float64
		_ = json.Unmarshal(args[0], &pairs)
		t.SeedOffsets(pairs)
	case "step":
		var m k.Market
		if err := json.Unmarshal(args[0], &m); err != nil {
			return err
		}
		t.Step(m, num(1), num(2))
	case "note_settlement":
		var final any
		_ = json.Unmarshal(args[1], &final)
		t.NoteSettlement(num(0), final)
	case "on_settled":
		var ticker, result string
		var final any
		var price *float64
		_ = json.Unmarshal(args[0], &ticker)
		_ = json.Unmarshal(args[1], &result)
		_ = json.Unmarshal(args[2], &final)
		_ = json.Unmarshal(args[4], &price)
		t.OnSettled(ticker, result, final, num(3), price)
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
	if rel := math.Abs(t.Sigma2-want.Sigma2) / want.Sigma2; rel > tolerance {
		return fmt.Sprintf("sigma2 python %v go %v", want.Sigma2, t.Sigma2)
	} else {
		worst["sigma2 (relative)"] = math.Max(worst["sigma2 (relative)"], rel)
	}
	if got := t.OffsetPct(); got != want.Offset {
		return fmt.Sprintf("offset python %v go %v", want.Offset, got)
	}
	for _, a := range t.Accounts {
		v, name := want.Views[a.Params.Name], a.Params.Name
		if v == nil || a.View == nil {
			if (v == nil) != (a.View == nil) {
				return name + ": a view on one side only"
			}
			continue
		}
		if side := v[2].(string); side != a.View.Best.Side {
			return fmt.Sprintf("%s: side python %s go %s", name, side, a.View.Best.Side)
		}
		if bet := v[4].(bool); bet != a.View.Signal.Bet {
			return fmt.Sprintf("%s: bet python %v go %v", name, bet, a.View.Signal.Bet)
		}
		if !gap("p_up", a.View.PUp, v[0].(float64)) || !gap("p_model", a.View.PModel, v[1].(float64)) ||
			!gap("edge", a.View.Best.Edge, v[3].(float64)) {
			return fmt.Sprintf("%s: probabilities differ beyond %g", name, tolerance)
		}
	}
	return ""
}

func gaps(w map[string]float64) string {
	var parts []string
	for _, name := range []string{"sigma2 (relative)", "p_model", "p_up", "edge"} {
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
