package engine

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// Hourly closes of a coin whose daily variance follows a known HAR process: the fit should
// recover the weights within noise, beat the sixty-day mean out of sample, and the daily
// realised variance should be the sum of the hours.
func synthetic(days int, seed int64) ([]HourlyClose, []float64) {
	rng := rand.New(rand.NewSource(seed))
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	price := 50000.0
	var closes []HourlyClose
	var trueRV []float64
	rv := make([]float64, 0, days)
	base := 0.0004 // (2%)^2 a day
	for d := 0; d < days; d++ {
		// tomorrow's variance from a HAR with weights 0.35, 0.35, 0.25 on a base of 0.05.
		v := base * 0.05
		if len(rv) >= harMonth {
			dd, w, m := harRow(rv, len(rv)-1)
			v += 0.35*dd + 0.35*w + 0.25*m
		} else {
			v = base
		}
		v *= math.Exp(rng.NormFloat64() * 0.3) // day-to-day noise on the level
		trueRV = append(trueRV, v)
		sigmaH := math.Sqrt(v / 24)
		for h := 0; h < 24; h++ {
			price *= math.Exp(rng.NormFloat64() * sigmaH)
			closes = append(closes, HourlyClose{At: start.Add(time.Duration(d*24+h) * time.Hour), Close: price})
		}
		rv = append(rv, v)
	}
	return closes, trueRV
}

func TestDailyRealisedVariance(t *testing.T) {
	closes, _ := synthetic(40, 1)
	rvs := DailyRealisedVariance(closes)
	if len(rvs) != 40 { // the first day has 23 returns (no close before its first hour), still over the floor
		t.Fatalf("days %d", len(rvs))
	}
	for i := 1; i < len(rvs); i++ {
		if !rvs[i].Day.After(rvs[i-1].Day) || rvs[i].RV <= 0 {
			t.Fatalf("day %d: %+v", i, rvs[i])
		}
	}
	// A day with a hole in the recording is dropped, not read as calm.
	holed := append([]HourlyClose(nil), closes[:24*10]...)
	holed = append(holed, closes[24*10+8:]...) // eight hours missing from day 10
	got := DailyRealisedVariance(holed)
	if len(got) != len(rvs)-1 {
		t.Fatalf("a day short of %d hours must be dropped: %d vs %d", minHoursInDay, len(got), len(rvs))
	}
}

func TestHARFitsAndBeatsTheNaiveForecast(t *testing.T) {
	closes, _ := synthetic(600, 7)
	var rv []float64
	for _, d := range DailyRealisedVariance(closes) {
		rv = append(rv, d.RV)
	}
	h, err := FitHAR(rv)
	if err != nil {
		t.Fatal(err)
	}
	if h.N < 500 || h.R2 < 0.15 || h.Bd < 0 || h.Bw < 0 || h.Bm < 0 { // 30% day-to-day noise on the level caps R² near 0.3
		t.Fatalf("fit %+v", h)
	}
	if sum := h.Bd + h.Bw + h.Bm; sum < 0.6 || sum > 1.1 {
		t.Fatalf("the weights should sum near the process's 0.95: %+v", h)
	}
	f, err := h.Forecast(rv)
	if err != nil || !(f > 0) {
		t.Fatalf("forecast %v %v", f, err)
	}
	sigma := SigmaPerSqrtSecond(f)
	if daily := sigma * math.Sqrt(86400); daily < 0.005 || daily > 0.08 {
		t.Fatalf("a daily sigma of %.4f is not a coin's", daily)
	}
	ho, err := HoldoutHAR(rv, 90)
	if err != nil {
		t.Fatal(err)
	}
	if ho.Days != 90 || !(ho.HAR < ho.Naive) {
		t.Fatalf("holdout %+v: the HAR forecast should beat the sixty-day mean on a HAR process", ho)
	}
	if _, err := FitHAR(rv[:50]); err == nil {
		t.Fatal("fifty days must not be enough")
	}
	if _, err := HoldoutHAR(rv[:100], 90); err == nil {
		t.Fatal("a holdout that leaves no training part must be refused")
	}
}

func TestSolve4(t *testing.T) {
	a := [4][4]float64{{4, 1, 0, 0}, {1, 3, 1, 0}, {0, 1, 2, 1}, {0, 0, 1, 5}}
	want := [4]float64{1, -2, 3, 0.5}
	var b [4]float64
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			b[i] += a[i][j] * want[j]
		}
	}
	got, err := solve4(a, b)
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Fatalf("x = %v, want %v", got, want)
		}
	}
	if _, err := solve4([4][4]float64{{1, 1, 0, 0}, {1, 1, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1}}, b); err == nil {
		t.Fatal("a singular system must be refused")
	}
}

// A ladder priced from a known lognormal gives that lognormal back; a ladder with crossed legs
// shows the pair and its fee.
func TestImpliedFromLadderAndCrossings(t *testing.T) {
	sigma, tau, median := 1e-4, 86400.0, 100000.0 // 2.6% a day
	var legs []Leg
	for k := 90000.0; k <= 110000; k += 1000 {
		p := 1 - NormCDF(math.Log(k/median)/(sigma*math.Sqrt(tau)))
		legs = append(legs, Leg{Strike: k, Bid: p - 0.01, Ask: p + 0.01})
	}
	im := ImpliedFromLadder(legs, tau)
	if !im.OK || im.Legs < 10 {
		t.Fatalf("implied %+v", im)
	}
	if math.Abs(im.Sigma/sigma-1) > 0.02 || math.Abs(im.Median/median-1) > 0.002 || im.RMSE > 0.002 {
		t.Fatalf("implied sigma %v (want %v) median %v (want %v) rmse %v", im.Sigma, sigma, im.Median, median, im.RMSE)
	}
	if got := ImpliedFromLadder(legs[:2], tau); got.OK {
		t.Fatal("two legs are not a distribution")
	}
	if got := ImpliedFromLadder([]Leg{{Strike: 1, Bid: 0.2, Ask: 0.22}, {Strike: 2, Bid: 0.5, Ask: 0.52}, {Strike: 3, Bid: 0.8, Ask: 0.82}}, tau); got.OK {
		t.Fatal("prices rising with the strike fit no distribution")
	}
	if len(Crossings(legs)) != 0 {
		t.Fatal("a well-ordered ladder has no crossing")
	}
	// The 95,000 leg's ask (0.30) sits under the 96,000 leg's bid (0.36): a 6c gap; fees at 0.30
	// and 0.36 are ceil(1.47) + ceil(1.61) = 2 + 2 cents; net 2.
	crossed := []Leg{{Ticker: "A", Strike: 95000, Bid: 0.28, Ask: 0.30}, {Ticker: "B", Strike: 96000, Bid: 0.36, Ask: 0.38}, {Ticker: "C", Strike: 97000, Bid: 0.20, Ask: 0.22}}
	xs := Crossings(crossed)
	if len(xs) != 1 || xs[0].Low.Ticker != "A" || xs[0].High.Ticker != "B" || xs[0].GapCents != 6 || xs[0].FeeCents != 4 || xs[0].NetCents != 2 {
		t.Fatalf("crossings %+v", xs)
	}
	for _, p := range []float64{0.001, 0.02425, 0.1, 0.5, 0.9, 0.99} {
		if got := NormCDF(probit(p)); math.Abs(got-p) > 1e-9 {
			t.Fatalf("probit(%v) round-trips to %v", p, got)
		}
	}
}
