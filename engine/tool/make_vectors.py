"""Reference values from CPython for engine/test/pyfloat_test.dart.

    python3 engine/tool/make_vectors.py

Writes engine/test/data/pyfloat_vectors.json. Floats are stored as repr strings so nothing is
lost in transit. Regenerate only on purpose: the file records what Python does.
"""
import json
import math
import os
import random
import sys

rnd = random.Random(20260920)
out = {"python": sys.version.split()[0]}

xs = [i / 1000 for i in range(-7000, 7001, 7)]
xs += [rnd.uniform(-6.5, 6.5) for _ in range(3000)]
xs += [rnd.uniform(-1e-3, 1e-3) for _ in range(200)]
xs += [0.84375, 1.25, 1 / 0.35, 2.857142857142857, 2.8571434, 6.0, 1e-9, 1e-300, 5e-324, 0.0, 27.0]
out["erf"] = [[repr(x), repr(math.erf(x))] for x in xs]

vals = []
for nd in (2, 3, 8):
    for _ in range(1500):
        vals.append((rnd.uniform(-200, 200), nd))
    for _ in range(300):
        vals.append((rnd.uniform(0, 1), nd))
for k in range(-40, 41):  # exact binary ties at two and three decimals
    vals += [(k / 8, 2), (k / 8 + 100, 2), (k / 16, 3), (k / 2, 0)]
vals += [(2.675, 2), (1.005, 2), (0.285, 2), (-0.001, 2), (-0.0, 2), (1e-9, 8), (149.995, 2),
         (7.8, 2), (0.145, 3), (81094.005, 2), (1e22, 2), (5e-324, 2)]
out["round"] = [[repr(x), nd, repr(round(x, nd))] for x, nd in vals]

reprs = [rnd.uniform(-1e5, 1e5) for _ in range(500)]
reprs += [rnd.uniform(0, 1) * 10 ** rnd.randint(-12, 22) for _ in range(800)]
reprs += [0.0, -0.0, 1.0, 150.0, 0.31, 1e16, 1e15, 9999999999999998.0, 1e-4, 1e-5, 0.0001234,
          1.5e-05, 1e22, 1e21, 123456789012345680.0, 5e-324, 1.7976931348623157e308, -2.5e-7]
out["repr"] = [repr(x) for x in reprs]

pairs = [(rnd.uniform(-50, 50), rnd.uniform(0.01, 1.2)) for _ in range(2000)]
pairs += [(rnd.uniform(0, 30), rnd.choice([0.31, 0.4, 0.57, 0.1, 0.99])) for _ in range(1000)]
pairs += [(1.0, 0.1), (0.3, 0.1), (0.6, 0.2), (5.0, 1.0), (-5.0, 1.0), (0.0, 0.5), (-0.0, 0.5), (7.0, -2.0)]
out["floordiv"] = [[repr(a), repr(b), repr(a // b)] for a, b in pairs]

path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "test", "data", "pyfloat_vectors.json")
with open(path, "w") as f:
    json.dump(out, f)
print({k: len(v) for k, v in out.items() if isinstance(v, list)})
