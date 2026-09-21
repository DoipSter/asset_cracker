package analysis

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestBandOf(t *testing.T) {
	for tau, want := range map[float64]string{899: "over 10 min", 600: "over 10 min", 599.9: "5 to 10 min", 300: "5 to 10 min", 299: "2 to 5 min",
		120: "2 to 5 min", 119: "1 to 2 min", 60: "1 to 2 min", 59.9: "under 60 s", 0: "under 60 s", -3: "under 60 s"} {
		if got := Bands[BandOf(tau)].Label; got != want {
			t.Errorf("%v s to close: got %q, want %q", tau, got, want)
		}
	}
}

// 1, 2, 3, 4: mean 2.5; squared deviations 2.25 + 0.25 + 0.25 + 2.25 = 5; sd = sqrt(5/3) = 1.290994;
// SE = sd / sqrt(4) = 0.645497; t = 2.5 / 0.645497 = 3.872983.
func TestWindowStatWorked(t *testing.T) {
	s := WindowStat([]float64{1, 2, 3, 4})
	if s.N != 4 || !near(s.Mean, 2.5) || !near(s.SE, math.Sqrt(5.0/3)/2) || !near(s.T, 2.5/(math.Sqrt(5.0/3)/2)) {
		t.Fatalf("got %+v", s)
	}
	if math.Abs(s.SE-0.645497) > 1e-6 || math.Abs(s.T-3.872983) > 1e-6 {
		t.Fatalf("got %+v", s)
	}
}

func TestWindowStatEdges(t *testing.T) {
	if s := WindowStat(nil); s != (Stat{}) {
		t.Errorf("no windows: %+v", s)
	}
	if s := WindowStat([]float64{-42}); s != (Stat{N: 1, Mean: -42}) {
		t.Errorf("one window has a mean and no spread: %+v", s)
	}
	// identical windows: no spread, so t is undefined and is reported as 0, not as infinity
	if s := WindowStat([]float64{0.1, 0.1, (1.0 - 0.6) / 4}); s.SE != 0 || s.T != 0 || !near(s.Mean, 0.1) {
		t.Errorf("identical windows: %+v", s)
	}
	if s := WindowStat([]float64{math.NaN(), math.Inf(1), 3}); math.IsNaN(s.Mean+s.SE+s.T) || math.IsInf(s.Mean+s.SE+s.T, 0) {
		t.Errorf("a bad input reached the output: %+v", s)
	}
}

func TestVerdictRule(t *testing.T) {
	for _, c := range []struct {
		n    int
		t    float64
		want string
	}{{29, 9, "unresolved"}, {30, 1.99, "unresolved"}, {30, -1.99, "unresolved"}, {30, 2, "up"}, {30, -2, "down"}, {500, 0, "unresolved"}, {0, 0, "unresolved"}} {
		if got := Verdict(Stat{N: c.n, T: c.t}, "up", "down"); got != c.want {
			t.Errorf("n=%d t=%v: got %q, want %q", c.n, c.t, got, c.want)
		}
	}
}

func TestTopWindowShare(t *testing.T) {
	for _, c := range []struct {
		per  []float64
		want float64
	}{
		{nil, 0},
		{[]float64{500}, 1},
		{[]float64{970, 20, 10}, 0.97},           // 970 of 1000
		{[]float64{500, -400}, 5},                // the best window is five times the whole result
		{[]float64{-300, -100, 50}, 300.0 / 350}, // a loss: the worst window's share of it
		{[]float64{100, -100}, 0},                // a total of exactly zero has no shares
	} {
		if got := TopWindowShare(c.per); !near(got, c.want) {
			t.Errorf("%v: got %v, want %v", c.per, got, c.want)
		}
	}
}

