/// One strategy's paper account: what it holds, and how it decides to bet, sell and settle.
/// Ported from `Account` in kalshi_trader.py.
library;

import 'dart:math' as math;

import 'model.dart';
import 'pyfloat.dart';

/// A bet, from the moment it is placed. It carries its own status and, once closed, how it
/// ended. The JSON keys are the ones the Python app writes, so either app can load the file.
class Lot {
  final int id, contracts;
  final double t, price, multiplier, fee, cost, strike, close, btcPrice, modelProb, edge;
  final String time, ticker, side; // side: "UP" or "DOWN"
  String status; // open, sold, won, lost
  double? payout, pnl, exitT, exitPrice, exitBtc;
  String? result;
  Object? finalValue; // as Kalshi sent it: often a string like "81094.00"

  Lot(
      {required this.id,
      required this.t,
      required this.time,
      required this.ticker,
      required this.side,
      required this.contracts,
      required this.price,
      required this.multiplier,
      required this.fee,
      required this.cost,
      required this.strike,
      required this.close,
      required this.btcPrice,
      required this.modelProb,
      required this.edge,
      this.status = 'open'});

  factory Lot.fromJson(Map<String, dynamic> m) {
    double n(String key) => (m[key] as num).toDouble();
    double? opt(String key) => (m[key] as num?)?.toDouble();
    return Lot(
        id: m['id'] as int,
        t: n('t'),
        time: m['time'] as String,
        ticker: m['ticker'] as String,
        side: m['side'] as String,
        contracts: (m['contracts'] as num).toInt(),
        price: n('price'),
        multiplier: n('multiplier'),
        fee: n('fee'),
        cost: n('cost'),
        strike: n('strike'),
        close: n('close'),
        btcPrice: n('btc_price'),
        modelProb: opt('model_prob') ?? 0.0,
        edge: opt('edge') ?? 0.0,
        status: m['status'] as String)
      ..payout = opt('payout')
      ..pnl = opt('pnl')
      ..exitT = opt('exit_t')
      ..exitPrice = opt('exit_price')
      ..exitBtc = opt('exit_btc')
      ..result = m['result'] as String?
      ..finalValue = m['final_value'];
  }

  Map<String, dynamic> toJson() => {
        'id': id, 't': t, 'time': time, 'ticker': ticker, 'side': side,
        'contracts': contracts, 'price': price, 'multiplier': multiplier, 'fee': fee,
        'cost': cost, 'strike': strike, 'close': close, 'btc_price': btcPrice,
        'model_prob': modelProb, 'edge': edge, 'status': status,
        if (status != 'open') ...{'payout': payout, 'pnl': pnl, 'exit_t': exitT},
        if (status == 'sold') 'exit_price': exitPrice,
        if (status != 'open') 'exit_btc': exitBtc,
        if (status == 'won' || status == 'lost') ...{'result': result, 'final_value': finalValue},
      };

  Lot copy() => Lot.fromJson(toJson());
}

/// Something that happened to a bet: placed, sold early, or settled. [lot] is a snapshot
/// taken at that moment.
class TradeEvent {
  final String kind; // bet, sold, settled
  final String strategy;
  final Lot lot;
  final bool? won; // settled only

  TradeEvent(this.kind, this.strategy, Lot lot, {this.won}) : lot = lot.copy();
}

/// One side of the market as a strategy sees it: what it costs and what it is worth.
class Option {
  final String side;
  final double ask, cost, p, size, edge, mult;
  const Option(this.side, this.ask, this.cost, this.p, this.size, this.edge, [this.mult = 0]);
}

/// Would this strategy bet right now, and if not, why not. Shown in the side panel.
class Signal {
  final String side, why;
  final double conf, edge, need;
  final bool bet;
  const Signal(this.side, this.conf, this.edge, this.need, this.bet, this.why);
}

/// What the model thinks right now, for the display.
class AccountView {
  final double pUp, pModel, mid, tau;
  final Option best;
  final Signal signal;
  const AccountView(this.pUp, this.pModel, this.mid, this.best, this.tau, this.signal);
}

/// What the Lottery strategy needs from the trader: the chance of UP from a model that
/// respects a recent volatility spike, and the size of that spike. Null until there is
/// enough history.
class TailContext {
  final double? pTail, spike;
  const TailContext(this.pTail, this.spike);
}

