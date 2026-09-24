package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/doipster/asset_cracker/service/internal/config"
	"github.com/doipster/asset_cracker/service/internal/exercise"
	"github.com/doipster/asset_cracker/service/internal/proposals"
	"github.com/doipster/asset_cracker/service/internal/readsurface"
	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runMCP is `assetcracker mcp`: the read surface (docs/mcp-read-surface.md) and the strategy
// proposal tools as one MCP server on stdin/stdout, for one client, until it hangs up. It opens
// the same database the service would (AC_DATABASE_URL) and runs no feeds, no engine and no
// HTTP server. Every read tool runs inside a READ ONLY transaction (store.ReadOnly). The
// strategy tools (internal/proposals) are a client of the RUNNING service on the Pi's loopback:
// strategy_register adds a DRAFT; strategy_deploy seeds it, through the buckets page's own
// routes and operator key. strategy_exercise (internal/exercise) replays a shape on
// the recorded tape in this process, a read, and asks the service through the same door to
// record that it ran. Those drafts, deploys and records are the only changes this process can cause.
//
// The intended way to reach it from a laptop is over SSH, which keeps the data on the Pi and
// borrows SSH's authentication:
//
//	ssh acdeploy@rpi-v5-1.local env AC_DATABASE_URL=postgres:///assetcracker?host=/var/run/postgresql /opt/assetcracker/current/assetcracker mcp
//
// Only MCP messages may go to stdout. Logging goes to stderr (set in main), which ssh carries
// separately.
func runMCP() error {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()

	server := mcp.NewServer(&mcp.Implementation{Name: "assetcracker", Title: "Asset Cracker: market data, analyses, strategy proposals", Version: version}, nil)
	readsurface.Register(server, db)
	door := proposals.New()
	proposals.RegisterWith(server, door)
	// strategy_exercise: a shape replayed on the tape in this process (a read), its record
	// written through the running service like a registration is.
	exercise.Register(server, db, door, version)
	slog.Info("mcp on stdio", "version", version, "database", cfg.DatabaseName())
	return server.Run(ctx, &mcp.StdioTransport{})
}

// runExercise is `assetcracker exercise`: one strategy_exercise input on stdin, the answer on
// stdout. Used to walk a roster on the Pi with a binary that is not yet the live mcp process.
func runExercise() error {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var in exercise.Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("the input is not a strategy_exercise shape: %w", err)
	}
	ans, err := exercise.NewTool(db, proposals.New(), version).Run(ctx, in)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(ans)
}
