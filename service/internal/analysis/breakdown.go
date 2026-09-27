package analysis

import (
	"math"
	"sort"
)

// The breakdown is the Evidence page's look inside one strategy version: how its window results
// are spread, what a bet looks like, the same money cut by roster member, entry price, coin and
// time to close, and what entering cost. It describes; it does not test. The leaderboard's
// verdict, on windows and corrected for every version tried, is the test.

// Bet is one bucket's bet on one settled market (store.VersionBet).
type Bet struct {
	Close     int64 // unix seconds: the window it belongs to
	Coin      string
	Tau       float64 // seconds from its first buy to the close
	Member    string  // the roster member that fired it; "" outside a roster
	Qty       int64   // contracts bought
	CostCents float64 // contracts x entry price, fees not inside
	FeeCents  int64
	PnLCents  int64 // fees inside
	Won       bool  // a settlement paid it something
	SideMid   *float64
	// QuotedSpread is the recorded book's ask less bid when it entered, in dollars.
	QuotedSpread *float64
}

// WindowDist is how one value per window is spread: cents, over the windows with a bet.
type WindowDist struct {
	N       int     `json:"n"`
	Mean    float64 `json:"mean_cents"`
	SD      float64 `json:"sd_cents"`
	SE      float64 `json:"se_cents"`
	T       float64 `json:"t"`
	Min     float64 `json:"min_cents"`
	P10     float64 `json:"p10_cents"`
	P25     float64 `json:"p25_cents"`
	Median  float64 `json:"median_cents"`
	P75     float64 `json:"p75_cents"`
	P90     float64 `json:"p90_cents"`
	Max     float64 `json:"max_cents"`
	Skew    float64 `json:"skew"`
	ShareUp float64 `json:"share_up"`
}

// HistBin is one bar of the window histogram: windows whose P&L is at least Lo and below Hi.
// The first bar also holds every window below it (OpenBelow), the last every window above it
// (OpenAbove), so a few far windows do not squeeze the rest into two bars.
type HistBin struct {
	Lo        float64 `json:"lo_cents"`
	Hi        float64 `json:"hi_cents"`
	N         int     `json:"n"`
	OpenBelow bool    `json:"open_below,omitempty"`
	OpenAbove bool    `json:"open_above,omitempty"`
}

// BetStats is what a bet looks like, each bet counted once.
type BetStats struct {
	N           int     `json:"n"`
	WinRate     float64 `json:"win_rate"`
	AvgPrice    float64 `json:"avg_price"` // dollars a contract, fees not inside, weighted by contracts
	AvgWin      float64 `json:"avg_win_cents"`
	AvgLoss     float64 `json:"avg_loss_cents"`
	Mean        float64 `json:"mean_cents"`
	SD          float64 `json:"sd_cents"`
	PnLCents    int64   `json:"pnl_cents"`
	StakedCents int64   `json:"staked_cents"` // what the bets cost, fees inside, as the leaderboard counts it
	PerDollar   float64 `json:"per_dollar"`
	FeesCents   int64   `json:"fees_cents"`
}

// CutRow is the bets that share one key: a member, a price band, a coin, a time band.
// PerDollarSE is the ratio estimator's standard error of PerDollar, bets taken as independent,
// which bets in one window are not: it is a floor on the noise, not the noise.
type CutRow struct {
	Key         string  `json:"key"`
	Bets        int     `json:"bets"`
	WinRate     float64 `json:"win_rate"`
	AvgPrice    float64 `json:"avg_price"`
	PnLCents    int64   `json:"pnl_cents"`
	StakedCents int64   `json:"staked_cents"`
	PerDollar   float64 `json:"per_dollar"`
	PerDollarSE float64 `json:"per_dollar_se"`
	SDCents     float64 `json:"sd_cents"` // per bet
}

// EntryCosts is what entering cost beyond the price: the spread the book showed, what was paid
// over the mid, and the venue's fee.
type EntryCosts struct {
	Contracts          int64   `json:"contracts"`
	BetsWithBook       int     `json:"bets_with_book"`
	QuotedSpreadMean   float64 `json:"quoted_spread_mean"` // dollars
	QuotedSpreadMedian float64 `json:"quoted_spread_median"`
	ShareAtOneCent     float64 `json:"share_at_one_cent"` // of bets with a book: a spread of 1 cent or less
	BetsWithMid        int     `json:"bets_with_mid"`
	OverMidPerContract float64 `json:"over_mid_per_contract_cents"` // average price paid above the side's mid
	OverMidCents       int64   `json:"over_mid_cents"`
	FeePerContract     float64 `json:"fee_per_contract_cents"`
	FeesCents          int64   `json:"fees_cents"`
	MeanSecondsToClose float64 `json:"mean_seconds_to_close"`
}