class Account {
  final StrategyParams params;
  double cash = startBalance;
  List<Lot> log = []; // every bet placed, oldest first
  int bets = 0, wins = 0, losses = 0, nextId = 1;
  double realizedPnl = 0.0;
  AccountView? view;

  Account(this.params);

  String get name => params.name;

  // ---- persistence ----------------------------------------------------------

  void load(Map<String, dynamic> d) {
    cash = (d['cash'] as num).toDouble();
    log = [for (final lot in (d['log'] as List? ?? [])) Lot.fromJson(lot as Map<String, dynamic>)];
    bets = (d['bets'] as num? ?? 0).toInt();
    wins = (d['wins'] as num? ?? 0).toInt();
    losses = (d['losses'] as num? ?? 0).toInt();
    realizedPnl = (d['realized_pnl'] as num? ?? 0.0).toDouble();
    nextId = 1 + log.fold(0, (m, lot) => math.max(m, lot.id));
  }

  Map<String, dynamic> toJson(double equity) => {
        'strategy': params.blurb,
        'balance': pyRound(equity, 2),
        'profit': pyRound(equity - startBalance, 2),
        'return_pct': pyRound((equity / startBalance - 1) * 100, 3),
        'cash': pyRound(cash, 2),
        'realized_pnl': pyRound(realizedPnl, 2),
        'bets': bets, 'wins': wins, 'losses': losses,
        'log': [for (final lot in log.skip(math.max(0, log.length - logKept))) lot.toJson()],
      };

  // ---- helpers --------------------------------------------------------------

  List<Lot> openLots([String? ticker]) => [
        for (final lot in log)
          if (lot.status == 'open' && (ticker == null || lot.ticker == ticker)) lot
      ];

  /// Cash plus what open bets could be sold for right now, after the selling fee (or their
  /// cost, if there's no bid to price them by).
  double equity(Market? market) {
    var total = cash;
    for (final lot in openLots()) {
      var bid = 0.0;
      if (market != null && market.ticker == lot.ticker) {
        bid = lot.side == 'UP' ? market.yesBid : market.noBid;
      }
      if (bid > 0) {
        total += lot.contracts * bid - kalshiFee(lot.contracts, bid);
      } else {
        total += lot.cost;
      }
    }
    return total;
  }

  /// How many different rounds this strategy has bet in.
  int participated() => {for (final lot in log) lot.ticker}.length;

  // ---- deciding -------------------------------------------------------------

  /// Look at the open window: maybe sell, maybe bet.
  List<TradeEvent> step(Market market, double? price, double now, double pModel, bool paused,
      [TailContext ctx = const TailContext(null, null)]) {
    final tau = market.close - now;
    final ya = market.yesAsk, na = market.noAsk, yb = market.yesBid;
    if (price == null || price == 0 || ya <= 0 || na <= 0) {
      view = null;
      return [];
    }
    if (params.lottery) return _lottery(market, price, now, pModel, paused, ctx);

    final mid = yb > 0 ? (yb + ya) / 2 : ya;
    final pUp = mid + params.shrink * (pModel - mid); // blend with what the market believes
    final options = [
      _quote('UP', ya, pUp, market.yesAskSize, slippage, 0.99),
      _quote('DOWN', na, 1 - pUp, market.noAskSize, slippage, 0.99),
    ];
    final events = <TradeEvent>[];
    if (!paused && tau >= minTau && params.exit == 'ev') {
      events.addAll(_exits(market, now, pUp, price));
    }
    // Kalshi keeps one net position per market: you can't hold UP and DOWN at once. So once
    // a strategy holds a side in this round it may only add to that side, or sell it.
    final held = {for (final lot in openLots(market.ticker)) lot.side};
    final best = _firstMax(options.where((o) => held.isEmpty || held.contains(o.side)));
    view = AccountView(pUp, pModel, mid, best, tau, _signal(market, now, best, tau, paused));
    if (paused || tau < minTau) return events;
    return events..addAll(_entries(market, now, price, best, tau));
  }

  /// One side priced for buying: the ask plus slippage (capped), and the edge left after the
  /// fee if the side is worth [p].
  static Option _quote(String side, double ask, double p, double size, double slip, double cap) {
    final c = math.min(cap, ask + slip);
    return Option(side, ask, c, p, size, p - c - feeRate * c * (1 - c), p / ask);
  }

