package kalshi15m3

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/broker"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// In every test below lambda and the staleness costs are PLACEHOLDERS (see fixtures_test.go).

var doge = Market{Ticker: "KXDOGE15M-T", MarketID: 102, Strike: 0.0884, Close: 1000}

// Test 15. Partial sells release a basis that sums exactly to the cost, whatever the split, and
// the window's OpenCents and LostCents follow.
func TestPositionApplyPartialSells(t *testing.T) {
	for _, split := range [][]int{{58}, {1, 57}, {19, 19, 20}, {7, 7, 7, 7, 7, 7, 7, 9}, {57, 1}} {
		p := &Position{Ticker: "T", Side: "yes"}
		w := &Window{Close: 1000, EquityCents: 100000}
		if cash, basis, err := p.Apply(broker.Buy, broker.Fill{Qty: 58, Price: 1200, PremiumCents: 696, FeeCents: 43}, w); err != nil || cash != -739 || basis != 739 {
			t.Fatalf("buy: cash %d basis %d err %v", cash, basis, err)
		}
		if p.Contracts != 58 || p.CostCents != 739 || p.PremiumCents != 696 || w.OpenCents != 739 || w.LostCents != 0 {
			t.Fatalf("after the buy: %+v %+v", *p, *w)
		}
		var released, lost int64
		for _, n := range split {
			// sold at 0.07: under the 12.74c basis, so every part loses
			f := broker.Fill{Qty: n, Price: 700, PremiumCents: broker.PremiumCents(broker.Sell, n, 700), FeeCents: broker.FeeCents(n, 700)}
			cash, basis, err := p.Apply(broker.Sell, f, w)
			if err != nil {
				t.Fatal(err)
			}
			if cash != broker.BucketCents(broker.Sell, f) {
				t.Fatalf("cash %d is not the broker's figure", cash)
			}
			released += basis
			lost += basis - cash
			if w.OpenCents != 739-released || w.LostCents != lost {
				t.Fatalf("split %v after %d: window %+v, released %d lost %d", split, n, *w, released, lost)
			}
		}
		if released != 739 || p.Contracts != 0 || p.CostCents != 0 || p.PremiumCents != 0 || w.OpenCents != 0 {
			t.Fatalf("split %v: released %d of 739, left %+v", split, released, *p)
		}
	}
	p := &Position{Ticker: "T", Side: "yes", Contracts: 3, CostCents: 100}
	if _, _, err := p.Apply(broker.Sell, broker.Fill{Qty: 4}, nil); err == nil || p.Contracts != 3 {
		t.Error("a sale of more than is held must be refused and change nothing")
	}
}

// Test 19, first half. Plan 4.5's worked example to the cent, through Decide and a real Paper.
func TestSizingWorkedExample(t *testing.T) {
	a := NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true)
	h := newHarness(t, a)
	// p_model 0.16 with lambda 1 is p = 0.16 up to float rounding; 0.007 is the plan's ILLUSTRATIVE stale cost.
	_, intents, reports, _ := h.step("DOGE", doge, dogeBook, view("DOGE", 0.16), 500)
	if len(intents) != 1 {
		t.Fatalf("want one intent, got %d", len(intents))
	}
	o := intents[0].Order
	wantSteps := []broker.CostStep{{UpTo: 1200, MaxCostCents: 739}, {UpTo: 1300, MaxCostCents: 440}, {UpTo: 1400, MaxCostCents: 135}}
	if o.Action != broker.Buy || o.Side != broker.Yes || o.Qty != 58 || o.Limit != 1400 || o.MaxCostCents != 739 || !reflect.DeepEqual(o.CostSteps, wantSteps) {
		t.Fatalf("order %+v", o)
	}
	if o.ClientID != "v3:31:1:1" {
		t.Errorf("client id %q", o.ClientID)
	}
	if k := intents[0].Kelly; k < 0.029583 || k > 0.029585 {
		t.Errorf("kelly %v, the plan has 0.029584", k)
	}
	r := reports[0]
	if r.Status != broker.Filled || len(r.Fills) != 1 || r.Fills[0].Qty != 58 || r.Fills[0].PremiumCents != 696 || r.Fills[0].FeeCents != 43 {
		t.Fatalf("report %+v", r)
	}
	w := a.Windows[1000]
	if a.CashCents != 100000-739 || w == nil || w.EquityCents != 100000 || w.Used() != 739 || a.Bets != 1 {
		t.Fatalf("cash %d window %+v", a.CashCents, w)
	}
	if intents[0].Detail["plumbing"] != true {
		t.Error("an order formed on placeholder numbers must say so")
	}

	// The same window, other coins. A signal with a SMALLER k finds the budget spent; one with
	// k = 0.10 raises the budget to 2500c and has 1761c of it left.
	small := Size(SizeInput{PSide: 0.5340, Asks: []broker.Price{5000}, StaleUnits: 70, Kappa: 0.25, CapBps: 2500, SeedCents: 100000,
		EquityCents: w.EquityCents, KMax: w.KMax, UsedCents: w.Used(), CashCents: a.CashCents})
	if small.Kelly <= 0 || small.Kelly >= w.KMax || small.BlockedBy != BlockedBudgetSpent {
		t.Fatalf("a smaller bet in a spent window: %+v", small)
	}
	big := Size(SizeInput{PSide: 0.57206, Asks: []broker.Price{5000}, StaleUnits: 70, Kappa: 0.25, CapBps: 2500, SeedCents: 100000,
		EquityCents: w.EquityCents, KMax: w.KMax, UsedCents: w.Used(), CashCents: a.CashCents})
	if big.BudgetCents != 2500 || big.RoomCents != 1761 || big.StakeCents != 1761 || big.Binding != "window" {
		t.Fatalf("a bigger bet in the same window: %+v", big)
	}
}

// Test 19, the thin top level: 20 shown at 0.12. 20 fill there, 13 more fit under the 440c
// ceiling at 0.13, and at 0.14 the 135c ceiling is already passed: 33 for 435c, 25 unfilled.
func TestSizingThinTop(t *testing.T) {
	a := NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true)
	h := newHarness(t, a)
	thin := book(dogeBook.YesBids, [][2]string{{"0.8800", "20"}, {"0.8700", "23.62"}, {"0.8600", "28.28"}, {"0.8500", "210.84"}, {"0.8400", "54.92"}})
	_, _, reports, events := h.step("DOGE", doge, thin, view("DOGE", 0.16), 500)
	r := reports[0]
	if r.Status != broker.Partial || r.Unfilled != 25 || len(r.Fills) != 2 || r.Fills[0].Qty != 20 || r.Fills[1].Qty != 13 {
		t.Fatalf("report %+v", r)
	}
	pos := a.Position(doge.Ticker, "yes")
	if pos == nil || pos.Contracts != 33 || pos.CostCents != 435 || a.CashCents != 100000-435 || a.Windows[1000].OpenCents != 435 {
		t.Fatalf("position %+v cash %d", pos, a.CashCents)
	}
	// Test 16: a partial entry is ONE bet, and it starts the gap.
	if pos.Entries != 1 || a.Bets != 1 || pos.LastBuyAt != 500 || // whole seconds survive the trip through a time exactly
		events[0].Kind != "bought" {
		t.Fatalf("a partial entry counts one bet: %+v, bets %d", pos, a.Bets)
	}
	decisions, intents, _, _ := h.step("DOGE", doge, dogeBook, view("DOGE", 0.16), 503)
	if len(intents) != 0 || decisions[0].BlockedBy != BlockedCoolingDown {
		t.Fatalf("inside the gap: %+v", decisions)
	}
	_, intents, _, _ = h.step("DOGE", doge, dogeBook, view("DOGE", 0.16), 509)
	if len(intents) != 1 || intents[0].Order.Qty != 23 { // 739 - 435 = 304c of room; 304 / 12.7392
		t.Fatalf("after the gap the remainder of the budget may be tried: %+v", intents)
	}
	if pos.Entries != 2 {
		t.Errorf("entries %d", pos.Entries)
	}
}

