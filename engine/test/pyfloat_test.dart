import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'package:asset_cracker_engine/src/pyfloat.dart';
import 'package:test/test.dart';

/// How many representable doubles apart two values are.
int ulps(double a, double b) {
  int bits(double v) => (ByteData(8)..setFloat64(0, v)).getInt64(0);
  if (a == b) return 0;
  if (a.isNegative != b.isNegative) return 1 << 30;
  return (bits(a) - bits(b)).abs();
}

void main() {
  final vectors = jsonDecode(File('test/data/pyfloat_vectors.json').readAsStringSync())
      as Map<String, dynamic>;

  int worstUlps(List rows) {
    var worst = 0;
    for (final row in rows) {
      final d = ulps(erf(double.parse(row[0])), double.parse(row[1]));
      if (d > worst) worst = d;
    }
    return worst;
  }

  test('erf is within one unit in the last place of the true value', () {
    final truth = jsonDecode(File('test/data/erf_truth.json').readAsStringSync()) as List;
    expect(worstUlps(truth), lessThanOrEqualTo(1));
  });

  // CPython's math.erf is the platform C library's. On macOS it was itself up to 3 ulp from the
  // true value (see tool/erf_truth.py), so agreement with it can only be this loose.
  test('erf agrees with CPython to within three units in the last place', () {
    expect(worstUlps(vectors['erf'] as List), lessThanOrEqualTo(3));
  });

  test('pyRound matches round(x, n) exactly', () {
    for (final row in vectors['round'] as List) {
      final got = pyRound(double.parse(row[0]), row[1] as int);
      expect(pyRepr(got), row[2], reason: 'round(${row[0]}, ${row[1]})');
    }
  });

  test('pyRepr matches repr(x) exactly', () {
    for (final text in vectors['repr'] as List) {
      expect(pyRepr(double.parse(text as String)), text);
    }
  });

  test('pyFloorDiv matches a // b exactly', () {
    for (final row in vectors['floordiv'] as List) {
      final got = pyFloorDiv(double.parse(row[0]), double.parse(row[1]));
      expect(pyRepr(got), row[2], reason: '${row[0]} // ${row[1]}');
    }
  });

  test('pyMedian and pySum', () {
    expect(pyMedian([3.0, 1.0, 2.0]), 2.0);
    expect(pyMedian([4.0, 1.0, 3.0, 2.0]), 2.5);
    expect(pySum([0.1, 0.2, 0.3]), 0.1 + 0.2 + 0.3);
  });
}
