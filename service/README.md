# service

The Asset Cracker service, in Go. See `docs/platform-brief.md` for where it is going.

**Today it records market data and runs one live engine (integer cents, paper broker) in simulation.**
v1 and v2 are frozen archives, imported only by `cmd/replay` and `cmd/replay2`. There is no order
code and there are no credentials. The pages are served on localhost. Every ledger account is `sim`.

The two pages divide the operator's desk (2026-09-23). **The home page holds the bank.** Its
balance sheet lists the bank's accounts, and each account's row has a menu (a right-click, a long
press, or its ⋯): move money out of it or into it, and on replenishment schedule a payday; the
bank's own row sets the four sustainment allocation rates (each account also shows its rate
beside its balance), schedules a payday, and closes the bank for the next one (the reset, after
typing `reset sim`). The paydays scheduled and the rule in force stand under the sheet. **The
buckets page deploys strategy accounts.** Its deploy form takes a strategy from the registry, a
seed in dollars ($1,000 by default, the convention every bucket so far started at), and where
the seed is drawn from: replenishment (refused when it is short) or the bank (the owners deposit
it). Deploy seeds the bucket, puts a draft or retired version on probation in the same write,
and the engine loads it without a process start (`Runner3.Deploy`, `store.DeployBucket`); a
version that holds a bucket must be closed out first (the × on its row). The registry under the
form retires a version (its bucket held settle-only), resumes a retired one that still holds a
bucket, and remixes; the new-orders switch is there too. A bucket that loses its last bet with
less than the floor left is closed at that settlement: reaped into replenishment, frozen, not
replaced. Each bucket's row says where its seed came from, read off the seed's ledger memo.

**Two market families, one engine** (2026-09-23). A version belongs to `kalshi15m`, the
15-minute rounds, or `kalshiladder`, Kalshi's daily and weekly above/below ladders (`KXBTCD` and
friends: a strike every $500, each leg open about a week, closing 5 pm ET). Each family has its
own `Runner3` instance over the same engine code, its own Paper and held set, and its own bucket
prefix (`kalshi15m3`, `kalshiladder3`); the buckets page's controls span both. The ladder runner
is fed by the ladder recorders (`kalshi.LadderRecorder` with an `Engine`): each minute, every
two-sided leg gets the model's view journaled with its row and one look by the engine; each
stored result settles what the buckets hold. A leg more than an hour from its close is priced
with the coin's **long volatility**, the standard deviation of its last sixty daily candle
returns per sqrt-second (`engine.LongSigmaFromDaily`, set at start), not the five-minute
estimate that prices the rounds; the view says which (`horizon`). The builder's **Markets**
choice picks the family; a ladder shape must set `tau_max` above an hour, tau is in seconds
(3 h = 10800, 30 h = 108000, 7 d = 604800), and the window cap is per close date, because every
leg closing at the same instant is one window. Presets: Day Value, Day Favourite, Week Value.
Designed from the model side on purpose: `docs/longshot-protocol.md` reserves the price-band
question on these horizons for its one look each.

**The volatility forecast and the ladder watcher** (2026-09-23, `internal/app/volmodel.go`,
`internal/engine/vol.go`, `internal/engine/ladder_fit.go`). The long volatility above is set at
start from the daily closes and then replaced, at start and every six hours, by a HAR-RV forecast
(Corsi: tomorrow's realised variance on today's, the week's mean and the month's mean), fitted by
least squares on the daily realised variances built from three years of recorded hourly candles.
Each fit is scored on its last ninety days out of sample against the sixty-day mean it replaces,
and the log carries the coefficients, the in-sample R², the holdout errors and the forecast; the
rounds keep their fast estimate untouched while the measurement protocol runs. The same object
watches every ladder close with three or more two-sided legs each minute: the lognormal the
market's mids imply (its sigma against the forecast, its median, the fit's error) and any pair of
legs whose prices cross the order of their strikes, net of Kalshi's fee. It writes one
`analysis_result` row per series and close every ten minutes under `ladder.implied`, warns once
per half hour when a crossing beats the fee, and places no order: the engine trades one leg at a
time, and a pair is a later design. All of it is read from prices and quotes, never from an
outcome. `docs/calibration-protocol.md` is the DRAFT of the third model, a fitted belief; it
waits on the owner's yes because it reads outcomes.

