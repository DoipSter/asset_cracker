# Working on Asset Cracker

This file is the contract. It is here so that anyone — a person joining the project, or an
agent picking it up cold — can work on it the same way without being told, and so that
anyone reading the history later can tell what happened and why.

## Branching

**Never commit directly to `main`.** `main` is what works; it moves only through a pull
request that CI has passed.

Every change starts on a branch, named for what it does:

| Prefix | For |
|---|---|
| `feature/` | new behaviour — `feature/one-minute-chart` |
| `fix/` | something is wrong — `fix/spread-from-closing-book` |
| `research/` | investigation, may never merge — `research/market-microstructure` |
| `integration/` | several finished branches gathered for one review — `integration/all-features` |

Branching is cheap and needs no permission. Deciding what lands on `main` is the owner's
call, not the contributor's.

### Stacked branches

A branch started from another branch rather than from `main` carries everything below it.
Check before merging rather than assuming either way:

```bash
git merge-base --is-ancestor feature/lower feature/upper && echo "already included"
```

When a stack is finished, give the tip an `integration/` name that says what it holds. The
tip's own name usually describes only the last thing added to it, which is misleading once
three other features are sitting underneath.

## Committing

**Commit as you go. Do not leave work uncommitted to "see if it's any good."** Uncommitted
work cannot be reviewed, diffed, reverted, bisected, or found again after a crash. If a
change turns out to be wrong, that is what `git revert` and branch deletion are for, and
both leave a record of what was tried. An experiment that is thrown away silently teaches
nobody anything; one that is committed and reverted teaches the next person not to try it.

A commit message explains itself to someone who was not there:

1. **A subject line that states the change**, not the area it touched. `Take the logged
   spread from the live book, not the closing bell` — not `fix logging`.
2. **What was wrong**, concretely, with the symptom that revealed it.
3. **What changed**, and why that approach over the obvious one.
4. **The measurement** that shows it worked. A number beats an assurance. If an approach
   was tried and rejected, say so and give its number too — that is the most useful part of
   the message six months later, because it stops the next person retrying it.

## Tests

```bash
python tests/run.py          # the whole suite, no arguments, no packages
python tests/run.py -v       # with each test named
```

Stdlib `unittest` only, because the app itself has no dependencies and the test suite
should not add the project's first one.

The tests never import `asset_cracker` — importing it builds a Tk window, which fails on a
headless machine. Anything they need from the app is pulled out of the source by
`helpers.app_value`, which means a test notices if the app's own table changes underneath
it. Display logic is checked as geometry and numbers rather than by eye.

**Every behaviour change needs a test that fails without it.** Where a fix is a judgement
call rather than a correction — an exit threshold, a position cap — the test pins the
behaviour and the commit message carries the evidence for the number chosen.

A skipped test proves nothing. If a scenario cannot produce the situation under test,
fix the scenario rather than skipping; `helpers.lagging_book_round` exists because a
synthetic order book priced off the same model the strategies use offers no edge, so
nothing ever trades and the test quietly tests nothing.

## CI

`.github/workflows/tests.yml` runs the suite on every push and pull request, on Windows and
Linux, on Python 3.12 and 3.13. Windows is where the app runs; Linux catches anything that
quietly depends on Windows path handling. A red branch does not merge.

## The platform, until the calibration protocol reports (owner, 2026-09-24)

The Go service under `service/` has not shown an edge. On 2026-09-23 every 15-minute version on
the leaderboard had lost money after fees, and none could be told from zero. Until
`docs/calibration-protocol.md` has reported its TEST result, three rules hold (INT-21):

- **No new strategy versions.** Nothing registers (`strategy_register`, the buckets page's New
  strategy) or deploys (`strategy_deploy`, the Deploy form) a version that is not already on the
  registry. Every `strategy_version` row raises the significance bar for all of them (the trials
  correction), and every variant tried is another look at the same tape.
- **No new bank or bucket bookkeeping.** No new paydays, reserves, allocation rules, bucket
  switches or history views until a version passes the promotion gate. Fixes to what exists are
  welcome. The owner excepted two on 2026-09-25 (TSK-51): deployed capital drawn on the home
  page's value chart, and the tax reserve counted as a debit against the total.
- **Batch releases.** A prod release restarts the service. In the 48 hours to 2026-09-24 there
  were 34 restarts, and all 416 of Kalshi's 429 replies in that time came within three minutes of
  one, while the engine rebuilt its books. Release when a change needs to be live, and carry
  everything that is ready in one release.

Still wanted: measurement (the calibration protocol's tool, before its TRAIN ends) and fixes.

## Simulation only

Every balance in this project is imaginary. `START_BALANCE` is a number in a file and no
code in this repository places a real order or touches a real account. Any change that
would alter that is out of scope for a pull request and needs a conversation first.
