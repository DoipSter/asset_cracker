package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/analysis"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// The recorded DOGE book of market 102 at 2026-09-21 07:41:28Z (db_samples.txt), the book stored
// with the second engine's sells 549 and 550.
var doge = kalshi.Quotes{
	YesBids: [][2]string{{"0.1000", "43.00"}, {"0.0990", "100.00"}, {"0.0930", "1.00"}, {"0.0920", "23.00"}, {"0.0910", "1.00"}},
	NoBids:  [][2]string{{"0.8800", "161.00"}, {"0.8700", "23.62"}, {"0.8600", "28.28"}, {"0.8500", "210.84"}, {"0.8400", "54.92"}},
}

var (
	t0     = time.Date(2026, 9, 21, 7, 41, 28, 52826000, time.UTC)
	closes = time.Date(2026, 9, 21, 7, 45, 0, 0, time.UTC)
)

// sale hand-builds the row main.go would read: the Trade exactly as the analysis layer has it
// (Displayed is the best bid's size on the sale's side, as the SQL takes it), plus the book.
func sale(order, bucket int64, side string, qty int, at time.Time, evaluation int64, q kalshi.Quotes) Sale {
	s := Sale{MarketID: 102, At: at, Limit: "0.09", EvaluationID: evaluation, BookAt: at.Add(-50 * time.Millisecond), Quotes: q}
	s.Trade = analysis.Trade{OrderID: order, BucketID: bucket, Sell: true, Side: side, Qty: qty, Second: at.Unix(),
		CostCents: int64(qty) * 50, PayoutCents: int64(qty) * 9, HasDepth: true}
	bids := q.YesBids
	if side == "no" {
		bids = q.NoBids
	}
	if len(bids) > 0 {
		s.Displayed, _ = strconv.ParseFloat(bids[0][1], 64)
	}
	return s
}

func snapshot(s Sale) Snapshot { return Snapshot{ID: s.EvaluationID, At: s.BookAt, Quotes: s.Quotes} }

var testBuckets = []analysis.Bucket{
	{ID: 21, VersionID: 9, Strategy: "Scalper", Engine: "v2", World: "real"},
	{ID: 22, VersionID: 10, Strategy: "Scalper", Engine: "v2", World: "anti"},
}

// syntheticMarket is plan test 47's trades: two sales in one second that share the displayed
// size, the same again by another bucket (its own paper world), a sale on the other side against
// a fractional level, a lot in the NEXT second against the unchanged book, an empty ladder, a
// level of less than one contract, a sale from before depth was recorded, a sale of the
// depth-aware broker, and a buy, which is no sale at all.
func syntheticMarket() MarketSales {
	thin := kalshi.Quotes{YesBids: [][2]string{{"0.1000", "0.62"}, {"0.0990", "100.00"}}}
	empty := kalshi.Quotes{YesBids: [][2]string{}}
	next := t0.Add(time.Second)
	m := MarketSales{ID: 102, Ticker: "KXDOGE15M-26SEP210345-45", Closes: closes, Result: "no"}
	m.Sales = []Sale{
		sale(550, 22, "yes", 30, t0, 7001, doge), // listed out of order: PriceSales sorts by order id, so must the check
		sale(549, 22, "yes", 40, t0, 7001, doge), // 40 of 43, leaving 3 for order 550
		sale(551, 21, "yes", 348, t0, 7001, doge),
		sale(552, 22, "no", 200, t0, 7001, doge), // 161 at the best no bid
		sale(553, 22, "yes", 25, next, 7002, doge),
		sale(554, 21, "yes", 5, next.Add(time.Second), 7003, empty),
		sale(555, 21, "yes", 5, next.Add(2*time.Second), 7004, thin),
	}
	old := sale(556, 21, "yes", 7, next.Add(3*time.Second), 0, kalshi.Quotes{})
	old.HasDepth, old.Displayed = false, 0
	v3 := sale(557, 21, "yes", 9, next.Add(4*time.Second), 7005, doge)
	v3.DepthPriced = true
	buy := sale(558, 21, "yes", 11, next.Add(5*time.Second), 0, kalshi.Quotes{})
	buy.Sell = false
	m.Sales = append(m.Sales, old, v3, buy)
	for _, s := range m.Sales {
		if s.EvaluationID != 0 && !s.DepthPriced && (len(m.Snapshots) == 0 || m.Snapshots[len(m.Snapshots)-1].ID != s.EvaluationID) {
			m.Snapshots = append(m.Snapshots, snapshot(s))
		}
	}
	return m
}

