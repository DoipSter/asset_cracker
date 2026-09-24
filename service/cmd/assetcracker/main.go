// Command assetcracker is the Asset Cracker service: ingest, one live engine, HTTP, and
// `assetcracker mcp` for the read surface. Simulated money only; no real orders.
package main

import (
	"log/slog"
	"os"

	"github.com/doipster/asset_cracker/service/internal/app"
)

var version = "dev" // set at build time by deploy/pi/deploy.sh

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	// Subcommands: `assetcracker mcp` serves the read surface on stdio; `assetcracker exercise`
	// reads one strategy_exercise input from stdin and writes the answer. Everything else is the service.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		if err := runMCP(); err != nil {
			slog.Error("mcp stopped", "err", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "exercise" {
		if err := runExercise(); err != nil {
			slog.Error("exercise stopped", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := app.Run(version); err != nil {
		slog.Error("stopped", "err", err)
		os.Exit(1)
	}
}
