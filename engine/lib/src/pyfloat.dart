/// Number handling that matches CPython, so the Dart engine makes the same decisions and
/// writes the same trade log as kalshi_trader.py. Each function here exists because Dart's
/// built-in behaves differently from Python's in some case the engine can reach.
library;

import 'dart:math' as math;
import 'dart:typed_data';

final _ten = BigInt.from(10);

/// Python's `round(x, ndigits)` for floats: the exact binary value of [x] rounded to
/// [ndigits] decimals, exact ties going to the even digit (`round(0.125, 2) == 0.12`).
/// Dart's `toStringAsFixed` sends ties up instead.
double pyRound(double x, int ndigits) {
  if (x.isNaN || x.isInfinite || x == 0) return x;
  final bytes = ByteData(8)..setFloat64(0, x);
  final hi = bytes.getUint32(0), lo = bytes.getUint32(4);
  final negative = (hi >> 31) != 0;
  final expBits = (hi >> 20) & 0x7ff;
  var mantissa = (BigInt.from(hi & 0xfffff) << 32) | BigInt.from(lo);
  int e;
  if (expBits == 0) {
    e = -1074;
  } else {
    mantissa |= BigInt.one << 52;
    e = expBits - 1075;
  }
  // |x| * 10^ndigits as the exact fraction num / den
  var num = mantissa, den = BigInt.one;
  if (ndigits >= 0) {
    num *= _ten.pow(ndigits);
  } else {
    den *= _ten.pow(-ndigits);
  }
  if (e >= 0) {
    num <<= e;
  } else {
    den <<= -e;
  }
  var q = num ~/ den;
  final twice = (num % den) << 1;
  if (twice > den || (twice == den && q.isOdd)) q += BigInt.one;

  String text;
  if (ndigits <= 0) {
    text = (q * _ten.pow(-ndigits)).toString();
  } else {
    final digits = q.toString().padLeft(ndigits + 1, '0');
    final cut = digits.length - ndigits;
    text = '${digits.substring(0, cut)}.${digits.substring(cut)}';
  }
  final value = double.parse(text);
  return negative ? -value : value;
}

/// Python's `repr(float)`, which is what the csv module writes: the shortest digits that
/// round-trip, fixed notation for exponents from -4 up to 15, otherwise `1.5e-05` style.
/// Dart's `toString` picks the same digits but switches notation at different sizes.
String pyRepr(double x) {
  if (x.isNaN) return 'nan';
  if (x.isInfinite) return x < 0 ? '-inf' : 'inf';
  if (x == 0) return x.isNegative ? '-0.0' : '0.0';
  final parts = x.abs().toStringAsExponential().split('e'); // shortest digits, "d.ddde+N"
  final digits = parts[0].replaceAll('.', '');
  final exp = int.parse(parts[1]);
  final sign = x < 0 ? '-' : '';
  if (exp >= -4 && exp < 16) {
    final point = exp + 1; // digits before the decimal point
    if (point <= 0) return '${sign}0.${'0' * -point}$digits';
    if (point >= digits.length) return '$sign$digits${'0' * (point - digits.length)}.0';
    return '$sign${digits.substring(0, point)}.${digits.substring(point)}';
  }
  final tail = digits.length > 1 ? '.${digits.substring(1)}' : '';
  final expText = exp.abs().toString().padLeft(2, '0');
  return '$sign${digits[0]}${tail}e${exp < 0 ? '-' : '+'}$expText';
}

/// How Python's csv module renders a field: None as empty, floats by repr, the rest by str.
String pyStr(Object? value) {
  if (value == null) return '';
  if (value is double) return pyRepr(value);
  if (value is bool) return value ? 'True' : 'False';
  return value.toString();
}

/// Python's float `a // b`. CPython derives it from fmod, which is not always the same as
/// flooring the rounded quotient a / b.
double pyFloorDiv(double a, double b) {
  if (b == 0) throw ArgumentError('float floor division by zero');
  var mod = a.remainder(b); // C fmod: exact, sign follows a
  var div = (a - mod) / b;
  if (mod != 0) {
    if ((b < 0) != (mod < 0)) {
      mod += b;
      div -= 1.0;
    }
  }
  if (div != 0) {
    var floored = div.floorToDouble();
    if (div - floored > 0.5) floored += 1.0;
    return floored;
  }
  return (a / b).isNegative ? -0.0 : 0.0;
}

/// Sum in order, one add at a time, as Python's `sum` does for floats up to 3.11.
/// (From 3.12 CPython's `sum` compensates for rounding, which can differ in the last bit.
/// The parity fixtures were recorded on 3.9, so this is the behaviour they hold.)
double pySum(Iterable<double> values) {
  var total = 0.0;
  for (final v in values) {
    total += v;
  }
  return total;
}

