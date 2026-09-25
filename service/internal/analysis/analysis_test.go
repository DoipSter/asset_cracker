package analysis

import (
	"encoding/json"
	"fmt"
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
		n       int
		t, minT float64
		want    string
	}{{29, 9, 2, "unresolved"}, {30, 1.99, 2, "unresolved"}, {30, -1.99, 2, "unresolved"}, {30, 2, 2, "up"}, {30, -2, 2, "down"}, {500, 0, 2, "unresolved"}, {0, 0, 2, "unresolved"},
		{30, 2.98, 2.9913, "unresolved"}, {30, 2.99, 2.9913, "unresolved"}, {30, 3, 2.9913, "up"}, {30, -3, 2.9913, "down"},
		{500, 99, 0, "unresolved"}, {500, -99, -1, "unresolved"}} { // no threshold: no verdict, whatever t is
		if got := Verdict(Stat{N: c.n, T: c.t}, c.minT, "up", "down"); got != c.want {
			t.Errorf("n=%d t=%v min %v: got %q, want %q", c.n, c.t, c.minT, got, c.want)
		}
	}
}

// Two-sided Bonferroni at 5%. 18 trials: each row is tested at 0.05/18 = 0.002778, which is
// 0.001389 in each tail, and the normal deviate that leaves that much above it is 2.9913.
// 10 cuts: 0.005, 0.0025 a tail, 2.8070. One trial is the plain 1.96, so the convention's 2 stands.
func TestCorrectedT(t *testing.T) {
	for trials, want := range map[int]float64{18: 2.9913, 10: 2.8070, 1: 2, 2: 2.2414, 0: 0, -4: 0} {
		if got := CorrectedT(trials); math.Abs(got-want) > 5e-5 {
			t.Errorf("%d trials: got %v, want %v", trials, got, want)
		}
	}
	// the cut only ever rises with the number of trials, and is a number however many there are
	last := 0.0
	for _, trials := range []int{1, 2, 18, 1000, 1e9, math.MaxInt64} {
		got := CorrectedT(trials)
		if math.IsNaN(got) || math.IsInf(got, 0) || got < last || got < MinAbsT {
			t.Errorf("%d trials: got %v after %v", trials, got, last)
		}
		last = got
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
// 1200 for the 12 it held at settlement: 200 + 1200 - 500 = 900. A bucket that only lost its
// stake: -93. A bucket that held both sides is paid on the one that won: 600 - 310 - 95 = 195.
func TestSettle(t *testing.T) {
	trades := []Trade{
		{OrderID: 1, BucketID: 1, Side: "yes", Qty: 12, CostCents: 385},
		{OrderID: 2, BucketID: 1, Side: "yes", Qty: 3, CostCents: 115},
		{OrderID: 3, BucketID: 1, Sell: true, Side: "yes", Qty: 3, CostCents: 115, PayoutCents: 200},
		{OrderID: 4, BucketID: 2, Side: "no", Qty: 1, CostCents: 93},
		{OrderID: 5, BucketID: 3, Side: "yes", Qty: 6, CostCents: 310},
		{OrderID: 6, BucketID: 3, Side: "no", Qty: 2, CostCents: 95},
	}
	settled := map[Holding]Paid{{1, "yes"}: {12, 1200}, {2, "no"}: {1, 0}, {3, "yes"}: {6, 600}, {3, "no"}: {2, 0}}
	if bad := Reconcile(trades, settled); len(bad) != 0 {
		t.Fatalf("this market's money is all there: %+v", bad)
	}
	f := Settle(Market{ID: 5, Coin: "BTC", Closes: 900, Result: "yes"}, trades, settled)
	// staked is what the BUYS cost (385 + 115 = 500 for bucket 1); orders count the sale too
	if f.Rounds[1] != (BucketRound{PnLCents: 900, Bets: 2, StakedCents: 500, Orders: 3}) || f.Rounds[2] != (BucketRound{PnLCents: -93, Bets: 1, StakedCents: 93, Orders: 1}) ||
		f.Rounds[3] != (BucketRound{PnLCents: 195, Bets: 2, StakedCents: 405, Orders: 2}) || len(f.Sales) != 1 || !f.Sales[0].WithoutDepth {
		t.Fatalf("got %+v", f)
	}
}

func TestHeldAtClose(t *testing.T) {
	held := HeldAtClose([]Trade{
		{BucketID: 1, Side: "yes", Qty: 12}, {BucketID: 1, Side: "yes", Qty: 3}, {BucketID: 1, Sell: true, Side: "yes", Qty: 3}, // 12 left
		{BucketID: 1, Side: "no", Qty: 4}, {BucketID: 1, Sell: true, Side: "no", Qty: 4}, // all sold: not a holding
		{BucketID: 2, Side: "no", Qty: 60},
	})
	if len(held) != 2 || held[Holding{1, "yes"}] != 12 || held[Holding{2, "no"}] != 60 {
		t.Fatalf("got %+v", held)
	}
}

// The defect this guards against: bucket 1 buys 12 yes for 385 cents, holds to the close, yes
// wins and pays 1200. Read after the result is stored and before the settlement rows are, the
// same market says -385 where the truth is +815. It must be held back, not booked.
func TestReconcile(t *testing.T) {
	win := []Trade{{OrderID: 1, BucketID: 1, Side: "yes", Qty: 12, CostCents: 385}}
	lose := []Trade{{OrderID: 2, BucketID: 14, Side: "no", Qty: 60, CostCents: 2400}}
	both := append(append([]Trade{}, win...), lose...)
	sold := []Trade{{OrderID: 3, BucketID: 5, Side: "yes", Qty: 7, CostCents: 300}, {OrderID: 4, BucketID: 5, Sell: true, Side: "yes", Qty: 7, CostCents: 300, PayoutCents: 350}}
	for name, c := range map[string]struct {
		trades  []Trade
		settled map[Holding]Paid
		want    []Mismatch
	}{
		"nobody traded":                     {nil, nil, nil},
		"everything sold before the close":  {sold, nil, nil},
		"held, settlement not yet written":  {win, nil, []Mismatch{{Holding{1, "yes"}, 12, 0}}},
		"held and paid":                     {win, map[Holding]Paid{{1, "yes"}: {12, 1200}}, nil},
		"a losing hold HAS a row, paying 0": {lose, map[Holding]Paid{{14, "no"}: {60, 0}}, nil}, // settlement 174 in the samples
		"a losing hold with no row":         {lose, nil, []Mismatch{{Holding{14, "no"}, 60, 0}}},
		// the two engines commit separately: one engine's rows being there says nothing of the other's
		"one bucket's rows in, another's not": {both, map[Holding]Paid{{1, "yes"}: {12, 1200}}, []Mismatch{{Holding{14, "no"}, 60, 0}}},
		"rows on the wrong side":              {win, map[Holding]Paid{{1, "no"}: {12, 0}}, []Mismatch{{Holding{1, "no"}, 0, 12}, {Holding{1, "yes"}, 12, 0}}},
		"rows for fewer contracts than held":  {win, map[Holding]Paid{{1, "yes"}: {5, 500}}, []Mismatch{{Holding{1, "yes"}, 12, 5}}},
		"a payout nobody's fills account for": {nil, map[Holding]Paid{{9, "yes"}: {3, 300}}, []Mismatch{{Holding{9, "yes"}, 0, 3}}},
		"sold more than was bought":           {sold[1:], nil, []Mismatch{{Holding{5, "yes"}, -7, 0}}},
	} {
		got := Reconcile(c.trades, c.settled)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %+v, want %+v", name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %+v, want %+v", name, got, c.want)
			}
		}
	}
}

