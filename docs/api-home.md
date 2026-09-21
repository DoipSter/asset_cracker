# The home page's API

What the home page (`/`) reads. All `GET`, all read-only, all JSON, all relative URLs so the
page works behind the SSH tunnel on any port. Money is integer **cents**. Times are unix
seconds (float) unless the name ends in `_at` and the value is RFC 3339. Every figure is
simulated money; `simulated: true` is always present and the page must always say so.

The phone widget moves to `/widget` and keeps using `/api/status`, `/api/round`, `/api/minute`,
`/api/history`, which do not change.

## GET api/home?range=1H|24H|7D|ALL

The balance sheet. Cheap: served from memory and the once-a-minute value snapshots.

```json
{
  "simulated": true, "release": "87a2100", "as_of": 1790000000.0,
  "healthy": true, "halted": [],
  "total": {
    "value_cents": 2567890,          // everything: every live bucket marked to market, plus the four money buckets
    "earned_cents": -1234,           // value now minus value at the start of the range
    "earned_pct": -0.05,
    "range": "24H",
    "since": 1789950000.0,           // the snapshot actually compared with
    "window_complete": false,        // false when history is shorter than the range: say "since 9:42 PM", not "24H"
    "at_risk_cents": 45210,          // cost of bets open right now
    "unrealized_cents": 310          // open bets marked at the bid, minus their cost
  },
  "series": [[1789950000, 2569124], [1789950060, 2569001]],   // total value over the range, oldest first, at most 300 points
  "money": { "deployed_cents": 2560000, "winnings_cents": 0, "replenishment_cents": 0, "tax_reserve_cents": 0, "fee_reserve_cents": 0, "venue_fees_paid_cents": 8389 },
  "composition": [                   // adds up to total.value_cents, in this order; the page rules a line between each
    { "key": "strategies", "label": "Strategies",       "buckets": 6,  "value_cents": 598000,  "earned_cents": -2100 },
    { "key": "anti",       "label": "Anti-world twins", "buckets": 6,  "value_cents": 534000,  "earned_cents": -3900 },   // a measurement, not a competitor: say so
    { "key": "v1",         "label": "First engine",     "buckets": 12, "value_cents": 172000,  "earned_cents": 400 },
    { "key": "money",      "label": "Money buckets",    "buckets": 4,  "value_cents": 0,       "earned_cents": 0 }
  ],
  "assets": [
    {
      "coin": "BTC", "name": "Bitcoin", "sign": "₿", "colour": "#F7931A", "decimals": 2,
      "price": 81234.56, "price_age_s": 0.2,
      "change_pct": 0.41,            // over the range; null if unknown
      "stake_cents": 16179,          // cost of bets open on this coin, all buckets
      "stake_value_cents": 15010,    // those bets at the bid now; null if no bid
      "open_bets": 4,
      "earned_cents": -420,          // realised on this coin over the range
      "round": { "ticker": "KXBTC15M-26SEP210345-45", "strike": 81300.12, "closes": 1790000700.0, "yes_bid": 0.41, "yes_ask": 0.43 }
    }
  ]
}
```

`assets` is always the five coins in the order BTC, ETH, SOL, XRP, DOGE. A coin with no stake still appears.

How the figures are made (added when the API was built, 2026-09-21; all additive):

- **Value is marked at the bid, with no selling fee taken off.** An open bet with no bid (a round
  that has closed and not yet settled, or an empty book) is worth 0 and is counted:
  `total.unmarked_bets`, and `unmarked_bets` on each asset. A value chart can therefore dip for
  the few seconds between a round's close and its settlement; that is a bet nobody could price,
  not a loss. `stake_value_cents` is null only when a coin has bets open and none has a bid.
- **Earned never counts money put in.** `earned = (value now - contributed now) - (value then -
  contributed then)`. `total.contributed_cents` is what has come in from outside to date, and
  `total.lifetime_earned_cents` is value minus that: exact, and older than the snapshots, which
  only began when this release was deployed. `ALL` means since the FIRST SNAPSHOT, so it is not
  lifetime; `lifetime_earned_cents` is.
