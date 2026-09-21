package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// The numbered tests are the plan's (docs/honest-fills-v3.md, section 10, tests 1 to 13). Every
// expected figure is worked out by hand in the comment beside it, in integers:
//
//	premium: qty * P / 100 cents, P in ten-thousandths of a dollar; a buy rounds up, a sell down
//	fee:     numerator 7 * qty * P * (10000 - P); cents = ceil(sum of numerators / 1e8), once per order

// dogeQuotes is a REAL recorded book, copied from db_samples.txt: market 102,
// KXDOGE15M-26SEP210345-45, 2026-09-21 07:41:28Z, the book stored with the second engine's sales
// 549 and 550. yes_levels is 87: the real book is far deeper than the five levels recorded.
const dogeQuotes = `{"no_ask": "0.9000", "no_bid": "0.8800", "no_bids": [["0.8800", "161.00"], ["0.8700", "23.62"], ["0.8600", "28.28"], ["0.8500", "210.84"], ["0.8400", "54.92"]], "yes_ask": "0.1200", "yes_bid": "0.1000", "yes_bids": [["0.1000", "43.00"], ["0.0990", "100.00"], ["0.0930", "1.00"], ["0.0920", "23.00"], ["0.0910", "1.00"]], "no_levels": 146, "yes_levels": 87, "no_ask_size": "43.00", "no_bid_depth": {"1c": 184.62, "3c": 423.74, "5c": 635.66}, "yes_ask_size": "161.00", "yes_bid_depth": {"1c": 170, "3c": 847.93, "5c": 3629.43}}`

// ethEmptyQuotes is the REAL recorded book of evaluation 71830 (market 106, ETH, 07:44:19Z): no
// Yes bids at all.
const ethEmptyQuotes = `{"no_ask": "0.0000", "no_bid": "0.9980", "no_bids": [["0.9980", "2022.00"], ["0.9970", "10.00"], ["0.9960", "124.00"], ["0.9950", "2482.52"], ["0.9940", "10.00"]], "yes_ask": "0.0020", "yes_bid": "0.0000", "yes_bids": [], "no_levels": 230, "yes_levels": 0, "no_ask_size": "0", "no_bid_depth": {"1c": 9969.27, "3c": 14675.52, "5c": 15829.75}, "yes_ask_size": "2022.00", "yes_bid_depth": {"1c": 0, "3c": 0, "5c": 0}}`

const (
	doge = "KXDOGE15M-26SEP210345-45"
	eth  = "KXETH15M-26SEP210345-45"
)

var (
	t0     = time.Date(2026, 9, 21, 7, 41, 28, 0, time.UTC)
	closes = time.Date(2026, 9, 21, 7, 45, 0, 0, time.UTC)
)

func quotes(t *testing.T, text string) kalshi.Quotes {
	t.Helper()
	var q kalshi.Quotes
	if err := json.Unmarshal([]byte(text), &q); err != nil {
		t.Fatal(err)
	}
	return q
}

// book builds quotes from [price, size] rows.
func book(yes, no [][2]string) kalshi.Quotes { return kalshi.Quotes{YesBids: yes, NoBids: no} }

// harness numbers evaluations and client ids so a test reads as a sequence of seconds.
type harness struct {
	t    *testing.T
	p    *Paper
	eval int64
	n    int
}

func newHarness(t *testing.T) *harness { return &harness{t: t, p: NewPaper(5)} }

func (h *harness) observe(ticker string, q kalshi.Quotes) {
	h.eval++
	h.p.ObserveBook(ticker, h.eval, t0.Add(time.Duration(h.eval)*time.Second), closes, q)
}

func (h *harness) order(bucket int64, ticker string, a Action, s Side, qty int, limit Price) Order {
	h.n++
	return Order{ClientID: fmt.Sprintf("v3:%d:%d:%d", bucket, h.eval, h.n), BucketID: bucket, MarketID: 102,
		Ticker: ticker, Action: a, Side: s, Qty: qty, Limit: limit, EvaluationID: h.eval,
		At: t0.Add(time.Duration(h.eval) * time.Second)}
}

func (h *harness) submit(o Order) Report {
	h.t.Helper()
	rep, err := h.p.Submit(context.Background(), o)
	if err != nil {
		h.t.Fatal(err)
	}
	return rep
}

// sell submits and commits a sale of Yes, the common case in these tests.
func (h *harness) sell(bucket int64, ticker string, qty int, limit Price) Report {
	h.t.Helper()
	rep := h.submit(h.order(bucket, ticker, Sell, Yes, qty, limit))
	h.p.Commit(rep.Order.ClientID)
	return rep
}

type wantFill struct {
	level, qty       int
	price            Price
	premium, fee, to int64 // to: the bucket's cash effect
}

func checkFills(t *testing.T, rep Report, want []wantFill) {
	t.Helper()
	if len(rep.Fills) != len(want) {
		t.Fatalf("got %d fills, want %d: %+v", len(rep.Fills), len(want), rep.Fills)
	}
	for i, w := range want {
		f := rep.Fills[i]
		got := wantFill{f.Level, f.Qty, f.Price, f.PremiumCents, f.FeeCents, BucketCents(rep.Order.Action, f)}
		if f.Seq != i+1 || got != w {
			t.Errorf("fill %d: got seq %d %+v, want seq %d %+v", i, f.Seq, got, i+1, w)
		}
	}
}

func checkStatus(t *testing.T, rep Report, status Status, unfilled int, reason string) {
	t.Helper()
	if rep.Status != status || rep.Unfilled != unfilled || rep.Reason != reason {
		t.Errorf("got %s, %d unfilled, %q; want %s, %d, %q", rep.Status, rep.Unfilled, rep.Reason, status, unfilled, reason)
	}
	if !rep.Final || rep.Model != "paper-1" {
		t.Errorf("Final %v Model %q", rep.Final, rep.Model)
	}
}

// transfer is what the store will write for one fill: the three ledger entries. They must sum
// to zero, and a zero entry is left out (the ledger forbids one).
func transfer(a Action, f Fill) (bucket, venue, fees int64) {
	bucket, fees = BucketCents(a, f), f.FeeCents
	if a == Buy {
		return bucket, f.PremiumCents, fees
	}
	return bucket, -f.PremiumCents, fees
}

