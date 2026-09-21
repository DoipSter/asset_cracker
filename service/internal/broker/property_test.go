package broker

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// ---- Test 8: the fee ----

// floatFee is a COPY of the second engine's fee (kalshi15m2/model.go, KalshiFee, FeeRate 0.07),
// in cents. It is copied, not imported, because this package must not depend on that engine;
// that package is frozen, so the copy cannot drift. The float and its epsilon live only here,
// in a test, as the thing the integer form is checked against.
func floatFee(contracts int, price float64) int64 {
	dollars := math.Ceil(0.07*float64(contracts)*price*(1-price)*100-1e-9) / 100
	return int64(math.Round(dollars * 100))
}

func TestFeeEqualsTheSecondEnginesFee(t *testing.T) {
	for cents := 1; cents <= 99; cents++ {
		for n := 1; n <= 2000; n++ {
			if got, want := FeeCents(n, Price(cents*100)), floatFee(n, float64(cents)/100); got != want {
				t.Fatalf("%d contracts at %dc: integer fee %dc, the second engine's %dc", n, cents, got, want)
			}
		}
	}
}

func TestFeeByHand(t *testing.T) {
	// 58 at 0.1200: 7*58*1200*8800 = 4,287,360,000 -> 42.8736c -> 43c.
	// 1 at 0.5000: 7*1*5000*5000 = 175,000,000 -> 1.75c -> 2c.
	// 100 at 0.5000: 17,500,000,000 -> exactly 175c: an exact figure is not rounded up.
	for _, c := range []struct {
		n    int
		p    Price
		want int64
	}{{58, 1200, 43}, {1, 5000, 2}, {100, 5000, 175}, {1, 91, 1}} {
		if got := FeeCents(c.n, c.p); got != c.want {
			t.Errorf("%d at %d: got %dc, want %dc", c.n, c.p, got, c.want)
		}
	}
}

func TestFeeSharesSumAndPerFillIsNeverCheaper(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	for i := 0; i < 20000; i++ {
		var before, perOrder, perFill, total int64
		for level := 0; level < 1+rng.Intn(5); level++ {
			own := feeNumerator(1+rng.Intn(300), Price(1+rng.Intn(9999)))
			perOrder += feeShare(before, own, false)
			perFill += feeShare(before, own, true)
			before += own
			total = ceilFee(before)
		}
		if perOrder != total {
			t.Fatalf("the shares add up to %dc, the order's fee is %dc", perOrder, total)
		}
		if perFill < perOrder {
			t.Fatalf("FeePerFill %dc is cheaper than per order %dc", perFill, perOrder)
		}
	}
}

// ---- Test 9: rounding, and levels worth less than their fee ----

func TestPremiumRounding(t *testing.T) {
	// 0.0990: 1 contract is 9.9c -> buy 10c, sell 9c. 3 contracts are 29.7c -> 30c, 29c.
	// 0.9980: 1 contract is 99.8c -> buy 100c, sell 99c. 7 contracts are 698.6c -> 699c, 698c.
	// A whole number of cents is not rounded either way: 100 at 0.0990 is 990c.
	for _, c := range []struct {
		qty       int
		p         Price
		buy, sell int64
	}{{1, 990, 10, 9}, {3, 990, 30, 29}, {100, 990, 990, 990}, {1, 9980, 100, 99}, {7, 9980, 699, 698}, {1, 91, 1, 0}} {
		if b, s := PremiumCents(Buy, c.qty, c.p), PremiumCents(Sell, c.qty, c.p); b != c.buy || s != c.sell {
			t.Errorf("%d at %d: buy %dc sell %dc, want %dc and %dc", c.qty, c.p, b, s, c.buy, c.sell)
		}
	}
}

