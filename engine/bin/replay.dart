/// Replay a recording made by tools/parity/record.py through the Dart engine and compare the
/// result with what the Python engine did live. This is the parity gate for the port.
///
///     dart run bin/replay.dart ../tools/parity/fixtures/2026-09-20_40min
///
/// Passes (exit 0) when, for every coin, the trade log is identical line for line and the
/// saved state agrees: each account's cash, balance, realized P&L, counts and bet log, the
/// stored index offsets, and the rounds watched.
///
/// If the folder holds trace.jsonl.gz (from `replay.py --trace`), every step is compared too:
/// volatility and index offset must be identical, each strategy must favour the same side and
/// make the same bet/no-bet call, and probabilities and edges must agree to within [tolerance].
/// Probabilities are not required to be bit-identical because they pass through erf, which
/// CPython takes from the platform C library (see test/pyfloat_test.dart).
library;

import 'dart:convert';
import 'dart:io';

import 'package:asset_cracker_engine/asset_cracker_engine.dart';

List<Map<String, dynamic>> readCalls(String folder) {
  final plain = File('$folder/calls.jsonl');
  final text = plain.existsSync()
      ? plain.readAsStringSync()
      : utf8.decode(gzip.decode(File('$folder/calls.jsonl.gz').readAsBytesSync()));
  return [
    for (final line in const LineSplitter().convert(text))
      if (line.trim().isNotEmpty) jsonDecode(line) as Map<String, dynamic>
  ];
}

double d(Object? v) => (v as num).toDouble();

const tolerance = 1e-12;

/// What the Python engine thought after each step, keyed by the call's position in the log.
Map<int, Map<String, dynamic>> readTrace(String folder) {
  final file = File('$folder/trace.jsonl.gz');
  if (!file.existsSync()) return {};
  final text = utf8.decode(gzip.decode(file.readAsBytesSync()));
  return {
    for (final line in const LineSplitter().convert(text))
      if (line.trim().isNotEmpty)
        (jsonDecode(line) as Map<String, dynamic>)['i'] as int: jsonDecode(line) as Map<String, dynamic>
  };
}

/// Compares one step with the trace. Returns a description of the first mismatch, or null.
/// [worst] collects the largest numeric gap seen in each quantity.
String? checkStep(KalshiTrader trader, Map<String, dynamic> want, Map<String, double> worst) {
  void gap(String name, double a, double b) {
    final g = (a - b).abs();
    if (g > (worst[name] ?? 0)) worst[name] = g;
  }

  if (trader.sigma2 != d(want['sigma2'])) return 'sigma2 python ${want['sigma2']} dart ${trader.sigma2}';
  if (trader.offsetPct != d(want['offset'])) return 'offset python ${want['offset']} dart ${trader.offsetPct}';
  for (final e in (want['views'] as Map<String, dynamic>).entries) {
    final view = trader.accounts[e.key]!.view;
    if (e.value == null || view == null) {
      if (e.value != null || view != null) return '${e.key}: a view on one side only';
      continue;
    }
    final v = e.value as List;
    gap('p_up', view.pUp, d(v[0]));
    gap('p_model', view.pModel, d(v[1]));
    gap('edge', view.best.edge, d(v[3]));
    if (view.best.side != v[2]) return '${e.key}: side python ${v[2]} dart ${view.best.side}';
    if (view.signal.bet != v[4]) return '${e.key}: bet python ${v[4]} dart ${view.signal.bet}';
    for (final (name, a, b) in [
      ('p_up', view.pUp, d(v[0])), ('p_model', view.pModel, d(v[1])), ('edge', view.best.edge, d(v[3]))
    ]) {
      if ((a - b).abs() > tolerance) return '${e.key}: $name python $b dart $a';
    }
  }
  return null;
}

void apply(KalshiTrader trader, String call, List args) {
  switch (call) {
    case 'observe':
      trader.observe(d(args[0]), d(args[1]));
    case 'seed_vol':
      trader.seedVol([for (final c in args[0] as List) d(c)]);
    case 'seed_offsets':
      trader.seedOffsets([for (final pair in args[0] as List) (d(pair[0]), d(pair[1]))]);
    case 'step':
      trader.step(Market.fromJson(args[0] as Map<String, dynamic>),
          args[1] == null ? null : d(args[1]), d(args[2]));
    case 'note_settlement':
      trader.noteSettlement(d(args[0]), args[1]);
    case 'on_settled':
      trader.onSettled(args[0] as String, args[1] as String?, args[2], d(args[3]),
          args[4] == null ? null : d(args[4]));
    default:
      throw ArgumentError('unknown call in recording: $call');
  }
}

/// The parts of the saved state that depend only on the inputs. ("updated" and "started_at"
/// are wall-clock stamps; "index_offset_pct" depends on the moment of the final save.)
Map<String, Object?> comparable(Map<String, dynamic> state) => {
      'rounds_monitored': state['rounds_monitored'],
      'last_round_ticker': state['last_round_ticker'],
      'index_offsets': state['index_offsets'],
      'leaderboard': state['leaderboard'],
      for (final e in (state['accounts'] as Map<String, dynamic>).entries) 'account ${e.key}': e.value,
    };

