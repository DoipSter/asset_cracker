/// All the strategy accounts for one coin, plus the shared price, volatility and index-gap
/// tracking. Ported from `KalshiTrader` in kalshi_trader.py.
library;

import 'dart:collection';
import 'dart:math' as math;

import 'account.dart';
import 'model.dart';
import 'pyfloat.dart';
import 'store.dart';

const csvFields = [
  'time', 'strategy', 'event', 'ticker', 'side', 'contracts', 'price', 'multiplier', 'fee',
  'cost', 'payout', 'pnl', 'result', 'strike', 'btc_price', 'final_value', 'balance_after',
  'model_prob', 'edge',
];

/// Python's `deque(maxlen=n)`: adding at one end past the limit drops from the other.
class _Bounded<T> {
  final int maxLen;
  final _q = ListQueue<T>();
  _Bounded(this.maxLen);

  void add(T v) {
    _q.addLast(v);
    if (_q.length > maxLen) _q.removeFirst();
  }

  void addFirst(T v) {
    _q.addFirst(v);
    if (_q.length > maxLen) _q.removeLast();
  }

  bool get isEmpty => _q.isEmpty;
  T get last => _q.last;
  set last(T v) {
    _q.removeLast();
    _q.addLast(v);
  }

  Iterable<T> get values => _q;
  void clear() => _q.clear();
}

/// One strategy's line on the leaderboard.
class Standing {
  final String name, blurb;
  final double equity, pnlPct;
  final int bets, wins, losses, joined;
  const Standing(this.name, this.blurb, this.equity, this.pnlPct, this.bets, this.wins,
      this.losses, this.joined);
}

/// Everything the display needs about one account.
class Snapshot {
  final String name, blurb;
  final double equity, pnl, pnlPct, cash;
  final List<Lot> open, log;
  final Market? market;
  final AccountView? view;
  final int bets, wins, losses, roundsMonitored, roundsJoined;
  final bool paused;
  const Snapshot(
      {required this.name,
      required this.blurb,
      required this.equity,
      required this.pnl,
      required this.pnlPct,
      required this.cash,
      required this.open,
      required this.log,
      required this.market,
      required this.view,
      required this.bets,
      required this.wins,
      required this.losses,
      required this.paused,
      required this.roundsMonitored,
      required this.roundsJoined});
}

class KalshiTrader {
  final Store store;
  final Clock clock;

  /// How far Kalshi's index runs above Coinbase's price for this coin, how uncertain that gap
  /// is, and the coin's typical volatility: the starting calibration.
  final double baseOffsetPct, sdPct, defaultSigma;

  /// (time, measured offset) for recent rounds: 24 rounds is about six hours.
  final _offsetObs = _Bounded<(double, double)>(24);
  late Map<String, Account> accounts;
  String selected = strategies.first.name;
  bool paused = false;
  late double startedAt;
  int roundsMonitored = 0; // 15-minute rounds the bots have watched
  String? _roundTicker; // the round being watched, so each is counted once

  late double sigma2;
  final _ring = _Bounded<(int, double)>(150); // (second, price), for the settlement average
  (int, double)? _volRef;
  final _minCloses = _Bounded<double>(60); // the price at the end of each finished minute
  (int, double)? _minLast; // (minute number, latest price in it), the minute in progress
  Market? market; // the latest quotes for the open window
  double _lastSave = 0.0;

  KalshiTrader(this.store,
      {required this.clock,
      double offsetPct = indexOffsetPct,
      this.sdPct = indexSdPct,
      this.defaultSigma = 8e-5})
      : baseOffsetPct = offsetPct {
    accounts = _freshAccounts();
    startedAt = clock.now();
    _load();
    _resetSignals();
  }

  static Map<String, Account> _freshAccounts() =>
      {for (final p in strategies) p.name: Account(p)};

  void _resetSignals() {
    sigma2 = sq(defaultSigma);
    _ring.clear();
    _volRef = null;
    _minCloses.clear();
    _minLast = null;
    market = null;
  }

