package main

import (
	"fmt"
	"math"
	"sort"
)

// Sections 3, 4 and 5 assembled: the figures train-result.json and test-result.json carry.

// estimate is a point estimate with its block-jackknife SE and the blocks it rests on.
type estimate struct {
	Value float64 `json:"value"`
	SE    float64 `json:"se"`
	B     int     `json:"b"`
	N     int     `json:"n"`
	OK    bool    `json:"ok"`
}

func lambdaEstimate(a []row, doubled bool) estimate {
	l, ok := lambdaHat(a)
	if !ok {
		return estimate{OK: false, N: len(a)}
	}
	blocks := make([]int64, len(a))
	for i, r := range a {
		blocks[i] = block(r.W, doubled)
	}
	se, b := jackknife(blocks, func(keep func(i int) bool) (float64, bool) {
		var sub []row
		for i, r := range a {
			if keep(i) {
				sub = append(sub, r)
			}
		}
		return lambdaHat(sub)
	})
	return estimate{Value: l, SE: se, B: b, N: len(a), OK: true}
}

// m1Report is 3.1 in full: the estimate that decides, and the slices that decide nothing.
type m1Report struct {
	Rows            int                 `json:"rows"`              // scored rows of TRAIN windows
	RowsInA         int                 `json:"rows_in_a"`         //
	Windows         int                 `json:"windows"`           //
	WindowsWithoutA int                 `json:"windows_without_a"` //
	LHat            estimate            `json:"l_hat"`
	TQuantile       float64             `json:"t_quantile"`
	Lambda          float64             `json:"lambda"`
	SelfCheck       selfCheckReport     `json:"self_check"`
	ByCoin          map[string]estimate `json:"by_coin"`
	ByPriceBand     map[string]estimate `json:"by_price_band"`
	ByDTercile      map[string]estimate `json:"by_d_tercile"`
}

type selfCheckReport struct {
	LHS, RHS, Residual, Tolerance float64
	Pass                          bool
}

// measureM1 runs 3.1 and 4.1 on the scored rows of the split's windows (already filtered to
// the frozen list and the covered coins). The self-check is computed first and, if it fails,
// the caller prints only it (main does that).
func measureM1(rows []row, doubled bool) m1Report {
	rep := m1Report{Rows: len(rows), ByCoin: map[string]estimate{}, ByPriceBand: map[string]estimate{}, ByDTercile: map[string]estimate{}}
	var a []row
	for _, r := range rows {
		if r.InA() {
			a = append(a, r)
		}
	}
	rep.RowsInA = len(a)
	keys, _ := perWindow(rows)
	_, byA := perWindow(a)
	rep.Windows = len(keys)
	for _, w := range keys {
		if len(byA[w]) == 0 {
			rep.WindowsWithoutA++
		}
	}
	rep.LHat = lambdaEstimate(a, doubled)
	if rep.LHat.OK {
		lhs, rhs, res, tol, pass := selfCheck(a, rep.LHat.Value)
		rep.SelfCheck = selfCheckReport{lhs, rhs, res, tol, pass}
		if rep.LHat.B >= 2 {
			rep.TQuantile = tQuantile(0.95, rep.LHat.B-1)
			rep.Lambda = lambdaFrozen(rep.LHat.Value, rep.LHat.SE, rep.TQuantile)
		}
	}
	// Reported, never used: by coin, by price band of q, by tercile of |d|.
	byCoin := map[string][]row{}
	byBand := map[string][]row{}
	for _, r := range a {
		byCoin[r.Coin] = append(byCoin[r.Coin], r)
		byBand[priceBand(r.Q)] = append(byBand[priceBand(r.Q)], r)
	}
	for k, v := range byCoin {
		rep.ByCoin[k] = lambdaEstimate(v, doubled)
	}
	for k, v := range byBand {
		rep.ByPriceBand[k] = lambdaEstimate(v, doubled)
	}
	if len(a) >= 3 {
		abs := make([]float64, len(a))
		for i, r := range a {
			abs[i] = math.Abs(r.D())
		}
		sorted := append([]float64{}, abs...)
		sort.Float64s(sorted)
		c1, c2 := sorted[len(sorted)/3], sorted[2*len(sorted)/3]
		terc := map[string][]row{}
		for i, r := range a {
			switch {
			case abs[i] < c1:
				terc["low"] = append(terc["low"], r)
			case abs[i] < c2:
				terc["mid"] = append(terc["mid"], r)
			default:
				terc["high"] = append(terc["high"], r)
			}
		}
		for k, v := range terc {
			rep.ByDTercile[k] = lambdaEstimate(v, doubled)
		}
	}
	return rep
}

