// Package pyfloat reproduces CPython's float behaviour where Go's differs, so that the Go port
// of the Python strategies makes the same decisions and writes the same trade log. Each
// function exists because the parity gate compares output character for character.
package pyfloat

import (
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// Round is Python's round(x, ndigits) for floats: the exact binary value rounded to ndigits
// decimals, exact ties to the even digit (round(0.125, 2) == 0.12).
func Round(x float64, ndigits int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || x == 0 {
		return x
	}
	r := new(big.Rat).SetFloat64(math.Abs(x)) // exact
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(ndigits))), nil)
	if ndigits >= 0 {
		r.Mul(r, new(big.Rat).SetInt(scale))
	} else {
		r.Quo(r, new(big.Rat).SetInt(scale))
	}
	q, rem := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	twice := new(big.Int).Lsh(rem, 1)
	if c := twice.Cmp(r.Denom()); c > 0 || (c == 0 && q.Bit(0) == 1) {
		q.Add(q, big.NewInt(1))
	}
	var text string
	if ndigits <= 0 {
		text = new(big.Int).Mul(q, scale).String()
	} else {
		digits := q.String()
		if len(digits) <= ndigits {
			digits = strings.Repeat("0", ndigits+1-len(digits)) + digits
		}
		cut := len(digits) - ndigits
		text = digits[:cut] + "." + digits[cut:]
	}
	v, _ := strconv.ParseFloat(text, 64)
	if x < 0 {
		return -v
	}
	return v
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Repr is Python's repr(float), which is what its csv module writes: the shortest digits that
// round-trip, fixed notation for exponents -4 to 15, otherwise "1.5e-05" style.
func Repr(x float64) string {
	switch {
	case math.IsNaN(x):
		return "nan"
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	case x == 0:
		if math.Signbit(x) {
			return "-0.0"
		}
		return "0.0"
	}
	sci := strconv.FormatFloat(math.Abs(x), 'e', -1, 64) // shortest digits: "d.ddde+NN"
	mant, expText, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expText)
	sign := ""
	if x < 0 {
		sign = "-"
	}
	if exp >= -4 && exp < 16 {
		point := exp + 1
		switch {
		case point <= 0:
			return sign + "0." + strings.Repeat("0", -point) + digits
		case point >= len(digits):
			return sign + digits + strings.Repeat("0", point-len(digits)) + ".0"
		default:
			return sign + digits[:point] + "." + digits[point:]
		}
	}
	tail := ""
	if len(digits) > 1 {
		tail = "." + digits[1:]
	}
	es := "+"
	if exp < 0 {
		es = "-"
	}
	e := strconv.Itoa(abs(exp))
	if len(e) < 2 {
		e = "0" + e
	}
	return sign + digits[:1] + tail + "e" + es + e
}

// FloorDiv is Python's float a // b, which CPython derives from fmod and is not always the
// floor of the rounded quotient.
func FloorDiv(a, b float64) float64 {
	mod := math.Mod(a, b)
	div := (a - mod) / b
	if mod != 0 && (b < 0) != (mod < 0) {
		div--
	}
	if div != 0 {
		f := math.Floor(div)
		if div-f > 0.5 {
			f++
		}
		return f
	}
	return math.Copysign(0, a/b)
}

// Sum adds in order, one at a time, as Python's sum did for floats up to 3.11. (From 3.12 it
// compensates for rounding and can differ in the last bit; the fixtures were recorded on 3.9.)
func Sum(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

// Median is Python's statistics.median.
func Median(values []float64) float64 {
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