// Partly filled and cancelled orders, as the depth-aware broker will write them. Trade.Qty is what
// FILLED (the reader adds up the fill rows), so the figures below are those sums, with what was
// asked for in the comments. Bucket 31 asks for 348 yes and gets 168 (partial), asks for 100
// more and gets none (cancelled: the reader never lists it; it is given here with Qty 0 to show
// that it would count for nothing anyway), sells 168 and only 43 fill (partial), then sells again
// and 25 fill: 168 - 43 - 25 = 100 held to the close. Bucket 32 is filled in two parts and sells
// all of it in two partial sales: nothing held, so it needs no settlement row.
func partialOrders() []Trade {
	return []Trade{
		{OrderID: 1, BucketID: 31, Side: "yes", Qty: 168, CostCents: 1231, DepthPriced: true},                                            // asked 348
		{OrderID: 2, BucketID: 31, Side: "yes", Qty: 0, DepthPriced: true},                                                               // asked 100, cancelled
		{OrderID: 3, BucketID: 31, Sell: true, Side: "yes", Qty: 43, CostCents: 315, PayoutCents: 400, DepthPriced: true},                // asked 168
		{OrderID: 4, BucketID: 31, Sell: true, Side: "yes", Qty: 25, CostCents: 183, PayoutCents: 150, DepthPriced: true},                // asked 125
		{OrderID: 5, BucketID: 32, Side: "no", Qty: 20, CostCents: 900, DepthPriced: true},                                               // asked 50
		{OrderID: 6, BucketID: 32, Side: "no", Qty: 13, CostCents: 610, DepthPriced: true},                                               // asked 30
		{OrderID: 7, BucketID: 32, Sell: true, Side: "no", Qty: 30, CostCents: 1373, PayoutCents: 1500, DepthPriced: true},               // asked 33
		{OrderID: 8, BucketID: 32, Sell: true, Side: "no", Qty: 3, CostCents: 137, PayoutCents: 120, DepthPriced: true},                  // asked 3
		{OrderID: 9, BucketID: 32, Sell: true, Side: "no", Qty: 0, MoneyMissing: true, HasDepth: true, Displayed: 50, DepthPriced: true}, // an exit that found no bid
	}
}

