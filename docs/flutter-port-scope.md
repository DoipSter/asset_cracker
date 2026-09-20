# Scope: morphing Asset Cracker into a cross-platform Flutter/Dart app

Status: **draft for discussion**, branch `flutter-scope`. Tracks Agora intent INT-9.
Nothing here changes the Python.

Progress (2026-09-20): phase 1 (parity harness, `tools/parity/`) and phase 2 (Dart engine,
`engine/`) are done and the parity gate passes on the 40-minute fixture. See section 7.

Author: Brad's agent, 2026-09-20. Statements are labelled **measured** (checked against the
code or the live system on that date) or **assumed** (not checked). Doipster's view is not in
here yet; the open questions at the end are for him.

## 1. Goal

Turn the Windows-only Python/tkinter widget into one app that runs on Windows, macOS and Linux,
and potentially iOS and Android, in this same repo. It stays what it is today: a simulator.
No account, no API key, no real orders.

Not goals: real trading, new strategies, changing the model, a web build (see 6.1).

## 2. What exists today (measured)

| File | Lines | What it holds |
|---|---|---|
| `kalshi_trader.py` | 714 | Model, six strategies, fees, accounting, settlement, JSON/CSV state |
| `asset_cracker.py` | 1,655 | Feeds (~310 lines), settings, and the tkinter UI (~1,120 lines) |
| `make_icon.py` | 63 | Icon drawing; needs Pillow |

Why it is Windows-only:

- `Toplevel.attributes("-transparentcolor", ...)` at `asset_cracker.py:669`. On this Mac
  (Python 3.9.6) tkinter raises `TclError: bad attribute "-transparentcolor"`, so the window
  cannot be created. The app does not start on macOS.
- Toasts shell out to PowerShell and the WinRT notification API; the app id is written to the
  Windows registry.
- Taskbar styling through `ctypes.windll.user32`.
- Fonts are hard-coded to "Segoe UI". Settings live under `%APPDATA%`.

Smaller things found on the way:

- `kalshi_trader.py:685` uses the `dict | dict` operator, which needs Python 3.9. The README
  says 3.8+.
- There are no tests, and the backtests the README cites are not in the repo.
- **Settlements reach the engine more than once** (measured in the 40-minute recording,
  2026-09-20). In `stream_kalshi`, once a round's result arrives `just_closed` is cleared, but
  the block at `asset_cracker.py:366-371` sets it again on the next pass for as long as the
  next round has not been found, so the result is reported again every second. One ETH round
  was delivered 7 times, one BTC round twice. Payouts are not doubled (the lots are already
  closed), but `note_settlement` stores the index-offset measurement again each time: the BTC
  state ended with 14 stored offsets for 13 distinct rounds. Repeats pull the 24-sample median
  toward that round and push older rounds out early. The Dart feed should report each
  settlement once; the Dart engine must still behave like the Python when given repeats, so
  the parity gate holds.
- Runtime state is written next to the script (`data_dir()`), which will not work for an
  installed or sandboxed app.

## 3. Target layout

```
engine/   pure Dart package. No Flutter imports.
  lib/src/model.dart        prob_yes, norm_cdf, kalshi_fee, parse_amount
  lib/src/account.dart      Account: step, entries, exits, lottery, settlement
  lib/src/trader.dart       KalshiTrader: volatility, index offset, records, snapshots
  lib/src/feeds/            Coinbase trades (WebSocket + polling fallback), Kalshi poller
  lib/src/store.dart        state and trade-log persistence behind an interface
  bin/headless.dart         runs the engine with no UI (desktop, server)
  bin/replay.dart           replays recorded ticks, writes a trade log (parity gate)
app/      Flutter app. Depends on engine/.
tools/    Python-side parity harness (recorder + replay). Does not edit the Python app.
```

The Python files stay at the repo root, unedited, until the parity gate (section 5) passes.

## 4. How each part maps

### 4.1 Engine (`kalshi_trader.py` -> `engine/`)

Straight port. It is arithmetic, deques and dicts. Points that need care:

