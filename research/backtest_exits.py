"""Drive the real trading engine over recorded rounds, to compare exit rules.

    python fetch_rounds.py KXBTC15M BTC-USD bt30_btc.json   # once, ~20 min
    python backtest_exits.py                                # the full month
    python backtest_exits.py 672                            # the last seven days

This is what produced the numbers in the commit that added take_capture. It is here so
those numbers can be checked rather than taken on trust.

Two caveats that the output repeats, because they matter when reading it:

  * The cache holds one-minute bars, so a strategy gets about fourteen decision points per
    round instead of the hundred-odd it sees live. That understates how much it trades --
    equally in every run, so the comparison holds even though the absolute P/L does not.
  * Fills assume the displayed price plus a cent of slippage. On a thin book you would do
    worse, so every balance here is a mildly optimistic upper bound.
"""

import copy
import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
import kalshi_trader as kt  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
CACHE = os.path.join(HERE, "bt30_btc.json")


def load():
    if not os.path.exists(CACHE):
        sys.exit(f"no data at {CACHE}\n"
                 f"build it first:  python fetch_rounds.py KXBTC15M BTC-USD bt30_btc.json")
    with open(CACHE) as f:
        data = json.load(f)
    px = {int(k): v for k, v in data["px"].items()}
    return sorted(data["markets"], key=lambda m: m["close"]), px


def run(rounds, px, strategy="Scalper", **params):
    """Replay every round through the real engine, with the strategy's parameters
    overridden. Passing capture=None removes take_capture entirely, which is how the
    engine behaved before it existed."""
    t = kt.KalshiTrader(tempfile.mkdtemp(prefix="ac_bt_"), {"BTC": {}})
    t.save = lambda *a, **k: None
    t._append_csv = lambda *a, **k: None
    t._log_exits = lambda *a, **k: None
    t._log_round = lambda *a, **k: None
    acct = t.accounts[strategy]
    acct.params = copy.deepcopy(acct.params)
    for key, value in params.items():
        if value is None:
            acct.params.pop(key, None)
        else:
            acct.params[key] = value

    for m in rounds:
        bars = m.get("bars") or []
        if len(bars) < 6 or not m.get("result"):
            continue
        for ts, yes_bid, yes_ask in bars:
            # px is keyed by the START of each candle, so the price AT ts is the candle
            # keyed ts-60. Using px[ts] would leak the next minute into the past.
            price = px.get(int(ts) - 60)
            if not price or ts >= m["close"]:  # the closing bar is the settlement itself
                continue
            t.observe("BTC", price, ts)
            t.step("BTC", {"ticker": m["ticker"], "coin": "BTC", "strike": m["strike"],
                           "close": m["close"], "yes_bid": yes_bid, "yes_ask": yes_ask,
                           "no_bid": round(1 - yes_ask, 3), "no_ask": round(1 - yes_bid, 3),
                           "yes_ask_size": 99999, "no_ask_size": 99999}, price, ts)
        t.on_settled(m["ticker"], m["result"], m.get("final"), m["close"],
                     px.get(int(m["close"]) - 60) or m["strike"])
    return acct


def report(acct, label):
    sold = [l for l in acct.log if l["status"] == "sold"]
    gains = [l for l in sold if l["pnl"] > 0]
    spent = sum(l["cost"] for l in acct.log)
    paid = sum(l.get("payout", 0.0) for l in acct.log if l["status"] != "open")
    expected = kt.START_BALANCE - spent + paid
    assert abs(acct.cash - expected) < 0.01, \
        f"{label}: cash {acct.cash:.2f} but the log says {expected:.2f}"
    print(f"  {label:22s} {len(acct.log):5d} bets  {len(sold):5d} sold "
          f"({len(gains):5d} at a profit)  {acct.wins:5d} won   "
          f"P/L ${acct.cash - kt.START_BALANCE:+9.2f}")


def main():
    rounds, px = load()
    n = int(sys.argv[1]) if len(sys.argv) > 1 else len(rounds)
    rounds = rounds[-n:]
    print(f"Scalper over {len(rounds)} recorded BTC rounds "
          f"({len(rounds) * 15 / 60 / 24:.1f} days), one-minute bars\n")
    report(run(rounds, px, take_capture=None, min_hold=None), "value exits only")
    for capture in (0.25, 0.50, 0.80, 0.95):
        report(run(rounds, px, take_capture=capture), f"take_capture {capture:.2f}")
    print("\nSelling early always costs something in hindsight: a position deep enough to\n"
          "trigger a capture exit usually goes on to win. The question is whether the\n"
          "reversals it avoids are worth the upside it clips.")


if __name__ == "__main__":
    main()
