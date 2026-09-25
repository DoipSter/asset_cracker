package main

import (
	"math"
	"testing"
	"time"
)

// A calibrator trained where p_model carries what the mid misses is useful on a fresh span; one
// that is the mid itself improves nothing and is not. The bootstrap is deterministic.
func TestPassRule(t *testing.T) {
	train, test := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	obs, y := synth(train, 14*96, 0.7, 11)
	res, err := fitTrain(obs, y)
	if err != nil {
		t.Fatal(err)
	}
	tobs, ty := synth(test, 14*96, 0.7, 12)
	out, err := judgeTest(res, tobs, ty, nil, nil, "sha")
	if err != nil {
		t.Fatal(err)
	}
	v := out.Verdict
	if !v.Useful || !v.OverallOK || v.BinsPassing < binsNeeded || out.Answer != "yes" || v.Clusters != 14*96 {
		t.Fatalf("a real improvement was not useful: %+v", v)
	}
	for _, b := range v.Bins {
		if b.Passes && !(b.Figures.Improvement > 0 && b.P5 > 0) {
			t.Errorf("bin %s passes without both: %+v", b.Bin, b)
		}
		if b.Resamples != bootResamples {
			t.Errorf("bin %s held in %d resamples", b.Bin, b.Resamples)
		}
	}
	again, _ := judgeTest(res, tobs, ty, nil, nil, "sha")
	if again.Verdict.OverallP5 != v.OverallP5 {
		t.Error("the bootstrap is not reproducible")
	}

	// p_cal equal to the mid: every improvement is zero, nothing passes.
	var s []scored
	for i, o := range tobs {
		q := mid(o)
		s = append(s, scored{bin: o.Bin, closes: o.Closes, cal: q, mid: q, blend: q, y: ty[i]})
	}
	flat := judge(s)
	if flat.Useful || flat.OverallOK || flat.BinsPassing != 0 || math.Abs(flat.Overall.Improvement) > 1e-15 {
		t.Fatalf("the mid passed against itself: %+v", flat)
	}
}

// The strict reading: an overall pass with fewer than three bins passing on their own intervals
// is not useful.
func TestPassRuleNeedsThreeBins(t *testing.T) {
	base := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	var s []scored
	for i := 0; i < 1000; i++ {
		w := base.Add(time.Duration(i) * 15 * time.Minute)
		for k := range bins {
			y := float64(i % 2)
			q := 0.5
			cal := 0.5
			if k < 2 { // only two bins know the answer
				cal = 0.3 + 0.4*y
			}
			s = append(s, scored{bin: k, closes: w, cal: cal, mid: q, blend: q, y: y})
		}
	}
	v := judge(s)
	if !v.OverallOK || v.BinsPassing != 2 || v.Useful {
		t.Fatalf("overall %v, bins %d, useful %v", v.OverallOK, v.BinsPassing, v.Useful)
	}
}
