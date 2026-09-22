# v3 measurement protocol (pre-registered)

**Second amendment, 2026-09-22 UTC (evening of 2026-09-21 PT), at the owner's instruction.** Release `000fa9d`
(2026-09-22 04:17 UTC) stopped the v1 and v2 engines; the last v2 `decision` row is at 04:17:01 UTC **[PROD]**. The
first text measured lambda on v2's decisions and M2/M2s on v2's orders, so its TRAIN could never close. Since
04:17:04 UTC every BTC and ETH snapshot carries the third engine's forecast, a fork of v2's model **[PROD; READ:
engine/fork_test.go `TestForkMatchesV2`]**. So the source changes to it, and with it `t0`, the coins and M2/M2s
(3.2 to 3.4); the three choices, thresholds, block rule, looks, trials and outcomes do not. It is made before the
first text's TRAIN could close, so it costs no TEST window (section 0), and before any statistic of section 3 or any
Brier figure was computed on the forecast rows **[as stated by the owner's session; `cmd/measure3` and `research/v3/`
did not exist, so no tool record could show otherwise]**. Its commit is the new `T_c`.

## What the owner is agreeing to

Your yes to this text, given before it is committed, settles three choices. Changing one later means a new protocol
with a later start, and the rounds used so far could no longer confirm anything.
1. How far to trust the model: the cautious end of what the data supports, not the best guess.
2. How pretend orders fill: at the price shown, with a measured charge for the price moving a second later.
3. If this protocol says no: no new strategy is added. There is no fallback strategy.

Two facts. "Add nothing" is an accepted result, not a failure. And the only data that can confirm anything is
15-minute rounds that close AFTER this file's commit; every earlier round can only be used to set the numbers.
A mechanical slip (a wrong column name, a command that cannot run) is corrected in a separate errata file without
restarting the clock, under the narrow rule of 0.1; anything else is a new protocol.

The data is the forecast the third engine records every second in each market snapshot, from 2026-09-22 04:17:04 UTC,
for the coins that carry it: BTC and ETH at this amendment, not the five of the first text. A round of two coins is a
smaller sample than a round of five, and BTC and ETH move together, so the test is weaker than first planned.

Step S0 of `docs/honest-fills-v3.md` (6.1 to 6.5, 7.1; decisions D1, D2, D3, D13, D15), committed BEFORE any
measurement is run. Simulated money only. Every query below is unexecuted, and no statistic here measures prod.

Labels: **[READ]** read in the code, migrations or row samples on 2026-09-21 (for this amendment, in the code at
`000fa9d`). **[PROD]** a time or a key's presence observed on prod on 2026-09-22 UTC by the owner's session, before
this amendment; never a statistic of section 3. **[INFERRED]** concluded from what was read. **[NOT VERIFIED]** could
not be checked from here. **[CONVENTION]** a chosen number, not a measurement.

## 0. The three choices this commit fixes, and what stays open
D2, D15 and D3 are fixed by `T_c`'s commit (section 1): the owner's yes to this text, which D1 requires
before the S0 commit, is his choice on all three (a preferred alternative goes in BEFORE that commit). Any later
change to them, like any other change to this file, is a new protocol with a new `T_c`. Before TRAIN's last close that
costs no TEST window, as TEST starts only after the embargo AND `T_c`; after that, section 8 ("No way out") applies.

| # | Choice | Fixed as | The alternative, not chosen |
|---|---|---|---|
| D2 | Which lambda is frozen | The lower one-sided 95% bound (4.1). | The clipped point estimate, `clamp(floor(100 L_hat)/100, 0, 1)`. Trades more and sooner; overbets if `L_hat` is high by chance. |
| D15 | Fill model the versions run under | `paper-1`: fill at the displayed price; M2 and M2s are charged in the entry test and both exit tests. | `paper-2`: fill only what two consecutive snapshots both show. M2 and M2s are still measured and recorded, as the same proxy. |
| D3 | What happens on "no" (R1 or R2 fails) | Register nothing (section 8). | Register ONE "Scalper v3 (fills only)" at the parent's unmeasured belief 0.5: one trial, 18 -> 19, `CorrectedT` 3.007, its two per-fill lines judged at `CorrectedT(19 + 2)`. |

Until that commit exists the owner has approved nothing, and the table is the plan author's proposal. His yes covers
D2, D15, D3 and D1: this protocol as a whole, including that "register nothing" is an accepted outcome, the 480/24/192
split, the t thresholds, the block-doubling rule and its loss of power, the third engine's recorded forecast as the
data and the coins rule with its smaller sample (section 1), that M2 and M2s are proxies measured on the recorded book,
and the accepted leak of section 6 with its price (`measure3 test` is run whatever the live page shows).
- NOT settled by that commit, and still the owner's: D13, who runs `measure3` against prod and as which role. Assumed:
  the owner, as `assetcracker_ro`, read-only over the documented ssh tunnel. **[NOT VERIFIED]** that this role can
  `select` from the tables in section 2; if it cannot, the grant is the owner's to make and nothing is run until then.