// Breakdown is the whole look.
type Breakdown struct {
	Windows   WindowDist `json:"windows"`
	Histogram []HistBin  `json:"histogram"`
	Bets      BetStats   `json:"bets"`
	ByMember  []CutRow   `json:"by_member,omitempty"`
	ByPrice   []CutRow   `json:"by_entry_price"`
	ByCoin    []CutRow   `json:"by_coin"`
	ByTime    []CutRow   `json:"by_time_to_close"`
	Costs     EntryCosts `json:"costs"`
	What      string     `json:"what"`
}

// EntryPriceBands are the bands of the first fill's price, as the attribution of 2026-09-23 cut
// them and strategy_exercise's by_entry_price still does, so the two can be read side by side.
var EntryPriceBands = []struct {
	Name string
	Max  float64 // exclusive
}{{"under 0.20", 0.20}, {"0.20 to 0.40", 0.40}, {"0.40 to 0.60", 0.60}, {"0.60 to 0.80", 0.80}, {"0.80 and above", math.Inf(1)}}

// LadderBands cut a ladder leg's time to close, which is hours or days where a round's is seconds.
var LadderBands = []Band{{"1 day and over", 86400}, {"6 to 24 h", 21600}, {"1 to 6 h", 3600}, {"under 1 h", math.Inf(-1)}}

// BreakdownWhat is the method, for the page's fold and for an agent reading the document.
const BreakdownWhat = "Every bet the version made on a market that has settled, every life of it, money from the ledger with fees inside. " +
	"Windows are the independent sample: the spread, the quantiles and t are over one figure per window (a close, both coins added). " +
	"The cuts count bets, and bets in one window are not independent (BTC and ETH move together), so a cut's standard error is a floor " +
	"on its noise. A cut describes where the money came from; it is not a test, and a rule chosen by looking at one is a new version to be " +
	"judged on windows it has not seen. Price is the first fill's, the spread the recorded book's when it entered, the mid the one the order recorded."

// Break computes the breakdown of a version's bets; ladder says which time bands to cut by.
func Break(bets []Bet, ladder bool) Breakdown {
	out := Breakdown{Histogram: []HistBin{}, ByPrice: []CutRow{}, ByCoin: []CutRow{}, ByTime: []CutRow{}, What: BreakdownWhat}
	if len(bets) == 0 {
		return out
	}
	byClose := map[int64]float64{}
	for _, b := range bets {
		byClose[b.Close] += float64(b.PnLCents)
	}
	closes := make([]int64, 0, len(byClose))
	for c := range byClose {
		closes = append(closes, c)
	}
	sort.Slice(closes, func(i, j int) bool { return closes[i] < closes[j] })
	perWindow := make([]float64, len(closes))
	for i, c := range closes {
		perWindow[i] = byClose[c]
	}
	out.Windows = distOf(perWindow)
	out.Histogram = histogramOf(perWindow)
	out.Bets = betStats(bets)

	members := false
	for _, b := range bets {
		members = members || b.Member != ""
	}
	if members {
		out.ByMember = cutBy(bets, func(b Bet) string {
			if b.Member == "" {
				return "(none recorded)"
			}
			return b.Member
		}, nil)
	}
	priceOrder := make([]string, len(EntryPriceBands))
	for i, pb := range EntryPriceBands {
		priceOrder[i] = pb.Name
	}
	out.ByPrice = cutBy(bets, func(b Bet) string { return EntryPriceBands[priceBandOf(entryPrice(b))].Name }, priceOrder)
	out.ByCoin = cutBy(bets, func(b Bet) string { return b.Coin }, nil)
	bands := Bands
	if ladder {
		bands = LadderBands
	}
	timeOrder := make([]string, len(bands))
	for i, tb := range bands {
		timeOrder[i] = tb.Label
	}
	out.ByTime = cutBy(bets, func(b Bet) string { return bands[bandIn(bands, b.Tau)].Label }, timeOrder)
	out.Costs = costsOf(bets)
	return out
}

