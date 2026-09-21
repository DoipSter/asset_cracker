"""Record everything the v2 trading engine is told during a live run.

v2 is kalshi_trader.py as of main dc10fd4: one KalshiTrader for every coin, one balance per
strategy shared across coins, and an anti-world twin for each strategy. (The harness for the
first version, one trader per coin, is in v1/ and needs the engine at ed05fc2.)

Runs the unmodified engine from the live feeds with no window, wired the way Monitor wires it
in asset_cracker.py, and logs every call into the engine to calls.jsonl with the wall-clock time
it was made. replay.py feeds that log back through a fresh engine.

    python tools/parity/record.py --minutes 45
    python tools/parity/record.py --minutes 45 --coins BTC,ETH

Simulation only: public reads, no account, no orders.
"""

import argparse
import collections
import json
import os
import queue
import re
import sys
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
sys.path.insert(0, ROOT)

from kalshi_trader import KalshiTrader  # noqa: E402

RECORDED = ("observe", "seed_vol", "seed_offsets", "step", "note_settlement", "on_settled")


def load_app():
    """asset_cracker builds nothing at import, but it does import tkinter and ctypes.wintypes.
    Both import on macOS and Linux, so it can be used for its feeds without opening a window."""
    import asset_cracker
    return asset_cracker


class Feed:
    """One coin's feeds, queued for the main thread as Monitor does."""

    def __init__(self, app, asset, trader, log):
        self.app, self.asset, self.coin, self.trader, self.log = app, asset, asset["coin"], trader, log
        self.results = queue.Queue()
        self.price = None
        self._clock_offsets = collections.deque(maxlen=200)

    def now(self):
        offsets = list(self._clock_offsets)
        return time.time() + (max(offsets) if offsets else 0)

    def start(self):
        app, asset = self.app, self.asset

        def on_price(price, exchange_ts):
            if exchange_ts:
                self._clock_offsets.append(exchange_ts - time.time())
            self.results.put(("price", price, exchange_ts))

        def seed():
            try:
                rows = app._get_json("/candles?granularity=60", asset["product"])
                rows.sort(key=lambda r: r[0])
                self.results.put(("seed", [(r[0] + 60, r[4]) for r in rows[-50:]]))
            except Exception:
                pass

        def offsets():
            try:
                found = app.fetch_recent_offsets(asset["series"], asset["product"])
                if found:
                    self.results.put(("offsets", found))
            except Exception:
                pass

        for target, args in (
            (app.stream_prices, (on_price, lambda: self.results.put(("offline",)), asset["product"])),
            (app.stream_kalshi, (lambda m: self.results.put(("market", m)),
                                 lambda t, r, v, c: self.results.put(("settled", t, r, v, c)),
                                 lambda: self.trader.pending_tickers(self.coin), self.now, asset["series"])),
            (seed, ()), (offsets, ()),
        ):
            threading.Thread(target=target, args=args, daemon=True).start()

    def pump(self, call):
        latest = None
        try:
            while True:
                msg = self.results.get_nowait()
                if msg[0] == "price":
                    latest = msg  # bursts: only the newest reaches the engine
                else:
                    self.handle(msg, call)
        except queue.Empty:
            pass
        if latest:
            self.handle(latest, call)

    def handle(self, msg, call):
        kind = msg[0]
        if kind == "price":
            self.price = msg[1]
            call("observe", self.coin, self.price, msg[2] or self.now())
        elif kind == "seed":
            call("seed_vol", self.coin, [p for t, p in msg[1] if t <= self.now()])
        elif kind == "market":
            if self.price:
                call("step", self.coin, msg[1], self.price, self.now())
            self.trader.save()
        elif kind == "settled":
            _, ticker, result, final, close = msg
            call("note_settlement", self.coin, close, final)
            call("on_settled", ticker, result, final, self.now(), self.price)
        elif kind == "offsets":
            call("seed_offsets", self.coin, [list(pair) for pair in msg[1]])


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--minutes", type=float, default=45.0)
    ap.add_argument("--coins", default="")
    ap.add_argument("--out", default=os.path.join(ROOT, "tools", "parity", "recordings"))
    args = ap.parse_args()

    app = load_app()
    coins = [c.strip().upper() for c in args.coins.split(",") if c.strip()] or list(app.ASSETS)
    assets = {c: app.ASSETS[c] for c in coins}
    folder = os.path.join(args.out, time.strftime("%Y%m%d_%H%M%S") + "_v2")
    os.makedirs(folder)

    counts, events = collections.Counter(), collections.Counter()
    with open(os.path.join(folder, "calls.jsonl"), "w") as log:
        started = time.time()
        log.write(json.dumps({"t": started, "meta": {"engine": "v2", "coins": coins,
                  "python": sys.version.split()[0], "assets": assets}}) + "\n")
        trader = KalshiTrader(folder, assets)

        def call(name, *a):
            assert name in RECORDED
            log.write(json.dumps({"t": time.time(), "call": name, "args": list(a)}) + "\n")
            counts[name] += 1
            for e in getattr(trader, name)(*a) or []:
                events[e["kind"]] += 1

        feeds = [Feed(app, assets[c], trader, log) for c in coins]
        for f in feeds:
            f.start()
        print(f"recording v2, {', '.join(coins)}, {args.minutes:g} min -> {folder}", flush=True)
        stop_at, report_at = time.monotonic() + args.minutes * 60, time.monotonic() + 60
        try:
            while time.monotonic() < stop_at:
                for f in feeds:
                    f.pump(call)
                if time.monotonic() >= report_at:
                    report_at += 60
                    log.flush()
                    print(f"  calls {dict(counts)} events {dict(events)}", flush=True)
                time.sleep(0.008)
        except KeyboardInterrupt:
            print("stopped early")
        trader.save(force=True)
    print(f"done. calls {dict(counts)} events {dict(events)}")
    print(f"replay it with: python tools/parity/replay.py {folder}")


if __name__ == "__main__":
    main()