func outcome(t *testing.T, c Checker, order int64) Outcome {
	t.Helper()
	for _, o := range c.Outcomes {
		if o.OrderID == order {
			return o
		}
	}
	t.Fatalf("order %d has no outcome", order)
	return Outcome{}
}

func TestParityEqualsPriceSales(t *testing.T) {
	c := Checker{}
	c.Market(syntheticMarket())
	if len(c.Outcomes) != 9 {
		t.Fatalf("%d outcomes, want 9 (the buy is not a sale)", len(c.Outcomes))
	}
	// by hand: filled at the best bid, by order id
	want := map[int64]int{549: 40, 550: 3, 551: 43, 552: 161, 553: 25, 554: 0, 555: 0, 556: 0, 557: 9}
	for order, filled := range want {
		o := outcome(t, c, order)
		if o.ParityFilled != filled {
			t.Errorf("order %d: parity filled %d, want %d", order, o.ParityFilled, filled)
		}
		if !o.Analysis.WithoutDepth && o.Qty-o.ParityFilled != o.Analysis.Beyond {
			t.Errorf("order %d: parity beyond %d, PriceSales beyond %d", order, o.Qty-o.ParityFilled, o.Analysis.Beyond)
		}
	}
	sum := Summarise(c.Outcomes, testBuckets)
	if !sum.ParityOK || len(sum.Mismatches) != 0 {
		t.Fatalf("parity should hold: %+v", sum.Mismatches)
	}
	if len(sum.ByStrategy) != 2 {
		t.Fatalf("%d rows, want 2", len(sum.ByStrategy))
	}
	real, anti := sum.ByStrategy[0], sum.ByStrategy[1]
	// Compared: the sales the broker walked. Of the 9, order 556 has no depth and order 557 is a
	// depth-priced pass-through, so 7: orders 551, 554, 555 of bucket 21 and 549, 550, 552, 553 of 22.
	if sum.SalesCompared != 7 || real.SalesCompared != 3 || anti.SalesCompared != 4 {
		t.Errorf("compared %d (%d + %d), want 7 (3 + 4)", sum.SalesCompared, real.SalesCompared, anti.SalesCompared)
	}
	if got := sum.Gate(modeParity); got != exitAgree {
		t.Errorf("gate: exit %d, want 0", got)
	}
	// bucket 21: 348 + 5 + 5 + 9 sold with depth, 43 + 0 + 0 + 9 filled; one sale without depth
	if real.Sells != 5 || real.SellsPriced != 4 || real.SellsWithoutDepth != 1 || real.ContractsSold != 367 || real.ContractsBeyondBid != 315 || real.ParityBeyondBid != 315 || !real.ParityAgrees {
		t.Errorf("bucket 21's row: %+v", real)
	}
	// bucket 22: 40 + 30 + 200 + 25 sold, 40 + 3 + 161 + 25 filled
	if anti.Sells != 4 || anti.ContractsSold != 295 || anti.ContractsBeyondBid != 66 || anti.ParityBeyondBid != 66 || anti.World != "anti" {
		t.Errorf("bucket 22's row: %+v", anti)
	}
	if len(c.Apart) != 0 {
		t.Errorf("nothing should be listed apart: %+v", c.Apart)
	}
}

