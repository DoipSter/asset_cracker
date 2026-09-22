package app

import (
	"fmt"
	"log/slog"

	"github.com/doipster/asset_cracker/service/internal/analysis"
	"github.com/doipster/asset_cracker/service/internal/config"
)

// gateSettings is the promotion gate's rule as this process applies it: the analysis package's
// defaults, each replaced by the operator's setting where one was given. Settings that together
// cannot be applied (analysis.GateSettings.Valid) are refused whole, with a warning, and the
// defaults stand: a mistyped variable must not make a looser gate.
func gateSettings(cfg config.Gate) analysis.GateSettings {
	g := analysis.DefaultGate
	if cfg.MinEdgePerDollar != 0 {
		g.MinEdgePerDollar = cfg.MinEdgePerDollar
	}
	if cfg.Power != 0 {
		g.Power = cfg.Power
	}
	if cfg.MaxDrawdownCents != 0 {
		g.MaxDrawdownCents = cfg.MaxDrawdownCents
	}
	if !g.Valid() {
		slog.Warn("promotion gate settings cannot be applied; the defaults stand", "given", fmt.Sprintf("%+v", cfg), "defaults", fmt.Sprintf("%+v", analysis.DefaultGate))
		return analysis.DefaultGate
	}
	return g
}
