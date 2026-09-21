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
  `total.unmarked_bets`, and `unmarked_bets` on each asset. The LIVE value can therefore dip for
  the few seconds between a round's close and its settlement; that is a bet nobody could price,
  not a loss. `stake_value_cents` is null only when a coin has bets open and none has a bid.
- **No snapshot is written while the books cannot be trusted or priced.** The once-a-minute
  writer skips the whole minute while any open bet's round has already closed and not yet
  settled (the dip above), and while any engine is halted (its memory and the ledger disagree).
  The table is append-only, so a minute missing from `series` is honest where a false step
  would be permanent. As a second line of defence a range never STARTS from a snapshot that
  counted any unmarked bet (see `since`). A bet with no bid in a round still open, such as a
  losing side late on, does not stop a snapshot being written: it is fairly worth about nothing.
- **A live bucket no engine holds is counted at its ledger cash.** Setting a series to recording
  only, or switching the second engine off, leaves its buckets' cash in the ledger; they stay in
  their group's value, count and contribution, so a configuration change never reads as a loss.
  A bet such a bucket still had open is not counted: nothing in memory can mark it.
- **Earned never counts money put in.** `earned = (value now - contributed now) - (value then -
  contributed then)`. `total.contributed_cents` is what has come in from outside to date, and
  `total.lifetime_earned_cents` is value minus that: exact, and older than the snapshots, which
  only began when this release was deployed. `ALL` means since the FIRST SNAPSHOT, so it is not
  lifetime; `lifetime_earned_cents` is.
- **range** defaults to `24H`; anything else unknown is a 400. Before any snapshot exists:
  `earned_cents` 0, `since` = now, `window_complete` false, and `series` holds only the value
  right now. `series` always ends with the value right now.
- `since` is the snapshot really compared with: the newest FULLY PRICED one (no unmarked bets)
  at or before the start of the range. It can therefore be a few minutes older than the range
  asked for, late in a round when a losing side had no bid, and older still if the service was
  down then. `ALL` likewise starts from the first fully priced snapshot.
- `composition[].earned_cents` add up to `total.earned_cents`. A group's figure is null if its
  snapshot for that moment is missing (it is written in the same batch as the total, so this
  should not happen). The money buckets earn nothing by construction: what is in them was moved
  there, not made there.
- `assets[].earned_cents` is what was realised on ROUNDS OF THAT COIN THAT SETTLED since `since`:
  everything those rounds paid the buckets (settlements and early sales) less what the buckets
  paid for them, fees included, from the ledger. A round counts only once every bucket that
  held contracts in it to the end has its settlement booked, so it never appears with its
  stakes paid and its winnings still to come; a round an engine never booked (it was halted)
  stays out. It is null if that could not be read.
- `assets[].change_pct` is the live price against the first Coinbase candle of the range. It is
  null for `ALL`, and null for the first poll or two after a start, until the candles arrive:
  the API never waits for Coinbase. It goes back to null when the candles have not been fetched
  successfully for two candle widths (and never less than five minutes; a chosen allowance, not
  a measured one): against an older candle it would no longer be the change over the range.
  `price` and `price_age_s` are null when no trade has been seen.
- `money.deployed_cents` is the cash in the live buckets right now. `venue_fees_paid_cents` and
  the four money buckets are as of the last settlement, bucket closure or service start, which
  is the only time the four can change; fees paid lags by up to a round.
- `healthy` is the health check's answer, at most five seconds old (it pings the database, and
  this document is otherwise served from memory), and also false when the ledger's side could
  not be read.
- `history_error` and `capital_error` (strings) appear only when a lookup failed. If an earlier
  lookup had worked, the last good figures are still served and the string says they may be out
  of date. If none ever has, there are no figures, and NULL is served, never 0:
  - the ledger never read since the service started: `total.contributed_cents`,
    `total.lifetime_earned_cents`, `total.earned_cents`, `total.earned_pct`, every
    `composition[].earned_cents` and every field of `money` except `deployed_cents` are null,
    and `total.value_cents` leaves the money buckets out. `capital_error` says so.
  - the value history never read: `total.earned_cents`, `total.earned_pct`, every
    `composition[].earned_cents` and every `assets[].earned_cents` are null. (Before any
    snapshot EXISTS, which is a lookup that worked, they are 0 as above.)
- Snapshots are looked up at most once a minute per range, so `earned_cents` moves with the live
  value every poll but its starting point moves once a minute.

## GET api/asset?coin=BTC&range=15M|1H|24H|7D

The chart box for one asset and the stake held in it.

