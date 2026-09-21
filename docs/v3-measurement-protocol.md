# v3 measurement protocol (pre-registered)

## What the owner is agreeing to

Your yes to this text, given before it is committed, settles three choices. Changing one later means a new protocol
with a later start, and the rounds used so far could no longer confirm anything.
1. How far to trust the model: the cautious end of what the data supports, not the best guess.
2. How pretend orders fill: at the price shown, with a measured charge for the price moving a second later.
3. If this protocol says no: no new strategy is added. There is no fallback strategy.

Two facts. "Add nothing" is an accepted result, not a failure. And the only data that can confirm anything is
15-minute rounds that close AFTER this file's commit; every earlier round can only be used to set the numbers.

Step S0 of `docs/honest-fills-v3.md` (6.1 to 6.5, 7.1; decisions D1, D2, D3, D13, D15), committed BEFORE any
measurement is run. Simulated money only. Every query below is unexecuted, and no figure here measures prod.

Labels: **[READ]** read in the code, migrations or row samples on 2026-09-21. **[INFERRED]** concluded from what was
read. **[NOT VERIFIED]** could not be checked from here. **[CONVENTION]** a chosen number, not a measurement.

## 0. The three choices this commit fixes, and what stays open
D2, D15 and D3 are fixed by the commit that first adds this file: the owner's yes to this text, which D1 requires
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
split, the t thresholds, the block-doubling rule and its loss of power, that M2 and M2s are proxies, and the accepted
leak of section 6 with its price (`measure3 test` is run whatever the live page shows).
- NOT settled by that commit, and still the owner's: D13, who runs `measure3` against prod and as which role. Assumed:
  the owner, as `assetcracker_ro`, read-only over the documented ssh tunnel. **[NOT VERIFIED]** that this role can
  `select` from the tables in section 2; if it cannot, the grant is the owner's to make and nothing is run until then.
- Likewise open: D9 (fee rounding per order or per fill) and D10 (quarter Kelly, the window cap, the seed). They set
  no number in this protocol and are listed only because they enter `frozen-params.json` beside the measured values.

## 1. Times, units and the split
- **`T_c`** is the committer time, in UTC, of the LAST commit that changed this file, with that commit's hash, as
  printed by `git log -1 --format='%H %cI' -- docs/v3-measurement-protocol.md`: that is the commit that first adds the
  file, as any later commit to it is a new protocol (section 0) with its own `T_c`. It is the committing machine's
  clock **[CONVENTION: git's committer time is taken as true]**; pushing the same day, so that a second party holds
  the timestamp, is the owner's action. `measure3 train` refuses to run if that command prints nothing (the file is in
  no commit) or if `git status --porcelain` on the file prints anything (changed since its commit). It writes the hash
  and `T_c` into `frozen-params.json` and `train-result.json`; `measure3 test` takes both from there and never
  recomputes them, so the result files cannot disagree. **Squash and rebase**: the commit reaches main by a merge
  commit, as PRs #1 and #3 did **[READ: `git log --merges`]**. If it is rewritten anyway: before `frozen-params.json`
  exists the later time stands (it burns more, never less); after, the recorded hash and `T_c` stand, since the
  embedded sha-256 shows the text is unchanged, and `test-result.json` also records the hash git prints then.
- **Window**: all `market` rows that share one `closes_at` and whose `instrument.symbol` is one of `KXBTC15M`,
  `KXETH15M`, `KXSOL15M`, `KXXRP15M`, `KXDOGE15M` **[READ: 0006, 0007; these five carry `spec.v2 = true`]**. The
  window's key is `w = extract(epoch from closes_at)`, an integer multiple of 900.
- **v2 originals**: `strategy_version` rows with `version = 2`, joined to `strategy` with `family = 'kalshi15m'`,
  whose `params->>'anti'` is absent or not `true` **[READ: 0007; the sampled twins carry `"anti": true`]**. Six are
  expected; the tool prints the ids it found and stops if there are not six. Orders are tied to a version by
  `trade_order.bucket_id -> bucket.strategy_version_id`, so a restaked bucket is included **[READ: 0001]**.
