# Spot trend protocol (pre-registered, live weeks only)

## What Brad agreed to (2026-09-21)
Three simple "hold a coin while it trends up" ideas on Coinbase prices, tested ONLY on weeks that start after this
file's commit. The three years of recorded history are left to the context-aware model (Brad's option "c"), so this
test never looks at them for results. It takes 28 weeks (about 6 months); Brad judged that fine because shorter-horizon
strategies run meanwhile and the aim is to evaluate the full mix. "No" is an accepted result; a "yes" only permits a
paper strategy under a protocol of its own. Simulated; nothing trades.

Any change before the first live week starts (Monday 2026-09-28 00:00 UTC, if committed on 2026-09-21) is a new
protocol that costs nothing, because no outcome can have been seen. After that, changes follow section 0.1 of
`docs/v3-measurement-protocol.md` (errata only, in `docs/spot-protocol-errata.md`).

Labels: **[READ]** read in code, migrations or Coinbase's API. **[CONVENTION]** chosen, not measured.
**[ASSUMPTION]** believed, not checked.

## 1. Times and the look
- `T_c`: committer time (UTC) of the last commit changing this file,
  `git log -1 --format='%H %cI' -- docs/spot-protocol.md`.
- **Live weeks:** the first 28 complete weeks, Monday 00:00 UTC to the next Monday, that START after `T_c`.
- **One look.** Nothing computes or shows a running figure for these ideas until all 28 weeks are complete. The tool
  then writes `research/spot/live-attempt.json` (the week keys) before reading any return, and
  `research/spot/live-result.json` after. A rerun only for a mechanical failure, quoting the error.

## 2. Data
Daily candles (`granularity_s` 86400, starting 00:00 UTC) of BTC-USD, ETH-USD, SOL-USD, XRP-USD and DOGE-USD in the
`candle` table **[READ: migration 0015]**. `P_i(t)`, coin `i`'s price at Monday 00:00 UTC `t`, is the `close` of its
candle starting at `t - 1 day`. Week `w` runs from Monday `t_w` to `t_w + 7 days`; coin return
`r_iw = P_i(t_w + 7d) / P_i(t_w) - 1`. Past return `m_iw = P_i(t_w) / P_i(t_w - 4 weeks) - 1`. The four weeks
before the first live week are INPUT only (they set the first positions), never an outcome. A week missing any price
it needs is dropped and counted.

## 3. The three ideas
Weights `w_iw` sum to at most 1; the rest is cash. Lookback **4 weeks for all three [CONVENTION: fixed without any
data; there is no training stage to choose it]**.
- **H1, each coin's own trend:** `w_iw = 1/5` if `m_iw > 0`, else 0.
- **H2, bitcoin's trend:** `w_iw = 1/5` for every coin if `m_BTC,w > 0`, else all 0.
- **H3, the two strongest:** `w_iw = 1/2` for the two coins with the largest `m_iw` (a tie goes to the symbol first
  alphabetically), else 0. **[CONVENTION: two]**

## 4. The weekly number, with costs
`x_w = sum_i w_iw r_iw - (1/5) sum_i r_iw - c * sum_i |w_iw - w_i,w-1|`: the idea's return, less holding all five
equally, less the cost of changing weights (the first week pays for its move from cash). Fills at `P_i(t_w)`.
**[ASSUMPTIONS]** cash earns nothing; drift within a week is ignored; the benchmark's own rebalancing is free (this
favours the benchmark). Reported at `c` = 0, 0.10, 0.25, 0.40, 0.60 and 1.00 percent per side; **only 0.60% gives a
verdict [ASSUMPTION: Coinbase's entry-tier taker fee plus spread, not read from Brad's account]**.

## 5. Statistics
- Unit: one week, the five coins pooled into `x_w` (they move together, so they are not separate samples).
- SE: block jackknife over consecutive counted weeks in blocks of 4 **[CONVENTION]**; `B` blocks (7 if no week
  drops). `t = mean(x_w) / SE`.
- Verdict per idea: "confirmed" if `t >= t_{1-0.05/3, B-1}` (one-sided, Bonferroni over three ideas), otherwise
  "not shown". Nothing else earns a verdict.
- Reported with no verdict: every figure at every cost level, the returns against cash, weeks dropped. The power is
  unknown and is said to be.

## 6. Outcomes
- **Confirmed:** a paper strategy for that idea may be designed as a new trial (its own protocol and
  `strategy_version` row, simulated money, real bid/ask measured). Nothing trades on this result alone.
- **Not shown:** an accepted result, written up on the evidence page with every figure.
- The result file records `T_c`, this file's and the errata's sha-256, the tool's git sha, the week keys, the drops
  and every figure above.
