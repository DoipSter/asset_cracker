package kalshi

import (
	"fmt"
	"math/big"
)

// Prices on Kalshi are dollars with up to four decimals ("0.9950"). They are handled here as
// whole ten-thousandths of a dollar so that 1 - 0.9950 is exactly 0.0050, never 0.00500000001.
const priceScale = 10000

func parsePrice(s string) (int64, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	r.Mul(r, big.NewRat(priceScale, 1))
	if !r.IsInt() {
		return 0, fmt.Errorf("price %q is finer than 1/%d of a dollar", s, priceScale)
	}
	return r.Num().Int64(), nil
}

func formatPrice(v int64) string { return fmt.Sprintf("%d.%04d", v/priceScale, v%priceScale) }

// Quotes is the top of a market's order book, as decimal text.
type Quotes struct {
	YesBid     string `json:"yes_bid"`
	YesAsk     string `json:"yes_ask"`
	NoBid      string `json:"no_bid"`
	NoAsk      string `json:"no_ask"`
	YesAskSize string `json:"yes_ask_size"`
	NoAskSize  string `json:"no_ask_size"`
	YesLevels  int    `json:"yes_levels"`
	NoLevels   int    `json:"no_levels"`
}

// BookQuotes reads live quotes from an order book. Kalshi's book lists the BIDS on each side.
// Buying Yes is filled by a No bid, so the Yes ask is 1 minus the best No bid, and the size
// available at that ask is that No bid's size; and the other way round.
//
// The order book is used, not the market list or the single-market endpoint, because those
// are cached for several seconds and were measured to go stale (54/55 shown while the book
// had moved to 61).
func BookQuotes(yes, no [][]string) (Quotes, error) {
	best := func(levels [][]string) (price int64, size string, n int, err error) {
		for _, lv := range levels {
			if len(lv) < 2 {
				continue
			}
			qty, ok := new(big.Rat).SetString(lv[1])
			if !ok || qty.Sign() <= 0 {
				continue // skip empty levels
			}
			p, perr := parsePrice(lv[0])
			if perr != nil {
				return 0, "", 0, perr
			}
			n++
			if p > price {
				price, size = p, lv[1]
			}
		}
		return price, size, n, nil
	}
	yesBid, yesSize, yesN, err := best(yes)
	if err != nil {
		return Quotes{}, err
	}
	noBid, noSize, noN, err := best(no)
	if err != nil {
		return Quotes{}, err
	}
	q := Quotes{YesBid: formatPrice(yesBid), NoBid: formatPrice(noBid), YesLevels: yesN, NoLevels: noN,
		YesAsk: "0.0000", NoAsk: "0.0000", YesAskSize: "0", NoAskSize: "0"}
	if noBid > 0 {
		q.YesAsk, q.YesAskSize = formatPrice(priceScale-noBid), noSize
	}
	if yesBid > 0 {
		q.NoAsk, q.NoAskSize = formatPrice(priceScale-yesBid), yesSize
	}
	return q, nil
}
