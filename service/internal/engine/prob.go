package engine

import (
	"github.com/doipster/asset_cracker/service/internal/broker"
)

// Prices in this file are the broker's ten-thousandths of a dollar. A per-contract cost with the
// fee inside needs more digits than that (the fee of a 0.12 contract is 0.007392), so costs are
// carried as whole 1e-10 dollars in int64 ("e10"): exact, and far from overflow (a dollar is
// 1e10). They become a float only where they meet a probability, which is a float already.
const (
	priceScale = 10000       // broker.Price units in a dollar
	e10PerUnit = 1_000_000   // 1e-10 dollars in one price unit
	e10PerCent = 100_000_000 // 1e-10 dollars in a cent
	e10Dollar  = 1e10        // as a float, for the one division into dollars
	feeRate7   = 7           // Kalshi's taker fee is 7% of price * (1 - price) [FACT: venue schedule]
	maxPrice   = priceScale - 1
)

// feeE10 is the taker fee of ONE contract at price t, exactly: 0.07 t (1 - t) dollars is
// 7 * T * (10000 - T) / 1e10 with T in price units. This unrounded fee is what the Kelly
// fraction, the journaled edge and the buy limit are worked with (plan 4.3). It is NOT what the
// ledger books: the broker rounds the fee of the whole order up to a cent. So the last test
// before an order is formed is made with bookedBuyCents or bookedSaleCents, the order's own
// figures (Size, and Account.exit).
func feeE10(t broker.Price) int64 { return feeRate7 * int64(t) * (priceScale - int64(t)) }

// buyUnitE10 is u of plan 4.5: what one contract bought at t costs for the purposes of the
// TEST, that is price plus fee plus the measured staleness cost.
func buyUnitE10(t broker.Price, staleUnits int64) int64 {
	return int64(t)*e10PerUnit + feeE10(t) + staleUnits*e10PerUnit
}

// paidE10 is what one contract bought at t really costs the bucket before rounding: price plus
// fee. The staleness cost is not a ledger cost, so it is not in here.
func paidE10(t broker.Price) int64 { return int64(t)*e10PerUnit + feeE10(t) }

// sellNetE10 is what one contract sold at bid b is worth for the purposes of the exit tests:
// the bid less the fee less the measured staleness cost of selling. It rises with b everywhere
// (its slope is 1 - 0.07 (1 - 2b), never under 0.93), which is what lets a limit be found by
// walking down the displayed bids until the test first fails.
func sellNetE10(b broker.Price, staleSellUnits int64) int64 {
	return int64(b)*e10PerUnit - feeE10(b) - staleSellUnits*e10PerUnit
}

// bookedBuyCents is what the ledger books for qty contracts bought at ONE price: the premium
// rounded up plus the order's fee rounded up, by the broker's own two functions.
func bookedBuyCents(qty int, t broker.Price) int64 {
	return broker.PremiumCents(broker.Buy, qty, t) + broker.FeeCents(qty, t)
}

// bookedSaleCents is what the ledger books for qty contracts sold at ONE price: the premium
// rounded down less the order's fee rounded up. It can be zero or negative (one contract at
// 0.0091 is 0c of premium and 1c of fee).
func bookedSaleCents(qty int, b broker.Price) int64 {
	return broker.PremiumCents(broker.Sell, qty, b) - broker.FeeCents(qty, b)
}

// saleCanBookNothing reports a bid at which ONE contract sold brings the bucket nothing or less.
// An immediate-or-cancel sale cannot name a least quantity, so an order whose limit is such a
// bid can come back with exactly that fill. Where one contract books at least a cent, every fill
// of any size at that price or a better one does too, and so does the order, whichever way the
// fee is rounded (the order's rounded fee is never more than the sum of its fills' rounded
// fees). TestSaleProceedsArePositive checks that over every price and a run of sizes; with the
// broker's present rounding it puts the line at two cents.
func saleCanBookNothing(b broker.Price) bool { return bookedSaleCents(1, b) <= 0 }

func dollars(e10 int64) float64 { return float64(e10) / e10Dollar }

// priceDollars is a price as the float the second engine would have parsed from the quote text:
// the division of two exactly representable integers rounds to the same double as parsing
// "0.1200" does.
func priceDollars(p broker.Price) float64 { return float64(p) / priceScale }

// Blend is the probability of Yes the engine acts on: the market's mid, moved toward the model
// by lambda. Written exactly as the second engine writes its blend (account.go: pUp), float64()
// wrapper included, so that lambda = 1 reproduces v2's Model strategy to the bit.
func Blend(mid, pModel, lambda float64) float64 { return mid + float64(lambda*(pModel-mid)) }

// kelly is the Kelly fraction of plan 4.5 at taker price t: (p - u) / (1 - u), v2's form. Zero
// when there is no edge, so nothing downstream sees a negative stake.
func kelly(pSide float64, t broker.Price, staleUnits int64) float64 {
	u := dollars(buyUnitE10(t, staleUnits))
	if !(pSide > u) || u >= 1 {
		return 0
	}
	return (pSide - u) / (1 - u)
}

// top is the touch of a two-sided book.
type top struct {
	YesBid, NoBid broker.Price
	Mid           float64 // of Yes, as v2 computes it: (yes bid + yes ask) / 2, the yes ask being 1 - no bid
}

// touch reads the mid. ok is false unless BOTH sides show a bid: lambda was measured on
// two-sided books only (protocol, M1), and v2 forms no view without both asks, so a one-sided
// book gives no probability here and no entry.
func touch(s broker.Sides) (top, bool) {
	if s.Crossed || s.YesErr != nil || s.NoErr != nil || len(s.Yes) == 0 || len(s.No) == 0 {
		return top{}, false
	}
	yb, nb := s.Yes[0].Bid, s.No[0].Bid
	ya := broker.Price(priceScale) - nb
	return top{YesBid: yb, NoBid: nb, Mid: (priceDollars(yb) + priceDollars(ya)) / 2}, true
}

// sideProb is the blended probability that the given side pays.
func sideProb(pYes float64, s broker.Side) float64 {
	if s == broker.Yes {
		return pYes
	}
	return 1 - pYes
}
