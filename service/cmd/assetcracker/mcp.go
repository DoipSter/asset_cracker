package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/doipster/asset_cracker/service/internal/config"
	"github.com/doipster/asset_cracker/service/internal/proposals"
	"github.com/doipster/asset_cracker/service/internal/readsurface"
	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runMCP is `assetcracker mcp`: the read surface (docs/mcp-read-surface.md) and the strategy
// proposal tools as one MCP server on stdin/stdout, for one client, until it hangs up. It opens
// the same database the service would (AC_DATABASE_URL) and runs no feeds, no engine and no
// HTTP server. Every read tool runs inside a READ ONLY transaction (store.ReadOnly). The four
// strategy tools (internal/proposals) are a client of the RUNNING service on the Pi's loopback:
// strategy_register asks it, through the buckets page's own route and operator key, to add a
// DRAFT to the registry; the rest read. That draft is the only change this process can cause.
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
	proposals.Register(server)
	slog.Info("mcp on stdio", "version", version, "database", cfg.DatabaseName())
	return server.Run(ctx, &mcp.StdioTransport{})
}