// ---- Test 1: the worked examples A, B, D, E, F of section 2.4, to the cent ----

func TestExampleA_TheRealSaleReplayed(t *testing.T) {
	// Sell 9 yes, limit 0.0900. Level 0 is 0.1000 x 43: take 9 at 0.1000.
	//   premium  floor(9 * 1000 / 100)            = 90c
	//   fee      7 * 9 * 1000 * 9000 = 567,000,000 -> 5.67c -> ceil 6c
	//   bucket   90 - 6                            = 84c
	// Ledger: bucket +84, venue -90, fees +6.
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	rep := h.sell(7, doge, 9, 900)
	checkStatus(t, rep, Filled, 0, "")
	checkFills(t, rep, []wantFill{{0, 9, 1000, 90, 6, 84}})
	if b, v, f := transfer(Sell, rep.Fills[0]); b != 84 || v != -90 || f != 6 || b+v+f != 0 {
		t.Errorf("ledger: bucket %d venue %d fees %d", b, v, f)
	}
	if got := h.p.Held(7, doge, YesBids, 1000); got != 9 {
		t.Errorf("hold at 0.1000: got %d, want 9", got)
	}
	want := []LevelSeen{{1000, 43, 0, 9}, {990, 100, 0, 0}, {930, 1, 0, 0}, {920, 23, 0, 0}, {910, 1, 0, 0}}
	if !reflect.DeepEqual(rep.Seen, want) {
		t.Errorf("seen: got %+v", rep.Seen)
	}
}

func TestExampleB_ALargeSale(t *testing.T) {
	// Sell 348 yes, limit 0.0900: the size of the review's ETH incident. Every level is taken.
	//
	//   level  bid     take  premium (floor)         numerator            cumulative      ceil  share  to bucket
	//   0      0.1000   43   43000/100   = 430   7*43*1000*9000 = 2,709,000,000    2,709,000,000   28    28    402
	//   1      0.0990  100   99000/100   = 990   7*100*990*9010 = 6,243,930,000    8,952,930,000   90    62    928
	//   2      0.0930    1     930/100   =   9   7*1*930*9070   =    59,045,700    9,011,975,700   91     1      8
	//   3      0.0920   23   21160/100   = 211   7*23*920*9080  = 1,344,929,600   10,356,905,300  104    13    198
	//   4      0.0910    1     910/100   =   9   7*1*910*9090   =    57,903,300   10,414,808,600  105     1      8
	//
	// 168 filled, premium 1649c, fee 105c, 1544c to the bucket in five transfers; 180 unfilled.
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	rep := h.sell(7, doge, 348, 900)
	checkStatus(t, rep, Partial, 180, ReasonNothingMore)
	checkFills(t, rep, []wantFill{
		{0, 43, 1000, 430, 28, 402},
		{1, 100, 990, 990, 62, 928},
		{2, 1, 930, 9, 1, 8},
		{3, 23, 920, 211, 13, 198},
		{4, 1, 910, 9, 1, 8},
	})
	var toBucket, fee int64
	for _, f := range rep.Fills { // five transfers, each balanced
		b, v, fe := transfer(Sell, f)
		if b+v+fe != 0 {
			t.Errorf("transfer %d does not balance", f.Seq)
		}
		toBucket, fee = toBucket+b, fee+fe
	}
	if toBucket != 1544 || fee != 105 {
		t.Errorf("got %dc to the bucket, fee %dc; want 1544c, 105c", toBucket, fee)
	}
}

func TestExampleD_ABuyThatWalks(t *testing.T) {
	// Buy 200 yes, limit 0.1300, max cost 2500c. Filled from no_bids at 1 minus the bid:
	//   level 0  bid 0.8800 -> 0.1200 x 161: premium ceil(193200/100) = 1932c
	//            numerator 7*161*1200*8800 = 11,901,120,000 -> 119.01 -> 120c.  So far 2052c.
	//   level 1  bid 0.8700 -> 0.1300 x 23 (floor of 23.62): premium ceil(29900/100) = 299c
	//            numerator 7*23*1300*8700 = 1,820,910,000; cumulative 13,722,030,000 -> 137.22 -> 138c, share 18c.
	//            So far 1932 + 299 + 138 = 2369c <= 2500c.
	//   level 2  bid 0.8600 -> 0.1400 fails the limit.
	// 184 filled, the bucket pays 2369c, 16 unfilled.
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	o := h.order(7, doge, Buy, Yes, 200, 1300)
	o.MaxCostCents = 2500
	rep := h.submit(o)
	checkStatus(t, rep, Partial, 16, ReasonNothingMore)
	checkFills(t, rep, []wantFill{{0, 161, 1200, 1932, 120, -2052}, {1, 23, 1300, 299, 18, -317}})
	if b, v, f := transfer(Buy, rep.Fills[1]); b != -317 || v != 299 || f != 18 {
		t.Errorf("ledger: bucket %d venue %d fees %d", b, v, f)
	}
}

func TestExampleE_AnExitThatCannotHappen(t *testing.T) {
	// The real ETH book has yes_bids: []. Its depth aggregates are all 0 here, but even a
	// non-zero aggregate has no price and is never filled from (see TestNeverTheAggregates).
	h := newHarness(t)
	h.observe(eth, quotes(t, ethEmptyQuotes))
	rep := h.submit(h.order(7, eth, Sell, Yes, 50, 1))
	checkStatus(t, rep, Cancelled, 50, ReasonNoBids)
	checkFills(t, rep, nil)
}

func TestExampleF_ALevelWorthLessThanItsFee(t *testing.T) {
	// Constructed. Sell 101 yes, limit 0.0001, into [0.0100 x 1] [0.0099 x 100].
	//   level 0  1 at 0.0100: premium floor(100/100) = 1c; numerator 7*1*100*9900 = 6,930,000 -> 0.0693 -> 1c.
	//            The bucket gets 0c: the transfer is venue -1, fees +1, and the bucket entry is left out.
	//   level 1  100 at 0.0099: premium floor(9900/100) = 99c; numerator 7*100*99*9901 = 686,139,300;
	//            cumulative 693,069,300 -> 6.93 -> 7c, share 6c. The bucket gets 93c.
	h := newHarness(t)
	h.observe(doge, book([][2]string{{"0.0100", "1"}, {"0.0099", "100"}}, nil))
	rep := h.sell(7, doge, 101, 1)
	checkStatus(t, rep, Filled, 0, "")
	checkFills(t, rep, []wantFill{{0, 1, 100, 1, 1, 0}, {1, 100, 99, 99, 6, 93}})
	if b, v, f := transfer(Sell, rep.Fills[0]); b != 0 || v != -1 || f != 1 {
		t.Errorf("the first transfer: bucket %d (the entry the store leaves out) venue %d fees %d", b, v, f)
	}
}

