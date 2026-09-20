"""A week of Kalshi crypto markets: volume by 15-minute round and by hour, bracket
concentration, and the signatures you would expect if settlement prices were being pushed.

Manipulation is assumed possible, so the tests look for its fingerprints rather than
assuming an efficient market: volume clustering into the settlement minute, price moves
that reverse right after settlement, and rounds that settle a hair over the strike.
"""
import json, math, os, statistics as st
from collections import defaultdict
from datetime import datetime

HERE = os.path.dirname(os.path.abspath(__file__))
d = json.load(open(os.path.join(HERE, "week_data.json")))
spot_all = {c: {int(k): v for k, v in px.items()} for c, px in d.get("spot", {}).items()}

print(f"=== scraped {datetime.fromtimestamp(d['scraped_at']):%b %d %H:%M}, {d['days']} days ===\n")

# ---------- 1. per coin: rounds, volume, outcome balance ----------------------------
print("1) 15-MINUTE ROUNDS PER COIN")
print(f"  {'coin':5s} {'rounds':>7s} {'total vol':>13s} {'median/round':>13s} {'UP won':>7s} {'no-vol':>7s}")
per_coin = {}
for coin, rounds in d["coins"].items():
    vols = [r["volume"] for r in rounds]
    ups = sum(1 for r in rounds if r["result"] == "yes")
    dead = sum(1 for v in vols if v == 0)
    per_coin[coin] = rounds
    print(f"  {coin:5s} {len(rounds):7d} {sum(vols):13,.0f} {st.median(vols):13,.0f} "
          f"{100*ups/max(1,len(rounds)):6.1f}% {dead:7d}")

# ---------- 2. where volume sits inside a 15-minute round ---------------------------
print("\n2) WHEN DOES THE MONEY ARRIVE? (share of a round's volume by minute before close)")
print("   A market that is merely pricing risk spreads its volume. Money concentrated in the")
print("   final minute is where settlement pressure would show up.")
print(f"  {'coin':5s} " + " ".join(f"{m:>6s}" for m in ["14-10m", "9-5m", "4-2m", "last 1m"]))
for coin, rounds in per_coin.items():
    buckets = [0.0, 0.0, 0.0, 0.0]
    for r in rounds:
        for ts_, yb, ya, vol, oi in r.get("bars", []):
            left = r["close"] - ts_
            if left > 600: buckets[0] += vol
            elif left > 300: buckets[1] += vol
            elif left > 120: buckets[2] += vol
            else: buckets[3] += vol
    tot = sum(buckets) or 1
    print(f"  {coin:5s} " + " ".join(f"{100*b/tot:5.1f}%" for b in buckets))

# ---------- 3. how close do rounds settle to the strike? ----------------------------
print("\n3) SETTLEMENT MARGIN: how far the final index landed from the price to beat")
print("   A clean market scatters. An excess of near-zero margins would suggest rounds being")
print("   nudged just over the line.")
print(f"  {'coin':5s} {'median |margin|':>16s} {'<0.01%':>8s} {'<0.02%':>8s} {'<0.05%':>8s} {'expected <0.05%':>16s}")
for coin, rounds in per_coin.items():
    margins = []
    for r in rounds:
        f, k = None, r.get("floor")
        try:
            f = float(str(r.get("final")).replace(",", ""))
        except (TypeError, ValueError):
            pass
        if f and k:
            margins.append(abs(f - k) / k * 100)
    if len(margins) < 30:
        continue
    margins.sort()
    n = len(margins)
    frac = lambda t: 100 * sum(1 for m in margins if m < t) / n
    # if margins were spread uniformly over the observed range, this is what <0.05% implies
    expected = 100 * min(1.0, 0.05 / (st.median(margins) * 2)) if st.median(margins) else 0
    print(f"  {coin:5s} {st.median(margins):15.4f}% {frac(0.01):7.1f}% {frac(0.02):7.1f}% "
          f"{frac(0.05):7.1f}% {expected:15.1f}%")

# ---------- 4. does a late volume surge predict the outcome? ------------------------
print("\n4) DOES LATE MONEY KNOW SOMETHING? (rounds split by final-minute volume share)")
print("   If a late surge predicts the winner better than the price did, that is the")
print("   footprint of informed or self-fulfilling flow rather than ordinary hedging.")
for coin, rounds in per_coin.items():
    rows = []
    for r in rounds:
        bars = r.get("bars", [])
        if not bars or r["result"] not in ("yes", "no"):
            continue
        tot = sum(b[3] for b in bars)
        late = sum(b[3] for b in bars if r["close"] - b[0] <= 120)
        early = [b for b in bars if 120 < r["close"] - b[0] <= 300]
        if tot < 500 or not early:
            continue
        mid = (early[-1][1] + early[-1][2]) / 2  # the price ~3 min out
        rows.append((late / tot, mid, r["result"] == "yes"))
    if len(rows) < 40:
        continue
    rows.sort()
    half = len(rows) // 2
    for label, grp in (("calm finish", rows[:half]), ("heavy finish", rows[half:])):
        # Brier score: how well the 3-minutes-out price predicted the result (lower = better)
        brier = st.mean((p - (1 if up else 0)) ** 2 for _, p, up in grp)
        print(f"  {coin:5s} {label:14s} n={len(grp):4d}  late-vol share "
              f"{100*st.median([x[0] for x in grp]):5.1f}%  price Brier {brier:.4f}")

# ---------- 5. hourly bracket markets ------------------------------------------------
br = d.get("hourly_brackets", [])
if br:
    print(f"\n5) HOURLY BRACKET MARKETS (KXBTC): {len(br)} settled brackets")
    by_event = defaultdict(list)
    for m in br:
        by_event[m["event"]].append(m)
    widths, live_counts, conc = [], [], []
    for ev, ms in by_event.items():
        ms.sort(key=lambda m: m.get("floor") or 0)
        vols = sorted((m["volume"] for m in ms), reverse=True)
        tot = sum(vols) or 1
        live_counts.append(sum(1 for v in vols if v > 0))
        conc.append(100 * sum(vols[:3]) / tot)
        floors = [m["floor"] for m in ms if m.get("floor")]
        if len(floors) > 2:
            widths.append(st.median([b - a for a, b in zip(floors, floors[1:])]))
    print(f"   {len(by_event)} hourly events, median {st.median([len(v) for v in by_event.values()]):.0f} brackets each")
    print(f"   bracket width (median): ${st.median(widths):,.0f}" if widths else "")
    print(f"   brackets that traded at all (median): {st.median(live_counts):.0f}")
    print(f"   share of an hour's volume in its top 3 brackets (median): {st.median(conc):.1f}%")
    tot_hour = sum(m["volume"] for m in br)
    tot_15 = sum(r["volume"] for r in per_coin.get("BTC", []))
    print(f"   BTC volume: hourly brackets {tot_hour:,.0f} vs 15-minute rounds {tot_15:,.0f} "
          f"({100*tot_hour/max(1,tot_hour+tot_15):.0f}% of the combined total)")

ab = d.get("hourly_ab", [])
if ab:
    print(f"\n6) HOURLY ABOVE/BELOW (KXBTCD): {len(ab)} settled markets, "
          f"total volume {sum(m['volume'] for m in ab):,.0f}")
