# The read surface: `assetcracker mcp`

How an agent, or a script driving one, reads the recorded market data and the analyses computed
from it, without reading whole tables. It is the first piece of "Agent access (MCP)" in
`platform-brief.md` section 9: market data and analysis results, read-only. The ledger, the
journal, the metrics and the strategy registry (TSK-42) are not here yet; they will join under the
same server.

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

- **Read-only by the database, not by convention.** Every call runs in a `READ ONLY` transaction:
  Postgres itself refuses any insert, update, delete or DDL inside one, whatever the role. In the
  real database the transaction also takes the `assetcracker_ro` role (`db/grants.sql` says what
  it may see); in dev, where that role has no grants, it runs as the connected user, still read-only.
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
command against the **dev** database:

```json
{
  "mcpServers": {
    "assetcracker-dev": {
      "command": "ssh",
      "args": ["-o", "BatchMode=yes", "acdeploy@rpi-v5-1.local",
               "env", "AC_DATABASE_URL=postgres:///assetcracker_dev?host=/var/run/postgresql",
               "/opt/assetcracker/dev/assetcracker", "mcp"]
    }
  }
}
```

That binary is what `deploy/pi/deploy.sh` (dev mode) copies; it must be a build that has the
subcommand (2026-09-21 or later). Any MCP client works the same way: run that command, speak the
protocol on its stdin/stdout. Logging goes to stderr, which ssh carries separately.

**The real database** is not reachable this way yet: `acdeploy` cannot connect to it (peer auth
maps the OS user to a role of the same name), and `assetcracker_ro` has no login. Two ways to open
it, both admin steps for Brad: a `pg_ident.conf` map letting `acdeploy` connect as `assetcracker_ro`
over the socket, or mounting this same server on the running service's localhost HTTP mux
(`mcp.StreamableHTTPHandler`, reached through the `tools/view.sh` tunnel). Dev and prod record the
same public candles, so for the history nothing is lost meanwhile.

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

## What is not here

- Writes of any kind, proposals, raw SQL. Not planned for this surface; proposals are TSK-42's
  second half and go through a person.
- The ledger, journal, metrics, commentary, strategy versions (TSK-42's first half). Same server,
  later.
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