// A sale at or after the recorded close is still priced in parity mode: PriceSales has no such
// test, and parity mode is that rule and nothing more.
func TestParityIgnoresTheClose(t *testing.T) {
	m := MarketSales{ID: 102, Ticker: "T", Closes: closes, Result: "no", Sales: []Sale{sale(1, 21, "yes", 10, closes.Add(time.Second), 1, doge)}}
	c := Checker{}
	c.Market(m)
	if o := c.Outcomes[0]; o.ParityFilled != 10 || o.Analysis.Beyond != 0 {
		t.Errorf("filled %d, PriceSales beyond %d", o.ParityFilled, o.Analysis.Beyond)
	}
	if c.Counters.SalesAtOrAfterClose != 1 {
		t.Errorf("the late sale should be counted")
	}
}

// If the recorded book and the figure the analysis layer read ever disagree, the check must say
// so and name the sale, not pass.
func TestParityReportsADifference(t *testing.T) {
	s := sale(1, 21, "yes", 50, t0, 1, doge)
	s.Displayed = 50 // the analysis layer believes 50 were shown; the book says 43
	c := Checker{}
	c.Market(MarketSales{ID: 102, Ticker: "T", Closes: closes, Result: "no", Sales: []Sale{s}})
	sum := Summarise(c.Outcomes, testBuckets)
	if sum.ParityOK || len(sum.Mismatches) != 1 || sum.Mismatches[0].OrderID != 1 || sum.Mismatches[0].AnalysisFill != 50 || sum.Mismatches[0].ParityFill != 43 {
		t.Errorf("want one mismatch on order 1, 50 against 43: %+v", sum)
	}
	if sum.ByStrategy[0].ParityAgrees {
		t.Errorf("the row should not agree")
	}
	if sum.Gate(modeParity) != exitDiffer || sum.Gate(modeReal) != exitAgree {
		t.Errorf("gate: parity exit %d (want 1), real exit %d (want 0: real mode gates nothing)", sum.Gate(modeParity), sum.Gate(modeReal))
	}
}

// The gate must not pass on nothing. Each of these runs compares no sale at all, and before the
// 2026-09-21 review each printed the PARITY pass line and exited 0.
func TestTheGateDoesNotPassOnNothing(t *testing.T) {
	old := sale(1, 21, "yes", 7, t0, 0, kalshi.Quotes{})
	old.HasDepth, old.Displayed = false, 0 // made before depth was recorded
	v3 := sale(2, 21, "yes", 9, t0, 1, doge)
	v3.DepthPriced = true // a pass-through: parity is Qty and PriceSales' beyond is 0, both by construction
	unlisted := sale(3, 99, "yes", 10, t0, 1, doge)
	for _, tc := range []struct {
		name  string
		sales []Sale
	}{
		{"no sale at all (no settled market, an empty range, every window left out)", nil},
		{"every sale made before depth was recorded", []Sale{old}},
		{"only depth-priced pass-throughs", []Sale{v3}},
		{"only sales of buckets that are not listed", []Sale{unlisted}},
		{"all three", []Sale{old, v3, unlisted}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Checker{}
			c.Market(MarketSales{ID: 102, Ticker: "T", Closes: closes, Result: "no", Sales: tc.sales})
			sum := Summarise(c.Outcomes, testBuckets)
			if !sum.ParityOK || sum.SalesCompared != 0 {
				t.Fatalf("parity_ok %v, compared %d; want true (nothing differs) and 0", sum.ParityOK, sum.SalesCompared)
			}
			if got := sum.Gate(modeParity); got != exitNothingCompared {
				t.Errorf("parity gate: exit %d, want 3", got)
			}
			if got := sum.Gate(modeReal); got != exitAgree {
				t.Errorf("real mode gates nothing: exit %d, want 0", got)
			}
			var buf bytes.Buffer
			printTable(&buf, Document{Mode: modeParity, Summary: sum})
			if out := buf.String(); !strings.Contains(out, "PARITY: NOTHING COMPARED") || !strings.Contains(out, "NOT a pass") || strings.Contains(out, "agree on") {
				t.Errorf("the verdict must say plainly that nothing was compared:\n%s", out)
			}
			for _, r := range sum.ByStrategy {
				if r.SalesCompared != 0 {
					t.Errorf("row %+v", r)
				}
			}
			if len(sum.ByStrategy) > 0 && !strings.Contains(buf.String(), "nothing compared") {
				t.Errorf("a row with nothing compared must not read as agreeing:\n%s", buf.String())
			}
		})
	}
}

