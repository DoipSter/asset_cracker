package pyfloat

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
)

func load(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

func f(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

func ulps(a, b float64) int64 {
	if a == b {
		return 0
	}
	d := int64(math.Float64bits(a)) - int64(math.Float64bits(b))
	if d < 0 {
		d = -d
	}
	return d
}

func TestAgainstCPython(t *testing.T) {
	var v struct {
		Round    [][]any    `json:"round"`
		Repr     []string   `json:"repr"`
		FloorDiv [][]string `json:"floordiv"`
		Erf      [][]string `json:"erf"`
	}
	load(t, "pyfloat_vectors.json", &v)
	for _, row := range v.Round {
		if got := Repr(Round(f(row[0].(string)), int(row[1].(float64)))); got != row[2].(string) {
			t.Fatalf("round(%v, %v) = %s, want %s", row[0], row[1], got, row[2])
		}
	}
	for _, text := range v.Repr {
		if got := Repr(f(text)); got != text {
			t.Fatalf("repr: got %s, want %s", got, text)
		}
	}
	for _, row := range v.FloorDiv {
		if got := Repr(FloorDiv(f(row[0]), f(row[1]))); got != row[2] {
			t.Fatalf("%s // %s = %s, want %s", row[0], row[1], got, row[2])
		}
	}
	worst := int64(0)
	for _, row := range v.Erf {
		worst = max(worst, ulps(math.Erf(f(row[0])), f(row[1])))
	}
	t.Logf("math.Erf vs CPython on macOS: worst %d ulp over %d values", worst, len(v.Erf))
	if worst > 3 {
		t.Errorf("math.Erf is %d ulp from CPython; expected at most 3", worst)
	}

	var truth [][]string
	load(t, "erf_truth.json", &truth)
	worst = 0
	for _, row := range truth {
		worst = max(worst, ulps(math.Erf(f(row[0])), f(row[1])))
	}
	if worst > 1 {
		t.Errorf("math.Erf is %d ulp from the true value; expected at most 1", worst)
	}
}

// How closely Go's own math library tracks the platform C library CPython uses. These do not
// have to be identical, but the parity replay's tolerances rest on how far apart they are.
func TestLibmDistance(t *testing.T) {
	var v struct {
		PowHalf [][]any    `json:"pow_half"`
		Log     [][]string `json:"log"`
		Exp     [][]string `json:"exp"`
	}
	load(t, "libm_vectors.json", &v)
	var wPow, wLog, wExp int64
	var nPow, nLog, nExp int
	for _, row := range v.PowHalf {
		d := ulps(math.Pow(0.5, row[0].(float64)/300), f(row[1].(string)))
		wPow = max(wPow, d)
		if d > 0 {
			nPow++
		}
	}
	for _, row := range v.Log {
		d := ulps(math.Log(f(row[0])), f(row[1]))
		wLog = max(wLog, d)
		if d > 0 {
			nLog++
		}
	}
	for _, row := range v.Exp {
		d := ulps(math.Exp(f(row[0])), f(row[1]))
		wExp = max(wExp, d)
		if d > 0 {
			nExp++
		}
	}
	t.Logf("pow(0.5, dt/300): %d of %d differ, worst %d ulp", nPow, len(v.PowHalf), wPow)
	t.Logf("log:              %d of %d differ, worst %d ulp", nLog, len(v.Log), wLog)
	t.Logf("exp:              %d of %d differ, worst %d ulp", nExp, len(v.Exp), wExp)
	if wPow > 2 || wLog > 2 || wExp > 2 {
		t.Errorf("Go's math library is further from CPython's than expected")
	}
}