func TestHeldAtClosePartialAndCancelled(t *testing.T) {
	held := HeldAtClose(partialOrders())
	if len(held) != 1 || held[Holding{31, "yes"}] != 100 {
		t.Fatalf("got %+v, want bucket 31 holding 100 yes and nothing else", held)
	}
	// only cancelled orders: not a holding, and not a zero entry either
	if held := HeldAtClose([]Trade{{OrderID: 1, BucketID: 31, Side: "yes", Qty: 0}, {OrderID: 2, BucketID: 31, Sell: true, Side: "yes", Qty: 0}}); len(held) != 0 {
		t.Fatalf("got %+v", held)
	}
}

// The defect S1 guards against: a settlement row covers the contracts that FILLED. Compared with
// the 348 that were asked for, the market would be held back for ever, and its whole window with
// it, for every engine.
func TestReconcilePartialAndCancelled(t *testing.T) {
	trades := partialOrders()
	if bad := Reconcile(trades, map[Holding]Paid{{31, "yes"}: {100, 10000}}); len(bad) != 0 {
		t.Fatalf("the settlement covers exactly what filled and was not sold: %+v", bad)
	}
	if bad := Reconcile(trades, map[Holding]Paid{{31, "yes"}: {100, 0}}); len(bad) != 0 {
		t.Fatalf("a losing hold's row pays 0 and still covers it: %+v", bad)
	}
	// not yet written: held back, and the figure reported is what filled, not what was asked for
	if bad := Reconcile(trades, nil); len(bad) != 1 || bad[0] != (Mismatch{Holding{31, "yes"}, 100, 0}) {
		t.Fatalf("got %+v", bad)
	}
	// a row for the contracts ASKED for is wrong, and is said to be
	if bad := Reconcile(trades, map[Holding]Paid{{31, "yes"}: {348, 34800}}); len(bad) != 1 || bad[0] != (Mismatch{Holding{31, "yes"}, 100, 348}) {
		t.Fatalf("got %+v", bad)
	}
	// a market where every order was cancelled has nothing to reconcile and nothing missing
	none := []Trade{{OrderID: 1, BucketID: 31, Side: "yes", Qty: 0, MoneyMissing: true}}
	if bad := Reconcile(none, nil); len(bad) != 0 {
		t.Fatalf("got %+v", bad)
	}
	if got := MoneyMissing(trades); len(got) != 0 {
		t.Fatalf("an order that filled nothing has no money to be missing: %v", got)
	}
}