func TestThePrintedVerdictAndInstructions(t *testing.T) {
	c := Checker{RealMode: true}
	c.Market(syntheticMarket())
	doc := Document{Mode: modeReal, Summary: Summarise(c.Outcomes, testBuckets), Apart: c.Apart, Counters: c.Counters}
	var buf bytes.Buffer
	printTable(&buf, doc)
	out := buf.String()
	for _, want := range []string{
		"agree on all 7 sales compared",
		// the sells column cannot equal the API's: it must say which columns can, and how to compare sells
		"sells_priced, sells_without_depth, contracts_sold, contracts_beyond_bid",
		"MINUS its unsettled_sells",
		// the real columns, explained
		"real proceeds c (at bid)", "real not run", "PROCEEDS", "not profit or loss", "a cent under the bid",
		"real_unfilled is contracts of the sales the broker walked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the printed report does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "the analysis columns must also equal") {
		t.Errorf("the old instruction (every analysis column equals the API's) is still printed")
	}
	if !strings.Contains(whatReal, whatRealCents) || !strings.Contains(whatReal, whatRealUnfilled) || strings.Contains(whatParity, "PROCEEDS") {
		t.Errorf("the JSON's what must carry the same two explanations in real mode only")
	}
	buf.Reset()
	doc.Mode = modeParity
	printTable(&buf, doc)
	if strings.Contains(buf.String(), "real proceeds") {
		t.Errorf("parity mode prints no real columns:\n%s", buf.String())
	}
}

// A recorded book whose best yes bid and best no bid add up to more than 1.0000 cannot exist on
// a matching venue. The broker rejects orders on it; the check counts the sale, lists it apart,
// and in parity mode shows it as a difference with the reason, never as agreement.
func TestASaleOnACrossedBook(t *testing.T) {
	crossed := kalshi.Quotes{YesBids: [][2]string{{"0.6000", "100.00"}}, NoBids: [][2]string{{"0.5000", "100.00"}}}
	s := sale(1, 21, "yes", 10, t0, 1, crossed)
	c := Checker{RealMode: true}
	c.Market(MarketSales{ID: 102, Ticker: "T", Closes: closes, Result: "no", Sales: []Sale{s}, Snapshots: []Snapshot{snapshot(s)}})
	if c.Counters.SalesOnCrossedBooks != 1 {
		t.Errorf("counted %d sales on a crossed book, want 1", c.Counters.SalesOnCrossedBooks)
	}
	if len(c.Apart) != 2 || !strings.Contains(c.Apart[0].Why, "crossed") || !strings.Contains(c.Apart[1].Why, "crossed") {
		t.Errorf("listed apart once per mode, with the reason: %+v", c.Apart)
	}
	o := c.Outcomes[0]
	if o.ParityFilled != 0 || !o.Walked || o.Real || !o.RealNotRun {
		t.Errorf("%+v", o)
	}
	sum := Summarise(c.Outcomes, testBuckets)
	if sum.ParityOK || len(sum.Mismatches) != 1 || !strings.Contains(sum.Mismatches[0].ParityReason, "crossed") || sum.Gate(modeParity) != exitDiffer {
		t.Errorf("PriceSales fills 10 from that book and the broker none: a difference, named: %+v", sum)
	}
	if r := sum.ByStrategy[0]; r.RealUnfilled != 0 || r.RealNotRun != 10 {
		t.Errorf("real unfilled %d, not run %d; want 0 and 10", r.RealUnfilled, r.RealNotRun)
	}
}