// ---- Test 2: example C ----

func TestExampleC_TheSameBookAgain(t *testing.T) {
	h := newHarness(t)
	q := quotes(t, dogeQuotes)
	h.observe(doge, q)
	h.sell(7, doge, 348, 900) // example B: every level held in full: 43, 100, 1, 23, 1

	h.observe(doge, q) // the next second, the same book
	checkStatus(t, h.sell(7, doge, 100, 900), Cancelled, 100, ReasonAlreadyTaken)

	// Level 0 rises to 60: only the excess, 60 - 43 = 17, is new.
	q60 := quotes(t, dogeQuotes)
	q60.YesBids[0][1] = "60.00"
	h.observe(doge, q60)
	rep := h.submit(h.order(7, doge, Sell, Yes, 100, 900))
	checkStatus(t, rep, Partial, 83, ReasonNothingMore)
	if len(rep.Fills) != 1 || rep.Fills[0].Qty != 17 || rep.Fills[0].Level != 0 {
		t.Errorf("got %+v, want 17 at level 0", rep.Fills)
	}
	if err := h.p.Void(rep.Order.ClientID); err != nil { // keep the hold at 43 for the next step
		t.Fatal(err)
	}

	// Level 0 falls to 10: the hold there drops to 10 and 33 are displaced. Every lower shown
	// level is already held in full, so the 33 fall off the visible book, and nothing can fill.
	q10 := quotes(t, dogeQuotes)
	q10.YesBids[0][1] = "10.00"
	h.observe(doge, q10)
	if got := h.p.Held(7, doge, YesBids, 1000); got != 10 {
		t.Errorf("hold at 0.1000 after the fall: got %d, want 10", got)
	}
	if got := h.p.TotalHeld(7, doge, YesBids); got != 10+100+1+23+1 {
		t.Errorf("total held: got %d, want 135 (168 less the 33 that fell off)", got)
	}
	checkStatus(t, h.sell(7, doge, 100, 900), Cancelled, 100, ReasonAlreadyTaken)

	// Level 0 shows 43 again: only the rise, 43 - 10 = 33, can fill.
	h.observe(doge, q)
	rep = h.sell(7, doge, 100, 900)
	checkStatus(t, rep, Partial, 67, ReasonNothingMore)
	if len(rep.Fills) != 1 || rep.Fills[0].Qty != 33 {
		t.Errorf("got %+v, want 33", rep.Fills)
	}
}

// ---- Test 3: displacement (C2) ----

func TestDisplacement(t *testing.T) {
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	h.sell(7, doge, 43, 1000) // hold 43 at 0.1000, nothing at 0.0990

	// A real taker sells 33: the book shows [0.1000 x 10] [0.0990 x 100]. The hold at 0.1000
	// drops to 10 and the 33 move to 0.0990, where 100 - 33 = 67 are available, not 100.
	h.observe(doge, book([][2]string{{"0.1000", "10"}, {"0.0990", "100"}}, nil))
	if a, b := h.p.Held(7, doge, YesBids, 1000), h.p.Held(7, doge, YesBids, 990); a != 10 || b != 33 {
		t.Fatalf("holds: got %d and %d, want 10 and 33", a, b)
	}
	rep := h.sell(7, doge, 500, 900)
	checkStatus(t, rep, Partial, 433, ReasonNothingMore)
	if len(rep.Fills) != 1 || rep.Fills[0].Qty != 67 || rep.Fills[0].Price != 990 {
		t.Fatalf("got %+v, want 67 at 0.0990", rep.Fills)
	}
	// In all the bucket sold 43 + 67 = 110, which is what existed: 143 less the taker's 33.

	// A displaced hold falls and moves again by the same rule. Holds are now 10 and 100.
	// The book shows [0.1000 x 10] [0.0990 x 20] [0.0980 x 5]: 0.0990 keeps 20 and releases 80;
	// 0.0980 has room for 5; the other 75 do not fit on the shown levels and are dropped.
	h.observe(doge, book([][2]string{{"0.1000", "10"}, {"0.0990", "20"}, {"0.0980", "5"}}, nil))
	for bid, want := range map[Price]int{1000: 10, 990: 20, 980: 5} {
		if got := h.p.Held(7, doge, YesBids, bid); got != want {
			t.Errorf("hold at %d: got %d, want %d", bid, got, want)
		}
	}
	if got := h.p.TotalHeld(7, doge, YesBids); got != 35 {
		t.Errorf("total held: got %d, want 35", got)
	}
}

func TestDisplacementNeverMovesUp(t *testing.T) {
	// Holds 10 at 0.0990 and nothing at 0.1000. The display at 0.0990 falls to 4 while 0.1000
	// shows 50 free contracts. The 6 released may only move DOWN: a level's hold rises only by
	// what a BETTER level released. 0.0980 has room for 2, so 4 are dropped.
	h := newHarness(t)
	h.p.RestoreHold(7, doge, Sell, Yes, 990, 10)
	h.observe(doge, book([][2]string{{"0.1000", "50"}, {"0.0990", "4"}, {"0.0980", "2"}}, nil))
	for bid, want := range map[Price]int{1000: 0, 990: 4, 980: 2} {
		if got := h.p.Held(7, doge, YesBids, bid); got != want {
			t.Errorf("hold at %d: got %d, want %d", bid, got, want)
		}
	}
}

// ---- Test 4: hold refresh ----