**The bucket's lifecycle by hand.** The × on a bucket's row closes it out once nothing is open:
what it holds goes to the common pool and the bucket is frozen with its record (`Runner3.Reap`
for the live engine, refused while a bet is on; the ledger alone for an old engine's). The next
life is a **Deploy** of the same version from the buckets page, at whatever seed and source the
operator chooses ("`<name> life N`"). `Runner3.Reap` with restake, and `api/controls/version/reap`,
remain for the engine's own restart from the pool (the owners cover a shortfall, recorded as its
own deposit). **Resume** puts a retired version that still holds a bucket back on probation, as
it stands. **Remix** fills the builder with a version's own numbers (`engine.ToShape`) so a
variant starts from what traded. **Moving money** is the home page's: from an account's menu,
out of a bucket into a reserve or the pool, between the pools and reserves, to or from the
owners; never into a bucket (the allocator would read it as a gain). Money taken out of a bucket
by hand lowers its high-water mark by the same amount (`store.HighWaterMark`), so the strategy is
not asked to earn it back before its next gain counts. Each is a ledger transfer with a memo. A
bucket's own seed, from the ledger, is the figure the engine measures it against: the allocator's
mark until its first high, the window cap (`window_cap_bps` of the lesser of equity and seed) and
the martingale ceiling (`engine.Account.Seed`), so a bucket deployed at $500 is sized as a $500
bucket and not asked to reach $1,000 first. The version's `seed_cents` stays the convention the
engine seeds at when nobody chose.

**The strategy builder** (buckets page, "New strategy") makes a version-3 row from a shape:
the exit rule (`hold` or `ev`), a side filter (whichever the belief favours, the favourite only,
the longshot only), time gates, price band, bets per round, the Kelly fraction and window cap, a
volatility trigger, a late window for the model's weight (`lambda_late` inside `lambda_late_tau`
seconds of the close; 2026-09-23), a roster (`members`, 2 to 8 shapes plus a pick: structural
eligibility then recent shadow return; one version, not a bucket that rewrites its
`strategy_version_id`; 2026-09-23), and the sizing (Kelly, or a bounded martingale registered as a
negative control). Eleven presets fill the form: Value, Late, Favourite, Model, Scalper, Calm
Scalper, Tail, Martingale control, and three ladder shapes. Every number set is a `convention` the owner chose, every one left blank is
`inherited` from the parent, the name carries "(conventions)", and `engine.Params.Validate` is
the gate (`engine.FromShape`). No shape can make the engine buy where its belief has no edge after
costs; the shapes decide when, which side, how much and how to leave. Registered as `draft`;
deploying it seeds it. Each is one more trial the leaderboard's threshold is corrected for. Versions
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
| `internal/exercise` | `strategy_exercise`, on the same MCP server: a shape or a roster replayed through the live engine and paper broker on the recorded tape over a settled window, answered as a leaderboard-style row with P&L by entry time, price and member, pick counts, every blocked decision counted, and the orders that filled. Every run is recorded as an `analysis_result` (`strategy.exercise`) through the service's `POST /api/controls/exercise/record`; 15-minute windows past the protocol's TRAIN are refused until TEST has been looked at |

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
`strategy_register` (a draft, through the running service's route and operator key; deploying
it stays a click on the buckets page) and `strategy_versions` (the registry). The key comes from
`~/.config/assetcracker/operator_key` on the Pi, put there once by the owner. What each tool
returns, and what it deliberately cannot do, is in `docs/mcp-read-surface.md`.

## Settings

| Variable | Default |
|---|---|
| `AC_DATABASE_URL` | `postgres:///assetcracker?host=/var/run/postgresql` (unix socket, peer auth, no password) |
| `AC_HTTP_ADDR` | `127.0.0.1:8377`. `0.0.0.0:8377` puts the page on the house network; set the key below with it |
| `AC_OPERATOR_KEY` | unset: no passphrase. Set, every change on either page (the bank's menus on home, the deploy form and registry on buckets) and in the assets dialog needs it (`X-Operator-Key`); reads never do |
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
    deploy/pi/deploy.sh check  # before a release: what the Pi runs, and whether this commit may replace it

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
