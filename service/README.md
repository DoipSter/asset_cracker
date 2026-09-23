# service

The Asset Cracker service, in Go. See `docs/platform-brief.md` for where it is going.

**Today it records market data and runs one live engine (integer cents, paper broker) in simulation.**
v1 and v2 are frozen archives, imported only by `cmd/replay` and `cmd/replay2`. There is no order
code and there are no credentials. The pages are served on localhost. Every ledger account is `sim`.

The buckets page is the operator's desk for the paper fund: turn new orders on or off; approve or
retire a version-3 strategy, which seeds a fresh $1,000 bucket or holds one settle-only at once,
without a process start (`Runner3.Reload`); set the four sustainment allocation rates, taken at
settlement from a bucket's gain above its high-water mark, by typing over the figures of the
rule in force (the remainder follows; a save row with the reason appears while a figure
differs); and reset the simulated books, which
empties them and starts again from the approved versions. A bucket that loses its last bet with
less than the floor left is closed at that settlement: reaped into replenishment, frozen, not
replaced.

**The bucket's lifecycle by hand** (each version row): **Reap** closes the version's bucket once
nothing is open, what it holds going to the common pool and the bucket frozen with its record
(`Runner3.Reap`, refused while a bet is on); **Reap & restake** does that and seeds "`<name>
life N`" from the pool at once (the owners cover a shortfall, recorded as its own deposit);
**Restake** opens the next life of a version whose every bucket is frozen. Approving a retired
version resumes its held bucket as it stands; a version whose bucket was reaped comes back by
Restake, not by Approve. **Remix** fills the builder with a version's own numbers
(`engine.ToShape`) so a variant starts from what traded. **Transfer** moves simulated money by
hand: out of a bucket into a reserve or the pool, between the pools and reserves, to or from the
owners; never into a bucket (the allocator would read it as a gain). Money taken out of a bucket
by hand lowers its high-water mark by the same amount (`store.HighWaterMark`), so the strategy is
not asked to earn it back before its next gain counts. Each is a ledger transfer with a memo.

**The strategy builder** (buckets page, "New strategy") makes a version-3 row from a shape:
the exit rule (`hold` or `ev`), a side filter (whichever the belief favours, the favourite only,
the longshot only), time gates, price band, bets per round, the Kelly fraction and window cap, a
volatility trigger, and the sizing (Kelly, or a bounded martingale registered as a negative
control). Eight presets fill the form: Value, Late, Favourite, Model, Scalper, Calm Scalper, Tail,
Martingale control. Every number set is a `convention` the owner chose, every one left blank is
`inherited` from the parent, the name carries "(conventions)", and `engine.Params.Validate` is
the gate (`engine.FromShape`). No shape can make the engine buy where its belief has no edge after
costs; the shapes decide when, which side, how much and how to leave. Registered as `draft`;
Approve seeds it. Each is one more trial the leaderboard's threshold is corrected for. Versions
whose four numbers were MEASURED come only from `cmd/measure3` (`docs/v3-measurement-protocol.md`).

| Package | |
|---|---|
| `cmd/assetcracker` | Thin main: `app.Run`, and `assetcracker mcp` |
| `internal/app` | Start ingest, one runner, HTTP |
| `internal/engine` | The live kalshi15m engine: pure `Decide` / `Fold`, integer cents |
| `internal/broker` | Paper fills from the recorded book |
| `internal/runner` | The live runner (Runner3): load and reload of the held set, steps, settlements, the sustainment allocation, the run-out close |
| `internal/config` | Settings from the environment, with defaults that suit the Pi |
| `internal/store` | Postgres access (pgx). Imports none of our packages |
| `internal/analysis` | Promotion-gate figures; maps store reads into facts |
| `internal/coinbase` | Trade prints from Coinbase's WebSocket `matches` channel, to `price_tick` |
| `internal/kalshi` | Once a second per series: the open round, its order-book quotes, to `evaluation`; and how each round settled, to `market` |
| `internal/legacy` | Frozen v1/v2 engines and runners. Replay only |
| `internal/pyfloat` | CPython's float rounding, repr and floor division, where Go differs |
| `cmd/replay` | v1 parity gate |
| `cmd/replay2` | v2 parity gate |
| `cmd/measure3` | The v3 measurement protocol, executed: `train` walks to TRAIN's 480th window and freezes lambda and the staleness costs; `test` is the one look at TEST; `emit-migration` writes the version rows. Read-only, as `assetcracker_ro` over the SSH tunnel; refuses a protocol text other than the one it embeds |
| `internal/web` | The status page, and the buckets-page controls (orders, version-3 approval, allocation, sim reset) |
| `internal/health` | `GET /healthz` on localhost: prices, rounds, counts; 503 if anything is stale |
| `internal/readsurface` | The agents' read-only door: the MCP tools of `assetcracker mcp`. Windowed, capped, paged reads of candles, prints, the book and markets, and in-database summaries (returns, vol profile, momentum grid, features, stored analyses). Every call in a READ ONLY transaction with a timeout. `docs/mcp-read-surface.md` |
| `internal/proposals` | The agents' door to the strategy builder, on the same MCP server: presets, a dry run through the engine, registration of a draft through the running service's route and operator key, the registry. Imports the engine only |

What it watches comes from the `instrument` table (`db/migrations/0002_seed_sources.sql`), so
adding a coin is a row, not a code change.

## Seeing it

    tools/view.sh          # from the Mac: opens an SSH tunnel to the Pi and the page in a browser
    tools/view.sh stop

The page is doipster's phone widget, rebuilt in the browser from his drawing code and kept in
step with `main` (last matched 2026-09-21, dc10fd4): same palette and proportions; a pill per
coin with its coloured sign; the bell, world toggle and close level with the island; the index
price to each coin's own precision; the price-to-beat card with its countdown; the 15-minute
round chart with a marker for every bet; the 1M chart drawn from the live feed, one dot per
second, with its tick count; 15M, 1H, 24H and 7D; the strategies panel on the right, following
the coin you are looking at, with Account, Log and Strategies. The bell turns on browser
notifications for the tracked strategy.

The live engine is v3 (integer cents, paper broker). v1 and v2 are frozen archives: their
ledger rows remain, they place no new sim bets, and `cmd/replay` / `cmd/replay2` still check
them. The Python widget stays as it is (INT-10). Not ported: the bankruptcy post-mortem file
(the ledger and journal hold the same facts) and the per-round CSV (rounds are rows in
`market`). Not carried over by choice: Pause all and Reset all (the page is read-only), the
amber glow after a bet, and dragging a frameless window.

## Reading it from an agent, and proposing a strategy

    assetcracker mcp       # the read surface and the proposal tools on stdin/stdout; nothing else runs

The repository's `.cursor/mcp.json` runs that over SSH against the record (`assetcracker`) on
the Pi, as `assetcracker_ro`, so Cursor's agent can call `instruments`, `candles`, `bars`,
`book`, `markets`, `returns_summary`, `vol_profile`, `momentum_grid`, `features` and
`analysis_results` directly. It uses the release binary. The same server carries the strategy
builder as four tools: `strategy_presets`, `strategy_build` (a dry run, every number labelled),
`strategy_register` (a draft, through the running service's route and operator key; Approve
stays a click on the buckets page) and `strategy_versions` (the registry). The key comes from
`~/.config/assetcracker/operator_key` on the Pi, put there once by the owner. What each tool
returns, and what it deliberately cannot do, is in `docs/mcp-read-surface.md`.

## Settings

| Variable | Default |
|---|---|
| `AC_DATABASE_URL` | `postgres:///assetcracker?host=/var/run/postgresql` (unix socket, peer auth, no password) |
| `AC_HTTP_ADDR` | `127.0.0.1:8377`. `0.0.0.0:8377` puts the page on the house network; set the key below with it |
| `AC_OPERATOR_KEY` | unset: no passphrase. Set, every change on the buckets page and the assets dialog needs it (`X-Operator-Key`); reads never do |
| `AC_USER_AGENT` | `asset-cracker/0.1` |
| `AC_V3` | on unless exactly `off`. The buckets page can override this; with no saved switch, off means settle-only |
| `AC_GATE_MIN_EDGE` | `0.02`: the after-fee return per dollar staked the promotion gate's sample floor is sized to find |
| `AC_GATE_POWER` | `0.8`: the chance of finding it when it is there |
| `AC_GATE_MAX_DRAWDOWN_CENTS` | `25000`: the deepest fall from peak the gate allows |