// Test 16, second half. A cancelled entry is nothing: no bet, no gap, no window.
func TestCancelledEntryCountsNothing(t *testing.T) {
	a := NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true)
	e := testEngine(t, a)
	m := doge
	m.EvaluationID = 9
	_, intents := e.Decide("DOGE", m, dogeBook, view("DOGE", 0.16), 500)
	before := cloneAccounts(e.Accounts)
	for _, status := range []broker.Status{broker.Cancelled, broker.Rejected} {
		events := e.Apply(intents, []broker.Report{{Order: intents[0].Order, Status: status, Fills: []broker.Fill{}, Unfilled: 58, Reason: broker.ReasonAlreadyTaken}})
		if len(events) != 1 || events[0].Kind != "entry_unfilled" {
			t.Fatalf("events %+v", events)
		}
		if !reflect.DeepEqual(before, cloneAccounts(e.Accounts)) {
			t.Fatalf("a %s entry changed the account: %+v", status, *a)
		}
	}
	m.EvaluationID = 10
	if _, again := e.Decide("DOGE", m, dogeBook, view("DOGE", 0.16), 501); len(again) != 1 {
		t.Error("no gap was started, so the next second may try again")
	}
	if ev := e.Apply(intents, nil); len(ev) != 1 || ev[0].Kind != "inconsistent" {
		t.Error("reports that do not match the intents must be refused")
	}
}

func holding(t *testing.T, staleSell float64, contracts int, premium, fee int64) (*harness, *Account) {
	a := NewAccount(testScalper(t, 1, 0.007, staleSell), 31, 100000, true)
	h := newHarness(t, a)
	ev := h.engine.Fold(Booked{BucketID: 31, MarketID: doge.MarketID, Coin: "DOGE", Ticker: doge.Ticker, Close: doge.Close, Strike: doge.Strike,
		Action: broker.Buy, Side: broker.Yes, Why: "entry", At: 400, Kelly: 0.03, Window: Window{Close: 1000, EquityCents: 100000},
		Fills: []broker.Fill{{Seq: 1, Qty: contracts, Price: 1200, PremiumCents: premium, FeeCents: fee}}})
	if ev[0].Kind != "bought" {
		t.Fatalf("%+v", ev)
	}
	return h, a
}

// Test 17. A partial exit keeps Exiting; the next second the rule is evaluated from scratch; when
// it stops firing the want is dropped; on an empty bid side the order is STILL sent, the tries and
// blocked seconds grow, and the position rides to settlement.
func TestExitLifecycle(t *testing.T) {
	h, a := holding(t, 0.006, 50, 600, 37)
	rich := book([][2]string{{"0.6000", "10"}, {"0.5900", "5"}, {"0.2000", "100"}}, [][2]string{{"0.3800", "100"}})
	decisions, intents, reports, _ := h.step("DOGE", doge, rich, view("DOGE", 0.30), 500)
	if len(intents) != 1 || decisions[0].Action != "exit" || decisions[0].BlockedBy != "" {
		t.Fatalf("decisions %+v", decisions)
	}
	o := intents[0].Order
	if o.Action != broker.Sell || o.Qty != 50 || o.Limit != 5900 || intents[0].Why != "value" {
		t.Fatalf("the exit is for ALL held, down to the lowest bid at which the rule holds: %+v why %s", o, intents[0].Why)
	}
	if reports[0].Status != broker.Partial {
		t.Fatalf("report %+v", reports[0])
	}
	pos := a.Position(doge.Ticker, "yes")
	// two fills, each releasing its floor: 637*10/50 = 127, then 510*5/40 = 63
	if pos.Contracts != 35 || pos.Exiting != "value" || pos.CostCents != 637-127-63 {
		t.Fatalf("after a partial exit: %+v", *pos)
	}

	// Empty bid side: the order is still sent, and is cancelled.
	empty := book(nil, [][2]string{{"0.3800", "100"}})
	for try := 1; try <= 2; try++ {
		_, intents, reports, events := h.step("DOGE", doge, empty, view("DOGE", 0.30), 500+float64(try))
		if len(intents) != 1 || intents[0].Order.Action != broker.Sell || intents[0].Order.Qty != 35 {
			t.Fatalf("try %d: the order must still be sent: %+v", try, intents)
		}
		if reports[0].Status != broker.Cancelled || reports[0].Reason != broker.ReasonNoBids || events[0].Kind != "exit_unfilled" {
			t.Fatalf("try %d: %+v", try, reports[0])
		}
		if pos.ExitTries != try || pos.BlockedSeconds != try || pos.Contracts != 35 || pos.Exiting != "value" {
			t.Fatalf("try %d: %+v", try, *pos)
		}
	}

	// A book on which the rule no longer fires: dropped, nothing sent, the rest is held.
	poor := book([][2]string{{"0.2500", "100"}}, [][2]string{{"0.7000", "100"}})
	decisions, intents, _, _ = h.step("DOGE", doge, poor, view("DOGE", 0.30), 510)
	if len(intents) != 0 || decisions[0].ClearExit != "yes" || pos.Exiting != "" || pos.ExitTries != 0 {
		t.Fatalf("the rule stopped firing: %+v, position %+v", decisions, *pos)
	}
	// ...and with nothing wanted, an empty side sends nothing.
	if _, intents, _, _ = h.step("DOGE", doge, empty, view("DOGE", 0.30), 511); len(intents) != 0 {
		t.Fatal("no exit is wanted, so an empty side sends nothing")
	}

	// Wanted again, and a crossed book: no price in it can be trusted, nothing is sent, and the
	// second still counts as one in which the exit was wanted and nothing filled.
	pos.Exiting = "value"
	crossed := book([][2]string{{"0.6000", "10"}}, [][2]string{{"0.5000", "10"}})
	decisions, intents, _, _ = h.step("DOGE", doge, crossed, view("DOGE", 0.30), 512)
	if len(intents) != 0 || decisions[0].BlockedBy != BlockedExitNoBook || pos.BlockedSeconds != 3 {
		t.Fatalf("a crossed book with an exit pending: %+v, position %+v", decisions, *pos)
	}
	// Then too late: under min_tau no more orders, and every such second is counted too. It settles
	// with ALL its blocked seconds: two empty orders, one crossed book, two seconds too late.
	for i, now := range []float64{995, 996} {
		decisions, intents, _, _ = h.step("DOGE", doge, empty, view("DOGE", 0.30), now)
		if len(intents) != 0 || decisions[0].BlockedBy != BlockedExitTooLate || pos.BlockedSeconds != 4+i {
			t.Fatalf("under min_tau: %+v, position %+v", decisions, *pos)
		}
	}
	if pos.ExitTries != 0 {
		t.Fatalf("a second with no order sent is not a try: %+v", *pos)
	}
	rows := h.engine.SettleRows(doge.Ticker, "yes")
	if len(rows) != 1 || rows[0].Qty != 35 || rows[0].PayoutCents != 3500 || !rows[0].Won {
		t.Fatalf("rows %+v", rows)
	}
	cash := a.CashCents
	events := h.engine.ApplySettlement(doge.Ticker, "yes")
	if len(events) != 1 || events[0].Kind != "settled" || events[0].BlockedSeconds != 5 || a.CashCents != cash+3500 {
		t.Fatalf("settled: %+v", events)
	}
	if len(a.Positions) != 0 || len(a.Windows) != 0 {
		t.Fatalf("a settled window is pruned: %+v %+v", a.Positions, a.Windows)
	}
}

