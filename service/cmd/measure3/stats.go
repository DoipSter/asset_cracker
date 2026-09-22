package main

import (
	"math"
	"sort"
	"time"
)

// The arithmetic of docs/v3-measurement-protocol.md, sections 3 to 5, on rows already read.
// Nothing here touches a database or a file, so every function is checked on made-up rows.
//
// Every product is wrapped in mul, an explicit float64 conversion. The Go specification lets a
// compiler fuse x*y+z into one rounded operation, and arm64 does; the same rows would then give
// different low digits on the Pi and on a Mac, and (q-y)^2 - (q-y)^2 came out at 1e-19 in a
// test. An explicit conversion rounds the product first, so a rerun reproduces digits anywhere.

func mul(x, y float64) float64 { return float64(x * y) }
func sq(x float64) float64     { return float64(x * x) }

// feeRate is the venue's fee rate in the actionable-set test (3.1): a fact of the fee schedule.
const feeRate = 0.07

// row is one M1 row (3.1): the first snapshot in a 15-second bin of one market.
type row struct {
	W        int64 // window key: epoch seconds of closes_at, a multiple of 900
	Coin     string
	MarketID int64
	At       time.Time
	AtUnix   float64 // e.at, seconds
	P, Q     float64 // the forecast, and the yes mid
	Y        int     // 1 if the market settled yes
	YesAsk   float64
	NoAsk    float64
	YesBid   float64
	NoBid    float64
}

// D is p - q.
func (r row) D() float64 { return r.P - r.Q }

// InA reports whether the row is in the actionable set A (3.1): the favoured side's forecast
// clears that side's ask and the fee on it. d = 0 is not in A.
func (r row) InA() bool {
	d := r.D()
	if d == 0 {
		return false
	}
	ps, ask := r.P, r.YesAsk
	if d < 0 {
		ps, ask = 1-r.P, r.NoAsk
	}
	return ps-ask-mul(mul(feeRate, ask), 1-ask) > 0
}

// block is a window's block (5.1): six hours, or twelve when doubled.
func block(w int64, doubled bool) int64 {
	if doubled {
		return w / 43200
	}
	return w / 21600
}

// lambdaHat is L_hat = sum_A d (y - q) / sum_A d^2 on the rows given (already restricted to A).
// ok is false when the denominator is zero.
func lambdaHat(a []row) (l float64, ok bool) {
	var num, den float64
	for _, r := range a {
		d := r.D()
		num += mul(d, float64(r.Y)-r.Q)
		den += sq(d)
	}
	if den == 0 {
		return 0, false
	}
	return num / den, true
}

// selfCheck is 3.1's identity on the A rows: lhs = Brier(model) - Brier(mid), rhs = V (1 - 2 L),
// V = mean d^2. It returns both sides, the residual and the tolerance; pass is |lhs-rhs| <= tol.
func selfCheck(a []row, l float64) (lhs, rhs, residual, tol float64, pass bool) {
	if len(a) == 0 {
		return 0, 0, 0, 0, false
	}
	var bm, bq, v float64
	for _, r := range a {
		y := float64(r.Y)
		bm += sq(r.P - y)
		bq += sq(r.Q - y)
		v += sq(r.D())
	}
	n := float64(len(a))
	bm, bq, v = bm/n, bq/n, v/n
	lhs, rhs = bm-bq, mul(v, 1-2*l)
	residual = math.Abs(lhs - rhs)
	tol = math.Max(1e-12, mul(1e-9, math.Max(bm, bq)))
	return lhs, rhs, residual, tol, residual <= tol
}

