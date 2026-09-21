"""Browse past runs of the app.

    python sessions.py                 # every run, newest first
    python sessions.py 20260920_234512 # one run in detail
    python sessions.py last            # the most recent run

Reads the files the app writes next to itself. Nothing here talks to a network or changes
anything, so it is safe to run while the app is going.
"""

import collections
import csv
import os
import sys

# The app keeps its files beside itself. Running from a clone, look next door for the
# deployed copy, since that is where the real history accumulates.
HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CANDIDATES = [HERE, os.path.join(os.path.dirname(HERE), "asset_cracker")]


def data_dir():
    for folder in CANDIDATES:
        if os.path.exists(os.path.join(folder, "kalshi_sessions.csv")):
            return folder
    for folder in CANDIDATES:
        if os.path.exists(os.path.join(folder, "kalshi_trades.csv")):
            return folder
    sys.exit("no logs found in:\n  " + "\n  ".join(CANDIDATES))


FOLDER = data_dir()


def rows(name):
    path = os.path.join(FOLDER, name)
    if not os.path.exists(path):
        return []
    with open(path, newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))


def money(x):
    try:
        return float(x)
    except (TypeError, ValueError):
        return 0.0


def list_sessions():
    sessions = rows("kalshi_sessions.csv")
    if not sessions:
        print(f"No session index in {FOLDER}.\n"
              "Rows written before sessions were recorded have no run attached to them; "
              "they are still in\nkalshi_trades.csv, just not separable.")
        return
    print(f"{len(sessions)} run(s) in {FOLDER}\n")
    print(f"  {'session':17s} {'started':>17s} {'mins':>6s} {'bank':>8s} {'rounds':>7s} "
          f"{'bets':>6s} {'best':>16s} {'balance':>10s} {'bust':>5s}")
    for r in sorted(sessions, key=lambda x: x["session"], reverse=True):
        print(f"  {r['session']:17s} {r['started'][5:16]:>17s} "
              f"{r['minutes']:>6s} {money(r['bank']):8,.0f} {r['rounds']:>7s} "
              f"{r['bets']:>6s} {r['best']:>16s} {money(r['best_balance']):10,.2f} "
              f"{r['bankruptcies']:>5s}")
    print("\n  python sessions.py <session>   for one run in detail")


def detail(session):
    index = {r["session"]: r for r in rows("kalshi_sessions.csv")}
    trades = [r for r in rows("kalshi_trades.csv") if r.get("session") == session]
    exits = [r for r in rows("kalshi_exits.csv") if r.get("session") == session]
    rounds_ = [r for r in rows("kalshi_rounds.csv") if r.get("session") == session]
    if not (trades or rounds_ or session in index):
        sys.exit(f"nothing recorded for session {session}")

    head = index.get(session)
    print(f"Session {session}")
    if head:
        print(f"  {head['started']} to {head['last_seen']}  ({head['minutes']} minutes)")
        print(f"  ${money(head['bank']):,.0f} each, ${money(head['round_cap']):,.0f} at risk "
              f"per round, {head['strategies']} strategies over {head['coins']}")
        print(f"  {head['rounds']} rounds monitored, {head['bets']} bets, "
              f"{head['bankruptcies']} bankruptcies")

    if trades:
        print(f"\n  {'strategy':16s} {'bets':>5s} {'won':>5s} {'lost':>5s} {'sold':>5s} "
              f"{'staked':>10s} {'ending balance':>15s}")
        by = collections.defaultdict(list)
        for r in trades:
            by[r["strategy"]].append(r)
        last_balance = {}
        for r in trades:
            if r.get("balance_after"):
                last_balance[r["strategy"]] = money(r["balance_after"])
        for name in sorted(by, key=lambda n: -last_balance.get(n, 0.0)):
            rs = by[name]
            staked = sum(money(r["cost"]) for r in rs if r["event"] == "BET")
            counts = collections.Counter(
                r["result"] if r["event"] == "SETTLED" else r["event"] for r in rs)
            print(f"  {name:16s} {counts['BET']:5d} {counts['yes'] + counts['no']:5d} "
                  f"{'':5s} {counts['SOLD']:5d} {staked:10,.2f} "
                  f"{last_balance.get(name, 0.0):15,.2f}")

    if exits:
        gave = sum(money(r["gave_up"]) for r in exits)
        held_better = sum(1 for r in exits if money(r["gave_up"]) > 0)
        print(f"\n  {len(exits)} early sales graded against holding: holding would have paid "
              f"more in {held_better},")
        print(f"  for ${gave:+,.2f} overall (positive means selling cost it).")

    if rounds_:
        skipped = sum(1 for r in rounds_ if r["bets"] == "0")
        gaps = sorted(abs(money(r["index_gap_pct"])) for r in rounds_
                      if r.get("index_gap_pct"))
        print(f"\n  {len(rounds_)} rounds logged, {skipped} with no bet at all")
        if gaps:
            print(f"  index estimate vs Kalshi's settled value: median "
                  f"{gaps[len(gaps) // 2]:.4f}%, worst {gaps[-1]:.4f}%")

    reports = [f for f in os.listdir(FOLDER)
               if f.startswith("kalshi_bankrupt_") and f.endswith(".md")]
    if reports:
        print(f"\n  postmortems in this folder: {', '.join(sorted(reports))}")


def main():
    arg = sys.argv[1] if len(sys.argv) > 1 else None
    if not arg:
        return list_sessions()
    if arg == "last":
        sessions = rows("kalshi_sessions.csv")
        if not sessions:
            sys.exit("no session index yet")
        arg = max(r["session"] for r in sessions)
    detail(arg)


if __name__ == "__main__":
    main()