The three gate settings are read by `cmd/assetcracker`; one that is unset or does not parse keeps
its default, and a set that cannot be applied (a power of 1, a negative edge) is refused whole with
a warning, so a mistyped variable never makes a looser gate. The rule they set is described in
`docs/api-home.md` under "The promotion gate".

## Working on it

    cd service && go vet ./... && go test ./...
    tools/guard-frozen.sh      # frozen v1/v2 archives must not change unless AC_ALLOW_LEGACY=1
    deploy/pi/deploy.sh        # test, cross-compile for linux/arm64, copy to /opt/assetcracker

The running service is the systemd unit on the Pi, database `assetcracker`. It refuses to start
against a scratch database (`*_dev`): that database rehearses migrations and constraint tests
and is not a second market. See `docs/deployment.md`.

## The strategy port and its parity gate

    cd service && go run ./cmd/replay ../tools/parity/fixtures/2026-09-20_40min

On the 40-minute fixture (2026-09-20, Go 1.27.1): all 4,733 steps agree with the Python's trace
(same side and same bet/no-bet call for every strategy; volatility and index offset
bit-identical; probabilities and edges within 2.2e-16), the 60 trade rows are identical
character for character, and the saved state matches. The same binary gives the same result on
the Pi. `go test ./...` runs it against every fixture.

Two things that had to be got right, both measured:

- **Fused multiply-add.** Go may fuse `x*y + z` into one operation and does on arm64. That
  moved probabilities by up to 3.7e-13 and, worse, would make the Pi and an Intel machine
  disagree. Every product feeding an addition is wrapped in `float64(...)`, which the spec says
  forbids the fusion. After that, volatility matched bit for bit.
- **Different maths libraries.** CPython uses the platform C library's `erf`, `log` and `pow`;
  Go has its own. They differ by at most a few units in the last place
  (`internal/pyfloat` measures it), so probabilities are compared to 1e-12, not exactly.

The trade log's `time` column is local time, as the Python writes it, so a replay compares
equal only in the time zone the recording was made in.

The port keeps the Python's float-dollar bookkeeping on purpose, so that it can be checked
against it. The platform's ledger is integer cents; joining the two is the paper broker's job.

## Carried over from the Python app, and one thing fixed

Kept because they were measured there: the `matches` channel over `ticker`; quotes from the
order book, never the cached market list; predicting the next round's ticker so it is picked up
seconds after the close.

Fixed: the Python poller re-armed its results watch every second until the next round
appeared, delivering one settlement up to seven times. Here a round is handed to the watch
once, and the database only accepts a result for a market that has none.
`internal/kalshi/poller_test.go` replays that exact scenario.
