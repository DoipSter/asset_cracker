"""True erf to 60+ digits by Taylor series for |x| < 0.84375, correctly rounded to a double.

    cd engine && python3 tool/erf_truth.py

Writes test/data/erf_truth.json. It exists because CPython's math.erf is the platform C
library's, and on macOS (Python 3.9.6, 2026-09-20) that was off by up to 3 units in the last
place in this range, so it cannot be the reference for accuracy. The Dart erf is held to 1.
"""
import json, math, struct
from decimal import Decimal, getcontext
getcontext().prec = 70
PI = Decimal("3.14159265358979323846264338327950288419716939937510582097494459230781640628620899")
def true_erf(x):
    x = Decimal(x); term = x; total = x; n = 0
    while abs(term) > Decimal(10) ** -68:
        n += 1
        term = -term * x * x / n
        total += term / (2 * n + 1)
    return 2 * total / PI.sqrt()
def bits(v): return struct.unpack(">q", struct.pack(">d", v))[0]
def nearest(d):  # correctly rounded double of a Decimal
    return float(d)
rows = json.load(open("test/data/pyfloat_vectors.json"))["erf"]
small = [float(a) for a, _ in rows if 2e-9 <= abs(float(a)) < 0.84375]
out = []
py_off = 0; worst_py = 0
for x in small:
    t = nearest(true_erf(x))
    d = abs(bits(math.erf(x)) - bits(t))
    py_off += d > 0; worst_py = max(worst_py, d)
    out.append([repr(x), repr(t)])
print("python math.erf vs truth: differing", py_off, "of", len(small), "worst", worst_py, "ulp")
json.dump(out, open("test/data/erf_truth.json", "w"))
