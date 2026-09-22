# Platform brief: what Asset Cracker is becoming

Status: **draft, 2026-09-20.** Captures decisions Brad made in one working session, so nobody
has to re-derive them. Doipster has not reviewed it. It supersedes `flutter-port-scope.md`,
which described a narrower goal (the same widget, cross-platform).

Lines marked **Decided** are Brad's. Lines marked **Proposed** are the agent's recommendation
and are open to change. Lines marked **Open** need an answer.

## 1. What we are building

A service that runs trading strategies against live market data, first with simulated money and
eventually with real money, and keeps a complete record of what it did and why. Strategies live
in capital **buckets**. A supervisor watches them, shuts down the ones that fail, and recycles
capital to new ones. People and agents study the record to find better strategies. A
cross-platform app shows performance, the trade history, and the controls.

The existing widget is the starting point, not the shape. It has one venue, two coins, six
hard-coded strategies, and the engine inside the window.

## 2. Principles

1. **Sim first, same path.** Simulated and real orders go through the same code, with a paper
   broker or a live broker at the end. Real trading is switched on per bucket, late, behind
   explicit gates (section 11).
2. **Assume nothing about edge.** The existing result (six strategies lost over one month, at
   one-minute resolution, final minute untested) is inconclusive. The system exists to produce
   conclusive results, in either direction.
3. **Everything is journaled.** Every evaluation, not only every trade: inputs, what the model
   said, what the market said, which rule fired or blocked, which strategy version.
4. **Statistical bars, not balances.** A strategy earns capital by clearing a gate built from
   sample size, after-fee return with a confidence bound, and correction for how many things
   were tried (section 7).
5. **People are never locked out; limits always bind.** Anyone can comment, weight, pause or
   override at any time. Hard limits (breakers, caps) still apply unless someone changes the
   limit itself, and that change is recorded.
6. **Agents propose, people approve.** An agent can read everything and draft strategies. It
   cannot move capital, change limits, or promote a strategy on its own.

## 3. Stack

| Part | Choice | Status |
|---|---|---|
| Service | **Go**, one binary | Decided |
| Database | **PostgreSQL** | Decided |
| Host | Brad's Raspberry Pi 5, 16 GB, 1 TB NVMe (measured, section 12). Postgres lives only here, never on a laptop. | Decided |
| Client | **Flutter/Dart**: desktop and mobile, a client only | Decided |
| Client API | HTTP + JSON, with a WebSocket for live updates; described by an OpenAPI file the Dart client is generated from | Proposed |
| Agent access | MCP server inside the Go service, read-only at first | Proposed |
| Heavy analysis | SQL and Python against Postgres, outside the service. Doipster's `research/` fits here. | Proposed |

Why Go: a single static binary cross-compiled for the Pi, low memory, strong types, good at
long-running concurrent network work, solid Postgres libraries. Its weak spot is statistics.
The service computes only the gate metrics itself (means, bootstrap intervals, drawdown, Brier
score: all simple); exploratory analysis happens in SQL and Python where the tools are.

Consequences to accept:

- The Dart engine written earlier on 2026-09-20 (`engine/` on branch `flutter-scope`) is
  retired as an engine. It justified itself by running on the phone; with a service, the phone
  is a viewer. Its number-matching notes stay useful for the Go port.
- Doipster's strategies get ported to Go as the first strategy family. The record-and-replay
  harness (`tools/parity/`) is language-neutral and becomes the check for that port, then the
  regression suite.
- Doipster writes Python. Go puts the engine out of his day-to-day reach unless he picks it
  up. His path in is research against Postgres and strategy design. **Decided (Brad):** he
  will adapt.
- **Decided (Brad): the two run in parallel, in this repo.** The Python widget and `research/`
  stay where they are and keep working; nothing in the platform work edits or removes them.
  The platform lives in its own top-level folders (`service/`, `app/`, `db/`).

## 4. Concepts

- **Source**: somewhere data or execution comes from (Kalshi, Coinbase, more later). Market
  data and order execution are separate capabilities; a source may offer one or both.
- **Instrument**: a thing that can be traded, with its own payoff, fee and position rules. A
  15-minute yes/no contract and a spot coin are different instrument types.
- **Strategy**: code plus parameters, **versioned**. Results always attach to a version. A
  retired version cannot return without an explicit override.
- **Venue account**: the one actual account at a venue (or its simulated stand-in). Sim or
  real, never mixed.
- **Bucket**: **one virtual subdivision of one venue account** (decided by Brad, 2026-09-20),
  with its own cash in the ledger, its own risk limits, and one strategy version. The venue
  sees only the whole account; the split exists in our ledger. The unit the supervisor acts on.
