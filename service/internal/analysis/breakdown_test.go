package analysis

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

// Four bets in three windows: a roster's two members, two coins, two price bands.
func breakdownBets() []Bet {
	return []Bet{
		// window 900: a Favourite win on BTC at 0.80 and a Late loss on ETH at 0.50
		{Close: 900, Coin: "BTC", Tau: 400, Member: "Favourite", Qty: 10, CostCents: 800, FeeCents: 14, PnLCents: 186, Won: true, SideMid: f64(0.795), QuotedSpread: f64(0.01)},
		{Close: 900, Coin: "ETH", Tau: 90, Member: "Late", Qty: 20, CostCents: 1000, FeeCents: 35, PnLCents: -1035, SideMid: f64(0.49), QuotedSpread: f64(0.02)},
		// window 1800: a Favourite loss
		{Close: 1800, Coin: "BTC", Tau: 350, Member: "Favourite", Qty: 10, CostCents: 750, FeeCents: 13, PnLCents: -763, QuotedSpread: f64(0.01)},
		// window 2700: a Late win
		{Close: 2700, Coin: "ETH", Tau: 30, Member: "Late", Qty: 10, CostCents: 450, FeeCents: 17, PnLCents: 533, Won: true, SideMid: f64(0.445)},
	}
}

func TestBreakWindowsAndBets(t *testing.T) {
	b := Break(breakdownBets(), false)
	w := b.Windows
	// windows: 186 - 1035 = -849, then -763, then +533
	if w.N != 3 || w.Mean != round((-849-763+533)/3.0, 1) || w.Median != -763 || w.Min != -849 || w.Max != 533 {
		t.Errorf("windows %+v", w)
	}
	_, sd := meanSD([]float64{-849, -763, 533})
	if w.SD != round(sd, 1) || w.SE != round(sd/math.Sqrt(3), 1) || math.Abs(w.ShareUp-1.0/3) > 1e-3 {
		t.Errorf("spread %+v, sd %v", w, sd)
	}
	// percentile_cont: p25 of (-849, -763, 533) sits halfway between the first two
	if w.P25 != round(-849+0.5*(-763+849), 1) {
		t.Errorf("p25 %v", w.P25)
	}
	s := b.Bets
	if s.N != 4 || s.WinRate != 0.5 || s.PnLCents != 186-1035-763+533 || s.FeesCents != 79 || s.StakedCents != 3000+79 {
		t.Errorf("bets %+v", s)
	}
	if s.AvgPrice != round(3000.0/50/100, 4) || s.AvgWin != round((186+533)/2.0, 1) || s.AvgLoss != round((-1035-763)/2.0, 1) {
		t.Errorf("per bet %+v", s)
	}
}

func TestBreakCuts(t *testing.T) {
	b := Break(breakdownBets(), false)
	if len(b.ByMember) != 2 || b.ByMember[0].Key != "Favourite" || b.ByMember[0].Bets != 2 || b.ByMember[0].PnLCents != 186-763 {
		t.Errorf("by member %+v", b.ByMember)
	}
	// price bands in the attribution's order, empty ones left out: 0.45 and 0.50 are one band, 0.75 and 0.80 two
	var keys []string
	for _, c := range b.ByPrice {
		keys = append(keys, c.Key)
	}
	if strings.Join(keys, "|") != "0.40 to 0.60|0.60 to 0.80|0.80 and above" {
		t.Errorf("price bands %v", keys)
	}
	keys = nil
	for _, c := range b.ByTime {
		keys = append(keys, c.Key)
	}
	if strings.Join(keys, "|") != "5 to 10 min|1 to 2 min|under 60 s" {
		t.Errorf("time bands %v", keys)
	}
	// the ratio estimator by hand for the Late member
	late := b.ByMember[1]
	r := float64(-1035+533) / float64(1035+467)
	d1, d2 := -1035-r*1035, 533-r*467
	if want := round(math.Sqrt(2*(d1*d1+d2*d2))/1502, 4); late.PerDollarSE != want || late.PerDollar != round(r, 4) {
		t.Errorf("late %+v, want se %v", late, want)
	}
	// no member recorded anywhere: no member cut at all
	plain := breakdownBets()
	for i := range plain {
		plain[i].Member = ""
	}
	if got := Break(plain, false).ByMember; got != nil {
		t.Errorf("a version that is not a roster has a member cut: %+v", got)
	}
	// a ladder is cut by hours and days
	ladder := breakdownBets()
	ladder[0].Tau, ladder[1].Tau, ladder[2].Tau, ladder[3].Tau = 2*86400, 7200, 7300, 600
	keys = nil
	for _, c := range Break(ladder, true).ByTime {
		keys = append(keys, c.Key)
	}
	if strings.Join(keys, "|") != "1 day and over|1 to 6 h|under 1 h" {
		t.Errorf("ladder bands %v", keys)
	}
}

func TestBreakCosts(t *testing.T) {
	c := Break(breakdownBets(), false).Costs
	if c.Contracts != 50 || c.BetsWithBook != 3 || c.QuotedSpreadMedian != 0.01 || c.ShareAtOneCent != round(2.0/3, 4) {
		t.Errorf("book %+v", c)
	}
	// over the mid: 800-795, 1000-980, 450-445 cents over 40 contracts that had a mid
	if c.BetsWithMid != 3 || c.OverMidCents != 30 || c.OverMidPerContract != round(30.0/40, 2) {
		t.Errorf("mid %+v", c)
	}
	if c.FeesCents != 79 || c.FeePerContract != round(79.0/50, 2) || c.MeanSecondsToClose != round((400+90+350+30)/4.0, 0) {
		t.Errorf("fees %+v", c)
	}
}

func TestHistogram(t *testing.T) {
	var xs []float64
	for i := 0; i < 200; i++ {
		xs = append(xs, float64((i%21)-10)*100) // -1000 to +1000 cents, evenly
	}
	xs = append(xs, -9000, 12000) // two far windows
	bins := histogramOf(xs)
	total, zeroEdge := 0, false
	for _, b := range bins {
		total += b.N
		zeroEdge = zeroEdge || b.Lo == 0
	}
	if total != len(xs) || !zeroEdge || len(bins) > 23 {
		t.Errorf("%d windows in %d bins, an edge on zero %v", total, len(bins), zeroEdge)
	}
	if !bins[0].OpenBelow || !bins[len(bins)-1].OpenAbove {
		t.Errorf("the far windows are not in open end bars: %+v ... %+v", bins[0], bins[len(bins)-1])
	}
	if one := histogramOf([]float64{250, 250}); len(one) != 1 || one[0].N != 2 {
		t.Errorf("identical windows: %+v", one)
	}
}

// Nothing bet yet: a whole document, every list a list.
func TestBreakEmpty(t *testing.T) {
	raw, _ := json.Marshal(Break(nil, false))
	for _, list := range []string{"histogram", "by_entry_price", "by_coin", "by_time_to_close"} {
		if !strings.Contains(string(raw), `"`+list+`":[]`) {
			t.Errorf("%s is not an empty list: %s", list, raw)
		}
	}
	if strings.Contains(string(raw), "by_member") {
		t.Error("an empty breakdown has a member cut")
	}
}
