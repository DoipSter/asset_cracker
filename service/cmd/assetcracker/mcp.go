package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/doipster/asset_cracker/service/internal/config"
	"github.com/doipster/asset_cracker/service/internal/readsurface"
	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runMCP is `assetcracker mcp`: the read surface (docs/mcp-read-surface.md) as an MCP server on
// stdin/stdout, for one client, until it hangs up. It opens the same database the service would
// (AC_DATABASE_URL) and nothing else: no feeds, no engines, no HTTP. Every tool reads inside a
// READ ONLY transaction (store.ReadOnly), so this process can change nothing.
//
// The intended way to reach it from a laptop is over SSH, which keeps the data on the Pi and
// borrows SSH's authentication:
//
//	ssh acdeploy@rpi-v5-1.local env AC_DATABASE_URL=postgres:///assetcracker_dev?host=/var/run/postgresql /opt/assetcracker/dev/assetcracker mcp
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

	server := mcp.NewServer(&mcp.Implementation{Name: "assetcracker", Title: "Asset Cracker read surface", Version: version}, nil)
	readsurface.Register(server, db)
	slog.Info("mcp read surface on stdio", "version", version, "database", cfg.DatabaseName())
	return server.Run(ctx, &mcp.StdioTransport{})
}
