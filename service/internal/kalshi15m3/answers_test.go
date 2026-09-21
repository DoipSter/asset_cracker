package kalshi15m3

import (
	"reflect"
	"strings"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/broker"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// Every answer the broker can give, for an entry and for an exit, made by a REAL Paper and driven
// through Apply (plan 4.4's table). Where the engine's own orders never draw an answer (a stale
// snapshot, a crossed book, a closed market, a malformed order) the order or the Paper's book is
// changed by hand between the decision and the broker; the reaction is the engine's either way.
//
// The reaction depends on three things only: buy or sell, whether anything filled, and whether the
// order was rejected before it reached a book. The table shows that for every reason there is.
func TestEveryBrokerAnswer(t *testing.T) {
	const now = 500.0
	oneLevel := book(dogeBook.YesBids, [][2]string{{"0.8800", "20"}})
	thin := book(dogeBook.YesBids, [][2]string{{"0.8800", "20"}, {"0.8700", "23.62"}, {"0.8600", "28.28"}})
	rich := book([][2]string{{"0.6000", "10"}, {"0.5900", "5"}, {"0.2000", "100"}}, [][2]string{{"0.3800", "100"}})
	oneBid := book([][2]string{{"0.6000", "10"}}, [][2]string{{"0.3800", "100"}})
	crossed := book([][2]string{{"0.6000", "10"}}, [][2]string{{"0.5000", "10"}})

	observe := func(q kalshi.Quotes) func(*harness, Market, []Intent) {
		return func(h *harness, m Market, _ []Intent) {
			h.paper.ObserveBook(m.Ticker, m.EvaluationID, unixTime(now), unixTime(m.Close), q)
		}
	}
	order := func(change func(*broker.Order)) func(*harness, Market, []Intent) {
		return func(_ *harness, _ Market, ins []Intent) { change(&ins[0].Order) }
	}
	earlier := func(q kalshi.Quotes, p float64) func(*harness, *Account) {
		return func(h *harness, _ *Account) { h.step("DOGE", doge, q, view("DOGE", p), 480) }
	}

	cases := []struct {
		name   string
		exit   bool
		book   kalshi.Quotes
		p      float64
		prep   func(*harness, *Account)
		tamper func(*harness, Market, []Intent)
		status broker.Status
		reason string // the report's reason begins with this
		filled int
	}{
		// ---- entries: Scalper, p 0.16 on the recorded DOGE book, 58 wanted at up to 0.14
		{name: "entry filled", book: dogeBook, p: 0.16, status: broker.Filled, filled: 58},
		{name: "entry partial, the book ran out", book: oneLevel, p: 0.16, status: broker.Partial, reason: broker.ReasonNothingMore, filled: 20},
		{name: "entry partial, the ceiling cut it", book: thin, p: 0.16, status: broker.Partial, reason: broker.ReasonCeiling, filled: 33},
		{name: "entry cancelled, no bids", book: dogeBook, p: 0.16, tamper: observe(book(dogeBook.YesBids, nil)), status: broker.Cancelled, reason: broker.ReasonNoBids},
		{name: "entry cancelled, outside the limit", book: dogeBook, p: 0.16, tamper: observe(book(dogeBook.YesBids, [][2]string{{"0.8000", "100"}})),
			status: broker.Cancelled, reason: broker.ReasonOutsideLimit},
		{name: "entry cancelled, already taken", book: oneLevel, p: 0.16, prep: earlier(oneLevel, 0.16), status: broker.Cancelled, reason: broker.ReasonAlreadyTaken},
		{name: "entry cancelled, the ceiling", book: thin, p: 0.16, prep: earlier(thin, 0.16), status: broker.Cancelled, reason: broker.ReasonCeiling},
		{name: "entry rejected, stale book", book: dogeBook, p: 0.16, tamper: order(func(o *broker.Order) { o.EvaluationID++ }), status: broker.Rejected, reason: broker.ReasonStaleBook},
		{name: "entry rejected, crossed book", book: dogeBook, p: 0.16, tamper: observe(crossed), status: broker.Rejected, reason: broker.ReasonCrossed},
		{name: "entry rejected, closed", book: dogeBook, p: 0.16, tamper: order(func(o *broker.Order) { o.At = unixTime(doge.Close) }), status: broker.Rejected, reason: broker.ReasonClosed},
		{name: "entry rejected, unreadable side", book: dogeBook, p: 0.16, tamper: observe(book(dogeBook.YesBids, [][2]string{{"0.8800", "many"}})),
			status: broker.Rejected, reason: broker.ReasonUnreadable},
		{name: "entry rejected, no book", book: dogeBook, p: 0.16, tamper: func(h *harness, m Market, _ []Intent) { h.paper.Forget(m.Ticker) }, status: broker.Rejected, reason: broker.ReasonNoBook},
		{name: "entry rejected, bad quantity", book: dogeBook, p: 0.16, tamper: order(func(o *broker.Order) { o.Qty = 0 }), status: broker.Rejected, reason: broker.ReasonBadQty},
		{name: "entry rejected, bad limit", book: dogeBook, p: 0.16, tamper: order(func(o *broker.Order) { o.Limit = 0 }), status: broker.Rejected, reason: broker.ReasonBadLimit},
		{name: "entry rejected, bad ceiling", book: dogeBook, p: 0.16, tamper: order(func(o *broker.Order) { o.MaxCostCents = -1 }), status: broker.Rejected, reason: broker.ReasonBadCeiling},
		{name: "entry rejected, no client id", book: dogeBook, p: 0.16, tamper: order(func(o *broker.Order) { o.ClientID = "" }), status: broker.Rejected, reason: broker.ReasonNoClientID},
		{name: "entry rejected, not an order", book: dogeBook, p: 0.16, tamper: order(func(o *broker.Order) { o.Side = "maybe" }), status: broker.Rejected, reason: broker.ReasonBadOrder},

		// ---- exits: 50 yes held for 637c, overpaid against p 0.30 down to a bid of 0.59
		{name: "exit filled", exit: true, book: book([][2]string{{"0.6000", "100"}}, rich.NoBids), p: 0.30, status: broker.Filled, filled: 50},
		{name: "exit partial, the book ran out", exit: true, book: rich, p: 0.30, status: broker.Partial, reason: broker.ReasonNothingMore, filled: 15},
		{name: "exit partial, dust", exit: true, book: rich, p: 0.30, status: broker.Partial, reason: broker.ReasonDust, filled: 10,
			tamper: func(h *harness, m Market, ins []Intent) {
				// the engine never offers under two cents; made by hand so that the broker's answer exists
				ins[0].Order.Limit = 1
				observe(book([][2]string{{"0.6000", "10"}, {"0.0050", "1"}}, rich.NoBids))(h, m, ins)
			}},
		{name: "exit cancelled, no bids", exit: true, book: book(nil, rich.NoBids), p: 0.30, prep: func(_ *harness, a *Account) { a.Position(doge.Ticker, "yes").Exiting = "value" },
			status: broker.Cancelled, reason: broker.ReasonNoBids},
		{name: "exit cancelled, outside the limit", exit: true, book: rich, p: 0.30, tamper: observe(book([][2]string{{"0.5000", "100"}}, rich.NoBids)),
			status: broker.Cancelled, reason: broker.ReasonOutsideLimit},
		{name: "exit cancelled, already taken", exit: true, book: oneBid, p: 0.30, prep: earlier(oneBid, 0.30), status: broker.Cancelled, reason: broker.ReasonAlreadyTaken},
		{name: "exit rejected, stale book", exit: true, book: rich, p: 0.30, tamper: order(func(o *broker.Order) { o.EvaluationID++ }), status: broker.Rejected, reason: broker.ReasonStaleBook},
		{name: "exit rejected, crossed book", exit: true, book: rich, p: 0.30, tamper: observe(crossed), status: broker.Rejected, reason: broker.ReasonCrossed},
		{name: "exit rejected, closed", exit: true, book: rich, p: 0.30, tamper: order(func(o *broker.Order) { o.At = unixTime(doge.Close) }), status: broker.Rejected, reason: broker.ReasonClosed},
		{name: "exit rejected, unreadable side", exit: true, book: rich, p: 0.30, tamper: observe(book([][2]string{{"0.6000", "many"}}, rich.NoBids)),
			status: broker.Rejected, reason: broker.ReasonUnreadable},
		{name: "exit rejected, no book", exit: true, book: rich, p: 0.30, tamper: func(h *harness, m Market, _ []Intent) { h.paper.Forget(m.Ticker) }, status: broker.Rejected, reason: broker.ReasonNoBook},
		{name: "exit rejected, bad quantity", exit: true, book: rich, p: 0.30, tamper: order(func(o *broker.Order) { o.Qty = 0 }), status: broker.Rejected, reason: broker.ReasonBadQty},
		{name: "exit rejected, bad limit", exit: true, book: rich, p: 0.30, tamper: order(func(o *broker.Order) { o.Limit = 0 }), status: broker.Rejected, reason: broker.ReasonBadLimit},
		{name: "exit rejected, no client id", exit: true, book: rich, p: 0.30, tamper: order(func(o *broker.Order) { o.ClientID = "" }), status: broker.Rejected, reason: broker.ReasonNoClientID},
		{name: "exit rejected, not an order", exit: true, book: rich, p: 0.30, tamper: order(func(o *broker.Order) { o.Action = "swap" }), status: broker.Rejected, reason: broker.ReasonBadOrder},
	}

	covered := map[string]bool{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var h *harness
			var a *Account
			if c.exit {
				h, a = holding(t, 0.006, 50, 600, 37)
			} else {
				a = NewAccount(testScalper(t, 1, 0.007, 0.006), 31, 100000, true)
				h = newHarness(t, a)
			}
			if c.prep != nil {
				c.prep(h, a)
			}
			before := cloneAccounts(h.engine.Accounts)[0]
			var was Position
			if p := a.Position(doge.Ticker, "yes"); p != nil {
				was = *p
			}
			var tamper func(Market, []Intent)
			if c.tamper != nil {
				tamper = func(m Market, ins []Intent) {
					if len(ins) != 1 {
						t.Fatalf("want one order to change, got %d", len(ins))
					}
					c.tamper(h, m, ins)
				}
			}
			decisions, intents, reports, events := h.stepWith("DOGE", doge, c.book, view("DOGE", c.p), now, tamper)
			if len(intents) != 1 || len(reports) != 1 || len(events) == 0 {
				t.Fatalf("want one order, one answer and an event: decisions %+v events %+v", decisions, events)
			}
			in, r := intents[0], reports[0]
			if (in.Order.Action == broker.Sell) != c.exit && in.Order.Action != "swap" {
				t.Fatalf("the order is a %s", in.Order.Action)
			}
			filled := 0
			for _, f := range r.Fills {
				filled += f.Qty
			}
			if r.Status != c.status || !strings.HasPrefix(r.Reason, c.reason) || (c.reason == "") != (r.Reason == "") || filled != c.filled {
				t.Fatalf("the broker answered %s %q with %d filled; want %s %q with %d", r.Status, r.Reason, filled, c.status, c.reason, c.filled)
			}
			covered[string(c.status)+"|"+c.reason] = true
			pos := a.Position(doge.Ticker, "yes")
			w := a.Windows[1000]

			switch {
			case !c.exit && filled > 0: // the position and the window grow; ONE bet; the gap starts
				var cost int64
				for _, f := range r.Fills {
					cost += f.PremiumCents + f.FeeCents
				}
				if events[0].Kind != "bought" || pos == nil || pos.Contracts != was.Contracts+filled || pos.CostCents != was.CostCents+cost ||
					pos.Entries != was.Entries+1 || pos.LastBuyAt != now || a.Bets != before.Bets+1 || a.CashCents != before.CashCents-cost ||
					w == nil || w.OpenCents != pos.CostCents || w.KMax != in.Kelly {
					t.Fatalf("after a buy of %d for %dc: event %+v position %+v window %+v cash %d", filled, cost, events[0], pos, w, a.CashCents)
				}
			case !c.exit: // nothing happened: no bet, no gap, no window, no cash
				if events[0].Kind != "entry_unfilled" || !reflect.DeepEqual(before, cloneAccounts(h.engine.Accounts)[0]) {
					t.Fatalf("an entry with nothing filled changed the account: event %+v account %+v", events[0], *a)
				}
			case filled == was.Contracts: // the position closes
				if events[0].Kind != "sold" || pos != nil || w.OpenCents != 0 || a.CashCents != before.CashCents+events[0].CashCents || events[0].CashCents <= 0 {
					t.Fatalf("after selling all: event %+v position %+v window %+v", events[0], pos, w)
				}
			case filled > 0: // the position shrinks and the exit stays wanted
				if events[0].Kind != "sold" || pos == nil || pos.Contracts != was.Contracts-filled || pos.Exiting != in.Why || pos.ExitTries != 0 ||
					pos.BlockedSeconds != was.BlockedSeconds || a.CashCents != before.CashCents+events[0].CashCents || w.OpenCents != pos.CostCents {
					t.Fatalf("after selling %d: event %+v position %+v window %+v", filled, events[0], pos, w)
				}
			default: // the position rides; the exit stays wanted; a second is counted; a try only if it reached a book
				tries := was.ExitTries
				if c.status == broker.Cancelled {
					tries++
				}
				if events[0].Kind != "exit_unfilled" || pos == nil || pos.Contracts != was.Contracts || pos.CostCents != was.CostCents ||
					pos.PremiumCents != was.PremiumCents || pos.Exiting != in.Why || pos.BlockedSeconds != was.BlockedSeconds+1 || pos.ExitTries != tries ||
					a.CashCents != before.CashCents || !reflect.DeepEqual(a.Windows, before.Windows) || a.Bets != before.Bets {
					t.Fatalf("after an exit with nothing filled: event %+v position %+v (was %+v) cash %d", events[0], pos, was, a.CashCents)
				}
				if events[0].ExitTries != tries || events[0].BlockedSeconds != was.BlockedSeconds+1 || events[0].Left != was.Contracts {
					t.Fatalf("the event must carry the counts: %+v", events[0])
				}
			}
			if a.CashCents < 0 {
				t.Fatalf("cash %d", a.CashCents)
			}
		})
	}

	// Every status, and every reason the Paper has, was driven through Apply at least once.
	for _, want := range []string{
		"filled|", "partial|" + broker.ReasonNothingMore, "partial|" + broker.ReasonCeiling, "partial|" + broker.ReasonDust,
		"cancelled|" + broker.ReasonNoBids, "cancelled|" + broker.ReasonOutsideLimit, "cancelled|" + broker.ReasonAlreadyTaken, "cancelled|" + broker.ReasonCeiling,
		"rejected|" + broker.ReasonNoClientID, "rejected|" + broker.ReasonBadOrder, "rejected|" + broker.ReasonBadQty, "rejected|" + broker.ReasonBadLimit,
		"rejected|" + broker.ReasonBadCeiling, "rejected|" + broker.ReasonNoBook, "rejected|" + broker.ReasonStaleBook, "rejected|" + broker.ReasonClosed,
		"rejected|" + broker.ReasonUnreadable, "rejected|" + broker.ReasonCrossed,
	} {
		if !covered[want] {
			t.Errorf("no case drew the answer %q", want)
		}
	}
}