func entryPrice(b Bet) float64 {
	if b.Qty <= 0 {
		return 0
	}
	return b.CostCents / float64(b.Qty) / 100
}

func priceBandOf(p float64) int {
	for i, b := range EntryPriceBands {
		if p < b.Max {
			return i
		}
	}
	return len(EntryPriceBands) - 1
}

func bandIn(bands []Band, tau float64) int {
	for i, b := range bands {
		if tau >= b.MinTau {
			return i
		}
	}
	return len(bands) - 1
}

// quantile is percentile_cont's: linear between the two nearest of the sorted values.
func quantile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	if lo >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (pos-float64(lo))*(sorted[lo+1]-sorted[lo])
}

func meanSD(xs []float64) (mean, sd float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(ss / float64(len(xs)-1))
}

func distOf(xs []float64) WindowDist {
	n := len(xs)
	d := WindowDist{N: n}
	if n == 0 {
		return d
	}
	mean, sd := meanSD(xs)
	sorted := append([]float64{}, xs...)
	sort.Float64s(sorted)
	up := 0
	var skew float64
	for _, x := range xs {
		if x > 0 {
			up++
		}
		if sd > 0 {
			z := (x - mean) / sd
			skew += z * z * z
		}
	}
	d.Mean, d.SD = round(mean, 1), round(sd, 1)
	if n >= 2 && sd > 0 {
		se := sd / math.Sqrt(float64(n))
		d.SE, d.T = round(se, 1), round(mean/se, 2)
		d.Skew = round(skew/float64(n), 2)
	}
	d.Min, d.Max = sorted[0], sorted[n-1]
	d.P10, d.P25, d.Median = round(quantile(sorted, 0.1), 1), round(quantile(sorted, 0.25), 1), round(quantile(sorted, 0.5), 1)
	d.P75, d.P90 = round(quantile(sorted, 0.75), 1), round(quantile(sorted, 0.9), 1)
	d.ShareUp = round(float64(up)/float64(n), 4)
	return d
}

// histogramOf cuts the windows into bars of a round width (1, 2 or 5 times a power of ten cents,
// one edge on zero). The width is the Freedman-Diaconis rule's, twice the interquartile range
// over the cube root of the count, widened until the bars from the 2nd to the 98th percentile
// number at most 22; the windows beyond those go into the two end bars, which are open.
func histogramOf(xs []float64) []HistBin {
	if len(xs) == 0 {
		return []HistBin{}
	}
	sorted := append([]float64{}, xs...)
	sort.Float64s(sorted)
	lo, hi := quantile(sorted, 0.02), quantile(sorted, 0.98)
	iqr := quantile(sorted, 0.75) - quantile(sorted, 0.25)
	width := niceWidth(2 * iqr / math.Cbrt(float64(len(xs))))
	if iqr <= 0 {
		width = niceWidth((hi - lo) / 10)
	}
	for (hi-lo)/width > 22 {
		width = niceWidth(width * 1.5)
	}
	start := math.Floor(lo/width) * width
	n := int(math.Floor((hi-start)/width)) + 1
	bins := make([]HistBin, n)
	for i := range bins {
		bins[i] = HistBin{Lo: start + float64(i)*width, Hi: start + float64(i+1)*width}
	}
	for _, x := range xs {
		i := int(math.Floor((x - start) / width))
		switch {
		case i < 0:
			bins[0].OpenBelow, i = true, 0
		case i > n-1:
			bins[n-1].OpenAbove, i = true, n-1
		}
		bins[i].N++
	}
	return bins
}

func niceWidth(raw float64) float64 {
	if !(raw > 0) {
		return 100 // every window the same: one dollar wide
	}
	e := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*e >= raw*(1-1e-9) {
			return math.Max(1, m*e)
		}
	}
	return math.Max(1, 10*e)
}

