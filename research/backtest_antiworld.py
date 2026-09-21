"""Does doing the opposite work?

    python fetch_rounds.py KXBTC15M BTC-USD bt30_btc.json   # once, ~20 min
    python backtest_antiworld.py

Every strategy loses money. Either the model is systematically WRONG, in which case taking
the other side should pay; or the model is roughly right and the LOSSES ARE COSTS -- the
spread and Kalshi's fee on every buy and every sell -- in which case both sides lose.

READ THE PAIR COLUMN CAREFULLY. A twin matches its original's STAKE, not its contract count,
because the two sides of a market are different prices and matching contracts would have the
twin committing well over twice the capital, straight through the exposure cap. The cost of
that choice is that a pair is not a hedge: equal money at 18c and at 85c buys very different
quantities, so each pair holds a standing long position in whichever side was cheaper. Over
this month the twins ended up with anywhere from 0.03x to 3.25x their original's contracts.

So a positive pair does NOT prove the model is backwards, and a negative one does not isolate
the fees. What a twin does show honestly is whether taking the other side, at the same risk,
would have done better -- which is the question the panel is there to answer.
"""

import json
import os
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)
import kalshi_trader as kt  # noqa: E402

CACHE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "bt30_btc.json")


def load():
    if not os.path.exists(CACHE):
        sys.exit(f"no data at {CACHE}\n"
                 f"build it first:  python fetch_rounds.py KXBTC15M BTC-USD bt30_btc.json")
    with open(CACHE) as f:
        data = json.load(f)
    return (sorted(data["markets"], key=lambda m: m["close"]),
            {int(k): v for k, v in data["px"].items()})


def run(rounds, px):
    t = kt.KalshiTrader(tempfile.mkdtemp(prefix="ac_anti_"), {"BTC": {}})
    t.save = t._append_csv = t._log_exits = t._log_round = lambda *a, **k: None
    # No reviving here. Live, a strategy that runs out is staked again so the
    # comparison keeps running; in a backtest that would inject fresh capital and
    # turn a wipe-out into an apparent profit. A dead strategy stays dead.
    t._check_broke = lambda *a, **k: []
    for m in rounds:
        bars = m.get("bars") or []
        if len(bars) < 6 or not m.get("result"):
            continue
        for ts, yes_bid, yes_ask in bars:
            # px is keyed by the START of each candle, so the price AT ts is the candle
            # keyed ts-60; reading px[ts] would leak the next minute into the past
            price = px.get(int(ts) - 60)
            if not price or ts >= m["close"]:
                continue
            t.observe("BTC", price, ts)
            t.step("BTC", {"ticker": m["ticker"], "coin": "BTC", "strike": m["strike"],
                           "close": m["close"], "yes_bid": yes_bid, "yes_ask": yes_ask,
                           "no_bid": round(1 - yes_ask, 3), "no_ask": round(1 - yes_bid, 3),
                           "yes_ask_size": 99999, "no_ask_size": 99999}, price, ts)
        t.on_settled(m["ticker"], m["result"], m.get("final"), m["close"],
                     px.get(int(m["close"]) - 60) or m["strike"])
    return t


def costs(acct):
    """Kalshi's fee on the way in and, for anything sold, on the way out."""
    return sum(lot["fee"] for lot in acct.log) + sum(
        kt.kalshi_fee(lot["contracts"], lot["exit_price"])
        for lot in acct.log if lot["status"] == "sold" and lot.get("exit_price"))


def main():
    rounds, px = load()
    n = int(sys.argv[1]) if len(sys.argv) > 1 else len(rounds)
    rounds = rounds[-n:]
    print(f"{len(rounds)} recorded BTC rounds ({len(rounds) * 15 / 60 / 24:.1f} days), "
          f"one-minute bars. Each account starts with ${kt.START_BALANCE:,.0f}.\n")
    t = run(rounds, px)

    print(f"  {'strategy':16s} {'bets':>5s} {'P/L':>10s}      "
          f"{'twin':16s} {'bets':>5s} {'P/L':>10s}      {'pair':>10s} {'fees':>9s}")
    both_lost = 0
    for real in kt.STRATEGIES:
        a = t.accounts[real["name"]]
        b = t.accounts[f"Anti {real['name']}"]
        pa, pb = a.cash - kt.START_BALANCE, b.cash - kt.START_BALANCE
        if not a.log and not b.log:
            continue
        if pa < 0 and pb < 0:
            both_lost += 1
        bust = lambda x: "  BUST" if x.cash < kt.BANKRUPT_AT else ""
        print(f"  {a.name:16s} {len(a.log):5d} {pa:+10,.2f}{bust(a):6s}"
              f"{b.name:16s} {len(b.log):5d} {pb:+10,.2f}{bust(b):6s}"
              f"{pa + pb:+10,.2f} {costs(a) + costs(b):9,.2f}")

    traded = [p for p in kt.STRATEGIES if t.accounts[p["name"]].log
              or t.accounts[f"Anti {p['name']}"].log]
    print(f"\n  Both sides lost in {both_lost} of {len(traded)} pairs.\n")
    print(f"  {'pair':10s} {'orig contracts':>15s} {'twin contracts':>15s} {'ratio':>7s}")
    for real in kt.STRATEGIES:
        a = t.accounts[real["name"]]
        b = t.accounts[f"Anti {real['name']}"]
        if not a.log or not b.log:
            continue
        ca = sum(l["contracts"] for l in a.log)
        cb = sum(l["contracts"] for l in b.log)
        print(f"  {real['name']:10s} {ca:15,d} {cb:15,d} {cb / ca:7.2f}")
    print("\n  Those ratios are why the pair column is not a clean measure of cost. Equal\n"
          "  money buys very unequal quantities at 18c and at 85c, so each pair carries a\n"
          "  standing long position in whichever side was cheaper. A positive pair does not\n"
          "  prove the model is backwards; it shows that taking the other side at the same\n"
          "  risk would have done better over this month.")


if __name__ == "__main__":
    main()