```json
{
  "coin": "BTC", "range": "15M", "decimals": 2,
  "points": [[1790000000, 81234.56]],       // oldest first, at most 300
  "round": { "ticker": "...", "strike": 81300.12, "opens": 1789999800.0, "opens_measured": true, "closes": 1790000700.0 },   // null outside 15M
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
five minutes, 7D: hourly), each timed at its candle's end, except the newest, which is still
forming and is timed at the moment it was fetched (so no point is ever later than now).
`round.opens` is Kalshi's own open time for the round and `round.opens_measured` is true; if
Kalshi sent none, it is `closes` minus the series' configured round length and
`opens_measured` is false. Outside a round, `15M` gives
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
is read from the bucket's name. `events[].note` is the stored detail put into words; a part the
stored detail does not have is left out of the note, not shown as $0.00.

The database part of this document (name, status, life, seed, allocated, bets, events, policy) is
read at most once a minute, whether the read works or not; cash, equity, at-risk, high-water and
unmarked come from the engines' books on every request. When a read fails, the last good list is
served and `buckets_error` (a string) says so; only if no read has ever worked is the answer a
500. `capital_error` and null `money` fields mean what they mean in api/home.

## GET api/analysis

The evidence. Expensive, so computed at most once a minute and cached; `computed_at` says when.
The service keeps it warm by itself, from the moment it starts, whether or not anybody has the
page open. **A result that cannot be told from zero must say so**: `verdict` is `unresolved`
unless there are at least 30 independent windows AND |t| reaches the threshold in `conventions`
that applies to that row, AND `coverage.complete` is true.

```json
{
  "simulated": true,
  "computed_at": 1790000000.0, "windows_recorded": 16,
  "stale": false,                    // true: the last refresh failed and this is the document before it; "stale_reason" (string) is then present
  "coverage": {                      // how much of the settled history the figures rest on
    "markets_settled": 80,           // rounds with a result
    "markets_aggregated": 80,        // of those, read and in hand
    "markets_unreconciled": 0,       // read and held back: their settlement rows do not match what was held at the close
    "windows_incomplete": 0,         // windows left out whole: a market in them is unsettled, unreconciled or not yet read
    "complete": true                 // markets_aggregated has reached markets_settled. While false, EVERY verdict is unresolved
  },
  "conventions": {                   // the verdict rule. Conventions chosen in advance, not measurements
    "note": "...",                   // the rule in words: which threshold applies to which rows, and what the correction is for
    "min_windows": 30,
    "min_abs_t": 2,                  // one hypothesis stated in advance: scorecard.overall
    "family_alpha": 0.05,            // rows compared side by side: the chance that ANY of them gets a false verdict
    "trials": 18,                    // MEASURED: select count(*) from strategy_version. 0 = could not be read
    "leaderboard_min_abs_t": 2.9913, // two-sided Bonferroni for `trials` rows: sqrt(2) x erfinv(1 - family_alpha/trials), never below min_abs_t. 0 when trials is 0
    "scorecard_cuts": 10,            // the five bands and five coins
    "scorecard_cut_min_abs_t": 2.807,   // the same correction with that count: by_band and by_coin rows
    "dominant_window_share": 0.5     // a leaderboard row whose top_window_share reaches this is flagged
  },
  "scorecard": {
    "what": "Brier score of the model's probability against the market's own mid price, on settled rounds. Lower is better. The unit is one 15-minute window (all coins together), because bets inside a window are not independent.",
    "overall": { "n_rows": 20242, "n_windows": 11, "brier_model": 0.1635, "brier_market": 0.1561, "diff": 0.0074, "se": 0.0044, "t": 1.69, "verdict": "unresolved" },
    "by_band": [ { "band": "under 60 s", "n_rows": 0, "n_windows": 0, "brier_model": 0, "brier_market": 0, "diff": 0, "se": 0, "t": 0, "verdict": "unresolved" } ],
    "by_coin": [ { "coin": "BTC", "n_rows": 0, "n_windows": 0, "brier_model": 0, "brier_market": 0, "diff": 0, "se": 0, "t": 0, "verdict": "unresolved" } ]
  },
  "fills": {
    "what": "The simulator sells any size at the bid. This re-prices every early sale as if only the size actually displayed at that second had filled, the rest riding to settlement.",
    "by_strategy": [ { "strategy": "Scalper", "engine": "v2", "world": "real", "sells": 39, "sells_priced": 27, "contracts_sold": 774, "contracts_beyond_bid": 558,
                       "share_beyond": 0.72, "sale_pnl_cents": 35800, "sale_pnl_capped_cents": 13400, "unsettled_sells": 0,
                       "sells_without_depth": 12 } ]   // sales made before order-book depth was recorded (release 87a2100): counted, never guessed at
  },
  "leaderboard": {
    "what": "Per strategy version. Windows are the independent sample, not bets. Lifetime includes every earlier life of a strategy that ran out.",
    "rows": [ { "strategy": "Scalper", "engine": "v2", "world": "real", "lives": 1, "book_cents": 127000, "lifetime_pnl_cents": 27693,
                "bets": 36, "windows": 5, "mean_window_pnl_cents": 5538, "se_cents": 5100, "t": 1.09,
                "top_window_share": 0.97, "verdict": "unresolved", "flags": ["one window is 97% of the result"] } ]
  }
}
```

Bands, in this order: `over 10 min`, `5 to 10 min`, `2 to 5 min`, `1 to 2 min`, `under 60 s`; each
includes its lower end (600 s exactly is `over 10 min`). `diff` is model minus market: positive
means the model is WORSE. `verdict` for the scorecard is `model better`, `model worse` or
`unresolved`; for the leaderboard `ahead`, `behind` or `unresolved`. The three `what` strings are
longer than the examples above: they say which column is the model, what a row is, and how each
figure is made. Show them whole.

**Coverage, and what the page must do with it.** The per-market figures live in memory and are
read back after every restart, 100 settled markets a minute, newest first. Until
`coverage.complete` is true the document is PART of the history:

- every `verdict`, scorecard and leaderboard, is `unresolved`;
- every leaderboard row's `flags` starts with `partial: only N of M settled markets read ...`;
- all three `what` strings start with `Partial: only N of M settled markets read ...`;
- the figures are still given, and cover only the markets read. `windows_recorded`,
  `lifetime_pnl_cents`, `bets`, `windows`, `sells` and the rest count those markets only, while
  `lives` and `book_cents` are always whole-history figures.

The page must show a banner whenever `coverage.complete` is false or `stale` is true.

**A settled market is only used once its money is all there.** A round's result is stored before
its settlement rows, in separate transactions. So for each market, per bucket and side, the
contracts still held at the close (bought less sold) must equal the settlement rows' `qty`; a
losing hold has a row too, paying 0. A market that does not reconcile is not kept, its whole
window is left out (`windows_incomplete`), it is counted in `markets_unreconciled`, and it is read
again every minute. Normally that lasts one refresh. A figure that STAYS above 0 is a round whose
settlement was never written (a halted runner, or a stop between the two writes): it keeps
`complete` false, and so keeps every verdict `unresolved`, until somebody deals with it. A fill
whose recorded detail has no cost (or a sale's no payout) holds its market back the same way.

**Verdicts are corrected for the number of rows looked at together.** `scorecard.overall` is one
hypothesis stated in advance and needs |t| >= `min_abs_t`. The leaderboard compares `trials`
strategy versions at once, so its rows need |t| >= `leaderboard_min_abs_t`; the `by_band` and
`by_coin` rows are ten exploratory cuts and need |t| >= `scorecard_cut_min_abs_t`. Both are
two-sided Bonferroni cuts at `family_alpha`. This is a convention, like the rest of the rule: it
holds the chance of ANY false verdict among the rows shown side by side to 5%, it assumes nothing
about how the rows depend on each other, and it is conservative when they move together, as a
twin and its original do. If `trials` is 0 the count could not be read: no leaderboard verdict is
given and each row's `flags` says so. The verdict, and the dominant-window flag, are decided on
`t` and `top_window_share` exactly as published (two places), so a number and its label never
disagree.

**`book_cents` is not equity.** It is the ledger cash of the version's live buckets plus their
open bets AT COST, 0 if no bucket is live. For the marked value use api/buckets `equity_cents`,
which marks the same bets at the bid; the two differ by the unrealised P&L. A first-engine version
spans its BTC and ETH buckets, which api/buckets lists apart. For up to a minute after a round
settles `book_cents` can read low: the bets have left "open" and the payout may not have been in
the ledger when it was read. `lifetime_pnl_cents` is REALISED trading P&L on the settled windows
read (payouts and sale proceeds less what the bets cost, fees inside), over every life, before
any sustainment allocation. It is not book or equity less the seed.

**Fills.** `sells` is every early sale in the windows read plus `unsettled_sells`. The contract
and P&L figures cover only the `sells_priced` ones: settled rounds where depth was recorded. The
displayed size is the best bid level alone. The simulator books a sale one cent under the bid and
the size bid within that cent is not counted, so `contracts_beyond_bid` is an upper bound.

**Errors.** If a refresh fails, the document before it is served with `"stale": true` and
`stale_reason`. If nothing has been computed yet the answer is **503, still JSON and still this
shape**: every list empty, `stale` true, `stale_reason` saying why, `computed_at` 0 and
`coverage.complete` false.