func TestHoldRefresh(t *testing.T) {
	five := [][2]string{{"0.1000", "50"}, {"0.0990", "50"}, {"0.0920", "50"}, {"0.0910", "50"}, {"0.0900", "50"}}

	t.Run("absent inside the visible range releases", func(t *testing.T) {
		// 0.0930 is above the worst shown bid (0.0900) and not listed: it displays 0. Its 5
		// contracts are displaced to the next shown level down, 0.0920.
		h := newHarness(t)
		h.p.RestoreHold(7, doge, Sell, Yes, 930, 5)
		h.observe(doge, book(five, nil))
		if a, b := h.p.Held(7, doge, YesBids, 930), h.p.Held(7, doge, YesBids, 920); a != 0 || b != 5 {
			t.Errorf("got %d at 0.0930 and %d at 0.0920, want 0 and 5", a, b)
		}
	})
	t.Run("absent below five shown levels does not", func(t *testing.T) {
		h := newHarness(t)
		h.p.RestoreHold(7, doge, Sell, Yes, 890, 5)
		h.observe(doge, book(five, nil))
		if got := h.p.Held(7, doge, YesBids, 890); got != 5 {
			t.Errorf("got %d, want the hold left alone at 5: the level cannot be seen", got)
		}
	})
	t.Run("fewer than five shown means absent is empty", func(t *testing.T) {
		h := newHarness(t)
		h.p.RestoreHold(7, doge, Sell, Yes, 890, 5)
		h.observe(doge, book(five[:3], nil))
		if got := h.p.TotalHeld(7, doge, YesBids); got != 0 {
			t.Errorf("got %d held, want 0: the whole ladder is visible and 0.0890 is not in it", got)
		}
	})
	t.Run("an unreadable side leaves its holds alone and takes no orders", func(t *testing.T) {
		h := newHarness(t)
		h.p.RestoreHold(7, doge, Sell, Yes, 1000, 5)
		h.observe(doge, book([][2]string{{"0.10001", "50"}}, nil)) // finer than a ten-thousandth
		if got := h.p.Held(7, doge, YesBids, 1000); got != 5 {
			t.Errorf("got %d, want 5", got)
		}
		if rep := h.submit(h.order(7, doge, Sell, Yes, 1, 1)); rep.Status != Rejected {
			t.Errorf("got %s, want rejected", rep.Status)
		}
	})
}

// ---- Test 5: whole contracts, five levels, no aggregates ----

func TestFractionalSizesFloor(t *testing.T) {
	// 23.62 displayed is 23 contracts (example D uses the real one). A level of 0.62 fills nothing.
	h := newHarness(t)
	h.observe(doge, book([][2]string{{"0.1000", "0.62"}}, nil))
	checkStatus(t, h.submit(h.order(7, doge, Sell, Yes, 1, 1)), Cancelled, 1, ReasonNothingMore)

	h.observe(doge, book([][2]string{{"0.1000", "23.62"}}, nil))
	rep := h.submit(h.order(7, doge, Sell, Yes, 30, 1))
	if len(rep.Fills) != 1 || rep.Fills[0].Qty != 23 || rep.Unfilled != 7 {
		t.Errorf("got %+v, %d unfilled; want 23 filled, 7 unfilled", rep.Fills, rep.Unfilled)
	}
}

func TestNeverASixthLevel(t *testing.T) {
	six := [][2]string{{"0.1000", "1"}, {"0.0990", "1"}, {"0.0980", "1"}, {"0.0970", "1"}, {"0.0960", "1"}, {"0.0950", "1000"}}
	h := newHarness(t)
	h.observe(doge, book(six, nil))
	rep := h.submit(h.order(7, doge, Sell, Yes, 100, 1))
	if len(rep.Fills) != 5 || rep.Unfilled != 95 || len(rep.Seen) != 5 {
		t.Errorf("got %d fills, %d unfilled, %d seen; want 5, 95, 5", len(rep.Fills), rep.Unfilled, len(rep.Seen))
	}
}

func TestNeverTheAggregates(t *testing.T) {
	q := kalshi.Quotes{YesBid: "0.1000", YesBidDepth: kalshi.Depth{C1: 170, C3: 847.93, C5: 3629.43}}
	h := newHarness(t)
	h.observe(doge, q)
	checkStatus(t, h.submit(h.order(7, doge, Sell, Yes, 10, 1)), Cancelled, 10, ReasonNoBids)
}

func TestLevelsOneNeverTouchesLevelTwo(t *testing.T) {
	h := newHarness(t)
	h.p = NewPaper(1)
	h.observe(doge, quotes(t, dogeQuotes))
	rep := h.submit(h.order(7, doge, Sell, Yes, 348, 1))
	checkStatus(t, rep, Partial, 305, ReasonNothingMore)
	if len(rep.Fills) != 1 || rep.Fills[0].Qty != 43 || len(rep.Seen) != 1 {
		t.Errorf("got %+v seen %+v, want 43 at level 0 only", rep.Fills, rep.Seen)
	}
}

// ---- Test 6: limits ----

func TestLimits(t *testing.T) {
	cases := []struct {
		name         string
		a            Action
		s            Side
		limit        Price
		filled       int
		worst        Price // the worst taker price filled at
		reasonIfNone string
	}{
		// yes_bids 0.1000 x 43, 0.0990 x 100, 0.0930 x 1, ...
		{"sell at the best bid only", Sell, Yes, 1000, 43, 1000, ""},
		{"sell down to 0.0990", Sell, Yes, 990, 143, 990, ""},
		{"sell stops at the first failing price", Sell, Yes, 925, 144, 930, ""},
		{"sell above the book", Sell, Yes, 1001, 0, 0, ReasonOutsideLimit},
		// no_bids 0.8800 x 161, 0.8700 x 23.62, 0.8600 x 28.28: a buy of yes pays 0.12, 0.13, 0.14
		{"buy at the best ask only", Buy, Yes, 1200, 161, 1200, ""},
		{"buy between two asks", Buy, Yes, 1399, 184, 1300, ""},
		{"buy below the book", Buy, Yes, 1199, 0, 0, ReasonOutsideLimit},
		// a buy of no is filled by yes_bids: pays 0.9000, 0.9010, ...
		{"buy no at the best ask only", Buy, No, 9000, 43, 9000, ""},
		// a sale of no hits no_bids
		{"sell no down to 0.8700", Sell, No, 8700, 184, 8700, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.observe(doge, quotes(t, dogeQuotes))
			rep := h.submit(h.order(7, doge, c.a, c.s, 1000, c.limit))
			filled := 0
			var worst Price
			for _, f := range rep.Fills {
				filled += f.Qty
				worst = f.Price
			}
			if filled != c.filled || worst != c.worst {
				t.Errorf("filled %d down to %d, want %d down to %d", filled, worst, c.filled, c.worst)
			}
			if c.filled == 0 && rep.Reason != c.reasonIfNone {
				t.Errorf("reason %q, want %q", rep.Reason, c.reasonIfNone)
			}
		})
	}
}

