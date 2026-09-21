package store

import (
	"testing"
	"time"
)

// A restake puts $1,000 in: value and contributed rise together, and nothing was earned. An
// allocation takes money out: both fall together, and nothing was lost.
func TestEarnedIgnoresMoneyMovedInOrOut(t *testing.T) {
	then := ValueSnapshot{ValueCents: 1_380_000, ContributedCents: 1_380_000}
	cases := []struct {
		name string
		now  ValueSnapshot
		want int64
	}{
		{"nothing happened", then, 0},
		{"a plain gain", ValueSnapshot{ValueCents: 1_381_234, ContributedCents: 1_380_000}, 1234},
		{"a loss, then a restake from outside", ValueSnapshot{ValueCents: 1_380_000 - 99_000 + 100_000, ContributedCents: 1_480_000}, -99_000},
		{"a gain, part of it allocated away", ValueSnapshot{ValueCents: 1_380_000 + 5_000 - 2_000, ContributedCents: 1_380_000 - 2_000}, 5_000},
	}
	for _, c := range cases {
		if got := EarnedBetween(then, c.now); got != c.want {
			t.Errorf("%s: earned %d, want %d", c.name, got, c.want)
		}
	}
}

// However long the span, and wherever it falls against the epoch, one snapshot a minute binned
// at BinSeconds never comes back as more than maxPoints points.
func TestBinSecondsBoundsThePoints(t *testing.T) {
	const maxPoints = 299
	for _, span := range []time.Duration{0, time.Minute, time.Hour, 5 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 400 * 24 * time.Hour} {
		w := BinSeconds(span, maxPoints)
		if w < 60 {
			t.Fatalf("span %v: a bin of %d s is narrower than the minute rows are written at", span, w)
		}
		for _, start := range []int64{1_790_000_000, 1_790_000_017, 1_790_003_599} {
			bins := map[int64]bool{}
			for at := start; at <= start+int64(span.Seconds()); at += 60 {
				bins[at/w] = true
			}
			if len(bins) > maxPoints {
				t.Errorf("span %v from %d: %d points with %d s bins, want at most %d", span, start, len(bins), w, maxPoints)
			}
		}
	}
	if w := BinSeconds(24*time.Hour, 300); w > 300 {
		t.Errorf("24H in 300 points: %d s bins throw away more than they need to", w)
	}
}

func TestLifeOf(t *testing.T) {
	for name, want := range map[string]int{
		"kalshi15m2 Scalper v2":               1,
		"kalshi15m2 Scalper v2 life 2":        2,
		"kalshi15m2 Anti Model v2 life 13":    13,
		"KXBTC15M Value v1":                   1,
		"a bucket whose life is not a life x": 1,
	} {
		if got := LifeOf(name); got != want {
			t.Errorf("LifeOf(%q) = %d, want %d", name, got, want)
		}
	}
}

// The three shapes of bucket_event.detail the service writes.
func TestEventNote(t *testing.T) {
	cases := []struct{ kind, detail, want string }{
		{"seeded", `{"cents": 100000}`, "seeded with $1000.00"},
		{"tripped", `{"note": "ran out: $0.41 left after 12 rounds"}`, "ran out: $0.41 left after 12 rounds"},
		{"allocated", `{"winnings": 1250, "replenishment": 500, "tax_reserve": 0, "fee_reserve": 25}`,
			"winnings $12.50, replenishment $5.00, tax reserve $0.00, fee reserve $0.25"},
		{"paused", `{}`, ""},
		{"paused", `not json`, ""},
	}
	for _, c := range cases {
		if got := EventNote(c.kind, []byte(c.detail)); got != c.want {
			t.Errorf("EventNote(%s, %s) = %q, want %q", c.kind, c.detail, got, c.want)
		}
	}
}
