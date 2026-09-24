# The agents' door: `assetcracker mcp`

How an agent, or a script driving one, reads the recorded market data and the analyses computed
from it, without reading whole tables; and, since 2026-09-22, how it proposes a strategy. It is
"Agent access (MCP)" in `platform-brief.md` section 9: the read surface (market data and
analysis results, read-only), and the first proposal tool (draft a strategy version, which waits
for a person). The ledger, the journal and the metrics (TSK-42) are not here yet; they will join
under the same server. The proposal tools are in their own section, [The proposal door](#the-proposal-door-strategies),
because they are the one part of this server that can change anything.

Brad asked for it on 2026-09-21: a surface to query the data in a windowed, efficient pattern,
so static parsers can summarise it without the full set, and a way to read the derivative, the
analyses themselves, through the same door.

## What it is

`assetcracker mcp` is a subcommand of the service binary. It opens the database the service
would (`AC_DATABASE_URL`) and speaks the Model Context Protocol on stdin/stdout to one client
until the client hangs up. No feeds, no engines, no HTTP, no writes. It runs where Postgres runs,
on the Pi; a laptop reaches it over SSH, so the data stays on the Pi and SSH's key is the
authentication. Code: `service/internal/readsurface` (the tools), `service/internal/store/readonly.go`
(the transaction they run in), `service/cmd/assetcracker/mcp.go` (the subcommand).

### Guarantees

These hold for the ten read tools. The five `strategy_*` tools are described in their own section
below: three are a client of the running service (one of them registers a draft), one is a dry run
in this process, and `strategy_exercise` reads the record under the same guarantees and asks the
service to record that it ran.

- **Read-only by the database, not by convention.** Every call runs in a `READ ONLY` transaction:
  Postgres itself refuses any insert, update, delete or DDL inside one, whatever the role. In the
  record the transaction also takes the `assetcracker_ro` role (`db/grants.sql` says what
  it may see). Against a database where that role has no grants, it stays the connected user, still read-only.
- **Bounded.** Every statement has a timeout: 10 s, or 30 s for the two grid tools. Raw reads
  return at most `limit` rows (default 500, cap 2000) and say `truncated: true` with a `next`
  cursor when the window held more. Reads that bin before they limit (`bars`, `book`, resampled
  `candles`) cut the *window* to `limit` bins first, so the scan is bounded by the window and
  never by how much fell in it.
- **Windowed.** Every read takes a `from` and an optional `to` (default now) and touches only
  rows inside them, through the indexes the tables already have: `candle (instrument_id,
  granularity_s, at)`, `price_tick (instrument_id, at)`, `evaluation (market_id, at)`,
  `market (instrument_id, closes_at)`. Times accept RFC 3339, a date, or `-7d`, `-36h`, `-2w`.
- **Small on the wire.** Answers are `{"columns": [...], "rows": [[...], ...], "next", "truncated",
  "note"}`: names once, then arrays. `note` states the units and conventions of that answer
  (bar start times, log returns, basis points) so a reader is never guessing.

## Connecting

From Cursor on the Mac, the repository's `.cursor/mcp.json` declares the server as an SSH
command against the **record** (`assetcracker`):

```json
{
  "mcpServers": {
    "assetcracker": {
      "command": "ssh",
      "args": ["-o", "BatchMode=yes", "acdeploy@rpi-v5-1.local",
               "env", "AC_DATABASE_URL=postgres:///assetcracker?host=/var/run/postgresql",
               "/opt/assetcracker/current/assetcracker", "mcp"]
    }
  }
}
```

The binary is the release systemd is running. Any MCP client works the same way: run that
command, speak the protocol on its stdin/stdout. Logging goes to stderr, which ssh carries
separately.

`acdeploy` connects as itself and the transaction takes `assetcracker_ro`
(`deploy/pi/setup-prod-db.sh` grants that membership). The scratch database is not a second
copy of the tape, and this server does not read it.

## The tools

Call `instruments` first: it says what is recorded and how far it reaches, so windows can be
planned to hold data.

| Tool | Reads | Answers |
|---|---|---|
| `instruments` | `instrument`, coverage of `candle`, `price_tick`, `market` | One row per instrument: kind, source, first/last hourly and daily candle, first/last trade print, markets and how many settled |
| `candles` | `candle` | Stored 1 h / 1 d OHLCV inside a window, oldest first; `resample_s` (a multiple of the stored length) bins on the way out: 14400 for 4 h bars, 604800 for weeks from daily |
| `bars` | `price_tick` | OHLC, volume and trade count per `bucket_s` seconds from the recorded Coinbase prints (recorded since the service started, not history) |
| `book` | `evaluation`, `market` | The Kalshi book of a series' rounds, the last snapshot per round in every `step_s`: spot price, best bid/ask each side, bid depth within 1c/3c/5c (from 2026-09-21 07:29 UTC), v3's probability and variance |
| `markets` | `market` | Rounds or ladder legs closing in the window: strike, times, result, settlement value. Paged on `(closes_at, ticker)` |
| `returns_summary` | `candle` | One line: bars, mean and sd of per-bar log return (bp), annualised vol, total return, best/worst bar, fraction up, skew |
| `vol_profile` | `candle` | Mean and sd of per-bar return by UTC hour (hourly) or day of week (daily). The intraday shape is the strongest structure in the history |
| `momentum_grid` | `candle` | For each (lookback, horizon) in bars: Pearson, Spearman, long/short return, forward return after up and after down, a naive t. One coin, or all five pooled with ranks within coin |
| `features` | `candle` | The per-bar vector a model would consume: trailing log returns at each lookback, 7- and 30-bar vol and their ratio, 30-bar volume z, hour, day of week. The server reads the padding; the rows are exactly the window |
| `analysis_results` | `analysis_result` | Analyses already computed: key, source, code sha, window, params, when; the result JSON on request |

Every summary uses one return convention: the natural log of a candle's close over the previous
stored candle's close, for the same instrument and length. `candle_return` (below) is that
convention as a view.

### Reading the answers honestly

The summaries return the statistic and, where one is cheap, a naive t. They do not correct for
two things the reader must: overlapping forward windows (a 7-bar horizon has n/7 independent
observations, which `t_naive` already divides by) and the correlation between coins (pairwise
about 0.7 on daily returns: five coins are about 1.3 independent series, so a pooled t should be
halved). A grid of many cells will show a cell near t = 2 by chance. The tools make the look cheap;
they do not make it a test. A test is a protocol (`docs/spot-protocol.md`, `docs/v3-measurement-protocol.md`).

## The derivative: what the analyses write, and the surface reads

Migration `0016_read_surface.sql` adds two objects, neither money:

- **`candle_return`**, a view: every stored candle with `ln(close / previous close)` of the same
  instrument and length. Computed on read, filtered through `candle`'s index; no refresh, no owner.
  It exists so the convention is written once and SQL run by hand uses the same one the tools do.
- **`analysis_result`**, an append-only table: `key` (a dotted name: `candle.momentum_grid.daily`,
  `candle.vol_profile.hour`, `spot.train`), `source` (what computed it), `code_sha`, `params`
  (what makes it reproducible), `window_from`/`window_to`, `computed_at`, `result` (JSON, in the
  analysis's own shape). A result is never edited; a rerun is a new row beside the old, and the
  two can be compared. `db/grants.sql` lets the service role insert and the read-only role select.

The MCP surface **reads** `analysis_result`; it does not write it. Writers are research tools
(`cmd/spot` when it exists, scripts under `research/`) and SQL run by hand. The convention for a
row is the same `columns`/`rows` shape the tools answer in, so a reader handles both alike. The
first two rows in dev were written on 2026-09-21 by an `insert ... select` that computed the
intraday vol profile and the daily momentum grid in the database and stored what it computed,
with `source` naming the exploratory SQL that produced them.

## The proposal door: strategies

Added 2026-09-22, when the strategy builder landed on the buckets page (`service/README.md`,
"The strategy builder"). Brad: "and now we want the mcp tool to be able to do that." Code:
`service/internal/proposals`.

| Tool | Does | Touches |
|---|---|---|
| `strategy_presets` | The eight standard shapes (Value, Late, Favourite, Model, Scalper, Calm Scalper, Tail, Martingale control), each with what it tests | The engine in this binary; nothing else |
| `strategy_build` | A dry run: the shape in, the version the engine would register out, every number labelled `convention` (you set it), `inherited` (the parent's), `limit` or `fact`; refuses exactly as registration would | The engine in this binary; nothing else |
| `strategy_register` | Registers the shape as a **draft** version 3 | The running service, over HTTP on the Pi's loopback |
| `strategy_versions` | The registry as the buckets page shows it: every version 3 with status and hypothesis, the new-orders switch, the allocation rule | The running service, a read |
| `strategy_exercise` | Runs the shape — or a roster of shapes — through the live engine and paper broker on the recorded tape over a window of settled markets: what it would have done. Added 2026-09-23, roster 2026-09-23; [its own section](#exercising-a-shape-on-the-tape) | The record (read-only, `internal/exercise`); the running service, for the one row that records the run |

### How a registration happens, and why that way

`strategy_register` does not write to the database. It builds the shape with the engine first
(so a refusal comes back with its reason before anything leaves the process), then POSTs it to
`http://127.0.0.1:8377/api/controls/version/new`, the route the buckets page uses, with the body
field `via: "mcp"`. The service builds and validates it again, inserts the strategy and its
version-3 row as `draft` with `code_ref` = `proposed via mcp; release <sha>`, and answers with
the id. Three reasons for the detour:

- **One write path, one gate.** The service holds the operator key and the write grants
  (`db/grants.sql`); the MCP process runs as `acdeploy`, which reads the record only as
  `assetcracker_ro` and can insert nothing. There is no second door to widen.
- **A draft trades nothing.** Seeding is the Deploy form on the buckets page (a seed and where
  it is drawn from), which stays a person's (brief §9: "each proposal waits for a person"). No
  tool here deploys, retires, moves money or places an order.
- **The registry says who.** `code_ref` records the origin; every registration is one more trial
  the leaderboard's threshold is corrected for, whoever made it.

### The operator key

When the service runs with `AC_OPERATOR_KEY` (it does, since the page went on the LAN), the
POST must carry it. The MCP process looks, at call time, in `AC_OPERATOR_KEY`, then in the file
named by `AC_OPERATOR_KEY_FILE`, then in `~/.config/assetcracker/operator_key` of the user
running it. The key never appears in a tool's answer, a log line, git, chat or Agora. Putting it
in place is one command by the owner, once, typed on the Pi's side of an SSH session:

```sh
ssh acdeploy@rpi-v5-1.local 'umask 077; mkdir -p ~/.config/assetcracker; read -rs k; printf "%s\n" "$k" > ~/.config/assetcracker/operator_key'
```

(type the key, Enter; nothing echoes). Until it is there, `strategy_register` returns the
service's 401 and says where the key goes; the three other tools work regardless. The service's
own copy stays in `/etc/assetcracker/env`, readable by root and the service only; the deploy user
is not given that file, because exchange keys will live in it one day.

`AC_SERVICE_URL` moves the target off the loopback default; nothing sets it on the Pi.

### Checking it

`cd service && go test ./internal/proposals/` runs a client against the server over an in-memory
transport (the SDK validates every answer against the schema it derived, which is how a
`required` field that should not have been was found), the dry run's labelling, and the HTTP
side against a stand-in service: the key header, the `via` field, the 401 and 409 texts.

### Exercising a shape on the tape

Added 2026-09-23, at Brad's word ("make it so the mcp surface can exercise all of these with the
engine when making a strategy"). `strategy_build` labels a shape; `strategy_exercise` runs it.
Code: `service/internal/exercise`. It is the step between building and registering: a shape that
has been exercised is still registered by `strategy_register` and judged live at the corrected
threshold, and nothing here changes that.

**What it does.** For every settled market of the shape's family closing in the window, it reads
the recorded snapshots (the two bid ladders, the spot price and the model's journaled view,
`model.v3`) in a READ ONLY transaction, rebuilds `engine.View` from each, and drives the very code
the runner drives: `engine.Decide` on each second's book, `broker.Paper` filling only what the
recorded depth displayed, `engine.Apply` folding the fills, `engine.ApplySettlement` at each
market's recorded result. One account, seeded at $1,000 by convention (`seed_cents` otherwise),
alone in its paper world. Rows of coins the model never priced (no `v3` key: SOL, XRP, DOGE at
this writing) are counted (`snapshots_unpriced`) and not read, since nothing can be entered on them.

**Input.** The shape, exactly as `strategy_build` takes it (`lambda_late` and `lambda_late_tau`
included), plus `from` (required: markets CLOSING from this time), `to` (default now; only
settled markets are replayed), `step_s` and `seed_cents`. A roster is the same shape with
`members` (2 to 8, each a member shape; v1 all hold, same family): one version that picks among
them. `structural_only` is roster order among whoever would enter. Otherwise the pick is
structural first, then recent shadow return per dollar over `lookback_windows` (default 16)
among the eligible; sit out if nobody would enter. Shadows are unit contracts on a fixed $1,000
seed, settled only after that market's close, and a settlement after now is invisible. Times take the read surface's forms.
Caps: 24 hours of 15-minute rounds or 7 days of ladders per call (a longer run is several calls);
`step_s` is 1 for the rounds by default, every recorded second as the live engine looks, and 60
for the ladders (at least 30: a leg is open for days). **A coarser step is not free**: measured
on 2026-09-23, thinning to one snapshot in five cost Mid-round Favourite 21 of its 76 bets, because
its band (0.70 to 0.90) is one the ask flickers across within a second; Value lost 5 of 240.

**Answer.** `simulated: true`, the built version's name, family and params; the window and its
counts (markets, windows, snapshots, and those without depth, without a view, without `vol_ratio`,
unpriced); a leaderboard-style row (bets, sells, windows with a bet, P&L, staked, fees, return per
dollar, mean window P&L with its plain SE and t, top-window share, drawdown, final cash, whether
it ran out); P&L cut by the entry's time band (`by_entry_band`, the analysis package's five) and
by its price (`by_entry_price`, the five bands of the 2026-09-23 attribution); every blocked
decision by reason (`blocked`, the engine's own texts); orders asked against filled for buys and
sales; the cumulative P&L by window (`series`, at most 300 points); and every order that filled
(`entries`, at most 400: ticker, second, tau, side, why, requested, filled, price, cash, Kelly,
and which limit set the stake). A roster also answers `by_member` (P&L cut by who fired) and
`picks` (sit_out / warmup / adaptive / roster counts). Then `recorded`, `record_id` and, when it was not, `record_error`.

**Every run is recorded.** The tool posts the shape, the window, the step, the seed, its release
and the summary to `POST /api/controls/exercise/record` on the running service, with the operator
key as `strategy_register` does, and the service writes one append-only `analysis_result` row:
key `strategy.exercise`, `params` the shape and window (what makes it reproducible), `result` the
summary. `analysis_results` with `key: strategy.exercise` lists them. The MCP process has no write
grant, so this is the only mark a run can leave, and it exists so that the shapes tried are
counted beside the shapes registered. A run the service could not record (no key in place, the
route not yet released) is still answered, with `recorded: false` and the reason: the answer
stands, the count does not. This departs from the plan's rule that every value tried be a
registered draft (`docs/honest-fills-v3.md`, section 8, with a dated note); the record does not
move the leaderboard's `trials`.

**The protocol guard.** A 15-minute-round window that reaches past TRAIN's 480th eligible window
(`docs/v3-measurement-protocol.md`, section 1) is refused, whole, until
`research/v3/test-result.json` is committed: a lambda read off TEST windows' P&L would be a lambda
chosen on TEST. TRAIN's end is found with the protocol's own existence query, which returns market
ids and no figure and which the protocol says is not a look. The Pi's MCP process cannot see the
repository, so the owner lifts the refusal by setting `AC_EXERCISE_PAST_TRAIN` in that process's
environment once the look is taken. While TRAIN is short of 480 windows nothing settled is past it
and nothing is refused. The ladders are under no protocol and are not guarded.

**What it is not.** A measurement. The window and the shape were chosen, and a shape read on the
window it was tuned on has been tuned on it; the answer says so in `note`, and the tool computes
no `L_hat` and no model-against-mid Brier. Nor is it a copy of a live bucket. Checked on
2026-09-23 against Value (conventions) and Mid-round Favourite (conventions) over their own live
windows at `step_s` 1: bet counts within one (240 against 241; 76 against 75), windows equal (121;
55), entry seconds the same almost everywhere, the worst window to the cent for Value; stakes a
contract or two apart and P&L about $25 apart on $1,400 staked. The three reasons are the live
path's and not the tape's: `Runner3.Step` gives up a coin's second when another coin's write holds
its lock, so inside one second the two coins can be decided in the other order and the window's
Kelly stake go to the other coin; the sustainment allocation lowers a live bucket's cash after
each new high, and Kelly sizes off cash; and a live settlement lands a few seconds after the close,
where the replay applies it at the first snapshot at or after it.

**Cost.** A 20-hour window of the two priced coins at every second is about 150,000 snapshots,
read in 7 s over the SSH tunnel from the Mac and replayed in 1.5 s; the statement timeout is 25 s
and the call's budget 45 s.

**Checking it.** `cd service && go test ./internal/exercise/`: the replay on a synthetic tape (the
money adds up, the cuts and blocked counts, a tape without depth or a view, an unordered tape
refused, the late lambda entering only inside its window, a roster of two disjoint members naming
who fired), the protocol walk on rows (the 480th
eligible window, ineligible windows, a coin covered later, the fifteen-minute wait), the guard's
refusal and the owner's word, and the tool over the in-memory transport against a stub tape and a
stub door (the schema, the window forms, the caps, `members`, the record's body, an unrecorded run).

## What is not here

- Deploying, retirement, the orders switch (the buckets page), the rates, the paydays, moving
  money, the reset (the home page's bank): clicks, a person's. Raw SQL, writes to any table (the
  exercise record is written by the service, not by this process).
- The ledger, journal, metrics, commentary (TSK-42's first half). Same server, later.
- Fifteen-minute candles: not stored. `bars` builds any bucket from the trade prints, but only
  from the day recording started.
- Prod access from a laptop (see Connecting).
- Data the tables do not hold: funding rates, open interest, news, Coinbase's order book. The
  surface can only show what the service records.

## Checking it

`cd service && go test ./internal/readsurface/` runs the unit tests: the time forms, the caps,
the paging, and a registration of every tool (the SDK infers each tool's schema from its Go
types at registration, so a type it cannot describe fails there, not on the Pi). Against a
database: build with `deploy/pi/deploy.sh` (dev), then any MCP client over the ssh command above.
On 2026-09-21 every tool was called that way against dev; the three-year pooled hourly grid,
the heaviest call, took 8 s.
