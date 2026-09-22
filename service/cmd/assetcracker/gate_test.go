package main

import (
	"testing"

	"github.com/doipster/asset_cracker/service/internal/analysis"
	"github.com/doipster/asset_cracker/service/internal/config"
)

// Each setting given replaces its default alone; a set that cannot be applied is refused whole.
func TestGateSettings(t *testing.T) {
	if g := gateSettings(config.Gate{}); g != analysis.DefaultGate {
		t.Errorf("nothing given: %+v", g)
	}
	if g := gateSettings(config.Gate{Power: 0.9}); g != (analysis.GateSettings{MinEdgePerDollar: 0.02, Power: 0.9, MaxDrawdownCents: 25_000}) {
		t.Errorf("power alone: %+v", g)
	}
	if g := gateSettings(config.Gate{MinEdgePerDollar: 0.05, MaxDrawdownCents: 10_000}); g != (analysis.GateSettings{MinEdgePerDollar: 0.05, Power: 0.8, MaxDrawdownCents: 10_000}) {
		t.Errorf("two given: %+v", g)
	}
	for name, bad := range map[string]config.Gate{"power 1": {Power: 1}, "negative edge": {MinEdgePerDollar: -1}, "negative drawdown": {MaxDrawdownCents: -5}} {
		if g := gateSettings(bad); g != analysis.DefaultGate {
			t.Errorf("%s: %+v, want the defaults", name, g)
		}
	}
}