// Bucket 31: -1231 + 400 + 150 + 10000 = 9319, ONE bet (the cancelled buy is none). Bucket 32:
// -900 - 610 + 1500 + 120 = 110, two bets.
func TestSettlePartialAndCancelled(t *testing.T) {
	settled := map[Holding]Paid{{31, "yes"}: {100, 10000}}
	f := Settle(Market{ID: 7, Coin: "DOGE", Closes: 900, Result: "yes"}, partialOrders(), settled)
	// staked: 1231 for bucket 31 (the cancelled buy cost nothing); 900 + 610 for bucket 32. Orders: the fills, 3 and 4.
	if len(f.Rounds) != 2 || f.Rounds[31] != (BucketRound{PnLCents: 9319, Bets: 1, StakedCents: 1231, Orders: 3}) || f.Rounds[32] != (BucketRound{PnLCents: 110, Bets: 2, StakedCents: 1510, Orders: 4}) {
		t.Fatalf("got %+v", f.Rounds)
	}
	// a bucket whose only order was cancelled placed no bet and has no row
	f = Settle(Market{ID: 8, Result: "no"}, []Trade{{OrderID: 1, BucketID: 40, Side: "yes", Qty: 0}}, nil)
	if len(f.Rounds) != 0 || len(f.Sales) != 0 {
		t.Fatalf("got %+v", f)
	}
}

// A depth-priced sale is passed through: what filled, as booked, nothing beyond the bid, whether
// or not the best bid alone would have covered it. The older engines' sales beside it are capped
// exactly as before, and their shared displayed size is not touched by it.
func TestPriceSalesDepthPricedPassThrough(t *testing.T) {
	trades := []Trade{
		// 43 shown at the best bid; the broker took 43 there and 57 on the second level: 100 honest contracts
		{OrderID: 1, BucketID: 31, Sell: true, Side: "yes", Qty: 100, Second: 50, CostCents: 733, PayoutCents: 950, HasDepth: true, Displayed: 43, DepthPriced: true},
		// no ladder joined to it at all: still priced, it was capped when it was made
		{OrderID: 2, BucketID: 31, Sell: true, Side: "yes", Qty: 5, Second: 51, CostCents: 40, PayoutCents: 30, DepthPriced: true},
		// an older engine's sale of 100 against the same 43, same bucket and second: capped as ever
		{OrderID: 3, BucketID: 31, Sell: true, Side: "yes", Qty: 100, Second: 50, CostCents: 1000, PayoutCents: 2000, HasDepth: true, Displayed: 43},
	}
	got := PriceSales(trades, "no")
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	if got[0] != (PricedSale{BucketID: 31, Qty: 100, PnLCents: 217, CappedCents: 217}) {
		t.Errorf("depth-priced sale: got %+v", got[0])
	}
	if got[1] != (PricedSale{BucketID: 31, Qty: 5, PnLCents: -10, CappedCents: -10}) {
		t.Errorf("depth-priced sale with no ladder: got %+v", got[1])
	}
	// 43 of 100 fill: 2000 x 0.43 - 1000 = -140; the other 57 ride and yes lost
	if got[2] != (PricedSale{BucketID: 31, Qty: 100, Beyond: 57, PnLCents: 1000, CappedCents: -140}) {
		t.Errorf("older sale: got %+v", got[2])
	}
	// the partial sales of partialOrders come through with their filled quantities; the exit that filled nothing is no sale
	got = PriceSales(partialOrders(), "yes")
	if len(got) != 4 || got[0].Qty != 43 || got[1].Qty != 25 || got[2].Qty != 30 || got[3].Qty != 3 {
		t.Fatalf("got %+v", got)
	}
	for _, p := range got {
		if p.Beyond != 0 || p.WithoutDepth || p.CappedCents != p.PnLCents {
			t.Errorf("got %+v", p)
		}
	}
}