func priceBand(q float64) string {
	switch {
	case q < 0.2:
		return "0.00-0.20"
	case q < 0.4:
		return "0.20-0.40"
	case q < 0.6:
		return "0.40-0.60"
	case q < 0.8:
		return "0.60-0.80"
	default:
		return "0.80-1.00"
	}
}

// ---- 3.2 and 3.3 -------------------------------------------------------------------------------

// nextQuote is the first snapshot of the same market within 3 s after a row (3.2).
type nextQuote struct {
	Found                        bool
	YesAsk, NoAsk, YesBid, NoBid float64
}

// staleReport is M2 or M2s.
type staleReport struct {
	Observations    int      `json:"observations"`
	NoSide          int      `json:"no_side"`    // the forecast alone would neither buy nor sell
	BothSides       int      `json:"both_sides"` // a crossed book: counted and skipped
	NoNext          int      `json:"no_next"`    // no snapshot within 3 s
	Vanished        int      `json:"vanished"`   // the next quote was gone, read as the worst price
	Mean            estimate `json:"mean"`       // the frozen figure's estimate
	MeanNoVanished  estimate `json:"mean_no_vanished"`
	Frozen          float64  `json:"frozen"`
	Note            string   `json:"note"`
	UnderMinimumObs bool     `json:"under_minimum_observations"`
}

// staleCost measures 3.2 (sell false) or 3.3 (sell true) on every M1 row, one observation per row.
func staleCost(rows []row, next func(row) nextQuote, sell, doubled bool) staleReport {
	rep := staleReport{}
	var obs, obsNoVan []float64
	var blocks, blocksNoVan []int64
	for _, r := range rows {
		var yes, no bool
		if sell {
			yes, no = r.YesBid > r.P, r.NoBid > 1-r.P
		} else {
			yes, no = r.YesAsk < r.P, r.NoAsk < 1-r.P
		}
		switch {
		case yes && no:
			rep.BothSides++
			continue
		case !yes && !no:
			rep.NoSide++
			continue
		}
		nx := next(r)
		if !nx.Found {
			rep.NoNext++
			continue
		}
		var v float64
		vanished := false
		if sell {
			now, then := r.YesBid, nx.YesBid
			if no {
				now, then = r.NoBid, nx.NoBid
			}
			if then == 0 {
				vanished = true // a bid of 0: the loss is real
			}
			v = now - then
		} else {
			now, then := r.YesAsk, nx.YesAsk
			if no {
				now, then = r.NoAsk, nx.NoAsk
			}
			if then == 0 {
				vanished, then = true, 1.00 // nothing could be bought under a dollar
			}
			v = then - now
		}
		bl := block(r.W, doubled)
		obs, blocks = append(obs, v), append(blocks, bl)
		if vanished {
			rep.Vanished++
		} else {
			obsNoVan, blocksNoVan = append(obsNoVan, v), append(blocksNoVan, bl)
		}
	}
	rep.Observations = len(obs)
	m, se, b := meanSE(obs, blocks)
	rep.Mean = estimate{Value: m, SE: se, B: b, N: len(obs), OK: len(obs) > 0}
	m2, se2, b2 := meanSE(obsNoVan, blocksNoVan)
	rep.MeanNoVanished = estimate{Value: m2, SE: se2, B: b2, N: len(obsNoVan), OK: len(obsNoVan) > 0}
	rep.Frozen = costFrozen(m)
	what := "buy"
	if sell {
		what = "sell"
	}
	rep.Note = fmt.Sprintf("measured on the recorded TRAIN book at rows where the forecast alone would %s; no order; a proxy for v3's fills", what)
	if len(obs) < minObservations {
		rep.UnderMinimumObs = true
		rep.Note += fmt.Sprintf("; frozen on %d observations, under the %d of analysis.MinWindows", len(obs), minObservations)
	}
	return rep
}