  void _load() {
    final d = store.loadState();
    if (d == null) return;
    try {
      (d['accounts'] as Map<String, dynamic>).forEach((name, acct) {
        accounts[name]?.load(acct as Map<String, dynamic>);
      });
      selected = d['selected'] as String? ?? selected;
      _roundTicker = d['last_round_ticker'] as String?;
      if (d.containsKey('rounds_monitored')) {
        roundsMonitored = (d['rounds_monitored'] as num).toInt();
      } else {
        // a save from before rounds were counted: estimate from how long it has run
        final joined = accounts.values.map((a) => a.participated()).fold(0, math.max);
        final span = clock.now() - ((d['started_at'] as num?)?.toDouble() ?? clock.now());
        roundsMonitored = math.max(joined, pyFloorDiv(span, 900).toInt());
      }
      paused = d['paused'] as bool? ?? false;
      startedAt = (d['started_at'] as num?)?.toDouble() ?? startedAt;
      for (final row in (d['index_offsets'] as List? ?? [])) {
        _offsetObs.add(((row[0] as num).toDouble(), (row[1] as num).toDouble()));
      }
    } catch (_) {
      accounts = _freshAccounts(); // an older format: start every account fresh
    }
  }

  Account account([String? name]) => accounts[name ?? selected]!;

  void select(String name) {
    if (accounts.containsKey(name)) {
      selected = name;
      save(force: true);
    }
  }

  /// (ticker, close time) of every round some account still holds a bet in.
  List<(String, double)> pendingTickers() {
    final seen = <String, double>{};
    for (final acct in accounts.values) {
      for (final lot in acct.openLots()) {
        seen[lot.ticker] = lot.close;
      }
    }
    return [for (final e in seen.entries) (e.key, e.value)];
  }

  // ---- how far the index sits above our exchange price ------------------------

  List<double> _recentOffsets() {
    final cutoff = clock.now() - 6 * 3600; // older than this and the market has moved on
    return [for (final (t, o) in _offsetObs.values) if (t >= cutoff) o];
  }

  /// Kalshi settles on an index built from several exchanges' order books, which sits a
  /// little above our exchange's last trade, and that gap drifts within minutes. Every settled
  /// round is a free measurement (see [noteSettlement]); the median of the recent ones shrugs
  /// off the odd outlier. Until a few have come in, fall back to the starting constant.
  double get offsetPct {
    final recent = _recentOffsets();
    return recent.length >= 3 ? pyMedian(recent) : baseOffsetPct;
  }

  /// (offset now, how many recent measurements it rests on).
  (double, int) offsetStatus() => (offsetPct, _recentOffsets().length);

  bool _addOffset(double when, double offset) {
    if (offset.abs() <= 0.002) {
      // anything wilder than 0.2% is bad data, not a real gap
      _offsetObs.add((when, pyRound(offset, 8)));
      return true;
    }
    return false;
  }

  /// A round settled. Kalshi's settled value IS the index averaged over that round's final
  /// minute, so comparing it with our own average over the same minute measures the gap.
  void noteSettlement(double close, Object? finalValue) {
    final indexAvg = parseAmount(finalValue);
    if (indexAvg == null) return;
    final ours = [for (final (s, p) in _ring.values) if (close - 60 <= s && s < close) p];
    if (ours.length < 40 || indexAvg <= 0) return; // too few ticks to average fairly
    if (_addOffset(close, indexAvg / (pySum(ours) / ours.length) - 1)) save(force: true);
  }

  /// Prime the estimate from rounds that settled before we started.
  void seedOffsets(List<(double, double)> measurements) {
    final sorted = [...measurements]..sort((a, b) {
        final c = a.$1.compareTo(b.$1);
        return c != 0 ? c : a.$2.compareTo(b.$2);
      });
    for (final (at, offset) in sorted) {
      _addOffset(at, offset);
    }
    save(force: true);
  }

  // ---- volatility -------------------------------------------------------------

  static List<double> _logReturns(List<double> closes) => [
        for (var i = 0; i + 1 < closes.length; i++)
          if (closes[i] > 0 && closes[i + 1] > 0) math.log(closes[i + 1] / closes[i])
      ];

  /// Start from real recent volatility using 1-minute closes (oldest first).
  void seedVol(List<double> closes) {
    final rets = _logReturns(closes);
    if (rets.length >= 5) {
      sigma2 = _clamp(pySum(rets.map((r) => r * r)) / rets.length / 60);
    }
    // Older minutes go in front of any we've already collected live.
    for (final c in closes.reversed) {
      _minCloses.addFirst(c);
    }
  }

