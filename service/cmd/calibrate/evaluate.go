package main

import (
	"math"
	"math/rand/v2"
	"sort"
	"time"
)

// "What counts as a result" with "The TEST figures" and "The pass rule, read strictly": Brier
// and log loss as means over observations, overall and by τ bin, for p_cal, the mid and the
// frozen blend; the improvement is the mid's log loss less p_cal's. Useful when the overall
// improvement is above zero with the 5th percentile of its 2,000 paired bootstrap resamples above
// zero, AND in at least three of the five bins the same holds for the bin.

const (
	blendLambda   = 0.5
	bootResamples = 2000
	bootSeed      = 20260925 // [CONVENTION] math/rand/v2's PCG, seeded (bootSeed, bootSeed)
	bootPercent   = 0.05     // the 5th percentile, nearest rank
	binsNeeded    = 3
)

// scored is one observation with its three probabilities and its outcome.
type scored struct {
	bin    int
	closes time.Time
	cal    float64
	mid    float64
	blend  float64
	y      float64
}

func brier(q, y float64) float64 { return (q - y) * (q - y) }

func logLoss(q, y float64) float64 {
	q = hold(q)
	return -(y*math.Log(q) + (1-y)*math.Log(1-q))
}

// figures are the pooled means of one set of observations.
type figures struct {
	N           int     `json:"n"`
	BrierCal    float64 `json:"brier_p_cal"`
	BrierMid    float64 `json:"brier_mid"`
	BrierBlend  float64 `json:"brier_blend"`
	LogCal      float64 `json:"logloss_p_cal"`
	LogMid      float64 `json:"logloss_mid"`
	LogBlend    float64 `json:"logloss_blend"`
	Improvement float64 `json:"improvement"` // logloss_mid - logloss_p_cal
}

func figuresOf(obs []scored, keep func(s scored) bool) figures {
	var f figures
	for _, s := range obs {
		if !keep(s) {
			continue
		}
		f.N++
		f.BrierCal += brier(s.cal, s.y)
		f.BrierMid += brier(s.mid, s.y)
		f.BrierBlend += brier(s.blend, s.y)
		f.LogCal += logLoss(s.cal, s.y)
		f.LogMid += logLoss(s.mid, s.y)
		f.LogBlend += logLoss(s.blend, s.y)
	}
	if f.N == 0 {
		nan := math.NaN()
		return figures{BrierCal: nan, BrierMid: nan, BrierBlend: nan, LogCal: nan, LogMid: nan, LogBlend: nan, Improvement: nan}
	}
	n := float64(f.N)
	f.BrierCal, f.BrierMid, f.BrierBlend = f.BrierCal/n, f.BrierMid/n, f.BrierBlend/n
	f.LogCal, f.LogMid, f.LogBlend = f.LogCal/n, f.LogMid/n, f.LogBlend/n
	f.Improvement = f.LogMid - f.LogCal
	return f
}

// binFigures is one bin's figures and its bootstrap reading.
type binFigures struct {
	Bin       string  `json:"bin"`
	Figures   figures `json:"figures"`
	P5        float64 `json:"improvement_p5"`
	Resamples int     `json:"resamples_holding_the_bin"`
	Passes    bool    `json:"passes"` // improvement > 0 and its p5 > 0
}

// verdict is the pass rule applied.
type verdict struct {
	Overall     figures      `json:"overall"`
	OverallP5   float64      `json:"overall_improvement_p5"`
	OverallOK   bool         `json:"overall_passes"`
	Bins        []binFigures `json:"bins"`
	BinsPassing int          `json:"bins_passing"`
	Useful      bool         `json:"useful"`
	Resamples   int          `json:"bootstrap_resamples"`
	Seed        uint64       `json:"bootstrap_seed"`
	Clusters    int          `json:"close_times"`
}

// judge computes the figures and the pass rule on TEST's scored observations.
func judge(obs []scored) verdict {
	v := verdict{Overall: figuresOf(obs, func(scored) bool { return true }), Resamples: bootResamples, Seed: bootSeed}
	overall, perBin, clusters := bootstrap(obs)
	v.Clusters = clusters
	v.OverallP5 = percentile(overall, bootPercent)
	v.OverallOK = v.Overall.Improvement > 0 && v.OverallP5 > 0
	for k := range bins {
		k := k
		f := figuresOf(obs, func(s scored) bool { return s.bin == k })
		b := binFigures{Bin: binLabels[k], Figures: f, P5: percentile(perBin[k], bootPercent), Resamples: len(perBin[k])}
		b.Passes = f.N > 0 && f.Improvement > 0 && b.P5 > 0
		if b.Passes {
			v.BinsPassing++
		}
		v.Bins = append(v.Bins, b)
	}
	v.Useful = v.OverallOK && v.BinsPassing >= binsNeeded
	return v
}

// bootstrap draws the close times with replacement, each bringing all of its observations, and
// returns every resample's overall improvement and, per bin, the improvement of each resample
// that holds the bin.
func bootstrap(obs []scored) (overall []float64, perBin [][]float64, clusters int) {
	type cluster struct {
		sum  float64
		n    int
		sumB []float64
		nB   []int
	}
	byClose := map[time.Time]*cluster{}
	for _, s := range obs {
		c := byClose[s.closes]
		if c == nil {
			c = &cluster{sumB: make([]float64, len(bins)), nB: make([]int, len(bins))}
			byClose[s.closes] = c
		}
		d := logLoss(s.mid, s.y) - logLoss(s.cal, s.y)
		c.sum, c.n = c.sum+d, c.n+1
		c.sumB[s.bin], c.nB[s.bin] = c.sumB[s.bin]+d, c.nB[s.bin]+1
	}
	keys := make([]time.Time, 0, len(byClose))
	for k := range byClose {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) }) // the draw order is fixed
	list := make([]*cluster, len(keys))
	for i, k := range keys {
		list[i] = byClose[k]
	}
	perBin = make([][]float64, len(bins))
	if len(list) == 0 {
		return nil, perBin, 0
	}
	rng := rand.New(rand.NewPCG(bootSeed, bootSeed))
	for r := 0; r < bootResamples; r++ {
		var sum float64
		var n int
		sumB, nB := make([]float64, len(bins)), make([]int, len(bins))
		for range list {
			c := list[rng.IntN(len(list))]
			sum, n = sum+c.sum, n+c.n
			for k := range bins {
				sumB[k], nB[k] = sumB[k]+c.sumB[k], nB[k]+c.nB[k]
			}
		}
		overall = append(overall, sum/float64(n))
		for k := range bins {
			if nB[k] > 0 {
				perBin[k] = append(perBin[k], sumB[k]/float64(nB[k]))
			}
		}
	}
	return overall, perBin, len(list)
}

// percentile is the nearest-rank p-quantile: the value at rank ceil(p·n). NaN for no values.
func percentile(x []float64, p float64) float64 {
	if len(x) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), x...)
	sort.Float64s(s)
	rank := int(math.Ceil(p * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	return s[rank-1]
}
