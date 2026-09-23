package engine

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// The volatility forecast behind a long market's probability (2026-09-23). The rounds keep their
// fast estimate (the five-minute-halflife EWMA in coin.go) untouched while the third engine's
// measurement protocol runs; this file is for horizons past LongHorizon, where the question is
// how much the coin moves over hours to days, and a running estimate says nothing about that.
//
// HAR-RV (Corsi 2009): tomorrow's realised variance regressed on today's, the last week's mean
// and the last month's mean. It is fitted by ordinary least squares on the realised daily
// variances built from the recorded hourly candles, prices only: no market outcome is read, so
// it is free of every protocol's one-look rule, and its quality is measured the honest way, by
// forecast error against realised variance on days the fit never saw.

// HourlyClose is one hourly candle's close at its START time, as the candle table stores it.
type HourlyClose struct {
	At    time.Time
	Close float64
}

// DayRV is one UTC day's realised variance: the sum of squared hourly log returns inside it.
type DayRV struct {
	Day time.Time // midnight UTC
	RV  float64
}

// minHoursInDay is how many hourly returns a day needs before its variance counts; a day with
// a gap in the recording would read as calm. [CONVENTION: 20 of 24]
const minHoursInDay = 20

// DailyRealisedVariance turns hourly closes into one realised variance per UTC day, oldest
// first. The first return of a day is against the last close of the day before, so nothing
// between days is lost. Days short of minHoursInDay returns are dropped.
func DailyRealisedVariance(closes []HourlyClose) []DayRV {
	sorted := append([]HourlyClose(nil), closes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	var out []DayRV
	var day time.Time
	var sum float64
	var n int
	flush := func() {
		if n >= minHoursInDay {
			out = append(out, DayRV{Day: day, RV: sum})
		}
		sum, n = 0, 0
	}
	for i := 1; i < len(sorted); i++ {
		a, b := sorted[i-1], sorted[i]
		if !(a.Close > 0 && b.Close > 0) {
			continue
		}
		d := b.At.UTC().Truncate(24 * time.Hour)
		if !d.Equal(day) {
			if !day.IsZero() {
				flush()
			}
			day = d
		}
		r := math.Log(b.Close / a.Close)
		sum += r * r
		n++
	}
	if !day.IsZero() {
		flush()
	}
	return out
}

// HAR is a fitted model of next-day realised variance.
type HAR struct {
	C, Bd, Bw, Bm float64 // intercept; weights on the day, the week's mean, the month's mean
	N             int     // days the fit used
	R2            float64 // in-sample
	Floor         float64 // the least a forecast may be: a quarter of the smallest variance seen
}

// The HAR horizons, in days. [CONVENTION: Corsi's 1, 5, 22]
const (
	harWeek  = 5
	harMonth = 22
)

// harRow is the regressors at day t: the day's variance, the week's mean, the month's mean.
func harRow(rv []float64, t int) (d, w, m float64) {
	d = rv[t]
	for i := t - harWeek + 1; i <= t; i++ {
		w += rv[i]
	}
	for i := t - harMonth + 1; i <= t; i++ {
		m += rv[i]
	}
	return d, w / harWeek, m / harMonth
}

// FitHAR fits the model on a series of daily variances, oldest first. It needs at least sixty
// days past the month lag.
func FitHAR(rv []float64) (HAR, error) {
	n := len(rv) - harMonth
	if n < 60 {
		return HAR{}, fmt.Errorf("har: %d usable days; sixty are needed", max(n, 0))
	}
	// Normal equations for y = X b, X = [1 d w m], solved by Gaussian elimination.
	var xtx [4][4]float64
	var xty [4]float64
	var ySum, ySq float64
	rows := 0
	floor := math.Inf(1)
	for t := harMonth - 1; t+1 < len(rv); t++ {
		d, w, m := harRow(rv, t)
		y := rv[t+1]
		x := [4]float64{1, d, w, m}
		for i := 0; i < 4; i++ {
			for j := 0; j < 4; j++ {
				xtx[i][j] += x[i] * x[j]
			}
			xty[i] += x[i] * y
		}
		ySum += y
		ySq += y * y
		rows++
		if y > 0 && y < floor {
			floor = y
		}
	}
	b, err := solve4(xtx, xty)
	if err != nil {
		return HAR{}, err
	}
	h := HAR{C: b[0], Bd: b[1], Bw: b[2], Bm: b[3], N: rows}
	if math.IsInf(floor, 1) {
		floor = 0
	}
	h.Floor = floor / 4
	// R² in sample.
	mean := ySum / float64(rows)
	var ssRes float64
	for t := harMonth - 1; t+1 < len(rv); t++ {
		d, w, m := harRow(rv, t)
		e := rv[t+1] - h.predict(d, w, m)
		ssRes += e * e
	}
	if ssTot := ySq - float64(rows)*mean*mean; ssTot > 0 {
		h.R2 = 1 - ssRes/ssTot
	}
	return h, nil
}

func (h HAR) predict(d, w, m float64) float64 {
	v := h.C + h.Bd*d + h.Bw*w + h.Bm*m
	if v < h.Floor {
		return h.Floor
	}
	return v
}

// Forecast is next-day realised variance from the tail of a series of daily variances (at
// least the month lag long).
func (h HAR) Forecast(rv []float64) (float64, error) {
	if len(rv) < harMonth {
		return 0, fmt.Errorf("har: %d days; %d are needed to forecast", len(rv), harMonth)
	}
	d, w, m := harRow(rv, len(rv)-1)
	return h.predict(d, w, m), nil
}

// SigmaPerSqrtSecond turns a daily variance into the per-sqrt-second volatility the model's
// probability takes (ProbYes scales it by sqrt(tau)). [CONVENTION: variance grows with time; a
// week's variance is seven days' worth]
func SigmaPerSqrtSecond(dailyVariance float64) float64 {
	if dailyVariance <= 0 {
		return 0
	}
	return math.Sqrt(dailyVariance / 86400)
}

// Holdout is how the fit did on days it never saw, against the naive forecast the ladders used
// before (the mean variance of the last sixty days): mean absolute error of log variance,
// which weighs a calm day's miss like a wild day's.
type Holdout struct {
	Days       int
	HAR, Naive float64 // mean |log(forecast) - log(actual)|
	Fit        HAR     // the fit on the training part
}

// HoldoutHAR fits on all but the last k days and forecasts each of those one step ahead.
func HoldoutHAR(rv []float64, k int) (Holdout, error) {
	if k < 10 || len(rv)-k < harMonth+60 {
		return Holdout{}, errors.New("har holdout: too few days for a training part and a held-out part")
	}
	train := rv[:len(rv)-k]
	h, err := FitHAR(train)
	if err != nil {
		return Holdout{}, err
	}
	out := Holdout{Fit: h}
	for t := len(train); t < len(rv); t++ {
		actual := rv[t]
		if !(actual > 0) {
			continue
		}
		f, err := h.Forecast(rv[:t])
		if err != nil {
			return Holdout{}, err
		}
		naive := 0.0
		lo := max(0, t-60)
		for _, v := range rv[lo:t] {
			naive += v
		}
		naive /= float64(t - lo)
		out.HAR += math.Abs(math.Log(f) - math.Log(actual))
		if naive > 0 {
			out.Naive += math.Abs(math.Log(naive) - math.Log(actual))
		}
		out.Days++
	}
	if out.Days == 0 {
		return Holdout{}, errors.New("har holdout: no held-out day had a variance")
	}
	out.HAR /= float64(out.Days)
	out.Naive /= float64(out.Days)
	return out, nil
}

// solve4 solves a 4x4 linear system by Gaussian elimination with partial pivoting.
func solve4(a [4][4]float64, b [4]float64) ([4]float64, error) {
	for col := 0; col < 4; col++ {
		pivot := col
		for r := col + 1; r < 4; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[pivot][col]) {
				pivot = r
			}
		}
		if math.Abs(a[pivot][col]) < 1e-300 {
			return [4]float64{}, errors.New("har: the regressors are collinear; no fit")
		}
		a[col], a[pivot] = a[pivot], a[col]
		b[col], b[pivot] = b[pivot], b[col]
		for r := col + 1; r < 4; r++ {
			f := a[r][col] / a[col][col]
			for c := col; c < 4; c++ {
				a[r][c] -= f * a[col][c]
			}
			b[r] -= f * b[col]
		}
	}
	var x [4]float64
	for r := 3; r >= 0; r-- {
		s := b[r]
		for c := r + 1; c < 4; c++ {
			s -= a[r][c] * x[c]
		}
		x[r] = s / a[r][r]
	}
	return x, nil
}
