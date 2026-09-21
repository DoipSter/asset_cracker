package broker

import (
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// Ladder names one side's resting bids, by the key the recorded quotes use.
type Ladder string

const (
	YesBids Ladder = "yes_bids"
	NoBids  Ladder = "no_bids"

	// RecordedLevels is how many bid levels per side a snapshot keeps (kalshi/quotes.go, depthOf).
	// Nothing past them can be filled from, because nothing past them was seen.
	RecordedLevels = 5
)

// Level is one recorded bid: its price and the WHOLE contracts displayed there. Kalshi shows
// fractional sizes (23.62 in the recorded DOGE book); an order is whole contracts, so the
// fraction is floored away and a level of 0.62 fills nothing.
type Level struct {
	Bid  Price
	Size int
}

// LadderFor says which resting bids an order draws on, and TakerPrice what the taker pays or
// receives against one of them. Kalshi's book lists only bids: buying Yes is filled by a No bid
// at 1 minus that bid, and the other way round; a sale hits the bids of its own side.
//
// The key is the RESTING order's side and price, so buying Yes at 0.12 and selling No at 0.88
// draw on the same contracts, which is true of the real book.
func LadderFor(a Action, s Side) Ladder {
	if (a == Buy) == (s == Yes) { // buy yes, sell no
		return NoBids
	}
	return YesBids // buy no, sell yes
}

// TakerPrice is the taker's price per contract against a resting bid.
func TakerPrice(a Action, bid Price) Price {
	if a == Buy {
		return priceScale - bid
	}
	return bid
}

// Sides is both ladders of one recorded snapshot: at most RecordedLevels levels each, best
// (highest bid) first. A side that could not be read carries its error and no levels; Paper
// then refuses orders on it and leaves its holds alone rather than guess.
//
// Crossed: both sides were read and the best Yes bid plus the best No bid is MORE than 1.0000,
// which is a Yes bid above the Yes ask (the ask is 1 minus the best No bid). A matching venue
// cannot show that: the two orders would have traded with each other. So it can only be a torn
// or faulty recording, and no price in it can be trusted. Paper treats the whole snapshot as it
// treats an unreadable side: orders on either side are rejected with ReasonCrossed and no hold
// is touched. [CONVENTION: an unusable snapshot fills nothing.] Filled as displayed, a bucket
// could buy Yes under the price it sells Yes at in the same second, money from a book that never
// existed. A sum of exactly 1.0000 (a locked book) is left alone: bought and sold at one price
// it gives nothing away, and the fees are still paid.
type Sides struct {
	Yes, No       []Level
	YesErr, NoErr error
	Crossed       bool
}

// errCrossed is what side returns for either ladder of a crossed snapshot.
var errCrossed = errors.New(ReasonCrossed)

// Ladders reads a recorded snapshot. The 1c/3c/5c cumulative depth figures in Quotes are never
// used: they have no prices, so nothing can be filled from them.
func Ladders(q kalshi.Quotes) Sides {
	var s Sides
	s.Yes, s.YesErr = parseLadder(q.YesBids)
	s.No, s.NoErr = parseLadder(q.NoBids)
	// The best bid is the best DISPLAYED bid, whatever its size floors to: a level showing 0.62
	// of a contract is still a resting order at that price.
	s.Crossed = s.YesErr == nil && s.NoErr == nil && len(s.Yes) > 0 && len(s.No) > 0 &&
		s.Yes[0].Bid+s.No[0].Bid > priceScale
	return s
}

// side is one ladder, or why it may not be used: its own reading error, or errCrossed.
func (s Sides) side(l Ladder) ([]Level, error) {
	if s.Crossed {
		return nil, errCrossed
	}
	if l == YesBids {
		return s.Yes, s.YesErr
	}
	return s.No, s.NoErr
}

func parseLadder(rows [][2]string) ([]Level, error) {
	sizes := map[Price]*big.Rat{}
	for _, row := range rows {
		size, ok := new(big.Rat).SetString(row[1])
		if !ok {
			return nil, fmt.Errorf("size %q is not a number", row[1])
		}
		if size.Sign() <= 0 {
			continue // an empty level is not a level, as in kalshi.ladder
		}
		bid, err := parsePrice(row[0])
		if err != nil {
			return nil, err
		}
		if bid < 1 || bid > priceScale-1 {
			return nil, fmt.Errorf("bid %q is outside 0.0001..0.9999", row[0])
		}
		if prev, dup := sizes[bid]; dup {
			size.Add(size, prev) // one price listed twice is one level
		}
		sizes[bid] = size
	}
	out := make([]Level, 0, len(sizes))
	for bid, size := range sizes {
		out = append(out, Level{Bid: bid, Size: floorRat(size)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bid > out[j].Bid })
	if len(out) > RecordedLevels {
		out = out[:RecordedLevels] // never a sixth level, whatever is handed in
	}
	return out, nil
}

// parsePrice reads decimal dollars ("0.0990") as exact ten-thousandths. big.Rat, not
// ParseFloat: 0.0990 has no exact float.
func parsePrice(s string) (Price, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("price %q is not a number", s)
	}
	r.Mul(r, big.NewRat(priceScale, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, fmt.Errorf("price %q is finer than 1/%d of a dollar", s, priceScale)
	}
	return Price(r.Num().Int64()), nil
}

// floorRat floors a positive size to whole contracts, capped at MaxQty.
func floorRat(r *big.Rat) int {
	n := new(big.Int).Quo(r.Num(), r.Denom()) // both positive: Quo is floor
	if !n.IsInt64() || n.Int64() > MaxQty {
		return MaxQty
	}
	return int(n.Int64())
}