// Test 26 in small: the loser's row pays nothing, and a position sold out earlier yields no row.
func TestSettlementRows(t *testing.T) {
	h, a := holding(t, 0.006, 50, 600, 37)
	if rows := h.engine.SettleRows(doge.Ticker, "no"); len(rows) != 1 || rows[0].PayoutCents != 0 || rows[0].Won || rows[0].CostCents != 637 {
		t.Fatalf("a loser's row: %+v", rows)
	}
	if rows := h.engine.SettleRows(doge.Ticker, ""); rows != nil {
		t.Fatal("no result, no rows")
	}
	a.MayOrder = false // settle-only still settles
	h.engine.ApplySettlement(doge.Ticker, "no")
	if a.CashCents != 100000-637 || len(a.Positions) != 0 {
		t.Fatalf("after a losing settlement: %+v", *a)
	}
	if rows := h.engine.SettleRows(doge.Ticker, "no"); len(rows) != 0 {
		t.Fatal("nothing held, no row")
	}
}

// Test 18. No entry in a step that sent an exit; an account that may not order sends nothing.
func TestExitExcludesEntryAndSettleOnlySendsNothing(t *testing.T) {
	h, a := holding(t, 0.006, 50, 600, 37)
	fresh := NewAccount(testScalper(t, 1, 0.007, 0.006), 32, 100000, true)
	idle := NewAccount(testScalper(t, 1, 0.007, 0.006), 33, 100000, false)
	idle.Positions[posKey{doge.Ticker, "yes"}] = &Position{Coin: "DOGE", Ticker: doge.Ticker, Side: "yes", Contracts: 50, CostCents: 637, PremiumCents: 600, LastBuyAt: 400, Close: 1000}
	spent := NewAccount(testScalper(t, 1, 0.007, 0.006), 34, 100000, true)
	spent.Exhausted = true
	h.engine = testEngine(t, a, fresh, idle, spent)

	// The bid has covered 80% of the way from 0.12 to a dollar, and an ask of 0.91 still has an
	// edge at p 0.99: the holder captures and does NOT also buy; the fresh account buys.
	high := book([][2]string{{"0.9000", "500"}}, [][2]string{{"0.0900", "500"}})
	m := doge
	m.EvaluationID = 1
	decisions, intents := h.engine.Decide("DOGE", m, high, view("DOGE", 0.99), 500)
	if len(decisions) != 2 || len(intents) != 2 {
		t.Fatalf("want the holder's exit and the fresh account's entry only: %+v", decisions)
	}
	if decisions[0].BucketID != 31 || decisions[0].Action != "exit" || intents[0].Why != "capture" || intents[0].Order.Limit != 9000 {
		t.Fatalf("holder: %+v %+v", decisions[0], intents[0].Order)
	}
	if decisions[1].BucketID != 32 || decisions[1].Action != "enter" || intents[1].Order.Action != broker.Buy {
		t.Fatalf("fresh: %+v", decisions[1])
	}
	for _, d := range decisions {
		if d.BlockedBy != "" || intents[d.Intent].Decision < 0 {
			t.Errorf("blocked_by is never set on a decision that sent an order: %+v", d)
		}
	}
	// Value v3 never sells early, whatever the bid.
	hv := newHarness(t, NewAccount(testValue(t, 1, 0.007), 40, 100000, true))
	hv.engine.Fold(Booked{BucketID: 40, Coin: "DOGE", Ticker: doge.Ticker, Close: 1000, Action: broker.Buy, Side: broker.Yes, At: 400,
		Window: Window{Close: 1000, EquityCents: 100000}, Fills: []broker.Fill{{Qty: 50, Price: 1200, PremiumCents: 600, FeeCents: 37}}})
	if _, intents, _, _ := hv.step("DOGE", doge, high, view("DOGE", 0.30), 500); len(intents) != 0 {
		t.Fatalf("Value holds to settlement: %+v", intents)
	}
}

// Test 21. Both exit rules charge stale_cost_sell: set high enough, neither fires on a book where
// each fired without it.
func TestExitsChargeStaleCostSell(t *testing.T) {
	valueBook := book([][2]string{{"0.6000", "10"}}, [][2]string{{"0.3800", "100"}})   // overpaid against p 0.57
	captureBook := book([][2]string{{"0.9000", "10"}}, [][2]string{{"0.0900", "100"}}) // not overpaid against p 0.99
	for _, c := range []struct {
		name   string
		q      kalshi.Quotes
		pModel float64
		why    string
	}{{"value", valueBook, 0.57, "value"}, {"capture", captureBook, 0.99, "capture"}} {
		h, _ := holding(t, 0, 50, 600, 37)
		_, intents, _, _ := h.step("DOGE", doge, c.q, view("DOGE", c.pModel), 500)
		if len(intents) != 1 || intents[0].Why != c.why {
			t.Fatalf("%s with no staleness cost must fire: %+v", c.name, intents)
		}
		// 0.02 undoes the value case (0.6 - 0.0168 - 0.02 < 0.57); 0.80 undoes the capture (0.9 - 0.0063 - 0.80 < 0.1274)
		for _, stale := range []float64{0.02, 0.80} {
			h, _ = holding(t, stale, 50, 600, 37)
			_, intents, _, _ = h.step("DOGE", doge, c.q, view("DOGE", c.pModel), 500)
			fires := len(intents) == 1 && intents[0].Order.Action == broker.Sell
			want := c.name == "capture" && stale == 0.02
			if fires != want {
				t.Errorf("%s with stale_cost_sell %v: fires %v, want %v", c.name, stale, fires, want)
			}
		}
	}
}

// fullFill is a report that fills the whole order at its best price, for driving the fold without
// the Paper's holds getting in the way of buying the same level again and again.
func fullFill(in Intent, price broker.Price) broker.Report {
	o := in.Order
	f := broker.Fill{Seq: 1, Qty: o.Qty, Price: price, PremiumCents: broker.PremiumCents(o.Action, o.Qty, price), FeeCents: broker.FeeCents(o.Qty, price)}
	return broker.Report{Order: o, Status: broker.Filled, Fills: []broker.Fill{f}, Model: broker.PaperModel, Final: true}
}