- **Common pool**: receives tax from successful buckets and the remains of closed ones; seeds
  new buckets.
- **Profit pool**: what is kept. Can fund expansion (more buckets, higher limits) by proposal
  and approval.
- **Ledger**: append-only, double-entry, integer cents. Every movement (fill, fee, settlement,
  tax, reap, seed) is a transfer with a reason. The source for tax reporting.
- **Decision journal**: one row per evaluation. The source for learning.
- **Commentary** and **human weight**: section 8.
- **Bench**: strategy versions that have passed paper probation and wait for a slot.
- **Trials registry**: every strategy version ever tried, by person or agent. Without the
  count, no significance figure means anything.
- **Broker**: paper or live, behind one interface.
- **Supervisor**: watches buckets, trips breakers, runs the lifecycle.

## 5. Bucket lifecycle

**Decided.** A failing bucket is closed, never topped up.

1. Seeded from the common pool with a strategy version from the bench.
2. Runs. A sustainment allocation is taken from its gains at a configured rate (section 6).
3. A **circuit breaker** trips: drawdown from peak, absolute floor, run of losses, or sim fills
   drifting from real fills. Thresholds are configuration.
4. De-activated: no new orders. Open positions follow the instrument's rule (proposed default:
   let expiring contracts settle; sell what has no expiry).
5. Remaining funds are **reaped** into the common pool as a ledger transfer.
6. The bucket is **frozen, not deleted**. Its ledger and journal are tax records and the
   material failure patterns are learned from.
7. A new bucket with a **different** strategy takes the slot. If the bench is empty the slot
   stays empty: nothing untested gets capital because a slot opened.

Known wrinkle: Kalshi nets positions per market on one real account. Two buckets on opposite
sides of the same market cancel at the venue while their virtual books do not. Needs internal
netting or a conflict rule before real trading. **Open.**

## 6. Money flows

**Naming (Brad, 2026-09-21):** the platform's own take from a bucket's gains is the **sustainment
allocation**. "Tax" means only government tax, and the tax reserve is the bucket that holds money
for it. The two were both being called tax; the ledger now says `sustainment` and `allocated`.

**Built 2026-09-21** for simulated money, on the version 2 buckets (migration 0008,
`service/internal/runner/runner2.go`, `tools/skim-policy.sh`). Brad's names for the buckets:

| Bucket | What it is | In the ledger |
|---|---|---|
| Capital in deployment | what the strategies trade with | accounts of kind `bucket` |
| Winnings | a skim off the top, kept | `profit_pool` |
| Replenishment | restarts buckets that died; receives what dead ones had left | `common_pool` |
| Tax reserve | a share of gains set aside, so tax is never a surprise | `tax_reserve` |
| Fee reserve | for fees that may come later: withdrawals, venue charges. Kalshi's per-trade fee is already paid on every fill | `fee_reserve` |

The sustainment allocation is taken at settlement, from a bucket's gain above its own **high-water mark** (book
value: cash plus still-live bets at cost), so a bucket climbing back from a loss is not charged
twice on the same dollars. A bucket is skimmed only once every round that has closed is settled
for it: the five coins settle seconds apart, and on dev a bucket skimmed after the first coin
went on to lose the others. What is taken really leaves the strategy's balance. The split is a
dated row in `skim_policy`, not code; each skim points at the policy that set it, and every new
high is recorded in `bucket_skim` whether or not anything was taken. **Every rate starts at
zero**: a tax rate is a fact about its owner, not something to guess. A restart is funded from
replenishment first; only the shortfall is brought in from outside, as its own recorded deposit.

Still to decide: whether the twins are skimmed like the originals (today they are, so a pair
stays comparable); the rates themselves.

The earlier sketch of these flows, kept for the parts not built yet:


Sustainment allocation (bucket to the money buckets; first sketched here as a "success tax"), reap (closed bucket to common pool), seed (common pool to
new bucket), take (common pool to profit pool), expansion (profit pool funds more buckets or
higher limits, by approval). Rates and thresholds are configuration and are Brad's and
Doipster's to set. Every flow is a ledger transfer, so any pool's balance can be rebuilt from
history.

**Open:** if two people's money is in it, who owns what, and how tax attribution works, must
be settled before real funds. That is a question for them and an accountant, not for code.

## 7. Statistics and the promotion gate

- **Calibration**: Brier score and log loss on every prediction; skill relative to the
  market's own price; reliability curve.
- **Edge**: return per dollar staked after fees and slippage, with a confidence interval
  bootstrapped by time window (bets in one round, and BTC and ETH in the same minutes, are
  correlated). P&L split into edge, fees and slippage.
