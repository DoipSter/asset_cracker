package main

import (
	"encoding/json"
	"math"
	"os"
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
	// Both passed: the engine still refuses, because drift_tol has no measurement under the
	// protocol as it stands. This is the check that keeps an unmeasured number out of the record.
	res.R2.Pass = true
	if err := writeJSON(r.researchPath(testResultFile), res); err != nil {
		t.Fatal(err)
	}
	if err := emitMigration(r); err == nil {
		t.Fatal("emit must refuse while drift_tol is not accepted by engine.Validate")
	} else if _, statErr := os.Stat(filepath.Join(r.Root, "db", "migrations", migrationName)); statErr == nil {
		t.Fatal("a migration was written despite the refusal")
	}
}