- Likewise open: D9 (fee rounding per order or per fill) and D10 (quarter Kelly, the window cap, the seed). They set
  no number in this protocol and are listed only because they enter `frozen-params.json` beside the measured values.

### 0.1 Errata: mechanical corrections that are not a new protocol
Added by the first amendment to this file, made on 2026-09-21 with the owner's yes, before any `measure3` code existed,
before any measurement was run and before TRAIN's last close (so it cost no TEST window); `T_c` was that amendment's
commit until the second (top of this file). No query here has ever been executed, so a slip is likely, and the
owner judged "any correction restarts the clock" too limiting. The rule is narrow on purpose. Where "may only" and
"may never" conflict, NEVER wins.
- **What is protocol.** Every predicate, join condition, bin expression, sort key and tie-break in a printed query is
  protocol whether or not the prose restates it, and so is every name written inside a prose sentence. The prose
  governs the printed queries' NAMES and SYNTAX only.
- **An erratum may only** correct a name or syntax in a printed query or command so that it runs and does what the
  text already says, or replace an instruction that cannot be executed as written with one that can. It is justified
  from `db/migrations` and source text ONLY, never from query output, row counts or samples. If two executable
  replacements could return different rows or values, it is not an erratum.
- **An erratum may never** add, remove or loosen a predicate, join, bin, sort key or tie-break; nor touch a number, a
  population, an estimator, a threshold, degrees of freedom, a split size, the embargo, the block rule, the three
  choices, what counts as a look, the trials table or the outcomes; nor settle a point on which this file is ambiguous
  or wrong about which rows are read or how a statistic is computed. Each of those is a new protocol with a new `T_c`.
  After TRAIN's last close a point that is not an erratum is section 8's abandonment: there is no third route.
- **The tool is covered too.** The queries this file gives only in words are written in `cmd/measure3`. Any change to
  `cmd/measure3` after its first run against prod that alters a query, a filter or an arithmetic step is an erratum and
  is entered as one, naming the commit; each result file records the tool's git sha at its first prod run and at this run.
- **Once a figure has been seen.** `measure3` appends every run, complete or not, to the committed
  `research/v3/runs.jsonl`. Once any run has printed a section-3 figure, an erratum may change only an instruction
  that FAILED with an error, and the entry quotes the error text. After `frozen-params.json` exists, `measure3 train`
  rerun under the errata then in force must reproduce its digits and row counts exactly, or `measure3 test` refuses.
  After `test-attempt.json` exists, an erratum may change only the instruction whose error that attempt list quotes.
- **Where, and what is checked.** Errata are appended to `docs/v3-measurement-protocol-errata.md`, never written into
  this file, so `T_c` does not move. An entry gives the text replaced and its replacement, the text that governs, why
  no decision can change, and the owner's yes; its date is its commit's committer time. `measure3` embeds the sha-256
  of this file AND of the errata file in every result file, in `frozen-params.json` and in each `test-attempt.json`
  attempt; lists the entries in force and those added since train; and refuses to run if the errata file has
  uncommitted changes, if any commit to it deletes a line other than "No errata yet.", or if
  `git branch -r --contains <its last commit>` prints nothing (a second party, the remote, must hold it before the run).
- **What this costs [said plainly].** Whether a fix is "names and syntax only" is still a judgement, and its one
  approver, the owner, may by then have seen figures. The guards are the checks above and that every entry is dated by
  git and held by the remote before the run; the evidence-page write-up (section 8) lists every erratum.

## 1. Times, units and the split
- **`T_c`** is the committer time, in UTC, of the LAST commit that changed this file, with that commit's hash, as
  printed by `git log -1 --format='%H %cI' -- docs/v3-measurement-protocol.md`: that is the second amendment's
  commit, as any later commit to this file is a new protocol (section 0) with its own `T_c`. It is the committing machine's
  clock **[CONVENTION: git's committer time is taken as true]**; pushing the same day, so that a second party holds
  the timestamp, is the owner's action. `measure3 train` refuses to run if that command prints nothing (the file is in
  no commit) or if `git status --porcelain` on the file prints anything (changed since its commit). It writes the hash
  and `T_c` into `frozen-params.json` and `train-result.json`; `measure3 test` takes both from there and never
  recomputes them, so the result files cannot disagree. **Squash and rebase**: the commit reaches main by a merge
  commit, as PRs #1 and #3 did **[READ: `git log --merges`]**. If it is rewritten anyway: before `frozen-params.json`
  exists the later time stands (it burns more, never less); after, the recorded hash and `T_c` stand, since the
  embedded sha-256 shows the text is unchanged, and `test-result.json` also records the hash git prints then.
