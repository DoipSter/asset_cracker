# v3: a depth-aware paper broker and an engine of our own. Final design and delivery plan

2026-09-21. Repo `asset_cracker`, branch `platform-brief`. The only change to the repo is this document
(`docs/honest-fills-v3.md`, not committed). Nothing was run against the Pi: every SQL sketch below is unexecuted.
Simulated money only. No live broker code is proposed.

Labels used throughout: **[READ]** I read it in the code or the samples today. **[INFERRED]** concluded from what
I read. **[ASSUMED]** could not be checked. **[CONVENTION]** a chosen number, not a measurement.
**[MEASURED]** comes from the recorded history by the written procedure in section 6.

This document starts from the broker-first draft (both judges' winner), fixes every flaw they found in it, and
takes the listed ideas from the other two. A completeness critic then found fourteen problems in that result; each
fix is worked into the section it concerns. Independent checkers then read those fixes against the code and found
three further problems, which the fixes themselves had opened (13.2). Section 13 lists what changed and why, in all
three rounds.

---

## 0. The short version

1. `service/internal/broker`: one interface (`Submit`, `Commit`, `Void`) and one implementation, `Paper`. It fills an
   immediate-or-cancel order by walking the five recorded bid levels of that second's book, never beyond, in whole
   contracts, and remembers per bucket what it took at each price. When the display at a price FALLS below what
   was taken there, the difference is not forgotten: it moves down to the next displayed level, because the real
   taker who caused the fall would, in a book we had already eaten, have eaten that level instead.
2. `service/internal/kalshi15m3`: integer cents; `Decide` moves no money; `Apply` runs only after the ledger
   transaction committed. v3 has no state in which memory is ahead of the ledger, so it never needs v2's permanent halt.
3. After a restart everything about money is rebuilt from the database (ledger, `trade_order`, `fill`,
   `settlement`), including the paper holds and the per-window budget. Which buckets v3 holds is decided ONCE, at
   start, and never changes while the service runs. Whether a bucket has run out is derived AFTER that rebuild has
   established what it still holds, never before: a bucket with little cash and a bet open is not closed.
4. "May place orders" and "is held" are separate. Every v3 bucket that is not frozen is held, valued and SETTLED
   whatever `AC_V3` or the version's status says; the switch and the status gate new orders only. `NewRunner3`
   therefore ALWAYS obtains the `SimSetup` (the service actor and the venue, fees and pool ledger ids), also when
   nothing may order: a settlement cannot be written without it.
5. v3 has two non-trading states. **Paused** (the database REPORTED an error, so the write certainly rolled back;
   memory is still exact; the book stays valid; value snapshots are NOT blocked). **Suspended** (the outcome of a
   write is unknown, a timeout included, so memory may be behind the ledger; heals by rebuilding from the database).
   While suspended v3 writes NOTHING, settlements included: a settlement row written from a memory that is short
   can never be corrected. A suspended v3 blocks every engine's value snapshots until it heals (5.3 bounds this).
6. A once-a-minute sweep settles v3 positions from `market.result`, because the poller tells engines about a
   result exactly once **[READ main.go:519-522]**. It runs on v3's own goroutine, also when `AC_V3` is off, and
   never while v3 is suspended: a heal is "rebuild, and only if that succeeded, sweep".
7. Nothing called from a poller or from the Coinbase stream ever waits on a lock that v3 holds across a database
   call: the model state has its own small mutex, `Step` and `Settled` give up rather than wait, and a rebuild
   reads outside the lock.
8. Sizing: a window's open cost plus the losses it has already realised, across all coins, never exceeds
   quarter-Kelly of its single best bet; and a buy that walks the book is sized level by level, each level at the
   Kelly fraction of ITS price.
9. The probability weight lambda and the two staleness costs (buying and selling) are MEASURED under a protocol
   that is committed to the repo BEFORE anything is run; the confirming data is only windows that close after that
   commit. It accumulates while the code is being built, so the protocol costs no calendar time. "Register
   nothing" is an accepted outcome.
10. Two versions (Scalper v3, Value v3), no twins: trials 18 -> 20, corrected |t| 2.991 -> 3.023. They are inserted
   as `draft`; a person sets `probation`; Runner3 places orders only for `probation` or `active`, as read at start.
11. Two migrations, because `db/migrate.sh` applies a file once and skips it for ever **[READ migrate.sh:32-40]**:
   **0012** (the `client_order_id` column, comments, the coin flag) and **0013** (the version rows, generated, only
   if the protocol passes).
12. The analysis readers are made fill-aware and released FIRST. The replayer is the next step, not this one.

---

## 1. What I verified in the code today

| Fact | Where | Consequence |
|---|---|---|
| Production runs release `78f5452`, with migrations 0010 and 0011 applied, released 2026-09-21 08:20 PT: the home page IS live. HEAD is `2f97595`, one small commit ahead (the "value snapshots have stopped" notice) and not yet released. The "NOT YET RUN ANYWHERE" comments inside 0010 and 0011 are older than the release. | **[REPORTED]** by the owner's session, and written in the brief's working copy at lines 291-292 (an uncommitted edit of today, not made by this plan; the committed brief still says "NOT yet released"). **[READ]** `git log`; the comments at 0010:63, 0011:15. I did not query prod myself. | Step S0 still reads `schema_migration` and the `/api/status` version on prod and dev (a reading, not this table, is what the steps rest on) and releases `2f97595` first if the owner wants it out before S1. |
| `migrate.sh` records a version once and skips it afterwards. | migrate.sh:32-40 | A "0012 part 2" can never apply. Two files. |
| `SaveResult` calls the engines' `Settled` only when `RecordResult` returns `first`. | main.go:519-530 | An engine that misses that call is never told again. v3 needs the sweep. |
| An error from `SaveQuotes` reaches the poller; nothing on that path, nor on the Coinbase `Observe` path, recovers a panic. | main.go:488-512, :145-157; only `recover()` is web/analysis.go:111 | Every v3 call from those paths is inside `safely()` and returns nothing to the caller. |
| The `model` map is built BEFORE `InsertEvaluation`, so before v1 and v2 step. All Coinbase prints arrive on ONE goroutine whose callback calls each engine's `Observe` in turn and then feeds the tick writer. `Runner2.Inputs` and `Runner2.Observe` take v2's engine lock. | main.go:488-497, :145-157; runner2.go:226-252 | Two v3 calls (`Inputs`, `Observe`) cannot "run last". They must never wait on a lock v3 holds across a database call, or a slow database stalls v1, v2 and the tick writer behind v3 (5.1). `RefreshCapital` already shows the pattern: read outside the lock, keep the result only if a generation counter has not moved (books.go:255-286). |
| A position sold out before the close gets NO settlement row: "a position sold early nets to no contracts and needs none". Readers decide "still held" by the NET of bought less sold per (bucket, side). | store/home.go:369-375, :386-395 | v3's rebuild and its guards must use the same net rule, or every early exit reads as an unsettled bet (5.4). |
| `fill.transfer_id` is `not null`; `ledger_entry.amount_cents <> 0`. | 0001:373, :141 | A fill with no cash effect at all cannot be recorded; 2.3's dust rule ends the walk there. |
| `PriceSales` starts a fresh displayed size for every (bucket, second, side), has no limit-price test, and takes the book from the latest evaluation within 5 s before the sale. | analysis.go:260-278; store/analysis.go:182-185 | `Paper` with holds across seconds cannot reproduce its counts. `brokercheck` has a parity mode built to match it and a real mode that is not required to (section 8). |
| A verdict needs `MinWindows = 30` windows and `|t| >= CorrectedT(trials)`, the two-sided Bonferroni cut. | analysis.go:22, :28-48, :327-336 | Every new verdict line in this plan uses that rule, with its own comparisons counted (7.5). |
| `SnapshotRefusal` refuses the whole minute if any book has `Halted != ""`, or any open position's round has closed. Past `kalshi.GiveUpAfter` it refuses for good. | books.go:349-372 | v3 must not set `Halted` when its memory is exact, and must never leave a position open on a settled round. |
| Runner2 caches the ledger capital and re-reads only when IT moves money (`refreshMoney`, `capitalGen`, `capitalFresh`, `heldElsewhere`). | runner2.go:44-52, 118-150 | v3's bucket ids must be in `heldElsewhere` before NewRunner2 reads and must not change while running: no bucket is created, added or dropped after `NewRunner3` returns (5.4). |
| `EnsureSimSetup` creates and seeds a bucket for any registered name; it does not look at `strategy_version.status`. It is safe to repeat: an existing bucket is left alone, frozen or not. | sim.go:41-44, loop 112-154 | Runner3 must filter by status itself BEFORE calling it, and may call it only in `NewRunner3`. |
| `EnsureSimSetup` with an EMPTY name list creates no bucket and moves no money: the bucket loop is `for _, name := range strategies`, and every deposit, seed, `bucket` insert and `seeded` event is inside it. Outside the loop it reads the `service` actor, inserts the seven sim ledger accounts and the `kalshi paper` venue account, each `on conflict ... do nothing`, reads their ids and commits. It is the ONLY source of a `SimSetup`. **[READ, not run]** | sim.go:45-154 (account helper 55-64, venue account 89-99, loop 112, return 152-153) | Safe for v3 to call with no names, and on prod it creates nothing at all, because v2 has made those accounts. **[INFERRED]** On a database where v2 has never started it would create the accounts and the venue account, which `NewRunner2` creates a moment later anyway; and an insert that conflicts may still use up a sequence number, as v2's does at every start today. It FAILS if the `service` actor or the `kalshi` source row is missing, exactly as `NewRunner2` then fails. |
| `RecordSettlements` writes `setup.ActorID` into `ledger_transfer.created_by` and debits `setup.VenueLedgerID` for every row with a payout; `CloseBucket` writes `setup.ActorID` and credits `setup.PoolLedgerID`. `CloseBucket` makes NO test for open positions: it sums the cash, reaps it and freezes the bucket. | sim.go:367-399, 163-199; `created_by` is `not null references actor` (0001) | A zero `SimSetup` fails the foreign key on every WINNING settlement (a loser writes no transfer and would pass), so v3 needs the setup in every mode in which it holds a bucket (5.4). And the only guard against freezing a bucket that still holds a bet is the runner's own (5.6). |
| `Runner2.Settled` returns at once when v2 is halted; `RecordResult` has stored `market.result` before any engine's `Settled` is called. | runner2.go:344-348; main.go:519-522 | v3 does the same while suspended (5.3). The result is not lost by that: the sweep reads it from the table after the heal, however late. |
| `AnalysisWindow` reads `o.qty` with `o.status = 'filled'`; `Reconcile` holds back a market whose held != settled, and the window with it, for every engine. | store/analysis.go:177-188 | One partial v3 order would silently remove windows from v2's leaderboard. Readers first. |
| Trials are `select count(*) from strategy_version`: a `draft` row counts. | store/analysis.go:61 | Version rows are written only once the protocol has passed. |
| `strategy_version.status` allows `draft`; the service role may update `status`. `trade_order` is not partitioned, already allows `partial`/`cancelled`/`rejected`, has `venue_order_id` and `detail`. Actor `claude` exists. A read-only role `assetcracker_ro` exists on prod. | 0001:90-91, 359-360; grants.sql:17; 0004:8; grants.sql:31 | No table change beyond one column. `measure3` and `brokercheck` run as `assetcracker_ro` (needs `select` granted on the tables they read: owner's call, D13). |
| The home page gives a group with no row in the `then` batch `earned_cents: null`. | web/home.go:327-333 | A fifth group needs a zero-baseline rule, or it shows "unknown" for every range that starts before it existed. |
| v2's `CoinState` methods (`observe`, `knownAvg`, `seedVol`) are unexported; `ProbYes` and `KalshiFee` are exported. The offset memory is 24 rounds, about six hours. | trader.go:35-48, model.go:142-150 | v3 forks `CoinState` (the package must not change) and calls `k2.ProbYes`. Six hours is the jackknife block length. |
| CI runs only the Python suite. | .github/workflows/tests.yml | The v2 parity gate is not enforced anywhere but by hand (D12). |

---

## 2. The broker: `service/internal/broker`

Imports `service/internal/kalshi` (for `Quotes`) and nothing else of ours. No store, no clock, no goroutines.

| File | Holds |
|---|---|
| `broker.go` | the interface and types; the package comment says nothing here can place a real order |
| `money.go` | `Price`, `PremiumCents`, fee arithmetic, `BucketCents` |
| `ladder.go` | `kalshi.Quotes` -> the ladder an order walks |
| `paper.go` | `Paper`: the fill rule and the holds |

### 2.1 Types

```go
// Price is ten-thousandths of a dollar, the unit kalshi/quotes.go uses, so a YES ask is exactly 10000 - a NO bid.
type Price int64
type Action string // "buy" | "sell"
type Side string   // "yes" | "no": the contract bought or sold

// Order is all an engine may say to a broker. Every order is immediate-or-cancel: nothing rests,
// because a resting order needs queue position and a one-second snapshot cannot give it.
type Order struct {
	ClientID     string // "v3:<bucket id>:<evaluation id>:<n>", unique for ever: the idempotency key
	BucketID     int64
	MarketID     int64
	Ticker       string
	Action       Action
	Side         Side
	Qty          int   // whole contracts, >= 1
	Limit        Price // buy: most to pay per contract. sell: least to accept
	MaxCostCents int64 // buys only, 0 = none: premium plus fee never passes this
	CostSteps    []CostStep // buys only, may be empty: a ceiling that tightens as the walk reaches worse prices (4.5)
	EvaluationID int64 // the snapshot the decision was made on
	At           time.Time
}
// CostStep: once a fill at taker price UpTo or worse is included, the order's premium plus fee SO FAR may not pass
// MaxCostCents. Steps are sorted by rising UpTo with falling MaxCostCents. One venue order cannot say this; a live
// broker would send one immediate-or-cancel order per step, each with the remaining ceiling (section 14).
type CostStep struct { UpTo Price; MaxCostCents int64 }
type Fill struct {
	Seq, Level   int   // 1.. within the order; recorded level, 0 = best
	Qty          int
	Price        Price // the taker's price per contract
	PremiumCents int64 // rounded against the bucket
	FeeCents     int64
}
type Status string // "filled" | "partial" | "cancelled" (nothing filled) | "rejected" (never reached a book)
type LevelSeen struct{ Bid Price; Displayed, Held, Taken int }
type Report struct {
	Order    Order
	Status   Status
	Fills    []Fill
	Unfilled int
	Reason   string
	Model    string      // "paper-1"; stored with the order, part of what a result means
	Seen     []LevelSeen // the evidence
	Final    bool        // always true for Paper; a live broker could answer false and finish later
}

type Broker interface {
	Name() string // goes in trade_order.broker: "paper"
	// Submit tries the order now. A market reason is a Report, never an error.
	Submit(ctx context.Context, o Order) (Report, error)
	// Commit: these orders' fills are in the ledger. Paper makes its holds permanent.
	Commit(clientIDs ...string)
	// Void: the fills were NOT recorded; forget them. A live broker cannot un-fill: it must return
	// ErrCannotVoid, and the runner then suspends and reconciles from the venue. A real fill that
	// cannot be recorded is a halt, not an undo. Written down here so the failure path is coded once.
	Void(clientIDs ...string) error
}
// BookObserver is the market-data side, kept apart because a real venue has its own book.
// The runner calls it once per snapshot, BEFORE the engine decides.
type BookObserver interface {
	ObserveBook(ticker string, evaluationID int64, at, closes time.Time, q kalshi.Quotes)
}
```

### 2.2 Money (`money.go`): exact integers, no epsilon

```go
// PremiumCents: a buy rounds UP, a sell rounds DOWN. Sub-cent levels are real (0.0990 in the samples) and
// Kalshi's rounding of them is unknown, so the bucket never gains from rounding.
func PremiumCents(a Action, qty int, p Price) int64 { n := int64(qty) * int64(p); if a == "buy" { return (n + 99) / 100 }; return n / 100 }
// feeNumerator: fee in cents = 7 * qty * P * (10000-P) / 1e8.
func feeNumerator(qty int, p Price) int64 { return 7 * int64(qty) * int64(p) * (10000 - int64(p)) }
// BucketCents is the bucket's cash effect of one fill: the store writes it and the engine applies it, so they cannot disagree.
func BucketCents(a Action, f Fill) int64 // buy: -(premium+fee); sell: +(premium-fee)
```

Fee: rounded up ONCE per order (`ceil(sum of numerators / 1e8)`), shared across fills cumulatively:
`fee_i = ceil(cum_i/1e8) - ceil(cum_{i-1}/1e8)`, so the shares always add up to the order's fee. `Params.FeePerFill`
(default false, frozen in the version) switches to rounding each fill up, the pessimistic reading. **[ASSUMED]**
per-order is what Kalshi does on a multi-level sweep; if wrong, truth is at most (levels - 1) cents dearer per order (D9).

### 2.3 The fill rule, exactly (`paper.go`)

`Paper{Levels int}`: how many recorded levels an order may walk, 5 in the service (all that is recorded); only
`brokercheck`'s parity mode sets 1 (section 8).

State, under one mutex: `holds map[holdKey]int`, `holdKey{BucketID, Ticker, Ladder ("yes_bids"|"no_bids"), Bid Price}`;
`pending map[clientID][]holdDelta`; `reports map[clientID]Report`; per ticker the last book
`{evaluationID, at, closes, ladders}`. The key is the RESTING order's side and price, so buying YES at 0.12 and
selling NO at 0.88 draw on the same contracts, which is true of the real book. Holds are per bucket (D6).
`Forget(ticker)` drops a settled market's state. The mutex is never held across anything but arithmetic.

**ObserveBook** (the replenishment rule). For each (bucket, ladder `S`) of this ticker, take its committed holds
BEST PRICE FIRST, carrying a number `displaced` that starts at 0. For the hold `h` at bid `P`:

1. `shown` = the recorded levels of `S` (at most five), `worst` = the lowest bid among them.
2. `P` listed: `displayed = floor(size)`.
3. `P` not listed and (`len(shown) < 5` or `P > worst`): the ladder is visible down past `P` and `P` is not in it
   (`ladder()` skips empty levels and sorts best first **[READ]**), so `displayed = 0`.
4. `P` not listed, five levels shown, `P < worst`: cannot be seen. Leave the hold alone.
5. Otherwise `new = min(h, displayed)`; `displaced += h - new`; the hold becomes `new` (deleted at 0).

Then, walking the SHOWN levels best first, every level worse than the best price that released something absorbs
what it can: `room = floor(size) - hold`, `move = min(displaced, room)`, `hold += move`, `displaced -= move`. What
is still displaced after the last shown level is dropped: it would sit on levels that cannot be seen (2.5, item 4).
A displaced hold is an ordinary hold from then on: it falls, and is displaced again, by the same rule.

Why: our paper order removed nothing from the real book, so the real book keeps showing the contracts we "took".
Under price-time priority the resting orders we took were at the front of the queue. While the display stays at or
above the hold, those same orders may still be what is shown, so only the excess is new:
`available = displayed - hold`. When the display FALLS below the hold, the difference `d` has really gone. It was
either cancelled or taken by a real taker, and the recording cannot tell which. If it was taken, then in the book
we had already eaten that taker would have found the level gone and eaten `d` from the NEXT level instead. So `d`
is moved down, not forgotten: forgetting it would hand v3 the displaced taker's liquidity for nothing, at exactly
the moments the book is being hit, which is when Scalper sells. Treating every fall as a take is the pessimistic
reading of the two **[CONVENTION: the conservative one; a cancel is charged as if it were a take; D16]**.
For one bucket and ladder the TOTAL held never rises on an observation; a level's hold rises only by what a better
level released. This is the counterfactual book under the single assumption that nobody else reacts to us.

**Submit(o)**

1. `rejected` if: no book for the ticker; the last book's `evaluationID != o.EvaluationID` ("stale book": the engine
   decided on a second the broker does not hold); `o.At >= closes`; `Qty < 1`; `Limit` outside 1..9999. A `ClientID`
   seen before returns its first report again.
2. Ladder and taker price. buy yes -> `no_bids`, taker = 10000 - bid. buy no -> `yes_bids`, taker = 10000 - bid.
   sell yes -> `yes_bids`, taker = bid. sell no -> `no_bids`, taker = bid. The 1c/3c/5c cumulative depth figures are
   never filled from: they have no prices.
3. Walk best first, at most `Levels` levels. At level `i`:
   - price test: buy `taker <= Limit`, sell `taker >= Limit`. The first failing level ends the walk.
   - `avail = max(0, floor(size_i) - hold_i - pendingHold_i)`; `take = min(remaining, avail)`.
   - buys: the ceiling at this level is the smallest of `MaxCostCents` (if set) and the `MaxCostCents` of every
     `CostStep` with `UpTo <= taker`. `take` = the largest n <= take with premium-so-far + order-fee-so-far (this
     fill included) <= that ceiling, computed directly, not by a decrementing loop. If the ceiling allows none, the
     walk ENDS: ceilings only tighten as the price worsens.
   - a level is never skipped. A real sweep cannot reach a worse bid without first taking the better one, so a
     level whose premium is smaller than the fee it adds (one contract at 0.0091: 0 cents of premium, up to a cent
     of fee) is TAKEN and booked as it is, even when that costs the bucket a cent. The one fill that cannot be
     booked is one whose premium and fee share are BOTH 0 cents: a `fill` row must point at a ledger transfer and an
     entry may not be zero **[READ 0001:373, :141]**. There the walk ENDS, the rest is unfilled, reason "the next
     level is worth less than a cent". No fill is ever made beyond a level that was left untouched. Two levels are not "untouched" and the walk
     passes over them: one this bucket already holds in full (it was taken earlier), and one that shows LESS THAN ONE
     whole contract (23.62 displayed fills 23; 0.62 displayed fills none, and a real sweep would not stop there)
     **[CONVENTION; how Kalshi matches a whole-contract order against a resting fraction is ASSUMED, not measured.
     Booking the next, worse price errs against the bucket]**. A CROSSED snapshot (best yes bid plus best no bid
     above 1.0000) is refused whole, reason "crossed book": it is a recording fault, not liquidity **[CONVENTION]**.
   - `take > 0`: append the fill and a pending hold.
4. `filled` if nothing is left, `partial` if something filled, else `cancelled`, with `Unfilled` and `Reason`.
5. Nothing is permanent until `Commit`. `Void` deletes the pending holds and the stored report.

### 2.4 Worked example, a real recorded book

`db_samples.txt`, market 102, `KXDOGE15M-26SEP210345-45`, 2026-09-21 07:41:28Z (the book stored with v2 sells 549 and 550):

```
yes_bids [0.1000 x 43] [0.0990 x 100] [0.0930 x 1] [0.0920 x 23] [0.0910 x 1]
no_bids  [0.8800 x 161] [0.8700 x 23.62] [0.8600 x 28.28] [0.8500 x 210.84] [0.8400 x 54.92]
```

**A. What really happened, replayed.** v2's Anti Scalper sold 5 + 4 YES at 0.09 (bid less a guessed cent) and booked
42c + 33c = 75c. As one paper order, sell 9 yes limit 0.0900: level 0 shows 43, take 9 at 0.1000. Premium
floor(9 x 1000 / 100) = 90c; fee ceil(5.67) = 6c; the bucket receives 84c. Ledger: bucket +84, venue -90, fees +6.
Hold `{yes_bids, 0.1000}` = 9.

**B. A large sale** (the size of the review's ETH incident): sell 348 yes limit 0.0900.

| Level | bid | shown | take | premium | cumulative raw fee | fee share | to bucket |
|---|---|---|---|---|---|---|---|
| 0 | 0.1000 | 43 | 43 | 430c | 27.0900c | 28c | 402c |
| 1 | 0.0990 | 100 | 100 | 990c | 89.5293c | 62c | 928c |
| 2 | 0.0930 | 1 | 1 | 9c | 90.1198c | 1c | 8c |
| 3 | 0.0920 | 23 | 23 | 211c | 103.5691c | 13c | 198c |
| 4 | 0.0910 | 1 | 1 | 9c | 104.1481c | 1c | 8c |

168 filled, fee 105c, 1544c to the bucket in five transfers; `partial`, 180 unfilled, reason "nothing more displayed
within the limit". v2 would have booked all 348 at 0.09.

**C. Next second, same book.** Every hold equals the display: `cancelled`, "already taken by this bucket". If level 0
then shows 60, 17 can fill. If it shows 10, the hold there drops to 10 and 33 are displaced down the ladder; every
lower level is already held in full, so they fall off the visible book, and 0 can fill. If level 0 later shows 43
again, 33 can.

**C2. A fall after a small sale** (the critic's case). After A alone the hold is 9 at 0.1000. Suppose instead the
bucket had sold 43: hold 43 at 0.1000, nothing at 0.0990. Next second a real taker sells 33 and the book shows
`[0.1000 x 10] [0.0990 x 100]`. The hold at 0.1000 drops to 10 and the 33 move to 0.0990: available there is
100 - 33 = 67, not 100. The bucket can sell 43 + 67 = 110 in all, which is what existed (143 less the taker's 33).
Forgetting the 33 would have let it sell 143.

**D. A buy that walks.** Buy 200 yes, limit 0.1300, max cost 2500c. From `no_bids`: 0.1200 x 161, 0.1300 x 23
(floor of 23.62), 0.1400 fails the limit. Premiums 1932c and 299c; fee shares 120c and 18c; the bucket pays 2369c;
16 unfilled; `partial`.

**E. An exit that cannot happen.** Evaluation 71830 (market 106, ETH, 07:44:19Z) has `yes_bids: []`. Sell yes:
`cancelled`, "no bids on this side". No money moves; a `trade_order` row with status `cancelled` and no fill IS
written, because a failed exit is evidence.

**F. A level worth less than its fee** (constructed). Sell 101 yes, limit 0.0001, into `[0.0100 x 1] [0.0099 x 100]`.
Level 0 is taken, as a venue would take it: premium 1c, fee share ceil(0.0693) = 1c, the bucket gets 0c; the
transfer is venue -1, fees +1 and the bucket entry is left out (an entry may not be zero). Level 1: 100 at 0.0099,
premium 99c, cumulative fee ceil(6.93) = 7c so its share is 6c, the bucket gets 93c. `filled`, 93c in two
transfers. Skipping level 0 would have booked a sequence no venue produces, and the favourable one. A single
contract at 0.0091 sold alone is premium 0c, fee 1c: the bucket PAYS a cent, and that is what is booked.

(All figures above were recomputed today with integer arithmetic.)

### 2.5 What the paper broker cannot know

1. Whether the displayed size was still there when the order arrived. The snapshot is up to a second old. `paper-1`
   fills at the displayed price with no guessed slippage; the entry test and both exit tests charge a MEASURED
   staleness cost (6.3, M2 and M2s), which is a proxy taken from v2's orders, and the analysis `execution` section
   re-prices every v3 fill on the next snapshot (7.5). A stricter `paper-2` (fill only what two consecutive
   snapshots both show) is a change of fill model and a new version.
2. Market impact and others' reactions. The hold rule is a floor on this, not a model of it.
3. Anything between snapshots: a level that emptied and refilled within a second stays held (errs against us).
   Whether a fall in the display was a cancel or a take: every fall is charged as a take (2.3), which errs against us.
4. Depth beyond five levels. Hidden or iceberg size, if Kalshi has any **[ASSUMED none]**. A hold displaced past
   the last shown level is dropped, so against a book deeper than five levels the rule is slightly generous there.
5. Kalshi's own rounding of sub-cent fills and multi-level fees.
6. Venue rules: size and position limits, rate limits, self-trade prevention, rejects, outages.
7. Other buckets: each is its own paper world, as the analysis layer already assumes. On one real account they
   would compete for the same contracts and opposite sides would net.
8. Adverse selection at real latency. Only the brief's gate closes these: sim and live fills compared on tiny size.

---

## 3. Recording: `service/internal/store/sim3.go` (new; `sim.go` and `RecordStep` untouched)

```go
type FillRow struct { Qty int; Price string /* "0.0990" */; PremiumCents, FeeCents int64 }
type OrderRow struct {
	DecisionIndex  int // into StepRecord3.Decisions, or -1
	BucketID, BucketLedgerID int64
	ClientID, Action, Side   string
	Qty            int    // REQUESTED
	Limit          string // the true limit, four decimals
	Status         string // filled | partial | cancelled | rejected
	Detail         any
	Fills          []FillRow
}
type StepRecord3 struct { EvaluationID int64; At time.Time; MarketID int64; Decisions []DecisionRow; Orders []OrderRow }

// RecordOrders journals a step and books its fills, all or nothing.
func (s *Store) RecordOrders(ctx context.Context, setup SimSetup, r StepRecord3) error
// OrdersRecorded says which client ids have a trade_order row. The rebuild uses it to REPORT whether the step that
// suspended it had committed; Step never decides anything by it (5.2).
func (s *Store) OrdersRecorded(ctx context.Context, clientIDs []string) (map[string]bool, error)
// SettlementsRecorded says which (bucket, side) of a market already have a settlement row: the same question for a payout.
func (s *Store) SettlementsRecorded(ctx context.Context, marketID int64, bucketIDs []int64) (map[[2]string]bool, error)
// TradableVersions is the v3 versions a person has approved: family, version, status in ('probation','active').
// The runner calls it ONCE, in NewRunner3; /api/status calls it again only to show "approved, waiting for restart".
func (s *Store) TradableVersions(ctx context.Context, family string, version int) ([]VersionRow, error) // name, id, params
// HeldBuckets is every sim bucket of that family and version that is not frozen, whatever its version's status:
// id, name, ledger account, version id and params. It creates nothing. What v3 HOLDS comes from here.
func (s *Store) HeldBuckets(ctx context.Context, family string, version int) ([]SimBucket, error)
// BucketFills is the rebuild read of 5.4; BucketCash the ledger sum per bucket; MarketResults the sweep's read.
// OpenQty is the ledger's net-open contracts per (bucket, side) of one market: the settlement path's last look (5.3).
func (s *Store) OpenQty(ctx context.Context, marketID int64, bucketIDs []int64) (map[[2]string]int, error)
```

`RecordSettlements` and `CloseBucket` (sim.go, used unchanged) and `RecordOrders` all take a `SimSetup`. v3 has
exactly one, obtained by `NewRunner3` from `EnsureSimSetup` in EVERY mode (5.4) and kept for the life of the
runner. A held bucket that is not in `setup.Buckets` (its version is retired, or `AC_V3` is off, so its name was
not passed) carries its own id and ledger account from `HeldBuckets`; nothing v3 writes looks a bucket up in
`setup.Buckets`.

A Go error is sorted into two kinds by one function, `DefinitelyRolledBack(err)` (exported: the runner is another
package). It is true in exactly three cases, each certain: the chain holds a `*pgconn.PgError` of severity ERROR (the
SERVER answered with an error, so the transaction did not commit; FATAL and PANIC are left as unknown **[INFERRED]**);
`ErrInvalidStep` (the step was refused in Go before any transaction was opened); `ErrRefused` (refused inside the
transaction on a path that returns before COMMIT is ever sent). A context deadline, a cancelled context, any
connection error, and any such error answering COMMIT itself is "outcome unknown": the client gave up, and the server
may still commit a moment later. An order the broker REJECTED or cancelled with no fill is not an error at all: it
is journaled as a `trade_order` row with no fill and no transfer, the reason in `detail` (a limit that is not a
storable price goes in as NULL with the text under `detail.limit_not_stored`).

One transaction, in order: `decision` rows (same insert as `RecordStep`; `size_alone` = requested contracts); per
order one `trade_order` (`broker 'paper'`, `qty` = requested, `limit_price` = the limit, the true `status`,
`client_order_id`, `detail`); **per fill exactly one `ledger_transfer`** (reason `fill`, memo like
`buy 161 yes @ 0.1200 (v3:31:71234:1 fill 1/2)`), its entries, and one `fill` row pointing at that transfer. One
transfer per fill is a hard requirement: `RealisedByCoin` joins fills to transfers.

Entries per fill, zero amounts skipped, same shape as sim.go:313-333 so every ledger reader sees v3 as it sees v2:

| | bucket | venue | fees |
|---|---|---|---|
| buy | -(premium + fee) | +premium | +fee |
| sell | +(premium - fee) | -premium | +fee |

A sale whose premium equals its fee share leaves the bucket entry out (venue and fees still balance); one whose
premium is 0c and fee share 1c is bucket -1, fees +1. Every reader that joins a fill to the BUCKET's entry must
therefore left-join it and read a missing entry as 0 (the rebuild read of 5.4 does; `RealisedByCoin` sums entries,
so a missing one already counts 0). A fill with neither premium nor fee is never made (2.3).

`trade_order.detail` for v3. The analysis layer books P&L from `detail.cost` and `detail.payout` in dollars and
turns them back with `math.Round(x*100)` **[READ analysis.go:201]**, exact for two-decimal images of cents:

```json
{"v": 3, "model": "paper-1", "coin": "DOGE", "ticker": "...", "close": 1789976700,
 "requested": 348, "filled": 168, "unfilled": 180, "reason": "...",
 "cost": 12.31,    // buy: what left the bucket, fee inside. sell: the cost basis of the contracts SOLD (4.2)
 "payout": 15.44,  // sells only: what reached the bucket, fee inside
 "why": "capture", // entry | value | capture
 "mid": 0.11, "p_model": 0.148, "p": 0.118, "lambda": 0.21, "stale_cost": 0.007, "stale_cost_sell": 0.006,
 "edge": 0.004, "kelly": 0.030,
 "binding": "kelly", // what set the size: kelly | window | cap | cash   ("book" is visible as unfilled > 0)
 "cost_steps": [[1200,739],[1300,440],[1400,135]], // buys: taker price e4, ceiling in cents (4.5)
 "window": {"close": 1789976700, "equity_cents": 100000, "k_max": 0.030, "open_cents": 0, "lost_cents": 0}, // as they stood BEFORE this order
 "seen": [[1000,43,0,43],[990,100,0,100]]}  // per level: bid e4, displayed, held, taken
```

`RecordSettlements` is used unchanged. `CloseBucket` is NOT called while the service runs (5.6).

---

## 4. The engine: `service/internal/kalshi15m3`

**As built (S4, 2026-09-21).** Where the code departs from this section, `service/internal/kalshi15m3/doc.go` lists
each departure and why; the code is the reference for the runner (section 5). The ones the runner must know:
`Apply(intents, reports)` (a report does not carry the Kelly fraction or the window's frozen cash); one call to
`AfterDecide(ticker, decisions)` after every `Decide`; `Account.MayOrder` set BEFORE `NewEngine` (an account that
was not validated can never order; a settle-only account is held whatever its stored params); `now` passed as
`UnixSeconds(evaluation time)` so an order's time equals its recorded `placed_at`; each order's detail built with
`engine.Detail` before `Apply` (it carries the broker's per-level `seen` evidence and `held_cents`). Quarter-Kelly
ceilings are CUMULATIVE per position, so a re-entry cannot spend a best-ask stake at worse prices. Entry and exit
tests charge the fee the broker will really book (rounded up per order), asks and bids are the first level showing
a whole contract, the buy limit never passes the version's band, and no sale is sent that would book nothing.

Ours, not a port: no parity requirement, its own tests. `int64` cents everywhere; the clock is passed in; no
goroutines, database or logging. It does not import `kalshi15m2` except `k2.ProbYes` (exported, called, not copied).

| File | Holds |
|---|---|
| `doc.go` | what v3 is; "Simulated money only. Nothing here can place an order." |
| `params.go` | `Params`, `Provenance`, the two strategies, `Validate` |
| `coin.go` | `Model` and `CoinState`: a fork of v2's volatility, price ring and offset learning (v2's methods are unexported and the package is frozen). Held to v2's numbers by test 23 and, live, by the drift gate (4.3). `Model` is a separate value from `Engine` so that the runner can guard it with its own small mutex (5.1): `Engine` never touches a `CoinState`. |
| `prob.go` | the blend |
| `position.go` | `Position.Apply`: the ONE function that folds a fill into a position, live and on rebuild |
| `sizing.go` | the window rule |
| `engine.go` | `Engine`, `Account`, `Decide`, `Apply`, `SettleRows`, `ApplySettlement` |

### 4.1 Types

```go
type Account struct {
	Params Params; BucketID int64; CashCents int64
	Positions map[posKey]*Position // posKey{Ticker, Side}
	Windows   map[int64]*Window    // by close, unix seconds; pruned once settled
	MayOrder  bool                 // AC_V3 on AND the version was probation/active at start. False = settle-only (5.4)
	Exhausted bool                 // ran out: places no more orders (5.6)
	Bets      int
}
type Position struct {
	Coin, Ticker, Side string; MarketID int64; Close, Strike float64
	Contracts int; CostCents int64 // premium plus fees of what is still held
	FirstAt, LastBuyAt float64; Entries int
	Exiting string // "" | "value" | "capture": an exit is wanted and has not finished
	ExitTries, BlockedSeconds int // consecutive empty exits; seconds an exit was wanted and nothing filled
}
type Window struct { Close, EquityCents int64; KMax float64; OpenCents, LostCents int64 } // Used = OpenCents + LostCents (4.5)
type Intent struct { Order broker.Order; Strategy string; Decision int; Why string; Detail map[string]any }

// Model: the per-coin state. The runner guards it with coinMu, never with the engine's lock.
func (m *Model) Observe(coin string, price, ts float64)
func (m *Model) NoteSettlement(coin string, closeAt float64, finalValue any)
// View is what one second's decision needs from the model, computed once and journaled every second (5.5):
// sigma2, offset, source, p_model, and Drift, the gate of 4.3 (ref is v2's published inputs, nil if v2 is absent).
func (m *Model) View(coin string, mk Market, price, now float64, ref *V2Inputs) View

func (e *Engine) Decide(coin string, m Market, book kalshi.Quotes, v View, now float64) ([]Decision, []Intent) // moves NO money; reads no CoinState
func (e *Engine) Apply(reports []broker.Report) []Event         // only after the ledger write committed
func (e *Engine) SettleRows(ticker, result string) []SettleRow  // what to record; changes nothing
func (e *Engine) ApplySettlement(ticker, result string) []Event // only after RecordSettlements committed
```

### 4.2 `Position.Apply`: partial fills, exactly

- Buy fill: `Contracts += qty`; `CostCents += premium + fee`.
- Sell fill of `n` of `N`: released basis `b = CostCents * n / N` rounded down, except the last contract takes what
  remains, so the parts always add up to the cost. `detail.cost` = sum of `b`; `detail.payout` = sum of
  `premium - fee`. Average cost, not lots **[CONVENTION]**: Kalshi keeps one net position per market and side.
- Cash moves by `broker.BucketCents`, the same function the store used.
- The position's window moves with it (4.5): a buy adds its cost to `OpenCents`; a sell takes the released basis
  `b` out of `OpenCents` and adds `max(0, b - proceeds)` to `LostCents`. Because live and rebuild both go through
  this one function, the window's account after a restart is the one it had before.

### 4.3 The probability

`p = mid + lambda * (p_model - mid)`. `mid` as v2 computes it. `p_model = k2.ProbYes(...)` from v3's `CoinState`.
`lambda` is one number for both versions (it is a property of the model), **[MEASURED]**, frozen with provenance.
`decision.model_prob` stays the RAW `p_model`, so the existing scorecard means the same for every engine.

**Note of 2026-09-23 (the owner's decision): a late window, for convention versions only.** The builder's shape
gained `lambda_late` and `lambda_late_tau` (`engine.Params.LambdaAt`): inside `lambda_late_tau` seconds of the close
(tau at or under it) the blend uses `lambda_late`, outside it `lambda` as above. Both are **[CONVENTION]** with
provenance, both or neither, `lambda_late` in 0..1 (0 is allowed: the belief is the mid there and nothing is
entered), and `Validate` refuses them on any version whose basis is measured: the protocol measures ONE lambda, and
that stays true. A version without them forms its belief exactly as before, and the order's stored `lambda` is the
weight the order was formed with. Why: on 162 windows the scorecard found the model's skill against the mid to
depend on the horizon (worse at 5 to 10 minutes, far better inside 2, where the book is one-sided most of the
time), which one number cannot express; the shape lets that be tried as a trial, judged live like every other.

**Note of 2026-09-23 (the owner's decision): a roster, one version.** The builder's shape gained
`members` (2 to 8), `lookback_windows` and `structural_only` (`engine.Composition`). A bucket still
holds one strategy version; the dance is that version. Each member is a normal shape (v1: all
`hold`, same family). At a snapshot a member is eligible only if `Decide` with its params would
send a buy; sit out if nobody is. Among the eligible the pick is roster order until every eligible
member has `lookback_windows` (default 16) settled *shadow* unit entries whose close is at or
before now, then the best shadow return per dollar. Shadows do not use the composition's cash.
`structural_only` is the ablation's baseline (roster order always). `Decision.Member` and
`Decision.Pick` name who fired. A settlement after now cannot elect a member (tested). Not a
bucket rewriting `strategy_version_id`, not the unbuilt bench, not online learning of K.

Entry test at the best ask `c`: `edge = p_side - c - fee(c) - stale_cost > 0`. `fee(c) = 0.07 c (1-c)` exactly.
`stale_cost` is **[MEASURED on v2's entries: a proxy for v3's]** (6.3, M2) and replaces v2's guessed 0.01
slippage. There is NO `min_edge` (v2's 0.03 was a guess at costs now charged exactly or measured). Buy limit: the
worst displayed taker price `L` at which `p_side - L - fee(L) - stale_cost > 0` still holds, so no fill can happen
at a price the strategy's own arithmetic calls unprofitable. How MUCH may be bought at each price up to `L` is
4.5's business: the edge shrinks to nothing at `L`, and so does the size.

**The drift gate.** lambda is measured on v2's model, and v3 computes `p_model` from a fork, so the fork must be
shown to be the same model every second it is relied on. The sink hands v3 the very inputs it journals for v2
(`run2.Inputs(coin)`: `sigma2`, `index_offset`, `offset_source` **[READ runner2.go:226-238]**; no change to
`runner2.go`). `View.Drift` is true when v2's inputs are absent, when `offset_source` differs between the two, or
when `|p_model - k2.ProbYes(same price, strike, tau and price ring, v2's sigma2 and offset)| > drift_tol`. While
`Drift` is true v3 sends no ENTRY for that coin (`blocked_by "model drift"`); exits go on, because keeping a
position is not the safer side of a doubt about the model. `drift_tol` is **[MEASURED on dev in S5]**: the 99th
percentile of that difference over the S5 run, taken after both engines hold 24 offset samples; it is frozen in
the params with that provenance. Until it has been measured the dev plumbing versions carry a labelled test value
and S5 REPORTS the difference instead of trusting the gate. The gate costs about one second in a hundred by
construction; the `execution` section counts the seconds it blocked, per coin (7.5).

### 4.4 Entries, exits, and the broker's answers

Per step (one coin's market), per account:

1. **A position here wants out** (`Exiting != ""` or a rule fires now): one sell intent for ALL contracts held on
   that side, and no entry in the same step. `min_hold` since `LastBuyAt`, and `tau >= min_tau`.
   - `value` (both versions' only reason to sell early is being overpaid; Value v3 has exit "hold" like its parent,
     so this applies to Scalper only): fires when `bid - fee(bid) - stale_cost_sell > p_side`. No `ExitMargin`.
     Limit = the lowest displayed bid where that still holds.
   - `capture` (Scalper): fires when the best bid has covered `take_capture` of the way from average entry price to
     a dollar AND `bid - fee(bid) - stale_cost_sell` beats the average cost per contract. Limit = the lowest
     displayed bid for which that is still true.
   `stale_cost_sell` is **[MEASURED on v2's sales: a proxy]** (6.3, M2s). It is charged in the TEST, not in the
   ledger: the sale is booked at the displayed bid, like a buy at the displayed ask. The review's headline finding
   (41% of early-sold contracts were beyond the displayed bid) was about sales, so sales are not to be the one
   place where a stale book costs nothing.
   The order IS sent even if the engine can see an empty side: the `cancelled` row is the evidence (example E).
2. **Otherwise maybe enter**: the drift gate (4.3), the inherited time/band/max-bets/min-gap tests (6.4), the edge
   test (4.3), the size and its per-price ceilings (4.5).

An account with `MayOrder` false (settle-only, 5.4) or `Exhausted` is skipped before step 1: it sends nothing.

| Answer | Entry | Exit |
|---|---|---|
| `filled` | position grows; `Entries++`; window used grows | position closes; `Exiting = ""` |
| `partial` | the same for what filled; the rest is gone (IOC). ONE bet toward `max_bets`. A later second may try again after `min_gap`; the holds guarantee it is not the same contracts. | position shrinks; `Exiting` stays; next second the rule is RE-EVALUATED from scratch on the new book: still fires -> the remainder is offered again; no longer fires -> `Exiting = ""`, the rest is held. |
| `cancelled` | nothing happened: no bet counted, no gap started. Journal thinning keeps a run of identical failures to one row per 15 s. | `ExitTries++`, `BlockedSeconds++`. The position rides. Below `min_tau` no more orders; it settles, booked as held to settlement, `BlockedSeconds` in the settlement event. |
| `rejected` | logged with the reason; not retried that second | same |

`decision.action` is the INTENT (`enter`/`exit`); whether it filled is on the order. `blocked_by` is never set on a
row that sent an order.

### 4.5 Sizing: one window across all coins is ONE bet

At a candidate buy in window `w` (all markets sharing one `closes_at`):

- `u = c + fee(c) + stale_cost`; `k = (p_side - u) / (1 - u)` (v2's Kelly form, the review checked it).
- `E_w` = the bucket's cash when it forms its first buy intent of `w`; frozen, so poller order cannot change the base.
- `k_w = max(KMax_w, k)`; `kappa = 0.25` **[CONVENTION]**.

```
budget_w = floor(kappa * k_w * E_w)                          // quarter-Kelly of the window's single best bet
cap_w    = window_cap_bps * min(E_w, seed_cents) / 10000     // owner's limit (2500 = v2's TotalCap), on the STARTING balance at most
room     = min( budget_w - Used_w, cap_w - Used_w, CashCents )
stake    = min( floor(kappa * k * E_w), room )
qty      = floor(stake / (c_cents + fee_per_contract_cents)) // what is actually paid; stale_cost is not a ledger cost
steps    = for each displayed taker price t_i from c up to the Limit of 4.3:
               { UpTo: t_i, MaxCostCents: min( floor(kappa * k(t_i) * E_w), room ) }   // k(t) is the formula above at t
order    = buy qty, Limit from 4.3, MaxCostCents = stake, CostSteps = steps;  no order if qty < 1 (blocked_by "window budget spent" etc.)
```

**Sized along the walk.** The stake above is quarter-Kelly at the BEST ask, and the order may walk to worse prices,
where the edge is smaller. So the order carries one ceiling per price: what has been spent once a fill at `t_i` is
included may not pass quarter-Kelly computed AT `t_i` (2.1 `CostStep`, enforced by the broker in 2.3). `k(t)` falls
to nothing at the limit, so the order tapers instead of spending a best-ask stake at the worst acceptable price.
Pure arithmetic in `sizing.go`; no new parameter.

`Used_w = OpenCents_w + LostCents_w`: the cost basis of what this window still holds, plus the losses it has
already realised. A sale releases the basis it sold; if it sold below that basis the shortfall stays in `Used_w`
for the rest of the window; a gain frees nothing beyond the basis. (Cost less proceeds, the earlier rule, let a
losing round trip hand its budget back: with `max_bets` 25, an 8 s gap and five coins a window could LOSE several
times the quarter-Kelly amount that the rule says is its whole risk.) The five coins are treated as perfectly
correlated whatever the direction: the conservative bound, not the measured 0.6 **[CONVENTION]**. A measured
design effect would be a later version and a new trial (M3 records the figure).

Worked, on the DOGE book of 2.4 with `E_w` = 100000c, `p` = 0.16, best ask 0.12, and `stale_cost` = 0.007
(ILLUSTRATIVE: the review's point estimate; the real value comes from M2): fee 0.007392, `u` = 0.134392,
`k` = 0.025608 / 0.865608 = 0.029584, stake floor(739.6) = 739c. Paid per contract 12.7392c -> `qty` = 58. Limit: price
+ fee + stale at 0.13 is 0.1449, at 0.14 is 0.1554, at 0.15 is 0.1659 > 0.16, so Limit = 0.1400. Steps: at 0.13
`k` = 0.015083 / 0.855083 = 0.017639 -> 440c; at 0.14 `k` = 0.004572 / 0.844572 = 0.005413 -> 135c; so
`[0.12: 739] [0.13: 440] [0.14: 135]`. Fill on the recorded book (161 shown at 0.12): 58 at 0.1200, premium 696c,
fee ceil(42.87) = 43c, 739c paid, and the steps never bind. `Used_w` = 739 = `budget_w`: a SOL signal with
`k` = 0.02 in the same window gets min(500, 0) -> "window budget spent". A BTC signal with `k` = 0.10 raises
`budget_w` to 2500c: 1761c left. v2 would have sized all three independently under the shared $250.

The same order against a THIN top level, 20 shown at 0.12: 20 fill there (premium 240c, fee 15c, 255c). At 0.13
the ceiling is 440c: 13 more fit (premium 169c, fee share 11c, 435c in all; a 14th would make 448c). At 0.14 the
ceiling is 135c, already passed, so the walk ends: 33 filled for 435c, 25 unfilled. Without the steps the order
would have spent up to the full 739c at 0.13 and 0.14, more than five times what quarter-Kelly allows at 0.14.

A losing round trip: that 739c position is sold for 400c. `OpenCents` 739 -> 0, `LostCents` 0 -> 339, `Used_w` = 339:
400c of budget is free again, not 739c. After enough losing trips `LostCents` alone reaches `budget_w` and the
window is shut for every coin.

---

## 5. The runner: `service/internal/runner/runner3.go` (`Runner3`, engine name `kalshi15m3`)

### 5.1 Locks, and who may wait on them

`Runner3` has two locks and its own goroutine.

- `coinMu` guards `Model` (the per-coin volatility, price ring and offsets) and nothing else. It is held only
  across arithmetic, never across a database or broker call.
- `r.mu` guards the engine's accounts, the state (5.3) and a generation counter `gen`. `Step` and the settlement
  path hold it across their ledger write, as v2 does.
- **Nothing called from a poller or from the Coinbase stream ever WAITS on `r.mu`.** `Observe` and `Inputs` take
  `coinMu` only. `Step` and `Settled` use `r.mu.TryLock()`: if another coin's step, a settlement or a swap holds
  it, they return at once (`skipped_busy` is counted in `/api/status`). A skipped `Step` costs one second of one
  coin: the book was not observed, which can only leave holds too high; a skipped `Settled` is picked up by the
  sweep. Lock order where both are needed is `r.mu` then `coinMu`.
- `Run(ctx)`, v3's goroutine, ticks every 10 s **[CONVENTION: a retry cadence; it gates no trade]**: rebuild if
  suspended, write-probe if paused, and once a minute the sweep and the cash check (5.3). The sweep and the cash
  check run ONLY when v3 is not suspended: in a tick that finds it suspended the order is rebuild first, and the
  sweep follows in the same tick only if that rebuild succeeded. It is started whenever v3 holds a bucket, also
  with `AC_V3` off. It and `Heal` (5.5, on the snapshot goroutine) are the only callers that block on `r.mu`,
  besides `Book()`.

What this does NOT remove: v3's own write happens on the poller's goroutine after v1 and v2 have stepped, so a slow
database lengthens that poller's pass by v3's write, exactly as v2's write does today. S5 measures it.

### 5.2 One step

```
Inputs(coin, m, closes, price, at, v2in):                      // BEFORE InsertEvaluation; coinMu only; inside safely(), nil on panic
    view := r.model.View(coin, m, price, at, v2in); remember it for this coin and second; return its journal map

Step(coin, evalID, at, marketID, info, closes, q, price):      // AFTER v1 and v2; inside safely(); returns nothing
 0  price == "" -> return.  !r.mu.TryLock() -> skipped_busy++, return.  suspended -> return (Run heals).
 1  r.paper.ObserveBook(ticker, evalID, at, closes, q)          // holds refreshed first
 2  paused, or no account with MayOrder -> return (observe-only / settle-only)
 3  decisions, intents := r.engine.Decide(coin, m, q, view, at) // no money moves; the view from Inputs, no CoinState read
 4  reports := for each intent: r.broker.Submit(ctx, intent.Order)
 5  thin decisions exactly as runner2.go:280-287 (15 s, inherited); a decision that sent an order is always kept
 6  nothing to write -> return
 7  err := r.db.RecordOrders(ctx2s, ...)                        // its own 2 s budget [CONVENTION]
 8  err == nil                          -> engine.Apply(reports); broker.Commit(ids); gen++; failures = 0
    definitelyRolledBack(err) (section 3) -> broker.Void(ids); failures++       // the server said no: memory == ledger
    anything else (deadline, connection) -> broker.Void(ids); suspend("the outcome of a write is unknown", healAfter = now + 10 s)
    failures >= 3 [CONVENTION]          -> pause("the database is refusing v3's writes")
```

Why a timeout is not treated as a refusal. `RecordOrders` of five fills is about thirty statements; on a Pi under
load a 2 s overrun will happen. When the deadline fires during `COMMIT` the client drops the connection, and the
server may still commit a moment later. Asking `OrdersRecorded` at once can therefore answer "none" about a step
that lands just afterwards; acting on that would release holds for contracts already taken, leave memory behind the
ledger with no flag, and let the engine re-send the same intent under a new client id (the evaluation id differs,
so the unique index does not catch it). So the runner never asks at once. It suspends, and the rebuild, not before
`healAfter` **[CONVENTION: 10 s; a dropped connection's transaction ends within the server's own bookkeeping, far
sooner]**, reads what is true. `OrdersRecorded` is used BY the rebuild's report ("the step that suspended me did /
did not commit") and by tests; it no longer decides anything in `Step`. A late commit that beats even that is
caught by the once-a-minute cash check below.

A step with no orders in it (decisions only) that fails to write is dropped and counts toward `failures`, whatever
the error: no money was in it, so there is nothing to be unsure about. It never suspends, exactly as v2 tolerates a
lost decision-only write.

### 5.3 Settlement, the sweep, and the cash check

`Settled(marketID, info)` (from the sink, `TryLock`) and the sweep share one function.

**Both RETURN AT ONCE while v3 is suspended**, exactly as `Runner2.Settled` does when v2 is halted **[READ
runner2.go:344-348]**, and so does the once-a-minute cash check. Suspended means memory may be BEHIND the ledger
(5.2: a timed-out order may still commit). `SettleRows` is built from memory, so a settlement written then could
pay 100 contracts where the ledger holds 150; and that row can never be corrected: `unique (market_id, bucket_id,
side)` refuses a second one, the rebuild's `open_lot` ignores a lot that has a settlement row, and the cash check
cannot see it, because after the rebuild memory equals a ledger that simply lacks the payout. The 50 would be a
stranded lot, and `analysis.Reconcile` would hold the window back for v1 and v2 for good. So the position waits:
it is settled by the sweep that follows the successful rebuild. Nothing is lost by waiting, because
`RecordResult` has stored `market.result` before `Settled` is ever called **[READ main.go:519-522]** and the sweep
reads the result from that table. This also makes 5.4's statement true that nothing but a panic or another
rebuild advances `gen` while v3 is suspended.

When not suspended: `rows := engine.SettleRows(ticker, result)`. **Last look before the write:** each row's `Qty`
is compared with the ledger's net-open quantity for that (bucket, market, side) (`OpenQty`, the `open_lot` sum of
5.4 for one market; one read, inside the same 2 s budget). Any difference -> suspend and write NOTHING; the rebuild
then reads the truth and the sweep settles the full quantity. This closes the case the suspended test alone does
not: a commit that lands later than `healAfter`, after a rebuild has already succeeded without it and before the
minute's cash check. It is a read that fails toward reporting: if `OpenQty` itself cannot be read, nothing is
written and the next sweep tries again.

```sql
-- OpenQty. Never run. $1 = market id, $2 = v3's bucket ids.
select o.bucket_id, o.side, sum(case when o.action = 'buy' then f.qty else -f.qty end)::int
  from trade_order o join fill f on f.order_id = o.id
 where o.market_id = $1 and o.bucket_id = any($2)
 group by o.bucket_id, o.side
having sum(case when o.action = 'buy' then f.qty else -f.qty end) > 0;
```

Then `RecordSettlements(ctx, setup, ...)` (2 s **[CONVENTION]**) with the `SimSetup` of 5.4; on nil ->
`ApplySettlement`, `gen++`, `paper.Forget`, `model.NoteSettlement`, exhaustion check (5.6), save `engine_state`.
On error -> `SettlementsRecorded(marketID, bucketIDs)`: all found (the write had committed, or a retry hit
`unique (market_id, bucket_id, side)`) -> apply; otherwise leave the position as it is and let the next sweep try
again. That is safe here, and not for orders, because a settlement is idempotent by that unique key: a late commit
makes the retry find the rows. If the query itself fails -> suspend.

**The sweep** runs at start (after the first rebuild, and only if it succeeded), once a minute, and from `Heal`
just before a snapshot (5.5), never while v3 is suspended: for every open position whose `Close` has passed, in
EVERY held bucket, read `market.result`; if present, settle as above. It needs `market.result`, `OpenQty`,
`RecordSettlements`, `SettlementsRecorded` and the `SimSetup` that `NewRunner3` obtained in every mode (5.4: the
service actor and the paper venue's ledger id, neither of which depends on `AC_V3` or on any version's status):
no book, no model, no version status. So it runs when `AC_V3` is off, when no version is tradable, and for a
bucket whose version was set to `bench` or `retired` with a bet open. v3 does not depend on the poller's single
callback. A v3 position in a closed round blocks value snapshots for as long as the result itself is missing,
which blocks v2's equally, OR for as long as v3 is suspended, which is next.

**What a suspended v3 costs everyone, said plainly.** `SnapshotRefusal` refuses the whole minute, for every
engine, while any book has `Halted != ""` **[READ books.go:353]**, and a suspended v3 sets it (5.4). That was
already so; what the rule above adds is that a round which closes DURING a suspension stays unsettled until the
heal, so for that long `analysis.Reconcile` also holds that window back for v1 and v2. How long:

- *A write whose outcome is unknown, database otherwise well.* `healAfter` (10 s **[CONVENTION]**) plus one
  rebuild. `Heal` runs before every snapshot and does rebuild THEN sweep in one call, so the closed round is
  settled in the same call that lifts the halt, and the deferral costs no minute beyond the one the suspension
  could already cost. **[INFERRED, not measured]**: this holds only if rebuild and sweep fit in `Heal`'s 3 s; if
  they do not, `Run`'s next 10 s tick finishes the job and one more snapshot is lost. S5 measures it.
- *The database cannot answer.* For as long as that lasts; no snapshot could have been written anyway, and v2 is
  in the same position.
- *The rebuild keeps FAILING for a reason that is not weather* (the self-check or the cash check disagrees every
  time): **not bounded by time.** v3 stays suspended, its closed round stays unsettled, every engine's snapshots
  are refused each minute and logged as a fault, and the window stays held back, until a person looks. This is
  the same exposure v2's permanent halt has today **[READ runner2.go:344-348, books.go:353]**, not a new one, and
  v3 is better off in one respect: whenever it does heal, the sweep settles every round it missed, however old,
  from `market.result`, and the held-back windows return. `/api/status` shows the suspension's reason and since
  when; the home page's "value snapshots have stopped" notice (`2f97595`) shows it to the owner. The alternative,
  leaving a suspended v3 out of the books so the others' snapshots go on, would write a false step into an
  append-only table; it is the owner's call (D21), and the default is to refuse.

**The cash check** runs with the sweep, once a minute (so, like it, not while suspended), and at the end of every
rebuild: each held bucket's `CashCents` against `BucketCash` (the ledger sum). A difference suspends and rebuilds. (Before, it ran only after a
settlement, up to fifteen minutes apart.) The read is made outside `r.mu` and compared under it only if `gen` has
not moved meanwhile; if it has, the check is simply repeated next minute.

### 5.4 States, and what survives a restart

Two separate questions, per bucket: **is it held** (valued from v3's book and settled by the sweep) and **may it
place orders**.

| | Decided | Rule |
|---|---|---|
| held | once, in `NewRunner3` | every sim bucket of `kalshi15m` version 3 that is not frozen (`HeldBuckets`), whatever `AC_V3` or the version's status |
| may order (`MayOrder`) | once, in `NewRunner3` | `AC_V3` on AND the version is `probation` or `active`. An account that is `Exhausted` sends nothing either (4.4), but that flag is derived by the rebuild, after it knows what is open, not here |

A held bucket that may not order is **settle-only** (D17): no entries and no exits; what it holds rides to settlement and
is booked by the sweep. That is the meaning of switching `AC_V3` off, and it harms nobody: the bet is valued at the
bid, settled on time, and `analysis.Reconcile` finds held == settled, so the window stays on v1's and v2's
leaderboard. (Were the bucket simply dropped, the lot would never settle, the stake would read as lost, and
`Reconcile` would hold back the WHOLE window for every engine for as long as v3 stayed off.)

| State | Entered when | Orders | `Book().Halted` | Leaves when |
|---|---|---|---|---|
| running | | by `MayOrder` | "" | |
| **paused** | 3 **[CONVENTION]** consecutive writes that the server REFUSED (or decision-only writes that failed) | none | **""**: every refused write certainly rolled back, so memory equals the ledger, the book is true, snapshots go on | the first successful write-probe (a decision-only record, tried by `Run` every 10 s) |
| **suspended** | a write whose outcome is unknown (deadline, connection); `SettlementsRecorded` failing; the settlement path's last look disagreeing with memory (5.3); a recovered panic; a failed self-check or cash check | none, and NO settlement either: `Settled`, the sweep and the cash check return at once (5.3) | the reason | `rebuild()` succeeds. Tried by `Run` every 10 s and by `snapshotValues` just before it reads the books, neither before `healAfter`. A snapshot is lost only if it falls inside those 10 s or the database cannot answer, in which case it could not have been written anyway |
| observe-only | `AC_V3` on and nothing is held (no version approved yet, or 0013 not applied) | none; `p_model` is journaled every second | an empty book | restart after an approval |
| absent | `AC_V3` off and no v3 bucket exists, or 0012 is not applied | | not in the books; there is nothing to value | restart |

If `HeldBuckets` cannot be read at start the service does not start (D19), the same as `NewRunner2` failing today
**[READ main.go:129-131]**: a v3 bucket that may hold a bet must not run unheld, and a database that cannot answer
that read will not let v2 start either. `deploy.sh` then rolls back. If the rebuild of a held bucket fails at
start, `NewRunner3` still returns the runner, SUSPENDED, with the bucket ids known (so `heldElsewhere` is right);
`Run` heals it. In that case nothing is swept and no bucket is closed at this start (steps 4 and 5 below are
skipped): what is open is not known yet. If `EnsureSimSetup` fails the service does not start either: it is the
same function, on the same accounts, that `NewRunner2` is about to call, so v2 could not have started.

**`NewRunner3`, once, before `NewRunner2`:**

1. `names` = `TradableVersions("kalshi15m", 3)` when `AC_V3` is on, else EMPTY. Then ALWAYS, in every mode,
   `setup := EnsureSimSetup(ctx, "kalshi15m3", "kalshi15m", 3, names, 100000)` (unchanged function). For a name in
   `names` it creates and seeds a bucket that does not exist yet. With `names` empty it creates no bucket and
   moves no money: the bucket loop ranges over `names`, and everything outside it is an `on conflict do nothing`
   insert of accounts v2 made long ago, followed by reads **[READ sim.go:45-154; section 1; never run with an
   empty list, so S5 checks it on dev]**. What it returns is what `RecordSettlements`, `RecordOrders` and
   `CloseBucket` cannot work without: `ActorID`, `VenueLedgerID`, `FeesLedgerID`, `PoolLedgerID` **[READ
   sim.go:163-199, 367-399]**. The setup is kept for the life of the runner. A held bucket that is not in
   `setup.Buckets` (a retired version; any bucket when `AC_V3` is off) carries its own ledger account id from
   `HeldBuckets`. This is the ONLY place v3 calls it. (An earlier revision called it only with `AC_V3` on; the
   settle-only sweep then had a zero setup, every WINNING settlement failed the `created_by` foreign key once a
   minute for ever, and every engine's snapshots stopped.)
2. `HeldBuckets("kalshi15m", 3)`: the candidate set, tradable or not. `MayOrder` is set per bucket.
3. `rebuild()` over that set: this, and nothing before it, establishes what each bucket still holds. Its last
   step derives `Exhausted` (see there).
4. If the rebuild succeeded: the start-up sweep (5.3), which may settle an old lot and change a bucket's cash.
5. If the rebuild succeeded: every bucket that is `Exhausted` NOW, which means ledger cash under 100c
   **[CONVENTION, inherited]** AND no open position after steps 3 and 4 (no `open_lot` row), is closed with
   `CloseBucket(ctx, setup, ..., restake = false)` (5.6) and leaves the set. Never on the cash test alone: a bucket
   that staked nearly all its cash and still holds the bet is exactly the one with cash under 100c, and
   `CloseBucket` would freeze it without looking **[READ sim.go:163-199: no position test]**; `HeldBuckets` would
   then drop it, the lot would never be swept, and the window would be held back for v1 and v2. A `CloseBucket`
   that fails is logged and the bucket simply stays held, exhausted, until the next start.
6. The ids of the set as it now stands go to `NewRunner2`'s `heldElsewhere`. From here on the set is fixed.

A status change made while the service runs (approval, `bench`, `retired`) takes effect at the next restart (D18).
`/api/status` compares the stored status with what the runner loaded and says "approved, waiting for restart" or
"no longer tradable: settle-only after restart"; `approve-version.sh` prints the same. Stopping orders NOW is
`AC_V3=off` and a restart, which keeps every bet held and settled.

**`rebuild()`** (start, and recovery). It never creates, adds or drops a bucket. All reads happen OUTSIDE `r.mu`,
into a fresh set of accounts and holds; then, under `r.mu`, the result is swapped in only if `gen` is what it was
when the reads began (the pattern of `RefreshCapital`, books.go:255-286); otherwise it is thrown away and tried
again at the next tick. While suspended nothing advances `gen` but a panic or another rebuild: `Step`, `Settled`,
the sweep and the cash check all return at once (5.2, 5.3).

1. Cash: `BucketCash` for the fixed bucket set; **ledger cash IS `CashCents`**. There is no saved cash to disagree with.
2. Fills: every v3 fill of those buckets that is either in a market with no result yet, or belongs to a lot that is
   NET open and has no settlement row. Net, exactly as `RealisedByCoin` decides it **[READ store/home.go:386-395]**:
   a position sold out before the close has no settlement row and never will, and must not read as an open bet.
   No time bound: the net-open lots are found through `trade_order (bucket_id, placed_at)` over v3's buckets only,
   a few thousand rows at most.

```sql
with open_lot as (                       -- bought more than sold, and never settled: still a bet
    select o.bucket_id, o.market_id, o.side
      from trade_order o join fill f on f.order_id = o.id
     where o.bucket_id = any($1)
       and not exists (select 1 from settlement x
                        where x.market_id = o.market_id and x.bucket_id = o.bucket_id and x.side = o.side)
     group by o.bucket_id, o.market_id, o.side
    having sum(case when o.action = 'buy' then f.qty else -f.qty end) > 0)
select o.bucket_id, o.market_id, m.ticker, i.underlying, extract(epoch from m.closes_at)::float8, m.strike::float8,
       m.result, o.action, o.side, o.detail, f.id, f.qty::int, f.price::text, f.fee_cents,
       coalesce(e.amount_cents, 0)       -- a sale whose premium equalled its fee has no bucket entry (section 3)
  from trade_order o
  join fill f              on f.order_id = o.id
  join bucket b            on b.id = o.bucket_id
  left join ledger_entry e on e.transfer_id = f.transfer_id and e.account_id = b.ledger_account_id
  join market m            on m.id = o.market_id
  join instrument i        on i.id = m.instrument_id
 where o.bucket_id = any($1)
   and (m.result is null
        or exists (select 1 from open_lot l
                    where l.bucket_id = o.bucket_id and l.market_id = o.market_id and l.side = o.side))
 order by f.id;
```

   Folded oldest first through `Position.Apply`, which also rebuilds each window's `OpenCents` and `LostCents`
   (that is why a fully sold position in a window still open is read too). A net-open lot whose market already HAS
   a result, however old (`AC_V3` was off for a day, the service was down over a close), is not a reason to
   suspend: it is handed to the sweep, which settles it at once. There is no look-back window and no guard count.
3. Windows: `E_w` and `k_max` from `detail.window` of the window's orders; `OpenCents` and `LostCents` from step 2.
4. Self-check: per open position, the folded `CostCents` must equal (sum of `-amount_cents` over its buy fills) less
   (sum of its sell orders' `detail.cost` in cents); and the cash check of 5.3. A failure leaves v3 suspended with
   the reason. v2's rule, kept, with a way back.
5. Paper holds: for every fill of step 2 in a market still open, `hold[{bucket, ticker, ladder, bid}] += qty`. The
   first `ObserveBook` applies the fall-and-displace rule to them from the book it sees. Releases and displacements
   from before the restart are not remembered: every contract ever taken starts held at the price it was taken at,
   which errs toward holding too much.
6. `engine_state['kalshi15m3']` holds only what the ledger cannot know: per coin the learned index offsets and
   round counters. Volatility and the price ring are seeded ONCE, at start, from the exchanges with the same
   `seedData` helper v2 uses; a rebuild does not touch the model. Losing the blob costs a few settlements of the
   constant offset (during which the drift gate, 4.3, blocks entries), and no money.
7. `Exhausted`, LAST, per account, from what steps 1 and 2 found: cash under 100c and no open position. It is an
   in-memory flag that no table stores, so every rebuild derives it again, and only here, after the fills have
   been folded. The rebuild only sets the flag; closing a bucket happens in `NewRunner3` alone (step 5 above).

A kill at any instant leaves one of two states: the step's transaction committed (rebuild sees it) or it did not
(nothing happened).

### 5.5 Wiring in `main.go`

- `config.Config.V3` from `AC_V3`, default **off**. Releasing the code and switching v3 on are separate acts.
  Off means "no new orders"; it does not mean "v3's bets are abandoned" (5.4).
- `NewRunner3` is built BEFORE `NewRunner2`; its bucket ids are appended to the `heldElsewhere` passed to
  `NewRunner2`, so v2's first capital read already contains v3's seeds (otherwise $2,000 of seed reads as earned).
  The ids never change while the service runs (5.4), which `heldElsewhere` and v2's cached capital both require:
  a bucket seeded mid-run would show as +$2,000 of value with nothing contributed, in an append-only table.
- `NewRunner3` finding nothing to hold and nothing to trade (0012/0013 not applied, `AC_V3` off with no v3 bucket)
  is logged and the service carries on WITHOUT v3. It has still called `EnsureSimSetup`, with no names, which
  created no bucket (5.4). Failing to find out is a failed start (5.4).
- Every v3 call made from someone else's goroutine is inside `safely(name, fn)`, which recovers a panic
  (suspending v3 with its text) and logs any error, and none of them waits on `r.mu` (5.1):
  - `model["v3"] = run3.Inputs(coin, m, closes, price, at, v2in)`, BEFORE `InsertEvaluation` and so before v1 and
    v2 step: `coinMu` only, `nil` on panic. `v2in` is the map the sink has just built for `model["v2"]`. It
    journals sigma2, offset, source, lambda, `p_model`, the v2-reference `p_model` and `drift`, EVERY second, also
    in observe-only mode. Later measurements then use unthinned rows, and the future replayer needs no model
    reconstruction.
  - `run3.Step(...)` and `run3.Settled(...)`, LAST in `SaveQuotes` / `SaveResult`, `TryLock`. Nothing from v3 is
    returned to the poller.
  - `run3.Observe(t)` in the Coinbase callback, after v1's and v2's: `coinMu` only. A rebuild or a slow write can
    no longer stall the one stream goroutine and back up the tick writer.
- `go run3.Run(ctx)` (5.1), under the same `WaitGroup` as the other workers.
- `snapshotValues` calls `run3.Heal(ctx3s)` (3 s, the budget `RefreshCapital` already gets there **[READ
  main.go:407]**; rebuild FIRST if suspended; then, only if v3 is not or no longer suspended, sweep if a position
  is past its close; a failed rebuild means no sweep; reads outside `r.mu`) before `src.Valuation()`. `src.Valuation()` includes `run3.Book()`.
- **No change to `runner2.go`.** v3 moves no capital at run time (5.6) and reads v2's inputs through the existing
  `Inputs` method, so v2's cache is never made stale by it.

### 5.6 Running out, and sustainment

A v3 account with cash under 100c and nothing open (v2's `BankruptAt`, **[CONVENTION, inherited]**) sets `Exhausted`
and stops ordering; the bucket stays `active` in the database and stays HELD. **At the next service start**,
`NewRunner3` (which runs before `NewRunner2` reads the capital) calls `CloseBucket(restake = false)` with the
setup it always has: reaped, frozen, not replaced (the brief's lifecycle; D7). The order there is fixed (5.4): the
rebuild first, then the start-up sweep, and only then the test "cash under 100c AND nothing open". `Exhausted` is
never derived from cash alone, at start or in a rebuild, because `CloseBucket` does not look for open positions
and a frozen bucket is no longer held or swept. No capital moves while the service runs, so no hook into v2 is
needed. v3 takes no sustainment allocation in this step (every rate is zero today; D8); `HighWaterCents` is nil,
as for v1.

---

## 6. Every number: measured, or labelled

### 6.1 Provenance travels with the version

```go
type Provenance struct { Kind string /* measured | convention | inherited | limit | fact */; Value float64; Note string }
type Params struct { ...fields... /* incl. Lambda, StaleCost, StaleCostSell, DriftTol */; FeePerFill bool; Provenance map[string]Provenance; ProtocolSHA, ResultSHA string }
```

`Validate` (and a reflection test) refuses a `Params` with any numeric field missing from `Provenance`. A test
asserts that migration 0013's params JSON equals the Go definitions.

### 6.2 The measurement protocol: committed BEFORE anything is run

`docs/v3-measurement-protocol.md`, committed in step S0 with this section's content. `cmd/measure3` embeds its
sha-256 and refuses to run if the file differs.

- **Unit**: the window (all markets sharing a `closes_at`). Eligible if every kalshi15m market in it has a result
  and at least one scored row. Ineligible windows are listed with the reason; none is dropped by hand.
- **Burned**: the 38 windows behind the review, and everything closing before the protocol's commit time `T_c`,
  may be used for estimation only, never for confirmation.
- **Split, by rule**: TRAIN = the first 480 eligible windows from v2's first trade (about five days). Embargo = the
  next 24 windows (six hours; 48 if the block is doubled, see Standard errors). TEST = the next 192 eligible windows, and every one must close after `T_c`. 480 and
  192 are **[CONVENTION]**, fixed now, not revisited after seeing outcomes. The data accrues while S1 to S5 are built.
- **Standard errors**: delete-one-block jackknife over 6-hour blocks (`floor(epoch/21600)`). The block length is a
  **[CONVENTION, motivated by the offset memory]**: 24 rounds is how long one learned offset stays in the model
  (trader.go), which bounds ONE source of dependence between windows. It does not bound the serial correlation of
  the per-window statistics themselves (volatility regimes), and that can be measured: `measure3 train` reports the
  autocorrelation of the per-window M1 statistic `sum_A d(y-q)` and of the per-window Brier difference at lags 1 to
  8 and at lag 24 (one block), each with its jackknife SE. **Stated now**: if the lag-24 autocorrelation of either
  exceeds its own SE, the block is DOUBLED to 12 hours for every SE in this protocol, TEST included, and the
  embargo doubles with it to 48 windows. This is decided by TRAIN alone, before any TEST row is read.
  Deterministic: a rerun reproduces the digits.
- **Looks**: TRAIN figures may be recomputed freely (they choose nothing but the closed-form values below). TEST is
  opened ONCE. The tool refuses to read TEST rows unless `research/v3/frozen-params.json` exists with the matching
  protocol sha, and refuses if `test-result.json` exists. Results are committed JSON files with the git sha, the
  split bounds and the windows used.
- **Who runs it**: read-only, as `assetcracker_ro`, over the documented ssh tunnel (D13).

### 6.3 The measurements

**M1. lambda.** Rows: v2 originals' decisions, one per evaluation, the FIRST row in each 15-second bin per market
(the journal writes extra rows when a strategy acts; binning stops action seconds being over-weighted;
**[CONVENTION]**, stated in advance), two-sided books only, restricted to the actionable set A: for the side the
model favours, `p_s - ask_s - 0.07 ask_s (1 - ask_s) > 0` with asks from `evaluation.quotes`. No free number
enters A. With `d = p - q`, `y` the result:

```sql
select distinct on (e.market_id, floor(extract(epoch from (m.closes_at - d.at)) / 15))
       extract(epoch from m.closes_at)::bigint as w, d.model_prob as p, d.market_prob as q, (m.result = 'yes')::int as y,
       (e.quotes->>'yes_ask')::float8 as yes_ask, (e.quotes->>'no_ask')::float8 as no_ask
  from decision d
  join evaluation e on e.id = d.evaluation_id and e.at = d.at
  join market m on m.id = e.market_id
 where d.strategy_version_id = any($1) and d.at >= $2 and d.at < $3 and e.at >= $2 and e.at < $3
   and m.result in ('yes','no') and d.model_prob is not null and d.market_prob is not null
   and (e.quotes->>'yes_bid')::numeric > 0 and (e.quotes->>'no_bid')::numeric > 0
 order by e.market_id, floor(extract(epoch from (m.closes_at - d.at)) / 15), d.at, d.id;
```

`L_hat = sum_A d(y-q) / sum_A d^2` (closed form: nothing is searched, so no hidden trials). Self-check the tool must
pass: `Brier(model) - Brier(mid) = V (1 - 2 L*)` on the same rows, so "the model is worse than the mid" is exactly
`L* < 1/2`. **Frozen value: `lambda = clamp(floor(100 (L_hat - t_{0.95,B-1} SE)) / 100, 0, 1)`**, the lower one-sided
95% bound: that is the meaning given here to "as far as the evidence supports" (0.95 is **[CONVENTION]**; D2).
Reported and never used to choose: `L_hat` on all rows, by coin, by price band, by tercile of |d|. Known weakness,
stated in advance: v3 acts at the large-|d| end of A, where the model's errors may concentrate; only the live
per-fill term (7.5) watches that.

**M2. stale_cost (buying).** For every v2 original BUY in TRAIN: the ask on the bought side in the NEXT snapshot of
that market (the first `evaluation` row after `placed_at`, within 3 s **[CONVENTION: snapshots are a second apart;
a later one is a different question]**; buys with no such row are counted and reported, not imputed) minus the ask
at entry, from `evaluation.quotes`. Window-clustered mean, block-jackknife SE. Frozen value: `max(0, mean)` rounded
to 0.0001. It is a cost, so the point estimate is charged, and the SE is recorded. (The review's figure on 9.5 h
was +0.70 to +0.77c.)

**M2s. stale_cost_sell (selling).** The mirror image, over v2 originals' SELLS in TRAIN: the bid on the sold side
at the sale minus the bid on that side in the next snapshot (same 3 s rule); a side with no bid in the next
snapshot counts as a bid of 0. Same mean, SE and freezing rule. Charged in both exit tests (4.4).

**What M2 and M2s are, said plainly.** They are measured, but on a DIFFERENT population from the one they gate:
v2's orders fire on the raw model and fill at the touch; v3's entries fire only where `lambda (p_model - mid)`
clears spread, fee and this very cost, which is where the disagreement with the market is largest and adverse
selection plausibly worst, and they may walk below the touch. So `Provenance.Note` for both reads "measured on v2's
orders in TRAIN; a proxy for v3's". **Stated now**: the live next-second re-pricing of v3's OWN fills (7.5) replaces
both proxies in any later version, and that later version is a new trial.

**M3. outcome correlation across coins**, from `market` and `instrument` over complete windows. Recorded only; sets
no parameter (sizing assumes 1).

**Decision rules.**
- R1: `lambda = 0` -> no version is registered (section 6.5).
- R2 (TEST, once): with lambda frozen, the per-window mean of `Brier(mid) - Brier(blend)` on TEST rows in A must have
  a block-jackknife `t >= t_{0.95,B-1}`, `B` being the number of blocks TEST actually has (one-sided 5%,
  **[CONVENTION]**). 192 windows are 48 hours: 8 six-hour blocks, 7 degrees of freedom, threshold **1.895**; if the
  block was doubled, 4 blocks and **2.353**. (Against the normal 1.645 the gate's real false-pass rate would be
  about 7%, not 5%. M1 already uses the t quantile.) If eligibility gaps leave a different `B`, the tool uses the
  realised `B` and prints it. Fail -> no version is registered; the negative result is written up. A further
  attempt is a new protocol on new data.
- R3: nothing else is estimated, tuned or compared on the recorded history in this step.

### 6.4 The parameter table

| Parameter | Scalper v3 | Value v3 | Kind | Source |
|---|---|---|---|---|
| `lambda` | M1 | same | measured | 6.3 |
| `stale_cost` | M2 | same | measured on v2's buys: a proxy | 6.3 |
| `stale_cost_sell` | M2s | n/a (never sells early) | measured on v2's sells: a proxy | 6.3 |
| `drift_tol` | S5 | same | measured on dev | 4.3; 99th percentile of the fork's difference from v2's model |
| fee | 7% p(1-p), ceil per order | | fact | venue schedule; rounding across fills [ASSUMED], `fee_per_fill` false |
| slippage, `min_edge`, `exit_margin`, `max_stake`, WindowCap | removed | removed | | guesses at costs now exact or measured; caps replaced by 4.5 |
| `kappa` | 0.25 | 0.25 | convention | quarter Kelly; a risk preference, not measurable |
| window correlation | 1 | 1 | convention | conservative bound; M3 records the measured figure |
| `window_cap_bps` | 2500 | 2500 | limit | the owner's; v2's TotalCap, on min(E_w, seed) |
| seed | 100000c | 100000c | convention | same as v2, so the lines compare |
| exhausted floor | 100c | 100c | inherited | `BankruptAt` |
| `tau_min`, `tau_max` | 25, 900 | 8, 900 | inherited | parent v2 |
| `band_min`, `band_max` | 0.05, 0.95 | same | inherited | parent v2 |
| `max_bets` per market | 25 | 1 | inherited | counts buy orders with a fill |
| `min_gap`, `min_hold` s | 8, 5 | 20, n/a | inherited | parent v2 |
| `take_capture` | 0.80 | n/a | inherited | the review found its effect unresolved; Scalper IS this rule |
| `min_tau` s | 8 | 8 | inherited | parent v2 |
| levels walked | 5 | 5 | fact | what is recorded |
| coins | five | five | convention | comparability with v2 |

"Inherited" is said plainly: these are UNMEASURED conventions that still gate actions, taken unchanged from the
parent so that parent-versus-child differs only in fills, sizing and belief. They define WHEN the strategy looks,
not what a trade costs. Measuring any of them is a tuning run, which needs the replayer and its trial accounting
(section 8); that is why it is not done here. The child changes three things at once, so v3 minus v2 cannot be
attributed to one cause; the per-fill decomposition (7.5) is what separates them.

Numbers that are not version parameters but still gate what the runner or a release does, each labelled where it
is used; none is a measurement and none is presented as one:

| Number | Where | Kind |
|---|---|---|
| 3 consecutive refused writes before pausing | 5.2, 5.4 | convention |
| 2 s budget for a v3 ledger write | 5.2, 5.3 | convention |
| 10 s retry tick, and 10 s before a rebuild after an unknown outcome | 5.1, 5.2 | convention; gates no trade |
| 3 s for `Heal` before a snapshot | 5.5 | inherited from `RefreshCapital`'s budget |
| 15 s journal thinning; 100c exhausted floor; 30 windows before any verdict | 5.2, 5.6, 7.1 | inherited (`runner2.go`, `BankruptAt`, `analysis.MinWindows`) |
| "within 3 s" for the next snapshot | 6.3 M2, M2s | convention |
| at least four settlements on dev; 24 hours on dev before prod | S5, S7 | convention: enough to see each code path once, not evidence of anything |

### 6.5 If the protocol says no

Then nothing model-based is registered on prod, `AC_V3` may still be on in observe-only mode (it journals `p_model`
every second), and this step delivers the broker, the engine, the recording path, the analysis changes and
`brokercheck`'s re-pricing of v2's recorded sales. The broker's live exercise is the dev database's plumbing
versions (S5). That is a legitimate result under "assume nothing about edge". The alternative, a "fills only"
Scalper v3 at the parent's unmeasured belief, is the owner's call (D3); I do not default to it.

---

## 7. Registration and display

### 7.1 Versions

| Strategy | v | parent | Hypothesis (goes in `strategy_version.hypothesis`) |
|---|---|---|---|
| Scalper | 3 | Scalper v2 | "Scalper v2's result rests on sales that fill at any size (41% of the contracts it sold early were beyond the displayed bid). Sent through a depth-aware broker, sized as one bet per window, leaning on the market with weight lambda = <M1> and charged the measured staleness <M2>, its return per window after fees is not above zero. Stated before it ran: fewer entries than v2, exits that often part-fill, no verdict before the evidence page's own minimum of 30 windows. Refuted if the per-fill term (outcome - p) is below zero at the corrected threshold for every comparison on the page, the added ones counted (7.5)." |
| Value | 3 | Value v2 | "Value v2 blends model and market at a guessed 0.5. At the measured weight it enters rarely; where it does, its return per window after fees is indistinguishable from zero. If it is below zero at the corrected threshold, the model adds nothing even at the weight the data chose." |

Not registered: Model and Late (at a measured lambda they are Value with other gates: a third and fourth look at
one question), Favorite (already answered by the month backtest), Lottery (a documented loser). Trials 18 -> 20:
`CorrectedT` 2.991 -> 3.023; every existing row's bar rises by 0.03.

**No verdict in this plan is read at an uncorrected threshold.** Besides the two leaderboard rows, v3 adds five
verdict-bearing comparisons to the evidence page: the blend-versus-mid line, and for each of the two versions the
per-fill terms "outcome minus mid" and "outcome minus p". They are the lines that will be quoted to say the model
is or is not backwards, and they are re-read on every refresh, so they are counted: each is judged by the
existing `analysis.Verdict` rule at `CorrectedT(trials + 5)`, 3.090 at 20 trials **[computed]**, with the existing
`MinWindows = 30` **[CONVENTION, inherited: analysis.go:22]**. The leaderboard keeps `CorrectedT(trials)`. The fee
and "entry minus mid" terms are arithmetic, shown without a verdict. The count 5 is written into the protocol in
S0; adding another such line later raises it.

### 7.2 Twins: no

1. The review found the twin answers nothing ("which side lost less" mostly reflects the price band). "Is the model
   backwards" is answered without a twin by splitting each fill's P&L into fee, distance from the mid at entry, and
   outcome minus mid; v3 records the mid and `p` on every order.
2. Under a depth-aware broker a twin fills from a DIFFERENT ladder; v2's twins already matched only 68% of their
   originals' stake at top-of-book. With real depth the pair is less comparable, not more.
3. Two more trials (|t| 3.05) for a measurement the review calls no evidence. v2's twins keep running.

### 7.3 Migration `0012_trade_order_client_id.sql` (step S3; safe with no v3 running)

```sql
-- The third engine's orders go through a broker that fills only what the recorded book displayed
-- (service/internal/broker), so an order can be partly filled or not at all, and a write whose answer
-- was lost must be recognisable afterwards. v1 and v2 never name the column and leave it null.
alter table trade_order add column client_order_id text;
create unique index trade_order_client_order_id on trade_order (client_order_id) where client_order_id is not null;
comment on column trade_order.client_order_id is 'Made by the engine, unique for ever. Null for the first two engines.';
comment on column trade_order.qty is 'Contracts REQUESTED. What filled is the sum of fill.qty. The first two engines always fill in full.';
-- Which coins the third engine looks at. Nothing trades until a version is approved and AC_V3 is on.
update instrument set spec = spec || '{"v3": true}'::jsonb
 where symbol in ('KXBTC15M','KXETH15M','KXSOL15M','KXXRP15M','KXDOGE15M');
```

Adding a nullable column is a catalogue change; the partial index builds over a few thousand rows. Grants are
table-level for insert **[READ grants.sql]**, so none changes.

### 7.4 Migration `0013_kalshi15m_v3_versions.sql` (step S7; generated by `measure3 -emit-migration`; only if R1 and R2 pass)

```sql
-- Generated from research/v3/frozen-params.json (sha <..>) under docs/v3-measurement-protocol.md (sha <..>).
do $$ begin if '<LAMBDA>' like '<%' then raise exception 'placeholders were never filled in'; end if; end $$;
with v3 (name, params, hypothesis) as (values
    ('Scalper', '<generated, provenance inside>'::jsonb, '<7.1>'),
    ('Value',   '<generated>'::jsonb,                    '<7.1>'))
insert into strategy_version (strategy_id, version, params, code_ref, hypothesis, parent_version_id, created_by, status)
select s.id, 3, v3.params, 'service/internal/kalshi15m3@<sha>, fills by broker paper-1', v3.hypothesis,
       (select p.id from strategy_version p where p.strategy_id = s.id and p.version = 2),
       (select id from actor where handle = 'claude'), 'draft'
  from v3 join strategy s on s.family = 'kalshi15m' and s.name = v3.name
on conflict (strategy_id, version) do update
   set params = excluded.params, hypothesis = excluded.hypothesis, code_ref = excluded.code_ref, status = 'draft'
 where strategy_version.hypothesis like 'DEV PLUMBING%';   -- only ever true on the dev database (S5)
```

The strategy rows are reused (`unique (strategy_id, version)` allows 3), so `AnalysisBuckets` picks v3 up as engine
"v3" unchanged. **Approval**: `tools/approve-version.sh <dev|prod> <version id>` shows the row and, on a typed yes,
runs `update strategy_version set status = 'probation'` as the owner, then prints "takes effect at the next restart of
the service": Runner3 reads statuses once, at start (5.4), so that no bucket is ever seeded or dropped under v2's
cached capital. Agents propose (the `draft` row, created by
`claude`); a person approves (the status change). A draft already counts as a trial, which is why 0013 is written
only after the protocol has passed.

Buckets: `kalshi15m3 Scalper v3`, `kalshi15m3 Value v3`, made by `EnsureSimSetup`, called only from `NewRunner3`
(5.4), under the existing rules: $1,000
each deposited from `owners (sim)` to the common pool and seeded from there, a `seeded` event each. Contributed and
external rise together, so seeding earns nothing.

### 7.5 Home page and analysis

Composition (`books.go`, `store/home.go`, `web/home.go`):

- `runner.Groups = {"strategies","anti","v1","v3","money"}`; `groupOf`: 1 -> `v1`, 3 -> `v3`, else anti/strategies.
  Its own group because "strategies" against "anti" is a PAIRED comparison of the same six names, and two unpaired
  buckets inside it would break the pairing and make the group's history incomparable with its past (D5).
- `BucketBook` gains `Version int`; `Value()` stops guessing the version from the engine label. The money group's
  contribution becomes "external less every other group" in a loop, so a new group cannot be forgotten.
- **Zero-baseline rule** in `web/home.go`: every batch writes every group of the release that wrote it, so a group
  absent from the `then` batch did not exist then: baseline value 0, contributed 0, `earned = value - contributed`
  now. The groups still add up to the total. Today that case yields `null` **[READ home.go:327-333]**.
- `Runner3.Book()` returns the existing `Book`/`BucketBook`/`Position` types: engine "v3", world "real",
  `EntryPrice` = average premium per contract, marked at the bid like everything else.
- `value_snapshot.key` is free text: no migration. `docs/api-home.md`: engine may be `v3`; five composition lines.

Analysis:

- **S1, before anything else**: `AnalysisWindow` q.1 takes contracts from fills and accepts partial orders
  (`join lateral (select sum(qty)::float8 qty from fill where order_id = o.id) f on f.qty > 0 ... where o.status in
  ('filled','partial')`); same in `AnalysisUnsettled`; `store.Buckets` counts buys with a fill. Every v1/v2 order has
  exactly one full fill, so their output is unchanged. `HeldAtClose`'s comment "a sale always closes a whole lot"
  and its tests are updated: it is bought minus sold per (bucket, side), which holds for average-cost positions.
- `analysis.Trade.DepthPriced` (`detail.v = 3`): `PriceSales` passes such a sale through with `Beyond = 0`; it was
  capped across five levels when made, and re-capping at level one would report honest fills as overselling.
- `fill_checks` (must all be zero): a v3 fill above `seen` displayed less held; an order whose decision has
  `blocked_by`; `status` disagreeing with `sum(fill.qty)` against `qty`.
- `execution` section for v3 (ships BEFORE v3 trades on prod, S6): requested against filled by action; share of
  exits that filled nothing; `binding` counts; `BlockedSeconds`; **next-second re-pricing** of each fill (filled there
  only if that price still shows at least the size taken), window-clustered, for buys and for sells separately:
  these two figures are what replace M2 and M2s in any later version (6.3); contracts filled at a level within 5 s
  **[CONVENTION, the look-back `PriceSales` already uses]** of a displacement onto it, so that the hold rule's effect
  can be seen; the **per-fill decomposition** (fee, entry minus mid, outcome minus mid, outcome minus `p`), the last
  two judged as 7.1 says; and the **drift gate's record** from `evaluation.model`: per coin, the distribution of
  `|p_model v3 - p_model v2 reference|`, the seconds `drift` was true, and the entries it blocked (4.3). The gate acts
  on the difference; this section only reports it.
- The model-versus-market scorecard stays v2's six versions. One added line, stated in advance: "blend at the frozen
  lambda versus the mid" on windows after the version's `created_at`, judged at `CorrectedT(trials + 5)` with
  `MinWindows`, like the per-fill terms (7.1).

---

## 8. The replayer: next step, not this one

Neither M1 nor M2 needs it (they are queries). Depth has been recorded only since 87a2100; live evidence is what
the leaderboard judges; and a replayer is a tuning machine whose rules should exist before it does. This step makes
it cheap: `Paper` and `Engine` are pure, take the clock, and take the very `kalshi.Quotes` JSON `evaluation.quotes`
stores; `p_model` is journaled per second from S5 on, so no model reconstruction will be needed.

This step ships only `service/cmd/brokercheck` (read-only), with two modes that answer two different questions.

- **`-mode parity`: is the walk right?** `analysis.PriceSales` starts a fresh displayed size for every (bucket,
  second, side), has no limit test, and reads the book from the latest evaluation within 5 s before the sale
  **[READ analysis.go:260-278, store/analysis.go:182-185]**. Parity mode is built to be that rule and nothing more: a
  NEW `Paper{Levels: 1}` per (bucket, second), the same 5 s book lookup handed to `ObserveBook` under a synthetic
  evaluation id, the limit at its minimum (0.0001, so it never binds), sales in order id order with `Commit`
  between them. Its filled and beyond-the-bid contract counts must equal `PriceSales`' EXACTLY, per strategy. Two
  implementations of one rule agreeing on production data is the acceptance test of the walk, the flooring and
  the within-second sharing. A sale of less than a cent's worth (2.3's end-of-walk case) is listed apart; none is
  expected.
- **`-mode real`: what would v2's sales have filled?** One `Paper` per bucket for the whole history, holds and
  displacement carried across seconds, five levels, each sale's true limit. REPORTED only. It is expected to fill
  LESS than parity (v2's Scalper sells lots in consecutive seconds against an unchanged book, and the second lot
  finds the first one's hold), and it is never required to match anything. No release waits on its figures, so
  nothing pushes anyone to loosen the hold rule until a gate passes.

The replayer must never be used for:
1. Choosing a parameter by comparing replays, unless EVERY value tried is first a `strategy_version` row (`draft`
   counts as a trial). The tool requires `-trial <version id>` and refuses without a registered id.
2. Reporting replay P&L as evidence of edge. It may appear only beside the live figure, labelled replay.
3. Replaying over the span a parameter was measured on and calling it a test.
4. Writing to the database (`default_transaction_read_only = on`; its store has no insert).
5. Filling what the recording does not show: hours before 87a2100 are skipped, not filled at the touch.

**Note of 2026-09-23: the replayer exists, as the MCP tool `strategy_exercise` (`service/internal/exercise`,
`docs/mcp-read-surface.md`).** It is the live path on the tape (`engine.Decide`, `broker.Paper`, `Apply`,
`ApplySettlement`), reading `evaluation.quotes` and the journaled `model.v3`, so no model is reconstructed. Against
the five rules: (2) its answer is labelled a replay and says in words it is not a measurement; (3) the 15-minute
windows past TRAIN's 480th eligible window are refused until `research/v3/test-result.json` exists (the owner sets
`AC_EXERCISE_PAST_TRAIN` in the MCP process's environment then); (4) the MCP process reads as `assetcracker_ro` and
writes nothing; (5) snapshots without depth decide nothing and are counted. **Rule 1 it departs from, by the
owner's decision of 2026-09-23:** a shape may be exercised without a registered row, and instead EVERY run is
recorded as an append-only `analysis_result` (key `strategy.exercise`: the shape, the window and the summary),
written by the running service through `POST /api/controls/exercise/record` behind the operator key. The count of
shapes tried is therefore on the record beside the count registered, but it does not move the leaderboard's
`trials`; whether it should (a Bonferroni over registered versions plus distinct exercised shapes) is a policy
question left open here. Checked against the ledger on 2026-09-23: over their own windows Value (conventions) and
Mid-round Favourite (conventions) replayed to the same bet counts within one (240/241, 76/75), the same windows
(121, 55) and the same entry seconds almost everywhere; stakes drifted by a contract or two and P&L by about $25 on
$1,400 staked, because the live runner drops a coin's second under lock contention (`Runner3.Step`, `TryLock`), the
sustainment allocation lowers a live bucket's cash, and settlement lands a few seconds later live.

**Note of 2026-09-23: a roster is the same sim.** `strategy_exercise` takes `members` and answers
`by_member` and `picks`. The first three members were exercised alone on
`2026-09-23T01:30:00Z`–`2026-09-24T01:30:00Z` (TRAIN, 96 windows): Favourite record **614**
(+$41.04, r/$ 0.0232, t 0.35), Value no-longshot **613** (+$30.92, r/$ 0.0299, t 0.62), Late
model **615** (+$176.86, r/$ 0.1376, t 0.97). Registering the composition as a draft waits on the
ablation (structural-only vs both vs best member) after this release; Late model is the member to
beat.

---

## 9. File-by-file change list

| File | Change | Step |
|---|---|---|
| `docs/v3-measurement-protocol.md` | new: section 6.2-6.3 verbatim, the block-doubling rule, R2's t quantile, the five counted comparisons of 7.1, the proxy note on M2/M2s | S0 |
| `service/internal/store/analysis.go`, `home.go` (`Buckets`) | contracts from fills; `status in ('filled','partial')` | S1 |
| `service/internal/analysis/analysis.go`, `_test.go` | `HeldAtClose` comment and partial/cancelled tests; `Trade.DepthPriced`; `PriceSales` pass-through | S1 |
| `service/internal/broker/*` | new package (section 2) | S2 |
| `service/cmd/brokercheck/main.go` | new, read-only; `-mode parity` (the gate) and `-mode real` (a report) | S2 |
| `tools/guard-frozen.sh`; `.github/workflows/tests.yml` | fail if `kalshi15m2`, `cmd/replay*`, `tools/parity` differ from main without an explicit override; Go job: `go vet`, `go test ./...`, the replay2 parity gate (D12) | S2 |
| `db/migrations/0012_trade_order_client_id.sql` | section 7.3 | S3 |
| `service/internal/store/sim3.go` | new (section 3): `RecordOrders`, `OrdersRecorded`, `SettlementsRecorded`, `TradableVersions`, `HeldBuckets`, `BucketFills`, `BucketCash`, `MarketResults`, `definitelyRolledBack` | S3 |
| `service/internal/kalshi15m3/*` | new package (section 4) | S4 |
| `service/internal/runner/runner3.go` | new (section 5): two locks, `Run`, `Inputs`, `Step`, `Settled`, `Observe`, `Heal`, `Book`, `rebuild`; `NewRunner3` always obtains the `SimSetup`; nothing settles while suspended; exhaustion derived after the rebuild | S5 |
| `service/internal/runner/books.go`, `books_test.go` | five groups, `BucketBook.Version`, money remainder loop, `Runner3.Book` | S5 |
| `service/internal/web/home.go`, `docs/api-home.md` | label "Third engine"; zero-baseline rule | S5 |
| `service/internal/config/config.go` | `V3` from `AC_V3`, default off; `AC_V3_FAIL_EVERY` and `AC_V3_DELAY_COMMIT` honoured only in a binary built with tag `faultinject` | S5 |
| `service/cmd/assetcracker/main.go` | `safely`, build order (`NewRunner3` before `NewRunner2`, also with `AC_V3` off), sink wiring, `model["v3"]` from `Inputs` before the insert, `go run3.Run`, `Heal` before valuation, `/api/status` v3 block (state, `skipped_busy`, held and tradable buckets, "waiting for restart") | S5 |
| `tools/dev-v3-plumbing.sql` | dev-only version rows; first statement raises unless `current_database() = 'assetcracker_dev'` | S5 |
| `service/internal/analysis/report.go`, `store/analysis.go`, `web/analysis.go` | `fill_checks`, `execution` (re-pricing of buys and sells, displacement, drift record), blend line and per-fill terms at `CorrectedT(trials + 5)`, "v1 or v2" wording | S6 |
| `service/cmd/measure3/main.go`, `research/v3/*.json` | new tool; committed results | S7 |
| `db/migrations/0013_kalshi15m_v3_versions.sql`, `tools/approve-version.sh` | generated; approval, which says it takes effect at the next restart | S7 |
| `docs/platform-brief.md` | build order, the third engine, the two states | each step |

NOT touched: `service/internal/kalshi15m2`, `service/cmd/replay`, `replay2`, `tools/parity`,
`service/internal/runner/runner2.go`, `runner.go`, `store/sim.go`, migrations 0001-0011.

---

## 10. Tests

`broker` (pure, table-driven):
1. Examples A, B, D, E, F of 2.4 to the cent, each fill's fee share, B's five transfers, F's missing bucket entry.
2. Example C: same book twice -> `cancelled`; display rises -> only the excess; falls then rises -> only the rise.
3. Displacement (C2): hold 43 at the best bid, display falls to 10 -> 33 move to the next level and only 67 of its 100 are available; a displaced hold falls and moves again by the same rule; what does not fit on the shown levels is dropped.
4. Hold refresh: absent inside the visible range releases; absent below five shown levels does not; fewer than five shown means absent is empty.
5. Fractional sizes floor; a level of 0.62 fills nothing. Never a sixth level; never the 1c/3c/5c aggregates. `Levels: 1` never touches level two.
6. Limits on both actions; the walk stops at the first failing price.
7. `MaxCostCents` never exceeded, fee included, for qty 1..500. `CostSteps`: 4.5's thin-top book fills 20 + 13 for 435c and stops; the cumulative cost at every fill is within the ceiling of its price; a ceiling already passed ends the walk.
8. Fee: integer form equals `round(k2.KalshiFee(n,p)*100)` for n 1..2000, p 1..99c on one level; shares always sum to the order fee; `FeePerFill` is never cheaper.
9. Premium rounding at 0.0990 and 0.9980: buy up, sell down. A level worth less than its fee is TAKEN (one contract at 0.0091 costs the bucket a cent); a fill of 0c premium and 0c fee ends the walk. Property: no order ever fills at a level while a better level of the same ladder still had contracts available to it.
10. Rejects: stale evaluation id, closed market, qty 0; a repeated client id returns the first report.
11. `Void` restores availability exactly; `Commit` makes it permanent; two orders in one step cannot take the same contracts.
12. Buckets do not share holds; buy-yes and sell-no at the complementary price DO share one key.
13. Property: between two observations one bucket never takes more than `floor(displayed)` at a level; on `ObserveBook` the total held per (bucket, ladder) never rises, and a level's hold rises only by what a better level released.

`kalshi15m3`:
14. `Decide` moves no money and reads no `CoinState`: deep-compare accounts before and after across a recorded round.
15. `Position.Apply`: partial sells release basis summing exactly to the cost; the window's `OpenCents` and `LostCents` follow.
16. Partial entry counts one bet; cancelled entry counts none and starts no gap.
17. Partial exit keeps `Exiting`; next step re-evaluates; rule stops firing -> cleared; empty bid side -> order still sent, `ExitTries`/`BlockedSeconds` grow, position rides and settles.
18. No entry in a step that sent an exit. An account with `MayOrder` false sends nothing, entries or exits.
19. Sizing: 4.5's example to the cent, its `CostSteps` `[739, 440, 135]`, and the thin-top fill; coin order within a window does not change the window's total; `Used_w` never passes `kappa * k_max * E_w` nor the cap.
20. Losing round trips: a sale below basis frees only the proceeds' worth of basis; repeated losing trips in one window stop at the budget, across coins; a winning sale frees its basis and no more.
21. Exits: `value` and `capture` both charge `stale_cost_sell`; with it set high enough neither fires on a book where they fired without it.
22. Blend: lambda 0 -> the mid and no entries ever; lambda 1 and stale 0 -> v2's Model probability.
23. `CoinState` fork: on the `tools/parity` recordings, sigma2, offset and `p_model` equal v2's at every step (test-only import of `kalshi15m2`).
24. Drift gate: v2's inputs absent, `offset_source` different, or the difference above `drift_tol` -> no entry, `blocked_by "model drift"`, exits still sent.
25. `Params.Validate` refuses a numeric field without provenance; 0013's params JSON equals the Go definitions.
26. Settlement: `contracts * 100` on the winning side; a loser's row with 0; a position sold out earlier yields NO row.

`runner` (fake store, real `Paper`):
27. `Apply` happens only after `RecordOrders` returns nil.
28. The server refuses a write (`*pgconn.PgError`) -> `Void`, engine unchanged, still running; third in a row -> **paused**, `Halted` stays ""; a successful probe resumes.
29. A write ends in a context deadline -> `Void`, **suspended**, no `OrdersRecorded` call from `Step`, no rebuild before `healAfter`. The fake store then lets the commit LAND after it would have answered "none": the rebuild finds the orders, cash, position and holds include them, and no second order was sent for the same intent.
30. The same with the commit never landing: the rebuild finds nothing, v3 resumes, memory equals the ledger.
31. Cash check: the fake ledger is moved under a running v3 -> suspended within one sweep, rebuilt, equal again.
32. Settlement write fails but rows exist (including the unique-violation retry) -> applied; none exist -> the next sweep settles it; the query fails -> suspended. `Settled`, the sweep and the once-a-minute cash check, each called while v3 is SUSPENDED, return at once: no `RecordSettlements`, no `OpenQty`, no `SettlementsRecorded` call reaches the fake store, and `gen` does not move.
32a. Settlement never runs on stale memory. v3 holds 100 YES; a write adding 50 times out (`Void`, suspended); the fake store lands the commit late; the market's result arrives (a) before `healAfter`, and separately (b) while every rebuild read is made to fail. In both: NO settlement row is written while suspended, by `Settled`, by `Run`'s sweep or by `Heal`. `Heal` with a failing rebuild does not sweep. Once the rebuild succeeds the sweep that follows it settles the FULL 150, and `analysis.Reconcile` on the resulting rows finds held == settled. (c) The commit lands AFTER a rebuild has already succeeded without it and the result arrives before the minute's cash check: the last look (`OpenQty` 150 against memory's 100) suspends and writes nothing; after the next rebuild the sweep settles 150.
33. Rebuild equals live: run a recorded round, rebuild a second runner from the fake store's rows, deep-compare cash, positions, windows (`OpenCents`, `LostCents`), and holds at least as large.
34. Net rule: a position fully sold more than a day ago, with no settlement row, does not suspend, is not a position after rebuild, and blocks no snapshot. A NET-open lot older than a day whose market has a result is settled by the sweep at start, not suspended over.
35. The sweep settles a position whose `Settled` call was never delivered, and one whose `Settled` was skipped because `TryLock` failed.
36. Settle-only: `AC_V3` off (and, separately, the version set to `retired`) with a bet open -> the bucket is held, no order is sent, the sweep settles it, `Book()` values it, and `analysis.Reconcile` on the resulting rows keeps the window. The open bet WINS (payout > 0; a second case loses), and the fake store REFUSES a `RecordSettlements` or `CloseBucket` whose `SimSetup` has a zero `ActorID`, `VenueLedgerID` or `PoolLedgerID`, as the real foreign keys would: the settlement carries the service actor and debits the paper venue. The fake cannot prove the real constraint, so the same case (a winner, `AC_V3` off) is an S5 check on the dev database.
37. Fixed bucket set: `NewRunner3` calls `EnsureSimSetup` exactly once in EVERY mode: with `AC_V3` off, or with no tradable version, the name list is EMPTY and the call CREATES NO BUCKET (no `bucket` row, no deposit, no seed, no `seeded` event in the fake store), and the runner holds a setup with non-zero actor, venue, fees and pool ids. After a status change in the fake store, `rebuild()` makes no `TradableVersions` and no `EnsureSimSetup` call and the bucket ids are unchanged; only `NewRunner3` calls them. `/api/status` reports "approved, waiting for restart".
38. Locks: with the fake store blocking inside `RecordOrders` (and, separately, inside a rebuild read), `Inputs` and `Observe` return at once, `Step` for another coin returns with `skipped_busy`, and `Settled` returns and leaves the position to the sweep. Run under `-race`.
39. Rebuild's swap: the generation moves between the rebuild's reads and its swap (a second rebuild, `Run`'s and `Heal`'s at once, or a recovered panic; a settlement no longer can, because `Settled` and the sweep return while suspended) -> the rebuilt state is discarded and the next attempt succeeds. A `Settled` call that arrives during the rebuild's reads returns, writes nothing, and the sweep after the swap settles the position.
40. A panic in `Inputs`, `Step`, `Settled` or `Observe` is recovered, v3 suspended, `Inputs` returns nil, nothing reaches the caller.
41. No tradable version and no v3 bucket -> `EnsureSimSetup` is called with NO names and creates no bucket; no orders; `p_model` still produced. A `draft` version is not traded. `HeldBuckets` failing, or `EnsureSimSetup` failing -> `NewRunner3` returns the error.
42. Exhausted account stops ordering and moves no capital; the next `NewRunner3` closes it without restake, using the setup it obtained with `AC_V3` off as well as on. ORDER: a bucket with ledger cash under 100c and a bet still OPEN is NOT closed at start (no `CloseBucket` call; it stays in the held set and in `heldElsewhere`; its lot is swept at the close). One whose old lot the start-up sweep settles as a WINNER, lifting its cash over 100c, is not closed; one whose lot settles as a loser is closed at that same start, after the sweep. With the first rebuild failing, no bucket is closed and nothing is swept. `Exhausted` is set again by every later rebuild and never before its fills are folded.
43. `books_test.go`: five groups add up; money is the remainder; a v3 bucket lands in `v3`, tradable or settle-only. `home_test.go`: zero-baseline rule; group earned figures sum to the total's across a range that starts before v3.

`store`/`analysis`/tools:
43a. `EnsureSimSetup(ctx, "kalshi15m3", "kalshi15m", 3, nil, 100000)` against a real dev database, not the fake (in the manner of the ledger-rules test, which already runs on one), AFTER v2's own `EnsureSimSetup` call has made the shared accounts: the counts of `bucket`, `ledger_transfer`, `ledger_entry`, `bucket_event` and `ledger_account` rows are the same before and after, and the returned actor, venue, fees and pool ids equal those v2's own call returns. This is the measurement behind "an empty list creates nothing"; until it has run, that statement is a reading of the code.
44. `HeldAtClose`/`Reconcile` with partial and cancelled orders; `PriceSales` pass-through; `fill_checks`; the added verdict lines use `CorrectedT(trials + 5)` and `MinWindows`.
45. Dev database only: `RecordOrders` with a two-fill order writes two balanced transfers; a fill with no bucket entry balances and is read back as 0 by the rebuild query; a cancelled order writes none; a duplicate `client_order_id` rolls the whole step back; the ledger sums to zero. `definitelyRolledBack` is true for a constraint violation and false for a cancelled context.
46. `measure3`: the Brier identity on synthetic rows; jackknife against a hand-worked case; R2 uses `t_{0.95,B-1}` for the realised `B` (1.895 at 8 blocks, 2.353 at 4); the autocorrelation report and the block-doubling rule on a synthetic autocorrelated series; M2s on hand-made quotes, a vanished bid counting as 0; refuses TEST without frozen params, a second TEST read, or a protocol sha mismatch.
47. `brokercheck`: on synthetic trades parity mode equals `PriceSales` exactly, including two sales in one second; real mode on the same trades never fills more than parity.

---

## 11. Delivery: small steps, each releasable alone

Every step: `go vet` and `go test ./...` on the Mac (no database), the dev binary on the Pi against
`assetcracker_dev` (`deploy/pi/deploy.sh`), then `deploy.sh prod` (health-checks, rolls back). **Standing checks
after EVERY step, dev then prod**: v1 and v2 `halted` empty in `/api/status`; both still placing orders;
`select sum(amount_cents) from ledger_entry where mode = 'sim'` = 0; a value snapshot every minute, none missing
that v2 alone would have written; evaluation rows per market per minute unchanged (the measurable proxy for
"pollers not slowed"); replay2 parity passes; `/healthz` ok. **Rollback criterion**: `deploy.sh rollback` must land
on a binary that can read every row already written, which is why readers ship before writers.

**S0. Know the ground; commit the protocol.** Read `schema_migration` and the running version on prod and dev.
Expected on prod, as reported (1, row 1): release `78f5452`, 0010 and 0011 applied, the home page live. The
reading, not the expectation, is what S1 rests on: if it differs, stop and say so. `2f97595` (the stopped-snapshots
notice) is not released; whether it goes out alone first or rides with S1 is the owner's word at the time, and
either way S1's "identical but for timestamps" comparison is made against the binary actually running. Commit
`docs/v3-measurement-protocol.md` (6.2-6.3, the comparison count of 7.1, the block rule); record `T_c`. No code.

**S1. Analysis reads fills.** Test 44 (its first three parts). Dev: save `/api/analysis` before, deploy, compare: identical but for
timestamps; `explain` shows the lateral read uses `fill (order_id)`. Release.

**S2. `broker`, `brokercheck`, the frozen-paths guard and CI job.** Tests 1-13 and 47. Nothing in the service imports the
package. Dev, then prod read-only: `brokercheck -mode parity` counts equal the analysis layer's
`contracts_beyond_bid` per strategy, exactly: that is the gate. `brokercheck -mode real` is run and its report
kept; it is expected to fill less and gates nothing (section 8). Release (service behaviour unchanged).

**S3. Migration 0012 and `sim3.go`.** Test 45 on dev. Dev: v1/v2 orders keep inserting; `\d trade_order` shows the
index. Apply to prod; release.

**S4. `kalshi15m3`.** Tests 14-26 with a placeholder lambda in TEST fixtures only. Not imported by `main`. Merge.

**S5. `runner3.go`, groups, `main.go` wiring, `AC_V3` default off.** Tests 27-43 (32a included) and 43a. Dev: run
`tools/dev-v3-plumbing.sql` (strategies "Scalper (dev plumbing)" at `probation` and "Value (dev plumbing)" at `draft`,
hypothesis "DEV PLUMBING, not a trial", lambda and stale_cost labelled test values; never in a migration, never on prod), `AC_V3=on`, at least four settlements
**[CONVENTION: enough to see each path once]**, then:
  - `fill_checks` zero; every v3 fill within the displayed size at its level in the evaluation row of THAT second
    (`e.market_id = o.market_id and e.at = o.placed_at`, exact join; a level not displayed at all must also fail:
    compare with `is distinct from true`); summed per (bucket, market, ladder, price) between falls of the display,
    fills never exceed the display (the hold rule, checked from rows).
  - `status` against fills: `filled` iff sum = qty, `partial` iff 0 < sum < qty, `cancelled` iff none.
  - Window rule from rows: per (bucket, `closes_at`), in fill order, open basis (buys' cost less sells'
    `detail.cost`) plus realised losses (`max(0, detail.cost - detail.payout)` per sale) never exceeds the
    `detail.window` budget nor the cap; and for every buy, the cumulative cost at each fill is within the
    `detail.cost_steps` ceiling of that fill's price.
  - `kill -9` mid-round, restart: the `/api/status` v3 block (cash, positions, window used, holds) equals before, and equals 5.4's SQL.
  - Fault injection (dev binary built with `faultinject`, `AC_V3_FAIL_EVERY=n`): a refused write voids and, three in a row, pauses and resumes;
    an unknown outcome suspends and rebuilds; settlement retry; ledger still sums to zero; v1/v2 unaffected.
  - Stop Postgres on dev for ten seconds mid-round, once with `AC_V3` off (the baseline) and once with it on: v3
    pauses or suspends and recovers by itself; snapshots resume in the same minute v2's would. Measured in both
    runs and compared: v1's and v2's step latency (time from `InsertEvaluation` returning to v2's `Step`
    returning), the count of "tick buffer full" warnings, evaluation rows per market per minute, and v3's
    `skipped_busy`. v1/v2 figures with v3 on must not be worse than the baseline run's; if they are, v3 does not
    go to prod until they are not.
  - A delayed commit (dev binary, `faultinject`: the store sleeps past the 2 s budget and then commits): v3
    suspends, waits out `healAfter`, rebuilds WITH the orders; no duplicate order for the same intent; the
    ledger equals memory.
  - Switch `AC_V3` off and restart mid-round with a bet open: the bucket is held settle-only, the bet settles at
    the close, `/api/analysis` still lists that window for v1 and v2, and the total shows no step. The same with
    the plumbing version set to `retired` instead. **Repeated until at least one WINNING and one losing bet have
    each settled with `AC_V3` off**: a loser writes no ledger transfer and would pass even with no `SimSetup`; only
    a winner proves the settlement carries the service actor and debits the paper venue (read the
    `ledger_transfer.created_by` and the venue entry of that settlement). Before and after that restart, the row
    counts of `bucket`, `bucket_event` and `ledger_account` are equal: the empty-list `EnsureSimSetup` created nothing.
  - Settlement while suspended (dev binary, `faultinject`): with a bet open, delay a commit past the 2 s budget
    inside the round's last seconds so that the result arrives while v3 is suspended: no `settlement` row for
    that (market, bucket, side) appears until `/api/status` shows v3 running again; then one row appears whose
    `qty` equals the net of the fills; `/api/analysis` keeps the window. Record how many value snapshots were
    missed across the suspension: the claim "at most the one the suspension would cost anyway" (5.3) is
    **[INFERRED]** until this has been seen, and if rebuild plus sweep do not fit `Heal`'s 3 s the number says so.
  - Exhaustion order: on dev, bring a plumbing bucket's cash under 100c with a bet open (a stake sized to do it),
    restart: the bucket is still `active`, still held, and its bet settles at the close. Restart again after it
    lost: now it is frozen and reaped, and the total shows no step.
  - Approve a second plumbing version while the service runs, then force a suspend: no bucket is created, no step
    in the total, `/api/status` says "approved, waiting for restart"; after a restart the bucket exists and v2's
    capital read already contains its seed.
  - A position sold out completely, then the service left running past 24 h **[CONVENTION: the age the old guard
    tripped at]** and restarted: v3 rebuilds and runs.
  - **Measure `drift_tol`** (4.3): the 99th percentile of `|p_model v3 - p_model v2 reference|` from
    `evaluation.model`, after both engines hold 24 offset samples; commit it with the run's bounds. This is the
    measurement the gate's number comes from; until then the gate's value is a labelled test value.
  - Poll-pass p99 and evaluation cadence before and with v3.
  - The five groups add up; ranges starting before v3 show figures, not null.
Prod release with `AC_V3` OFF: the only visible change is a fifth composition line with zero buckets. Then, if the
owner agrees (D4), `AC_V3=on` on prod in observe-only mode: no buckets, no orders, `p_model`, the v2 reference and
`drift` journaled per second.

**S6. `fill_checks`, `execution`, the blend line in `/api/analysis`.** Read-only; checked on dev against the
plumbing rows. Release. (Before v3 trades on prod, so the next-second comparison exists from its first fill.)

**S7. Measure, register, approve, switch on.** When TRAIN is complete: `measure3 train` on prod as
`assetcracker_ro`; commit `train-result.json` and `frozen-params.json`. When TEST is complete: `measure3 test`,
once; commit. R1 and R2 pass -> `measure3 -emit-migration` writes 0013; on dev, first set the plumbing versions to `retired`
(they are separate strategies, "Scalper (dev plumbing)" and "Value (dev plumbing)", so 0013 never touches their rows
or buckets; dev history before that is never evidence), then apply 0013; 24 hours **[CONVENTION]** on dev with S5's checks;
apply to prod; Brad runs `approve-version.sh prod`; restart with `AC_V3=on` (the restart is what makes the
approval take effect, and what seeds the two buckets BEFORE v2 reads its capital). Fail -> 6.5, written up on the evidence page.

**Next step, not this one**: the replayer; sustainment for v3; measured-correlation sizing (new version);
`paper-2` if the execution section calls for it; a measured stale-price gate for DOGE.

---

## 12. Decisions that are the owner's

Each: the options, my default and why, and what can proceed without the answer.

- **D1. Approve the measurement protocol (6.2-6.3) before anything is run**, including that the outcome may be "no
  version registered", that R2 is judged against `t_{0.95,B-1}` (1.895 at eight blocks), that the block doubles to
  12 hours if TRAIN's lag-24 autocorrelation exceeds its SE (R2 then needs 2.353 on four blocks: a real loss of
  power, accepted in advance), that M2 and M2s are proxies taken from v2's orders, and that the five added
  evidence-page comparisons are judged at `CorrectedT(trials + 5)` (7.1). Options: as written; amend counts or the
  bound; reject. Default: as written. *Blocks only
  S7; S0's commit needs your yes to the text, S1-S6 proceed regardless.*
- **D2. lambda frozen at the lower 95% bound, or at the clipped point estimate.** Default: the lower bound (both
  judges preferred it; overbetting on an over-estimated weight is the costlier error). The point estimate trades
  more and sooner. *Blocks S7 only; must be fixed before `measure3` first runs.*
- **D3. If the protocol says no: register nothing (default), or register ONE "Scalper v3 (fills only)" at the
  parent's unmeasured belief 0.5, hypothesis about the fill artefact only.** Default: nothing; your first rule, and
  `brokercheck` already answers the artefact question on recorded history without spending a trial. The option buys
  live evidence of partial-fill behaviour for one trial (|t| 3.007). *Nothing waits on this until S7.*
- **D4. Observe-only v3 on prod from S5** (journals `p_model` per second, no buckets, no orders). Default: yes.
  *S5's release with `AC_V3` off proceeds either way.*
- **D5. A `v3` composition group of its own (default), or v3 inside "strategies" for now.** Own group keeps the
  six-against-six pairing and needs the zero-baseline rule (tested). Inside "strategies" needs no home-page change.
  *S1-S4 proceed; S5 needs the answer.*
- **D6. Paper holds per bucket (default; each bucket its own world, as the analysis layer assumes) or shared across
  v3 buckets** (closer to one real account, but results then depend on each other). *Blocks S2's hold key only.*
- **D7. An exhausted v3 bucket is frozen at the next start and NOT staked again** (default; the brief's lifecycle,
  and no capital moves under v2's cache). "Exhausted" is cash under 100c AND nothing open, decided after that
  start's rebuild and sweep (5.4): a bucket with a bet still open is never frozen. Restaking as v2 does needs a change to `runner2.go`. *Blocks S5.*
- **D8. No sustainment allocation from v3 in this step** (default; every rate is zero today). If rates are set
  first, v3 must learn to skim before its results compare with v2's. *Nothing waits.*
- **D9. Fee rounding on a multi-level sweep: once per order (default, as the fee is described) or per fill**
  (`fee_per_fill`, up to four cents dearer, never flattering). If you know Kalshi's rule, it replaces the
  convention. *Blocks S7's frozen params only.*
- **D10. Conventions that are risk preferences, not measurable**: quarter Kelly (applied at each price level of a
  walking buy), window cap 25% of min(window-start cash, seed), $1,000 seed, correlation taken as 1, and a window's
  realised losses counting against its budget until it closes. Default: as listed. *Blocks S7.*
- **D11. Where `p_model` comes from: a forked `CoinState` held to v2 by test 23 and the live drift gate of 4.3 (default),
  or a read-only `Runner2.ModelFor` accessor** (exactly the number lambda was measured on, but it edits
  `runner2.go` and idles v3 whenever v2 is halted). Under the default the gate reads v2's inputs through the
  existing `Inputs` method, so v3 also sends no ENTRY for a coin v2 does not run, or while the fork differs from
  v2 by more than the tolerance measured on dev in S5 (about one second in a hundred by construction); exits go
  on. *Blocks S4.*
- **D12. Add the Go CI job and frozen-paths guard to the shared GitHub workflow.** Default: yes; today nothing but
  habit enforces v2 parity. It is Sean's repo too. *S2's code proceeds without it.*
- **D13. Who runs `measure3` and `brokercheck` against prod, and as which role.** Default: you, as
  `assetcracker_ro`, with `select` granted on the tables they read (a `grants.sql` line). I changed no roles or
  settings. *Blocks S2's prod check and S7.*
- **D14. `created_by` = `claude` with your `approve-version.sh` as the recorded approval (default), or your own
  actor row.** *Blocks S7.*
- **D15. `paper-1` (fill at the displayed price; a measured staleness cost is charged in the entry test and in both
  exit tests, and reported) or start from the stricter `paper-2`.** Default: `paper-1`. The fill model is part of
  what a version's result means, so choose before S7.
- **D16. What the hold rule does when a display falls: move the difference down the ladder, charging every fall as
  a take (default), or forget it and report the bias.** The recording cannot tell a cancel from a take. The default
  is the pessimistic reading and will under-fill when market makers merely requote; forgetting is optimistic at
  exactly the moments the book is being hit. With the second option 2.5 must list it as a known optimistic bias
  and the `execution` section's displacement figure becomes the only watch on it. *Blocks S2's hold rule.*
- **D17. `AC_V3` off, or a version taken off `probation`, means settle-only: no entries and NO exits; open bets ride
  to the close and are settled (default).** The alternative lets a settle-only bucket still send exits, which is
  kinder to Scalper's open positions but means the "off" switch still places orders. Either way the bucket stays
  held and is settled; abandoning it is not offered, because it would strand the bet and hold v1's and v2's
  windows back from the leaderboard. Settle-only still calls `EnsureSimSetup` at start, with no names, for the
  ledger ids a settlement needs; it creates no bucket and moves no money (5.4). *Blocks S5.*
- **D18. A status change (approval, bench, retired) takes effect at the next restart, never while the service runs
  (default).** The alternative, picking it up live, needs a change to `runner2.go` so that v2's cached capital
  learns of a bucket seeded mid-run; without that it writes a false +$2,000 into an append-only table. *Blocks S5.*
- **D19. If v3's held buckets cannot be read at start, the service does not start (default), as when `NewRunner2`
  fails today.** The alternative starts v1 and v2 without v3 and leaves any open v3 bet unheld until the next
  restart: wrong value figures and held-back windows in exchange for v1/v2 uptime. A database that cannot answer
  that read is unlikely to let v2 start either. *Blocks S5.*
- **D20. Release `2f97595` (the stopped-snapshots notice) on its own before S1, or let it ride with S1.** Default:
  on its own, so that S1's before-and-after comparison of `/api/analysis` changes one thing. Prod is on `78f5452`
  with 0010 and 0011 applied. *Blocks nothing but S1's release.*
- **D21. A suspended v3 blocks every engine's value snapshots until it heals (default), as a halted v2 does
  today.** Because v3 settles nothing while suspended (5.3), a round that closes meanwhile also keeps its window off
  v1's and v2's leaderboard until the heal. Ordinary case: `healAfter` 10 s **[CONVENTION]** plus one rebuild, so
  about one snapshot **[INFERRED; S5 measures]**. If the rebuild keeps failing on its self-check it is NOT bounded:
  snapshots stay stopped until a person looks, and the home page says so. The alternative, leaving a suspended v3
  out of the books so the others carry on, writes a false step into an append-only table the moment v3 drops out
  and another when it returns. *Blocks S5.*

---

## 13. What changed from the winning draft, and why

| Judges' finding | Fix here |
|---|---|
| "0012 part 1 then part 2" cannot apply | Two migrations, 0012 and 0013 (7.3, 7.4) |
| Dev-only version rows collide with the later insert | Guarded script; 0013's `on conflict` fires only on rows marked DEV PLUMBING (7.4) |
| Settlement commit lost -> unique violation on every retry -> snapshots blocked | `SettlementsRecorded`; found means committed (5.3) |
| A brief database stall suspends v3 and gaps everyone's chart | Paused (book valid, snapshots continue) apart from suspended; heal before every snapshot (5.4). Narrowed by the critic's finding 4 below: only a write the server REFUSED may pause; a timed-out write that carried orders suspends, and can cost the one snapshot that falls inside its 10 s wait. Narrowed again by 13.2: nothing settles while suspended, and a rebuild that keeps failing stops snapshots without a time bound, as a halted v2 does (5.3; D21) |
| Thin-edge entries flattered; the comparison shipped after trading | Measured `stale_cost` in the entry test (M2); `execution` ships in S6, before S7 |
| Lambda thin: 90 windows, point estimate, unlimited reruns | Pre-committed protocol, 480/24/192 windows, block jackknife, lower bound, TEST opened once (6.2-6.3) |
| Versions inserted as `probation` | `draft`, `approve-version.sh`, Runner3 trades approved versions only (7.4) |
| Average cost against `HeldAtClose`'s lot comment | Comment and tests updated in S1 (7.5) |
| Decrementing `MaxCost` loop; dust stops the walk | Direct computation. A level worth less than its fee is TAKEN, as a venue would (skipping it, the first fix, was the critic's finding 13); only a fill with no cash effect at all ends the walk (2.3) |
| "Inherited" gates claimed unmeasurable | Said plainly that they are unmeasured and still gate (6.4) |
| v3 changes three things at once | Said in 6.4; the per-fill decomposition separates them (7.5) |
| `CapitalMoved()` hook into `runner2.go` | Removed: v3 moves no capital while the service runs (5.6) |
| Taken from the others | One transfer per fill; recover on `Observe`; read `schema_migration` first; `p_model` every second; cadence SQL; `BucketBook.Version`; `BlockedSeconds`; frozen-paths guard and Go CI; rollback criterion; `FeePerFill`; shared resting key; counterfactual-book wording; window-rule SQL, p99 and fault injection checks; zero-baseline rule |

### 13.1 The completeness critic's fourteen findings

| # | Finding | Fix here |
|---|---|---|
| 1 | The one-day look-back guard suspended v3 for good a day after its first fully sold position (no settlement row exists for one), blocking every engine's snapshots | The rebuild read and every "still held" test are NET, as `RealisedByCoin` is; no look-back, no guard; an old net-open lot goes to the sweep (5.4; test 34) |
| 2 | `rebuild()` re-read the tradable versions and could seed or drop a bucket mid-run under v2's cached capital | Versions and `EnsureSimSetup` only in `NewRunner3`; the bucket set is fixed; status changes wait for a restart (5.4, 7.4; test 37; D18) |
| 3 | `AC_V3` off, a benched version or a failed `NewRunner3` stranded an open bet and held v1's and v2's windows back | Held and may-order are separate; every non-frozen v3 bucket is held and swept whatever the switch says; an unreadable bucket set fails the start (5.3, 5.4; test 36; D17, D19) |
| 4 | A 2 s deadline during `COMMIT` could be followed by a late commit after `OrdersRecorded` said "none" | Only a server-reported error is a refusal; anything else suspends and the rebuild, 10 s later, reads the truth; cash is checked against the ledger every minute and on every rebuild (5.2, 5.3; tests 28-31). Simpler than the suggested fix: on a server-reported error `Step` does not ask `OrdersRecorded` at all, because the rollback is certain |
| 5 | `Inputs` and `Observe` waited on a lock held across database calls, before v1/v2 step and on the one stream goroutine | `coinMu` for the model; `Inputs` inside `safely()`; `Step`/`Settled` `TryLock`; rebuild reads outside `r.mu` and swaps under a generation counter; S5 measures v1/v2 latency and tick-buffer warnings through a Postgres stop (5.1, 5.4, 5.5; tests 38-40) |
| 6 | Releasing a hold on a fall handed v3 the displaced taker's liquidity | The difference moves down the ladder (2.3, example C2; tests 3, 13; D16); the execution section reports fills shortly after a displacement (7.5) |
| 7 | Stake sized at the best ask but spent up to the limit | `CostSteps`: a ceiling per price, quarter-Kelly at THAT price (2.1, 2.3, 4.5; tests 7, 19) |
| 8 | A losing sale handed its budget back | `Used_w` = open basis + realised losses (4.2, 4.5; test 20; S5's row check) |
| 9 | R2 judged a 7-degree-of-freedom t against 1.645; the block length was dressed as a fact | `t_{0.95,B-1}` on the realised blocks; block labelled a convention; autocorrelation reported on TRAIN; doubling rule stated in advance (6.2, 6.3; test 46) |
| 10 | Three verdicts at an uncorrected threshold; an unlabelled 30 | All five added comparisons at `CorrectedT(trials + 5)`; 30 is `analysis.MinWindows`, labelled (7.1, 7.5; test 44) |
| 11 | `stale_cost` measured on another population, and nothing charged on sales | Labelled a proxy in the provenance; M2s added and charged in both exit tests; live re-pricing replaces both in any later version (4.3, 4.4, 6.3; tests 21, 46) |
| 12 | The S2 gate demanded an exact match the real broker cannot give | `brokercheck -mode parity` (the gate) and `-mode real` (a report) (section 8, S2; test 47) |
| 13 | The dust rule filled beyond an untouched level | Never skip a level; take it and book it truthfully; end the walk only at a fill with no cash effect (2.3, example F; test 9) |
| 14 | Numbers gating behaviour without a label; the drift check had no figure and no consequence | Each labelled where used and tabled in 6.4; the drift gate has a measured tolerance and an action (4.3, S5) |

### 13.2 What the checkers of those fixes found

Five independent checkers confirmed the five highest-ranked fixes against the code and found three problems the
fixes themselves had opened.

| # | Finding | Fix here |
|---|---|---|
| A (high) | Fix 3 made the sweep run with `AC_V3` off, but fix 2 had `EnsureSimSetup` called only with `AC_V3` on, and it is the only source of a `SimSetup`. The settle-only sweep would hand `RecordSettlements` a zero setup: every WINNING settlement fails `created_by`'s foreign key, is retried once a minute for ever, and the closed round stops every engine's snapshots. A loser writes no transfer, so the dev check passed whenever the bet lost. An exhausted bucket could not be closed either. | `NewRunner3` ALWAYS calls `EnsureSimSetup`, with an empty name list when nothing is tradable; read in sim.go, that creates no bucket and moves no money (section 1; 5.4 step 1; section 3). Tests 36, 37, 41 assert "creates no bucket", not "is not called"; test 43a measures it on the dev database; S5 requires a winning settlement with `AC_V3` off. |
| B (high) | Fix 4 made "suspended" mean "memory may be behind the ledger", but only `Step` was barred. `Settled` and the sweep could write a settlement from the short memory, and `Heal` swept even after a failed rebuild. Such a row is unique per (market, bucket, side) and can never be corrected; the rebuild then ignores the lot and the cash check cannot see it. | `Settled`, the sweep and the cash check return at once while suspended, as `Runner2.Settled` does when halted; `Heal` and `Run` rebuild first and sweep only if that succeeded; a last look (`OpenQty`) before every settlement write covers a commit that lands after a successful rebuild (5.1, 5.3, 5.5; tests 32, 32a, 39; S5). The cost, said in 5.3 and D21: a suspended v3 with a closed round blocks everyone's snapshots and holds that window back until it heals. |
| C (low) | `NewRunner3` closed "exhausted" buckets before `rebuild()` had established what was open, and `CloseBucket` makes no position test: a bucket with little cash and a bet open could be frozen and its lot stranded. | Order fixed: setup, held set, rebuild, start-up sweep, THEN the exhaustion test (cash under 100c AND nothing open), then the ids to `NewRunner2`. `Exhausted` is the rebuild's last step, every time (5.4, 5.6; test 42; S5; D7). |

Not taken up in this revision, recorded so it is not lost: one checker also noted that the lock rules around a
recovered panic are unspecified (every lock released by `defer`; suspending from `safely()` without waiting on
`r.mu`, for example an atomic flag that `Run` picks up; test 40 to assert that calls after a panic still return).

## 14. Assumptions and what was not verified

1. **[ASSUMED]** Kalshi rounds a multi-level taker fee once per order; sub-cent fills settle in whole cents somehow; no hidden size.
2. **[ASSUMED]** A real IOC order with a limit and a cost ceiling exists at the venue. A ceiling that tightens with the price (`CostSteps`) certainly does not exist as one order: live, it is one IOC order per price step. Nothing in simulation depends on either; to be checked before any live work.
3. **[INFERRED]** `evaluation.at` equals the `at` handed to the engines for the same snapshot (the sink passes one `at` to both), so `placed_at` joins its evaluation exactly.
4. **[INFERRED]** Every value-snapshot batch holds every group of the release that wrote it.
5. **[REPORTED, not observed]** Prod is on `78f5452` with 0010 and 0011 applied: told to me by the owner's session and written in the brief's uncommitted working copy (lines 291-292); I did not query prod, and S0 does. **[NOT VERIFIED]** v2's first-trade time (so when 480 windows complete); that `assetcracker_ro` can read the needed tables; every SQL sketch here. Nothing was run on the Pi.
6. The power of R2 on 192 windows is unknown until TRAIN gives the variance; the counts are conventions fixed in advance, and a failed TEST is reported as a result, not retried on the same data.
7. **[INFERRED]** A `*pgconn.PgError` from `RecordOrders` means the transaction did not commit (Postgres reports a failed `COMMIT` as an error and rolls back). Everything else is treated as unknown. **[ASSUMED]** 10 s is longer than a commit takes to land after the client dropped the connection; if that is ever wrong, the once-a-minute cash check is what catches it.
8. **[ASSUMED]** Both engines' models see the same prints in the same order (one callback, main.go:145-157) and are seeded close enough together that the fork's difference from v2 is small; S5 measures it, and the drift gate acts on it, so nothing rests on this being true.
9. **[NOT VERIFIED]** That a ledger transfer carrying only a venue and a fees entry (a sale whose premium equals its fee) upsets no existing reader; test 45 covers the ones named here.
10. **[READ, NOT RUN]** That `EnsureSimSetup` with an empty name list creates no bucket and moves no money. I read the function (sim.go:45-154); nobody has called it that way. Test 43a and the S5 row counts are the measurement. **[INFERRED]** That it creates nothing AT ALL on prod rests on v2 having already created the seven accounts and the venue account there, which I did not query. **[NOT RUN]** The `OpenQty` query of 5.3.
11. **[INFERRED, not measured]** That a suspension which spans a round's close costs no more value snapshots than the suspension alone would: it depends on rebuild plus sweep fitting inside `Heal`'s 3 s on the Pi. S5 records it.