- **range** defaults to `24H`; anything else unknown is a 400. Before any snapshot exists:
  `earned_cents` 0, `since` = now, `window_complete` false, and `series` holds only the value
  right now. `series` always ends with the value right now.
- `since` is the snapshot really compared with: the newest one at or before the start of the
  range. If the service was down then, it can be older than the range asked for.
- `composition[].earned_cents` add up to `total.earned_cents`. A group's figure is null if its
  snapshot for that moment is missing (it is written in the same batch as the total, so this
  should not happen). The money buckets earn nothing by construction: what is in them was moved
  there, not made there.
- `assets[].earned_cents` is what was realised on ROUNDS OF THAT COIN THAT SETTLED since `since`:
  everything those rounds paid the buckets (settlements and early sales) less what the buckets
  paid for them, fees included, from the ledger. It is null if that could not be read.
- `assets[].change_pct` is the live price against the first Coinbase candle of the range. It is
  null for `ALL`, and null for the first poll or two after a start, until the candles arrive:
  the API never waits for Coinbase. `price` and `price_age_s` are null when no trade has been seen.
- `money.deployed_cents` is the cash in the live buckets right now. `venue_fees_paid_cents` and
  the four money buckets are as of the last settlement, bucket closure or service start, which
  is the only time the four can change; fees paid lags by up to a round.
- `healthy` is the health check's answer and also false when the ledger's side could not be
  read. `history_error` and `capital_error` (strings) appear only when a lookup failed, and say
  that the earned figures may be out of date; the last good figures are still served.
- Snapshots are looked up at most once a minute per range, so `earned_cents` moves with the live
  value every poll but its starting point moves once a minute.

## GET api/asset?coin=BTC&range=15M|1H|24H|7D

The chart box for one asset and the stake held in it.

```json
{
  "coin": "BTC", "range": "15M", "decimals": 2,
  "points": [[1790000000, 81234.56]],       // oldest first, at most 300
  "round": { "ticker": "...", "strike": 81300.12, "opens": 1789999800.0, "closes": 1790000700.0 },   // null outside 15M
  "stake_cents": 16179, "stake_value_cents": 15010,
  "positions": [
    { "strategy": "Scalper", "engine": "v2", "world": "real", "side": "UP", "contracts": 12, "entry_price": 0.31,
      "cost_cents": 385, "value_cents": 410, "placed": 1790000100.0, "underlying_at_entry": 81250.10, "ticker": "..." }
  ],
  "markers": [ { "t": 1790000100.0, "price": 81250.10, "side": "UP", "strategy": "Scalper", "world": "real", "kind": "bet" } ]   // bets and early sales inside the chart's span
}
```

`world` is `real` for a strategy and `anti` for its anti-world twin. `engine` is `v1` or `v2`.
`strategy` is the name the pair shares: the twin of Scalper is `"strategy": "Scalper", "world": "anti"`,
not "Anti Scalper". (A bucket's `name` in api/buckets keeps the engine's own name.)

Added when built: `simulated: true`; `unmarked_bets`; a position's `value_cents` is null when
there is no bid; each marker has `engine`, and `kind` is `bet` or `sold`; `markers_truncated` is
true when there were more than 500 in the span and only the newest 500 are given. `range` defaults
to `15M`; an unknown coin or range is a 400. `15M` points are the open round's recorded
evaluations, one per five seconds; the others are Coinbase candle closes (1H: one a minute, 24H:
five minutes, 7D: hourly), each timed at its candle's end. `round.opens` is `closes` minus the
series' configured round length, not Kalshi's own open time. Outside a round, `15M` gives
`"points": [], "round": null`. Markers come from the engines' memory: a strategy that ran out and
was staked again took its earlier life's bets with it.

## GET api/buckets