// Test 20. A sale below basis frees only what it recovered; losing trips in one window stop at the
// budget, across coins; a winning sale frees its basis and no more.
func TestLosingRoundTrips(t *testing.T) {
	a := NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true)
	e := testEngine(t, a)
	buy := func(coin string, m Market, eval int64, now float64) (Intent, bool) {
		m.EvaluationID = eval
		decisions, intents := e.Decide(coin, m, dogeBook, view(coin, 0.16), now)
		if len(intents) == 0 {
			if decisions[0].BlockedBy != BlockedBudgetSpent && decisions[0].BlockedBy != BlockedUnderOne {
				t.Fatalf("blocked by %q", decisions[0].BlockedBy)
			}
			return Intent{}, false
		}
		e.Apply(intents, []broker.Report{fullFill(intents[0], 1200)})
		return intents[0], true
	}
	sell := func(in Intent, price broker.Price) {
		o := in.Order
		o.Action, o.ClientID = broker.Sell, o.ClientID+"s"
		out := Intent{Order: o, Why: "value", Coin: in.Coin, Close: in.Close, Strike: in.Strike, Window: in.Window}
		if ev := e.Apply([]Intent{out}, []broker.Report{fullFill(out, price)}); ev[0].Kind != "sold" {
			t.Fatalf("%+v", ev)
		}
	}

	// The plan's trip: 739c bought, sold for about 400c. 58 at 0.0720: 417c premium, 28c fee, 389c.
	first, _ := buy("DOGE", doge, 1, 500)
	w := a.Windows[1000]
	sell(first, 720)
	if w.OpenCents != 0 || w.LostCents != 739-389 || a.CashCents != 100000-739+389 {
		t.Fatalf("after one losing trip: %+v cash %d", *w, a.CashCents)
	}
	sz := Size(SizeInput{PSide: 0.16, Asks: []broker.Price{1200}, StaleUnits: 70, Kappa: 0.25, CapBps: 2500, SeedCents: 100000,
		EquityCents: w.EquityCents, KMax: w.KMax, UsedCents: w.Used(), CashCents: a.CashCents})
	if sz.RoomCents != 389 {
		t.Fatalf("only the proceeds' worth of budget is free again: %+v", sz)
	}

	// Keep losing, on two coins of the same window, until the engine stops by itself.
	other := Market{Ticker: "KXSOL15M-T", MarketID: 103, Strike: 1, Close: 1000}
	trips := 1
	for i := int64(2); i < 200; i++ {
		coin, m := "DOGE", doge
		if i%2 == 0 {
			coin, m = "SOL", other
		}
		in, ok := buy(coin, m, i, 500+float64(i))
		if !ok {
			break
		}
		if w.Used() > 739 {
			t.Fatalf("trip %d: used %d passes the window's budget of 739c", trips, w.Used())
		}
		sell(in, 100) // nearly a total loss
		trips++
	}
	if trips < 2 || trips > 50 || w.LostCents > 739 || w.OpenCents != 0 {
		t.Fatalf("after %d losing trips: %+v", trips, *w)
	}
	for _, c := range []struct {
		coin string
		m    Market
	}{{"DOGE", doge}, {"SOL", other}} {
		if _, ok := buy(c.coin, c.m, 900, 800); ok {
			t.Fatalf("the window is shut for every coin, %s included", c.coin)
		}
	}
	if lost := 100000 - a.CashCents; lost > 739 {
		t.Fatalf("the window lost %dc, more than the 739c the rule says is its whole risk", lost)
	}

	// A winning sale frees its basis and no more.
	b := NewAccount(testScalper(t, 1, 0.007, 0.006), 32, 100000, true)
	e = testEngine(t, b)
	a = b
	win, _ := buy("DOGE", doge, 1, 500)
	sell(win, 5000)
	if w := b.Windows[1000]; w.OpenCents != 0 || w.LostCents != 0 || w.Used() != 0 {
		t.Fatalf("after a winning trip: %+v", *w)
	}
}

// Test 19, the rest: coin order within a window does not change the window's budget, and changes
// its total by less than one contract; Used never passes kappa * k_max * E_w nor the cap.
func TestWindowIsOneBetWhateverTheOrder(t *testing.T) {
	sol := Market{Ticker: "KXSOL15M-T", MarketID: 103, Strike: 1, Close: 1000}
	solBook := book([][2]string{{"0.4800", "1000"}}, [][2]string{{"0.5000", "1000"}})
	type look struct {
		coin string
		m    Market
		q    kalshi.Quotes
		p    float64
	}
	small, big := look{"DOGE", doge, dogeBook, 0.16}, look{"SOL", sol, solBook, 0.57206}
	var used []int64
	for _, order := range [][]look{{small, big}, {big, small}} {
		a := NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true)
		h := newHarness(t, a)
		for i, l := range order {
			h.step(l.coin, l.m, l.q, view(l.coin, l.p), 500+float64(i))
		}
		w := a.Windows[1000]
		if w.KMax < 0.1 || w.KMax > 0.1002 || w.Used() > 2500 {
			t.Fatalf("window %+v", *w)
		}
		used = append(used, w.Used())
	}
	if d := used[0] - used[1]; d > 52 || d < -52 { // one SOL contract is 51.75c: whole contracts are the only difference
		t.Fatalf("the window's total depends on coin order by more than a contract: %v", used)
	}

	// A long random sequence of sizings in one window: what is staked never takes Used past either bound.
	seed := uint64(1)
	next := func(n uint64) uint64 { seed = seed*6364136223846793005 + 1442695040888963407; return (seed >> 33) % n }
	for _, equity := range []int64{100000, 250000, 4000} {
		var usedCents int64
		kMax, cash := 0.0, equity
		for i := 0; i < 2000; i++ {
			ask := broker.Price(500 + next(9000))
			p := priceDollars(ask) + float64(next(3000))/10000
			sz := Size(SizeInput{PSide: p, Asks: []broker.Price{ask, ask + 100}, StaleUnits: 70, Kappa: 0.25, CapBps: 2500, SeedCents: 100000,
				EquityCents: equity, KMax: kMax, UsedCents: usedCents, CashCents: cash})
			if sz.Qty < 1 {
				continue
			}
			kMax = max(kMax, sz.Kelly)
			usedCents, cash = usedCents+sz.StakeCents, cash-sz.StakeCents
			if budget := floorCents(0.25, kMax, equity); usedCents > budget || usedCents > 2500*min(equity, 100000)/10000 || cash < 0 {
				t.Fatalf("equity %d step %d: used %d, budget %d", equity, i, usedCents, budget)
			}
			for j, s := range sz.Steps {
				if s.MaxCostCents > sz.StakeCents || (j > 0 && s.MaxCostCents > sz.Steps[j-1].MaxCostCents) {
					t.Fatalf("the ceilings must fall along the walk: %+v", sz.Steps)
				}
			}
		}
	}
}

