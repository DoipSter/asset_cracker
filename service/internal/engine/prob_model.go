package engine

import (
	"math"
	"strconv"
	"strings"
)

// ParseAmount reads a number from Kalshi, which sometimes arrives as text ("79,604.96").
// Copied from the frozen v2 archive so this package does not import it at runtime.
func ParseAmount(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(x, ",", "")), 64)
		return f, err == nil
	}
	return 0, false
}

// NormCDF is the standard normal distribution function. Copied with ProbYes, byte for byte.
func NormCDF(x float64) float64 { return 0.5 * (1 + math.Erf(x/math.Sqrt(2))) }

// ProbYes is the chance the settlement value ends at or above the strike. Copied from the
// frozen v2 archive (and v1 before it) so the live engine does not import that package.
// TestForkMatchesV2 holds it to the original on the recorded fixtures.
func ProbYes(price, strike, tau, sigma2 float64, knownAvg *float64, offsetPct, sdPct float64) float64 {
	lift := 1 + offsetPct
	var mean, variance float64
	if tau >= 60 {
		mean = price * lift
		variance = sigma2 * (price * price) * ((tau - 60) + 60.0/3)
	} else {
		seen := (60 - tau) / 60
		m := price
		if knownAvg != nil {
			m = float64(seen**knownAvg) + float64((1-seen)*price)
		}
		mean = m * lift
		variance = ((tau / 60) * (tau / 60)) * sigma2 * (price * price) * tau / 3
	}
	sd := math.Sqrt(variance + float64((sdPct*price)*(sdPct*price)))
	return math.Min(0.999, math.Max(0.001, NormCDF((mean-strike)/sd)))
}
