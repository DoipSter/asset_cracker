package runner

import "testing"

// The split is integer cents, rounded down, so the parts can never exceed the gain and whatever
// is left over stays in the bucket.
func TestSkimSplitNeverExceedsTheGain(t *testing.T) {
	for _, c := range []struct{ gain, w, r, tx, f int64 }{{4065, 2000, 1000, 2500, 500}, {3, 2000, 1000, 2500, 500}, {1, 10000, 0, 0, 0}, {999999, 3333, 3333, 3333, 1}} {
		parts := []int64{c.gain * c.w / 10000, c.gain * c.r / 10000, c.gain * c.tx / 10000, c.gain * c.f / 10000}
		var sum int64
		for _, p := range parts {
			if p < 0 {
				t.Fatalf("negative part %v for %+v", parts, c)
			}
			sum += p
		}
		if sum > c.gain {
			t.Errorf("parts %v sum to %d, more than the gain %d", parts, sum, c.gain)
		}
	}
}
