package main

import (
	"testing"
	"time"
)

var (
	tT0  = time.Date(2026, 9, 22, 4, 17, 4, 0, time.UTC)
	tNow = tT0.Add(30 * 24 * time.Hour)
)

// A closes_at grid from t0's next quarter hour, n windows long, with BTC and ETH markets.
func grid(n int, mutate func(i int, m *marketRow)) []marketRow {
	first := tT0.Truncate(15 * time.Minute).Add(15 * time.Minute)
	var out []marketRow
	for i := 0; i < n; i++ {
		for j, s := range []string{"KXBTC15M", "KXETH15M"} {
			m := marketRow{ID: int64(i*10 + j), Symbol: s, ClosesAt: first.Add(time.Duration(i) * 15 * time.Minute), Result: "yes", Scored: true}
			if mutate != nil {
				mutate(i, &m)
			}
			out = append(out, m)
		}
	}
	return out
}

var cov = coverage{"KXBTC15M": tT0, "KXETH15M": tT0}

func TestEligibilityReasons(t *testing.T) {
	markets := grid(6, func(i int, m *marketRow) {
		switch {
		case i == 1 && m.Symbol == "KXETH15M":
			m.Result = ""
		case i == 2 && m.Symbol == "KXBTC15M":
			m.Scored = false
		}
	})
	markets = append(markets[:7], markets[8:]...) // window 3 loses its ETH market (index 3*2+1)
	// SOL closes too, with a result: not covered, so it decides nothing.
	markets = append(markets, marketRow{ID: 999, Symbol: "KXSOL15M", ClosesAt: markets[0].ClosesAt, Result: "no"})
	wins := judge(markets, cov, tT0, tNow)
	if len(wins) != 6 {
		t.Fatalf("%d windows", len(wins))
	}
	want := []string{"", "no result: KXETH15M", "no scored row: KXBTC15M", "missing market: KXETH15M", "", ""}
	for i, w := range wins {
		if w.Reason != want[i] || w.Eligible != (want[i] == "") {
			t.Errorf("window %d: eligible %v reason %q, want %q", i, w.Eligible, w.Reason, want[i])
		}
	}
}

func TestWindowsAtOrBeforeT0AndIncompleteOnesAreOut(t *testing.T) {
	markets := grid(3, nil)
	markets = append(markets, marketRow{ID: 1, Symbol: "KXBTC15M", ClosesAt: tT0, Result: "yes", Scored: true})
	now := markets[4].ClosesAt.Add(10 * time.Minute) // the third window closed 10 minutes ago
	wins := judge(markets, cov, tT0, now)
	if len(wins) != 2 {
		t.Fatalf("%d windows; the one at t0 and the one 10 minutes old must be out", len(wins))
	}
}

func TestCoverageByFirstForecast(t *testing.T) {
	c := coverage{"KXBTC15M": tT0, "KXETH15M": tT0.Add(2 * time.Hour)}
	if got := c.covered(tT0.Add(time.Hour)); len(got) != 1 || got[0] != "KXBTC15M" {
		t.Errorf("an hour in: %v", got)
	}
	if got := c.covered(tT0.Add(3 * time.Hour)); len(got) != 2 {
		t.Errorf("three hours in: %v", got)
	}
}

func TestTheSplit(t *testing.T) {
	markets := grid(800, func(i int, m *marketRow) {
		if i == 5 {
			m.Result = "" // one ineligible window inside TRAIN
		}
	})
	wins := judge(markets, cov, tT0, tNow)
	s := walkTrain(wins, cov, tT0)
	if !s.Complete || len(s.Train) != 480 || s.Walked != 481 || len(s.Ineligible) != 1 {
		t.Fatalf("complete %v train %d walked %d ineligible %d", s.Complete, len(s.Train), s.Walked, len(s.Ineligible))
	}
	if s.Train[0] != wins[0].W || s.Train[479] != wins[480].W {
		t.Error("TRAIN is not the first 480 eligible windows in order")
	}
	if s.FirstUnix != float64(tT0.UnixNano())/1e9 {
		t.Errorf("$1 must be t0 when the first close is within 900 s of it: %v", s.FirstUnix)
	}
	end := embargoEnd(s.Train[479], false)
	if end != s.Train[479]+24*900 || embargoEnd(s.Train[479], true) != s.Train[479]+48*900 {
		t.Error("embargo arithmetic")
	}
	tc := time.Unix(s.Train[479], 0).Add(time.Hour) // T_c inside the embargo: TEST waits for the embargo
	keys, _, _, n := walkTest(wins, cov, end, tc)
	if n != 192 || keys[0] != end+900 {
		t.Fatalf("TEST %d windows, first %d, want %d", n, keys[0], end+900)
	}
	tc = time.Unix(end, 0).Add(10 * time.Hour) // T_c after the embargo: TEST waits for T_c
	keys, _, _, _ = walkTest(wins, cov, end, tc)
	if time.Unix(keys[0], 0).Before(tc) || !time.Unix(keys[0], 0).After(tc) {
		t.Fatalf("TEST's first window %v must close after T_c %v", time.Unix(keys[0], 0), tc)
	}
	// Too few windows so far: the count is reported, nothing chosen.
	short := judge(grid(600, nil), cov, tT0, tNow)
	s2 := walkTrain(short, cov, tT0)
	_, _, _, n = walkTest(short, cov, embargoEnd(s2.Train[479], false), tT0)
	if n >= 192 {
		t.Fatalf("600 windows cannot hold TRAIN, the embargo and a full TEST: got %d", n)
	}
	// An incomplete TRAIN freezes nothing.
	s3 := walkTrain(judge(grid(100, nil), cov, tT0, tNow), cov, tT0)
	if s3.Complete || len(s3.Train) != 100 {
		t.Fatalf("complete %v with %d windows", s3.Complete, len(s3.Train))
	}
}