  /// Per-second variance of the price, from a run of one-minute closes.
  static double? _var(List<double> closes) {
    final rets = _logReturns(closes);
    return rets.isEmpty ? null : pySum(rets.map((r) => r * r)) / rets.length / 60;
  }

  static List<T> _tail<T>(List<T> list, int n) => list.sublist(math.max(0, list.length - n));

  /// What the Lottery strategy needs: how much volatility has just spiked (last ~5 minutes
  /// vs the last 45), and the chance of UP from a model that respects the spike.
  TailContext _tailContext(Market market, double price, double tau) {
    final closes = _minCloses.values.toList();
    final c45 = _tail(closes, 46), c6 = _tail(closes, 7);
    if (c45.length < 15 || c6.length < 4) return const TailContext(null, null);
    var s45 = _var(c45);
    final s5 = _var(c6);
    if (s45 == null || s45 == 0 || s5 == null) return const TailContext(null, null);
    s45 = _clamp(s45);
    final s2 = math.max(s45, s5) * sq(Lottery.fatten); // widen the tails a little
    final pTail = probYes(price, market.strike, tau, s2,
        knownAvg: tau < 60 ? _knownAvg(market.close) : null, offsetPct: offsetPct, sdPct: sdPct);
    return TailContext(pTail, math.sqrt(s5 / s45));
  }

  static double _clamp(double sigma2) => math.min(math.max(sigma2, sq(2e-5)), sq(3e-4));

  /// Feed every live price. Keeps a per-second record and updates volatility.
  void observe(double price, double ts) {
    final sec = ts.toInt();
    final minute = sec ~/ 60; // keep the last price of each minute, for the volatility windows
    if (_minLast != null && minute != _minLast!.$1) _minCloses.add(_minLast!.$2);
    _minLast = (minute, price);
    if (!_ring.isEmpty && _ring.last.$1 == sec) {
      _ring.last = (sec, price);
    } else {
      _ring.add((sec, price));
    }
    final ref = _volRef;
    if (ref == null) {
      _volRef = (sec, price);
    } else if (sec - ref.$1 >= 5) {
      // sample every 5s to dodge bid/ask bounce
      final dt = sec - ref.$1;
      final inst = sq(math.log(price / ref.$2)) / dt;
      final weight = 1 - math.pow(0.5, dt / 300).toDouble(); // ~5 minute half-life
      sigma2 = _clamp(sigma2 + weight * (inst - sigma2));
      _volRef = (sec, price);
    }
  }

  double? _knownAvg(double close) {
    final vals = [for (final (s, p) in _ring.values) if (s >= close - 60) p];
    return vals.isEmpty ? null : pySum(vals) / vals.length;
  }

  // ---- trading ----------------------------------------------------------------

  /// Give every strategy a look at the open window. Returns all their events.
  List<TradeEvent> step(Market market, double? price, double now) {
    this.market = market;
    if (price == null || price == 0) return [];
    if (market.ticker != _roundTicker) {
      // a new round started: count it once
      _roundTicker = market.ticker;
      roundsMonitored += 1;
    }
    final tau = market.close - now;
    final pModel = probYes(price, market.strike, tau, sigma2,
        knownAvg: tau < 60 ? _knownAvg(market.close) : null, offsetPct: offsetPct, sdPct: sdPct);
    final ctx = _tailContext(market, price, tau);
    final events = <TradeEvent>[];
    for (final acct in accounts.values) {
      events.addAll(acct.step(market, price, now, pModel, paused, ctx));
    }
    return _record(events, now);
  }

  /// A window closed and Kalshi reported the real result. Pay out or write off.
  List<TradeEvent> onSettled(String ticker, String? result, Object? finalValue, double now,
      double? price) {
    if (result != 'yes' && result != 'no') return [];
    final events = <TradeEvent>[];
    for (final acct in accounts.values) {
      events.addAll(acct.onSettled(ticker, result!, finalValue, now, price));
    }
    return _record(events, now);
  }

  List<TradeEvent> _record(List<TradeEvent> events, double now) {
    for (final e in events) {
      _appendCsv(e, now);
    }
    if (events.isNotEmpty) save(force: true);
    return events;
  }