- **`t0`** = 2026-09-22 04:17:04 UTC, fixed by this text: from then on every BTC and ETH snapshot carries the
  forecast **[PROD]**. Every window closing at or before `t0`, and every row before it, is out of scope; that includes
  the 38 windows behind `docs/reviews/strategy-review-2026-09-21.md`.
- **Coins**: those whose snapshots carry the forecast (`evaluation.model` has the key `v3`, section 2): `KXBTC15M` and
  `KXETH15M` at this amendment, covered in every window closing after `t0`. `KXSOL15M`, `KXXRP15M`, `KXDOGE15M` have
  `"trade": false` and carry no key **[PROD; READ: app/app.go, the `trade` skip]**. A coin may be ADDED only by
  configuration (its instrument's `trade`), never chosen on any figure (section 3, the evidence page, the read
  surface); it is covered in every window closing after `s_c`, its first row with the key, which the tool prints. A
  coin is never removed: a covered coin with no scored row makes the window ineligible, so switching one off stops the
  protocol (after TRAIN's last close, section 8's abandonment). Every figure of 3 and 4.2 is also reported per coin;
  per-coin figures decide nothing.
- **Window**: the `market` rows of the coins covered in it that share one `closes_at` **[READ: 0006, 0007; the five
  symbols above carry `spec.v2 = true`]**. The window's key is `w = extract(epoch from closes_at)`, a multiple of 900.
- **Scored row**: a row the M1 query (3.1) returns, before the restriction to the actionable set A. A market HAS one
  exactly when some `evaluation` row of it meets every condition of that query's WHERE clause, with the time bounds
  `$1`/`$2` replaced by `e.at >= t0`. An **existence query** over that clause tests this. It returns `(closes_at,
  market id)` only, never `p_model`, a quote or `result` as a value, so it is not a look (6). Unexecuted.
- **Eligible window**: closes after `t0`; has a market for each coin covered in it; every one has `result in
  ('yes','no')` and a scored row, by the existence query. Judged at run time, then FROZEN (below). An ineligible window
  is listed with its reason (`missing market`, `no result`, `no scored row: <ticker>`). None is dropped or added by hand.
- **Split, by rule.** 480, 24 and 192 are **[CONVENTION]**, fixed now, never revisited after outcomes are seen. With
  two coins a window has 2/5 of the rows five gave, and less independent ones: power is lost, and that is accepted.
  - TRAIN: the first 480 eligible windows in `closes_at` order.
  - Embargo: every window closing in the 24 x 900 s = 6 hours after TRAIN's last close (48 windows, 12 hours, if the
    block is doubled, 5.3). Counted by the clock, eligible or not. Never read for any statistic.
  - TEST: the first 192 eligible windows closing after BOTH the end of the embargo AND `T_c`.
  - **Expected [INFERRED: estimates if every window is eligible; each one that is not adds 15 minutes]**: TRAIN, the
    closes 2026-09-22 04:30 to 2026-09-27 04:15 UTC; embargo to 10:15 (16:15 if doubled); TEST, the closes 2026-09-27
    10:30 to 2026-09-29 10:15 UTC (16:15 if doubled), complete 15 minutes later.
- **Order of work, and the freeze.** `measure3 train` walks windows in `closes_at` order from `t0` with the existence
  query and stops at the 480th eligible one. Only then does M1 run, with `$1`/`$2` from those windows; it never
  selects a probability, quote or result for any later window. A split is complete when its last close is 15 minutes
  old **[CONVENTION: the poller stops asking for a result after 15 minutes; READ: kalshi/poller.go `GiveUpAfter`]**.
  The first complete run writes TRAIN's 480 window keys, the embargo's end and the windows then ineligible, with
  reasons, into `frozen-params.json`; reruns and `measure3 test` use that list, never re-deriving it. A given-up
  market gets its result only at a later restart **[READ: store/store.go `UnsettledMarkets`]**; a window so made
  eligible stays out, reported as "became eligible after the freeze". TEST's list is frozen likewise at its run (6).
- **Burned**: every window closing at or before `T_c`. Those closing after `t0` may be used for estimation (TRAIN)
  and never for confirmation (TEST).

## 2. What is read, and what the recording can and cannot supply
Tables and columns **[READ: `db/migrations/0001_init.sql`, checked against the row samples]**:
`evaluation (id, at, market_id, quotes, model)`, `market (id, instrument_id, closes_at, result)`, `instrument (id,
symbol)`, and `strategy_version` for the count of section 7 only. Reads only. No write, no DDL. `decision`,
`trade_order` and `bucket`, which the first text read, are not: no engine has written `decision` rows since `t0`.