  /// Python's `max(..., key=edge)`: the first of equals wins.
  static Option _firstMax(Iterable<Option> options) {
    Option? best;
    for (final o in options) {
      if (best == null || o.edge > best.edge) best = o;
    }
    return best!;
  }

  /// Buy the cheap side when it's much likelier than its price and volatility just spiked.
  /// One small bet per round, held to settlement.
  List<TradeEvent> _lottery(Market market, double price, double now, double pModel, bool paused,
      TailContext ctx) {
    final tau = market.close - now;
    final ya = market.yesAsk, na = market.noAsk, yb = market.yesBid;
    final pTail = ctx.pTail, spike = ctx.spike;
    final pUp = pTail ?? pModel;
    final options = [
      _quote('UP', ya, pUp, market.yesAskSize, Lottery.slippage, 0.999),
      _quote('DOWN', na, 1 - pUp, market.noAskSize, Lottery.slippage, 0.999),
    ];
    final cheap = options[1].ask < options[0].ask ? options[1] : options[0]; // the longshot
    final already = log.any((lot) => lot.ticker == market.ticker);

    String? why;
    if (paused) {
      why = 'paused';
    } else if (pTail == null) {
      why = 'collecting volatility history';
    } else if (tau < Lottery.minTau) {
      why = 'only bets with ${Lottery.minTau ~/ 60}+ min left';
    } else if (cheap.ask > Lottery.maxAsk) {
      why = 'no cheap side (needs ${(Lottery.maxAsk * 100).toStringAsFixed(0)}¢ or less)';
    } else if (already) {
      why = 'already bet this round';
    } else if (spike! < Lottery.minSpike) {
      why = 'volatility calm (${spike.toStringAsFixed(1)}x, '
          'needs ${Lottery.minSpike.toStringAsFixed(1)}x)';
    } else if (cheap.mult < Lottery.minMult) {
      why = 'only ${cheap.mult.toStringAsFixed(1)}x likelier '
          '(needs ${Lottery.minMult.toStringAsFixed(0)}x)';
    }
    final mid = yb > 0 ? (yb + ya) / 2 : ya;
    view = AccountView(pUp, pModel, mid, cheap, tau,
        Signal(cheap.side, cheap.p * 100, cheap.edge * 100, 0.0, why == null, why ?? 'will bet'));
    if (why != null || tau < minTau) return [];

    final unit = cheap.cost + feeRate * cheap.cost * (1 - cheap.cost);
    var n = math.min(pyFloorDiv(Lottery.stake * cash, unit).toInt(), cheap.size.toInt());
    while (n > 0 && n * cheap.cost + kalshiFee(n, cheap.cost) > cash) {
      n -= 1;
    }
    if (n < 1) return [];
    return [_bet(market, cheap, n, price, now)];
  }

  /// Mirrors the checks in [_entries], so what the display says is what the bot will do.
  Signal _signal(Market market, double now, Option best, double tau, bool paused) {
    final here = openLots(market.ticker);
    String clock(double sec) => '${sec.toInt() ~/ 60}:${(sec.toInt() % 60).toString().padLeft(2, '0')}';
    String? why;
    if (paused) {
      why = 'paused';
    } else if (tau < minTau || tau < params.tauMin) {
      why = 'too late this round';
    } else if (tau > params.tauMax) {
      why = 'waits for last ${clock(params.tauMax)}';
    } else if (here.length >= params.maxBets) {
      why = 'max bets this round';
    } else if (here.isNotEmpty && now - here.map((l) => l.t).reduce(math.max) < minGap) {
      why = 'cooling down';
    } else if (!(params.bandMin <= best.cost && best.cost <= params.bandMax)) {
      why = '${(best.cost * 100).toStringAsFixed(0)}¢ outside its range';
    } else if (best.edge < params.minEdge) {
      final e = best.edge * 100;
      why = 'edge ${e >= 0 ? '+' : ''}${e.toStringAsFixed(1)}¢ of '
          '${(params.minEdge * 100).toStringAsFixed(0)}¢ needed';
    }
    return Signal(best.side, best.p * 100, best.edge * 100, params.minEdge * 100, why == null,
        why ?? 'will bet');
  }