/// Where two JSON values first differ, or null if they are the same. Key order is ignored and
/// numbers compare by value, so 9 equals 9.0.
String? firstDifference(Object? a, Object? b, [String path = '']) {
  if (a is Map && b is Map) {
    for (final key in {...a.keys, ...b.keys}) {
      if (!a.containsKey(key) || !b.containsKey(key)) return '$path/$key: only on one side';
      final diff = firstDifference(a[key], b[key], '$path/$key');
      if (diff != null) return diff;
    }
    return null;
  }
  if (a is List && b is List) {
    if (a.length != b.length) return '$path: length ${a.length} vs ${b.length}';
    for (var i = 0; i < a.length; i++) {
      final diff = firstDifference(a[i], b[i], '$path[$i]');
      if (diff != null) return diff;
    }
    return null;
  }
  return a == b ? null : '$path: python $a, dart $b';
}

void main(List<String> argv) {
  if (argv.isEmpty) {
    stderr.writeln('usage: dart run bin/replay.dart <recording folder>');
    exit(2);
  }
  final folder = argv[0];
  final rows = readCalls(folder);
  final meta = rows.first['meta'] as Map<String, dynamic>;
  final coins = (meta['coins'] as List).cast<String>();

  final clock = ManualClock(d(rows.first['t']));
  final stores = <String, MemoryStore>{}, traders = <String, KalshiTrader>{};
  for (final coin in coins) {
    final a = meta['assets'][coin] as Map<String, dynamic>;
    stores[coin] = MemoryStore();
    traders[coin] = KalshiTrader(stores[coin]!,
        clock: clock,
        offsetPct: d(a['index_offset_pct']),
        sdPct: d(a['index_sd_pct']),
        defaultSigma: d(a['default_sigma']));
  }

  final trace = readTrace(folder);
  final worst = <String, double>{};
  final stepErrors = <String>[];
  var stepsChecked = 0;
  final counts = <String, int>{};
  for (var i = 0; i + 1 < rows.length; i++) {
    final row = rows[i + 1];
    clock.t = d(row['t']);
    final call = row['call'] as String;
    apply(traders[row['coin']]!, call, row['args'] as List);
    counts[call] = (counts[call] ?? 0) + 1;
    final want = trace[i];
    if (want != null) {
      stepsChecked++;
      final err = checkStep(traders[row['coin']]!, want, worst);
      if (err != null) stepErrors.add('call $i (${row['coin']}): $err');
    }
  }
  for (final t in traders.values) {
    t.save(force: true);
  }
  print('replayed ${rows.length - 1} calls: $counts (recorded on Python ${meta['python']})');

  var ok = true;
  if (trace.isEmpty) {
    print('no trace.jsonl.gz in the folder: steps not compared');
  } else if (stepErrors.isEmpty) {
    print('steps match ($stepsChecked compared; volatility and offset identical; '
        'largest gaps: ${worst.entries.map((e) => '${e.key} ${e.value.toStringAsExponential(1)}').join(', ')})');
  } else {
    ok = false;
    print('STEPS DIFFER in ${stepErrors.length} of $stepsChecked. First: ${stepErrors.first}');
  }
  for (final coin in coins) {
    final suffix = meta['assets'][coin]['suffix'] as String;
    final csv = File('$folder/kalshi_trades$suffix.csv');
    final live = csv.existsSync()
        ? csv.readAsStringSync().split('\r\n').where((l) => l.isNotEmpty).toList()
        : <String>[];
    final ours = stores[coin]!.trades.map(csvLine).toList();
    final n = live.length > ours.length ? live.length : ours.length;
    final diffs = [
      for (var i = 0; i < n; i++)
        if (i >= live.length || i >= ours.length || live[i] != ours[i]) i
    ];
    if (diffs.isEmpty) {
      print('$coin: trades match (${live.isEmpty ? 0 : live.length - 1} rows)');
    } else {
      ok = false;
      final i = diffs.first;
      print('$coin: TRADES DIFFER. python ${live.length - 1} rows, dart ${ours.length - 1}; '
          '${diffs.length} line(s) differ, first at line ${i + 1}:');
      print('    python: ${i < live.length ? live[i] : '(missing)'}');
      print('    dart:   ${i < ours.length ? ours[i] : '(missing)'}');
    }

    final theirs = comparable(
        jsonDecode(File('$folder/kalshi_balance$suffix.json').readAsStringSync())
            as Map<String, dynamic>);
    // Through text and back, so it is plain JSON like the Python's file.
    final mine = comparable(jsonDecode(jsonEncode(stores[coin]!.state)) as Map<String, dynamic>);
    final diff = firstDifference(theirs, mine);
    if (diff == null) {
      print('$coin: saved state matches (${theirs.length} sections)');
    } else {
      ok = false;
      print('$coin: STATE DIFFERS at $diff');
    }
  }
  print(ok ? 'PARITY OK' : 'PARITY FAILED');
  exit(ok ? 0 : 1);
}