// ---- 3.5 ---------------------------------------------------------------------------------------

type m3Report struct {
	Pairs map[string]estimate `json:"pairs"`
	Mean  float64             `json:"mean"`
}

// outcomeCorrelation is the Pearson correlation of y between each pair of covered coins over the
// windows both have a market in, with the block-jackknife SE over those windows.
func outcomeCorrelation(rows []row, doubled bool) m3Report {
	rep := m3Report{Pairs: map[string]estimate{}}
	yAt := map[int64]map[string]float64{} // window -> coin -> y
	for _, r := range rows {
		if yAt[r.W] == nil {
			yAt[r.W] = map[string]float64{}
		}
		yAt[r.W][r.Coin] = float64(r.Y)
	}
	coins := map[string]bool{}
	for _, r := range rows {
		coins[r.Coin] = true
	}
	var names []string
	for c := range coins {
		names = append(names, c)
	}
	sort.Strings(names)
	var sum float64
	n := 0
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			a, b := names[i], names[j]
			var ws []int64
			for w, m := range yAt {
				if _, ok := m[a]; ok {
					if _, ok := m[b]; ok {
						ws = append(ws, w)
					}
				}
			}
			sort.Slice(ws, func(x, y int) bool { return ws[x] < ws[y] })
			xs, ys := make([]float64, len(ws)), make([]float64, len(ws))
			blocks := make([]int64, len(ws))
			for k, w := range ws {
				xs[k], ys[k], blocks[k] = yAt[w][a], yAt[w][b], block(w, doubled)
			}
			r, ok := pearson(xs, ys)
			e := estimate{Value: r, N: len(ws), OK: ok}
			if ok {
				e.SE, e.B = jackknife(blocks, func(keep func(i int) bool) (float64, bool) {
					var kx, ky []float64
					for k := range xs {
						if keep(k) {
							kx, ky = append(kx, xs[k]), append(ky, ys[k])
						}
					}
					return pearson(kx, ky)
				})
				sum += r
				n++
			}
			rep.Pairs[a+"/"+b] = e
		}
	}
	if n > 0 {
		rep.Mean = sum / float64(n)
	} else {
		rep.Mean = math.NaN()
	}
	return rep
}

// ---- 5.3: the autocorrelation report and the block decision ----------------------------------

type lagReport struct {
	Lag int     `json:"lag"`
	R   float64 `json:"r"`
	SE  float64 `json:"se"`
	B   int     `json:"b"`
	OK  bool    `json:"ok"`
}

type autocorrReport struct {
	Numerator []lagReport `json:"numerator"` // (a) sum_A d (y - q) per window
	Brier     []lagReport `json:"brier"`     // (b) mean_A (p - y)^2 - (q - y)^2 per window
	Doubled   bool        `json:"block_doubled"`
	Why       string      `json:"why"`
}

var lags = []int{1, 2, 3, 4, 5, 6, 7, 8, 24}

// autocorrelations computes both series at every lag with six-hour blocks, and decides the
// doubling: if either lag-24 figure is greater than its own SE, the block is doubled for every
// SE in the protocol. Decided once, by TRAIN, before any TEST row is read.
func autocorrelations(a []row) autocorrReport {
	num, brier := windowSeries(a)
	rep := autocorrReport{}
	report := func(s series) []lagReport {
		var out []lagReport
		for _, k := range lags {
			r, se, b, ok := autocorrWithSE(s, k, false)
			out = append(out, lagReport{Lag: k, R: r, SE: se, B: b, OK: ok})
		}
		return out
	}
	rep.Numerator, rep.Brier = report(num), report(brier)
	for _, series := range [][]lagReport{rep.Numerator, rep.Brier} {
		for _, l := range series {
			if l.Lag == 24 && l.OK && !math.IsNaN(l.SE) && l.R > l.SE {
				rep.Doubled = true
			}
		}
	}
	if rep.Doubled {
		rep.Why = "a lag-24 autocorrelation exceeded its SE: the block is 12 hours and the embargo 48 windows"
	} else {
		rep.Why = "no lag-24 autocorrelation exceeded its SE: the block stays 6 hours"
	}
	return rep
}

