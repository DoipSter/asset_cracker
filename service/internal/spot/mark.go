package spot

import "fmt"

// Mark is qty at the last print, floored to whole cents: the sheet does not invent a fraction
// of a cent of value. The print is the same last print Fill uses; there is no bid.
func Mark(price, qty string) (int64, error) {
	px, err := parsePositive(price, "price")
	if err != nil {
		return 0, err
	}
	q, err := parsePositive(qty, "qty")
	if err != nil {
		return 0, err
	}
	cents := centsFloor(mul(q, px))
	if cents < 0 {
		return 0, fmt.Errorf("mark of %s at %s is negative", qty, price)
	}
	return cents, nil
}
