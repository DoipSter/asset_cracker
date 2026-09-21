// Package kalshi15m3 is the third engine of the kalshi15m family. It is ours, not a port: there
// is no Python to match, so it has its own tests and no parity gate of its own.
//
// Simulated money only. Nothing here can place an order. The engine forms an intent
// (broker.Order) and hands it back to its caller; what fills is decided by the paper broker
// (service/internal/broker) from the book the service recorded for that second.
//
// What is different from the second engine (package kalshi15m2, which stays exactly as it is so
// that its results remain comparable):
//
//   - Money is whole cents in int64 and prices are the broker's ten-thousandths of a dollar. No
//     float touches a balance, a cost basis or a payout. Floats appear only where a BELIEF is
//     compared with a price (the probability, the edge, the Kelly fraction), and the stake that
//     comes out of a Kelly fraction is floored to whole cents at once.
//   - Decide is pure. It moves no money and changes nothing: it returns what the engine would
//     like to do. Memory changes in exactly one place, Fold (reached through Apply), and the
//     runner calls that only after the ledger write has committed. So there is no state in which
//     memory is ahead of the ledger, and a restart rebuilds memory by folding the recorded fills
//     through the very same function.
//   - An order can come back filled, partly filled, cancelled or rejected, and the engine has a
//     rule for each (plan section 4.4).
//   - One 15-minute window across ALL coins is one bet: what a window has open, plus what it has
//     already lost, never passes quarter-Kelly of its single best bet (plan section 4.5).
//   - The probability leans on the market: p = mid + lambda * (p_model - mid). lambda has NO
//     default anywhere in this package. It is measured under docs/v3-measurement-protocol.md and
//     arrives with its provenance; a version without it cannot be constructed (params.go).
//
// The per-coin model state (volatility, the price ring, the learned index offset) is a fork of
// the second engine's CoinState, because that package's methods are unexported and the package
// is frozen. The arithmetic is kept byte for byte and a test holds the fork to the original on
// the recorded parity fixtures. ProbYes itself is not copied: it is exported, so it is called.
//
// No goroutines, no database, no logging, and the clock is always passed in. Model carries its
// own small mutex (plan section 5.1) so that the price stream never waits behind an engine lock
// that is held across a database write; Engine has no lock and is guarded by its caller.
//
// Design and worked examples: docs/honest-fills-v3.md, section 4.
//
// Where this package departs from the letter of that plan. Each takes the reading that spends or
// sells less; each is written out where it is coded and has a test (review of 2026-09-21):
//
//   - The per-price cost ceilings are per POSITION, not per order (Size): what the account already
//     holds in the market and side counts against the stake and against every step. The plan's
//     formula is per order, which let re-entries after min_gap spend a best-ask stake at the worst
//     acceptable prices.
//   - The last test before an order is formed uses the figures the ledger will BOOK for that
//     order, the fee rounded up per order (Size for buys, Account.exit for sells). The plan charges
//     "fee(c) exactly" per contract, which sends small orders at a loss after the real fee.
//   - No sale is sent that could book nothing or less (saleCanBookNothing): the position is held
//     to settlement and the decision says why. The plan books such a sale as it falls.
//   - An ask is a level showing at least ONE whole contract (decide): the edge, the band test, the
//     Kelly fraction and the window's k_max are taken at the price the broker will really give.
//     The mid is still the displayed touch, as lambda was measured on it.
//   - The buy limit never leaves the version's price band; the plan tests the best ask only.
//   - A settle-only account is held whatever its params say, and can never order (NewEngine).
//   - Apply takes the step's intents beside its reports, and the runner calls AfterDecide once
//     after every Decide: the soft state Decide may not touch (a dropped exit, and the seconds an
//     exit was wanted with no order sent) changes there.
package kalshi15m3