| Python | Dart | Risk |
|---|---|---|
| `math.erf` in `norm_cdf` | `dart:math` has **no erf**. Needs an implementation. | High for parity: a low-precision approximation shifts probabilities near thresholds. Use a double-precision algorithm (W. J. Cody's), not the 1e-7 Abramowitz-Stegun formula. |
| `kalshi_fee`: `ceil(x * 100 - 1e-9) / 100` | same expression on IEEE doubles | Low. Same operation order gives the same bits. |
| `round(x, 2)` on costs, payouts, pnl | Dart has no decimal round; Python's is correctly rounded on the decimal repr | Medium. Needs a helper that matches Python on halfway cases, with tests. |
| `int(stake // unit)` | `(stake / unit).floor()` | Low, but float floor-division differs from divide-then-floor in rare cases. Test. |
| `statistics.median` | hand-written | Low. |
| `time.time()` inside `offset_pct`, `offset_status`, `__init__`, `reset`; `time.monotonic()` in `save` | inject a `Clock` | **Design change.** The Python reads the wall clock inside the engine, so it cannot be replayed deterministically as written. The Dart engine takes the clock as a parameter. The Python side is handled by patching `kalshi_trader.time` from the harness, without editing the file. |
| JSON + CSV written next to the script | `Store` interface; file-backed on desktop, app-documents directory on mobile | Low. Keep the same JSON shape and CSV columns so existing files load. |

### 4.2 Feeds (`asset_cracker.py:167-474`)

| Python | Dart |
|---|---|
| Hand-rolled RFC 6455 WebSocket client (~90 lines) | `dart:io` `WebSocket`. The hand-rolled client goes away. |
| `matches` channel, 15 s silence timeout, 3 failures then fall back to polling | same logic |
| Kalshi once-a-second loop: find market, predict next ticker, order book quotes, settlement checks | same logic on an `HttpClient` with a persistent connection |
| Threads + `queue.Queue` drained every 8 ms by the UI | Streams on the main isolate. **Assumed** sufficient at this message rate; a separate isolate is the fallback if UI jank appears. |
| Clock correction: max of (exchange time - PC time) over 200 samples | same, lives in the engine so headless mode gets it too |

Ticker prediction formats dates with `%b` upper-cased. Dart needs a fixed English month table,
not locale formatting.

All requests send `User-Agent: seans-btc/1.0`. Kalshi answered 200 to that UA from this machine
(**measured**). Whether Kalshi rejects other or empty user agents is **not measured**.

### 4.3 UI (`asset_cracker.py:533-1655`)

The UI is drawn by hand on a canvas in logical pixels with a scale factor, which is the same
model as Flutter's `CustomPainter`.

| Today | Flutter |
|---|---|
| `Drawing` helpers: `rrect`, `circle`, `text`, `px` | `Canvas.drawRRect`, `drawCircle`, `TextPainter` |
| Canvas item tags bound to click handlers | `GestureDetector` regions or real widgets for buttons |
| Two `Toplevel` pages, one shown at a time (`Hub.switch`) | one window, two pages |
| Frameless transparent phone-shaped window, dragged by its body | **desktop only**, via the `window_manager` plugin (frameless, transparent, draggable). **Assumed** to work on all three desktops; transparency on Linux varies by compositor. |
| Side panels as separate windows that slide out from the phone's edges | the largest open UI question, see 6.3 |
| 15-minute round chart with bet markers; range charts 15M/1H/24H/7D | `CustomPainter`, repaint throttled as today (~10/s) |
| Amber glow on a strategy row after a bet | `AnimationController` |
| Segoe UI | bundle one font so every platform matches |

On a phone the "phone-shaped window" idea is redundant: the app is simply full screen, and the
side panels become pages or bottom sheets.

### 4.4 Platform services

| Today | Flutter |
|---|---|
| PowerShell toasts + registry app id | `flutter_local_notifications` (or equivalent). iOS and Android need a runtime permission prompt. |
| `%APPDATA%/AssetCracker/settings.json` | `shared_preferences` or a file under the app-support directory |
| `.ico` files from `make_icon.py` | per-platform launcher icons generated from one source image |
| DPI awareness call | handled by Flutter |

## 5. The parity gate

The README's conclusions rest on details: fee rounding, the final-minute average, the index
offset median. The port is only trustworthy if it reproduces the Python's decisions.

1. **Recorder** (`tools/`): log every input the engine receives, with timestamps: price ticks,
   seed candles, market quotes, settlements, seeded offsets.
2. **Python replay**: feed a recording through the unmodified `kalshi_trader.py`, with
   `kalshi_trader.time` patched to the recorded clock, writing a trade CSV.
3. **Dart replay**: `engine/bin/replay.dart` does the same.
4. **Gate**: the two CSVs match row for row on event, ticker, side, contracts, price, fee, cost,
   payout and pnl. `model_prob` and `edge` are compared after the 3-decimal rounding the Python
   already applies.

A recording needs to span enough rounds to exercise every strategy, including settlements and
at least one Scalper early sale. How many hours that takes is **not known**; the recorder should
report counts per event type so the decision is made on data.

If the gate cannot be met exactly because of `erf` or rounding at a threshold, the fallback is
to report every divergent decision with both engines' inputs, not to loosen the comparison.

What was measured when the gate first ran (2026-09-20): trade rows and saved state match
exactly, and volatility and index offset are bit-identical on every step. Probabilities agree to
2.2e-16, not bit for bit, because CPython's `erf` is the platform C library's and on macOS it
is less accurate than the Dart one (up to 3 units in the last place against 1). No decision
differed. One more hazard turned up: Python's `sum` changed in 3.12 to compensate for rounding,
so a recording made on 3.12+ may differ from one made on 3.9 in the last bit of an average.
The fixture's first line records the Python version for that reason.

## 6. Constraints found

### 6.1 Web target (measured)

Kalshi's API returned 200 to a request carrying a browser `Origin` header but sent no
`access-control-allow-origin`, so a browser would block the response. Coinbase sends
`access-control-allow-origin: *`. A Flutter web build would need a proxy for Kalshi. Web is
out of scope unless someone wants to run that proxy.

### 6.2 Mobile background execution

The Python app assumes it runs continuously: both coins stream and trade every second even when
hidden. iOS suspends a backgrounded app's sockets within seconds; Android allows a foreground
service with a persistent notification. (**Platform behaviour as documented by Apple and
Google; not tested here.**)

Consequence: a phone cannot be the place where the six strategies run unattended. Options:

- **A. Foreground only.** Mobile trades only while open. Results have gaps; the leaderboard is
  not comparable with a desktop run.
- **B. Viewer.** A headless engine runs on a desktop or small server; mobile displays its state.
  Needs a way to reach it (local network, or a small hosted endpoint).
- **C. Both.** Foreground-only by default, viewer when configured.

Keeping `engine/` free of Flutter is what keeps B and C possible. The choice can wait until
phase 4.

### 6.3 Side panels

Two extra OS windows attached to the main window's edges is a desktop-only pattern and awkward
even there in Flutter (multi-window support is limited). Proposal: make the window wider when a
panel opens and draw the panel inside the same canvas. Visually the same, one window.

### 6.4 Tooling (measured)

Flutter and Dart are not installed on this Mac. iOS builds need Xcode and an Apple developer
account for device installs; Windows desktop builds must be made on Windows; Linux on Linux.
No CI exists in the repo.

## 7. Phases

Each phase ends in something that can be checked. No time estimates: none of this has been
measured yet.

| Phase | Deliverable | Done when |
|---|---|---|
| 0 | This document agreed; INT-9 moved from draft to active | Doipster has answered section 8 |
| 1 | Parity harness: recorder + Python replay in `tools/` | **Done.** A 40-minute live recording replays through the Python identically |
| 2 | `engine/` model, accounts, trader, replay | **Done.** All 4,733 steps, 60 trade rows and the saved state match the Python |
| 3 | `engine/` feeds + `bin/headless.dart` | Headless run on macOS and Windows produces state files the Python app can load |
| 4 | `app/` on desktop: both coin pages, charts, panels, notifications | Side-by-side run with the Python app on Windows shows the same prices, round and bets |
| 5 | Mobile layout; decide 6.2 A/B/C | Runs on one iOS and one Android device |
| 6 | Retire the Python: move to `legacy/` or delete; README rewritten | Doipster agrees |

Phases 1 and 2 need no UI decisions and can start as soon as phase 0 is settled.

## 8. Open questions for Doipster

1. Do you agree with the direction at all: same repo, Dart engine, Flutter UI, Python kept as
   the reference until parity?
2. The backtests the README cites (a month of rounds per coin): can the scripts and data come
   into the repo? They would be a far stronger parity test than a few hours of recording.
3. Is the phone-shaped floating window something you want preserved on desktop, or was it a
   means to an end?
4. For mobile, which of 6.2 A/B/C matches what you would actually use?
5. Are existing `kalshi_balance*.json` / `kalshi_trades*.csv` files worth carrying forward, so
   the Dart app must read them, or is a fresh start fine?
6. Anything in the Python you already know is wrong or unfinished, so it is not ported as-is?
