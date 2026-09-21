# Parity harness

**Two engine versions, two harnesses.** `record.py` and `replay.py` here are for the second
version (kalshi_trader.py on main at dc10fd4: one trader, five coins, shared balances, twins);
the Go side is `service/cmd/replay2`. The first version's harness is in `v1/` and needs
kalshi_trader.py at ed05fc2; its fixture is `fixtures/2026-09-20_40min`, and its Go side is
`service/cmd/replay`. What follows was written for the first version; the method is the same.

`fixtures/2026-09-20_v2_17min/` is the first v2 fixture: 11,086 calls across five coins taken
from a live run of his unmodified engine, producing 101 bets, 14 early sales (the Scalper's and
its twin's mirrored ones), 75 settlements and 14 graded exits. It holds the calls, the Python
replay's per-step trace (with all twelve accounts' cash on every step), and the trade log,
early-sales log and final state that replay produced. The Go port is compared with those: the
Python given exactly these calls. No account ran out in it; that path is covered by unit tests
in `service/internal/kalshi15m2`.

`fixtures/2026-09-20_v2_50min/` is the whole of that run: 31,139 calls, 14,802 steps, 272 bets,
69 early sales, 199 settlements. It is the one that showed volatility cannot be held to bit
equality: on some DOGE steps the two engines differ around the fifteenth significant digit
(largest relative gap 1.2e-15), because the update computes `1 - 0.5^(dt/300)`, Go's `pow`
differs from the platform C library's by up to one unit in the last place, and the subtraction
magnifies it. Every decision, all twelve balances on every step, both logs and the final state
are identical. Volatility, like the probabilities, is compared to 1e-12.

Phase 1 of `docs/flutter-port-scope.md`. It answers one question: given the same inputs, does
an engine make the same trades? First for the Python against itself, later for the Dart port
against the Python.

Neither script edits `kalshi_trader.py` or `asset_cracker.py`.

## record.py

Runs the real engine from the live Coinbase and Kalshi feeds, with no window, wired the way
`Monitor` wires it in `asset_cracker.py` (same clock correction, same price coalescing, same
order of calls). Every call into the engine is appended to `calls.jsonl` with the wall-clock
time it was made:

    {"t": 1790000000.12, "coin": "BTC", "call": "step", "args": [{...market...}, 81234.5, 1790000000.31]}

The six recorded calls are the engine's whole input surface: `observe`, `seed_vol`,
`seed_offsets`, `step`, `note_settlement`, `on_settled`. The first line of the file is a `meta`
row with the coins and their calibration constants.

    python tools/parity/record.py --minutes 30

Output goes to a new folder under `tools/parity/recordings/` (gitignored), next to the
`kalshi_trades*.csv` and `kalshi_balance*.json` the live run wrote. It prints call and event
counts once a minute so you can see whether a recording has exercised bets, early sales and
settlements.

Simulation only, like the app: public GET requests, no account, no orders.

## replay.py

Feeds a recording through a fresh engine in a temporary folder and compares the result with
what the live run wrote.

    python tools/parity/replay.py tools/parity/recordings/<stamp>

The engine reads `time.time()` internally (the six-hour window for the index offset), so the
replay swaps the `time` module the engine sees for a clock set to each call's recorded time.
That is the only patch, and it is applied from outside.

Passes (`PARITY OK`, exit 0) when each coin's trade CSV is identical line for line and every
account ends with the same cash. On failure it prints the first differing line from each side.

## Comparing another engine

`replay.py --trace <file>.jsonl.gz` also writes what the engine thought after every `step`:
volatility, index offset, and each strategy's probability, favoured side, edge and bet/no-bet
call. The Dart engine's replay (`engine/bin/replay.dart`) reads `trace.jsonl.gz` from the same
folder and compares step by step, then compares the trade CSV and the saved state.

    python tools/parity/replay.py <folder> --trace <folder>/trace.jsonl.gz
    cd engine && dart run bin/replay.dart ../<folder>

## Fixture

`fixtures/2026-09-20_40min/` is a 40-minute live recording of both coins, stored with
`calls.jsonl` gzipped (replay.py reads either form): 12,562 engine calls, three full rounds per
coin, producing 32 bets, 2 early sales and 26 settlements (60 CSV rows). The Python replays it
identically. This is the first recording the Dart engine has to match.

    python tools/parity/replay.py tools/parity/fixtures/2026-09-20_40min

All six strategies traded in it (Model 26 rows, Scalper 10, Late 10, Value 6, Favorite 6,
Lottery 2). Lottery fired on ETH only, so its path is covered thinly; add a longer recording
when there is one with more.

## Known limit

In the live run the wall clock advances by microseconds during a call; in the replay it is
fixed at the call's start. This only matters if an index-offset measurement sits exactly on
the six-hour cutoff at that instant.