// jackknife is the delete-one-block standard error (5.2): stat is recomputed without each block
// in turn; blocks with no observation do not count. It returns the SE and B, the blocks counted.
// stat returns ok false when it cannot be computed on the rows it is given; such a block is
// still deleted (its deletion is one of the B recomputations), but a stat that fails on ANY
// deletion makes the SE NaN, which the caller reports rather than hides.
func jackknife(blocks []int64, stat func(keep func(i int) bool) (float64, bool)) (se float64, b int) {
	present := map[int64]bool{}
	for _, bl := range blocks {
		present[bl] = true
	}
	ids := make([]int64, 0, len(present))
	for id := range present {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	b = len(ids)
	if b < 2 {
		return math.NaN(), b
	}
	thetas := make([]float64, 0, b)
	for _, drop := range ids {
		theta, ok := stat(func(i int) bool { return blocks[i] != drop })
		if !ok {
			return math.NaN(), b
		}
		thetas = append(thetas, theta)
	}
	var mean float64
	for _, t := range thetas {
		mean += t
	}
	mean /= float64(b)
	var ss float64
	for _, t := range thetas {
		ss += sq(t - mean)
	}
	return math.Sqrt(mul(float64(b-1)/float64(b), ss)), b
}

// lambdaFrozen is 4.1: clamp(floor(100 (L_hat - t SE)) / 100, 0, 1).
func lambdaFrozen(lhat, se, t float64) float64 {
	if math.IsNaN(se) {
		return 0
	}
	v := math.Floor(mul(100, lhat-mul(t, se))) / 100
	return math.Min(1, math.Max(0, v))
}

// costFrozen is 3.2 and 3.3: max(0, mean) rounded to 0.0001.
func costFrozen(mean float64) float64 {
	if math.IsNaN(mean) || mean <= 0 {
		return 0
	}
	return math.Round(mul(mean, 10000)) / 10000
}

// perWindow groups rows by window and keeps a fixed order of windows.
func perWindow(rows []row) (keys []int64, byW map[int64][]row) {
	byW = map[int64][]row{}
	for _, r := range rows {
		byW[r.W] = append(byW[r.W], r)
	}
	for w := range byW {
		keys = append(keys, w)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys, byW
}

// series is one per-window statistic on the windows that have a row in A, in window order.
type series struct {
	W []int64
	X []float64
}

// windowSeries computes the two per-window series of 5.3 on A rows: (a) the M1 numerator
// sum d (y - q), and (b) the mean of (p - y)^2 - (q - y)^2. Windows with no A row take no part.
func windowSeries(a []row) (num, brier series) {
	keys, byW := perWindow(a)
	for _, w := range keys {
		var n, bsum float64
		for _, r := range byW[w] {
			d := r.D()
			y := float64(r.Y)
			n += mul(d, y-r.Q)
			bsum += sq(r.P-y) - sq(r.Q-y)
		}
		num.W, num.X = append(num.W, w), append(num.X, n)
		brier.W, brier.X = append(brier.W, w), append(brier.X, bsum/float64(len(byW[w])))
	}
	return num, brier
}

// autocorr is the lag-k sample autocorrelation with one global mean (5.3), over the windows the
// keep function admits: r_k = sum over lag-k pairs (x_w - xbar)(x_{w+900k} - xbar) / sum (x_w - xbar)^2.
// A pair takes part only when BOTH members are kept. ok is false with no variance or no pair.
func autocorr(s series, k int, keep func(i int) bool) (r float64, ok bool) {
	idx := map[int64]int{}
	var xbar float64
	n := 0
	for i, w := range s.W {
		if keep(i) {
			idx[w] = i
			xbar += s.X[i]
			n++
		}
	}
	if n < 2 {
		return 0, false
	}
	xbar /= float64(n)
	var den, num float64
	pairs := 0
	for i, w := range s.W {
		if !keep(i) {
			continue
		}
		den += sq(s.X[i] - xbar)
		if j, has := idx[w+900*int64(k)]; has {
			num += mul(s.X[i]-xbar, s.X[j]-xbar)
			pairs++
		}
	}
	if den == 0 || pairs == 0 {
		return 0, false
	}
	return num / den, true
}

// autocorrWithSE is r_k with its block-jackknife SE: a deleted block takes its windows out, and
// with them every pair that has a member in it (autocorr's keep does exactly that).
func autocorrWithSE(s series, k int, doubled bool) (r, se float64, b int, ok bool) {
	r, ok = autocorr(s, k, func(int) bool { return true })
	if !ok {
		return 0, math.NaN(), 0, false
	}
	blocks := make([]int64, len(s.W))
	for i, w := range s.W {
		blocks[i] = block(w, doubled)
	}
	se, b = jackknife(blocks, func(keep func(i int) bool) (float64, bool) { return autocorr(s, k, keep) })
	return r, se, b, true
}

// meanSE is a plain mean with its block-jackknife SE over observations tagged with blocks.
func meanSE(x []float64, blocks []int64) (mean, se float64, b int) {
	if len(x) == 0 {
		return math.NaN(), math.NaN(), 0
	}
	for _, v := range x {
		mean += v
	}
	mean /= float64(len(x))
	se, b = jackknife(blocks, func(keep func(i int) bool) (float64, bool) {
		var s float64
		n := 0
		for i, v := range x {
			if keep(i) {
				s += v
				n++
			}
		}
		if n == 0 {
			return 0, false
		}
		return s / float64(n), true
	})
	return mean, se, b
}

// pearson is the correlation of two equal-length series; ok false without variance.
func pearson(x, y []float64) (float64, bool) {
	if len(x) != len(y) || len(x) < 2 {
		return 0, false
	}
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	n := float64(len(x))
	mx, my = mx/n, my/n
	var sxy, sxx, syy float64
	for i := range x {
		sxy += mul(x[i]-mx, y[i]-my)
		sxx += sq(x[i] - mx)
		syy += sq(y[i] - my)
	}
	if sxx == 0 || syy == 0 {
		return 0, false
	}
	return sxy / math.Sqrt(mul(sxx, syy)), true
}

// ---- Student's t ------------------------------------------------------------------------------

// tQuantile is t_{p, df}: the one-sided quantile of Student's t, by bisection on its CDF. The
// protocol quotes 1.729 (df 19), 1.725 (df 20), 1.833 (df 9), 1.895 (df 7), 1.860 (df 8),
// 2.353 (df 3) and 2.132 (df 4) at p = 0.95; the test checks each.
func tQuantile(p float64, df int) float64 {
	if df < 1 || p <= 0 || p >= 1 {
		return math.NaN()
	}
	lo, hi := 0.0, 1000.0
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if tCDF(mid, df) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// tCDF is P(T <= t) for Student's t with df degrees of freedom, through the regularized
// incomplete beta function: for t >= 0, 1 - I_x(df/2, 1/2) / 2 with x = df / (df + t^2).
func tCDF(t float64, df int) float64 {
	x := float64(df) / (float64(df) + t*t)
	tail := 0.5 * betaInc(float64(df)/2, 0.5, x)
	if t >= 0 {
		return 1 - tail
	}
	return tail
}

// betaInc is the regularized incomplete beta I_x(a, b), by the continued fraction of
// Numerical Recipes (betacf), with the symmetry relation for x past the mean.
func betaInc(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lbeta := lgamma(a+b) - lgamma(a) - lgamma(b) + a*math.Log(x) + b*math.Log(1-x)
	front := math.Exp(lbeta)
	if x < (a+1)/(a+b+2) {
		return front * betacf(a, b, x) / a
	}
	return 1 - front*betacf(b, a, 1-x)/b
}

func lgamma(x float64) float64 {
	v, _ := math.Lgamma(x)
	return v
}

func betacf(a, b, x float64) float64 {
	const maxIt, eps, fpmin = 300, 3e-16, 1e-300
	qab, qap, qam := a+b, a+1, a-1
	c, d := 1.0, 1-qab*x/qap
	if math.Abs(d) < fpmin {
		d = fpmin
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIt; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < fpmin {
			d = fpmin
		}
		c = 1 + aa/c
		if math.Abs(c) < fpmin {
			c = fpmin
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// normalCDF is Phi(z), for the power figure train-result.json reports (4.2: it decides nothing).
func normalCDF(z float64) float64 { return 0.5 * math.Erfc(-z/math.Sqrt2) }
