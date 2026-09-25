package main

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// synth makes a span's observations: every 15-minute close, BTC and ETH, one row per bin. The
// truth leans from the mid toward p_model by `lean` on the logit scale, so p_model carries what
// the mid misses when lean > 0. seed fixes everything.
func synth(from time.Time, closes int, lean float64, seed uint64) ([]observation, []float64) {
	rng := rand.New(rand.NewPCG(seed, 7))
	var obs []observation
	var y []float64
	id := int64(seed * 1_000_000)
	for i := 0; i < closes; i++ {
		w := from.Add(time.Duration(i+1) * 15 * time.Minute)
		for ci, coin := range []string{"BTC", "ETH"} {
			truthLogit := rng.NormFloat64() * 1.5 // the round's drift, in logits
			for k := range bins {
				tau := (bins[k][0] + bins[k][1]) / 2
				// p_model sees the round; the mid sees it with noise and a favourite-longshot shrink
				pm := sigmoid(truthLogit + rng.NormFloat64()*0.3)
				q := sigmoid(0.8*truthLogit + rng.NormFloat64()*0.8)
				spread := 0.01 + 0.02*rng.Float64()
				yb, ya := math.Max(0.01, q-spread/2), math.Min(0.99, q+spread/2)
				p := sigmoid((1-lean)*logit((yb+ya)/2) + lean*truthLogit)
				out := 0.0
				if rng.Float64() < p {
					out = 1
				}
				id++
				obs = append(obs, observation{EvaluationID: id, MarketID: int64(i*2+ci) + int64(seed)*100000, Coin: coin, Closes: w,
					At: w.Add(-time.Duration(tau) * time.Second), Bin: k, Tau: tau, YesBid: yb, YesAsk: ya, PModel: pm, VolRatio: 0.5 + rng.Float64()})
				y = append(y, out)
			}
		}
	}
	return obs, y
}

// The fit converges, keeps every column that varies, drops one that does not, and recovers a
// calibrator whose predictions track the truth better than the mid does.
func TestFitRecoversTheTruth(t *testing.T) {
	from := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	obs, y := synth(from, 14*96, 0.7, 1)
	res, err := fitTrain(obs, y)
	if err != nil {
		t.Fatal(err)
	}
	if res.Model.Iterations < 2 || res.Model.Iterations > newtonMaxIt {
		t.Errorf("iterations %d", res.Model.Iterations)
	}
	if len(res.Model.Scaler.Dropped) != 0 || len(res.Model.Scaler.Columns) != 2+4+1+2+1 {
		t.Errorf("columns %v, dropped %v", res.Model.Scaler.Columns, res.Model.Scaler.Dropped)
	}
	if !res.Monotone || res.C <= 0 {
		t.Errorf("b %v c %v monotone %v: p_model carries the truth here", res.B, res.C, res.Monotone)
	}
	if res.InSample.LogCal >= res.InSample.LogMid {
		t.Errorf("in-sample log loss %v not below the mid's %v", res.InSample.LogCal, res.InSample.LogMid)
	}
	for _, c := range res.Coefficients {
		if math.IsNaN(c.Standardised) || !(c.ClusteredSE > 0) {
			t.Errorf("coefficient %+v", c)
		}
	}
	// A coin constant on TRAIN is left out and reported.
	var one []observation
	for _, o := range obs {
		if o.Coin == "BTC" {
			one = append(one, o)
		}
	}
	sc := fitScaler(one)
	if len(sc.Dropped) != 0 { // BTC is the reference: no coin column is made at all
		t.Errorf("dropped %v", sc.Dropped)
	}
	for i := range one {
		one[i].VolRatio = 1
	}
	if sc := fitScaler(one); len(sc.Dropped) != 1 || sc.Dropped[0] != "vol_ratio" {
		t.Errorf("a constant vol_ratio: dropped %v", sc.Dropped)
	}
}

// The penalty is ½·λ·Σβ² on the summed likelihood, the intercept free: at the fit, the gradient
// of the penalised likelihood is zero in every coordinate.
func TestFitSatisfiesThePenalisedScore(t *testing.T) {
	obs, y := synth(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), 200, 0.5, 3)
	sc := fitScaler(obs)
	X, err := sc.matrix(obs)
	if err != nil {
		t.Fatal(err)
	}
	beta, _, err := fitRidge(X, y)
	if err != nil {
		t.Fatal(err)
	}
	g := make([]float64, len(beta))
	for i, x := range X {
		mu := sigmoid(dot(x, beta))
		for j := range g {
			g[j] += x[j] * (y[i] - mu)
		}
	}
	for j := 1; j < len(g); j++ {
		g[j] -= lambdaRidge * beta[j]
	}
	for j, v := range g {
		if math.Abs(v) > 1e-6 {
			t.Errorf("score %d = %v at the fit", j, v)
		}
	}
}