func TestALevelWorthLessThanItsFeeIsTaken(t *testing.T) {
	// One contract at 0.0091 sold alone: premium floor(0.91) = 0c; numerator 7*1*91*9909 =
	// 6,312,033 -> 0.063c -> 1c. The bucket PAYS a cent, and that is what is booked.
	h := newHarness(t)
	h.observe(doge, book([][2]string{{"0.0091", "1"}}, nil))
	rep := h.submit(h.order(7, doge, Sell, Yes, 1, 1))
	checkStatus(t, rep, Filled, 0, "")
	checkFills(t, rep, []wantFill{{0, 1, 91, 0, 1, -1}})
}

func TestAFillOfNothingEndsTheWalk(t *testing.T) {
	// [0.0100 x 1] [0.0050 x 1] [0.0040 x 500]. Level 0: premium 1c, numerator 6,930,000 -> 1c.
	// Level 1: premium floor(0.5) = 0c; numerator 7*1*50*9950 = 3,482,500; cumulative
	// 10,412,500 -> 0.104c -> still 1c, share 0c. A fill of 0c and 0c cannot be booked, and the
	// level may not be skipped, so the walk ends: the 500 below are never reached.
	h := newHarness(t)
	h.observe(doge, book([][2]string{{"0.0100", "1"}, {"0.0050", "1"}, {"0.0040", "500"}}, nil))
	rep := h.submit(h.order(7, doge, Sell, Yes, 100, 1))
	checkStatus(t, rep, Partial, 99, ReasonDust)
	checkFills(t, rep, []wantFill{{0, 1, 100, 1, 1, 0}})
}

// ---- Properties (tests 9 and 13, and the job's own four) ----

// randomLadder is up to seven levels (the broker must ignore what is past five), sizes with
// fractions, sometimes empty levels, sub-cent prices, from a small set of prices so that holds
// and displays meet often.
func randomLadder(rng *rand.Rand, base int) [][2]string {
	rows := [][2]string{}
	used := map[int]bool{}
	for i := 0; i < rng.Intn(8); i++ {
		p := base - 10*rng.Intn(12)
		if used[p] {
			continue
		}
		used[p] = true
		size := fmt.Sprintf("%d.%02d", rng.Intn(40), rng.Intn(100))
		rows = append(rows, [2]string{fmt.Sprintf("0.%04d", p), size})
	}
	return rows
}

type levelKey struct {
	g   group
	bid Price
}

func snapshotHolds(p *Paper, ticker string) map[levelKey]int {
	out := map[levelKey]int{}
	if m := p.markets[ticker]; m != nil {
		for g, byPrice := range m.holds {
			for bid, qty := range byPrice {
				out[levelKey{g, bid}] = qty
			}
		}
	}
	return out
}

