package main

import (
	"math"
	"math/rand"
	"testing"
)

// The protocol's quoted t thresholds (4.1, 4.2), each to the three places it prints.
func TestTQuantileMatchesTheProtocol(t *testing.T) {
	for _, c := range []struct {
		df   int
		want float64
	}{{19, 1.729}, {20, 1.725}, {9, 1.833}, {7, 1.895}, {8, 1.860}, {3, 2.353}, {4, 2.132}, {2, 2.920}} {
		got := tQuantile(0.95, c.df)
		if math.Abs(got-c.want) > 0.0005 {
			t.Errorf("t_{0.95,%d} = %.4f, the protocol prints %.3f", c.df, got, c.want)
		}
	}
}

// made-up rows with a known lambda: y is drawn from q + lambda d, so the estimator recovers it.
func synthetic(n int, lambda float64, seed int64) []row {
	rng := rand.New(rand.NewSource(seed))
	rows := make([]row, 0, n)
	for i := 0; i < n; i++ {
		w := int64(1_790_000_000) + 900*int64(i/40) // 40 rows per window
		q := 0.2 + 0.6*rng.Float64()
		d := (rng.Float64() - 0.5) * 0.4
		p := math.Min(0.99, math.Max(0.01, q+d))
		truth := q + lambda*(p-q)
		y := 0
		if rng.Float64() < truth {
			y = 1
		}
		spread := 0.01
		rows = append(rows, row{W: w, Coin: []string{"BTC", "ETH"}[i%2], MarketID: int64(i%2) + 100*int64(i/40), AtUnix: float64(w) - 900 + float64(i%40)*15,
			P: p, Q: q, Y: y, YesAsk: q + spread/2, YesBid: q - spread/2, NoAsk: 1 - q + spread/2, NoBid: 1 - q - spread/2})
	}
	return rows
}

func inA(rows []row) []row {
	var a []row
	for _, r := range rows {
		if r.InA() {
			a = append(a, r)
		}
	}
	return a
}

func TestLambdaHatRecoversAKnownLambdaAndTheIdentityHolds(t *testing.T) {
	for _, lambda := range []float64{0, 0.5, 1} {
		a := inA(synthetic(40*480, lambda, 7))
		l, ok := lambdaHat(a)
		if !ok {
			t.Fatal("no denominator")
		}
		if math.Abs(l-lambda) > 0.08 {
			t.Errorf("lambda %.2f: L_hat %.3f on %d rows", lambda, l, len(a))
		}
		lhs, rhs, residual, tol, pass := selfCheck(a, l)
		if !pass {
			t.Errorf("lambda %.2f: self-check lhs %.12g rhs %.12g residual %.3g tol %.3g", lambda, lhs, rhs, residual, tol)
		}
	}
}

func TestActionableSet(t *testing.T) {
	// favoured yes, clears the ask and the fee: in A
	r := row{P: 0.70, Q: 0.60, YesAsk: 0.61, NoAsk: 0.40}
	if !r.InA() {
		t.Error("0.70 against an ask of 0.61 should be actionable")
	}
	// favoured yes, does not clear the fee: 0.62 - 0.61 - 0.07*0.61*0.39 < 0
	r = row{P: 0.62, Q: 0.60, YesAsk: 0.61, NoAsk: 0.40}
	if r.InA() {
		t.Error("0.62 against 0.61 does not clear the fee")
	}
	// favoured no: p_s = 1 - p = 0.70 against the no ask 0.41
	r = row{P: 0.30, Q: 0.60, YesAsk: 0.61, NoAsk: 0.41}
	if !r.InA() {
		t.Error("the no side at 0.70 against 0.41 should be actionable")
	}
	if (row{P: 0.6, Q: 0.6}).InA() {
		t.Error("d = 0 is never in A")
	}
}

func TestBlocksAndFreezing(t *testing.T) {
	if block(21600*5+900, false) != 5 || block(43200*3+900, true) != 3 {
		t.Error("block arithmetic")
	}
	if lambdaFrozen(0.874, 0.05, 1.729) != 0.78 { // floor(100*(0.874-0.08645))/100 = floor(78.755)/100
		t.Errorf("lambda frozen %.4f", lambdaFrozen(0.874, 0.05, 1.729))
	}
	if lambdaFrozen(0.02, 0.05, 1.729) != 0 || lambdaFrozen(1.2, 0.01, 1.729) != 1 {
		t.Error("clamping")
	}
	if costFrozen(-0.001) != 0 || costFrozen(0.00126) != 0.0013 || costFrozen(math.NaN()) != 0 {
		t.Error("cost freezing")
	}
}

func TestJackknifeMatchesTheClosedFormForAMean(t *testing.T) {
	// For a mean with one observation per block, the delete-one jackknife SE equals the usual
	// standard error of the mean, sqrt(var/n) with the n-1 variance.
	x := []float64{1, 2, 4, 8, 16}
	blocks := []int64{1, 2, 3, 4, 5}
	mean, se, b := meanSE(x, blocks)
	if b != 5 || mean != 6.2 {
		t.Fatalf("mean %v b %d", mean, b)
	}
	var ss float64
	for _, v := range x {
		ss += (v - mean) * (v - mean)
	}
	want := math.Sqrt(ss / 4 / 5)
	if math.Abs(se-want) > 1e-12 {
		t.Errorf("se %.12f want %.12f", se, want)
	}
	if _, se, b := meanSE([]float64{1}, []int64{1}); b != 1 || !math.IsNaN(se) {
		t.Error("one block has no SE")
	}
}

func TestAutocorrelationOfAnIndependentSeriesIsSmallAndAPeriodicOneIsLarge(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	var s series
	for i := 0; i < 480; i++ {
		s.W = append(s.W, 1_790_000_000+900*int64(i))
		s.X = append(s.X, rng.NormFloat64())
	}
	r, se, b, ok := autocorrWithSE(s, 24, false)
	if !ok || math.Abs(r) > 0.15 || b != 20 && b != 21 || se <= 0 {
		t.Errorf("independent: r_24 %.3f se %.3f b %d ok %v", r, se, b, ok)
	}
	for i := range s.X { // a 24-window cycle
		s.X[i] = math.Sin(2 * math.Pi * float64(i) / 24)
	}
	r, _, _, ok = autocorrWithSE(s, 24, false)
	if !ok || r < 0.9 {
		t.Errorf("periodic: r_24 %.3f", r)
	}
	// A missing window breaks its pairs and nothing else.
	s.W = append(s.W[:10], s.W[11:]...)
	s.X = append(s.X[:10], s.X[11:]...)
	if _, ok := autocorr(s, 1, func(int) bool { return true }); !ok {
		t.Error("a gap must not stop the estimate")
	}
}

func TestPearson(t *testing.T) {
	if r, ok := pearson([]float64{1, 0, 1, 1, 0}, []float64{1, 0, 1, 1, 0}); !ok || math.Abs(r-1) > 1e-12 {
		t.Errorf("identical series: %v %v", r, ok)
	}
	if _, ok := pearson([]float64{1, 1, 1}, []float64{0, 1, 0}); ok {
		t.Error("no variance, no correlation")
	}
}