  List<TradeEvent> _entries(Market market, double now, double price, Option best, double tau) {
    final here = openLots(market.ticker);
    final c = best.cost;
    if (!(params.tauMin <= tau && tau <= params.tauMax) ||
        best.edge < params.minEdge ||
        !(params.bandMin <= c && c <= params.bandMax) ||
        here.length >= params.maxBets ||
        (here.isNotEmpty && now - here.map((l) => l.t).reduce(math.max) < minGap)) {
      return [];
    }
    final committed = pySum(here.map((l) => l.cost));
    final room = windowCap * (cash + committed) - committed;
    final unit = c + feeRate * c * (1 - c); // cost of one contract, fee included
    final kelly = (best.p - unit) / (1 - unit);
    final stake = math.min(math.min(kellyFraction * kelly, maxStake) * cash, room);
    var n = math.min(pyFloorDiv(stake, unit).toInt(), best.size.toInt());
    while (n > 0 && n * c + kalshiFee(n, c) > cash) {
      n -= 1;
    }
    if (n < 1) return [];
    return [_bet(market, best, n, price, now)];
  }

  TradeEvent _bet(Market market, Option option, int n, double price, double now) {
    final c = option.cost;
    final fee = kalshiFee(n, c);
    final cost = pyRound(n * c + fee, 2);
    cash -= cost;
    final lot = Lot(
        id: nextId, t: now, time: isoSeconds(now), ticker: market.ticker, side: option.side,
        contracts: n, price: pyRound(c, 2), multiplier: pyRound(1 / c, 2), fee: fee, cost: cost,
        strike: market.strike, close: market.close, btcPrice: price,
        modelProb: pyRound(option.p, 3), edge: pyRound(option.edge, 3));
    nextId += 1;
    bets += 1;
    log.add(lot);
    return TradeEvent('bet', name, lot);
  }

  /// Sell early when the market's bid beats what we think a bet is worth.
  List<TradeEvent> _exits(Market market, double now, double pUp, double price) {
    final events = <TradeEvent>[];
    for (final lot in openLots(market.ticker)) {
      final bid = lot.side == 'UP' ? market.yesBid : market.noBid;
      if (bid <= 0 || now - lot.t < 15) continue;
      final pSide = lot.side == 'UP' ? pUp : 1 - pUp;
      final sellC = math.max(0.01, bid - slippage);
      if (sellC - feeRate * sellC * (1 - sellC) > pSide + exitMargin) {
        final n = lot.contracts;
        final proceeds = pyRound(n * sellC - kalshiFee(n, sellC), 2);
        lot
          ..exitPrice = pyRound(sellC, 2)
          ..exitBtc = price;
        _close(lot, 'sold', proceeds, now);
        events.add(TradeEvent('sold', name, lot));
      }
    }
    return events;
  }

  void _close(Lot lot, String status, double payout, double now) {
    lot
      ..status = status
      ..payout = payout
      ..pnl = pyRound(payout - lot.cost, 2)
      ..exitT = now;
    cash += payout;
    realizedPnl += lot.pnl!;
    if (lot.pnl! > 0) {
      wins += 1;
    } else {
      losses += 1;
    }
  }

  List<TradeEvent> onSettled(String ticker, String result, Object? finalValue, double now,
      double? price) {
    final events = <TradeEvent>[];
    for (final lot in openLots(ticker)) {
      final won = (result == 'yes') == (lot.side == 'UP');
      lot
        ..result = result
        ..finalValue = finalValue
        ..exitBtc = price;
      _close(lot, won ? 'won' : 'lost', won ? lot.contracts.toDouble() : 0.0, now);
      events.add(TradeEvent('settled', name, lot, won: won));
    }
    return events;
  }
}

/// A unix time as local `YYYY-MM-DDTHH:MM:SS`, as Python's
/// `datetime.fromtimestamp(ts).isoformat(timespec="seconds")`.
String isoSeconds(double ts) {
  final d = DateTime.fromMicrosecondsSinceEpoch((ts * 1e6).round());
  String two(int v) => v.toString().padLeft(2, '0');
  return '${d.year.toString().padLeft(4, '0')}-${two(d.month)}-${two(d.day)}'
      'T${two(d.hour)}:${two(d.minute)}:${two(d.second)}';
}