// Rebuild equals live, as far as this package goes: the stored detail and fills of every order
// that filled, read back through JSON and folded by FoldRecorded into accounts made with the final
// cash, give the same money, positions and windows.
func TestFoldRecordedEqualsLive(t *testing.T) {
	calls := loadCalls(t, v2Fixtures[0])
	r := newReplay(t, calls[0], 0)
	live := []*Account{NewAccount(testScalper(t, 1, 0.007, 0.006), 1, 100000, true), NewAccount(testValue(t, 1, 0.007), 2, 100000, true)}
	h := newHarness(t, live...)
	type row struct {
		in     Intent
		detail []byte
		fills  []broker.Fill
	}
	var rows []row
	traded := map[string]bool{}
	for i, c := range calls[1:] {
		if c.Call == "on_settled" {
			var ticker string
			_ = jsonArg(c, 0, &ticker)
			if traded[ticker] {
				break // up to the first settlement of something traded: a rebuild reads only what has not settled
			}
		}
		s := r.feed(t, c)
		if s == nil || s.Price == 0 {
			continue
		}
		m := Market{Ticker: s.Market.Ticker, MarketID: marketID(s.Market.Ticker), EvaluationID: int64(i), Strike: s.Market.Strike, Close: s.Market.Close}
		v := r.model.View(s.Coin, m, s.Price, s.Now, r.v2Inputs(s.Coin))
		h.evalID = int64(i) - 1
		h.paper.ObserveBook(m.Ticker, int64(i), unixTime(s.Now), unixTime(m.Close), topOfBook(s.Market))
		decisions, intents := h.engine.Decide(s.Coin, m, topOfBook(s.Market), v, s.Now)
		h.engine.AfterDecide(m.Ticker, decisions)
		var reports []broker.Report
		for _, in := range intents {
			rep, err := h.paper.Submit(t.Context(), in.Order)
			if err != nil {
				t.Fatal(err)
			}
			blob, err := json.Marshal(h.engine.Detail(in, rep)) // BEFORE Apply: the basis a sale releases
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Fills) > 0 {
				traded[in.Order.Ticker] = true
				rows = append(rows, row{in, blob, rep.Fills})
			}
			reports = append(reports, rep)
			h.paper.Commit(in.Order.ClientID)
		}
		h.engine.Apply(intents, reports)
	}
	if len(rows) < 10 {
		t.Fatalf("only %d filled orders before the first settlement", len(rows))
	}

	rebuilt := []*Account{NewAccount(live[0].Params, 1, live[0].CashCents, true), NewAccount(live[1].Params, 2, live[1].CashCents, true)}
	e := testEngine(t, rebuilt...)
	var costs, payouts int64
	for _, rw := range rows {
		var detail map[string]any
		if err := json.Unmarshal(rw.detail, &detail); err != nil {
			t.Fatal(err)
		}
		o := rw.in.Order
		b, err := BookedFromRow(o.BucketID, o.MarketID, string(o.Action), string(o.Side), UnixSeconds(o.At), detail, rw.fills)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range e.FoldRecorded(b) {
			if ev.Kind == "inconsistent" {
				t.Fatalf("%+v", ev)
			}
		}
		// The broker's evidence is in the detail, and every fill fits inside it: taken at its level,
		// and never more than was displayed there less what this bucket already held.
		seen, _ := detail["seen"].([]any)
		if len(seen) == 0 {
			t.Fatalf("a filled order's detail has no seen: %s", rw.detail)
		}
		for _, f := range rw.fills {
			lv, _ := seen[f.Level].([]any)
			if len(lv) != 4 || lv[0] != float64(broker.TakerPrice(o.Action, f.Price)) || lv[3] != float64(f.Qty) || float64(f.Qty) > lv[1].(float64)-lv[2].(float64) {
				t.Fatalf("fill %+v against seen %v", f, seen)
			}
		}
		if o.Action == broker.Buy {
			costs += int64(detail["cost_cents"].(float64))
		} else {
			payouts += int64(detail["payout_cents"].(float64))
		}
	}
	if got := live[0].CashCents + live[1].CashCents; got != 200000-costs+payouts {
		t.Fatalf("cash %d is not the seed less detail.cost plus detail.payout (%d)", got, 200000-costs+payouts)
	}
	for i := range live {
		l, rb := live[i], rebuilt[i]
		if l.CashCents != rb.CashCents || l.Bets != rb.Bets || !reflect.DeepEqual(l.Windows, rb.Windows) || len(l.Positions) != len(rb.Positions) {
			t.Fatalf("%s: live cash %d bets %d windows %v; rebuilt cash %d bets %d windows %v", l.Params.Name,
				l.CashCents, l.Bets, l.Windows, rb.CashCents, rb.Bets, rb.Windows)
		}
		for key, lp := range l.Positions {
			a, b := *lp, *rb.Positions[key]
			// soft state a restart does not carry: the rule is evaluated afresh the next second
			a.Exiting, a.ExitTries, a.BlockedSeconds, b.Exiting, b.ExitTries, b.BlockedSeconds = "", 0, 0, "", 0, 0
			if a != b {
				t.Fatalf("%s %v: live %+v rebuilt %+v", l.Params.Name, key, a, b)
			}
		}
	}
	if _, err := BookedFromRow(1, 1, "buy", "yes", 0, map[string]any{"v": 2.0}, nil); err == nil {
		t.Error("a detail that is not version 3 must be refused")
	}
}

// Test 25, the half that lives here: no version without provenance, and none on a placeholder.
func TestParamsRefuseWhatIsNotMeasured(t *testing.T) {
	if _, err := Scalper(placeholder(0.2, 0.007, 0.006, 0.001)); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("a placeholder lambda must not make a version: %v", err)
	}
	if _, err := NewEngine(NewAccount(testScalper(t, 0.2, 0.007, 0.006), 1, 100000, true)); err == nil {
		t.Fatal("NewEngine must refuse params that may not be registered")
	}
	if _, err := Scalper(Measured{}); err == nil {
		t.Fatal("a version with no measurements must not be constructed")
	}
	// SHAPE ONLY: these are not measurements either. The test needs a Measured that is labelled
	// the way cmd/measure3 will label a real one, to show that such a one is accepted.
	shaped := func(v float64) Provenance {
		return Provenance{Kind: KindMeasured, Value: v, Note: "TEST SHAPE ONLY: not a measurement"}
	}
	m := Measured{Lambda: shaped(0.2), StaleCost: shaped(0.007), StaleCostSell: shaped(0.006), DriftTol: shaped(0.001), ProtocolSHA: "test", ResultSHA: "test"}
	for _, build := range []func(Measured) (Params, error){Scalper, Value} {
		p, err := build(m)
		if err != nil {
			t.Fatal(err)
		}
		for key := range p.numericFields() {
			if _, has := p.Provenance[key]; !has && !(p.Exit == "hold" && exitOnlyFields[key]) {
				t.Errorf("%s: %s has no provenance", p.Name, key)
			}
		}
		q := p
		q.Provenance = map[string]Provenance{}
		for k, v := range p.Provenance {
			q.Provenance[k] = v
		}
		delete(q.Provenance, "kappa")
		if err := q.Validate(); err == nil || !strings.Contains(err.Error(), "kappa has no provenance") {
			t.Errorf("a numeric field without provenance must be refused: %v", err)
		}
		q.Provenance["kappa"] = Provenance{Kind: KindConvention, Value: 0.5, Note: "x"}
		if err := q.Validate(); err == nil {
			t.Error("a provenance that disagrees with its field must be refused")
		}
	}
	for _, bad := range []Measured{
		{Lambda: shaped(0), StaleCost: shaped(0.007), StaleCostSell: shaped(0.006), DriftTol: shaped(0.001), ProtocolSHA: "a", ResultSHA: "b"},     // R1
		{Lambda: shaped(0.2), StaleCost: shaped(0.00705), StaleCostSell: shaped(0.006), DriftTol: shaped(0.001), ProtocolSHA: "a", ResultSHA: "b"}, // finer than 0.0001
		{Lambda: shaped(0.2), StaleCost: shaped(0.007), StaleCostSell: shaped(0.006), DriftTol: shaped(0.001)},                                     // no shas
		{Lambda: Provenance{Kind: KindConvention, Value: 0.5, Note: "a guess"}, StaleCost: shaped(0.007), StaleCostSell: shaped(0.006), DriftTol: shaped(0.001), ProtocolSHA: "a", ResultSHA: "b"},
	} {
		if _, err := Scalper(bad); err == nil {
			t.Errorf("must be refused: %+v", bad)
		}
	}
}