```json
{
  "policy": { "id": 1, "since_at": "2026-09-21T06:16:28Z", "note": "...", "winnings_bps": 0, "replenish_bps": 0, "tax_bps": 0, "fees_bps": 0 },
  "money": { "deployed_cents": 0, "winnings_cents": 0, "replenishment_cents": 0, "tax_reserve_cents": 0, "fee_reserve_cents": 0, "venue_fees_paid_cents": 0 },
  "buckets": [
    { "name": "kalshi15m2 Scalper v2", "engine": "v2", "strategy": "Scalper", "world": "real", "status": "active", "life": 1,
      "seed_cents": 100000, "equity_cents": 127693, "cash_cents": 120000, "at_risk_cents": 7000, "high_water_cents": 100000,
      "allocated_cents": 0, "bets": 36 }
  ],
  "events": [ { "at": "2026-09-21T06:15:11Z", "bucket": "kalshi15m2 Scalper v2", "kind": "allocated", "note": "..." } ]   // newest first, at most 30
}
```

`allocated_cents` is the sustainment allocation taken from that bucket to date. `status` is
`active`, `tripped`, `winding_down` or `frozen`; frozen buckets are listed after live ones.

Added when built: `simulated: true`; `unmarked_bets` per bucket. `equity_cents` is cash plus open
bets at the bid, the same marking as api/home (so it is a little above the widget's equity, which
takes the selling fee off). `high_water_cents` is null for the first engine, which takes no
sustainment allocation and keeps no mark. `bets` is the buys the database has recorded for that
bucket. A frozen bucket shows the cash the ledger says it holds, which after reaping is 0. `life`
is read from the bucket's name. `events[].note` is the stored detail put into words. The database
part of this document is kept for ten seconds.

## GET api/analysis

The evidence. Expensive, so computed at most once a minute and cached; `computed_at` says when.
**A result that cannot be told from zero must say so**: `verdict` is `unresolved` unless there
are at least 30 independent windows AND |t| >= 2.

```json
{
  "computed_at": 1790000000.0, "windows_recorded": 16,
  "scorecard": {
    "what": "Brier score of the model's probability against the market's own mid price, on settled rounds. Lower is better. The unit is one 15-minute window (all coins together), because bets inside a window are not independent.",
    "overall": { "n_rows": 20242, "n_windows": 11, "brier_model": 0.1635, "brier_market": 0.1561, "diff": 0.0074, "se": 0.0044, "t": 1.69, "verdict": "unresolved" },
    "by_band": [ { "band": "under 60 s", "n_rows": 0, "n_windows": 0, "brier_model": 0, "brier_market": 0, "diff": 0, "se": 0, "t": 0, "verdict": "unresolved" } ],
    "by_coin": [ { "coin": "BTC", "n_rows": 0, "n_windows": 0, "brier_model": 0, "brier_market": 0, "diff": 0, "se": 0, "t": 0, "verdict": "unresolved" } ]
  },
  "fills": {
    "what": "The simulator sells any size at the bid. This re-prices every early sale as if only the size actually displayed at that second had filled, the rest riding to settlement.",
    "by_strategy": [ { "strategy": "Scalper", "engine": "v2", "world": "real", "sells": 39, "contracts_sold": 774, "contracts_beyond_bid": 558,
                       "share_beyond": 0.72, "sale_pnl_cents": 35800, "sale_pnl_capped_cents": 13400, "unsettled_sells": 0,
                       "sells_without_depth": 12 } ]   // sales made before order-book depth was recorded (release 87a2100): counted, never guessed at
  },
  "leaderboard": {
    "what": "Per strategy version. Windows are the independent sample, not bets. Lifetime includes every earlier life of a strategy that ran out.",
    "rows": [ { "strategy": "Scalper", "engine": "v2", "world": "real", "lives": 1, "equity_cents": 127693, "lifetime_pnl_cents": 27693,
                "bets": 36, "windows": 5, "mean_window_pnl_cents": 5538, "se_cents": 5100, "t": 1.09,
                "top_window_share": 0.97, "verdict": "unresolved", "flags": ["one window is 97% of the result"] } ]
  }
}
```

Bands, in this order: `over 10 min`, `5 to 10 min`, `2 to 5 min`, `1 to 2 min`, `under 60 s`.
`diff` is model minus market: positive means the model is WORSE. `verdict` for the scorecard is
`model better`, `model worse` or `unresolved`; for the leaderboard `ahead`, `behind` or `unresolved`.