- **`t0`**: the smallest `trade_order.placed_at` over v2 originals' buckets. **[NOT VERIFIED]**: the sampled v2
  buckets were created at 2026-09-21 05:47:45 UTC, so `t0` is expected shortly after that; the tool measures it.
- **Scored row**: a row the M1 query (3.1) returns, before the restriction to the actionable set A. A market HAS one
  exactly when some v2 original's `decision` row meets every condition of that query's WHERE clause except the time
  bounds `$2`/`$3`. An **existence query** over that clause tests this. It returns `(closes_at, market id)` only,
  never `model_prob`, `market_prob`, a quote or `result` as a value, so it is not a look (6). Unexecuted.
- **Eligible window**: closes after `t0`; has all five markets; every one has `result in ('yes','no')` and a scored
  row, by the existence query. Judged at run time, then FROZEN (below). An ineligible window is listed with its reason
  (`missing market`, `no result`, `no scored row: <ticker>`). None is dropped or added by hand.
- **Split, by rule.** 480, 24 and 192 are **[CONVENTION]**, fixed now, never revisited after outcomes are seen.
  - TRAIN: the first 480 eligible windows in `closes_at` order (about five days).
  - Embargo: every window closing in the 24 x 900 s = 6 hours after TRAIN's last close (48 windows, 12 hours, if the
    block is doubled, 5.3). Counted by the clock, eligible or not. Never read for any statistic.
  - TEST: the first 192 eligible windows closing after BOTH the end of the embargo AND `T_c`.
- **Order of work, and the freeze.** `measure3 train` walks windows in `closes_at` order from `t0` with the existence
  query and stops at the 480th eligible one. Only then does M1 run, with `$2`/`$3` from those windows; it never
  selects a probability, quote or result for any later window. A split is complete when its last close is 15 minutes
  old **[CONVENTION: the poller stops asking for a result after 15 minutes; READ: kalshi/poller.go `GiveUpAfter`]**.
  The first complete run writes TRAIN's 480 window keys, the embargo's end and the windows then ineligible, with
  reasons, into `frozen-params.json`; reruns and `measure3 test` use that list, never re-deriving it. A given-up
  market gets its result only at a later restart **[READ: store/store.go `UnsettledMarkets`]**; a window so made
  eligible stays out, reported as "became eligible after the freeze". TEST's list is frozen likewise at its run (6).
- **Burned**: the 38 windows behind `docs/reviews/strategy-review-2026-09-21.md`, and every window closing at or
  before `T_c`. They may be used for estimation (TRAIN) and never for confirmation (TEST).

## 2. What is read, and what the recording can and cannot supply
Tables and columns **[READ: `db/migrations/0001_init.sql`, checked against the row samples]**:
`evaluation (id, at, market_id, quotes)`, `decision (id, at, evaluation_id, strategy_version_id, model_prob,
market_prob)`, `market (id, instrument_id, closes_at, result)`, `instrument (id, symbol)`, `trade_order (id,
bucket_id, market_id, action, side, status, placed_at)`, `bucket (id, strategy_version_id)`,
`strategy_version (id, strategy_id, version, params)`, `strategy (id, family)`. Reads only. No write, no DDL.

- `decision.model_prob` for a v2 row is the raw model's P(yes) (`View.PModel`), not the strategy's blend, and
  `decision.market_prob` is the yes mid, `(yes_bid + yes_ask) / 2` when `yes_bid > 0` **[READ: runner2.go:289,
  kalshi15m2/account.go:190-194]**. All six originals journal the same two numbers for one evaluation, so which
  original's row is picked does not change `p` or `q` **[INFERRED]**.
- v2 journals a decision when the strategy acted, when its answer changed, and at least every 15 s **[READ:
  runner2.go:279-284]**. That is why M1 bins by 15 s.
- The top-of-book keys `yes_bid`, `yes_ask`, `no_bid`, `no_ask` are in the oldest sampled row (evaluation id 2,
  2026-09-21 02:49:09 UTC), so they cover the whole of v2's life **[READ: samples]**.