// The review's case: 348 contracts sold into a displayed bid of 8. Bought for $74.00 all in, sold
// for $170.00 after the fee: the simulator booked +$96.00.
//
//	Only 8 fill: 17000 x 8/348 = 390.80 cents of proceeds. 340 ride to settlement.
//	Their side LOST:  390.80 + 0      - 7400 = -7009.20 -> -7009
//	Their side WON:   390.80 + 34000  - 7400 = 26990.80 -> 26991
func TestPriceSalesBeyondTheBid(t *testing.T) {
	sale := Trade{OrderID: 1, BucketID: 7, Sell: true, Side: "yes", Qty: 348, Second: 100, CostCents: 7400, PayoutCents: 17000, HasDepth: true, Displayed: 8}
	lost := PriceSales([]Trade{sale}, "no")
	if len(lost) != 1 || lost[0].PnLCents != 9600 || lost[0].Beyond != 340 || lost[0].CappedCents != -7009 {
		t.Fatalf("side lost: %+v", lost)
	}
	if won := PriceSales([]Trade{sale}, "yes"); won[0].CappedCents != 26991 {
		t.Fatalf("side won: %+v", won)
	}
}

// Two sales by one bucket in one second share a displayed 6.9 (6 whole contracts): the first, of
// 5, fills; the second, of 4, gets the 1 that is left and 3 ride. Another bucket in the same
// second has the 6 to itself, and so does the same bucket a second later. Side "no" won.
//
//	first:   200 x 5/5 - 150           =  50
//	second:  160 x 1/4 - 120 + 3 x 100 = 220
func TestPriceSalesSharedSize(t *testing.T) {
	trades := []Trade{
		{OrderID: 11, BucketID: 1, Sell: true, Side: "no", Qty: 4, Second: 50, CostCents: 120, PayoutCents: 160, HasDepth: true, Displayed: 6.9},
		{OrderID: 10, BucketID: 1, Sell: true, Side: "no", Qty: 5, Second: 50, CostCents: 150, PayoutCents: 200, HasDepth: true, Displayed: 6.9},
		{OrderID: 12, BucketID: 2, Sell: true, Side: "no", Qty: 6, Second: 50, CostCents: 300, PayoutCents: 240, HasDepth: true, Displayed: 6.9},
		{OrderID: 13, BucketID: 1, Sell: true, Side: "no", Qty: 6, Second: 51, CostCents: 300, PayoutCents: 240, HasDepth: true, Displayed: 6.9},
		{OrderID: 9, BucketID: 1, Side: "no", Qty: 9, Second: 40, CostCents: 270}, // a buy: not a sale
	}
	got := PriceSales(trades, "no")
	want := []PricedSale{
		{BucketID: 1, Qty: 5, Beyond: 0, PnLCents: 50, CappedCents: 50},
		{BucketID: 1, Qty: 4, Beyond: 3, PnLCents: 40, CappedCents: 220},
		{BucketID: 2, Qty: 6, Beyond: 0, PnLCents: -60, CappedCents: -60},
		{BucketID: 1, Qty: 6, Beyond: 0, PnLCents: -60, CappedCents: -60},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sale %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPriceSalesWithoutDepthAndEmptyBook(t *testing.T) {
	got := PriceSales([]Trade{
		{OrderID: 1, BucketID: 1, Sell: true, Side: "yes", Qty: 10, CostCents: 500, PayoutCents: 700},                                // before depth was recorded
		{OrderID: 2, BucketID: 1, Sell: true, Side: "yes", Qty: 10, CostCents: 500, PayoutCents: 700, HasDepth: true, Displayed: 0},  // ladder recorded, and empty
		{OrderID: 3, BucketID: 1, Sell: true, Side: "yes", Qty: 10, CostCents: 500, PayoutCents: 700, HasDepth: true, Displayed: -4}, // nonsense is nothing displayed
	}, "yes")
	if !got[0].WithoutDepth || got[0].CappedCents != 0 || got[0].Beyond != 0 || got[0].PnLCents != 200 {
		t.Errorf("without depth is counted and never priced: %+v", got[0])
	}
	for _, s := range got[1:] { // nothing fills, all ten ride and win: 1000 - 500
		if s.WithoutDepth || s.Beyond != 10 || s.CappedCents != 500 {
			t.Errorf("empty book: %+v", s)
		}
	}
}

// A bucket buys twice (385 and 115 cents, fees inside), sells one lot early for 200 and is paid
// 1000 at settlement: 200 + 1000 - 500 = 700. A bucket that only lost its stake: -93.
func TestSettle(t *testing.T) {
	f := Settle(Market{ID: 5, Coin: "BTC", Closes: 900, Result: "yes"}, []Trade{
		{OrderID: 1, BucketID: 1, Side: "yes", Qty: 12, CostCents: 385},
		{OrderID: 2, BucketID: 1, Side: "yes", Qty: 3, CostCents: 115},
		{OrderID: 3, BucketID: 1, Sell: true, Side: "yes", Qty: 3, CostCents: 115, PayoutCents: 200},
		{OrderID: 4, BucketID: 2, Side: "no", Qty: 1, CostCents: 93},
	}, map[int64]int64{1: 1000, 2: 0})
	if f.Rounds[1] != (BucketRound{700, 2}) || f.Rounds[2] != (BucketRound{-93, 1}) || len(f.Sales) != 1 || !f.Sales[0].WithoutDepth {
		t.Fatalf("got %+v", f)
	}
}

func score(id int64, coin string, closes int64, band int, n int64, model, market float64) MarketFacts {
	f := MarketFacts{Market: Market{ID: id, Coin: coin, Closes: closes, Result: "yes"}}
	f.Score[band] = BandSum{n, model, market}
	return f
}

// Window 900:  BTC 2 rows (model 0.5, market 0.3 summed squared error), ETH 2 rows late (0.1, 0.3).
//
//	the window: model 0.6/4 = 0.15, market 0.6/4 = 0.15, diff 0.
//
// Window 1800: BTC 4 rows (1.0, 0.6): model 0.25, market 0.15, diff 0.10.
// Overall: 8 rows in 2 windows; model (0.15+0.25)/2 = 0.20, market 0.15, diff 0.05;
//
//	sd of {0, 0.1} = 0.070711, SE = sd/sqrt(2) = 0.05, t = 1.00. Eight rows, but n is 2.
func TestScorecardIsClusteredByWindow(t *testing.T) {
	doc := Build(Inputs{Facts: []MarketFacts{score(1, "BTC", 900, 0, 2, 0.5, 0.3), score(2, "ETH", 900, 4, 2, 0.1, 0.3), score(3, "BTC", 1800, 0, 4, 1.0, 0.6)}})
	o := doc.Scorecard.Overall
	if o.NRows != 8 || o.NWindows != 2 || o.BrierModel != 0.2 || o.BrierMarket != 0.15 || o.Diff != 0.05 || o.SE != 0.05 || o.T != 1 || o.Verdict != "unresolved" {
		t.Errorf("overall: %+v", o)
	}
	// "over 10 min": BTC only, diff 0.1 in both windows: a mean with no spread, so no t
	if b := doc.Scorecard.ByBand[0]; b.Band != "over 10 min" || b.NWindows != 2 || b.Diff != 0.1 || b.SE != 0 || b.T != 0 {
		t.Errorf("band: %+v", b)
	}
	// ETH: one window, (0.1 - 0.3)/2 = -0.1
	if c := doc.Scorecard.ByCoin[1]; c.Coin != "ETH" || c.NWindows != 1 || c.NRows != 2 || c.Diff != -0.1 || c.SE != 0 || c.Verdict != "unresolved" {
		t.Errorf("coin: %+v", c)
	}
	if len(doc.Scorecard.ByBand) != 5 || len(doc.Scorecard.ByCoin) != 5 || doc.Scorecard.ByCoin[4].Coin != "DOGE" || doc.Scorecard.ByCoin[4].NWindows != 0 {
		t.Errorf("every band and coin appears, scored or not: %+v", doc.Scorecard)
	}
}

// Thirty windows of a model worse by 0.01 or 0.03 alternately: mean 0.02, sd = 0.01 x sqrt(30/29),
// SE = 0.01/sqrt(29) = 0.001857, t = 10.77. With 29 of them the same evidence is "unresolved".
func TestScorecardVerdictNeedsThirtyWindows(t *testing.T) {
	var facts []MarketFacts
	for i := range 30 {
		facts = append(facts, score(int64(i+1), "BTC", int64(900*(i+1)), 2, 10, 2.0+0.1+0.2*float64(i%2), 2.0))
	}
	if o := Build(Inputs{Facts: facts}).Scorecard.Overall; o.NWindows != 30 || o.Diff != 0.02 || o.SE != 0.0019 || o.T != 10.77 || o.Verdict != "model worse" {
		t.Errorf("30 windows: %+v", o)
	}
	if o := Build(Inputs{Facts: facts[:29]}).Scorecard.Overall; o.NWindows != 29 || o.Verdict != "unresolved" {
		t.Errorf("29 windows: %+v", o)
	}
}

func round1(id, closes, bucket, pnl int64, bets int) MarketFacts {
	return MarketFacts{Market: Market{ID: id, Coin: "BTC", Closes: closes}, Rounds: map[int64]BucketRound{bucket: {pnl, bets}}}
}

// Scalper v2 ran out once: bucket 1 (frozen, replaced) lost 100000 in window 900; its second life,
// bucket 2, made 97000 in window 1800 across two coins (90000 + 7000: one window, not two), then
// 3000 and 1000. Lifetime -100000 + 97000 + 3000 + 1000 = 1000 over 4 windows, mean 250, and the
// best window is 97 times the whole result. Forget life 1 and it would read +101000.
func TestLeaderboardKeepsEveryLife(t *testing.T) {
	buckets := []Bucket{
		{ID: 1, VersionID: 10, Strategy: "Scalper", Engine: "v2", World: "real", Frozen: true, Replaced: true},
		{ID: 2, VersionID: 10, Strategy: "Scalper", Engine: "v2", World: "real"},
		{ID: 3, VersionID: 11, Strategy: "Scalper", Engine: "v2", World: "anti", Frozen: true}, // a twin that ran out
		{ID: 4, VersionID: 12, Strategy: "Late", Engine: "v2", World: "real"},                  // never bet
	}
	facts := []MarketFacts{round1(1, 900, 1, -100000, 40), round1(2, 1800, 2, 90000, 3), round1(3, 1800, 2, 7000, 1), round1(4, 2700, 2, 3000, 2),
		round1(5, 3600, 2, 1000, 1), round1(6, 3600, 3, -500, 1),
		round1(7, 4500, 2, 999999, 9)} // window 4500 is incomplete: left out everywhere
	doc := Build(Inputs{Facts: facts, Buckets: buckets, BookCents: map[int64]int64{2: 101000, 4: 100000}, Incomplete: map[int64]bool{4500: true}, MarketsSettled: 9})
	if doc.WindowsRecorded != 4 || doc.Coverage.WindowsIncomplete != 1 || doc.Coverage.Complete || doc.Coverage.MarketsAggregated != 7 {
		t.Errorf("coverage: %d %+v", doc.WindowsRecorded, doc.Coverage)
	}
	rows := map[string]LeaderRow{}
	for _, r := range doc.Leaderboard.Rows {
		rows[r.Strategy+" "+r.World] = r
	}
	s := rows["Scalper real"]
	if s.Lives != 2 || s.LifetimePnLCents != 1000 || s.Bets != 47 || s.Windows != 4 || s.MeanWindowPnLCents != 250 || s.EquityCents != 101000 ||
		s.TopWindowShare != 97 || s.Verdict != "unresolved" || len(s.Flags) != 2 || !strings.Contains(s.Flags[0], "life 2") || !strings.Contains(s.Flags[1], "more than the whole result") {
		t.Errorf("Scalper: %+v", s)
	}
	if a := rows["Scalper anti"]; a.Windows != 1 || a.LifetimePnLCents != -500 || a.SECents != 0 || a.T != 0 || a.EquityCents != 0 || a.TopWindowShare != 1 ||
		len(a.Flags) != 2 || a.Flags[0] != "ran out and was not staked again" || !strings.Contains(a.Flags[1], "only one window") {
		t.Errorf("twin: %+v", a)
	}
	if l := rows["Late real"]; l.Windows != 0 || l.Bets != 0 || l.T != 0 || l.TopWindowShare != 0 || l.EquityCents != 100000 || len(l.Flags) != 0 || l.Verdict != "unresolved" {
		t.Errorf("never bet: %+v", l)
	}
	if doc.Leaderboard.Rows[0].Strategy != "Scalper" || doc.Leaderboard.Rows[0].World != "real" {
		t.Errorf("best lifetime first: %+v", doc.Leaderboard.Rows[0])
	}
}

func TestLeaderboardDominantWindowFlag(t *testing.T) {
	doc := Build(Inputs{Buckets: []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v1", World: "real"}},
		Facts: []MarketFacts{round1(1, 900, 1, 970, 1), round1(2, 1800, 1, 20, 1), round1(3, 2700, 1, 10, 1)}})
	if r := doc.Leaderboard.Rows[0]; r.TopWindowShare != 0.97 || len(r.Flags) != 1 || r.Flags[0] != "one window is 97% of the result" {
		t.Errorf("got %+v", r)
	}
}

func TestFillsByStrategy(t *testing.T) {
	buckets := []Bucket{{ID: 1, VersionID: 10, Strategy: "Scalper", Engine: "v2", World: "real", Frozen: true, Replaced: true},
		{ID: 2, VersionID: 10, Strategy: "Scalper", Engine: "v2", World: "real"}, {ID: 3, VersionID: 11, Strategy: "Value", Engine: "v2", World: "real"}}
	facts := []MarketFacts{
		{Market: Market{ID: 1, Closes: 900}, Sales: []PricedSale{{BucketID: 1, Qty: 348, Beyond: 340, PnLCents: 9600, CappedCents: -7009}, {BucketID: 1, Qty: 5, WithoutDepth: true, PnLCents: 77}}},
		{Market: Market{ID: 2, Closes: 1800}, Sales: []PricedSale{{BucketID: 2, Qty: 52, Beyond: 0, PnLCents: 400, CappedCents: 400}, {BucketID: 3, Qty: 4, PnLCents: -10, CappedCents: -10}}},
		{Market: Market{ID: 3, Closes: 2700}, Sales: []PricedSale{{BucketID: 2, Qty: 1000, Beyond: 1000, PnLCents: 5, CappedCents: -5}}}, // incomplete window
	}
	doc := Build(Inputs{Facts: facts, Buckets: buckets, UnsettledSells: map[int64]int{2: 3, 99: 1}, Incomplete: map[int64]bool{2700: true}})
	rows := doc.Fills.ByStrategy
	want := FillRow{Strategy: "Scalper", Engine: "v2", World: "real", Sells: 6, SellsPriced: 2, ContractsSold: 400, ContractsBeyondBid: 340, ShareBeyond: 0.85,
		SalePnLCents: 10000, SalePnLCappedCents: -6609, UnsettledSells: 3, SellsWithoutDepth: 1}
	if len(rows) != 2 || rows[0] != want || rows[1].Strategy != "Value" || rows[1].ShareBeyond != 0 {
		t.Errorf("got %+v", rows)
	}
}

// With nothing at all, and with one window, the document must still be plain JSON: every list
// an array, no null, and nothing json.Marshal refuses (it refuses NaN and Inf).
func TestDocumentIsAlwaysCleanJSON(t *testing.T) {
	one := score(1, "BTC", 900, 1, 3, 0.4, 0.2)
	one.Rounds = map[int64]BucketRound{1: {0, 1}}
	for name, in := range map[string]Inputs{"empty": {}, "one window": {Facts: []MarketFacts{one}, Buckets: []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v1", World: "real"}}}} {
		raw, err := json.Marshal(Build(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s := string(raw); strings.Contains(s, "null") || strings.Contains(s, "NaN") || strings.Contains(s, "Inf") {
			t.Errorf("%s: %s", name, s)
		}
		var back map[string]any
		if err := json.Unmarshal(raw, &back); err != nil || back["simulated"] != true || back["stale"] != false {
			t.Errorf("%s: simulated and stale are always present: %v %s", name, err, raw)
		}
	}
}
