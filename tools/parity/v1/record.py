"""Record everything the trading engine is told during a live run.

Runs the unmodified engine (kalshi_trader.py) from the live feeds with no window, wired the
same way Monitor wires it in asset_cracker.py, and logs every call into the engine to
calls.jsonl with the wall-clock time it was made. replay.py feeds that log back through a
fresh engine; if the engine is deterministic, it reproduces the same trade CSV.

Run it:
    python tools/parity/record.py --minutes 30
    python tools/parity/record.py --minutes 30 --coins BTC

Each run writes to a new folder under tools/parity/recordings/. Simulation only: it reads
public data and places no orders, exactly like the app.
"""

import argparse
import collections
import json
import os
import queue
import sys
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
sys.path.insert(0, ROOT)

import asset_cracker as app  # noqa: E402  (the feeds live here; importing opens no window)
from kalshi_trader import KalshiTrader  # noqa: E402

# The engine methods that take input from outside. Everything else is derived from these.
RECORDED = ("observe", "seed_vol", "seed_offsets", "step", "note_settlement", "on_settled")


class Recorder:
    """One coin: the engine, its feeds, and the log of calls made into the engine."""

    def __init__(self, asset, folder, log, lock):
        self.asset = asset
        self.coin = asset["coin"]
        self.log, self.lock = log, lock
        self.trader = KalshiTrader(
            folder, suffix=asset["suffix"], offset_pct=asset["index_offset_pct"],
            sd_pct=asset["index_sd_pct"], default_sigma=asset["default_sigma"])
        self.results = queue.Queue()
        self.price = None
        self._clock_offsets = collections.deque(maxlen=200)  # exchange time - PC time
        self.counts = collections.Counter()
        self.events = collections.Counter()

    # ---- the same clock correction Monitor.now() applies -------------------

    def now(self):
        offsets = list(self._clock_offsets)
        return time.time() + (max(offsets) if offsets else 0)

    # ---- calling the engine, and writing down that we did ------------------

    def call(self, name, *args):
        assert name in RECORDED
        row = {"t": time.time(), "coin": self.coin, "call": name, "args": list(args)}
        with self.lock:
            self.log.write(json.dumps(row) + "\n")
        self.counts[name] += 1
        out = getattr(self.trader, name)(*args)
        for e in out or []:
            self.events[e["kind"]] += 1
        return out

    # ---- feeds, started exactly as Monitor starts them ---------------------

    def start(self):
        def on_price(price, exchange_ts):
            if exchange_ts:
                self._clock_offsets.append(exchange_ts - time.time())
            self.results.put(("price", price, exchange_ts))

        def seed():
            try:
                rows = app._get_json("/candles?granularity=60", self.asset["product"])
                rows.sort(key=lambda r: r[0])
                self.results.put(("seed", [(r[0] + 60, r[4]) for r in rows[-50:]]))
            except Exception:
                pass

        def offsets():
            try:
                found = app.fetch_recent_offsets(self.asset["series"], self.asset["product"])
                if found:
                    self.results.put(("offsets", found))
            except Exception:
                pass

        threads = [
            (app.stream_prices, (on_price, lambda: self.results.put(("offline",)),
                                 self.asset["product"])),
            (app.stream_kalshi, (lambda m: self.results.put(("market", m)),
                                 lambda t, r, v, c: self.results.put(("settled", t, r, v, c)),
                                 self.trader.pending_tickers, self.now, self.asset["series"])),
            (seed, ()),
            (offsets, ()),
        ]
        for target, args in threads:
            threading.Thread(target=target, args=args, daemon=True).start()

    # ---- Monitor._pump and Monitor._handle, minus the drawing --------------

    def pump(self):
        latest_price = None
        try:
            while True:
                msg = self.results.get_nowait()
                if msg[0] == "price":
                    latest_price = msg  # bursts: only the newest reaches the engine
                else:
                    self.handle(msg)
        except queue.Empty:
            pass
        if latest_price:
            self.handle(latest_price)

    def handle(self, msg):
        kind = msg[0]
        if kind == "price":
            self.price = msg[1]
            self.call("observe", self.price, msg[2] or self.now())
        elif kind == "seed":
            done = [p for t, p in msg[1] if t <= self.now()]
            self.call("seed_vol", done)
        elif kind == "market":
            if self.price:
                self.call("step", msg[1], self.price, self.now())
            self.trader.save()
        elif kind == "settled":
            _, ticker, result, final, close = msg
            self.call("note_settlement", close, final)
            self.call("on_settled", ticker, result, final, self.now(), self.price)
        elif kind == "offsets":
            self.call("seed_offsets", [list(pair) for pair in msg[1]])


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--minutes", type=float, default=30.0)
    ap.add_argument("--coins", default="BTC,ETH")
    ap.add_argument("--out", default=os.path.join(ROOT, "tools", "parity", "recordings"))
    args = ap.parse_args()

    folder = os.path.join(args.out, time.strftime("%Y%m%d_%H%M%S"))
    os.makedirs(folder)
    coins = [c.strip().upper() for c in args.coins.split(",") if c.strip()]
    lock = threading.Lock()
    with open(os.path.join(folder, "calls.jsonl"), "w") as log:
        meta = {"t": time.time(), "meta": {
            "coins": coins, "python": sys.version.split()[0],
            "assets": {c: app.ASSETS[c] for c in coins}}}
        log.write(json.dumps(meta) + "\n")
        recorders = [Recorder(app.ASSETS[c], folder, log, lock) for c in coins]
        for r in recorders:
            r.start()
        print(f"recording {', '.join(coins)} for {args.minutes:g} min -> {folder}", flush=True)
        stop_at = time.monotonic() + args.minutes * 60
        report_at = time.monotonic() + 60
        try:
            while time.monotonic() < stop_at:
                for r in recorders:
                    r.pump()
                if time.monotonic() >= report_at:
                    report_at += 60
                    log.flush()
                    for r in recorders:
                        print(f"  {r.coin}: calls {dict(r.counts)} events {dict(r.events)}",
                              flush=True)
                time.sleep(0.008)  # Monitor pumps every 8 ms
        except KeyboardInterrupt:
            print("stopped early")
        for r in recorders:
            r.trader.save(force=True)

    print("done.")
    for r in recorders:
        print(f"  {r.coin}: calls {dict(r.counts)} events {dict(r.events)}")
    print(f"replay it with: python tools/parity/replay.py {folder}")


if __name__ == "__main__":
    main()
