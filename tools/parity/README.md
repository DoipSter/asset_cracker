# Parity harness

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