// ---- Test 7: cost ceilings ----

func cost(rep Report) (total int64) {
	for _, f := range rep.Fills {
		total += f.PremiumCents + f.FeeCents
	}
	return total
}

func TestMaxCostNeverExceeded(t *testing.T) {
	for _, perFill := range []bool{false, true} {
		for _, maxCost := range []int64{1, 12, 13, 14, 255, 739, 2052, 2053, 2500, 6000} {
			for qty := 1; qty <= 500; qty++ {
				h := newHarness(t)
				h.p.FeePerFill = perFill
				h.observe(doge, quotes(t, dogeQuotes))
				o := h.order(7, doge, Buy, Yes, qty, 1600)
				o.MaxCostCents = maxCost
				rep := h.submit(o)
				if got := cost(rep); got > maxCost {
					t.Fatalf("qty %d, ceiling %dc: paid %dc", qty, maxCost, got)
				}
				// The ceiling takes as many as fit, not fewer: when it is what stopped the
				// order, one more contract at the level it stopped on would have passed it.
				if rep.Reason == ReasonCeiling {
					var premium, numerator, fees int64
					filled := map[int]int{}
					for _, f := range rep.Fills {
						premium, fees, numerator = premium+f.PremiumCents, fees+f.FeeCents, numerator+feeNumerator(f.Qty, f.Price)
						filled[f.Level] = f.Qty
					}
					last := len(rep.Fills) - 1
					level := 0
					if last >= 0 {
						level = rep.Fills[last].Level
						if rep.Seen[level].Taken == rep.Seen[level].Displayed {
							level++ // the level was emptied: the ceiling stopped the next one
							last = -1
						}
					}
					taker := TakerPrice(Buy, rep.Seen[level].Bid)
					n := 1
					if last >= 0 { // redo the last fill one larger
						f := rep.Fills[last]
						premium, fees, numerator = premium-f.PremiumCents, fees-f.FeeCents, numerator-feeNumerator(f.Qty, f.Price)
						n = f.Qty + 1
					}
					more := premium + PremiumCents(Buy, n, taker) + fees + feeShare(numerator, feeNumerator(n, taker), perFill)
					if more <= maxCost {
						t.Fatalf("qty %d, ceiling %dc: stopped at %dc although %dc was possible", qty, maxCost, cost(rep), more)
					}
				}
			}
		}
	}
}

func TestMaxCostSingleContract(t *testing.T) {
	// One contract at 0.1200 costs premium 12c + fee ceil(0.7392) 1c = 13c. A ceiling of 12c
	// allows none, and the walk ends.
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	o := h.order(7, doge, Buy, Yes, 5, 1600)
	o.MaxCostCents = 12
	checkStatus(t, h.submit(o), Cancelled, 5, ReasonCeiling)
	o = h.order(7, doge, Buy, Yes, 5, 1600)
	o.MaxCostCents = 13
	rep := h.submit(o)
	checkStatus(t, rep, Partial, 4, ReasonCeiling)
	checkFills(t, rep, []wantFill{{0, 1, 1200, 12, 1, -13}})
}

func TestCostStepsThinTopBook(t *testing.T) {
	// Section 4.5's order (buy 58 yes, limit 0.1400, stake 739c, steps 0.12: 739c, 0.13: 440c,
	// 0.14: 135c) against a THIN top level: 20 shown at 0.12, the rest of the real DOGE book.
	//   0.12  20 fill: premium ceil(24000/100) = 240c; numerator 7*20*1200*8800 = 1,478,400,000 -> 14.784 -> 15c. 255c.
	//   0.13  ceiling 440c. 13 more: premium ceil(16900/100) = 169c; numerator 7*13*1300*8700 = 1,029,210,000;
	//         cumulative 2,507,610,000 -> 25.08 -> 26c, share 11c. 240 + 169 + 26 = 435c <= 440c.
	//         A 14th: premium 182c; cumulative 2,586,780,000 -> 26c; 240 + 182 + 26 = 448c > 440c.
	//   0.14  ceiling 135c, already passed: the walk ends.
	// 33 filled for 435c, 25 unfilled.
	q := quotes(t, dogeQuotes)
	q.NoBids[0][1] = "20.00"
	h := newHarness(t)
	h.observe(doge, q)
	o := h.order(7, doge, Buy, Yes, 58, 1400)
	o.MaxCostCents = 739
	o.CostSteps = []CostStep{{1200, 739}, {1300, 440}, {1400, 135}}
	rep := h.submit(o)
	checkStatus(t, rep, Partial, 25, ReasonCeiling)
	checkFills(t, rep, []wantFill{{0, 20, 1200, 240, 15, -255}, {1, 13, 1300, 169, 11, -180}})
	if got := cost(rep); got != 435 {
		t.Errorf("paid %dc, want 435c", got)
	}

	// On the recorded book (161 shown at 0.12) the steps never bind: 58 at 0.1200, premium
	// 696c, fee 7*58*1200*8800 = 4,287,360,000 -> 42.87 -> 43c, 739c paid.
	h = newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	o = h.order(7, doge, Buy, Yes, 58, 1400)
	o.MaxCostCents = 739
	o.CostSteps = []CostStep{{1200, 739}, {1300, 440}, {1400, 135}}
	rep = h.submit(o)
	checkStatus(t, rep, Filled, 0, "")
	checkFills(t, rep, []wantFill{{0, 58, 1200, 696, 43, -739}})
}

