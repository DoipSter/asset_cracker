package main

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tool refuses to run against a protocol text other than the one it embeds, and finds
// the checkout it runs from. It does not need a database for either.
func TestFindsTheCheckoutAndEmbedsTheProtocol(t *testing.T) {
	r, err := findRepo(".")
	if err != nil {
		t.Skip("not run from inside the checkout:", err)
	}
	sum, err := sha256File(filepath.Join(r.Root, protocolPath))
	if err != nil {
		t.Fatal(err)
	}
	if sum != protocolSHA {
		t.Fatalf("the protocol text has changed (sha %s); the tool embeds %s and must be read again under the new amendment", sum[:12], protocolSHA[:12])
	}
	if _, err := findRepo(os.TempDir()); err == nil {
		t.Error("a directory outside any checkout must not be found")
	}
}

func TestNaNIsWrittenAsNull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")
	res := trainResult{Notes: []string{}}
	res.M1.LHat.SE = math.NaN()
	res.Power.Power192 = math.Inf(1)
	res.M3.Mean = math.NaN()
	if err := writeJSON(path, res); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	if strings.Contains(s, "NaN") || strings.Contains(s, "Inf") {
		t.Fatalf("NaN or Inf reached the file:\n%s", s)
	}
	if !strings.Contains(s, `"se": null`) || !strings.Contains(s, `"power_at_192_windows": null`) || !strings.Contains(s, `"mean": null`) {
		t.Fatalf("nulls missing:\n%s", s)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["provenance"].(map[string]any)["t_c"]; !ok {
		t.Fatal("times must survive the walk")
	}
}

func TestFilesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x", frozenFile)
	fp := frozenParams{Lambda: 0.42, TrainWindows: []int64{1, 2}, Ineligible: map[string]string{"k": "no result: KXETH15M"}, FrozenAt: time.Unix(0, 0).UTC()}
	if err := writeJSON(path, fp); err != nil {
		t.Fatal(err)
	}
	var back frozenParams
	if ok, err := readJSON(path, &back); err != nil || !ok || back.Lambda != 0.42 || len(back.TrainWindows) != 2 || back.Ineligible["k"] == "" {
		t.Fatalf("ok %v err %v back %+v", ok, err, back)
	}
	if ok, err := readJSON(filepath.Join(dir, "missing.json"), &back); ok || err != nil {
		t.Fatal("a missing file is not an error")
	}
	runs := filepath.Join(dir, runsFile)
	for i := 0; i < 2; i++ {
		if err := appendRun(runs, runRecord{Command: "train", Note: "n"}); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(runs)
	if n := len(splitLines(string(b))); n != 2 {
		t.Fatalf("%d lines in runs.jsonl", n)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func TestEmitRefusesWithoutResults(t *testing.T) {
	r := repo{Root: t.TempDir()}
	if err := emitMigration(r); err == nil {
		t.Fatal("emit with no frozen-params.json must refuse")
	}
	if err := writeJSON(r.researchPath(frozenFile), frozenParams{Lambda: 0}); err != nil {
		t.Fatal(err)
	}
	if err := emitMigration(r); err == nil {
		t.Fatal("emit with lambda 0 must refuse: R1 failed")
	}
	if err := writeJSON(r.researchPath(frozenFile), frozenParams{Lambda: 0.4, ProtocolSHA: protocolSHA}); err != nil {
		t.Fatal(err)
	}
	if err := emitMigration(r); err == nil {
		t.Fatal("emit without test-result.json must refuse")
	}
	res := testResult{Frozen: frozenParams{ProtocolSHA: protocolSHA}}
	res.R2.Pass = false
	res.R2.Verdict = "fail"
	if err := writeJSON(r.researchPath(testResultFile), res); err != nil {
		t.Fatal(err)
	}
	if err := emitMigration(r); err == nil {
		t.Fatal("emit after a failed R2 must refuse")
	}
	// Both passed, and the frozen costs are whole ten-thousandths: the migration is written,
	// with drift_tol as the third amendment's fact 0. A frozen cost the engine refuses (finer
	// than 0.0001) still stops it: the engine's rule, not this tool's, decides.
	res.R2.Pass = true
	if err := writeJSON(r.researchPath(testResultFile), res); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(r.researchPath(frozenFile), frozenParams{Lambda: 0.4, StaleCost: 0.00125, StaleCostSell: 0.001, ProtocolSHA: protocolSHA}); err != nil {
		t.Fatal(err)
	}
	if err := emitMigration(r); err == nil {
		t.Fatal("a cost finer than 0.0001 must be refused by the engine")
	}
	if err := writeJSON(r.researchPath(frozenFile), frozenParams{Lambda: 0.4, StaleCost: 0.0012, StaleCostSell: 0.001, ProtocolSHA: protocolSHA}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.Root, "db", "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	// emit records HEAD's sha as code_ref: give the temp dir one commit.
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v %s", args, err, out)
		}
	}
	if err := emitMigration(r); err != nil {
		t.Fatalf("emit refused after both rules passed: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(r.Root, "db", "migrations", migrationName))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"drift_tol":0`, `"kind":"fact"`, `"lambda":0.4`, `"stale_cost":0.0012`, "'Scalper'", "'Value'", "'draft'", "DEV PLUMBING%"} {
		if !strings.Contains(s, want) {
			t.Errorf("migration lacks %s", want)
		}
	}
	if strings.Contains(s, "placeholder") && !strings.Contains(s, "position('placeholder'") {
		t.Error("a placeholder reached the migration")
	}
}