func TestProperties(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		p := &Paper{Levels: 5, FeePerFill: seed%2 == 0}
		var eval int64
		n := 0
		for second := 0; second < 120; second++ {
			q := book(randomLadder(rng, 1000), randomLadder(rng, 8900))
			before := snapshotHolds(p, doge)
			eval++
			at := t0.Add(time.Duration(second) * time.Second)
			p.ObserveBook(doge, eval, at, at.Add(time.Hour), q)
			after := snapshotHolds(p, doge)
			checkObservation(t, seed, before, after, Ladders(q))

			taken := map[levelKey]int{} // since this observation, committed or pending
			var ids []string
			for k := 0; k < rng.Intn(5); k++ {
				n++
				o := Order{ClientID: fmt.Sprint("o", n), BucketID: int64(1 + rng.Intn(2)), Ticker: doge,
					Action: []Action{Buy, Sell}[rng.Intn(2)], Side: []Side{Yes, No}[rng.Intn(2)],
					Qty: 1 + rng.Intn(80), Limit: Price(1 + rng.Intn(9999)), EvaluationID: eval, At: at}
				if o.Action == Buy && rng.Intn(2) == 0 {
					o.MaxCostCents = int64(1 + rng.Intn(3000))
				}
				if o.Action == Buy && rng.Intn(3) == 0 {
					// A ceiling a cent or two UNDER the exact cost of n contracts at the best
					// level, no more of them than are shown there, on an order that wants more and whose
					// limit admits every level: the ceiling then cuts the fill short INSIDE the
					// level, with about one contract's worth of money left over. The walk must
					// end there (see TestCeilingNeverReachesPastABetterLevel, which hunts the
					// case directly; this draw meets it with holds in play).
					best, _ := Ladders(q).side(LadderFor(o.Action, o.Side))
					if len(best) > 0 && best[0].Size >= 2 {
						fit, taker := 2+rng.Intn(best[0].Size-1), TakerPrice(Buy, best[0].Bid)
						o.Qty, o.Limit = fit+rng.Intn(40), priceScale-1
						o.MaxCostCents = PremiumCents(Buy, fit, taker) + FeeCents(fit, taker) - 1 - int64(rng.Intn(2))
					}
				}
				if o.Action == Buy && rng.Intn(3) == 0 {
					o.CostSteps = []CostStep{{Price(rng.Intn(9999)), int64(rng.Intn(2000))}, {Price(rng.Intn(9999)), int64(rng.Intn(500))}}
				}
				rep, err := p.Submit(context.Background(), o)
				if err != nil {
					t.Fatal(err)
				}
				checkReport(t, seed, p.FeePerFill, rep)
				g := group{o.BucketID, LadderFor(o.Action, o.Side)}
				if rng.Intn(4) == 0 {
					if err := p.Void(o.ClientID); err != nil {
						t.Fatal(err)
					}
					continue
				}
				ids = append(ids, o.ClientID)
				for _, s := range rep.Seen {
					key := levelKey{g, s.Bid}
					taken[key] += s.Taken
					// Between two observations one bucket never takes more than floor(displayed)
					// at a level, and with what it held before never holds more than is displayed.
					if taken[key] > s.Displayed || s.Held+s.Taken > s.Displayed && s.Taken > 0 {
						t.Fatalf("seed %d: bucket %d took %d at %d where %d are displayed (held %d)", seed, o.BucketID, taken[key], s.Bid, s.Displayed, s.Held)
					}
				}
			}
			p.Commit(ids...)
		}
	}
}