/// Python's `statistics.median`.
double pyMedian(List<double> values) {
  final sorted = [...values]..sort();
  final n = sorted.length;
  if (n == 0) throw StateError('no median for empty data');
  return n.isOdd ? sorted[n ~/ 2] : (sorted[n ~/ 2 - 1] + sorted[n ~/ 2]) / 2;
}

// ---------------------------------------------------------------------------
// erf: dart:math has none. This is the algorithm from Sun's fdlibm (s_erf.c), the one most C
// libraries descend from, accurate to under one unit in the last place.
// ---------------------------------------------------------------------------

const _erx = 8.45062911510467529297e-01;
const _efx = 1.28379167095512586316e-01;
const _efx8 = 1.02703333676410069053e+00;
const _pp = [
  1.28379167095512558561e-01, -3.25042107247001499370e-01, -2.84817495755985104766e-02,
  -5.77027029648944159157e-03, -2.37630166566501626084e-05,
];
const _qq = [
  1.0, 3.97917223959155352819e-01, 6.50222499887672944485e-02, 5.08130628187576562776e-03,
  1.32494738004321644526e-04, -3.96022827877536812320e-06,
];
const _pa = [
  -2.36211856075265944077e-03, 4.14856118683748331666e-01, -3.72207876035701323847e-01,
  3.18346619901161753674e-01, -1.10894694282396677476e-01, 3.54783043256182359371e-02,
  -2.16637559486879084300e-03,
];
const _qa = [
  1.0, 1.06420880400844228286e-01, 5.40397917702171048937e-01, 7.18286544141962662868e-02,
  1.26171219808761642112e-01, 1.36370839120290507362e-02, 1.19844998467991074170e-02,
];
const _ra = [
  -9.86494403484714822705e-03, -6.93858572707181764372e-01, -1.05586262253232909814e+01,
  -6.23753324503260060396e+01, -1.62396669462573470355e+02, -1.84605092906711035994e+02,
  -8.12874355063065934246e+01, -9.81432934416914548592e+00,
];
const _sa = [
  1.0, 1.96512716674392571292e+01, 1.37657754143519042600e+02, 4.34565877475229228821e+02,
  6.45387271733267880336e+02, 4.29008140027567833386e+02, 1.08635005541779435134e+02,
  6.57024977031928170135e+00, -6.04244152148580987438e-02,
];
const _rb = [
  -9.86494292470009928597e-03, -7.99283237680523006574e-01, -1.77579549177547519889e+01,
  -1.60636384855821916062e+02, -6.37566443368389627722e+02, -1.02509513161107724954e+03,
  -4.83519191608651397019e+02,
];
const _sb = [
  1.0, 3.03380607434824582924e+01, 3.25792512996573918826e+02, 1.53672958608443695994e+03,
  3.19985821950859553908e+03, 2.55305040643316442583e+03, 4.74528541206955367215e+02,
  -2.24409524465858183362e+01,
];

double _poly(List<double> c, double x) {
  var acc = c[c.length - 1];
  for (var i = c.length - 2; i >= 0; i--) {
    acc = c[i] + x * acc;
  }
  return acc;
}

/// The error function, as `math.erf` in Python.
double erf(double x) {
  if (x.isNaN) return x;
  if (x.isInfinite) return x > 0 ? 1.0 : -1.0;
  final bytes = ByteData(8)..setFloat64(0, x);
  final ix = bytes.getUint32(0) & 0x7fffffff; // high word of |x|, compared as fdlibm does
  final negative = x < 0;
  final ax = x.abs();

  if (ix < 0x3feb0000) {
    // |x| < 0.84375
    if (ix < 0x3e300000) {
      // |x| < 2**-28
      if (ix < 0x00800000) return 0.125 * (8.0 * x + _efx8 * x); // avoid underflow
      return x + _efx * x;
    }
    final z = x * x;
    return x + x * (_poly(_pp, z) / _poly(_qq, z));
  }
  if (ix < 0x3ff40000) {
    // 0.84375 <= |x| < 1.25
    final s = ax - 1.0;
    final pq = _poly(_pa, s) / _poly(_qa, s);
    return negative ? -_erx - pq : _erx + pq;
  }
  if (ix >= 0x40180000) {
    // |x| >= 6
    return negative ? 1e-300 - 1.0 : 1.0 - 1e-300;
  }
  final s = 1.0 / (ax * ax);
  final rs = ix < 0x4006DB6E // |x| < 1/0.35
      ? _poly(_ra, s) / _poly(_sa, s)
      : _poly(_rb, s) / _poly(_sb, s);
  final zBytes = ByteData(8)
    ..setFloat64(0, ax)
    ..setUint32(4, 0); // |x| with its low 32 bits cleared
  final z = zBytes.getFloat64(0);
  final r = math.exp(-z * z - 0.5625) * math.exp((z - ax) * (z + ax) + rs);
  return negative ? r / ax - 1.0 : 1.0 - r / ax;
}