func TestRealModeHoldsAcrossSecondsAndWalksDeeper(t *testing.T) {
	c := Checker{RealMode: true}
	c.Market(syntheticMarket())
	type fill struct{ best, deeper int }
	// Every sale's limit is 0.09, so the 0.1000, 0.0990, 0.0930, 0.0920 and 0.0910 levels all pass.
	want := map[int64]fill{
		549: {40, 0},
		550: {3, 27},   // 3 left at 0.1000, then 27 of the 100 at 0.0990
		551: {43, 125}, // bucket 21's own world: the whole ladder, 43+100+1+23+1
		552: {161, 39}, // sells NO: every no bid is above 0.09, so 161, then 23 (floor of 23.62), then 16 of the 28 at 0.8600
		553: {0, 25},   // next second, unchanged book: 0.1000 is held in full; 0.0990 has 73 left
		554: {0, 0},    // empty ladder
		555: {0, 5},    // 0.62 of a contract at the best bid fills nothing; 0.0990 x 100 is within the limit
		556: {0, 0},    // no depth: not run
		557: {0, 0},    // depth-aware already: not run
	}
	for order, w := range want {
		o := outcome(t, c, order)
		if o.RealFilledBest != w.best || o.RealFilledDeeper != w.deeper {
			t.Errorf("order %d: real filled %d at best + %d deeper, want %d + %d (%s)", order, o.RealFilledBest, o.RealFilledDeeper, w.best, w.deeper, o.RealReason)
		}
		// Plan test 47, as it can be true: AT THE BEST BID real mode never fills more than parity.
		if o.RealFilledBest > o.ParityFilled {
			t.Errorf("order %d: real filled %d at the best bid, parity only %d", order, o.RealFilledBest, o.ParityFilled)
		}
	}
	if o := outcome(t, c, 556); o.Real {
		t.Errorf("a sale without depth must not reach the broker")
	}
	// order 549: 40 at 0.1000. Premium floor(40 x 1000 / 100) = 400c, fee ceil(7 x 40 x 1000 x 9000 / 1e8) = ceil(25.2) = 26c.
	if o := outcome(t, c, 549); o.RealBucketCents != 374 {
		t.Errorf("order 549: %dc to the bucket, want 374", o.RealBucketCents)
	}
	sum := Summarise(c.Outcomes, testBuckets)
	if !sum.ParityOK {
		t.Errorf("real mode must not disturb the parity figures: %+v", sum.Mismatches)
	}
	for _, r := range sum.ByStrategy {
		if r.RealFilledBest > r.ParityFilled {
			t.Errorf("%s %s: real at best %d > parity %d", r.Strategy, r.World, r.RealFilledBest, r.ParityFilled)
		}
	}
}

// The plan's expectation for real mode: a sale split over consecutive seconds against an
// unchanged book. Parity refreshes the book every second and fills both lots; the real broker
// remembers the first lot.
func TestRealModeFillsLessWhenLotsFollowEachOther(t *testing.T) {
	one := kalshi.Quotes{YesBids: [][2]string{{"0.1000", "43.00"}}}
	a, b := sale(1, 21, "yes", 43, t0, 1, one), sale(2, 21, "yes", 43, t0.Add(time.Second), 2, one)
	m := MarketSales{ID: 102, Ticker: "T", Closes: closes, Result: "no", Sales: []Sale{a, b}, Snapshots: []Snapshot{snapshot(a), snapshot(b)}}
	c := Checker{RealMode: true}
	c.Market(m)
	if o := c.Outcomes[1]; o.ParityFilled != 43 || o.RealFilledBest+o.RealFilledDeeper != 0 || o.RealReason != "already taken by this bucket" {
		t.Errorf("second lot: parity %d, real %d+%d (%s)", o.ParityFilled, o.RealFilledBest, o.RealFilledDeeper, o.RealReason)
	}
}