// TestCeilingNeverReachesPastABetterLevel hunts the case the 2026-09-21 review measured: a buy
// whose ceiling cuts the fill short inside the best level, with the money left over within a
// cent of one more contract. Premium rounds up per fill and the fee once per order, so ONE
// contract at the next (dearer) level can cost a cent less in total than one more at the best
// level, mostly above 0.50 where the fee falls as the price rises. checkReport fails any fill
// made while a better level still had contracts. On the code as reviewed this test fails within
// the first few thousand draws; TestProperties' 40 seeds never met the case.
func TestCeilingNeverReachesPastABetterLevel(t *testing.T) {
	rng := rand.New(rand.NewSource(21))
	cut := 0
	for i := 0; i < 60000; i++ {
		p := &Paper{Levels: 5, FeePerFill: i%4 == 0}
		bid := 40 + rng.Intn(9900) // the best No bid; a buy of Yes pays 1 - bid
		rows := [][2]string{{fmt.Sprintf("0.%04d", bid), fmt.Sprint(2 + rng.Intn(1000))}}
		for next := bid - 1 - rng.Intn(30); next > 0 && len(rows) < 3; next -= 1 + rng.Intn(30) {
			rows = append(rows, [2]string{fmt.Sprintf("0.%04d", next), fmt.Sprint(1 + rng.Intn(1000))})
		}
		q := book(nil, rows)
		p.ObserveBook(doge, 1, t0, closes, q)
		best := Ladders(q).No[0]
		n, taker := 2+rng.Intn(min(best.Size-1, 60)), TakerPrice(Buy, best.Bid)
		o := Order{ClientID: fmt.Sprint("c", i), BucketID: 1, Ticker: doge, Action: Buy, Side: Yes, Qty: n + rng.Intn(50),
			Limit: priceScale - 1, EvaluationID: 1, At: t0}
		// just under the cost of n at the best level: n-1 fit there, the nth does not
		o.MaxCostCents = PremiumCents(Buy, n, taker) + feeShare(0, feeNumerator(n, taker), p.FeePerFill) - 1 - int64(rng.Intn(2))
		rep, err := p.Submit(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		checkReport(t, int64(i), p.FeePerFill, rep)
		if len(rep.Fills) == 1 && rep.Seen[0].Taken < rep.Seen[0].Displayed && rep.Unfilled > 0 {
			cut++
			if rep.Reason != ReasonCeiling {
				t.Fatalf("draw %d: the ceiling cut the fill short at the best level, but the reason is %q", i, rep.Reason)
			}
		}
	}
	if cut < 1000 {
		t.Fatalf("only %d of the draws were cut short by the ceiling: the test no longer reaches its case", cut)
	}
}

// checkObservation: on ObserveBook the total held per (bucket, ladder) never rises, a level's
// hold rises only by what a BETTER level released, and no visible level is held past its display.
func checkObservation(t *testing.T, seed int64, before, after map[levelKey]int, sides Sides) {
	t.Helper()
	groups := map[group][]Price{}
	seen := map[levelKey]bool{}
	for _, m := range []map[levelKey]int{before, after} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				groups[k.g] = append(groups[k.g], k.bid)
			}
		}
	}
	for g, prices := range groups {
		sort.Slice(prices, func(i, j int) bool { return prices[i] > prices[j] })
		carried, totalBefore, totalAfter := 0, 0, 0 // carried: released by better levels, not yet absorbed
		for _, bid := range prices {
			b, a := before[levelKey{g, bid}], after[levelKey{g, bid}]
			totalBefore, totalAfter = totalBefore+b, totalAfter+a
			if a > b {
				carried -= a - b
				if carried < 0 {
					t.Fatalf("seed %d: the hold at %d rose from %d to %d by more than better levels released", seed, bid, b, a)
				}
			} else {
				carried += b - a
			}
		}
		if totalAfter > totalBefore {
			t.Fatalf("seed %d: total held rose from %d to %d on an observation", seed, totalBefore, totalAfter)
		}
		shown, _ := sides.side(g.ladder)
		for _, lv := range shown {
			if got := after[levelKey{g, lv.Bid}]; got > lv.Size {
				t.Fatalf("seed %d: %d held at %d where %d are displayed", seed, got, lv.Bid, lv.Size)
			}
		}
	}
}