- **The forecast.** A covered coin's snapshot has `model->'v3'` = `{"v":3, "ok":false}` when there is nothing to
  decide on (no strike, or no trade price younger than 30 s), else `ok` true with `sigma2`, `index_offset`,
  `offset_samples`, `offset_source`, `p_model` and `drift` **[READ: app/sink.go `SaveQuotes`, `price`; engine/coin.go
  `View`, `Journal`]**. `p_model` is the raw model's P(yes), what `decision.model_prob` held for v2, not a blend.
  `TestForkMatchesV2` requires sigma2, the offset and `p_model` to equal v2's exactly at every step of two recorded
  fixtures **[READ]**. With no live v2, `drift` is always false **[READ: `View`, `ref == nil`]**; nothing reads it.
- **One row, one second.** The forecast and the quotes are written together, one row per market per second **[PROD;
  READ: `SaveQuotes`]**, so `p` and `q` come from the same row and second.
- The top-of-book keys `yes_bid`, `yes_ask`, `no_bid`, `no_ask` are in every sampled row **[READ: samples]**. Depth
  fields (since release `87a2100`, before `t0`) are not read: M1, M2, M2s and M3 use top-of-book quotes, the forecast
  and results only.

## 3. The measurements
All prices are in dollars per contract as stored (`"0.4500"`). `y = 1` if `market.result = 'yes'`, else 0.
`p` = the row's `model->'v3'->>'p_model'`; `q` = the same row's yes mid, `(yes_bid + yes_ask) / 2`; `d = p - q`.
Row order is fixed by the queries, so a rerun reproduces digits.