- **Depth fields** (`yes_bids`, `no_bids`, `yes_bid_depth`, `no_bid_depth`) and `evaluation.model` contents exist only
  in rows written by release `87a2100` or later. Its commit time is 2026-09-21 07:30:26 UTC **[READ: `git log`]**; the
  sampled evaluation id 2 (02:49 UTC) has neither field and the sampled rows at 07:41:28 and 07:44:19 UTC have both
  **[READ]**, so it went live between 07:30:26 and 07:41:28 UTC **[INFERRED]**. The first depth row is **[NOT
  VERIFIED]**; the tool prints it: the smallest `evaluation.at` whose `quotes` has `yes_bids`.
- **Consequence.** No measurement in this protocol reads a depth field or `evaluation.model`: M1, M2, M2s and M3 use
  top-of-book quotes, decisions, orders and results only, so the windows between `t0` and the first depth row are
  usable. A later statistic that needs depth must restrict itself by the key's presence, never by a time.

## 3. The measurements
All prices are in dollars per contract as stored (`"0.4500"`). `y = 1` if `market.result = 'yes'`, else 0.
`p = model_prob`, `q = market_prob`, `d = p - q`. Row order is fixed by the queries, so a rerun reproduces digits.

### 3.1 M1: lambda
Rows: v2 originals' decisions, the FIRST row in each 15-second bin per market **[CONVENTION: the journal writes extra
rows on action seconds; binning stops those seconds being over-weighted]**, two-sided books only. `$1` is the six
original version ids; `$2` (the split's first close minus 900 s) and `$3` (its last close) are known from the
existence query before this query runs (section 1, "Order of work"). Unexecuted:

```sql
select distinct on (e.market_id, floor(extract(epoch from (m.closes_at - d.at)) / 15))
       extract(epoch from m.closes_at)::bigint as w, d.model_prob as p, d.market_prob as q, (m.result = 'yes')::int as y,
       (e.quotes->>'yes_ask')::float8 as yes_ask, (e.quotes->>'no_ask')::float8 as no_ask
  from decision d
  join evaluation e on e.id = d.evaluation_id and e.at = d.at
  join market m on m.id = e.market_id
 where d.strategy_version_id = any($1) and d.at >= $2 and d.at < $3 and e.at >= $2 and e.at < $3
   and m.result in ('yes','no') and d.model_prob is not null and d.market_prob is not null
   and (e.quotes->>'yes_bid')::numeric > 0 and (e.quotes->>'no_bid')::numeric > 0
 order by e.market_id, floor(extract(epoch from (m.closes_at - d.at)) / 15), d.at, d.id;
```

Rows inside those bounds whose window is not in the frozen list are discarded. **Actionable set A**: the favoured side
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
Every `trade_order` with `action = 'buy'` and `status = 'filled'` in a v2 original's bucket, whose market's window is
in TRAIN. The entry snapshot is the `evaluation` row of that market with `at = placed_at` (**[INFERRED]**: the sampled
order's `placed_at` equals its `decision_at`, and a decision's `at` is its evaluation's by foreign key). The next
snapshot is the first `evaluation` row of that market with `placed_at < at <= placed_at + 3 s` **[CONVENTION:
snapshots are a second apart; a later one answers a different question]**. The observation is
`ask(next, bought side) - ask(entry, bought side)`, `yes_ask` or `no_ask` by `trade_order.side`.

Not imputed, but counted and reported by reason: no entry snapshot; no next snapshot within 3 s; a quote of 0 on the
traded side in the ENTRY snapshot (nothing was shown, so the fill cannot be priced). **A quote that has vanished in
the NEXT snapshot is read as the worst price, for buys and sells alike [CONVENTION: the conservative reading]**: a
next ask of 0 counts as 1.00, and in 3.3 a next bid of 0 counts as 0. The recording writes an ask as `"0.0000"`
exactly when the other side has no bid **[READ: kalshi/quotes.go `BookQuotes`]**, so nothing could then be bought
under a dollar. These are counted, and the mean without them is reported beside the frozen one and freezes nothing.
One observation per order, not weighted by contracts **[CONVENTION]**. Point estimate: the mean; SE: section 5.
**Frozen value: `max(0, mean)` rounded to 0.0001.** It is a cost: the point estimate is charged, the SE recorded.

### 3.3 M2s: stale_cost_sell (selling)
The mirror image over `action = 'sell'`, `status = 'filled'`: `bid(entry, sold side) - bid(next, sold side)`,
`yes_bid` or `no_bid`. A sold side with no bid in the next snapshot counts as a bid of 0 (the loss is real); a bid of
0 in the ENTRY snapshot is excluded and counted, as in 3.2. Same mean, SE and freezing rule. Under 30 observations
**[CONVENTION, the 30 of `analysis.MinWindows`]** it is still frozen, and the provenance note says so.

### 3.4 What M2 and M2s are
Measured, but on a DIFFERENT population from the one they gate. v2's orders fire on the raw model and fill at the
touch. v3's entries fire only where `lambda (p - q)` clears spread, fee and this cost, where disagreement with the
market is largest and adverse selection plausibly worst, and they may walk below the touch. Both are a **proxy
measured on v2's entries and exits**; `Provenance.Note` for both reads "measured on v2's orders in TRAIN; a proxy for
v3's". Live next-second re-pricing of v3's OWN fills replaces both in any later version, which is a new trial.

### 3.5 M3: outcome correlation across coins
Over TRAIN windows: the Pearson correlation of `y` for each of the ten coin pairs, and their mean, with the
block-jackknife SE. Recorded only. It sets no parameter; sizing assumes a correlation of 1 **[CONVENTION]**.

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
1. A window's block is `floor(w / 21600)` (six hours). An M2 or M2s order takes its market's window's block.
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
- **A leak that is accepted, said plainly.** The live evidence page keeps showing v2's and v1's results over the TEST
  days, as it does today. Its scorecard is the window-mean of `Brier(model) - Brier(mid)` on every settled round, TEST
  windows included **[READ: store/analysis.go `scored`, analysis/report.go `buildScorecard`]**. By 3.1 that is
  `V (1 - 2 L)` on all scored rows: a continuous, informal view of a statistic strongly correlated with R2's. v2's P&L
  lines are a noisier view of the same thing. They are not frozen, because blinding the released page is not
  proportionate. What is removed instead is any chance to act on them: the decision to run `measure3 test` is made
  now, in this file, and it WILL be run whatever the page shows (section 8, "No way out"). No TEST-only figure may be
  derived from the page: differencing two readings of the scorecard is a hand-made look, and is not allowed.
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
   window lists (used; ineligible, with reasons; eligible only after the freeze), the version ids, the first depth
   row's time, every count and exclusion by reason, `L_hat`, its SE, `B`, the t quantile, lambda, the self-check's
   residual and tolerance, the autocorrelations and whether the block doubled, M2 and M2s (with and without
   vanished-quote observations) and M3 with SEs, and for TEST the attempts, the statistic, SE, `B`, `t` and threshold.
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
1. **[NOT VERIFIED]** `t0`; the prod trials count; that `assetcracker_ro` can read the tables of section 2; that prod
   holds no gaps large enough to delay 480 eligible windows far beyond five days. All are printed by the tool.
2. **[INFERRED]** `trade_order.placed_at` equals its entry evaluation's `at`. Orders for which it does not hold are
   counted under "no entry snapshot", so a wrong inference shows as a large count, not as a silent bias.
3. **[INFERRED]** All six originals journal the same `p` and `q` for one evaluation.
4. **[CONVENTION]**, each fixed now: 480 / 24 / 192; 15-second bins; "within 3 s"; one observation per order; a
   vanished next quote read as the worst price (ask 1.00, bid 0); the one-sided 0.95; the six-hour block and its
   doubling rule; the global-mean autocorrelation with whole windows deleted; the self-check tolerance and floor; the
   15-minute wait before a split is complete; rounding lambda down to 0.01 and costs to 0.0001; equal window weights
   in R2; the 30-observation note in M2s; git's committer time (of the last commit touching this file) as `T_c`; the
   live scorecard's view of TEST windows is accepted and not blinded.
5. The M1 query in 3.1, the existence query of section 1, and the populations of 3.2, 3.3 and 3.5 (described in words,
   to be written as queries in `cmd/measure3`) have never been run against any database.
6. **[READ]** This file and `docs/reviews/` are in this checkout's `.git/info/exclude`: while untracked they are
   hidden from `git status`, and committing needs `git add -f`. Hence `git log`, not `git status`, in section 1.