// The per-price ceilings are per POSITION, not per order. Measured in review through the real
// Paper: on a thin book that does not change, re-entries after min_gap each started the taper
// again from nothing and the position ended at 737c with 11 contracts bought at 0.14, where
// quarter-Kelly at 0.14 is 135c all told. Same params, same book, same figures here.
func TestReentryCannotPassTheCumulativeCeiling(t *testing.T) {
	a := NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true)
	h := newHarness(t, a)
	thin := book(dogeBook.YesBids, [][2]string{{"0.8800", "20"}, {"0.8700", "23.62"}, {"0.8600", "28.28"}})
	_, intents, reports, _ := h.step("DOGE", doge, thin, view("DOGE", 0.16), 500)
	first := []broker.CostStep{{UpTo: 1200, MaxCostCents: 739}, {UpTo: 1300, MaxCostCents: 440}, {UpTo: 1400, MaxCostCents: 135}}
	if !reflect.DeepEqual(intents[0].Order.CostSteps, first) || reports[0].Status != broker.Partial || intents[0].Detail["held_cents"] != int64(0) {
		t.Fatalf("the first order: %+v, %+v", intents[0].Order, reports[0])
	}
	pos := a.Position(doge.Ticker, "yes")
	if pos.Contracts != 33 || pos.CostCents != 435 { // 20 at 0.12 and 13 at 0.13: the taper works within one order
		t.Fatalf("position %+v", *pos)
	}

	// The book still DISPLAYS 20 at 0.12 (a paper order removes nothing), so every re-entry is
	// sized there. What the position already cost now counts against every step: 739 - 435 at
	// 0.12, 440 - 435 at 0.13, and nothing at 0.14.
	again := []broker.CostStep{{UpTo: 1200, MaxCostCents: 304}, {UpTo: 1300, MaxCostCents: 5}, {UpTo: 1400, MaxCostCents: 0}}
	for _, now := range []float64{509, 518, 527, 536} {
		decisions, intents, reports, events := h.step("DOGE", doge, thin, view("DOGE", 0.16), now)
		if len(intents) != 1 || !reflect.DeepEqual(intents[0].Order.CostSteps, again) || intents[0].Order.MaxCostCents != 304 || intents[0].Detail["held_cents"] != int64(435) {
			t.Fatalf("t=%v: the re-entry: %+v %+v", now, decisions, intents)
		}
		if reports[0].Status != broker.Cancelled || reports[0].Reason != broker.ReasonCeiling || events[0].Kind != "entry_unfilled" {
			t.Fatalf("t=%v: %+v", now, reports[0])
		}
		// Never past quarter-Kelly at the worst price it holds a fill at (440c at 0.13), and nothing at 0.14.
		if pos.Contracts != 33 || pos.CostCents != 435 || pos.CostCents > 440 || pos.Entries != 1 || a.Windows[1000].Used() != 435 || a.CashCents != 100000-435 {
			t.Fatalf("t=%v: position %+v window %+v", now, *pos, a.Windows[1000])
		}
		for _, lv := range reports[0].Seen {
			if lv.Bid == 8600 && lv.Taken != 0 {
				t.Fatalf("t=%v: a fill at 0.14: %+v", now, reports[0].Seen)
			}
		}
	}

	// When 0.12 really is offered again the rest of quarter-Kelly AT 0.12 may be bought, and no more.
	_, intents, reports, _ = h.step("DOGE", doge, dogeBook, view("DOGE", 0.16), 545)
	if len(intents) != 1 || intents[0].Order.Qty != 23 || reports[0].Status != broker.Filled || pos.CostCents != 435+294 || pos.CostCents > 739 {
		t.Fatalf("with 0.12 on offer again: %+v, position %+v", intents, *pos)
	}
	// ...after which the position holds quarter-Kelly of this market and asks for nothing.
	decisions, intents, _, _ := h.step("DOGE", doge, dogeBook, view("DOGE", 0.16), 554)
	if len(intents) != 0 || decisions[0].BlockedBy != BlockedUnderOne {
		t.Fatalf("10c of room buys nothing at 12.74c: %+v", decisions)
	}

	in := SizeInput{PSide: 0.16, Asks: []broker.Price{1200, 1300, 1400}, StaleUnits: 70, Kappa: 0.25, CapBps: 2500, SeedCents: 100000,
		EquityCents: 100000, KMax: 0.5, UsedCents: 739, CashCents: 99261, HeldCents: 739}
	if sz := Size(in); sz.Qty != 0 || sz.BlockedBy != BlockedHeldKelly {
		t.Fatalf("a position already at quarter-Kelly of its market: %+v", sz)
	}
	in.HeldCents = 435
	if sz := Size(in); sz.Qty != 23 || sz.StakeCents != 304 || !reflect.DeepEqual(sz.Steps, again) {
		t.Fatalf("held 435c: %+v", sz)
	}
}

// The order detail carries the broker's evidence, per level [bid e4, displayed, held, taken], and
// as [] (never null) when the order reached no book.
func TestDetailCarriesWhatTheBrokerSaw(t *testing.T) {
	a := NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true)
	h := newHarness(t, a)
	thin := book(dogeBook.YesBids, [][2]string{{"0.8800", "20"}, {"0.8700", "23.62"}, {"0.8600", "28.28"}})
	var details []map[string]any
	for _, now := range []float64{500, 509} {
		h.evalID++
		m := doge
		m.EvaluationID = h.evalID
		h.paper.ObserveBook(m.Ticker, m.EvaluationID, unixTime(now), unixTime(m.Close), thin)
		_, intents := h.engine.Decide("DOGE", m, thin, view("DOGE", 0.16), now)
		rep, err := h.paper.Submit(t.Context(), intents[0].Order)
		if err != nil {
			t.Fatal(err)
		}
		blob, err := json.Marshal(h.engine.Detail(intents[0], rep))
		if err != nil {
			t.Fatal(err)
		}
		var detail map[string]any
		if err := json.Unmarshal(blob, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
		h.engine.Apply(intents, []broker.Report{rep})
		h.paper.Commit(intents[0].Order.ClientID)
	}
	want := [][]any{
		{[]any{8800.0, 20.0, 0.0, 20.0}, []any{8700.0, 23.0, 0.0, 13.0}, []any{8600.0, 28.0, 0.0, 0.0}},
		{[]any{8800.0, 20.0, 20.0, 0.0}, []any{8700.0, 23.0, 13.0, 0.0}, []any{8600.0, 28.0, 0.0, 0.0}}, // the re-entry sees its own holds
	}
	for i, detail := range details {
		got, _ := detail["seen"].([]any)
		if len(got) != len(want[i]) {
			t.Fatalf("order %d: seen %v", i, detail["seen"])
		}
		for j := range got {
			if !reflect.DeepEqual(got[j], want[i][j]) {
				t.Fatalf("order %d level %d: seen %v, want %v", i, j, got[j], want[i][j])
			}
		}
	}
	blob, err := json.Marshal(h.engine.Detail(Intent{Order: broker.Order{BucketID: 31}, Detail: map[string]any{"v": 3}}, broker.Report{Status: broker.Rejected}))
	if err != nil || !strings.Contains(string(blob), `"seen":[]`) {
		t.Fatalf("an order that saw no book stores seen as []: %s %v", blob, err)
	}
}

// An early sale never books nothing or less, and never takes cash below zero. The review's probe:
// one contract, a bid of 0.0091, cash 0. The plan's per-contract test calls it a sale worth making
// (0.0091 - 0.00063 - 0.006 = 0.00247 > p 0.001); the ledger would book 0c of premium and 1c of fee.
func TestSaleNeverBooksNothing(t *testing.T) {
	hold := func(contracts int, premium, fee, cash int64) (*harness, *Account, *Position) {
		h, a := holding(t, 0.006, contracts, premium, fee)
		a.CashCents = cash
		return h, a, a.Position(doge.Ticker, "yes")
	}
	no := [][2]string{{"0.9600", "100"}}
	for _, c := range []struct {
		name      string
		contracts int
		yesBids   [][2]string
	}{
		{"the review's probe: one contract at 0.0091 would COST a cent", 1, [][2]string{{"0.0091", "100"}}},
		{"one contract at 0.0150 books 1c of premium and 1c of fee", 1, [][2]string{{"0.0150", "100"}}},
		{"a thousand at 0.0150 would book 1396c, but a fill of ONE of them books nothing", 1000, [][2]string{{"0.0150", "5000"}}},
	} {
		h, a, pos := hold(c.contracts, 13*int64(c.contracts), 0, 0)
		for i := 1; i <= 2; i++ {
			decisions, intents, _, _ := h.step("DOGE", doge, book(c.yesBids, no), view("DOGE", 0.001), 500+float64(i))
			if len(intents) != 0 || len(decisions) != 1 || decisions[0].BlockedBy != BlockedExitNoProceeds || decisions[0].Why != "value" || decisions[0].Action != "none" {
				t.Fatalf("%s: %+v %+v", c.name, decisions, intents)
			}
			if a.CashCents != 0 || pos.Contracts != c.contracts || pos.BlockedSeconds != i || pos.ExitTries != 0 {
				t.Fatalf("%s: held to settlement, and the second is counted: cash %d position %+v", c.name, a.CashCents, *pos)
			}
		}
	}

	// A bid worth selling at above a bid that is not: the walk stops above it.
	h, a, pos := hold(50, 650, 0, 0)
	_, intents, reports, _ := h.step("DOGE", doge, book([][2]string{{"0.0300", "10"}, {"0.0150", "100"}}, no), view("DOGE", 0.001), 500)
	if len(intents) != 1 || intents[0].Order.Limit != 300 || reports[0].Status != broker.Partial || pos.Contracts != 40 || a.CashCents != 30-3 {
		t.Fatalf("the limit stays where one contract still books something: %+v cash %d", intents, a.CashCents)
	}
	// With no bid displayed the pending exit is still sent, and never under that line either.
	_, intents, _, _ = h.step("DOGE", doge, book(nil, no), view("DOGE", 0.001), 501)
	if len(intents) != 1 || saleCanBookNothing(intents[0].Order.Limit) {
		t.Fatalf("the limit of an exit on an empty side: %+v", intents)
	}
}