### 3.1 M1: lambda
Rows: snapshots whose forecast has `ok` true, the FIRST row in each 15-second bin per market, two-sided books only.
**Changed from the first text**: the source is the snapshot's forecast, not v2's `decision` rows (so the version ids
are gone), and `q` is computed from the row's quotes by the formula v2 used for `decision.market_prob` **[READ in the
first text: kalshi15m2/account.go:190-194]**. Kept: the bins, two-sided books, the estimator, A, the self-check, the
window as the unit. The bin's reason changes **[CONVENTION]**: the forecast is written every second, and the bin thins
it to one row per 15 s per market, v2's journal's lowest rate, so no stretch of seconds outweighs another. `$1` (the
later of `t0` and the split's first close minus 900 s) and `$2` (its last close) are known from the existence query
before this query runs (section 1, "Order of work"). Unexecuted:

```sql
select distinct on (e.market_id, floor(extract(epoch from (m.closes_at - e.at)) / 15))
       extract(epoch from m.closes_at)::bigint as w, i.symbol as coin, e.market_id, e.at,
       (e.model->'v3'->>'p_model')::float8 as p,
       ((e.quotes->>'yes_bid')::float8 + (e.quotes->>'yes_ask')::float8) / 2 as q,
       (m.result = 'yes')::int as y,
       (e.quotes->>'yes_ask')::float8 as yes_ask, (e.quotes->>'no_ask')::float8 as no_ask,
       (e.quotes->>'yes_bid')::float8 as yes_bid, (e.quotes->>'no_bid')::float8 as no_bid
  from evaluation e
  join market m on m.id = e.market_id
  join instrument i on i.id = m.instrument_id
 where e.at >= $1 and e.at < $2
   and i.symbol in ('KXBTC15M', 'KXETH15M', 'KXSOL15M', 'KXXRP15M', 'KXDOGE15M')
   and m.result in ('yes','no')
   and (e.model->'v3'->>'ok')::boolean and e.model->'v3'->>'p_model' is not null
   and (e.quotes->>'yes_bid')::numeric > 0 and (e.quotes->>'no_bid')::numeric > 0
 order by e.market_id, floor(extract(epoch from (m.closes_at - e.at)) / 15), e.at, e.id;
```

Rows inside those bounds whose window is not in the frozen list, or whose coin is not covered in that window, are
discarded. A row without the key has a null `ok` and is not returned. **Actionable set A**: the favoured side
is yes if `d > 0`, no if `d < 0`; a row with `d = 0` is not in A. With `p_s` the model's probability of the favoured
side (`p` or `1 - p`) and `ask_s` its ask, the row is in A when `p_s - ask_s - 0.07 ask_s (1 - ask_s) > 0`. The 0.07
is the venue's fee rate (a fact of the fee schedule, not a tuned number). No free number enters A.

- Estimator: `L_hat = sum_A d (y - q) / sum_A d^2`. Closed form: nothing is searched, so no hidden trials.
- Self-check before any other figure prints: on the same rows, `lhs = Brier(model) - Brier(mid)` and
  `rhs = V (1 - 2 L_hat)`, with `V = mean_A d^2`, must meet `|lhs - rhs| <= max(1e-12, 1e-9 x max(Brier(model),
  Brier(mid)))` **[CONVENTION: relative to the two numbers subtracted, as rounding is; absolute floor 1e-12]**. Plain
  summation on made-up rows of TRAIN's size left about 1e-14 **[a simulation, not prod]**. On failure the tool prints
  both sides, the residual and the tolerance, nothing else. "Model worse than mid" is exactly `L_hat < 1/2`.
- Reported and never used to choose anything: `L_hat` on all scored rows of TRAIN windows, by coin, by price band, by
  tercile of `|d|`; the count of rows and of windows with no row in A.
- Known weakness, stated in advance: v3 acts at the large-`|d|` end of A, where the model's errors may concentrate.
  Only the live per-fill term on the evidence page watches that.

### 3.2 M2: stale_cost (buying)
**Changed from the first text**: v2's orders no longer exist, so M2 is measured on the recorded book, with no order
and no lambda (unknown during TRAIN). Population: every M1 row (3.1, before the restriction to A) of a TRAIN window.
Its **buy side** is the side a buyer led by the forecast alone would buy: yes if `yes_ask < p`, no if `no_ask < 1 - p`
(no fee). A row with neither is not an observation; both needs a crossed book, and is counted and skipped. The next
snapshot is the first `evaluation` row of that market, by `at` then `id`, with `row.at < at <= row.at + 3 s`
**[CONVENTION: snapshots are a second apart; a later one answers a different question]**. The observation is
`ask(next, buy side) - ask(row, buy side)`, `yes_ask` or `no_ask`. Counted and reported by reason, not imputed: no buy
side; no next snapshot within 3 s (the row's book is two-sided, so its own quotes are never 0). **A quote that has
vanished in the NEXT snapshot is read as the worst price, for buys and sells alike [CONVENTION: the conservative
reading]**: a next ask of 0 counts as 1.00, and in 3.3 a next bid of 0 counts as 0. The recording writes an ask as
`"0.0000"` exactly when the other side has no bid **[READ: kalshi/quotes.go `BookQuotes`]**, so nothing could then be
bought under a dollar. These are counted, and the mean without them is reported beside the frozen one and freezes
nothing. One observation per row **[CONVENTION]**. Point estimate: the mean; SE: section 5. **Frozen value:
`max(0, mean)` rounded to 0.0001.** It is a cost: the point estimate is charged, the SE recorded.

### 3.3 M2s: stale_cost_sell (selling)
The mirror image on the same rows. The **sell side** is the side a holder led by the forecast would sell: yes if
`yes_bid > p`, no if `no_bid > 1 - p`; neither is not an observation, both is counted and skipped. The observation is
`bid(row, sell side) - bid(next, sell side)`, `yes_bid` or `no_bid`. A sell side with no bid in the next snapshot
counts as a bid of 0 (the loss is real). Same next snapshot, exclusions, mean, SE and freezing rule. Under 30
observations **[CONVENTION, the 30 of `analysis.MinWindows`]** it is still frozen, and the provenance note says so.

### 3.4 What M2 and M2s are
**Measured on the TRAIN book, a proxy, and conservative** only in two places: a vanished quote is read as the worst
price, and a negative mean freezes as 0. `Provenance.Note` for both reads "measured on the recorded TRAIN book at rows
where the forecast alone would buy (sell); no order; a proxy for v3's fills". What could make them wrong:
- **Too low.** v3 enters only where `lambda (p - q)` clears spread, fee and this cost, where disagreement is largest
  and adverse selection plausibly worst; the proxy also counts the many rows where `p` barely clears the ask, whose
  next-second move is mostly noise. A real order takes liquidity and may walk below the touch; a quote does not.
- **Either way.** A snapshot's book may be older than its `at` (poll latency, **[NOT VERIFIED]**); if two snapshots
  repeat one cached book the change reads 0. The first row per 15-second bin is not a random entry time.
- **Too high.** The worst-price reading of a vanished quote; and when the quote moves away, an order limited to the
  displayed price would miss rather than pay the move the proxy charges.
Live next-second re-pricing of v3's OWN fills replaces both in any later version, which is a new trial.

### 3.5 M3: outcome correlation across coins
Over TRAIN windows: the Pearson correlation of `y` for each pair of coins covered in them (one pair, BTC and ETH, at
this amendment; ten if all five are covered), and their mean, with the block-jackknife SE. Recorded only. It sets no
parameter; sizing assumes a correlation of 1 **[CONVENTION]**.

## 4. What is frozen, and the decision rules
### 4.1 Frozen by `measure3 train`, into `research/v3/frozen-params.json`
- `lambda = clamp(floor(100 (L_hat - t_{0.95,B-1} x SE)) / 100, 0, 1)`: the lower one-sided 95% bound, rounded DOWN to
  two places. `B` is the number of blocks TRAIN actually has; `SE` is from section 5. 0.95 is **[CONVENTION]**. 480
  windows are 120 hours: about 20 six-hour blocks, 19 degrees of freedom, `t = 1.729`; at 21 blocks (TRAIN starting
  inside a block) `1.725`; if the block is doubled, about 10 blocks and `1.833`. The realised `B` is used and printed.
- `stale_cost`, `stale_cost_sell` by 3.2 and 3.3. The block length (5.3). The split: its bounds, TRAIN's window keys,
  the embargo's end and the ineligible windows (section 1). `T_c` and its commit's hash. Nothing else (R3).

### 4.2 Rules
- **R1 (TRAIN).** `lambda = 0` -> no version is registered (section 8), and TEST is not opened. `lambda > 0` ->
  `measure3 test` WILL be run once TEST is complete, whatever the live page shows (section 8, "No way out").
- **R2 (TEST, once).** With lambda frozen, `blend = q + lambda d`. For each TEST window with at least one row in A,
  `b_w` = the mean over its A rows of `(q - y)^2 - (blend - y)^2`. The statistic is the mean of `b_w` over those
  windows **[CONVENTION: windows weigh equally, because the window is the unit]**; windows with no row in A are
  counted and reported. `t = mean / SE`, SE by the block jackknife. **Pass: `t >= t_{0.95,B-1}`**, one-sided 5%
  **[CONVENTION]**, `B` the blocks TEST actually has and that hold at least one such window. Thresholds: `B = 8` (192
  windows, six-hour blocks, aligned) **1.895**; `B = 9` (TEST starting inside a block) 1.860; `B = 4` (12-hour blocks)
  **2.353**; `B = 5` 2.132; for any other `B` the tool computes `t_{0.95,B-1}` and prints it; `B < 3` is a fail,
  reported as "too few blocks". The normal 1.645 is not used: at 7 degrees of freedom it would pass about 7% of the
  time with no effect. Doubling the block costs real power; that is accepted in advance.
- **R3.** Nothing else is estimated, tuned or compared on the recorded history in this step.
- R2's power is unknown until TRAIN gives the variance; `train-result.json` reports it; a low figure changes nothing.

## 5. Standard errors: the block jackknife
1. A window's block is `floor(w / 21600)` (six hours). An M2 or M2s observation takes its row's window's block.
2. Delete-one-block: with `theta_(-b)` the statistic recomputed without block `b` and `theta_bar` their mean,
   `SE^2 = ((B - 1) / B) x sum_b (theta_(-b) - theta_bar)^2`. For ratio statistics (`L_hat`) the whole ratio is
   recomputed, not the numerator alone. Blocks with no observation for a statistic do not count toward its `B`.
3. **The block length is a [CONVENTION], motivated by the offset memory**: 24 rounds is how long one learned index
   offset stays in v2's model **[READ: kalshi15m2/trader.go]**, which bounds ONE source of dependence between windows,
   not the serial correlation of the statistics (volatility regimes). That is measured: `measure3 train` reports, on
   TRAIN, the autocorrelation of (a) the per-window M1 numerator `sum_A d (y - q)` and (b) the per-window mean over A
   of `(p - y)^2 - (q - y)^2`, at lags 1 to 8 and at lag 24, each with its block-jackknife SE. A lag-`k` pair is two
   TRAIN windows whose `closes_at` differ by exactly `900 k` seconds; windows with no row in A take no part.
   - **The estimator, exactly**: the sample autocorrelation with one global mean **[CONVENTION: not Pearson over the
     pairs]**, `r_k = sum over lag-k pairs of (x_w - xbar)(x_{w+900k} - xbar) / sum over windows of (x_w - xbar)^2`,
     with `xbar` and the denominator over TRAIN's windows that have a row in A. Its delete-one-block value removes
     block `b`'s windows; every pair with EITHER member in `b` goes with them, and `xbar` and the denominator are
     recomputed. `B` counts the blocks holding at least one such window. At lag 24 every pair straddles two adjacent
     six-hour blocks, so this SE is larger than a pair-level one: on a made-up independent series of 480 windows it
     was about 1.3 times the true spread of `r_24`, and the rule below fired about 9% of the time **[a simulation, not
     prod]**. So the rule is stricter than "one SE" suggests: accepted in advance, not to be "corrected" later.
   - **Stated now: if the lag-24 autocorrelation of either series is greater than its own SE** (signed: positive
     dependence is what makes a jackknife SE too small **[CONVENTION]**), **the block is DOUBLED to 12 hours
     (`floor(w / 43200)`) for every SE in this protocol, TRAIN's own included and TEST's, and the embargo doubles to
     48 windows.** Decided by TRAIN alone, by the six-hour figures, before any TEST row is read, once. There is no
     third block length: if the check would fail again at 12 hours, that is reported and 12 hours stands.

## 6. Looks
- **TRAIN figures may be recomputed freely.** They choose nothing but the closed-form values of 4.1; the owner's three
  choices are already fixed (section 0), so no TRAIN figure can steer them. The FIRST complete `measure3 train` run
  freezes the values and the window list (section 1); later reruns use that list, must reproduce the digits and may
  not change `frozen-params.json`. A run before TRAIN is complete prints figures and freezes nothing.
- **One look at TEST.** A look is any computation, by any tool, page, notebook or hand-written query, of `L_hat`, of a
  Brier difference between model, blend and mid, or of any statistic of section 3, on rows of windows that close after
  TRAIN's last window, made before `research/v3/test-result.json` is committed; the one exception is the accepted leak
  below. `measure3 test` refuses to read TEST rows unless `frozen-params.json` exists with this file's sha-256 and
  `lambda > 0`, and refuses if `test-result.json` exists. It is run once.
- **Not before TEST is complete.** The tool first counts the eligible windows closing after both the embargo's end and
  `T_c`, with the existence query: it reads only `market.closes_at`, whether `result` is set, and whether a scored row
  exists. It returns no `p`, `q` or `y` and computes no statistic of section 3, so it is not a look. With fewer than
  192, or with the 192nd close under 15 minutes old (section 1), it prints the count, writes nothing and stops.
  Otherwise it uses the first 192 in `closes_at` order and reads no later window.
- **Written down BEFORE the look.** After that count and before the first query returning `p`, `q` or `y` of a TEST
  window, the tool writes `research/v3/test-attempt.json` and syncs it to disk: the UTC time, the git sha, this file's
  sha-256, the 192 window keys and the windows then ineligible, with reasons. That list is TEST, frozen: a retry uses
  it and never re-derives it. If the file already exists the tool refuses, unless given `-retry-reason "<text>"`,
  which appends the time and reason to its list of attempts; `test-result.json` carries that list. Both are committed.
- **What a crash means.** A run that dies for a mechanical reason (connection lost, tunnel down) before any TEST
  statistic was printed or written may be rerun with `-retry-reason`; that is how the failed attempt comes to be
  recorded. Once any TEST statistic has been seen by anyone, the look is spent, whatever happened to either file. The
  attempt file is a record, not a lock: deleting it is a breach of this protocol that no tool can prevent.
- **A leak that is accepted, said plainly.** The evidence page's scorecard is the window-mean of `Brier(model) -
  Brier(mid)` on every settled round, TEST windows included, built from `decision` rows **[READ: store/analysis.go
  `scored`, analysis/report.go `buildScorecard`]**. By 3.1 that is `V (1 - 2 L)` on all scored rows: a continuous,
  informal view of a statistic strongly correlated with R2's; P&L lines are a noisier view of the same thing. Since
  `t0` no engine writes `decision` rows **[PROD]**, so at this amendment the page shows none of TRAIN or TEST
  **[INFERRED]**; if an engine writes them again, the view returns and is accepted. It is not frozen, because blinding
  the released page is not proportionate. The agents' read surface returns each snapshot's `p_model_v3` with its
  quotes **[READ: readsurface/raw.go]**; reading rows is not a look, computing from them what the look rule names is.
  What is removed is any chance to act on either: the decision to run `measure3 test` is made now, in this file, and
  it WILL be run whatever the page shows (section 8, "No way out"). No TEST-only figure may be derived from the page:
  differencing two readings of the scorecard is a hand-made look, and is not allowed.
- **A look, so not allowed before `test-result.json`**: the plan's S6 "blend versus mid" line in `/api/analysis`.
  Until then it must show nothing for windows closing after TRAIN's end (this protocol's rule; the plan lacks it).
- `cmd/measure3` embeds this file's sha-256 and refuses to run if the file differs, is in no commit or is modified.

## 7. Trials: what each outcome registers
The registry is `select count(*) from strategy_version` **[READ: store/analysis.go:61]**; 18 is what migrations 0004
and 0007 insert **[READ]**, and the count on prod is **[NOT VERIFIED]** until `measure3` prints it.

| Outcome | Versions registered | Trials | Leaderboard `CorrectedT` | Added evidence-page lines |
|---|---|---|---|---|
| R1 fails (`lambda = 0`) | none | 18 -> 18 | 2.991, unchanged | none |
| R2 fails, or the protocol is abandoned (section 8) | none | 18 -> 18 | 2.991, unchanged | none |
| R1 and R2 pass | Scalper v3, Value v3, as `draft` | 18 -> 20 | 3.023 | five, each judged at `CorrectedT(20 + 5)` = 3.090, with `MinWindows = 30` **[CONVENTION, inherited]** |

The five lines are: blend versus mid, and for each version "outcome minus mid" and "outcome minus p". Adding such a
line later raises the count. No verdict is read at an uncorrected threshold. R2 is a gate, not a strategy, and is not
a row in the registry. Its price: with no effect it passes one time in twenty (4.2). A pass is permission to register
two hypotheses for live judgement at the corrected threshold; it is not evidence that either makes money.

## 8. "Register nothing" is an accepted outcome
If R1 or R2 fails then: no model-based version is registered on prod; no migration 0013 is generated; the trials count
does not move. `AC_V3` may still run observe-only. The step still delivers the broker, the engine, the recording path,
the fill-aware analysis and `brokercheck`: a legitimate result under "assume nothing about edge", not a failure.

**What is written down on a failure**, all committed:
1. `research/v3/train-result.json` always, and `test-attempt.json` and `test-result.json` if TEST was opened. A result
   file holds: the git sha, this file's sha-256, `T_c` and its commit's hash, `t0`, the split bounds, the frozen
   window lists (used; ineligible, with reasons; eligible only after the freeze), the covered coins and each added
   coin's `s_c`, every count and exclusion by reason, per-coin figures, `L_hat`, its SE, `B`, the t quantile, lambda,
   the self-check's residual and tolerance, the autocorrelations and whether the block doubled, M2 and M2s (with and
   without vanished-quote observations) and M3 with SEs, and for TEST the attempts, the statistic, SE, `B`, `t` and
   threshold.
2. A dated note under `docs/reviews/` stating which rule failed, the figures, and what they do and do not show. A
   failed R2 shows that the frozen blend did not beat the mid on these 192 windows at this power. It does not show
   that the model is useless, and the note says so in those words.
3. The evidence page carries the negative result.

**No retry on this data.** A further attempt is a new protocol file, a new commit, a new `T_c`, and TEST data made
only of windows closing after that new `T_c`. Every window used here, TEST included, is then burned.

**No way out once TEST can be seen.**
- Once `frozen-params.json` exists with `lambda > 0`, `measure3 test` MUST be run, as soon as the tool reports TEST
  complete (section 6). The sample is fixed by rule, so the timing changes nothing but the temptation.
- Replacing or abandoning this protocol after TRAIN's last close, when R1 has not failed and no `test-result.json` is
  committed, counts as a spent look and a failed R2: from then on the live page shows TEST's rounds. Nothing is
  registered. The dated note of item 2 is written and says "abandoned, TEST not opened". Every window up to the new
  `T_c` is burned.

## 9. Assumptions, gathered
1. **[NOT VERIFIED]** the prod trials count; that `assetcracker_ro` can read the tables of section 2, `evaluation.model`
   included; that BTC and ETH keep carrying the forecast and prod holds no gaps large enough to delay 480 eligible
   windows far beyond the estimated dates. The tool prints the counts, `ok`-false rows and ineligible windows.
2. **[PROD]** `t0`; that SOL, XRP and DOGE carry no forecast. **[INFERRED]** the dates of section 1.
3. **[READ; NOT VERIFIED on prod]** `p_model` is v2's model: the fork test shows it on two recorded fixtures; with no
   live v2 since `t0` it cannot be compared on prod, and the drift check is off (section 2).
4. **[CONVENTION]**, each fixed now: 480 / 24 / 192; 15-second bins; "within 3 s"; one observation per row; the buy
   and sell sides of 3.2 and 3.3 (no fee, no lambda); coins added, never removed; a vanished next quote read as the
   worst price (ask 1.00, bid 0); the one-sided 0.95; the six-hour block and its
   doubling rule; the global-mean autocorrelation with whole windows deleted; the self-check tolerance and floor; the
   15-minute wait before a split is complete; rounding lambda down to 0.01 and costs to 0.0001; equal window weights
   in R2; the 30-observation note in M2s; git's committer time (of the last commit touching this file) as `T_c`; the
   live scorecard's view of TEST windows is accepted and not blinded.
5. The M1 query in 3.1, the existence query of section 1, and the populations of 3.2, 3.3 and 3.5 (described in words,
   to be written as queries in `cmd/measure3`) have never been run against any database.
6. **[READ, at both amendments]** This file is tracked; `docs/reviews/` is still in this checkout's `.git/info/exclude`,
   so the dated notes section 8 requires need `git add -f` or that line removed. `measure3 train`'s refusal for an
   uncommitted protocol rests on both `git log` and `git status --porcelain` for this file.