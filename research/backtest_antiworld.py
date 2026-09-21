"""Does doing the opposite work?

    python fetch_rounds.py KXBTC15M BTC-USD bt30_btc.json   # once, ~20 min
    python backtest_antiworld.py

Every strategy loses money. There are two explanations and they call for opposite responses:

  * The model is systematically WRONG. Then inverting it should make money, and the twin
    beats the original by more than the round trip costs.
  * The model is roughly right and the LOSSES ARE COSTS -- the spread and Kalshi's fee on
    every buy and every sell. Then both sides lose, because both pay them, and no amount of
    tuning the model will help.

Running the pair separates the two. Nothing else in this project can.
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
    print(f"\n  Both sides lost in {both_lost} of {len(traded)} pairs.")
    print("\n  The 'pair' column is the two P/Ls added together. If the model carried real\n"
          "  information, one side would win more than the other lost and the pair would be\n"
          "  positive. A pair that is negative and close to the fees beside it is the market\n"
          "  charging for the privilege of having an opinion, in either direction.")


if __name__ == "__main__":
    main()
