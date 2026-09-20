"""A week of Kalshi Bitcoin-related markets, with volume, for price/manipulation analysis.

Collects three layers:
  1. KXBTC15M  - the 15-minute up/down rounds (per-minute volume from candlesticks)
  2. KXBTC     - the hourly BRACKET markets (volume + open interest per price bracket)
  3. KXBTCD    - the hourly above/below strikes
plus the 15-minute rounds for the other candidate coins, so the app can trade up to five.

Bulk market lists are cheap (1000 per page) and already carry volume_fp / open_interest_fp,
so per-bracket volume needs no candlestick call. Candlesticks are fetched only for the
15-minute rounds, where per-minute volume is the point.
"""
import json, os, sys, time, urllib.request
from collections import defaultdict
from datetime import datetime, timezone

KAL = "https://api.elections.kalshi.com/trade-api/v2"
HERE = os.path.dirname(os.path.abspath(__file__))
DAYS = 7
COINS = {"BTC": "KXBTC15M", "ETH": "KXETH15M", "SOL": "KXSOL15M",
         "XRP": "KXXRP15M", "DOGE": "KXDOGE15M"}


def log(*a):
    print(*a, flush=True)


def get(url):
    for a in range(6):
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "kalshi-week/1.0"})
            with urllib.request.urlopen(req, timeout=30) as r:
                return json.load(r)
        except Exception:
            time.sleep(1 + a * 1.5)
    return {}


ts = lambda s: datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()
cutoff = time.time() - DAYS * 86400


def all_settled(series, cap=6000):
    """Every settled market for a series inside the window, paging the bulk list."""
    out, cursor = [], ""
    while len(out) < cap:
        r = get(f"{KAL}/markets?series_ticker={series}&status=settled&limit=1000"
                + (f"&cursor={cursor}" if cursor else ""))
        got = r.get("markets", [])
        if not got:
            break
        keep = [m for m in got if ts(m["close_time"]) >= cutoff]
        out += keep
        cursor = r.get("cursor") or ""
        if not cursor or len(keep) < len(got):
            break
        time.sleep(0.1)
    return out


def slim(m):
    """Just the fields the analysis needs."""
    num = lambda k: float(m.get(k) or 0)
    return {
        "ticker": m["ticker"], "event": m.get("event_ticker"),
        "open": ts(m["open_time"]), "close": ts(m["close_time"]),
        "floor": m.get("floor_strike"), "cap": m.get("cap_strike"),
        "label": m.get("yes_sub_title", ""), "result": m.get("result"),
        "final": m.get("expiration_value"),
        "volume": num("volume_fp"), "oi": num("open_interest_fp"),
        "yes_bid": num("yes_bid_dollars"), "yes_ask": num("yes_ask_dollars"),
    }


out = {"scraped_at": time.time(), "days": DAYS, "coins": {}, "hourly_brackets": [], "hourly_ab": []}

# --- layer 1: 15-minute rounds per coin, with per-minute volume -----------------------
for coin, series in COINS.items():
    mk = all_settled(series)
    log(f"{coin}: {len(mk)} settled 15-minute rounds in the last {DAYS} days")
    rounds = []
    for i, m in enumerate(mk):
        row = slim(m)
        cs = get(f"{KAL}/series/{series}/markets/{m['ticker']}/candlesticks"
                 f"?start_ts={int(row['open'])}&end_ts={int(row['close'])}&period_interval=1")
        bars = []
        for b in cs.get("candlesticks", []):
            try:
                bars.append([b["end_period_ts"],
                             float(b["yes_bid"]["close_dollars"]),
                             float(b["yes_ask"]["close_dollars"]),
                             float(b.get("volume_fp") or 0),
                             float(b.get("open_interest_fp") or 0)])
            except (KeyError, TypeError, ValueError):
                pass
        row["bars"] = bars
        rounds.append(row)
        if i % 100 == 0:
            log(f"   {coin} {i}/{len(mk)}")
            json.dump(out, open(os.path.join(HERE, "week_partial.json"), "w"))
        time.sleep(0.09)
    out["coins"][coin] = rounds
    json.dump(out, open(os.path.join(HERE, "week_partial.json"), "w"))
    log(f"{coin}: done")

# --- layer 2 & 3: hourly brackets and above/below (volume comes with the list) ---------
for key, series in (("hourly_brackets", "KXBTC"), ("hourly_ab", "KXBTCD")):
    mk = all_settled(series)
    out[key] = [slim(m) for m in mk]
    log(f"{series}: {len(mk)} settled markets "
        f"({len({m['event'] for m in out[key]})} events)")

# --- Coinbase spot for every coin, one minute at a time -------------------------------
PRODUCTS = {"BTC": "BTC-USD", "ETH": "ETH-USD", "SOL": "SOL-USD", "XRP": "XRP-USD", "DOGE": "DOGE-USD"}
iso = lambda v: datetime.fromtimestamp(v, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
out["spot"] = {}
lo = cutoff - 7200
hi = time.time() + 120
for coin, product in PRODUCTS.items():
    px, t = {}, lo
    while t < hi:
        e = min(t + 300 * 60, hi)
        for row in get(f"https://api.exchange.coinbase.com/products/{product}/candles"
                       f"?granularity=60&start={iso(t)}&end={iso(e)}") or []:
            px[int(row[0])] = [row[3], row[4], row[5]]  # open, close, volume
        t = e
        time.sleep(0.14)
    out["spot"][coin] = px
    log(f"spot {coin}: {len(px)} minutes")

json.dump(out, open(os.path.join(HERE, "week_data.json"), "w"))
log(f"\nDONE -> week_data.json ({os.path.getsize(os.path.join(HERE, 'week_data.json')):,} bytes)")
