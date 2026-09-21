"""Replay a v2 recording through a fresh, unmodified engine and compare.

Reads calls.jsonl(.gz) from a folder made by record.py, builds a new KalshiTrader in a
temporary folder, and makes the same calls in the same order. The engine reads the wall clock
internally, so the clock it sees is replaced with the recorded time of each call.
kalshi_trader.py itself is not edited.

Passes when the replayed trade log and early-sales log are identical to the live run's, line for
line, and every account ends with the same cash.

    python tools/parity/replay.py <folder>
    python tools/parity/replay.py <folder> --trace <folder>/trace.jsonl.gz

--trace writes what the engine thought after every step (volatility, index offset, each
strategy's view of that coin, and all twelve accounts' cash) for another engine to be checked
against. The Go port's replay reads it: service/cmd/replay2.
"""

import argparse
import collections
import gzip
import json
import os
import shutil
import sys
import tempfile
import time as real_time

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, ROOT)

import kalshi_trader  # noqa: E402


class ReplayClock:
    """Stands in for the `time` module inside kalshi_trader during a replay."""

    def __init__(self):
        self.t = 0.0

    def time(self):
        return self.t

    def monotonic(self):
        return self.t

    def localtime(self, secs=None):
        return real_time.localtime(self.t if secs is None else secs)

    def strftime(self, fmt, *args):
        return real_time.strftime(fmt, *(args or (real_time.localtime(self.t),)))


def read_lines(path):
    try:
        with open(path, newline="") as f:
            return f.read().splitlines()
    except FileNotFoundError:
        return []


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("folder")
    ap.add_argument("--trace")
    args = ap.parse_args()

    path = os.path.join(args.folder, "calls.jsonl")
    opener = open
    if not os.path.exists(path):
        path, opener = path + ".gz", gzip.open
    with opener(path, "rt") as f:
        rows = [json.loads(line) for line in f if line.strip()]
    meta, calls = rows[0]["meta"], rows[1:]
    if meta.get("engine") != "v2":
        sys.exit("this recording is for the first engine version: use tools/parity/v1/replay.py at ed05fc2")

    clock = ReplayClock()
    clock.t = rows[0]["t"]
    kalshi_trader.time = clock  # the only patch: the engine's view of the clock

    out = tempfile.mkdtemp(prefix="parity_replay_")
    trader = kalshi_trader.KalshiTrader(out, meta["assets"])
    counts, events = collections.Counter(), collections.Counter()
    trace = gzip.open(args.trace, "wt") if args.trace else None
    for i, row in enumerate(calls):
        clock.t = row["t"]
        result = getattr(trader, row["call"])(*row["args"])
        counts[row["call"]] += 1
        for e in result or []:
            events[e["kind"]] += 1
        if trace and row["call"] == "step":
            coin = row["args"][0]
            c = trader.coins[coin]
            views = {}
            for n, a in trader.accounts.items():
                v = a.views.get(coin)
                if not a.params.get("anti"):
                    views[n] = None if v is None else [v["p_up"], v["p_model"], v["best"]["side"],
                                                       v["best"]["edge"], v["signal"]["bet"]]
            trace.write(json.dumps({"i": i, "coin": coin, "sigma2": c.sigma2, "offset": c.offset_pct,
                                    "views": views, "cash": {n: a.cash for n, a in trader.accounts.items()}}) + "\n")
    trader.save(force=True)
    if trace:
        trace.close()
        # The state this replay ended in, under the recorded clock: what another engine's replay
        # is compared with. The live run's own state file was saved on its own schedule, so its
        # equity figures belong to a different moment.
        shutil.copy(os.path.join(out, "kalshi_balance.json"), os.path.join(os.path.dirname(args.trace), "replay_state.json"))

    print(f"replayed {len(calls)} calls: {dict(counts)}")
    print(f"events produced: {dict(events) or 'none'}")
    ok = True
    for name in ("kalshi_trades.csv", "kalshi_exits.csv"):
        live, again = read_lines(os.path.join(args.folder, name)), read_lines(os.path.join(out, name))
        diffs = [i for i in range(max(len(live), len(again)))
                 if (live[i] if i < len(live) else None) != (again[i] if i < len(again) else None)]
        if diffs:
            ok = False
            i = diffs[0]
            print(f"{name}: DIFFERS. live {max(0, len(live) - 1)} rows, replay {max(0, len(again) - 1)}; first at line {i + 1}:")
            print(f"    live:   {live[i] if i < len(live) else '(missing)'}")
            print(f"    replay: {again[i] if i < len(again) else '(missing)'}")
        else:
            print(f"{name}: match ({max(0, len(live) - 1)} rows)")

    def cash(folder):
        try:
            with open(os.path.join(folder, "kalshi_balance.json")) as f:
                return {n: (a["cash"], a.get("bankruptcies", 0), a.get("retired", False))
                        for n, a in json.load(f)["accounts"].items()}
        except FileNotFoundError:
            return {}
    if cash(args.folder) != cash(out):
        ok = False
        print(f"ACCOUNTS DIFFER.\n  live   {cash(args.folder)}\n  replay {cash(out)}")
    else:
        print(f"final cash, bankruptcies and retirements match for {len(cash(out))} accounts")
    shutil.rmtree(out, ignore_errors=True)
    print("PARITY OK" if ok else "PARITY FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
