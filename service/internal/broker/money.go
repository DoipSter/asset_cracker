package broker

// Money here is whole cents in int64 and prices are whole ten-thousandths of a dollar. There is
// no float and no epsilon anywhere in this file: the second engine's fee needs "- 1e-9" before
// its ceil to undo float error, and an integer ceil needs nothing.

const (
	priceScale = 10000 // Price units in a dollar

	// feeDenominator turns a fee numerator into cents. Kalshi's taker fee is 7% of
	// contracts * price * (1 - price) dollars. With the price in ten-thousandths that is
	// 7 * qty * P * (10000 - P) / 100 / 10000 / 10000 dollars, so in cents it is
	// 7 * qty * P * (10000 - P) / 1e8.
	feeDenominator = 100_000_000

	// MaxQty bounds an order and a displayed size so that the fee numerator cannot overflow:
	// 7 * 1e9 * 5000 * 5000 is 1.75e17 per level, 8.75e17 over five levels, under int64's 9.2e18.
	// It is a guard, not a venue rule; the recorded books show sizes in the thousands.
	MaxQty = 1_000_000_000
)

// PremiumCents is what qty contracts at price p come to in whole cents. A buy rounds UP and a
// sell rounds DOWN. Sub-cent levels are real (0.0990 in the recorded books) and Kalshi's own
// rounding of them is unknown [ASSUMED, plan section 14], so the bucket never gains from rounding.
func PremiumCents(a Action, qty int, p Price) int64 {
	n := int64(qty) * int64(p) // ten-thousandths of a dollar; 100 of them make a cent
	if a == Buy {
		return (n + 99) / 100
	}
	return n / 100
}

// feeNumerator is the taker fee of one fill before rounding: fee in cents = numerator / 1e8.
// Numerators add, so the fee of a whole order can be rounded once.
func feeNumerator(qty int, p Price) int64 {
	return 7 * int64(qty) * int64(p) * (priceScale - int64(p))
}

// ceilFee rounds a summed fee numerator up to whole cents.
func ceilFee(numerator int64) int64 {
	return (numerator + feeDenominator - 1) / feeDenominator
}

// FeeCents is the taker fee of qty contracts at price p filled at ONE level, rounded up to a
// cent. It equals the second engine's KalshiFee to the cent (tested), without the float.
func FeeCents(qty int, p Price) int64 { return ceilFee(feeNumerator(qty, p)) }

// feeShare is one fill's share of the order's fee, given the numerators summed BEFORE it and
// its own.
//
// Default: the order's fee is rounded up ONCE, and each fill takes what it adds to that rounded
// total: ceil(after/1e8) - ceil(before/1e8). The shares therefore always add up to the order's
// fee exactly. That Kalshi rounds a multi-level sweep once per order is [ASSUMED]. If it is
// wrong the truth is at most (levels - 1) cents dearer per order.
//
// perFill rounds every fill up on its own: the pessimistic reading, never cheaper (tested).
func feeShare(before, own int64, perFill bool) int64 {
	if perFill {
		return ceilFee(own)
	}
	return ceilFee(before+own) - ceilFee(before)
}

// BucketCents is the bucket's cash effect of one fill: a buy pays premium and fee, a sell
// receives premium less fee. The store writes this figure and the engine applies it, so the two
// cannot disagree. It can be zero or negative for a sale (one contract at 0.0091 is 0c of
// premium and 1c of fee: the bucket PAYS a cent, and that is what is booked).
func BucketCents(a Action, f Fill) int64 {
	if a == Buy {
		return -(f.PremiumCents + f.FeeCents)
	}
	return f.PremiumCents - f.FeeCents
}
