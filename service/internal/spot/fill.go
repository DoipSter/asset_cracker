// Package spot is the paper fill and mark for Coinbase coins. It is not a runner and it
// cannot place an order. A later slice will call these from a spot-family runner.
//
// Fill is at the last print. That print is not a book: there is no bid, no ask, no depth.
// Filling at it is optimistic against a real taker, who would pay the spread. The fee is a
// named convention: 60 bps of the USD notional (Coinbase Exchange retail taker, 2026-09-24).
// Size is a whole number of the product increment. Money is integer cents.
package spot

import (
	"fmt"
	"math/big"
	"strings"
)

// FeeBps is the taker fee in basis points of the USD notional. [CONVENTION]
const FeeBps = 60

// Fill is one paper fill at the last print.
type Fill struct {
	Qty           string // decimal text, a whole number of the increment
	PremiumCents  int64  // USD notional: rounded up on a buy, down on a sell
	FeeCents      int64  // FeeBps of the premium, rounded up
	LeftoverCents int64  // a buy: stake minus premium minus fee; a sell: 0
}

// Buy spends at most stakeCents at the last print. Qty is floored to increment. If one
// increment plus its fee will not fit, Qty is empty and LeftoverCents is the stake.
func Buy(price, increment string, stakeCents int64) (Fill, error) {
	px, inc, err := parseBook(price, increment)
	if err != nil {
		return Fill{}, err
	}
	if stakeCents <= 0 {
		return Fill{}, fmt.Errorf("stake %d is not positive", stakeCents)
	}
	// Largest notional n (cents) such that n + fee(n) <= stake.
	n := stakeCents * 10000 / (10000 + FeeBps)
	qty := floorToIncrement(centsToDollars(n), px, inc)
	for qty.Sign() > 0 {
		prem := centsCeil(mul(qty, px))
		fee := feeCents(prem)
		if prem+fee <= stakeCents {
			return Fill{Qty: ratString(qty), PremiumCents: prem, FeeCents: fee, LeftoverCents: stakeCents - prem - fee}, nil
		}
		qty.Sub(qty, inc)
	}
	return Fill{LeftoverCents: stakeCents}, nil
}

// Sell sells qty at the last print. qty must be a whole number of increment.
func Sell(price, increment, qty string) (Fill, error) {
	px, inc, err := parseBook(price, increment)
	if err != nil {
		return Fill{}, err
	}
	q, err := parsePositive(qty, "qty")
	if err != nil {
		return Fill{}, err
	}
	if !multipleOf(q, inc) {
		return Fill{}, fmt.Errorf("qty %s is not a whole number of increment %s", qty, increment)
	}
	prem := centsFloor(mul(q, px))
	return Fill{Qty: ratString(q), PremiumCents: prem, FeeCents: feeCents(prem)}, nil
}

func feeCents(premium int64) int64 {
	if premium <= 0 {
		return 0
	}
	return (premium*FeeBps + 9999) / 10000
}

func parseBook(price, increment string) (px, inc *big.Rat, err error) {
	if px, err = parsePositive(price, "price"); err != nil {
		return nil, nil, err
	}
	if inc, err = parsePositive(increment, "increment"); err != nil {
		return nil, nil, err
	}
	return px, inc, nil
}

func parsePositive(s, what string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return nil, fmt.Errorf("%s %q is not a number", what, s)
	}
	if r.Sign() <= 0 {
		return nil, fmt.Errorf("%s %s is not positive", what, s)
	}
	return r, nil
}

func mul(a, b *big.Rat) *big.Rat { return new(big.Rat).Mul(a, b) }

func centsToDollars(c int64) *big.Rat { return new(big.Rat).SetFrac64(c, 100) }

// floorToIncrement is floor((dollars/price)/increment)*increment.
func floorToIncrement(dollars, price, increment *big.Rat) *big.Rat {
	units := new(big.Rat).Quo(dollars, price)
	units.Quo(units, increment)
	n := new(big.Int)
	n.Quo(units.Num(), units.Denom())
	if n.Sign() < 0 {
		n.SetInt64(0)
	}
	return new(big.Rat).Mul(new(big.Rat).SetInt(n), increment)
}

func multipleOf(qty, increment *big.Rat) bool {
	q := new(big.Rat).Quo(qty, increment)
	return q.IsInt()
}

func centsCeil(dollars *big.Rat) int64 {
	c := new(big.Rat).Mul(dollars, big.NewRat(100, 1))
	n, rem := new(big.Int), new(big.Int)
	n.QuoRem(c.Num(), c.Denom(), rem)
	if rem.Sign() > 0 {
		n.Add(n, big.NewInt(1))
	}
	return n.Int64()
}

func centsFloor(dollars *big.Rat) int64 {
	c := new(big.Rat).Mul(dollars, big.NewRat(100, 1))
	n := new(big.Int)
	n.Quo(c.Num(), c.Denom())
	return n.Int64()
}

func ratString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	s := strings.TrimRight(r.FloatString(16), "0")
	return strings.TrimRight(s, ".")
}
