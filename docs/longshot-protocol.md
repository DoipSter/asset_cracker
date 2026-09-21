# Long-shot protocol (pre-registered)

## What the owner is agreeing to
Brad asked for this on 2026-09-21. It asks one question at four horizons: when a Kalshi crypto contract is priced as a
long shot (10 cents or less) shortly before its close, does buying the OTHER side at the price actually offered make
money after Kalshi's fee? Only markets that close AFTER this file's commit count. Each horizon is tested exactly once,
when its sample is complete. "No" is an accepted result, and a "yes" registers nothing by itself: it only permits a
strategy to be designed and tested as a new trial under a protocol of its own. Simulated money; no order is placed.

Labels: **[READ]** read in code, migrations or the public API on 2026-09-21. **[CONVENTION]** a number chosen, not
measured. Nothing here has been run against data; the one printed query was checked with `EXPLAIN` on the DEV
database only (no rows read).

## 1. Times, the look, and changes
- **`T_c`** is the committer time (UTC) of the last commit that changed this file, printed by
  `git log -1 --format='%H %cI' -- docs/longshot-protocol.md`. Any later commit to this file is a new protocol with a
  new `T_c`. Mechanical slips are corrected under the rule of section 0.1 of `docs/v3-measurement-protocol.md`,
  applied to this file, in the append-only `docs/longshot-protocol-errata.md`.
- **Seen before writing, disclosed:** the live leaderboard rows of v2's Lottery (it buys contracts priced 15 cents or
  less) and its anti-world twin, from before `T_c`. That is related evidence and is never used here. Nobody has
  computed outcome-minus-price by price band on any recorded data.
- **One look per horizon.** The tool refuses to run a horizon until its sample (section 4) is complete, writes the
  window keys it will use to `research/longshot/<horizon>-attempt.json` before reading any outcome, and writes
  `research/longshot/<horizon>-result.json` after. A rerun is allowed only for a mechanical failure, with the error
  quoted in the attempt file. Nothing on the live pages shows outcome-minus-price by price band before a horizon's
  result is committed.

## 2. Observations
A **window** is a close time: every market in the population closing at the same instant, across all five coins.
Markets closing together are one sample, not several, because they share the moment's crypto move.

**H15, the 15-minute markets.** Series `KXBTC15M KXETH15M KXSOL15M KXXRP15M KXDOGE15M` **[READ: instrument rows]**.
For each market with `result in ('yes','no')` closing after `T_c`: the FIRST evaluation row with
`closes_at - 300 s <= at < closes_at - 240 s` **[CONVENTION: about five minutes before the close]**, read by:
```sql
select distinct on (e.market_id) e.market_id, m.closes_at, m.result, e.at,
       (e.quotes->>'yes_bid')::numeric as yes_bid, (e.quotes->>'yes_ask')::numeric as yes_ask,
       coalesce((e.quotes->'yes_bids'->0->>1)::numeric, 0) as yes_bid_size,
       coalesce((e.quotes->'no_bids'->0->>1)::numeric, 0) as no_bid_size
  from evaluation e
  join market m on m.id = e.market_id
  join instrument i on i.id = m.instrument_id
 where i.symbol in ('KXBTC15M','KXETH15M','KXSOL15M','KXXRP15M','KXDOGE15M')
   and m.result in ('yes','no')
   and m.closes_at > $1 and m.closes_at <= $2
   and e.at >= m.closes_at - interval '300 seconds' and e.at < m.closes_at - interval '240 seconds'
 order by e.market_id, e.at;
```
`$1` is `T_c`; `$2` is the close of the last window of the sample (section 4).

**Hh, Hd, Hw, the longer horizons.** Markets of the above/below series `KXBTCD KXETHD KXSOLD KXXRPD KXDOGED`
(`strike_type` "greater") **[READ: public API, 2026-09-21]**, recorded once a minute by the ladder recorder that is
built after this commit. Each series carries events of different lengths; an event's horizon is its open-to-close
time: **hourly** up to 70 minutes, **daily** 20 to 30 hours, **weekly** 6 to 8 days; anything else is left out
**[CONVENTION]**. Observation: the first snapshot with `tau` at most 15 minutes (hourly), 3 hours (daily) or 24 hours
(weekly), and more than 80% of that **[CONVENTION]**. Their query is written in the tool once the recorder's
tables exist, under the errata rule (names only).

**Eligible observation** (every horizon). A two-sided top of book: `0 < yes_bid < yes_ask < 1`. Mid
`m = (yes_bid + yes_ask) / 2`.
- If `m <= 0.10` the long shot is YES: price `p = m`, outcome `y = 1` if the result is yes. The favourite is NO,
  bought at `c = 1 - yes_bid`, and the size at the yes bid must be at least one contract.
- If `m >= 0.90` the long shot is NO: `p = 1 - m`, `y = 1` if the result is no. The favourite is YES, bought at
  `c = yes_ask`, and the size at the NO bid (the YES ask) must be at least one contract.
- Otherwise the market is not in the sample. A snapshot whose book is one-sided or crossed is not in the sample.

## 3. Statistics
Per observation: the return of buying ONE favourite contract at `c` and holding it to the result,
`r = (1 - y) - c - f`, with Kalshi's fee `f = ceil(0.07 * c * (1 - c) * 100) / 100` dollars for a one-contract
order **[READ: the fee rule the engines use]**. Per window `w`: `R_w` = mean of `r` over its observations; `D_w` =
mean of `(y - p)`.
- **Primary, one per horizon:** is the mean over windows of `R_w` above zero? `t` = mean / SE.
- **Secondary, reported with no verdict:** the mean of `D_w` (are long shots priced above their win rate?), and the
  count of observations per window.
- **SE:** block jackknife over consecutive windows. Blocks span 6 hours for H15 and Hh **[CONVENTION]**; for Hd and
  Hw each window is its own block. `B` = number of blocks.
- **Verdict:** the four primary tests are one family at 5% overall, so each is one-sided at 1.25%: "profitable" if
  `t >= t_{0.9875, B-1}`, otherwise "not shown". Nothing else earns a verdict.

## 4. Sample, and when each horizon runs
Windows are taken in close order, starting with the first that closes after `T_c`; a window counts once it holds at
least one eligible observation and its markets all have results. **[CONVENTIONS]** H15: the first 480 such windows
(about 5 days if most windows qualify). Hh: the first 240. Hd: the first 60 (about two months). Hw: the first 26
(about six months). Each horizon runs once, when its count is reached; the power is unknown and is reported.

## 5. Outcomes
- **Profitable:** a long-shot strategy for that horizon may be designed. It is a new trial: its own protocol,
  its own `strategy_version` row, judged on data that closes after that protocol's commit. Nothing trades on this
  result alone.
- **Not shown:** nothing is registered. The result, with the figures, is written up on the evidence page.
- Either way the result file records `T_c`, this file's sha-256, the errata in force, the window keys, the
  eligible counts and every figure above.
