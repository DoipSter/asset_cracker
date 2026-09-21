package kalshi

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
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

	// Depth behind the top of book. Without it two questions can never be answered from stored
	// data: how much could really have been SOLD at a moment (a sale hits the bids on its own
	// side), and how much more could have been BOUGHT within the slippage already assumed (a buy
	// of Yes is filled by the No bids). Found missing by the 2026-09-21 strategy review, in which
	// a simulated sale of 348 contracts met a displayed bid of 8.
	YesBids     [][2]string `json:"yes_bids"`      // the five best Yes bids, best first: [price, size]
	NoBids      [][2]string `json:"no_bids"`       // likewise for No
	YesBidDepth Depth       `json:"yes_bid_depth"` // contracts bid within 1c, 3c and 5c of the best Yes bid
	NoBidDepth  Depth       `json:"no_bid_depth"`
}

// Depth is cumulative size within a distance of the best price on one side.
type Depth struct {
	C1 float64 `json:"1c"`
	C3 float64 `json:"3c"`
	C5 float64 `json:"5c"`
}

type level struct {
	price int64 // ten-thousandths of a dollar
	size  float64
	text  [2]string
}

// ladder parses one side's bids, best first, skipping empty levels.
func ladder(levels [][]string) ([]level, error) {
	var out []level
	for _, lv := range levels {
		if len(lv) < 2 {
			continue
		}
		size, err := strconv.ParseFloat(lv[1], 64)
		if err != nil || size <= 0 {
			continue
		}
		p, err := parsePrice(lv[0])
		if err != nil {
			return nil, err
		}
		out = append(out, level{p, size, [2]string{lv[0], lv[1]}})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].price > out[j].price })
	return out, nil
}

func depthOf(l []level) (top [][2]string, d Depth) {
	top = [][2]string{}
	for i, lv := range l {
		if i < 5 {
			top = append(top, lv.text)
		}
		switch gap := l[0].price - lv.price; {
		case gap <= 100:
			d.C1 += lv.size
			fallthrough
		case gap <= 300:
			d.C3 += lv.size
			fallthrough
		case gap <= 500:
			d.C5 += lv.size
		}
	}
	round := func(v float64) float64 { return math.Round(v*100) / 100 }
	return top, Depth{round(d.C1), round(d.C3), round(d.C5)}
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
	yesLadder, err := ladder(yes)
	if err != nil {
		return Quotes{}, err
	}
	noLadder, err := ladder(no)
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
	q.YesBids, q.YesBidDepth = depthOf(yesLadder)
	q.NoBids, q.NoBidDepth = depthOf(noLadder)
	return q, nil
}
