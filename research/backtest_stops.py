"""Should the Scalper cut its losses?

    python fetch_rounds.py KXBTC15M BTC-USD bt30_btc.json   # once, ~20 min
    python backtest_stops.py                                # the full month

It does not, today: `take_capture` requires proceeds to beat cost so it can only fire at a
profit, and the value exit fires when the market is paying MORE than the model thinks the
position is worth, which is selling into strength. A position that is dying is held to zero.

This script is the evidence behind leaving it that way. It answers three questions in order:

  1. How does the Scalper's money actually end up -- won, lost, sold up, sold down?
  2. Do the positions a stop would cut go on to recover?
  3. What happens if you actually switch a stop on?

The answer to (3) is that every stop tested loses more money. The answer to (2) explains part
of it, and the fill analysis explains the rest -- and exposes a limit of this data that any
future attempt needs to know about.
"""

import json
import os
import statistics
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


def run(rounds, px, track_worst=False, **overrides):
    """Replay the rounds through the real engine. With `track_worst`, also record how far
    underwater each position ever got, as a fraction of what it cost."""
    t = kt.KalshiTrader(tempfile.mkdtemp(prefix="ac_stops_"), {"BTC": {}})
    t.save = t._append_csv = t._log_exits = t._log_round = lambda *a, **k: None
    # No reviving here. Live, a strategy that runs out is staked again so the
    # comparison keeps running; in a backtest that would inject fresh capital and
    # turn a wipe-out into an apparent profit. A dead strategy stays dead.
    t._check_broke = lambda *a, **k: []
    acct = t.accounts["Scalper"]
    acct.params = dict(acct.params)
    for key, value in overrides.items():
        if value is None:
            acct.params.pop(key, None)
        else:
            acct.params[key] = value

    worst = {}
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
            if track_worst:
                for lot in acct.open_lots(m["ticker"]):
                    if ts <= lot["t"]:
                        continue
                    bid = yes_bid if lot["side"] == "UP" else round(1 - yes_ask, 3)
                    sell = max(0.01, bid - kt.SLIPPAGE)
                    got = lot["contracts"] * sell - kt.kalshi_fee(lot["contracts"], sell)
                    worst[lot["id"]] = min(worst.get(lot["id"], 9.9),
                                           max(0.0, got) / lot["cost"])
        t.on_settled(m["ticker"], m["result"], m.get("final"), m["close"],
                     px.get(int(m["close"]) - 60) or m["strike"])
    return acct, worst


def where_the_money_went(acct):
    print("\n1. How the Scalper's money actually ends up\n")
    print(f"   {'how it ended':24s} {'n':>5s} {'staked':>11s} {'net':>12s}")
    for label, group in (
            ("won at settlement", [l for l in acct.log if l["status"] == "won"]),
            ("LOST at settlement", [l for l in acct.log if l["status"] == "lost"]),
            ("sold at a profit",
             [l for l in acct.log if l["status"] == "sold" and l["pnl"] > 0]),
            ("sold at a loss",
             [l for l in acct.log if l["status"] == "sold" and l["pnl"] <= 0])):
        print(f"   {label:24s} {len(group):5d} "
              f"{sum(l['cost'] for l in group):11,.2f} "
              f"{sum(l.get('pnl', 0.0) or 0.0 for l in group):+12,.2f}")
    cut = sum(1 for l in acct.log if l["status"] == "sold" and l["pnl"] <= 0)
    losers = cut + sum(1 for l in acct.log if l["status"] == "lost")
    print(f"\n   It cuts a loser {cut} times out of {losers} ({cut / losers:.0%}). "
          f"The rest are held to zero.")


