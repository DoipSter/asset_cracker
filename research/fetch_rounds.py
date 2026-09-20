"""Download a month of one coin's settled 15-minute rounds, with per-minute quotes, plus
Coinbase's per-minute prices over the same span.

    python fetch_rounds.py KXBTC15M BTC-USD bt30_btc.json

Takes about twenty minutes and writes ~2.6 MB. Resumable: rerunning it keeps the rounds
already downloaded and fetches only what is missing, so an interrupted run costs nothing.

The output feeds backtest_exits.py. It is gitignored -- it is a cache of a public API, not
source, and anyone can rebuild it with this script.
"""

import json
import os
import sys
import time
import urllib.request
from datetime import datetime, timezone

KALSHI = "https://api.elections.kalshi.com/trade-api/v2"
COINBASE = "https://api.exchange.coinbase.com"
DAYS = 31
UA = {"User-Agent": "asset-cracker-research/1.0"}


def log(*a):
    print(*a, flush=True)


def get(url):
    """Kalshi and Coinbase both rate-limit and both blip. Back off and retry rather than
    losing a twenty-minute download to one timeout."""
    for attempt in range(8):
        try:
            with urllib.request.urlopen(urllib.request.Request(url, headers=UA),
                                        timeout=30) as r:
                return json.load(r)
        except Exception:
            time.sleep(1 + attempt * 1.5)
    raise RuntimeError(f"gave up on {url}")


def epoch(iso_string):
    return datetime.fromisoformat(iso_string.replace("Z", "+00:00")).timestamp()


def settled_rounds(series, cutoff):
    """Every settled round in the window, newest first, one page at a time."""
    markets, cursor = [], ""
    while True:
        page = get(f"{KALSHI}/markets?series_ticker={series}&status=settled&limit=1000"
                   + (f"&cursor={cursor}" if cursor else ""))
        got = page["markets"]
        markets += [m for m in got if epoch(m["close_time"]) >= cutoff]
        log(f"  listing: {len(got)} this page, {len(markets)} kept")
        cursor = page.get("cursor") or ""
        if not cursor or not got or epoch(got[-1]["close_time"]) < cutoff:
            return markets


def quotes(series, markets, cache, out_path):
    """One minute-by-minute quote series per round. Saved every hundred rounds so an
    interrupted run keeps its progress."""
    have = {m["ticker"] for m in cache["markets"]}
    out = list(cache["markets"])
    todo = [m for m in markets if m["ticker"] not in have]
    log(f"  {len(have)} already cached, {len(todo)} to fetch")
    for i, m in enumerate(todo):
        opened, closed = epoch(m["open_time"]), epoch(m["close_time"])
        cs = get(f"{KALSHI}/series/{series}/markets/{m['ticker']}/candlesticks"
                 f"?start_ts={int(opened)}&end_ts={int(closed)}&period_interval=1")
        bars = []
        for b in cs.get("candlesticks", []):
            try:
                bars.append((b["end_period_ts"],
                             float(b["yes_bid"]["close_dollars"]),
                             float(b["yes_ask"]["close_dollars"])))
            except (KeyError, TypeError, ValueError):
                pass  # a bar with no trades has no prices; skipping it is correct
        out.append({"ticker": m["ticker"], "open": opened, "close": closed,
                    "strike": m.get("floor_strike"), "result": m["result"],
                    "final": m.get("expiration_value"), "bars": bars})
        if i % 100 == 0:
            log(f"  quotes {i}/{len(todo)}")
            save(out_path, out, cache["px"])
        time.sleep(0.12)
    return out


def prices(product, rounds):
    """Coinbase's one-minute closes across the whole span, 300 minutes per request.

    Each key is the START of its candle, so the price AT time t is the candle keyed t-60.
    Reading px[t] as "the price at t" leaks the next minute into the past -- a look-ahead
    bug that once made a losing strategy backtest as a winner.
    """
    start = int(min(r["open"] for r in rounds)) - 7200
    end = int(max(r["close"] for r in rounds)) + 120
    iso = lambda v: datetime.fromtimestamp(v, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    px, t = {}, start
    while t < end:
        stop = min(t + 300 * 60, end)
        for row in get(f"{COINBASE}/products/{product}/candles"
                       f"?granularity=60&start={iso(t)}&end={iso(stop)}"):
            px[int(row[0])] = row[4]
        t = stop
        time.sleep(0.15)
    return px


def save(path, markets, px):
    with open(path, "w") as f:
        json.dump({"markets": markets, "px": {str(k): v for k, v in px.items()}}, f)


def main():
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    series, product, name = sys.argv[1:4]
    out_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), name)
    cache = {"markets": [], "px": {}}
    if os.path.exists(out_path):
        with open(out_path) as f:
            cache = json.load(f)

    log(f"listing settled {series} rounds from the last {DAYS} days")
    markets = settled_rounds(series, time.time() - DAYS * 86400)
    log(f"{len(markets)} rounds")

    rounds = quotes(series, markets, cache, out_path)
    log(f"fetching {product} minute prices")
    px = prices(product, rounds)
    log(f"{len(px)} minutes")
    save(out_path, rounds, px)
    log(f"wrote {out_path}")


if __name__ == "__main__":
    main()