// Snapshots BETWEEN two sales are observed: a fall in the display between them displaces the
// hold, and a recovery after it frees the level again (plan 2.4, example C).
func TestRealModeObservesTheSecondsBetween(t *testing.T) {
	book := func(size string) kalshi.Quotes { return kalshi.Quotes{YesBids: [][2]string{{"0.1000", size}}} }
	a, b := sale(1, 21, "yes", 43, t0, 1, book("43.00")), sale(2, 21, "yes", 43, t0.Add(2*time.Second), 3, book("43.00"))
	between := Snapshot{ID: 2, At: a.BookAt.Add(time.Second), Quotes: book("10.00")}
	m := MarketSales{ID: 102, Ticker: "T", Closes: closes, Result: "no", Sales: []Sale{a, b}, Snapshots: []Snapshot{snapshot(a), between, snapshot(b)}}
	c := Checker{RealMode: true}
	c.Market(m)
	// the hold fell to 10 in between (33 displaced off the visible book), so 33 can fill again
	if o := c.Outcomes[1]; o.RealFilledBest != 33 {
		t.Errorf("second lot filled %d at the best bid, want 33", o.RealFilledBest)
	}
	if c.Counters.BooksObservedAgain != 0 {
		t.Errorf("no book should have needed observing again")
	}
	// the same two sales with the snapshots missing: each sale's own book is observed instead
	m.Snapshots = nil
	c = Checker{RealMode: true}
	c.Market(m)
	if o := c.Outcomes[1]; o.RealFilledBest != 0 || c.Counters.BooksObservedAgain != 2 {
		t.Errorf("without the snapshots: filled %d, observed again %d", o.RealFilledBest, c.Counters.BooksObservedAgain)
	}
}

// One Paper per bucket for the whole history, but a settled market's holds are dropped: the same
// ticker's holds must not leak into a later call, and another market starts clean.
func TestRealModeForgetsASettledMarket(t *testing.T) {
	one := kalshi.Quotes{YesBids: [][2]string{{"0.1000", "43.00"}}}
	a := sale(1, 21, "yes", 43, t0, 1, one)
	c := Checker{RealMode: true}
	c.Market(MarketSales{ID: 102, Ticker: "T", Closes: closes, Result: "no", Sales: []Sale{a}, Snapshots: []Snapshot{snapshot(a)}})
	b := sale(2, 21, "yes", 43, t0.Add(time.Second), 2, one)
	c.Market(MarketSales{ID: 103, Ticker: "T", Closes: closes, Result: "no", Sales: []Sale{b}, Snapshots: []Snapshot{snapshot(b)}})
	if o := c.Outcomes[1]; o.RealFilledBest != 43 {
		t.Errorf("filled %d, want 43", o.RealFilledBest)
	}
}

