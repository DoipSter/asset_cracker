package config

import (
	"testing"
	"time"
)

// AC_V3 is on unless it is exactly "off": the live binary has one engine, and only an explicit
// settle-only switch stops new orders.
func TestV3IsOnUnlessExactlyOff(t *testing.T) {
	for value, want := range map[string]bool{"": true, "on": true, "ON": true, "1": true, "true": true, "off": false, " off": true} {
		t.Setenv("AC_V3", value)
		if got := Load().V3; got != want {
			t.Errorf("AC_V3=%q: V3 = %v, want %v", value, got, want)
		}
	}
}

func TestDatabaseName(t *testing.T) {
	t.Setenv("PGDATABASE", "")
	for url, want := range map[string]string{
		"postgres:///assetcracker?host=/var/run/postgresql":     "assetcracker",
		"postgres:///assetcracker_dev?host=/var/run/postgresql": "assetcracker_dev",
		"host=/var/run/postgresql dbname=assetcracker_dev":      "assetcracker_dev",
		"::not a url::": "",
	} {
		if got := (Config{DatabaseURL: url}).DatabaseName(); got != want {
			t.Errorf("%q: database %q, want %q", url, got, want)
		}
	}
}

func TestFaultSettings(t *testing.T) {
	t.Setenv("AC_V3_FAIL_EVERY", "50")
	t.Setenv("AC_V3_FAIL_RUN", "3")
	t.Setenv("AC_V3_DELAY_COMMIT", "3s")
	t.Setenv("AC_V3_FAIL_OPS", "orders, reads")
	f := Load().V3Faults
	if f.Every != 50 || f.Run != 3 || f.Kind != "delay" || f.DelayCommit != 3*time.Second || len(f.Ops) != 2 || f.Ops[1] != "reads" {
		t.Fatalf("%+v", f)
	}
	t.Setenv("AC_V3_FAIL_EVERY", "")
	if Load().V3Faults.Every != 0 {
		t.Fatal("fault injection is on with AC_V3_FAIL_EVERY unset")
	}
}

// The gate's settings are numbers or nothing: unset and unparseable both read 0, which the
// binary treats as "keep the default for this one".
func TestGateSettings(t *testing.T) {
	t.Setenv("AC_GATE_MIN_EDGE", "0.05")
	t.Setenv("AC_GATE_POWER", "0.9")
	t.Setenv("AC_GATE_MAX_DRAWDOWN_CENTS", "10000")
	if g := Load().Gate; g != (Gate{0.05, 0.9, 10000}) {
		t.Fatalf("%+v", g)
	}
	t.Setenv("AC_GATE_MIN_EDGE", "")
	t.Setenv("AC_GATE_POWER", "most")
	t.Setenv("AC_GATE_MAX_DRAWDOWN_CENTS", "250.00")
	if g := Load().Gate; g != (Gate{}) {
		t.Fatalf("unset and unparseable are 0: %+v", g)
	}
}