- **Selection**: thresholds corrected for the number of trials; minimum sample from a power
  calculation (illustration, assuming independent even-money bets: a 2% edge at 95% confidence
  needs about 2,400 bets); walk-forward only, parameters frozen per version.
- **Risk**: maximum drawdown, time under water, worst window, risk of ruin at the sizing used.
- **Execution**: sim fill against real fill, fill rate, and their drift.
- **Regimes**: every metric sliced by time to close, volatility, price band and late-volume
  share. Doipster's settlement-minute finding is the first hypothesis to test.

The gate is configuration, not code: minimum N, lower confidence bound on after-fee return
above zero after correction, calibration no worse than the market, drawdown inside the limit
through probation.

## 8. Commentary and human weight

Brad's want, as the agent understands it (**confirm**): people can record their thinking and
lean on selections, without the automation standing in the way.

- **Commentary** attaches to anything: a trade, a decision, a strategy version, a bucket, a
  time span. Author and timestamp recorded. Agents read it over MCP; it is part of the
  learning material.
- **Human weight** is a multiplier a person sets on a strategy, bucket or instrument (0 mutes,
  1 is neutral, above 1 leans in, up to a cap). It is applied on top of the strategy's own
  sizing, never instead of it.
- **Both versions are scored.** The journal keeps what the strategy would have done alone and
  what happened with the weight applied, so the record shows whether discretion helped.
  Without this, human input would quietly contaminate every statistic in section 7.
- **No barrier, with a floor.** Weights and overrides take effect immediately and need no
  approval. They cannot exceed hard limits; changing a limit is a separate, recorded act.

## 9. Agent access (MCP)

Read-only first: ledger, journal, metrics, commentary, strategy versions, trials registry. Then
proposal tools: draft a strategy version, propose a bucket, propose a threshold change. Each
proposal waits for a person. No tool moves capital or places orders.

## 10. The app (Flutter)

Performance by bucket, strategy version and pool; the full trade history with export for tax;
strategy management (versions, parameters, bench, retire); supervisor view (breakers, frozen
buckets, the lifecycle log); commentary and weights; live market views carried over from the
widget. Phone and desktop are the same client. Reaching the Pi from outside the house goes
through a private VPN such as WireGuard or Tailscale. The service is never exposed to the
internet.

## 11. Real trading

Last, per bucket, behind gates: the strategy version passed the promotion gate on sim; sim and
live fills have been compared on tiny size; hard caps per order, per bucket and per day;
a kill switch that needs no app. Exchange keys live only on the Pi.

The agent builds this code. It does not place trades, enter or handle API keys, or advise on
what to trade or how much to allocate. Tax treatment is for an accountant; the ledger's job is
to be complete enough for one.

## 12. Running on the Pi

Measured over SSH on 2026-09-20 (`rpi-v5-1`, 10.0.0.172 by DHCP, also answers as
`rpi-v5-1.local`): Raspberry Pi 5 Model B rev 1.1, 4 cores, 16 GB RAM, 1 TB NVMe (Crucial P3
Plus) with 862 GB free, Debian 12 bookworm 64-bit, kernel 6.12. Clock synchronized by
systemd-timesyncd. 37 C idle, no throttling. Nothing installed beyond the desktop: no
Postgres, Go or Docker; only SSH listens on the network. Debian's Postgres is 15;
`deploy/pi/setup.sh` installs 17 from apt.postgresql.org instead.

Set up on 2026-09-20: Brad reserved 10.0.0.172 on his router. `create-deploy-user.sh` made
`acdeploy`, a key-only account whose passwordless sudo is an allowlist (verified: off-list
commands, including `sudo bash`, are refused). `setup.sh`, run as `acdeploy`, installed
PostgreSQL 17.11 and created `assetcracker` (owner role `assetcracker`, for the service) and
`assetcracker_dev` (owner role `acdeploy`). Verified: Postgres listens on 127.0.0.1 and ::1
only, the memory settings and UTC took effect, and `acdeploy` cannot connect to the real
database. Both databases use the `en_US.UTF-8` locale (Brad generated it on the Pi; the
empty databases were recreated with it). Reach the dev database from the Mac with
`ssh -L 5433:/var/run/postgresql/.s.PGSQL.5432 acdeploy@rpi-v5-1.local`.

Data checksums are on (Brad enabled them with `pg_checksums` while the databases were empty;
the deploy user is deliberately not allowed to).

Not yet done: a backup target; power-loss behaviour.

The service needs outbound internet to the exchanges. Postgres backups go to a second machine:
these are tax records. Tick data volume, measured today: about 57 KB a minute of raw input for
two coins, so roughly 80 MB a day; dozens of instruments means gigabytes a month. Partition the
tick tables by time and set a retention policy.