func TestRealModeLimits(t *testing.T) {
	zero, high, none, fine := sale(1, 21, "yes", 5, t0, 1, doge), sale(2, 21, "yes", 5, t0, 1, doge), sale(3, 21, "yes", 5, t0, 1, doge), sale(4, 21, "yes", 5, t0, 1, doge)
	zero.Limit, high.Limit, none.Limit, fine.Limit = "0", "0.0995", "", "0.09001"
	m := MarketSales{ID: 102, Ticker: "T", Closes: closes, Result: "no", Sales: []Sale{zero, high, none, fine}, Snapshots: []Snapshot{snapshot(zero)}}
	c := Checker{RealMode: true}
	c.Market(m)
	if o := c.Outcomes[0]; o.RealFilledBest != 5 || c.Counters.LimitsRaisedToMinimum != 1 {
		t.Errorf("a limit of 0 accepts any bid: filled %d, raised %d", o.RealFilledBest, c.Counters.LimitsRaisedToMinimum)
	}
	// limit 0.0995: 0.1000 passes, 0.0990 does not. 38 are left at 0.1000 after the first sale.
	if o := c.Outcomes[1]; o.RealFilledBest != 5 || o.RealFilledDeeper != 0 {
		t.Errorf("limit 0.0995: %+v", o)
	}
	if len(c.Apart) != 2 || c.Apart[0].OrderID != 3 || c.Apart[1].OrderID != 4 || c.Apart[0].Mode != modeReal {
		t.Errorf("the missing and the too-fine limit should be listed apart: %+v", c.Apart)
	}
	// Those two were never submitted: 5 + 5 contracts not run, none of them "unfilled".
	if r := Summarise(c.Outcomes, testBuckets).ByStrategy[0]; r.RealNotRun != 10 || r.RealUnfilled != 0 || r.RealFilledBest != 10 {
		t.Errorf("not run %d, unfilled %d, filled at best %d; want 10, 0, 10", r.RealNotRun, r.RealUnfilled, r.RealFilledBest)
	}
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{{"0.09", 900, true}, {"0.0990", 990, true}, {"0", 0, true}, {"-0.01", -100, true}, {"1", 0, false}, {"abc", 0, false}, {"", 0, false}} {
		got, err := parseLimit(tc.in)
		if (err == nil) != tc.ok || (tc.ok && int64(got) != tc.want) {
			t.Errorf("parseLimit(%q) = %d, %v", tc.in, got, err)
		}
	}
}

// A sale placed at or after the close is rejected by the real broker and listed apart.
func TestRealModeListsRejectsApart(t *testing.T) {
	s := sale(1, 21, "yes", 10, closes, 1, doge)
	c := Checker{RealMode: true}
	c.Market(MarketSales{ID: 102, Ticker: "T", Closes: closes, Result: "no", Sales: []Sale{s}, Snapshots: []Snapshot{snapshot(s)}})
	if len(c.Apart) != 1 || c.Apart[0].Mode != modeReal || !strings.HasPrefix(c.Apart[0].Why, "rejected: ") {
		t.Errorf("apart: %+v", c.Apart)
	}
	if o := c.Outcomes[0]; o.ParityFilled != 10 || o.RealFilledBest != 0 {
		t.Errorf("%+v", o)
	}
	// The broker never walked the book for it, so its 10 contracts are not "unfilled": that
	// column is what the book could not take. They are counted as not run.
	if o := c.Outcomes[0]; o.Real || !o.RealNotRun {
		t.Errorf("real %v, not run %v; want false and true", o.Real, o.RealNotRun)
	}
	if r := Summarise(c.Outcomes, testBuckets).ByStrategy[0]; r.RealUnfilled != 0 || r.RealNotRun != 10 {
		t.Errorf("real unfilled %d, not run %d; want 0 and 10", r.RealUnfilled, r.RealNotRun)
	}
}

func TestComplete(t *testing.T) {
	markets := []analysis.Market{{ID: 1, Closes: 900}, {ID: 2, Closes: 900}, {ID: 3, Closes: 1800}, {ID: 4, Closes: 2700}, {ID: 5, Closes: 2700}}
	kept, out := Complete(markets, map[int64]string{1: "bucket 3 held 5 yes"}, map[int64]bool{2700: true})
	if len(kept) != 1 || kept[0].ID != 3 {
		t.Errorf("kept %+v, want market 3 only", kept)
	}
	if out[1] != "bucket 3 held 5 yes" || out[2] == "" || out[4] == "" || out[5] == "" || len(out) != 4 {
		t.Errorf("left out: %+v", out)
	}
}