func betStats(bets []Bet) BetStats {
	s := BetStats{N: len(bets)}
	var pnl []float64
	var cost float64
	var qty, wins int64
	var winSum, lossSum float64
	var nWin, nLoss int
	for _, b := range bets {
		pnl = append(pnl, float64(b.PnLCents))
		cost += b.CostCents
		qty += b.Qty
		s.PnLCents += b.PnLCents
		s.FeesCents += b.FeeCents
		if b.Won {
			wins++
		}
		switch {
		case b.PnLCents > 0:
			winSum, nWin = winSum+float64(b.PnLCents), nWin+1
		case b.PnLCents < 0:
			lossSum, nLoss = lossSum+float64(b.PnLCents), nLoss+1
		}
	}
	mean, sd := meanSD(pnl)
	s.Mean, s.SD = round(mean, 1), round(sd, 1)
	s.WinRate = round(float64(wins)/float64(len(bets)), 4)
	if qty > 0 {
		s.AvgPrice = round(cost/float64(qty)/100, 4)
	}
	if nWin > 0 {
		s.AvgWin = round(winSum/float64(nWin), 1)
	}
	if nLoss > 0 {
		s.AvgLoss = round(lossSum/float64(nLoss), 1)
	}
	s.StakedCents = int64(math.Round(cost)) + s.FeesCents
	if s.StakedCents > 0 {
		s.PerDollar = round(float64(s.PnLCents)/float64(s.StakedCents), 4)
	}
	return s
}

// cutBy groups the bets by key, in `order` when one is given (keys with no bets left out) and by
// name otherwise.
func cutBy(bets []Bet, key func(Bet) string, order []string) []CutRow {
	groups := map[string][]Bet{}
	for _, b := range bets {
		k := key(b)
		groups[k] = append(groups[k], b)
	}
	keys := order
	if keys == nil {
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	out := []CutRow{}
	for _, k := range keys {
		g := groups[k]
		if len(g) == 0 {
			continue
		}
		s := betStats(g)
		row := CutRow{Key: k, Bets: s.N, WinRate: s.WinRate, AvgPrice: s.AvgPrice, PnLCents: s.PnLCents, StakedCents: s.StakedCents,
			PerDollar: s.PerDollar, SDCents: s.SD}
		row.PerDollarSE = round(ratioSE(g, s.PerDollar), 4)
		out = append(out, row)
	}
	return out
}

// ratioSE is the standard error of total P&L over total stake, each bet an independent draw: the
// ratio estimator's, sqrt(n/(n-1) x sum((pnl - r x stake)^2)) / sum(stake). 0 below two bets.
func ratioSE(bets []Bet, r float64) float64 {
	n := len(bets)
	if n < 2 {
		return 0
	}
	var stake, ss float64
	for _, b := range bets {
		st := b.CostCents + float64(b.FeeCents)
		stake += st
		d := float64(b.PnLCents) - r*st
		ss += d * d
	}
	if stake <= 0 {
		return 0
	}
	return finite(math.Sqrt(float64(n)/float64(n-1)*ss) / stake)
}

func costsOf(bets []Bet) EntryCosts {
	var c EntryCosts
	var spreads []float64
	var overMid, midQty, tau float64
	at1 := 0
	for _, b := range bets {
		c.Contracts += b.Qty
		c.FeesCents += b.FeeCents
		tau += b.Tau
		if b.QuotedSpread != nil {
			spreads = append(spreads, *b.QuotedSpread)
			if *b.QuotedSpread <= 0.01+1e-9 {
				at1++
			}
		}
		if b.SideMid != nil && b.Qty > 0 {
			c.BetsWithMid++
			overMid += b.CostCents - float64(b.Qty)*(*b.SideMid)*100
			midQty += float64(b.Qty)
		}
	}
	c.BetsWithBook = len(spreads)
	if len(spreads) > 0 {
		mean, _ := meanSD(spreads)
		sort.Float64s(spreads)
		c.QuotedSpreadMean, c.QuotedSpreadMedian = round(mean, 4), round(quantile(spreads, 0.5), 4)
		c.ShareAtOneCent = round(float64(at1)/float64(len(spreads)), 4)
	}
	if midQty > 0 {
		c.OverMidPerContract, c.OverMidCents = round(overMid/midQty, 2), int64(math.Round(overMid))
	}
	if c.Contracts > 0 {
		c.FeePerContract = round(float64(c.FeesCents)/float64(c.Contracts), 2)
	}
	c.MeanSecondsToClose = round(tau/float64(len(bets)), 0)
	return c
}
