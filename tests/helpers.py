"""Shared fixtures for the test suite.

Two things every test here needs. First, a trader wired to a throwaway folder, because the
engine writes four files as it runs and no test should touch the real ones. Second, the
app's own ASSETS table, lifted out by text rather than imported: importing asset_cracker
builds a Tk window, which cannot happen on a headless machine.
"""

import os
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

import kalshi_trader as kt  # noqa: E402  (after the path is set up)

APP = os.path.join(ROOT, "asset_cracker.py")


def app_source():
    with open(APP, encoding="utf-8") as f:
        return f.read()


def app_value(name, src=None):
    """Pull one top-level assignment out of the app without importing it.

    `name` is the assignment to return, e.g. "ASSETS". The statement is taken line by line
    until its brackets balance, so it works whether the value is written on one line or
    spread over twenty, then executed in an empty namespace. The test sees exactly what the
    app defines, so the two cannot drift apart without a test noticing.
    """
    src = src or app_source()
    lines = src[src.index(f"\n{name} = ") + 1:].splitlines(keepends=True)
    stmt, depth = "", 0
    for line in lines:
        stmt += line
        # good enough for literals: the app has no brackets inside strings in these tables
        depth += sum(line.count(o) - line.count(c) for o, c in ("{}", "[]", "()"))
        if depth <= 0:
            break
    ns = {}
    exec(stmt, ns)
    return ns[name]


def new_trader(coins=None, quiet=True):
    """A trader on a fresh temp folder. `quiet` stops it writing the balance JSON on every
    event, which tests do not need and which dominates their runtime."""
    t = kt.KalshiTrader(tempfile.mkdtemp(prefix="ac_test_"), coins or {"BTC": {}})
    if quiet:
        t.save = lambda *a, **k: None
    return t


def market(coin="BTC", ticker=None, strike=80000.0, close=1_800_000_900.0,
           yes_bid=0.44, yes_ask=0.45, size=99999):
    """One coin's order book, shaped the way the live feed hands it over. The no side is
    the mirror of the yes side, as Kalshi quotes it."""
    return {"ticker": ticker or f"KX{coin}15M-TEST", "coin": coin, "strike": strike,
            "close": close, "yes_bid": yes_bid, "yes_ask": yes_ask,
            "no_bid": round(1 - yes_ask, 3), "no_ask": round(1 - yes_bid, 3),
            "yes_ask_size": size, "no_ask_size": size}


def run_round(trader, coin, prices, mkt, step=1):
    """Feed a round tick by tick. `prices` is one price per step, ending at the close."""
    close = mkt["close"]
    start = close - len(prices) * step
    for i, px in enumerate(prices):
        ts = start + i * step
        trader.observe(coin, px, ts)
        trader.step(coin, mkt, px, ts)


def lagging_book_round(trader, coin="BTC", ticker="KXBTC15M-LAG", strike=80000.0,
                       close=1_800_000_900.0):
    """A round whose book is priced off where the coin was ninety seconds ago.

    Real books lag. Without that lag a synthetic book and the model share a formula, there
    is no edge, and no strategy opens a position -- which makes it impossible to test
    anything about selling. Returns the trader, having run the whole round.
    """
    import math

    def path(step):
        return 79940.0 + 260.0 * (step / 870) + 9.0 * math.sin(step / 47.0)

    for k in range(200):  # prime the volatility estimate
        trader.observe(coin, path(0) * (1 + (k % 5 - 2) * 4e-6), close - 900 - 200 + k)
    for step in range(0, 880, 2):
        now = close - 900 + step
        px, lagged = path(step), path(max(0, step - 90))
        tau = max(1.0, close - now)
        sd = max(1e-6, px * kt.DEFAULT_SIGMA * math.sqrt(tau))
        yes = min(0.97, max(0.03, round(kt.norm_cdf((lagged - strike) / sd), 2)))
        trader.observe(coin, px, now)
        trader.step(coin, market(coin, ticker, strike, close,
                                 round(yes - 0.005, 2), round(yes + 0.005, 2)), px, now)
    return trader


def cash_balances(acct):
    """What the account's own history says its cash should be. Every test that moves money
    checks this: start, minus what every bet cost, plus everything that paid out."""
    spent = sum(lot["cost"] for lot in acct.log)
    paid = sum(lot.get("payout", 0.0) for lot in acct.log if lot["status"] != "open")
    return kt.START_BALANCE - spent + paid
