"""Replay a recording through a fresh, unmodified engine and compare the trades.

Reads calls.jsonl from a folder made by record.py, builds a new KalshiTrader per coin in a
temporary folder, and makes the same calls in the same order. The engine reads the wall clock
internally (time.time() in its index-offset window), so the clock the engine sees is replaced
with the recorded time of each call. kalshi_trader.py itself is not edited.

Passes when the replayed trade CSV is identical, line for line, to the one the live run wrote,
and every account ends with the same cash.

Run it:
    python tools/parity/replay.py tools/parity/recordings/20260920_123000
    python tools/parity/replay.py <folder> --keep out/   # keep the replayed files
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

    def strftime(self, fmt, *args):
        return real_time.strftime(fmt, *(args or (real_time.localtime(self.t),)))


def read_lines(path):
    try:
        with open(path, newline="") as f:
            return f.read().splitlines()
    except FileNotFoundError:
        return []


def cash_by_account(path):
    try:
        with open(path) as f:
            return {n: a["cash"] for n, a in json.load(f)["accounts"].items()}
    except FileNotFoundError:
        return {}


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("folder")
    ap.add_argument("--keep", help="copy the replayed files here instead of discarding them")
    ap.add_argument("--trace", help="write what the engine thought after every step to this "
                                    ".jsonl.gz file, for comparing another engine against")
    args = ap.parse_args()

    path = os.path.join(args.folder, "calls.jsonl")
    opener = open
    if not os.path.exists(path):  # fixtures are stored compressed
        path, opener = path + ".gz", gzip.open
    with opener(path, "rt") as f:
        rows = [json.loads(line) for line in f if line.strip()]
    meta = rows[0]["meta"]
    calls = rows[1:]

    clock = ReplayClock()
    clock.t = rows[0]["t"]
    kalshi_trader.time = clock  # the only patch: the engine's view of the clock

    out = tempfile.mkdtemp(prefix="parity_replay_")
    traders, counts, events = {}, collections.Counter(), collections.Counter()
    for coin in meta["coins"]:
        a = meta["assets"][coin]
        traders[coin] = kalshi_trader.KalshiTrader(
            out, suffix=a["suffix"], offset_pct=a["index_offset_pct"],
            sd_pct=a["index_sd_pct"], default_sigma=a["default_sigma"])

    trace = gzip.open(args.trace, "wt") if args.trace else None
    for i, row in enumerate(calls):
        clock.t = row["t"]
        result = getattr(traders[row["coin"]], row["call"])(*row["args"])
        if trace and row["call"] == "step":
            t = traders[row["coin"]]
            views = {n: None if a.view is None else
                     [a.view["p_up"], a.view["p_model"], a.view["best"]["side"],
                      a.view["best"]["edge"], a.view["signal"]["bet"]]
                     for n, a in t.accounts.items()}
            trace.write(json.dumps({"i": i, "coin": row["coin"], "sigma2": t.sigma2,
                                    "offset": t.offset_pct, "views": views}) + "\n")
        counts[row["call"]] += 1
        for e in result or []:
            events[e["kind"]] += 1
    for t in traders.values():
        t.save(force=True)
    if trace:
        trace.close()

    print(f"replayed {len(calls)} calls: {dict(counts)}")
    print(f"trade events produced: {dict(events) or 'none'}")

    ok = True
    for coin in meta["coins"]:
        suffix = meta["assets"][coin]["suffix"]
        name = f"kalshi_trades{suffix}.csv"
        live, replayed = read_lines(os.path.join(args.folder, name)), read_lines(os.path.join(out, name))
        diffs = [i for i in range(max(len(live), len(replayed)))
                 if (live[i] if i < len(live) else None) != (replayed[i] if i < len(replayed) else None)]
        rows_live = max(0, len(live) - 1)
        if diffs:
            ok = False
            print(f"{coin}: TRADES DIFFER. live {rows_live} rows, replay {max(0, len(replayed) - 1)}; "
                  f"{len(diffs)} line(s) differ, first at line {diffs[0] + 1}:")
            i = diffs[0]
            print(f"    live:   {live[i] if i < len(live) else '(missing)'}")
            print(f"    replay: {replayed[i] if i < len(replayed) else '(missing)'}")
        else:
            print(f"{coin}: trades match ({rows_live} rows)")

        bal = f"kalshi_balance{suffix}.json"
        c_live, c_rep = cash_by_account(os.path.join(args.folder, bal)), cash_by_account(os.path.join(out, bal))
        if c_live != c_rep:
            ok = False
            print(f"{coin}: CASH DIFFERS. live {c_live} replay {c_rep}")
        else:
            print(f"{coin}: final cash matches for {len(c_live)} accounts")

    if args.keep:
        shutil.copytree(out, args.keep, dirs_exist_ok=True)
    shutil.rmtree(out, ignore_errors=True)
    print("PARITY OK" if ok else "PARITY FAILED")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