func TestCostStepsHoldAtEveryFill(t *testing.T) {
	// The cumulative cost at every fill is within the ceiling of ITS price, whatever the top
	// level shows and however the steps are ordered.
	steps := []CostStep{{1400, 135}, {1200, 739}, {1300, 440}} // deliberately unsorted
	for top := 0; top <= 70; top++ {
		q := quotes(t, dogeQuotes)
		q.NoBids[0][1] = fmt.Sprintf("%d.00", top)
		h := newHarness(t)
		h.observe(doge, q)
		o := h.order(7, doge, Buy, Yes, 400, 1400)
		o.CostSteps = steps
		rep := h.submit(o)
		var soFar int64
		for _, f := range rep.Fills {
			soFar += f.PremiumCents + f.FeeCents
			ceiling, _ := ceilingAt(o, f.Price)
			if soFar > ceiling {
				t.Fatalf("top %d: %dc spent once the fill at %d is in, ceiling %dc", top, soFar, f.Price, ceiling)
			}
		}
	}
}

func TestACeilingOfNothingEndsTheWalk(t *testing.T) {
	// A step of 0c at the best price: the stake formula reaches nothing at the limit.
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	o := h.order(7, doge, Buy, Yes, 10, 1400)
	o.CostSteps = []CostStep{{1200, 0}}
	checkStatus(t, h.submit(o), Cancelled, 10, ReasonCeiling)
}

func TestACeilingCutFillEndsTheWalk(t *testing.T) {
	// The 2026-09-21 code review's case, measured on the code as it then was: a buy that the
	// ceiling cut short at level 0 went on to take ONE contract at level 1, while 994 better
	// priced contracts were still showing. Premium is rounded up per fill and the fee once per
	// order, so the split came in a cent under the ceiling that one more at level 0 breaks. No
	// immediate-or-cancel sweep can produce that sequence. The walk must end at level 0.
	//
	// no_bids [0.2426 x 1000] [0.2416 x 1000]; buy 1000 yes, limit 0.9999, ceiling 540c.
	//   6 at 0.7574   premium ceil(45444/100) = 455c; numerator 7*6*7574*2426 = 771,730,008 -> 7.72 -> 8c.  463c.
	//   a 7th there   premium ceil(53018/100) = 531c; numerator 7*7*7574*2426 = 900,351,676 -> 9.0035 -> 10c. 541c > 540c.
	//   6 + 1 at 0.7584 (what must NOT happen): premium 455 + ceil(7584/100) = 455 + 76; numerator
	//                 771,730,008 + 7*1*7584*2416 = 899,990,616 -> 8.9999 -> 9c. 540c <= 540c.
	//
	// The second row is the review's other measured case, where the stray contract's fee share is 0c:
	// no_bids [0.0428 x 1000] [0.0409 x 1000], ceiling 673c.
	//   6 at 0.9572   premium ceil(57432/100) = 575c; numerator 7*6*9572*428 = 172,066,272 -> 1.72 -> 2c.  577c.
	//   a 7th there   premium ceil(67004/100) = 671c; numerator 200,743,984 -> 2.007 -> 3c. 674c > 673c.
	//   6 + 1 at 0.9591: 575 + ceil(9591/100) = 575 + 96; numerator 172,066,272 + 7*9591*409 = 199,525,305 -> 2c. 673c.
	for _, c := range []struct {
		best, next string
		ceiling    int64
		want       wantFill
	}{
		{"0.2426", "0.2416", 540, wantFill{0, 6, 7574, 455, 8, -463}},
		{"0.0428", "0.0409", 673, wantFill{0, 6, 9572, 575, 2, -577}},
	} {
		for _, levels := range []int{2, 1} { // 1: the cut fill is at the LAST level of the book
			rows := [][2]string{{c.best, "1000"}, {c.next, "1000"}}[:levels]
			h := newHarness(t)
			h.observe(doge, book(nil, rows))
			o := h.order(7, doge, Buy, Yes, 1000, 9999)
			o.MaxCostCents = c.ceiling
			rep := h.submit(o)
			// The reason is the ceiling even when no worse level exists: 994 were displayed and
			// untaken, so "nothing more displayed" would be false.
			checkStatus(t, rep, Partial, 994, ReasonCeiling)
			checkFills(t, rep, []wantFill{c.want})
			if s := rep.Seen[0]; s.Displayed != 1000 || s.Taken != 6 {
				t.Errorf("%s: level 0 seen %+v, want 1000 displayed and 6 taken", c.best, s)
			}
			h.p.Commit(o.ClientID)
			if got := h.p.TotalHeld(7, doge, NoBids); got != 6 {
				t.Errorf("%s: %d held on the ladder, want the 6 at the best bid and nothing below", c.best, got)
			}
		}
	}
}

// ---- Snapshots and levels that cannot be filled from ----

func TestACrossedBookFillsNothing(t *testing.T) {
	// The code review's case: yes_bids [0.6000 x 100], no_bids [0.5000 x 100]. The Yes ask is
	// 1 - 0.5000 = 0.5000, BELOW the Yes bid of 0.6000. Filled as displayed, a bucket buys 100
	// yes at 0.5000 and sells them at 0.6000 in the same second. No matching venue shows such a
	// book, so the snapshot is unusable: every order is rejected and no hold is touched.
	crossed := book([][2]string{{"0.6000", "100"}}, [][2]string{{"0.5000", "100"}})
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	h.sell(7, doge, 43, 1000) // hold 43 at the yes bid 0.1000

	h.observe(doge, crossed)
	for _, o := range []Order{
		h.order(7, doge, Buy, Yes, 100, 5500), h.order(7, doge, Sell, Yes, 100, 5500),
		h.order(7, doge, Buy, No, 100, 5500), h.order(7, doge, Sell, No, 100, 1),
	} {
		rep := h.submit(o)
		checkStatus(t, rep, Rejected, 100, ReasonCrossed)
		checkFills(t, rep, nil)
	}
	// A readable book without 0.1000 in it would have released this hold. An unusable one must not.
	if got := h.p.Held(7, doge, YesBids, 1000); got != 43 {
		t.Errorf("hold at 0.1000 after a crossed snapshot: got %d, want 43 left alone", got)
	}

	// The best DISPLAYED bid decides, even when it shows less than one contract; and one ten-
	// thousandth over is crossed.
	for _, q := range []kalshi.Quotes{
		book([][2]string{{"0.6000", "0.62"}, {"0.4000", "50"}}, [][2]string{{"0.5000", "100"}}),
		book([][2]string{{"0.6000", "100"}}, [][2]string{{"0.4001", "100"}}),
	} {
		h.observe(doge, q)
		checkStatus(t, h.submit(h.order(7, doge, Sell, No, 100, 1)), Rejected, 100, ReasonCrossed)
	}
	if !Ladders(crossed).Crossed {
		t.Error("Ladders should say the snapshot is crossed")
	}
	// The next usable book is used as ever: the hold of 43 is still there and still counts.
	h.observe(doge, quotes(t, dogeQuotes))
	checkStatus(t, h.sell(7, doge, 5, 1000), Cancelled, 5, ReasonAlreadyTaken)

	// NOT crossed: a sum of exactly 1.0000 (locked), a side with no bids, and the real book.
	locked := book([][2]string{{"0.6000", "100"}}, [][2]string{{"0.4000", "100"}})
	for _, q := range []kalshi.Quotes{locked, quotes(t, ethEmptyQuotes), quotes(t, dogeQuotes)} {
		if Ladders(q).Crossed {
			t.Errorf("not crossed: %+v", q)
		}
	}
	h.observe(doge, locked)
	checkStatus(t, h.submit(h.order(8, doge, Sell, Yes, 100, 1)), Filled, 0, "")
}