  // ---- reporting --------------------------------------------------------------

  /// Every strategy, best balance first.
  List<Standing> standings() {
    final rows = <Standing>[];
    for (final a in accounts.values) {
      final eq = a.equity(market);
      rows.add(Standing(a.name, a.params.blurb, eq, (eq / startBalance - 1) * 100, a.bets,
          a.wins, a.losses, a.participated()));
    }
    // a stable sort, as Python's: equal balances keep the strategies' listed order
    final order = List.generate(rows.length, (i) => i)
      ..sort((i, j) {
        final c = rows[j].equity.compareTo(rows[i].equity);
        return c != 0 ? c : i.compareTo(j);
      });
    return [for (final i in order) rows[i]];
  }

  Snapshot snapshot([String? name]) {
    final a = account(name);
    final eq = a.equity(market);
    return Snapshot(
        name: a.name, blurb: a.params.blurb, equity: eq, pnl: eq - startBalance,
        pnlPct: (eq / startBalance - 1) * 100, cash: a.cash, open: a.openLots(), log: a.log,
        market: market, view: a.view, bets: a.bets, wins: a.wins, losses: a.losses,
        paused: paused, roundsMonitored: roundsMonitored, roundsJoined: a.participated());
  }

  // ---- files ------------------------------------------------------------------

  void _appendCsv(TradeEvent e, double now) {
    final lot = e.lot;
    final closed = e.kind != 'bet';
    final row = <Object?>[
      isoSeconds(now), e.strategy,
      const {'bet': 'BET', 'sold': 'SOLD', 'settled': 'SETTLED'}[e.kind],
      lot.ticker, lot.side, lot.contracts,
      e.kind == 'sold' ? lot.exitPrice : lot.price,
      lot.multiplier, lot.fee, lot.cost,
      closed ? lot.payout : '', closed ? lot.pnl : '',
      e.kind == 'settled' ? lot.result : '', lot.strike,
      closed ? lot.exitBtc : lot.btcPrice,
      e.kind == 'settled' ? lot.finalValue : '',
      pyRound(accounts[e.strategy]!.equity(market), 2),
      lot.modelProb, lot.edge,
    ];
    store.appendTrade(csvFields, [for (final v in row) pyStr(v)]);
  }

  /// Save the state. Throttled to every few seconds unless forced.
  void save({bool force = false}) {
    if (!force && clock.monotonic() - _lastSave < 5) return;
    _lastSave = clock.monotonic();
    final table = standings();
    store.saveState({
      'note': "Simulation only. No real money. Uses Kalshi's real 15-minute BTC prices.",
      'updated': isoSeconds(clock.now()),
      'starting_balance_each': startBalance,
      'selected': selected,
      'leader': table.first.name,
      'paused': paused,
      'started_at': startedAt,
      'rounds_monitored': roundsMonitored,
      'last_round_ticker': _roundTicker,
      'index_offset_pct': pyRound(offsetPct, 8),
      'index_offset_note': "how far Kalshi's index sits above our exchange price, "
          'learned from recent settlements',
      'index_offsets': [for (final (t, o) in _offsetObs.values) [pyRound(t, 3), o]],
      'leaderboard': [
        for (final r in table)
          {
            'strategy': r.name, 'balance': pyRound(r.equity, 2),
            'return_pct': pyRound(r.pnlPct, 2), 'bets': r.bets, 'rounds_joined': r.joined,
          }
      ],
      'accounts': {for (final a in accounts.values) a.name: a.toJson(a.equity(market))},
    });
  }

  void setPaused(bool value) {
    paused = value;
    save(force: true);
  }

  /// Start every account over with the starting balance. The old files are kept, renamed.
  void reset() {
    final d = DateTime.fromMicrosecondsSinceEpoch((clock.now() * 1e6).round());
    String two(int v) => v.toString().padLeft(2, '0');
    store.archive('${d.year}${two(d.month)}${two(d.day)}_${two(d.hour)}${two(d.minute)}${two(d.second)}');
    accounts = _freshAccounts();
    startedAt = clock.now();
    roundsMonitored = 0;
    _roundTicker = null;
    save(force: true); // the learned index offset is about the market, so it stays
  }
}