// The line saleCanBookNothing draws is the right one, by the broker's own arithmetic: below it one
// contract books nothing or less; at or above it every fill of every size books at least a cent.
// An order is one or more such fills at prices no worse than its limit, and its fee rounded once
// is never more than its fills' fees rounded one by one, so the order books at least a cent too.
func TestSaleProceedsArePositive(t *testing.T) {
	line := broker.Price(0)
	for b := broker.Price(1); b <= maxPrice; b++ {
		if !saleCanBookNothing(b) {
			line = b
			break
		}
	}
	for b := broker.Price(1); b <= maxPrice; b++ {
		if saleCanBookNothing(b) != (b < line) {
			t.Fatalf("the prices at which one contract books nothing are not one run from the bottom: %d, line %d", b, line)
		}
		if b < line {
			continue
		}
		for _, qty := range append([]int{1000, 12345, 1_000_000, broker.MaxQty}, seq(1, 300)...) {
			if got := bookedSaleCents(qty, b); got < 1 {
				t.Fatalf("%d sold at %d books %dc", qty, b, got)
			}
		}
	}
	if bookedSaleCents(1, 91) != -1 || bookedSaleCents(1, 150) != 0 {
		t.Fatal("the review's two figures")
	}
	t.Logf("with the broker's present rounding, one contract books nothing under a bid of %d (ten-thousandths)", line)
}

func seq(from, to int) []int {
	out := make([]int, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// The last test before an order is formed is made with the ORDER's booked figures: the fee the
// broker will charge is the order's fee rounded up to a cent, not 0.07 c (1 - c) a contract.
func TestNoOrderAtANegativeEdgeAfterTheRealFee(t *testing.T) {
	// The review's entry: ask 0.50, p 0.5265, stale 0.007. Per contract the edge is 0.0020 and the
	// stake 105c buys 2; they book 100c + ceil(3.5c) = 104c against 105.3c - 1.4c = 103.9c believed.
	a := NewAccount(testValue(t, 1, 0.007), 40, 100000, true)
	h := newHarness(t, a)
	mid := book([][2]string{{"0.4900", "1000"}}, [][2]string{{"0.5000", "1000"}})
	decisions, intents, _, _ := h.step("DOGE", doge, mid, view("DOGE", 0.5265), 500)
	if len(intents) != 0 || decisions[0].BlockedBy != BlockedNoEdgeRounded || !(decisions[0].Edge > 0.0019) {
		t.Fatalf("an order the rounded fee turns into a loss: %+v %+v", decisions, intents)
	}
	// A little more belief and the same two contracts are worth more than the 104c they book.
	decisions, intents, reports, _ := h.step("DOGE", doge, mid, view("DOGE", 0.53), 501)
	if len(intents) != 1 || reports[0].Status != broker.Filled {
		t.Fatalf("p 0.53: %+v", decisions)
	}
	o, f := intents[0].Order, reports[0].Fills[0]
	if paid := f.PremiumCents + f.FeeCents; paid != bookedBuyCents(o.Qty, o.Limit) || paid > o.MaxCostCents || !(0.53*100*float64(o.Qty)-0.7*float64(o.Qty) > float64(paid)) {
		t.Fatalf("the order was tested with other figures than it was booked at: %+v %+v", o, f)
	}

	// Over a spread of beliefs, prices and sizes: no order Size forms is believed to pay less than
	// the broker books for it, and none asks for more contracts than its stake books.
	seed := uint64(7)
	next := func(n uint64) uint64 { seed = seed*6364136223846793005 + 1442695040888963407; return (seed >> 33) % n }
	formed, refused := 0, 0
	for i := 0; i < 20000; i++ {
		ask := broker.Price(300 + next(9400))
		p := priceDollars(ask) + float64(next(600))/10000
		sz := Size(SizeInput{PSide: p, Asks: []broker.Price{ask}, StaleUnits: 70, Kappa: 0.25, CapBps: 2500, SeedCents: 100000,
			EquityCents: int64(2000 + next(100000)), CashCents: 100000})
		if sz.BlockedBy == BlockedNoEdgeRounded {
			refused++
		}
		if sz.Qty < 1 {
			continue
		}
		formed++
		paid := bookedBuyCents(sz.Qty, ask)
		if paid > sz.StakeCents || !(p*100*float64(sz.Qty)-0.7*float64(sz.Qty) > float64(paid)) {
			t.Fatalf("ask %d p %v: %d contracts book %dc under a stake of %dc", ask, p, sz.Qty, paid, sz.StakeCents)
		}
	}
	if formed < 1000 || refused < 100 {
		t.Fatalf("the spread showed little: %d formed, %d refused for rounding", formed, refused)
	}

	// Exits. One contract held at 13c, a bid of 0.1790, p 0.16: per contract 0.1790 - 0.0103 - 0.006
	// = 0.1627 > 0.16, but the sale books 17c - 2c = 15c, less 0.6c of staleness: under the 16c it
	// is believed to pay. Nothing is sent. At 0.20 it books 18c, and the sale goes.
	he, _ := holding(t, 0.006, 1, 12, 1)
	if _, intents, _, _ := he.step("DOGE", doge, book([][2]string{{"0.1790", "100"}}, [][2]string{{"0.8000", "100"}}), view("DOGE", 0.16), 500); len(intents) != 0 {
		t.Fatalf("a sale the rounded fee turns into a loss: %+v", intents[0].Order)
	}
	_, intents, reports, _ = he.step("DOGE", doge, book([][2]string{{"0.2000", "100"}}, [][2]string{{"0.7900", "100"}}), view("DOGE", 0.16), 501)
	if len(intents) != 1 || intents[0].Why != "value" || reports[0].Fills[0].PremiumCents-reports[0].Fills[0].FeeCents != 18 {
		t.Fatalf("a sale worth making after the real fee: %+v", intents)
	}
}

// Kelly, the window's k_max and the band and edge tests are taken at the best ask that shows at
// least ONE whole contract: the price the broker will really give. The review's book: 0.62 of a
// contract at 0.12 and 500 at 0.13.
func TestSizedAtThePriceTheBrokerWillGive(t *testing.T) {
	a := NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true)
	h := newHarness(t, a)
	dust := book(dogeBook.YesBids, [][2]string{{"0.8800", "0.62"}, {"0.8700", "500"}})
	decisions, intents, reports, _ := h.step("DOGE", doge, dust, view("DOGE", 0.16), 500)
	if len(intents) != 1 {
		t.Fatalf("%+v", decisions)
	}
	o := intents[0].Order
	wantSteps := []broker.CostStep{{UpTo: 1300, MaxCostCents: 440}}
	if k := intents[0].Kelly; k < 0.017638 || k > 0.017640 || o.MaxCostCents != 440 || !reflect.DeepEqual(o.CostSteps, wantSteps) || o.Limit != 1300 {
		t.Fatalf("sized at 0.13, where the first whole contract is: kelly %v order %+v", intents[0].Kelly, o)
	}
	if e := decisions[0].Edge; e < 0.0150 || e > 0.0152 { // 0.16 - 0.13 - 0.007917 - 0.007
		t.Errorf("the edge is the edge at 0.13: %v", e)
	}
	if decisions[0].MarketProb != (0.10+0.12)/2 {
		t.Errorf("the mid is still the displayed touch: %v", decisions[0].MarketProb)
	}
	w := a.Windows[1000]
	if reports[0].Status != broker.Filled || reports[0].Fills[0].Price != 1300 || w.KMax != intents[0].Kelly || floorCents(0.25, w.KMax, w.EquityCents) != 440 {
		t.Fatalf("the window's best bet is the one that could be had: %+v %+v", reports[0], *w)
	}
	// So a second coin with k = 0.02 gets quarter-Kelly of THAT, 500c, less what is used; not room under a 739c that never was.
	sz := Size(SizeInput{PSide: 0.5340, Asks: []broker.Price{5000}, StaleUnits: 70, Kappa: 0.25, CapBps: 2500, SeedCents: 100000,
		EquityCents: w.EquityCents, KMax: w.KMax, UsedCents: w.Used(), CashCents: a.CashCents})
	if want := floorCents(0.25, sz.Kelly, 100000) - w.Used(); sz.BudgetCents-w.Used() != want || sz.RoomCents != want {
		t.Fatalf("the second coin's room: %+v, want %d", sz, want)
	}

	// The band test too: 0.62 of a contract at 0.95 and the first whole one at 0.96 is outside Scalper's band.
	b := NewAccount(testScalper(t, 1, 0.007, 0.006), 32, 100000, true)
	hb := newHarness(t, b)
	decisions, intents, _, _ = hb.step("DOGE", doge, book([][2]string{{"0.9400", "100"}}, [][2]string{{"0.0500", "0.62"}, {"0.0400", "100"}}), view("DOGE", 0.999), 500)
	if len(intents) != 0 || decisions[0].BlockedBy != BlockedBand {
		t.Fatalf("the band is tested at the price a contract can be had at: %+v", decisions)
	}
	// Nothing whole on the side at all: no order, and the decision says why.
	decisions, intents, _, _ = hb.step("DOGE", doge, book([][2]string{{"0.1000", "0.5"}}, [][2]string{{"0.8800", "0.62"}, {"0.8700", "0.9"}}), view("DOGE", 0.16), 501)
	if len(intents) != 0 || decisions[0].BlockedBy != BlockedNoSize || decisions[0].Edge != 0 || decisions[0].Side != "" {
		t.Fatalf("less than a contract on every level: %+v", decisions)
	}
}

// The buy limit never leaves the version's price band: no fill can land above band_max. Scalper's
// band ends at 0.95; at p 0.999 every ask up to 0.99 still has an edge.
func TestLimitStaysInsideTheBand(t *testing.T) {
	a := NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true)
	h := newHarness(t, a)
	high := book([][2]string{{"0.9300", "100"}}, [][2]string{{"0.0600", "3"}, {"0.0500", "4"}, {"0.0400", "500"}, {"0.0300", "500"}, {"0.0100", "500"}})
	decisions, intents, reports, _ := h.step("DOGE", doge, high, view("DOGE", 0.999), 500)
	if len(intents) != 1 {
		t.Fatalf("%+v", decisions)
	}
	o := intents[0].Order
	if o.Limit != 9500 || len(o.CostSteps) != 2 || o.CostSteps[1].UpTo != 9500 || o.Qty <= 7 {
		t.Fatalf("the limit is the last ask inside the band, 0.95, though 0.96 to 0.99 have an edge: %+v", o)
	}
	for _, f := range reports[0].Fills {
		if f.Price > 9500 {
			t.Fatalf("a fill above band_max: %+v", f)
		}
	}
	if reports[0].Status != broker.Partial || reports[0].Unfilled != o.Qty-7 {
		t.Fatalf("3 at 0.94 and 4 at 0.95 are all the band holds: %+v", reports[0])
	}
}