func TestALevelOfLessThanOneContractIsPassedOver(t *testing.T) {
	// The code review's case: yes_bids [0.5000 x 0.99] [0.1000 x 100], sell 50 yes, limit 0.0001.
	// A real sweep takes the 0.99 and goes on to 0.1000; it does not stop. An order here is whole
	// contracts and 0.99 cannot give one, so all 50 are booked at the WORSE price: the bucket
	// never gains from the fraction it could not take. [CONVENTION, see the comment in fill]
	//   50 at 0.1000: premium floor(50000/100) = 500c; numerator 7*50*1000*9000 = 3,150,000,000 -> 31.5 -> 32c. 468c.
	h := newHarness(t)
	h.observe(doge, book([][2]string{{"0.5000", "0.99"}, {"0.1000", "100"}}, nil))
	rep := h.submit(h.order(7, doge, Sell, Yes, 50, 1))
	checkStatus(t, rep, Filled, 0, "")
	checkFills(t, rep, []wantFill{{1, 50, 1000, 500, 32, 468}})
	if want := []LevelSeen{{5000, 0, 0, 0}, {1000, 100, 0, 50}}; !reflect.DeepEqual(rep.Seen, want) {
		t.Errorf("seen: got %+v, want %+v", rep.Seen, want)
	}

	// The limit still decides: 0.1000 is below a limit of 0.2000, so nothing fills, and the
	// reason is not "already taken", because this bucket took nothing at 0.5000.
	checkStatus(t, h.submit(h.order(8, doge, Sell, Yes, 50, 2000)), Cancelled, 50, ReasonNothingMore)

	// A buy passes over it too, and pays the dearer price: no_bids [0.9000 x 0.5] [0.8000 x 10].
	//   10 at 0.2000: premium 200c; numerator 7*10*2000*8000 = 1,120,000,000 -> 11.2 -> 12c.
	h.observe(doge, book(nil, [][2]string{{"0.9000", "0.5"}, {"0.8000", "10"}}))
	rep = h.submit(h.order(7, doge, Buy, Yes, 10, 9999))
	checkStatus(t, rep, Filled, 0, "")
	checkFills(t, rep, []wantFill{{1, 10, 2000, 200, 12, -212}})

	// A level with WHOLE contracts is never passed over: 1.99 shown gives 1, then the next level.
	h.observe(doge, book([][2]string{{"0.5000", "1.99"}, {"0.1000", "100"}}, nil))
	rep = h.submit(h.order(9, doge, Sell, Yes, 3, 1))
	if len(rep.Fills) != 2 || rep.Fills[0].Level != 0 || rep.Fills[0].Qty != 1 || rep.Fills[1].Qty != 2 {
		t.Errorf("got %+v, want 1 at level 0 and then 2 at level 1", rep.Fills)
	}
}

// ---- Test 10: rejects ----

func TestRejects(t *testing.T) {
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	good := func() Order { return h.order(7, doge, Sell, Yes, 5, 900) }
	cases := []struct {
		name   string
		change func(*Order)
		reason string
	}{
		{"stale evaluation id", func(o *Order) { o.EvaluationID-- }, ReasonStaleBook},
		{"closed market", func(o *Order) { o.At = closes }, ReasonClosed},
		{"qty 0", func(o *Order) { o.Qty = 0 }, ReasonBadQty},
		{"limit 0", func(o *Order) { o.Limit = 0 }, ReasonBadLimit},
		{"limit 1.0000", func(o *Order) { o.Limit = 10000 }, ReasonBadLimit},
		{"no book", func(o *Order) { o.Ticker = "KXNONE" }, ReasonNoBook},
		{"no client id", func(o *Order) { o.ClientID = "" }, ReasonNoClientID},
		{"not a side", func(o *Order) { o.Side = "maybe" }, ReasonBadOrder},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := good()
			c.change(&o)
			rep := h.submit(o)
			checkStatus(t, rep, Rejected, max(0, o.Qty), c.reason)
			raw, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			var back map[string]any
			_ = json.Unmarshal(raw, &back)
			if back["Fills"] == nil || back["Seen"] == nil {
				t.Errorf("Fills and Seen must serialise as [], never null: %s", raw)
			}
		})
	}
	if got := h.p.TotalHeld(7, doge, YesBids); got != 0 {
		t.Errorf("a rejected order held %d", got)
	}
}

func TestARepeatedClientIDReturnsTheFirstReport(t *testing.T) {
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	o := h.order(7, doge, Sell, Yes, 9, 900)
	first := h.submit(o)
	o.Qty = 40 // the retry even differs: the id decides
	again := h.submit(o)
	if !reflect.DeepEqual(first, again) {
		t.Errorf("got a different report for the same client id:\n%+v\n%+v", first, again)
	}
	h.p.Commit(o.ClientID)
	if got := h.p.Held(7, doge, YesBids, 1000); got != 9 {
		t.Errorf("held %d, want 9: the repeat must not take contracts again", got)
	}
}