## 13. Build order (sim only until the last step)

1. Postgres schema: ledger, accounts, buckets, pools, instruments, strategy versions, trials,
   journal, commentary. **Schema done 2026-09-20** (`db/`): applied to `assetcracker_dev` on
   the Pi, with tests that attempt each rule violation. **Service skeleton done** (`service/`):
   runs on the Pi by hand against the dev database. Not yet: a systemd unit, and the real
   database's roles (the service should not own the tables).
2. Sources: Coinbase and Kalshi market data in Go, recorded to Postgres. Fix the duplicate
   settlement delivery found in the Python poller. **Done 2026-09-20**, with a test that replays
   the duplicate-settlement scenario. Measured on the Pi: quotes at 1 Hz per series, trade
   prints about 47 ms behind the exchange's timestamp.
3. Strategy interface; port Doipster's six as the first family; check with the replay harness.
   **Done 2026-09-20** (`service/internal/kalshi15m`, `cmd/replay`): parity gate passes on the
   Mac and on the Pi; the six are in `strategy_version` as the first trials.
   **Version 2 ported 2026-09-21** (`service/internal/kalshi15m2`, `cmd/replay2`): shared balances
   across five coins, the anti-world twins, running out handled as freeze-reap-replace in the
   ledger. Twelve more rows in the trials registry.
4. Paper broker, journal, bucket lifecycle, supervisor and breakers. **Partly done 2026-09-20**
   (`service/internal/runner`): the six strategies run live in sim in twelve buckets; every
   decision is journaled with the rule that blocked it; fills, fees and payouts are ledger
   transfers; state survives a restart and is checked against the ledger. Not yet: human
   weights applied, breakers, reaping and replacement, a broker interface a live
   broker could share.
5. Metrics and the promotion gate; trials registry. **Started 2026-09-21**
   (`service/internal/analysis`, `GET api/analysis`): the model scored against the market's own
   mid price (Brier, by time to close and by coin), every early sale re-priced to the size that
   was really displayed at the bid, and a leaderboard per strategy version across all its lives.
   The unit of inference is the 15-minute window, not the bet. A verdict needs 30 windows and a
   |t| corrected for the number of versions tried (Bonferroni over the measured row count of
   `strategy_version`); both are stated conventions, carried in the JSON. A round is only
   counted once its payouts reconcile with what each bucket held.
   **Gate built 2026-09-21** (`analysis/gate.go`, `docs/api-home.md` "The promotion gate"): log
   loss beside Brier on every scorecard row; per version, return per dollar staked with a
   bootstrap standard error from resampling windows (1,000 draws, fixed seed), a lower bound at
   the leaderboard's own corrected threshold, drawdown walked window by window, and a sample
   floor from a power calculation sized to `min_edge_per_dollar` at `power`. The gate is four
   checks (windows, edge, drawdown, calibration), its settings from `AC_GATE_*`, the rest
   convention or measured; every decision on a complete document is stored in
   `metric_snapshot` with the configuration it was made under. Run against the Pi's dev database
   on 2026-09-21 (131 settled markets, 29 windows, 20 trials): every version's gate evaluated,
   none passed, on too few windows and no positive lower bound. Not yet: a bench that the gate
   feeds, and calibration for any model but the second engine's.
6. API and the Flutter client. **Started 2026-09-21**: the home page's read-only API
   (`docs/api-home.md`: `api/home`, `api/asset`, `api/buckets`, `api/analysis`) and the
   once-a-minute `value_snapshot` history behind "earned over a range" (migration 0010). The
   home page at `/` is the balance sheet: total value, earned over the chosen time scale, a
   ruled asset rail with the chart and stake box to its right (at phone width too), and
   separate Buckets and Evidence panes; the phone widget moved to `/widget`. Run against the
   Pi's dev database through two settlements on 2026-09-21: every statement executed, the
   earned arithmetic matched the snapshot rows, the ledger summed to zero. Released to prod as
   78f5452 on 2026-09-21 at 08:20 PT, with migrations 0010 and 0011. Not yet: the Flutter
   client, anything that writes.
7. MCP, read-only; then proposals.
8. More sources and instrument types.
9. Live broker behind the gates in section 11.

## 14. Open questions

1. ~~Is Doipster on board?~~ Decided by Brad: he will adapt. He still has not seen this.
2. ~~Which repo?~~ Decided by Brad: this one, in parallel with the Python.
3. Section 8: is that what was meant by commentary and weight?
4. Which sources and instrument types come after Kalshi and Coinbase?
5. Position netting across buckets on one real account.
6. Ownership and tax attribution if funds are shared.
7. ~~The Pi's specification.~~ Measured, section 12: more than enough.