// checkReport: what must be true of every answer, whatever the book and the order.
func checkReport(t *testing.T, seed int64, perFill bool, rep Report) {
	t.Helper()
	o := rep.Order
	if rep.Fills == nil || rep.Seen == nil {
		t.Fatalf("seed %d: nil Fills or Seen", seed)
	}
	if len(rep.Seen) > RecordedLevels {
		t.Fatalf("seed %d: %d levels seen", seed, len(rep.Seen))
	}
	filled := 0
	var paid, numerator, fees int64
	for i, f := range rep.Fills {
		filled += f.Qty
		s := rep.Seen[f.Level]
		if f.Seq != i+1 || f.Qty < 1 || s.Taken != f.Qty || f.Price != TakerPrice(o.Action, s.Bid) {
			t.Fatalf("seed %d: fill %+v does not match what was seen %+v", seed, f, s)
		}
		// never beyond what is displayed at a level, this bucket's earlier takes included
		if s.Held+s.Taken > s.Displayed {
			t.Fatalf("seed %d: took %d with %d held where %d are displayed", seed, s.Taken, s.Held, s.Displayed)
		}
		// within the limit
		if (o.Action == Buy && f.Price > o.Limit) || (o.Action == Sell && f.Price < o.Limit) {
			t.Fatalf("seed %d: filled at %d against a limit of %d", seed, f.Price, o.Limit)
		}
		// no fill at a level while a better level still had whole contracts available
		for _, better := range rep.Seen[:f.Level] {
			if better.Displayed-better.Held-better.Taken > 0 {
				t.Fatalf("seed %d: filled at level %d while %+v still had contracts", seed, f.Level, better)
			}
		}
		// money: the three ledger entries of the fill sum to zero, to the cent
		if b, v, fe := transfer(o.Action, f); b+v+fe != 0 {
			t.Fatalf("seed %d: fill %+v does not balance", seed, f)
		}
		if f.PremiumCents == 0 && f.FeeCents == 0 {
			t.Fatalf("seed %d: a fill with no cash effect cannot be booked: %+v", seed, f)
		}
		if f.PremiumCents != PremiumCents(o.Action, f.Qty, f.Price) || f.FeeCents < 0 {
			t.Fatalf("seed %d: premium or fee of %+v", seed, f)
		}
		paid += f.PremiumCents + f.FeeCents
		fees += f.FeeCents
		numerator += feeNumerator(f.Qty, f.Price)
		if o.Action == Buy {
			if ceiling, has := ceilingAt(o, f.Price); has && paid > ceiling {
				t.Fatalf("seed %d: %dc paid once the fill at %d is in, ceiling %dc", seed, paid, f.Price, ceiling)
			}
		}
	}
	if !perFill && fees != ceilFee(numerator) {
		t.Fatalf("seed %d: fee shares %dc, the order's fee %dc", seed, fees, ceilFee(numerator))
	}
	if filled+rep.Unfilled != o.Qty {
		t.Fatalf("seed %d: %d filled + %d unfilled is not the %d ordered", seed, filled, rep.Unfilled, o.Qty)
	}
	want := map[bool]Status{true: Filled, false: Partial}[rep.Unfilled == 0]
	if filled == 0 {
		want = Cancelled
	}
	if rep.Status != want || (rep.Status == Filled) != (rep.Reason == "") {
		t.Fatalf("seed %d: status %s reason %q with %d filled, %d unfilled", seed, rep.Status, rep.Reason, filled, rep.Unfilled)
	}
}

// ---- No float in any money path ----

// TestNoFloatInThePackage reads the package's own source. Nothing outside the tests may name a
// float type, write a float literal, or import math: quantities, prices, premiums and fees are
// integers from the recorded text to the ledger figure. (Recorded sizes are decimal text and are
// floored with big.Rat, which is exact.)
func TestNoFloatInThePackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		checked++
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"math"` || imp.Path.Value == `"strconv"` {
				t.Errorf("%s imports %s", e.Name(), imp.Path.Value)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "float64" || x.Name == "float32" {
					t.Errorf("%s: %s", fset.Position(x.Pos()), x.Name)
				}
			case *ast.BasicLit:
				if x.Kind == token.FLOAT {
					t.Errorf("%s: float literal %s", fset.Position(x.Pos()), x.Value)
				}
			}
			return true
		})
	}
	if checked < 4 {
		t.Fatalf("only %d source files found", checked)
	}
}

// ---- Concurrency: run under -race ----

func TestConcurrentUse(t *testing.T) {
	p := NewPaper(5)
	var q kalshi.Quotes = book([][2]string{{"0.1000", "43"}, {"0.0990", "100"}}, [][2]string{{"0.8800", "161"}})
	p.ObserveBook(doge, 1, t0, closes, q)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := fmt.Sprintf("w%d:%d", w, i)
				rep, _ := p.Submit(context.Background(), Order{ClientID: id, BucketID: int64(w % 2), Ticker: doge,
					Action: Sell, Side: Yes, Qty: 1, Limit: 1, EvaluationID: 1, At: t0})
				switch i % 3 {
				case 0:
					p.Commit(rep.Order.ClientID)
				case 1:
					_ = p.Void(rep.Order.ClientID)
				default:
					p.ObserveBook(doge, 1, t0, closes, q)
				}
				_ = p.TotalHeld(int64(w%2), doge, YesBids)
			}
		}(w)
	}
	wg.Wait()
	for bucket := int64(0); bucket < 2; bucket++ {
		if got := p.TotalHeld(bucket, doge, YesBids); got > 143 {
			t.Errorf("bucket %d holds %d of the 143 displayed", bucket, got)
		}
	}
}
