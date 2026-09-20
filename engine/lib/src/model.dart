/// The pricing model, fees and strategy parameters. Ported from kalshi_trader.py; every
/// constant and every arithmetic expression keeps the Python's order of operations, because
/// the parity gate compares trade logs row for row.
///
/// Simulated money only. Nothing here can place an order.
library;

import 'dart:math' as math;

import 'pyfloat.dart';

const startBalance = 150.0;

const feeRate = 0.07; // Kalshi's taker fee: 7% x price x (1 - price) per contract, rounded up
const slippage = 0.01; // we pay one cent worse than the displayed price, buying or selling
const kellyFraction = 0.25; // bet this fraction of the Kelly-optimal stake
const maxStake = 0.20; // never put more than this share of cash into one bet
const windowCap = 0.35; // ...or more than this share of the account into one window
const minGap = 20; // seconds between bets in the same window
const exitMargin = 0.02; // an early sale must beat our estimate of the hold value by this
const minTau = 8; // stop trading when this few seconds remain (orders take time)

// Kalshi settles on CF Benchmarks' index, not on Coinbase's trades. These are the BTC
// defaults the Python measured over 664 settled rounds (Sep 13-20 2026).
const indexOffsetPct = 0.000057; // the index sits this far above Coinbase, on average
const indexSdPct = 0.000144; // ...and the gap is uncertain by this much (one sd)
const defaultSigma = 8e-5; // BTC's typical volatility, per sqrt(second), until measured
const logKept = 500;

/// The "Lottery" strategy's settings. Kept as a documented negative result: see the README.
class Lottery {
  static const maxAsk = 0.15; // only cheap sides: 15 cents or less
  static const minMult = 3.0; // the model must say it's this many times likelier than the price
  static const minSpike = 1.3; // volatility over the last ~5 minutes vs the last 45
  static const minTau = 180; // only with 3+ minutes left, so a reversal has time to happen
  static const stake = 0.01; // 1% of cash per bet
  static const fatten = 1.15; // widen the volatility a little: real tails are fatter
  static const slippage = 0.005; // cheap contracts tick in tenths of a cent
}

class StrategyParams {
  final String name, blurb, exit; // exit: "hold" or "ev"
  final bool lottery;
  final double shrink, minEdge, tauMin, tauMax, bandMin, bandMax;
  final int maxBets;

  const StrategyParams(this.name, this.blurb,
      {required this.shrink,
      required this.minEdge,
      required this.maxBets,
      required this.exit,
      required this.tauMin,
      required this.tauMax,
      required this.bandMin,
      required this.bandMax,
      this.lottery = false});
}

const strategies = [
  StrategyParams('Value', 'Model + market blend, holds',
      shrink: 0.5, minEdge: 0.03, maxBets: 1, exit: 'hold',
      tauMin: 8, tauMax: 900, bandMin: 0.05, bandMax: 0.95),
  StrategyParams('Model', 'Trusts the model, adds bets',
      shrink: 1.0, minEdge: 0.05, maxBets: 3, exit: 'hold',
      tauMin: 8, tauMax: 900, bandMin: 0.05, bandMax: 0.95),
  StrategyParams('Late', 'Only bets the last 2.5 minutes',
      shrink: 1.0, minEdge: 0.03, maxBets: 2, exit: 'hold',
      tauMin: 8, tauMax: 150, bandMin: 0.05, bandMax: 0.95),
  StrategyParams('Scalper', 'In and out, cuts losers early',
      shrink: 0.5, minEdge: 0.03, maxBets: 3, exit: 'ev',
      tauMin: 25, tauMax: 900, bandMin: 0.05, bandMax: 0.95),
  StrategyParams('Favorite', 'Backs the favorite late',
      shrink: 1.0, minEdge: 0.0, maxBets: 1, exit: 'hold',
      tauMin: 8, tauMax: 240, bandMin: 0.62, bandMax: 0.88),
  StrategyParams('Lottery', 'Cheap longshots after a vol spike',
      shrink: 1.0, minEdge: 0.0, maxBets: 1, exit: 'hold',
      tauMin: 180, tauMax: 900, bandMin: 0.0, bandMax: Lottery.maxAsk, lottery: true),
];

/// The open round and its live quotes, in dollars per contract.
class Market {
  final String ticker;
  final double strike, close; // close: unix seconds
  final double yesBid, yesAsk, noBid, noAsk, yesAskSize, noAskSize;

  const Market(
      {required this.ticker,
      required this.strike,
      required this.close,
      required this.yesBid,
      required this.yesAsk,
      required this.noBid,
      required this.noAsk,
      required this.yesAskSize,
      required this.noAskSize});

  factory Market.fromJson(Map<String, dynamic> m) {
    double n(String key) => (m[key] as num).toDouble();
    return Market(
        ticker: m['ticker'] as String,
        strike: n('strike'),
        close: n('close'),
        yesBid: n('yes_bid'),
        yesAsk: n('yes_ask'),
        noBid: n('no_bid'),
        noAsk: n('no_ask'),
        yesAskSize: n('yes_ask_size'),
        noAskSize: n('no_ask_size'));
  }

  Map<String, dynamic> toJson() => {
        'ticker': ticker, 'strike': strike, 'close': close,
        'yes_bid': yesBid, 'yes_ask': yesAsk, 'no_bid': noBid, 'no_ask': noAsk,
        'yes_ask_size': yesAskSize, 'no_ask_size': noAskSize,
      };
}

/// A number from Kalshi, which sometimes arrives as a string with thousands separators
/// ("79,604.96"). Null if it isn't a usable number.
double? parseAmount(Object? value) {
  if (value is num) return value.toDouble();
  if (value == null) return null;
  return double.tryParse(value.toString().replaceAll(',', '').trim());
}

/// Kalshi's taker fee in dollars, rounded up to the next cent.
double kalshiFee(int contracts, double price) =>
    (feeRate * contracts * price * (1 - price) * 100 - 1e-9).ceilToDouble() / 100;

double normCdf(double x) => 0.5 * (1 + erf(x / math.sqrt(2)));

/// `x ** 2` as Python computes it: through the C library's pow.
double sq(double x) => math.pow(x, 2).toDouble();

/// Chance the settlement value (the index averaged over the round's last 60 seconds) ends at
/// or above the strike. [price] and [knownAvg] are Coinbase prices; the strike is on Kalshi's
/// index, which runs slightly higher, so both are lifted by [offsetPct]. [tau] is seconds
/// left; [knownAvg] is our own average over the part of the final minute already seen.
double probYes(double price, double strike, double tau, double sigma2,
    {double? knownAvg, double offsetPct = indexOffsetPct, double sdPct = indexSdPct}) {
  final lift = 1 + offsetPct;
  double mean, variance;
  if (tau >= 60) {
    // Walk to the start of the final minute, then average a 60-second walk.
    mean = price * lift;
    variance = sigma2 * sq(price) * ((tau - 60) + 60 / 3);
  } else {
    final seen = (60 - tau) / 60; // how much of the averaging window is already known
    mean = (knownAvg == null ? price : seen * knownAvg + (1 - seen) * price) * lift;
    variance = sq(tau / 60) * sigma2 * sq(price) * tau / 3;
  }
  final sd = math.sqrt(variance + sq(sdPct * price)); // our price vs the index: uncertain
  return math.min(0.999, math.max(0.001, normCdf((mean - strike) / sd)));
}
