# service

The Asset Cracker service, in Go. See `docs/platform-brief.md` for where it is going.

**Today it records market data and nothing else.** No strategies, no ledger writes, no order
code, no credentials. Every request it makes is an unauthenticated public read.

| Package | |
|---|---|
| `cmd/assetcracker` | Wires everything together; graceful shutdown on SIGINT/SIGTERM |
| `internal/config` | Settings from the environment, with defaults that suit the Pi |
| `internal/store` | Postgres access (pgx). Market data only |
| `internal/coinbase` | Trade prints from Coinbase's WebSocket `matches` channel, to `price_tick` |
| `internal/kalshi` | Once a second per series: the open round, its order-book quotes, to `evaluation`; and how each round settled, to `market` |
| `internal/health` | `GET /healthz` on localhost: prices, rounds, counts; 503 if anything is stale |

What it watches comes from the `instrument` table (`db/migrations/0002_seed_sources.sql`), so
adding a coin is a row, not a code change.

## Settings

| Variable | Default |
|---|---|
| `AC_DATABASE_URL` | `postgres:///assetcracker?host=/var/run/postgresql` (unix socket, peer auth, no password) |
| `AC_HTTP_ADDR` | `127.0.0.1:8377` |
| `AC_USER_AGENT` | `asset-cracker/0.1` |

## Working on it

    cd service && go vet ./... && go test ./...
    deploy/pi/deploy.sh        # test, cross-compile for linux/arm64, copy to /opt/assetcracker

Run it by hand on the Pi against the dev database, as `acdeploy`:

    AC_DATABASE_URL='postgres:///assetcracker_dev?host=/var/run/postgresql' /opt/assetcracker/assetcracker

There is no systemd unit yet. Installing one needs an admin on the Pi, and the real database
needs its roles sorted first (the service should not own the tables: see `db/README.md`).

## Carried over from the Python app, and one thing fixed

Kept because they were measured there: the `matches` channel over `ticker`; quotes from the
order book, never the cached market list; predicting the next round's ticker so it is picked up
seconds after the close.

Fixed: the Python poller re-armed its results watch every second until the next round
appeared, delivering one settlement up to seven times. Here a round is handed to the watch
once, and the database only accepts a result for a market that has none.
`internal/kalshi/poller_test.go` replays that exact scenario.
