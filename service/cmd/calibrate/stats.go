package main

import (
	"math"
	"sort"
)

// Student's t and the block jackknife, as cmd/measure3 computes them (its stats.go), for H15's
// row: the long-shot protocol's t uses the same block jackknife and a one-sided t quantile.

// jackknife deletes each block in turn; blocks with no observation do not count. It returns the
// SE and B, the blocks counted. A stat that cannot be computed on some deletion makes the SE NaN.
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
		ss += (t - mean) * (t - mean)
	}
	return math.Sqrt(float64(b-1) / float64(b) * ss), b
}

// meanSE is a plain mean with its block-jackknife SE.
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

// tQuantile is t_{p, df}, by bisection on the CDF.
func tQuantile(p float64, df int) float64 {
	if df < 1 || p <= 0 || p >= 1 {
		return math.NaN()
	}
	lo, hi := -1000.0, 1000.0
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

// tCDF is P(T <= t) through the regularized incomplete beta: for t >= 0, 1 - I_x(df/2, 1/2)/2
// with x = df / (df + t^2).
func tCDF(t float64, df int) float64 {
	x := float64(df) / (float64(df) + t*t)
	tail := 0.5 * betaInc(float64(df)/2, 0.5, x)
	if t >= 0 {
		return 1 - tail
	}
	return tail
}

// betaInc is I_x(a, b) by the continued fraction of Numerical Recipes, with the symmetry
// relation past the mean.
func betaInc(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log(1-x))
	if x < (a+1)/(a+b+2) {
		return front * betacf(a, b, x) / a
	}
	return 1 - front*betacf(b, a, 1-x)/b
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
