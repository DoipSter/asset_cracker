package main

import (
	"math"
	"testing"
	"time"
)

// TRAIN is the first 14 complete UTC days after T_c and TEST the next 14; a span runs an hour
// after its last close.
func TestSpans(t *testing.T) {
	tc := time.Date(2026, 9, 25, 8, 41, 45, 0, time.UTC) // the amendment's T_c
	train, test := spans(tc)
	d := func(m time.Month, day int) time.Time { return time.Date(2026, m, day, 0, 0, 0, 0, time.UTC) }
	if !train.From.Equal(d(9, 26)) || !train.To.Equal(d(10, 10)) || !test.From.Equal(d(10, 10)) || !test.To.Equal(d(10, 24)) {
		t.Fatalf("train %v, test %v", train, test)
	}
	if err := train.ready(d(10, 10).Add(59 * time.Minute)); err == nil {
		t.Error("TRAIN ran 59 minutes after its last close")
	}
	if err := train.ready(d(10, 10).Add(time.Hour)); err != nil {
		t.Errorf("TRAIN refused an hour after its last close: %v", err)
	}
}

// Student's t: the values the v3 protocol quotes, and the normal limit.
func TestTQuantile(t *testing.T) {
	for _, c := range []struct {
		p    float64
		df   int
		want float64
	}{{0.95, 19, 1.729}, {0.95, 4, 2.132}, {0.975, 1000000, 1.960}, {0.9875, 55, 2.30}} {
		if got := tQuantile(c.p, c.df); math.Abs(got-c.want) > 0.006 {
			t.Errorf("t(%v, %d) = %.4f, want %.3f", c.p, c.df, got, c.want)
		}
	}
}

// The 5th percentile is the nearest rank: the 100th of 2,000 sorted values.
func TestPercentileNearestRank(t *testing.T) {
	x := make([]float64, 2000)
	for i := range x {
		x[len(x)-1-i] = float64(i + 1) // 2000 down to 1
	}
	if got := percentile(x, 0.05); got != 100 {
		t.Errorf("p5 of 1..2000 = %v, want 100", got)
	}
	if !math.IsNaN(percentile(nil, 0.05)) {
		t.Error("no values must give NaN")
	}
}

// A long shot on either side, the size rule, the fee, and r.
func TestH15Eligible(t *testing.T) {
	w := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	yes := h15Row{MarketID: 1, Closes: w, Result: "no", YesBid: 0.03, YesAsk: 0.05, YesBidSize: 10, NoBidSize: 10} // mid 0.04: the long shot is YES
	o, ok := h15Eligible(yes)
	// The favourite NO at 1 - 0.03 = 0.97; fee ceil(7 x 0.97 x 0.03) = ceil(0.2037) = 1 cent; the
	// long shot lost, so the favourite pays 1: r = 1 - 0.97 - 0.01 = 0.02.
	if !ok || o.LongShot != "yes" || math.Abs(o.C-0.97) > 1e-12 || o.FeeCents != 1 || o.Y != 0 || math.Abs(o.R-0.02) > 1e-12 || math.Abs(o.P-0.04) > 1e-12 {
		t.Fatalf("yes long shot: %+v %v", o, ok)
	}
	no := h15Row{MarketID: 2, Closes: w, Result: "no", YesBid: 0.92, YesAsk: 0.94, YesBidSize: 10, NoBidSize: 10} // mid 0.93: the long shot is NO
	o, ok = h15Eligible(no)
	// The favourite YES at 0.94, fee ceil(7 x 0.94 x 0.06) = ceil(0.3948) = 1 cent; the long shot
	// (NO) won: r = 0 - 0.94 - 0.01 = -0.95.
	if !ok || o.LongShot != "no" || math.Abs(o.C-0.94) > 1e-12 || o.Y != 1 || math.Abs(o.R+0.95) > 1e-12 {
		t.Fatalf("no long shot: %+v %v", o, ok)
	}
	for name, x := range map[string]h15Row{
		"middle":         {YesBid: 0.40, YesAsk: 0.42, YesBidSize: 10, NoBidSize: 10},
		"one-sided":      {YesBid: 0, YesAsk: 0.05, YesBidSize: 10, NoBidSize: 10},
		"crossed":        {YesBid: 0.06, YesAsk: 0.05, YesBidSize: 10, NoBidSize: 10},
		"no size at bid": {YesBid: 0.03, YesAsk: 0.05, YesBidSize: 0.5, NoBidSize: 10},
		"no size at ask": {YesBid: 0.92, YesAsk: 0.94, YesBidSize: 10, NoBidSize: 0},
	} {
		if _, ok := h15Eligible(x); ok {
			t.Errorf("%s was eligible", name)
		}
	}
}

// Windows whose markets do not all have results are left out; the mean over windows, its
// jackknife over 6-hour blocks aligned to 00:00 UTC, and the verdict.
func TestH15Row(t *testing.T) {
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	var rows []h15Row
	var markets []h15Market
	id := int64(0)
	for i := 0; i < 96*4; i++ { // four days of windows, [base, base + 96 h)
		w := base.Add(time.Duration(i) * 15 * time.Minute)
		id++
		// A favourite YES at 0.94 that always wins pays 1 - 0.94 - 0.01 = 0.05 a contract.
		rows = append(rows, h15Row{MarketID: id, Closes: w, Result: "yes", YesBid: 0.92, YesAsk: 0.94, YesBidSize: 5, NoBidSize: 5})
		markets = append(markets, h15Market{MarketID: id, Symbol: "KXBTC15M", Closes: w, Settled: true})
	}
	// One window has a market with no result: it is left out whole.
	gap := base.Add(15 * time.Minute)
	markets = append(markets, h15Market{MarketID: 9999, Symbol: "KXETH15M", Closes: gap, Settled: false})
	rep := h15(rows, markets, "sha")
	if len(rep.WindowsLeftOut) != 1 || !rep.WindowsLeftOut[0].Equal(gap) || len(rep.Windows) != 96*4-1 {
		t.Fatalf("windows %d, left out %v", len(rep.Windows), rep.WindowsLeftOut)
	}
	// Every window returns exactly 0.05: no spread, so the SE is 0, t is no number and no verdict
	// is read from it.
	if math.Abs(rep.MeanR-0.05) > 1e-12 || rep.SE != 0 || !math.IsNaN(rep.T) || rep.Verdict != "not shown" {
		t.Errorf("constant returns: mean %v se %v t %v verdict %q", rep.MeanR, rep.SE, rep.T, rep.Verdict)
	}
	if rep.Blocks != 16 {
		t.Errorf("blocks %d, want 16 six-hour blocks in four days", rep.Blocks)
	}
	// Now a spread: every tenth favourite loses.
	for i := range rows {
		if i%10 == 0 {
			rows[i].Result = "no"
		}
	}
	rep = h15(rows, markets, "sha")
	if rep.SE <= 0 || math.IsNaN(rep.T) || math.Abs(rep.Threshold-tQuantile(0.9875, rep.Blocks-1)) > 1e-12 {
		t.Fatalf("se %v t %v threshold %v", rep.SE, rep.T, rep.Threshold)
	}
	want := "not shown"
	if rep.T >= rep.Threshold {
		want = "profitable"
	}
	if rep.Verdict != want {
		t.Errorf("verdict %q for t %.2f against %.2f", rep.Verdict, rep.T, rep.Threshold)
	}
}
