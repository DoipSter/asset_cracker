package main

import (
	"fmt"
	"sort"
	"time"
)

// Section 1 of the protocol: windows, coverage, eligibility and the split, on rows already read.

// The five series the M1 query names. A coin is COVERED in a window only once its snapshots
// carry the forecast (s_c below); at the second amendment that is BTC and ETH.
var symbols = []string{"KXBTC15M", "KXETH15M", "KXSOL15M", "KXXRP15M", "KXDOGE15M"}

const (
	trainWindows    = 480
	embargoWindows  = 24
	testWindows     = 192
	windowSeconds   = 900
	completeAfter   = 15 * time.Minute // a split is complete when its last close is this old
	minObservations = 30               // analysis.MinWindows, inherited: the M2s note
)

// marketRow is one market of one of the five series closing after t0.
type marketRow struct {
	ID       int64
	Symbol   string
	ClosesAt time.Time
	Result   string // "yes", "no", or anything else for "no result yet"
	Scored   bool   // the existence query found a scored row
}

// window is every market sharing one closes_at.
type window struct {
	W        int64
	ClosesAt time.Time
	Markets  map[string]marketRow // by symbol
	Eligible bool
	Reason   string // when not eligible
}

// coverage is s_c per symbol: the at of its first snapshot with the forecast. Absent means the
// coin carries no forecast and is never covered.
type coverage map[string]time.Time

// covered reports the coins covered in a window closing at closesAt: those with s_c before it.
func (c coverage) covered(closesAt time.Time) []string {
	var out []string
	for _, s := range symbols {
		if sc, ok := c[s]; ok && sc.Before(closesAt) {
			out = append(out, s)
		}
	}
	return out
}

// judge groups markets into windows in closes_at order and decides eligibility (section 1):
// a window closes after t0, has a market for each covered coin, and every one has a yes/no
// result and a scored row. Windows whose close is not yet complete (last close under 15 minutes
// old at now) are left out entirely: they cannot be judged yet.
func judge(markets []marketRow, cov coverage, t0, now time.Time) []window {
	byClose := map[int64]*window{}
	for _, m := range markets {
		if !m.ClosesAt.After(t0) || now.Sub(m.ClosesAt) < completeAfter {
			continue
		}
		w := m.ClosesAt.Unix()
		win := byClose[w]
		if win == nil {
			win = &window{W: w, ClosesAt: m.ClosesAt, Markets: map[string]marketRow{}}
			byClose[w] = win
		}
		win.Markets[m.Symbol] = m
	}
	out := make([]window, 0, len(byClose))
	for _, win := range byClose {
		win.Eligible, win.Reason = eligible(*win, cov)
		out = append(out, *win)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].W < out[j].W })
	return out
}

func eligible(win window, cov coverage) (bool, string) {
	coins := cov.covered(win.ClosesAt)
	if len(coins) == 0 {
		return false, "no covered coin"
	}
	for _, s := range coins {
		m, ok := win.Markets[s]
		switch {
		case !ok:
			return false, "missing market: " + s
		case m.Result != "yes" && m.Result != "no":
			return false, "no result: " + s
		case !m.Scored:
			return false, fmt.Sprintf("no scored row: %s", s)
		}
	}
	return true, ""
}

// split is what the walk decided.
type split struct {
	Train       []int64          // the 480 window keys, in order
	Ineligible  map[int64]string // window key -> reason, among the windows walked
	Complete    bool             // TRAIN has its 480th window and it is complete
	Walked      int              // windows judged
	EmbargoEnd  int64            // TRAIN's last close + 24 (or 48) windows, as epoch seconds
	FirstUnix   float64          // $1 of the M1 query: max(t0, first close - 900)
	LastUnix    float64          // $2 of the M1 query: the last close
	CoveredAtW  map[int64][]string
	FirstClose  time.Time
	LastClose   time.Time
	TrainLastAt time.Time
}

// walkTrain takes the first 480 eligible windows in closes_at order (section 1, "Order of work").
func walkTrain(wins []window, cov coverage, t0 time.Time) split {
	s := split{Ineligible: map[int64]string{}, CoveredAtW: map[int64][]string{}}
	for _, w := range wins {
		if len(s.Train) == trainWindows {
			break
		}
		s.Walked++
		if !w.Eligible {
			s.Ineligible[w.W] = w.Reason
			continue
		}
		if len(s.Train) == 0 {
			s.FirstClose = w.ClosesAt
		}
		s.Train = append(s.Train, w.W)
		s.CoveredAtW[w.W] = cov.covered(w.ClosesAt)
		s.LastClose = w.ClosesAt
	}
	s.Complete = len(s.Train) == trainWindows
	if len(s.Train) > 0 {
		first := s.FirstClose.Add(-windowSeconds * time.Second)
		if first.Before(t0) {
			first = t0
		}
		s.FirstUnix = float64(first.UnixNano()) / 1e9
		s.LastUnix = float64(s.LastClose.UnixNano()) / 1e9
		s.TrainLastAt = s.LastClose
	}
	return s
}

// embargoEnd is TRAIN's last close plus the embargo (5.3 doubles it with the block).
func embargoEnd(trainLast int64, doubled bool) int64 {
	n := int64(embargoWindows)
	if doubled {
		n *= 2
	}
	return trainLast + n*windowSeconds
}

// walkTest takes the first 192 eligible windows closing after BOTH the embargo's end and T_c.
// It reports how many eligible windows there are so far when short.
func walkTest(wins []window, cov coverage, embargoEnd int64, tc time.Time) (keys []int64, ineligible map[int64]string, coveredAt map[int64][]string, count int) {
	ineligible, coveredAt = map[int64]string{}, map[int64][]string{}
	for _, w := range wins {
		if w.W <= embargoEnd || !w.ClosesAt.After(tc) {
			continue
		}
		if len(keys) == testWindows {
			break
		}
		if !w.Eligible {
			ineligible[w.W] = w.Reason
			continue
		}
		keys = append(keys, w.W)
		coveredAt[w.W] = cov.covered(w.ClosesAt)
	}
	return keys, ineligible, coveredAt, len(keys)
}