def do_they_recover(acct, worst):
    print("\n2. Do the positions a stop would cut recover?\n")
    held = [l for l in acct.log if l["status"] in ("won", "lost") and l["id"] in worst]
    print(f"   {len(held)} positions were held to settlement.\n")
    print(f"   {'fell below':>11s} {'positions':>10s} {'won':>6s} {'win rate':>9s} "
          f"{'they paid':>12s} {'floor would':>13s}")
    for stop in (0.30, 0.50, 0.70, 0.85):
        floor = 1 - stop
        tripped = [l for l in held if worst[l["id"]] <= floor]
        if not tripped:
            continue
        won = [l for l in tripped if l["status"] == "won"]
        paid = sum(l.get("pnl", 0.0) or 0.0 for l in tripped)
        at_floor = sum(l["cost"] * floor - l["cost"] for l in tripped)
        print(f"   {floor:10.0%} {len(tripped):10d} {len(won):6d} "
              f"{len(won) / len(tripped):8.1%} {paid:+12,.2f} {at_floor:+13,.2f}")
    print("\n   Read the last two columns together: selling at the floor looks better than\n"
          "   holding. That is the case FOR a stop, and it is why the next section matters.")


def what_actually_happens(rounds, px, base):
    print("\n3. What happens when you switch one on\n")
    print(f"   {'rule':26s} {'positions':>10s} {'cut':>6s} {'written off':>12s} {'P/L':>12s}")
    print(f"   {'as shipped (no stop)':26s} {len(base.log):10d} {0:6d} "
          f"{sum(1 for l in base.log if l['status'] == 'lost'):12d} "
          f"{base.cash - kt.START_BALANCE:+12,.2f}")
    for label, params in ([(f"stop_loss {s:.2f}", {"stop_loss": s})
                           for s in (0.30, 0.50, 0.70, 0.85)]
                          + [(f"stop_tau {s}s", {"stop_tau": s})
                             for s in (60, 120, 180, 300)]):
        acct, _ = run(rounds, px, **params)
        cut = sum(1 for l in acct.log if l.get("why") == "stop")
        lost = sum(1 for l in acct.log if l["status"] == "lost")
        print(f"   {label:26s} {len(acct.log):10d} {cut:6d} {lost:12d} "
              f"{acct.cash - kt.START_BALANCE:+12,.2f}")
    print("\n   Every one of them is worse.")


def why_the_price_stop_fails(rounds, px):
    print("\n4. Why the price stop fails, and the caveat that comes with it\n")
    for stop in (0.50, 0.70):
        acct, _ = run(rounds, px, stop_loss=stop)
        stops = [l for l in acct.log if l.get("why") == "stop"]
        if not stops:
            continue
        floor = 1 - stop
        fills = [l["payout"] / l["cost"] for l in stops]
        through = [f for f in fills if f < floor - 0.02]
        print(f"   stop_loss {stop:.2f}, aiming to sell at {floor:.0%} of cost:")
        print(f"     {len(stops)} fired, filling at a median of {statistics.median(fills):.0%}"
              f" of cost (worst {min(fills):.0%})")
        print(f"     {len(through)} of {len(stops)} ({len(through) / len(stops):.0%}) filled "
              f"more than two points BELOW the floor\n")
    print("   A stop is only checked when a quote arrives, and this cache is one-minute\n"
          "   bars -- a whole minute in which a dying position falls straight through its\n"
          "   floor. Live, the app quotes every second, so real fills would land far closer\n"
          "   to the floor than this. THIS BACKTEST CANNOT FAIRLY TEST A PRICE STOP.\n\n"
          "   The time stop can be tested fairly -- the clock never gaps -- and it is worse\n"
          "   too, which is the stronger half of the argument for leaving both off.")


def main():
    rounds, px = load()
    n = int(sys.argv[1]) if len(sys.argv) > 1 else len(rounds)
    rounds = rounds[-n:]
    print(f"Scalper over {len(rounds)} recorded BTC rounds "
          f"({len(rounds) * 15 / 60 / 24:.1f} days), one-minute bars")
    base, worst = run(rounds, px, track_worst=True)
    where_the_money_went(base)
    do_they_recover(base, worst)
    what_actually_happens(rounds, px, base)
    why_the_price_stop_fails(rounds, px)


if __name__ == "__main__":
    main()
