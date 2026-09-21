"""What are the odds of a coin being above a price, some minutes from now?

    python forecast.py                          # BTC, 45 minutes, a ladder around spot
    python forecast.py ETH 15                   # another coin, another horizon
    python forecast.py BTC 45 87000 87500       # your own levels

Reads the last hour of one-minute candles from Coinbase's public API and treats the next
stretch as a random walk from where the price is now. That is deliberately the dullest
possible model, and it is the honest one: over a month of recorded rounds the correlation
between this project's model disagreeing with the market and the market's next move came out
at +0.02, which is nothing (see the README). So no drift term is applied. What the volatility
does give you is a defensible *range*, which is worth having even when direction is not.

Two different questions get two different answers, and they are widely confused:

  finishes above    where the price ends up when the clock runs out. This is what a Kalshi
                    15-minute contract settles on, so it is also roughly what one should cost.
  touches           whether the price gets there at ANY point on the way. Always the larger
                    number -- a driftless walk touches a level about twice as often as it ends
                    beyond it, because it gets there and wanders back.
"""

import json
import math
import statistics
import sys
import urllib.request
from datetime import datetime, timezone

PRODUCTS = {"BTC": "BTC-USD", "ETH": "ETH-USD", "SOL": "SOL-USD",
            "XRP": "XRP-USD", "DOGE": "DOGE-USD"}
DECIMALS = {"BTC": 0, "ETH": 0, "SOL": 2, "XRP": 4, "DOGE": 6}


def candles(product, minutes=60):
    url = f"https://api.exchange.coinbase.com/products/{product}/candles?granularity=60"
    req = urllib.request.Request(url, headers={"User-Agent": "asset-cracker-forecast/1.0"})
    with urllib.request.urlopen(req, timeout=20) as r:
        rows = json.load(r)
    rows.sort(key=lambda c: c[0])  # time, low, high, open, close, volume
    return rows[-minutes:]


def normal_cdf(x):
    return 0.5 * (1 + math.erf(x / math.sqrt(2)))


def measure(rows):
    closes = [c[4] for c in rows]
    returns = [math.log(b / a) for a, b in zip(closes, closes[1:])]
    return {
        "spot": closes[-1],
        "open": closes[0],
        "high": max(c[2] for c in rows),
        "low": min(c[1] for c in rows),
        "volume": sum(c[5] for c in rows),
        "sd_minute": statistics.stdev(returns),
        "samples": len(returns),
        "at": datetime.fromtimestamp(rows[-1][0], timezone.utc).astimezone(),
    }


def odds(spot, sigma, level):
    """Chance of finishing above `level`, and of touching it on the way.

    The touch figure is the reflection principle for a driftless walk: the chance of the
    maximum exceeding a level is twice the chance of the endpoint exceeding it. It assumes
    continuous watching, so treat it as an upper bound.
    """
    z = math.log(level / spot) / sigma
    above = 1 - normal_cdf(z)
    beyond = above if level > spot else 1 - above
    return above, min(1.0, 2 * beyond)


def ladder(spot, sigma, decimals):
    """Levels spanning roughly a standard deviation and a half either way, rounded to
    something a person would actually quote."""
    step = 10 ** (math.floor(math.log10(spot * sigma)) )
    out, k = [], -6
    while k <= 6:
        out.append(round(spot / step + k) * step)
        k += 1
    return sorted({round(v, decimals) for v in out if v > 0})


def main():
    args = sys.argv[1:]
    coin = (args[0].upper() if args and args[0].upper() in PRODUCTS else "BTC")
    args = args[1:] if args and args[0].upper() in PRODUCTS else args
    horizon = int(args[0]) if args and args[0].isdigit() else 45
    args = args[1:] if args and args[0].isdigit() else args
    levels = [float(a) for a in args] if args else None

    m = measure(candles(PRODUCTS[coin]))
    dec = DECIMALS[coin]
    spot, sigma = m["spot"], m["sd_minute"] * math.sqrt(horizon)
    levels = levels or ladder(spot, sigma, dec)

    print(f"{coin}  spot ${spot:,.{dec}f}   as of {m['at']:%H:%M:%S} local")
    print(f"  last hour: {(spot / m['open'] - 1) * 100:+.2f}%, "
          f"${m['low']:,.{dec}f} to ${m['high']:,.{dec}f}, {m['volume']:,.1f} traded")
    print(f"  volatility {m['sd_minute'] * 100:.4f}%/min over {m['samples']} returns "
          f"-> {sigma * 100:.3f}% over {horizon} min  (+/- ${spot * sigma:,.{dec}f})\n")

    print(f"  {'level':>12s} {'vs spot':>9s} {'sigmas':>7s} {'finishes above':>15s} "
          f"{'touches':>9s}")
    for level in levels:
        above, touch = odds(spot, sigma, level)
        gap = level - spot
        z = math.log(level / spot) / sigma
        mark = "  <- spot" if abs(gap) < spot * sigma * 0.15 else ""
        print(f"  ${level:>11,.{dec}f} {gap:>+9,.{dec}f} {z:>+7.2f} "
              f"{above * 100:>14.1f}% {touch * 100:>8.1f}%{mark}")

    print(f"\n  'finishes above' is what a {horizon}-minute contract settles on, so it is "
          f"also\n  roughly what one should cost. 'touches' is the chance of getting there "
          f"at all.")
    print(f"  No drift is assumed: over a month of rounds, direction was unpredictable\n"
          f"  (correlation +0.02). Volatility from {m['samples']} returns is a thin sample "
          f"-- a quiet\n  hour understates it and a busy one overstates it.")


if __name__ == "__main__":
    main()
