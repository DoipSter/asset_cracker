# Multi-coin trading, a bigger bank, an anti-world, and the tooling to check any of it

Eight commits gathering four features that were built as a stack. `main` is the merge of
`feature/one-minute-chart`; everything since sits on top of it, in order.

## What lands

**A bigger bank, capped in dollars.** $150 → $1,000 per strategy, with at most $250 at risk
across all open bets. Still a share of the *starting* balance rather than the current one: as
a share of equity, a winning run raises the ceiling on its own stakes, so a bad round costs
more the better things have been going. At $2,000 the old rule would have allowed $1,000 at
risk; the new one holds at $74.99 → $250 whatever the balance.

**Running out is no longer silent.** When a strategy has under a dollar and nothing
outstanding, a postmortem is written *before* anything is cleared — net by coin, by outcome,
by what triggered each early sale, the worst rounds and what share of the damage they were,
and the settings it was running. A line goes to `kalshi_bankruptcies.log`, the app notifies,
and the strategy is staked again so the comparison keeps running. `bankruptcies` counts the
lives, because a strategy on its third is saying something a balance is not.

**An anti-world.** A left panel holding a mirror of every strategy — `Anti Value`, `Anti
Model`, and so on — each with its own $1,000. A twin has no opinions: it takes the other side
of whatever its original does, at the same moment, for the same money, and closes when the
original closes. Matching the **stake** rather than the contract count matters, because the
two sides of a market are different prices and matching contracts had a twin committing $524
against the $250 cap its original had just respected.

**A world toggle.** A globe beside the bell flips which world the chart's bet markers come
from — the one place the two are otherwise indistinguishable. It deliberately does not touch
the panels.

**Session recording.** Every row in all three CSVs carries the run it came from, and
`kalshi_sessions.csv` indexes the runs with what each was *configured* with, not just how it
went. The bank and the cap have both changed mid-project; without the settings on the row, an
old session's numbers cannot be read correctly. `research/sessions.py` reads it back.

## Findings recorded rather than acted on

**The Scalper does not cut its losses, and shouldn't.** It cuts a loser 45 times out of 758.
Both a price stop and a time stop were implemented and measured; every setting is worse, by
$105 to $207 over a month. Positions that fall below 70% of cost still win 15% of the time,
and those pay 100¢ on contracts bought for 20¢. Both knobs ship off, with a test enforcing
it. The caveat is in `research/backtest_stops.py`: a stop aiming for 50% of cost filled at a
median of **26%** on one-minute bars, so that data cannot fairly test a price stop — the time
stop, which triggers on the clock and cannot gap, is the stronger half of the argument.

**Taking the other side is not free money either.** Three twins made money over the month and
three did not. But a stake-matched pair is not a hedge — equal money buys 0.03× to 3.25× the
contracts — so a positive pair does **not** prove the model is backwards.

## Checking it

116 stdlib `unittest` cases, `python tests/run.py`, green on Windows and Linux across Python
3.12 and 3.13. Three backtests drive the real engine rather than a reimplementation, so what
they measure is what the app does, and `research/fetch_rounds.py` rebuilds their input from
Kalshi's public API.

Worth knowing about two bugs the tooling caught that reasoning had not: the backtests were
silently **reviving bankrupt accounts**, so `Anti Model` appeared to make +$3,283 when it had
gone bankrupt 29 times and actually lost $25,717 of the $30,000 fed to it; and the first live
session row claimed **125 bets for a 25-second run**, because the accounts persist across
restarts. Both are fixed and both have tests.

## Still simulated

No account, no API key, no real orders. Every balance here is imaginary.

---

🤖 Generated with [Claude Code](https://claude.com/claude-code)