// ---- 4.2, R2 ------------------------------------------------------------------------------------

type r2Report struct {
	Lambda          float64  `json:"lambda"`
	Windows         int      `json:"windows"`           // TEST windows
	WindowsWithA    int      `json:"windows_with_a"`    //
	WindowsWithoutA int      `json:"windows_without_a"` //
	Statistic       estimate `json:"statistic"`         // mean of b_w
	T               float64  `json:"t"`
	Threshold       float64  `json:"threshold"`
	Pass            bool     `json:"pass"`
	Verdict         string   `json:"verdict"`
}

// blendTest is R2: with lambda frozen, b_w = mean over a window's A rows of (q-y)^2 - (blend-y)^2,
// the statistic is the mean of b_w over windows with a row in A, t = mean / SE by the block
// jackknife, pass if t >= t_{0.95,B-1} with B >= 3.
func blendTest(rows []row, lambda float64, doubled bool) r2Report {
	rep := r2Report{Lambda: lambda}
	keys, byW := perWindow(rows)
	rep.Windows = len(keys)
	var bw []float64
	var blocks []int64
	for _, w := range keys {
		var sum float64
		n := 0
		for _, r := range byW[w] {
			if !r.InA() {
				continue
			}
			y := float64(r.Y)
			blend := r.Q + mul(lambda, r.D())
			sum += sq(r.Q-y) - sq(blend-y)
			n++
		}
		if n == 0 {
			rep.WindowsWithoutA++
			continue
		}
		rep.WindowsWithA++
		bw, blocks = append(bw, sum/float64(n)), append(blocks, block(w, doubled))
	}
	m, se, b := meanSE(bw, blocks)
	rep.Statistic = estimate{Value: m, SE: se, B: b, N: len(bw), OK: len(bw) > 0}
	switch {
	case b < 3:
		rep.Verdict = "too few blocks"
	case math.IsNaN(se) || se == 0:
		rep.Verdict = "no standard error"
	default:
		rep.T = m / se
		rep.Threshold = tQuantile(0.95, b-1)
		rep.Pass = rep.T >= rep.Threshold
		if rep.Pass {
			rep.Verdict = fmt.Sprintf("pass: t %.3f >= %.3f at B = %d", rep.T, rep.Threshold, b)
		} else {
			rep.Verdict = fmt.Sprintf("fail: t %.3f < %.3f at B = %d", rep.T, rep.Threshold, b)
		}
	}
	return rep
}

// powerReport is 4.2's note: R2's power at TRAIN's variance, reported and deciding nothing.
type powerReport struct {
	MeanBW   float64 `json:"mean_b_w"`
	SDBW     float64 `json:"sd_b_w"`
	Windows  int     `json:"windows_with_a"`
	Power192 float64 `json:"power_at_192_windows"` // normal approximation, one-sided 5% at t_{0.95,7}
	Note     string  `json:"note"`
}

func powerAtTrain(rows []row, lambda float64) powerReport {
	keys, byW := perWindow(rows)
	var bw []float64
	for _, w := range keys {
		var sum float64
		n := 0
		for _, r := range byW[w] {
			if !r.InA() {
				continue
			}
			y := float64(r.Y)
			blend := r.Q + mul(lambda, r.D())
			sum += sq(r.Q-y) - sq(blend-y)
			n++
		}
		if n > 0 {
			bw = append(bw, sum/float64(n))
		}
	}
	rep := powerReport{Windows: len(bw), Note: "normal approximation with TRAIN's per-window mean and SD at the frozen lambda; it decides nothing"}
	if len(bw) < 2 {
		rep.MeanBW, rep.SDBW, rep.Power192 = math.NaN(), math.NaN(), math.NaN()
		return rep
	}
	var m float64
	for _, v := range bw {
		m += v
	}
	m /= float64(len(bw))
	var ss float64
	for _, v := range bw {
		ss += sq(v - m)
	}
	sd := math.Sqrt(ss / float64(len(bw)-1))
	rep.MeanBW, rep.SDBW = m, sd
	if sd > 0 {
		rep.Power192 = normalCDF(m/(sd/math.Sqrt(testWindows)) - tQuantile(0.95, 7))
	} else {
		rep.Power192 = math.NaN()
	}
	return rep
}