func TestMoneyMissing(t *testing.T) {
	got := MoneyMissing([]Trade{{OrderID: 1, Qty: 5}, {OrderID: 2, Qty: 5, MoneyMissing: true}, {OrderID: 3, Qty: 5, Sell: true, MoneyMissing: true},
		{OrderID: 4, Qty: 0, MoneyMissing: true}}) // filled nothing: there is no money to be missing, and it must not hold the market back
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("got %v", got)
	}
	if got := MoneyMissing(nil); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func score(id int64, coin string, closes int64, band int, n int64, model, market float64) MarketFacts {
	f := MarketFacts{Market: Market{ID: id, Coin: coin, Closes: closes, Result: "yes"}}
	f.Score[band] = BandSum{N: n, Model: model, Market: market}
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

// The scorecard's series is the overall row window by window: its last point is the row, and a
// long history is thinned to the bound with the last window kept. The same for a strategy's
// cumulative P&L. Neither changes a figure.
func TestSeriesEndWhereTheRowsStand(t *testing.T) {
	var facts []MarketFacts
	for i := range 700 {
		facts = append(facts, score(int64(i+1), "BTC", int64(900*(i+1)), 2, 10, 2.0+0.1+0.2*float64(i%2), 2.0))
	}
	sc := Build(Inputs{Facts: facts}).Scorecard
	if len(sc.Series) != SeriesPoints || sc.Series[len(sc.Series)-1][0] != 900*700 || sc.Series[0][0] != 900*2 {
		t.Fatalf("thinned series: %d points, first %v, last %v", len(sc.Series), sc.Series[0], sc.Series[len(sc.Series)-1])
	}
	if last := sc.Series[len(sc.Series)-1]; last[1] != sc.Overall.BrierModel || last[2] != sc.Overall.BrierMarket {
		t.Errorf("the series ends at %v, the row says %v %v", last, sc.Overall.BrierModel, sc.Overall.BrierMarket)
	}
	short := Build(Inputs{Facts: facts[:5]}).Scorecard
	if len(short.Series) != 5 || short.Series[0][0] != 900 {
		t.Errorf("a short history is not thinned: %v", short.Series)
	}
	if got := Build(Inputs{}).Scorecard.Series; got == nil || len(got) != 0 {
		t.Errorf("no facts: %v, want an empty list", got)
	}

	buckets := []Bucket{{ID: 2, VersionID: 10, Strategy: "Scalper", Engine: "v3", World: "real"}}
	rows := Build(Inputs{Facts: []MarketFacts{round1(1, 900, 2, -300, 1), round1(2, 1800, 2, 500, 1), round1(3, 2700, 2, 100, 1)}, Buckets: buckets, MarketsSettled: 3, Trials: 18}).Leaderboard.Rows
	if want := [][2]int64{{900, -300}, {1800, 200}, {2700, 300}}; len(rows) != 1 || fmt.Sprint(rows[0].Series) != fmt.Sprint(want) || rows[0].LifetimePnLCents != 300 {
		t.Errorf("cumulative P&L: %v (lifetime %d), want %v", rows[0].Series, rows[0].LifetimePnLCents, want)
	}
	if got := thin([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 4); fmt.Sprint(got) != "[2 5 7 10]" {
		t.Errorf("thin: %v", got)
	}
}

// round1 is one market with one bucket's result on it. The stake is 1000 cents a bet unless a
// test sets it: every test that reads return_per_dollar says what it staked.
func round1(id, closes, bucket, pnl int64, bets int) MarketFacts {
	return MarketFacts{Market: Market{ID: id, Coin: "BTC", Closes: closes}, Rounds: map[int64]BucketRound{bucket: {PnLCents: pnl, Bets: bets, StakedCents: 1000 * int64(bets), Orders: bets}}}
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
	// window 4500 is left out because a market in it has no result yet; every SETTLED market is read
	doc := Build(Inputs{Facts: facts, Buckets: buckets, BookCents: map[int64]int64{2: 101000, 4: 100000}, Incomplete: map[Window]bool{{Closes: 4500}: true}, MarketsSettled: 7, Trials: 18})
	if doc.WindowsRecorded != 4 || doc.Coverage != (Coverage{MarketsSettled: 7, MarketsAggregated: 7, WindowsIncomplete: 1, Complete: true}) {
		t.Errorf("coverage: %d %+v", doc.WindowsRecorded, doc.Coverage)
	}
	rows := map[string]LeaderRow{}
	for _, r := range doc.Leaderboard.Rows {
		rows[r.Strategy+" "+r.World] = r
	}
	s := rows["Scalper real"]
	if s.Lives != 2 || s.LifetimePnLCents != 1000 || s.Bets != 47 || s.Windows != 4 || s.MeanWindowPnLCents != 250 || s.BookCents != 101000 ||
		s.TopWindowShare != 97 || s.Verdict != "unresolved" || len(s.Flags) != 2 || !strings.Contains(s.Flags[0], "life 2") || !strings.Contains(s.Flags[1], "more than the whole result") {
		t.Errorf("Scalper: %+v", s)
	}
	if a := rows["Scalper anti"]; a.Windows != 1 || a.LifetimePnLCents != -500 || a.SECents != 0 || a.T != 0 || a.BookCents != 0 || a.TopWindowShare != 1 ||
		len(a.Flags) != 2 || a.Flags[0] != "ran out and was not staked again" || !strings.Contains(a.Flags[1], "only one window") {
		t.Errorf("twin: %+v", a)
	}
	if l := rows["Late real"]; l.Windows != 0 || l.Bets != 0 || l.T != 0 || l.TopWindowShare != 0 || l.BookCents != 100000 || len(l.Flags) != 0 || l.Verdict != "unresolved" {
		t.Errorf("never bet: %+v", l)
	}
	if doc.Leaderboard.Rows[0].Strategy != "Scalper" || doc.Leaderboard.Rows[0].World != "real" {
		t.Errorf("best lifetime first: %+v", doc.Leaderboard.Rows[0])
	}
}

func TestLeaderboardDominantWindowFlag(t *testing.T) {
	value := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v1", World: "real"}}
	doc := Build(Inputs{Buckets: value, Trials: 18,
		Facts: []MarketFacts{round1(1, 900, 1, 970, 1), round1(2, 1800, 1, 20, 1), round1(3, 2700, 1, 10, 1)}})
	if r := doc.Leaderboard.Rows[0]; r.TopWindowShare != 0.97 || len(r.Flags) != 1 || r.Flags[0] != "one window is 97% of the result" {
		t.Errorf("got %+v", r)
	}
	// The flag is decided on the share AS PUBLISHED. 496 of 1000 is 0.496, shown as 0.5, and a row
	// showing 0.5 beside a stated threshold of 0.5 must carry the flag.
	doc = Build(Inputs{Buckets: value, Trials: 18, Facts: []MarketFacts{round1(1, 900, 1, 496, 1), round1(2, 1800, 1, 300, 1), round1(3, 2700, 1, 204, 1)}})
	if r := doc.Leaderboard.Rows[0]; r.TopWindowShare != 0.5 || len(r.Flags) != 1 || r.Flags[0] != "one window is 50% of the result" {
		t.Errorf("got %+v", r)
	}
}

// forty windows making 100 and 300 cents alternately: mean 200, sd = 100 x sqrt(40/39),
// SE = 100/sqrt(39) = 16.01, t = 12.49. Forty windows and a t like that is "ahead" by any rule.
func steady(bucket int64) []MarketFacts {
	var facts []MarketFacts
	for i := range 40 {
		f := round1(int64(i+1), int64(900*(i+1)), bucket, int64(100+200*(i%2)), 1)
		f.Score[2] = BandSum{N: 10, Model: 2.0 + 0.1 + 0.2*float64(i%2), Market: 2.0} // and a model worse by 0.01 or 0.03: t = 12.49 too
		facts = append(facts, f)
	}
	return facts
}

// While fewer markets are aggregated than are settled the document is part of the history, read
// newest first. It must say so everywhere a reader could look, and give no verdict at all.
func TestPartialCoverageGivesNoVerdict(t *testing.T) {
	buckets := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v2", World: "real"}}
	whole := Build(Inputs{Facts: steady(1), Buckets: buckets, MarketsSettled: 40, Trials: 18})
	if r := whole.Leaderboard.Rows[0]; r.T != 12.49 || r.Verdict != "ahead" || len(r.Flags) != 0 || whole.Scorecard.Overall.Verdict != "model worse" ||
		whole.Scorecard.ByBand[2].Verdict != "model worse" || whole.Scorecard.ByCoin[0].Verdict != "model worse" || !whole.Coverage.Complete {
		t.Fatalf("with everything read the same evidence is a verdict: %+v %+v", r, whole.Scorecard)
	}
	if strings.Contains(whole.Leaderboard.What+whole.Fills.What+whole.Scorecard.What, "artial") {
		t.Errorf("a whole document must not call itself partial")
	}

	doc := Build(Inputs{Facts: steady(1), Buckets: buckets, MarketsSettled: 3360, Unreconciled: 2, Trials: 18})
	if doc.Coverage != (Coverage{MarketsSettled: 3360, MarketsAggregated: 40, MarketsUnreconciled: 2, Complete: false}) {
		t.Errorf("coverage: %+v", doc.Coverage)
	}
	r := doc.Leaderboard.Rows[0]
	if r.Verdict != "unresolved" || r.T != 12.49 || r.LifetimePnLCents != 8000 { // the figures stay; the label goes
		t.Errorf("row: %+v", r)
	}
	if len(r.Flags) != 1 || !strings.HasPrefix(r.Flags[0], "partial: only 40 of 3360 settled markets read (2 held back") {
		t.Errorf("flags: %q", r.Flags)
	}
	rows := append(append([]ScoreRow{doc.Scorecard.Overall}, doc.Scorecard.ByBand...), doc.Scorecard.ByCoin...)
	for _, s := range rows {
		if s.Verdict != "unresolved" {
			t.Errorf("scorecard row %+v", s)
		}
	}
	for _, what := range []string{doc.Scorecard.What, doc.Fills.What, doc.Leaderboard.What} {
		if !strings.HasPrefix(what, "Partial: only 40 of 3360 settled markets read") {
			t.Errorf("what: %q", what)
		}
	}
}

// The leaderboard's threshold rises with the number of versions compared, and the by-band and
// by-coin rows' with their own count of ten. t = 2.5 over 40 windows is a verdict for the one
// hypothesis stated in advance and for nothing else.
func TestVerdictsAreCorrectedForTheNumberOfRows(t *testing.T) {
	// Forty windows built to give a chosen t: x_i = m + d or m - d alternately has mean m,
	// sd = d x sqrt(40/39) and SE = d/sqrt(39), so t = m x sqrt(39)/d. With d = 1000, m = t x 1000/sqrt(39).
	windows := func(bucket int64, tt float64) []MarketFacts {
		var facts []MarketFacts
		m := tt * 1000 / math.Sqrt(39)
		for i := range 40 {
			v := m + 1000*float64(1-2*(i%2))
			f := round1(int64(i+1), int64(900*(i+1)), bucket, 0, 1)
			f.Rounds[bucket] = BucketRound{PnLCents: int64(math.Round(v * 1000)), Bets: 1, StakedCents: 1000, Orders: 1} // in thousandths, so whole cents lose nothing
			f.Score[2] = BandSum{N: 1000, Model: 2000 + v, Market: 2000}
			facts = append(facts, f)
		}
		return facts
	}
	buckets := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v2", World: "real"}}
	for _, c := range []struct {
		t                  float64
		trials             int
		leader, cut, whole string
	}{
		{2.5, 18, "unresolved", "unresolved", "model worse"}, // under 2.9913 and under 2.8070, over 2
		{2.9, 18, "unresolved", "model worse", "model worse"},
		{3.1, 18, "ahead", "model worse", "model worse"},
		{-3.1, 18, "behind", "model better", "model better"},
		{2.5, 1, "ahead", "unresolved", "model worse"},     // a registry of one is one hypothesis
		{9, 0, "unresolved", "model worse", "model worse"}, // the count could not be read: no leaderboard verdict, and the row says why
	} {
		doc := Build(Inputs{Facts: windows(1, c.t), Buckets: buckets, Trials: c.trials})
		r, band, overall := doc.Leaderboard.Rows[0], doc.Scorecard.ByBand[2], doc.Scorecard.Overall
		if r.T != c.t || band.T != c.t || overall.T != c.t {
			t.Fatalf("t=%v: the fixture gives %v, %v, %v", c.t, r.T, band.T, overall.T)
		}
		if r.Verdict != c.leader || band.Verdict != c.cut || doc.Scorecard.ByCoin[0].Verdict != c.cut || overall.Verdict != c.whole {
			t.Errorf("t=%v, %d trials: leaderboard %q, band %q, overall %q", c.t, c.trials, r.Verdict, band.Verdict, overall.Verdict)
		}
		if unknown := len(r.Flags) == 1 && strings.Contains(r.Flags[0], "could not be read"); unknown != (c.trials == 0) {
			t.Errorf("%d trials: flags %q", c.trials, r.Flags)
		}
	}
	cv := Build(Inputs{Trials: 18}).Conventions
	if cv.Trials != 18 || cv.LeaderboardMinAbsT != 2.9913 || cv.ScorecardCuts != 10 || cv.ScorecardCutMinAbsT != 2.807 || cv.MinAbsT != 2 || cv.FamilyAlpha != 0.05 {
		t.Errorf("conventions: %+v", cv)
	}
	if cv := Build(Inputs{}).Conventions; cv.Trials != 0 || cv.LeaderboardMinAbsT != 0 {
		t.Errorf("no count: %+v", cv)
	}
}

// The verdict is decided on t AS PUBLISHED. mean/SE = 1.996 is shown as 2.00, and a row showing
// 2.00 under a stated threshold of 2 must not read "unresolved".
func TestVerdictIsDecidedOnThePublishedT(t *testing.T) {
	var facts []MarketFacts
	m := 1.996 * 1000 / math.Sqrt(39)
	for i := range 40 {
		f := MarketFacts{Market: Market{ID: int64(i + 1), Coin: "BTC", Closes: int64(900 * (i + 1))}}
		f.Score[2] = BandSum{N: 1000, Model: 2000 + m + 1000*float64(1-2*(i%2)), Market: 2000}
		facts = append(facts, f)
	}
	if o := Build(Inputs{Facts: facts}).Scorecard.Overall; o.T != 2 || o.Verdict != "model worse" {
		t.Errorf("got %+v", o)
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
	doc := Build(Inputs{Facts: facts, Buckets: buckets, UnsettledSells: map[int64]int{2: 3, 99: 1}, Incomplete: map[Window]bool{{Closes: 2700}: true}})
	rows := doc.Fills.ByStrategy
	want := FillRow{Strategy: "Scalper", Engine: "v2", World: "real", Sells: 6, SellsPriced: 2, ContractsSold: 400, ContractsBeyondBid: 340, ShareBeyond: 0.85,
		SalePnLCents: 10000, SalePnLCappedCents: -6609, UnsettledSells: 3, SellsWithoutDepth: 1}
	if len(rows) != 2 || rows[0] != want || rows[1].Strategy != "Value" || rows[1].ShareBeyond != 0 {
		t.Errorf("got %+v", rows)
	}
}

// The unsettled map lists every bucket with a bet open in a round with no result, with how many
// of its orders are sales, often 0. A strategy that has only bought must not gain a row: before
// this was fixed, rows came and went with the bets (seen on production on 2026-09-21, when five
// all-zero rows appeared between 14:15 and 14:19 as Model and Value bet on the next round).
func TestABetOpenIsNotASale(t *testing.T) {
	buckets := []Bucket{{ID: 1, VersionID: 10, Strategy: "Scalper", Engine: "v2", World: "real"},
		{ID: 2, VersionID: 11, Strategy: "Model", Engine: "v2", World: "real"}}
	rows := Build(Inputs{Buckets: buckets, UnsettledSells: map[int64]int{1: 2, 2: 0}}).Fills.ByStrategy
	if len(rows) != 1 || rows[0].Strategy != "Scalper" || rows[0].Sells != 2 || rows[0].UnsettledSells != 2 {
		t.Errorf("got %+v", rows)
	}
}

// With nothing at all, and with one window, the document must still be plain JSON: every list
// an array, no null, and nothing json.Marshal refuses (it refuses NaN and Inf).
func TestDocumentIsAlwaysCleanJSON(t *testing.T) {
	one := score(1, "BTC", 900, 1, 3, 0.4, 0.2)
	one.Rounds = map[int64]BucketRound{1: {PnLCents: 0, Bets: 1, StakedCents: 100, Orders: 1}}
	value := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v1", World: "real"}}
	for name, in := range map[string]Inputs{"empty": {}, "one window": {Facts: []MarketFacts{one}, Buckets: value},
		"partial, nothing read yet": {Buckets: value, MarketsSettled: 3360, Trials: 18},
		"partial":                   {Facts: steady(1), Buckets: value, MarketsSettled: 3360, Unreconciled: 5, Trials: 18},
		"absurd counts":             {Facts: steady(1), Buckets: value, MarketsSettled: 40, Unreconciled: -3, Trials: math.MaxInt64},
		"no trial count":            {Facts: steady(1), Buckets: value, MarketsSettled: 40, Trials: -1},
		"whole, gate decided":       {Facts: gated(1), Buckets: value, MarketsSettled: 40, Trials: 18},
		"absurd settings":           {Facts: gated(1), Buckets: value, MarketsSettled: 40, Trials: 18, Gate: GateSettings{math.Inf(1), math.NaN(), -1}}} {
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
