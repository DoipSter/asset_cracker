package engine

import (
	"math"
	"sort"
)

// A ladder is a distribution. The yes price of the leg "above K" is the market's P(S_T > K), so
// the legs of one close, read across strikes, are the market's cumulative distribution of the
// coin's price at that close. Fitting a lognormal to them gives the market's implied volatility
// and median for that horizon, to set against the forecast (vol.go); and because a distribution
// function cannot rise with the strike, two legs whose prices cross are a riskless pair, or
// would be but for the fee. Both are read from quotes alone: no outcome, no protocol question.

// Leg is one leg's top of book in dollars.
type Leg struct {
	Ticker   string
	Strike   float64
	Bid, Ask float64 // yes bid and yes ask
}

// Implied is the lognormal the ladder's mids imply.
type Implied struct {
	OK     bool
	Why    string  // when not OK
	Legs   int     // legs the fit used (mids between the probit bounds)
	Sigma  float64 // per sqrt-second, comparable with the model's
	Median float64 // the strike the market puts at even odds
	RMSE   float64 // of the fitted yes mids against the quoted ones, in dollars
}

// probit bounds: a mid nearer 0 or 1 than this carries almost no information about the shape
// and a great deal of noise in the probit. [CONVENTION]
const (
	probitLo = 0.03
	probitHi = 0.97
)

// ImpliedFromLadder fits P(S_T > K) = 1 - Phi((ln K - mu) / (sigma sqrt(tau))) to the legs'
// mids by a straight line in (ln K, probit(1 - mid)): slope 1 / (sigma sqrt tau), intercept
// -mu / (sigma sqrt tau). tau is seconds to the close. At least three usable legs.
func ImpliedFromLadder(legs []Leg, tau float64) Implied {
	if !(tau > 0) {
		return Implied{Why: "no time to the close"}
	}
	var xs, zs []float64
	for _, l := range legs {
		if !(l.Strike > 0) || !(l.Bid > 0) || !(l.Ask > 0) || l.Ask > 1 || l.Bid > l.Ask {
			continue
		}
		mid := (l.Bid + l.Ask) / 2
		if mid < probitLo || mid > probitHi {
			continue
		}
		xs = append(xs, math.Log(l.Strike))
		zs = append(zs, probit(1-mid))
	}
	n := float64(len(xs))
	if len(xs) < 3 {
		return Implied{Legs: len(xs), Why: "fewer than three legs priced between 0.03 and 0.97"}
	}
	var sx, sz, sxx, sxz float64
	for i := range xs {
		sx += xs[i]
		sz += zs[i]
		sxx += xs[i] * xs[i]
		sxz += xs[i] * zs[i]
	}
	den := n*sxx - sx*sx
	if !(den > 0) {
		return Implied{Legs: len(xs), Why: "the strikes do not spread"}
	}
	b := (n*sxz - sx*sz) / den
	a := (sz - b*sx) / n
	if !(b > 0) {
		return Implied{Legs: len(xs), Why: "the prices do not fall with the strike: no distribution fits"}
	}
	sigmaSqrtTau := 1 / b
	mu := -a / b
	out := Implied{OK: true, Legs: len(xs), Sigma: sigmaSqrtTau / math.Sqrt(tau), Median: math.Exp(mu)}
	var ss float64
	for i := range xs {
		fitted := 1 - NormCDF(a+b*xs[i])
		quoted := 1 - NormCDF(zs[i])
		ss += (fitted - quoted) * (fitted - quoted)
	}
	out.RMSE = math.Sqrt(ss / n)
	return out
}

// Crossing is two legs of one close whose prices contradict the order of their strikes: the
// lower strike's yes ask under the higher strike's yes bid. Buying yes on the lower strike and
// selling yes on the higher one pays at least the gap whatever the coin does; the fee on the
// two fills is what is left to beat.
type Crossing struct {
	Low, High Leg
	GapCents  int64 // High.Bid - Low.Ask, per contract
	FeeCents  int64 // Kalshi's taker fee on both fills, one contract each, rounded up per fill
	NetCents  int64 // Gap - Fee
}

// Crossings finds every such pair among one close's legs, net of fee, best first. Kalshi's fee
// is 0.07 * p * (1 - p) per contract, rounded up to a cent on each fill. [FACT: the fee schedule
// the Paper charges]
func Crossings(legs []Leg) []Crossing {
	sorted := append([]Leg(nil), legs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Strike < sorted[j].Strike })
	var out []Crossing
	for i := 0; i < len(sorted); i++ {
		lo := sorted[i]
		if !(lo.Ask > 0) || lo.Ask >= 1 {
			continue
		}
		for j := i + 1; j < len(sorted); j++ {
			hi := sorted[j]
			if !(hi.Bid > 0) || hi.Strike <= lo.Strike || hi.Bid <= lo.Ask {
				continue
			}
			gap := int64(math.Round((hi.Bid - lo.Ask) * 100))
			fee := feeCentsOneContract(lo.Ask) + feeCentsOneContract(hi.Bid)
			out = append(out, Crossing{Low: lo, High: hi, GapCents: gap, FeeCents: fee, NetCents: gap - fee})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NetCents > out[j].NetCents })
	return out
}

// feeCentsOneContract is the taker fee of one contract at price p in dollars, rounded up to a
// cent: the same arithmetic as broker.FeeCents on a whole-cent price.
func feeCentsOneContract(p float64) int64 {
	return int64(math.Ceil(7 * p * (1 - p)))
}

// probit is the inverse of the standard normal distribution function (Acklam's rational
// approximation, relative error under 1.2e-9, refined by one Newton step).
func probit(p float64) float64 {
	switch {
	case p <= 0:
		return math.Inf(-1)
	case p >= 1:
		return math.Inf(1)
	}
	a := [6]float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := [5]float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := [6]float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := [4]float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	const plow, phigh = 0.02425, 1 - 0.02425
	var x float64
	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		x = (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p > phigh:
		q := math.Sqrt(-2 * math.Log(1-p))
		x = -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	default:
		q := p - 0.5
		r := q * q
		x = (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q / (((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	}
	// One Newton step on Phi(x) - p.
	e := NormCDF(x) - p
	u := e * math.Sqrt(2*math.Pi) * math.Exp(x*x/2)
	return x - u/(1+x*u/2)
}