func TestACancelledContextMovesNothing(t *testing.T) {
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := h.order(7, doge, Sell, Yes, 9, 900)
	if _, err := h.p.Submit(ctx, o); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	checkStatus(t, h.submit(o), Filled, 0, "") // nothing was remembered under the id
}

// ---- Test 11: Void and Commit ----

func TestVoidRestoresAndCommitKeeps(t *testing.T) {
	h := newHarness(t)
	q := quotes(t, dogeQuotes)
	h.observe(doge, q)

	// Two orders in one step cannot take the same contracts: 43 are shown at 0.1000.
	a := h.submit(h.order(7, doge, Sell, Yes, 30, 1000))
	b := h.submit(h.order(7, doge, Sell, Yes, 30, 1000))
	checkStatus(t, a, Filled, 0, "")
	checkStatus(t, b, Partial, 17, ReasonNothingMore)
	if b.Fills[0].Qty != 13 || b.Seen[0].Held != 30 {
		t.Errorf("second order: got %+v, seen %+v; want 13 with 30 already held", b.Fills, b.Seen[0])
	}

	// Void restores availability exactly, and frees the client id.
	if err := h.p.Void(a.Order.ClientID, b.Order.ClientID); err != nil {
		t.Fatal(err)
	}
	c := h.submit(h.order(7, doge, Sell, Yes, 43, 1000))
	checkStatus(t, c, Filled, 0, "")
	if c.Seen[0].Held != 0 {
		t.Errorf("after Void %d still held", c.Seen[0].Held)
	}
	again := h.submit(a.Order)
	checkStatus(t, again, Cancelled, 30, ReasonAlreadyTaken) // c's pending hold has it all now
	_ = h.p.Void(again.Order.ClientID)

	// Commit makes it permanent: the same book a second later offers nothing at 0.1000.
	h.p.Commit(c.Order.ClientID)
	h.observe(doge, q)
	checkStatus(t, h.submit(h.order(7, doge, Sell, Yes, 5, 1000)), Cancelled, 5, ReasonAlreadyTaken)
	if err := h.p.Void(c.Order.ClientID); !errors.Is(err, ErrCannotVoid) {
		t.Errorf("voiding a committed order: got %v, want ErrCannotVoid", err)
	}
	if got := h.p.Held(7, doge, YesBids, 1000); got != 43 {
		t.Errorf("held %d, want 43", got)
	}
}

func TestPendingHoldsSurviveAnObservation(t *testing.T) {
	// Not the runner's order of calls (it commits or voids before the next snapshot), but if a
	// snapshot does arrive in between, the pending fills still count against the book.
	h := newHarness(t)
	q := quotes(t, dogeQuotes)
	h.observe(doge, q)
	a := h.submit(h.order(7, doge, Sell, Yes, 43, 1000))
	h.observe(doge, q)
	checkStatus(t, h.submit(h.order(7, doge, Sell, Yes, 5, 1000)), Cancelled, 5, ReasonAlreadyTaken)
	h.p.Commit(a.Order.ClientID)
	if got := h.p.Held(7, doge, YesBids, 1000); got != 43 {
		t.Errorf("held %d, want 43", got)
	}
}

// ---- Test 12: who shares a hold ----

func TestBucketsDoNotShareHolds(t *testing.T) {
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	h.sell(7, doge, 43, 1000)
	checkStatus(t, h.sell(8, doge, 43, 1000), Filled, 0, "") // bucket 8 is its own paper world
	checkStatus(t, h.sell(7, doge, 1, 1000), Cancelled, 1, ReasonAlreadyTaken)
}

func TestBuyYesAndSellNoShareOneKey(t *testing.T) {
	// Buying Yes at 0.12 and selling No at 0.88 both take the No bid resting at 0.8800 x 161.
	if LadderFor(Buy, Yes) != NoBids || LadderFor(Sell, No) != NoBids || LadderFor(Buy, No) != YesBids || LadderFor(Sell, Yes) != YesBids {
		t.Fatal("ladders")
	}
	h := newHarness(t)
	h.observe(doge, quotes(t, dogeQuotes))
	buy := h.submit(h.order(7, doge, Buy, Yes, 100, 1200))
	h.p.Commit(buy.Order.ClientID)
	if got := h.p.Held(7, doge, NoBids, 8800); got != 100 {
		t.Fatalf("held %d at the No bid 0.8800, want 100", got)
	}
	rep := h.submit(h.order(7, doge, Sell, No, 100, 8800))
	checkStatus(t, rep, Partial, 39, ReasonNothingMore)
	if rep.Fills[0].Qty != 61 || rep.Fills[0].Price != 8800 || rep.Seen[0].Held != 100 {
		t.Errorf("got %+v, want the 61 that are left at 0.8800", rep.Fills)
	}
	// Buying No draws on the OTHER ladder and is not affected.
	checkStatus(t, h.submit(h.order(7, doge, Buy, No, 43, 9000)), Filled, 0, "")
}

// ---- Restart, and forgetting ----

func TestRestoreHoldAndForget(t *testing.T) {
	h := newHarness(t)
	// After a restart: a recorded buy of 100 yes at 0.12 and a recorded sale of 9 yes at 0.10.
	h.p.RestoreHold(7, doge, Buy, Yes, 1200, 100)
	h.p.RestoreHold(7, doge, Sell, Yes, 1000, 9)
	if a, b := h.p.Held(7, doge, NoBids, 8800), h.p.Held(7, doge, YesBids, 1000); a != 100 || b != 9 {
		t.Fatalf("restored %d and %d, want 100 and 9", a, b)
	}
	checkStatus(t, h.submit(h.order(7, doge, Sell, Yes, 1, 1)), Rejected, 1, ReasonNoBook) // holds are not a book
	h.observe(doge, quotes(t, dogeQuotes))
	rep := h.sell(7, doge, 43, 1000)
	if rep.Fills[0].Qty != 34 {
		t.Errorf("sold %d, want 43 - 9 = 34", rep.Fills[0].Qty)
	}
	h.p.Forget(doge)
	if h.p.TotalHeld(7, doge, YesBids) != 0 || h.p.TotalHeld(7, doge, NoBids) != 0 {
		t.Error("Forget left holds")
	}
	checkStatus(t, h.submit(h.order(7, doge, Sell, Yes, 1, 1)), Rejected, 1, ReasonNoBook)
}
