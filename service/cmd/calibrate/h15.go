package main

import (
	"math"
	"sort"
	"time"

	"github.com/doipster/asset_cracker/service/internal/broker"
)

// H15's row under Fold ("H15's row [OWNER]"): the long-shot protocol's sections 2 and 3, applied
// to the rounds of its five series closing in TEST. A long shot is a two-sided book with a mid of
// 0.10 or less (the long shot is YES, the favourite NO bought at 1 - yes_bid, the yes bid holding
// a contract) or of 0.90 or more (the long shot is NO, the favourite YES bought at yes_ask, the no
// bid holding a contract). r = (1 - y) - c - f, y = 1 when the long shot won and f Kalshi's fee on
// one contract; a window is a close time whose markets all have results; the t of the mean of the
// windows' R_w is over a block jackknife of 6-hour blocks of close time aligned to 00:00 UTC, and
// "profitable" when t >= t_{0.9875, B-1}.

const (
	h15Blocks    = 6 * time.Hour
	h15Quantile  = 0.9875 // one-sided 1.25%: the four horizons are one family at 5%
	longShotEdge = 0.10
)

type h15Obs struct {
	MarketID int64     `json:"market_id"`
	Closes   time.Time `json:"closes"`
	LongShot string    `json:"long_shot"` // "yes" or "no"
	P        float64   `json:"p"`         // the long shot's price, from the mid
	C        float64   `json:"c"`         // what the favourite cost
	FeeCents int64     `json:"fee_cents"`
	Y        int       `json:"y"` // 1 when the long shot won
	R        float64   `json:"r"`
}

type h15Window struct {
	Closes time.Time `json:"closes"`
	N      int       `json:"n"`
	R      float64   `json:"r_w"`
	D      float64   `json:"d_w"`
}

type h15Report struct {
	Rule              string      `json:"rule"`
	LongShotSHA       string      `json:"longshot_protocol_sha256"`
	Windows           []h15Window `json:"windows"`
	WindowsLeftOut    []time.Time `json:"windows_left_out_a_market_without_a_result"`
	Observations      int         `json:"eligible_observations"`
	RowsRead          int         `json:"rows_read"`
	MeanR             float64     `json:"mean_r_w"`
	SE                float64     `json:"se"`
	T                 float64     `json:"t"`
	Blocks            int         `json:"blocks"`
	Threshold         float64     `json:"t_threshold"`
	Verdict           string      `json:"verdict"` // "profitable" or "not shown"
	MeanD             float64     `json:"secondary_mean_d_w"`
	SED               float64     `json:"secondary_se_d_w"`
	MeanPerWindow     float64     `json:"secondary_observations_per_window"`
	EligibleByWindows []h15Obs    `json:"observations"`
}

// h15Eligible applies section 2's eligibility to one row; ok false leaves it out of the sample.
func h15Eligible(x h15Row) (h15Obs, bool) {
	yb, ya := x.YesBid, x.YesAsk
	if !(0 < yb && yb < ya && ya < 1) {
		return h15Obs{}, false
	}
	m := (yb + ya) / 2
	o := h15Obs{MarketID: x.MarketID, Closes: x.Closes}
	switch {
	case m <= longShotEdge:
		if x.YesBidSize < 1 {
			return h15Obs{}, false
		}
		o.LongShot, o.P, o.C = "yes", m, 1-yb
		if x.Result == "yes" {
			o.Y = 1
		}
	case m >= 1-longShotEdge:
		if x.NoBidSize < 1 {
			return h15Obs{}, false
		}
		o.LongShot, o.P, o.C = "no", 1-m, ya
		if x.Result == "no" {
			o.Y = 1
		}
	default:
		return h15Obs{}, false
	}
	// f = ceil(0.07 · c · (1 - c) · 100) / 100 for one contract: the engines' own rule.
	o.FeeCents = broker.FeeCents(1, broker.Price(math.Round(o.C*10000)))
	o.R = float64(1-o.Y) - o.C - float64(o.FeeCents)/100
	return o, true
}

// h15Row is the report: sample, primary statistic, verdict, secondary figures.
func h15(rows []h15Row, markets []h15Market, longShotSHA string) h15Report {
	rep := h15Report{Rule: "long-shot protocol sections 2-3, H15, on TEST's closes (calibration protocol: Fold)", LongShotSHA: longShotSHA,
		RowsRead: len(rows), Windows: []h15Window{}, WindowsLeftOut: []time.Time{}, EligibleByWindows: []h15Obs{}}
	unsettled := map[time.Time]bool{}
	for _, m := range markets {
		if !m.Settled {
			unsettled[m.Closes] = true
		}
	}
	for w := range unsettled {
		rep.WindowsLeftOut = append(rep.WindowsLeftOut, w)
	}
	sort.Slice(rep.WindowsLeftOut, func(i, j int) bool { return rep.WindowsLeftOut[i].Before(rep.WindowsLeftOut[j]) })

	byW := map[time.Time][]h15Obs{}
	for _, x := range rows {
		if unsettled[x.Closes] {
			continue
		}
		if o, ok := h15Eligible(x); ok {
			byW[x.Closes] = append(byW[x.Closes], o)
			rep.EligibleByWindows = append(rep.EligibleByWindows, o)
		}
	}
	rep.Observations = len(rep.EligibleByWindows)
	keys := make([]time.Time, 0, len(byW))
	for w := range byW {
		keys = append(keys, w)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
	var rs, ds []float64
	var blocks []int64
	for _, w := range keys {
		obs := byW[w]
		var r, d float64
		for _, o := range obs {
			r += o.R
			d += float64(o.Y) - o.P
		}
		n := float64(len(obs))
		rep.Windows = append(rep.Windows, h15Window{Closes: w, N: len(obs), R: r / n, D: d / n})
		rs, ds = append(rs, r/n), append(ds, d/n)
		blocks = append(blocks, w.Unix()/int64(h15Blocks/time.Second)) // aligned to 00:00 UTC
	}
	rep.MeanR, rep.SE, rep.Blocks = meanSE(rs, blocks)
	rep.MeanD, rep.SED, _ = meanSE(ds, blocks)
	// Identical windows leave rounding dust for an SE, not a spread (analysis.WindowRatio's rule):
	// the SE is then 0 and t is no number, so no verdict is read from it.
	if rep.SE <= 1e-12*math.Max(1, math.Abs(rep.MeanR)) {
		rep.SE = 0
	}
	rep.T = math.NaN()
	if rep.SE > 0 {
		rep.T = rep.MeanR / rep.SE
	}
	rep.Threshold = tQuantile(h15Quantile, rep.Blocks-1)
	rep.Verdict = "not shown"
	if !math.IsNaN(rep.T) && !math.IsNaN(rep.Threshold) && rep.T >= rep.Threshold {
		rep.Verdict = "profitable"
	}
	if len(keys) > 0 {
		rep.MeanPerWindow = float64(rep.Observations) / float64(len(keys))
	}
	return rep
}