// The order's time keeps its microseconds: unixTime(UnixSeconds(t)) is t, so the placed_at a
// rebuild reads back is the time the live fold used. Every microsecond of one second is tried,
// the second the review measured a 50% miss on.
func TestOrderTimeKeepsMicroseconds(t *testing.T) {
	base := time.Unix(1789976700, 0).UTC()
	for us := 0; us < 1_000_000; us++ {
		at := base.Add(time.Duration(us) * time.Microsecond)
		if got := unixTime(UnixSeconds(at)); !got.Equal(at) {
			t.Fatalf("%v came back as %v", at, got)
		}
	}
	if got := unixTime(1789976700.9999996); !got.Equal(base.Add(time.Second)) {
		t.Fatalf("a reading that rounds up to the next second must carry: %v", got)
	}
	// Through an order: what BookedFrom folds is what the rebuild computes from placed_at.
	at := base.Add(123457 * time.Microsecond)
	e := testEngine(t, NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true))
	m := doge
	m.Close = UnixSeconds(base) + 500
	_, intents := e.Decide("DOGE", m, dogeBook, view("DOGE", 0.16), UnixSeconds(at))
	if len(intents) != 1 || !intents[0].Order.At.Equal(at) || BookedFrom(intents[0], broker.Report{}).At != UnixSeconds(at) {
		t.Fatalf("the order's time: %+v", intents)
	}
}

// An engine HOLDS a settle-only account whatever its params say: holding and settling need none.
// Only ordering does, so only an account that may order is validated, and one that was not can
// never order.
func TestSettleOnlyAccountIsHeldWhateverItsParams(t *testing.T) {
	stale := testScalper(t, 1, 0.007, 0.006)
	stale.Provenance = map[string]Provenance{} // as a params row stored before a numeric field was added would read
	if stale.Validate() == nil || stale.ValidatePlumbing() == nil {
		t.Fatal("the fixture must not validate")
	}
	position := func() *Position {
		return &Position{Coin: "DOGE", Ticker: doge.Ticker, Side: "yes", MarketID: doge.MarketID, Contracts: 58, CostCents: 739, PremiumCents: 696, LastBuyAt: 400, Close: 1000}
	}
	for _, build := range []func(...*Account) (*Engine, error){NewEngine, NewPlumbingEngine} {
		if _, err := build(NewAccount(stale, 1, 100000, true)); err == nil {
			t.Fatal("an account that may ORDER on params that do not validate must still be refused")
		}
		held, empty := NewAccount(stale, 1, 5000, false), NewAccount(Params{}, 2, 50, false)
		held.Positions[posKey{doge.Ticker, "yes"}], empty.Positions[posKey{doge.Ticker, "yes"}] = position(), position()
		e, err := build(held, empty)
		if err != nil {
			t.Fatalf("a settle-only account must be held: %v", err)
		}
		m := doge
		m.EvaluationID = 1
		high := book([][2]string{{"0.9000", "500"}}, [][2]string{{"0.0900", "500"}})
		if ds, ins := e.Decide("DOGE", m, high, view("DOGE", 0.99), 500); len(ds) != 0 || len(ins) != 0 {
			t.Fatalf("settle-only sends nothing: %+v", ds)
		}
		held.MayOrder = true // flipped after the engine was built: the params were never checked, so still nothing
		if ds, ins := e.Decide("DOGE", m, high, view("DOGE", 0.99), 500); len(ds) != 0 || len(ins) != 0 {
			t.Fatalf("an account whose params were never validated formed an order: %+v", ins)
		}
		if rows := e.SettleRows(doge.Ticker, "yes"); len(rows) != 2 || rows[0].PayoutCents != 5800 || rows[1].PayoutCents != 5800 {
			t.Fatalf("rows %+v", rows)
		}
		events := e.ApplySettlement(doge.Ticker, "yes")
		if held.CashCents != 5000+5800 || empty.CashCents != 50+5800 || len(held.Positions) != 0 || len(empty.Positions) != 0 || len(events) != 2 {
			t.Fatalf("settled: %+v", events)
		}
	}
}