// Both time flags are read, and an empty range refused, before anything is connected to.
func TestParseRange(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	since, until, err := parseRange("", "", now)
	if err != nil || !since.IsZero() || !until.Equal(now) {
		t.Errorf("defaults: since %v until %v err %v; want the zero time (read from the database) and now", since, until, err)
	}
	since, until, err = parseRange("2026-09-21T00:00:00Z", "2026-09-21T06:00:00Z", now)
	if err != nil || since.Hour() != 0 || until.Hour() != 6 {
		t.Errorf("both given: %v %v %v", since, until, err)
	}
	for _, tc := range []struct{ since, until, want string }{
		{"garbage", "", "-since"},
		{"", "garbage", "-until"},
		{"garbage", "garbage", "-until"},
		{"2026-09-21T06:00:00Z", "2026-09-21T06:00:00Z", "not before"},
		{"2026-09-21T07:00:00Z", "2026-09-21T06:00:00Z", "not before"},
		{"2026-09-22T00:00:00Z", "", "not before"}, // after now, which is the default -until
	} {
		if _, _, err := parseRange(tc.since, tc.until, now); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseRange(%q, %q): %v; want an error naming %q", tc.since, tc.until, err, tc.want)
		}
	}
}

// Two orderings in main.go matter and neither can be exercised without a database, so the
// source itself is read (as the broker package's no-float test reads its own):
//
//   - main reads the time flags (parseRange) BEFORE it calls run, and run parses no flag: a bad
//     flag must never open a connection to the production database or be reported as a
//     connection error.
//   - run lists the open windows BEFORE the settled markets, as web/analysis.go compute does. In
//     the other order a result landing between the two statements keeps its window with that
//     coin missing, and nothing says so.
func TestMainReadsInTheRightOrder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]map[string]token.Pos{} // function -> callee -> first call
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		first := map[string]token.Pos{}
		calls[fn.Name.Name] = first
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
				if x, ok := f.X.(*ast.Ident); ok && (x.Name == "time" || x.Name == "pgx") {
					name = x.Name + "." + name
				}
			}
			if _, seen := first[name]; !seen {
				first[name] = call.Pos()
			}
			return true
		})
	}
	before := func(fn, a, b string) {
		t.Helper()
		pa, oka := calls[fn][a]
		pb, okb := calls[fn][b]
		if !oka || !okb || pa >= pb {
			t.Errorf("in %s, %s must be called before %s (found: %v, %v)", fn, a, b, oka, okb)
		}
	}
	before("main", "parseRange", "run")
	before("run", "openWindows", "markets")
	if _, parses := calls["run"]["time.Parse"]; parses {
		t.Errorf("run parses a time: flags are validated in main, before pgx.Connect")
	}
	if _, connects := calls["main"]["pgx.Connect"]; connects {
		t.Errorf("main connects itself: the connection belongs in run, after the flags are read")
	}
}

func TestSummariseSkipsUnlistedBuckets(t *testing.T) {
	c := Checker{}
	c.Market(MarketSales{ID: 102, Ticker: "T", Closes: closes, Result: "no", Sales: []Sale{sale(1, 99, "yes", 10, t0, 1, doge)}})
	sum := Summarise(c.Outcomes, testBuckets)
	if sum.UnlistedBuckets != 1 || len(sum.ByStrategy) != 0 || !sum.ParityOK || sum.SalesCompared != 0 {
		t.Errorf("%+v", sum)
	}
}

// Nothing in the JSON is null: a consumer iterates these.
func TestJSONHasNoNullSlices(t *testing.T) {
	doc := Document{Simulated: true, Mode: modeParity, LeftOut: []LeftOut{}, Summary: Summarise(nil, nil), Apart: []Apart{}}
	var buf bytes.Buffer
	if err := writeJSON(&buf, doc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "null") {
		t.Errorf("a null in the document:\n%s", buf.String())
	}
	var back map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"by_strategy", "mismatches", "apart", "left_out"} {
		if _, ok := back[k].([]any); !ok {
			t.Errorf("%s is not an array", k)
		}
	}
	if n, ok := back["sales_compared"].(float64); !ok || n != 0 {
		t.Errorf("sales_compared must be in the document, and 0 here: %v", back["sales_compared"])
	}
	printTable(&buf, doc) // must not panic on an empty document
}
