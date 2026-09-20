"""Heavy final-minute volume went with far worse prediction from the 3-minutes-out price.
The obvious innocent explanation: close rounds are both harder to call AND attract more late
money. So condition on how close the round actually was, and see whether late volume still
carries information once that is held fixed."""
import json, os, statistics as st
from collections import defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
d = json.load(open(os.path.join(HERE, "week_data.json")))

print("Within each band of 'how uncertain the round looked 3 minutes out', split by late volume.")
print("If late money is just following close rounds, the two columns should match.\n")
BANDS = [(0.40, 0.60, "toss-up  40-60c"), (0.25, 0.40, "leaning  25-40c"),
         (0.60, 0.75, "leaning  60-75c"), (0.10, 0.25, "clear    10-25c"),
         (0.75, 0.90, "clear    75-90c")]

for coin, rounds in d["coins"].items():
    rows = []
    for r in rounds:
        bars = r.get("bars", [])
        if not bars or r["result"] not in ("yes", "no"):
            continue
        tot = sum(b[3] for b in bars)
        early = [b for b in bars if 120 < r["close"] - b[0] <= 300]
        if tot < 500 or not early:
            continue
        late = sum(b[3] for b in bars if r["close"] - b[0] <= 60)  # the settlement minute only
        mid = (early[-1][1] + early[-1][2]) / 2
        rows.append((mid, late / tot, r["result"] == "yes"))
    if len(rows) < 100:
        continue
    print(f"--- {coin} ({len(rows)} rounds) ---")
    print(f"  {'band':18s} {'n':>5s} {'calm: price Brier':>18s} {'heavy: price Brier':>19s} {'gap':>7s}")
    for lo, hi, label in BANDS:
        grp = [r for r in rows if lo <= r[0] < hi]
        if len(grp) < 40:
            continue
        grp.sort(key=lambda x: x[1])  # by late-volume share
        half = len(grp) // 2
        brier = lambda g: st.mean((p - (1 if up else 0)) ** 2 for p, _, up in g)
        calm, heavy = brier(grp[:half]), brier(grp[half:])
        print(f"  {label:18s} {len(grp):5d} {calm:18.4f} {heavy:19.4f} {heavy-calm:+7.4f}")
    print()

print("Also: in toss-up rounds, does heavy late volume favour one side finishing in the money?")
for coin, rounds in d["coins"].items():
    rows = []
    for r in rounds:
        bars = r.get("bars", [])
        if not bars or r["result"] not in ("yes", "no"):
            continue
        tot = sum(b[3] for b in bars)
        early = [b for b in bars if 120 < r["close"] - b[0] <= 300]
        if tot < 500 or not early:
            continue
        mid = (early[-1][1] + early[-1][2]) / 2
        if not 0.40 <= mid < 0.60:
            continue
        late = sum(b[3] for b in bars if r["close"] - b[0] <= 60)
        rows.append((late / tot, r["result"] == "yes"))
    if len(rows) < 40:
        continue
    rows.sort()
    half = len(rows) // 2
    up_calm = 100 * sum(1 for _, u in rows[:half] if u) / max(1, half)
    up_heavy = 100 * sum(1 for _, u in rows[half:] if u) / max(1, len(rows) - half)
    print(f"  {coin:5s} n={len(rows):4d}  UP won: calm {up_calm:5.1f}%  heavy {up_heavy:5.1f}%"
          f"   (50% = no directional tilt)")
