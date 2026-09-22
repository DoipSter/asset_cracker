package engine

import (
	"math"

	"github.com/doipster/asset_cracker/service/internal/broker"
)

// Reasons a wanted buy is not sent, as stored in decision.blocked_by.
const (
	BlockedNoEdge               = "no edge after fee and staleness"
	BlockedNoEdgeRounded        = "no edge once the order's fee is rounded up"
	BlockedHeldKelly            = "already holds quarter-Kelly of this market at this price"
	BlockedNoSize               = "less than one whole contract displayed"
	BlockedBudgetSpent          = "window budget spent"
	BlockedCapReached           = "window cap reached"
	BlockedNoCash               = "no cash"
	BlockedUnderOne             = "the stake buys less than one contract"
	BlockedDrift                = "model drift"
	BlockedNoBook               = "no two-sided book"
	BlockedNoModel              = "no model view"
	BlockedTooLate              = "too late this round"
	BlockedTooEarly             = "waits for its time window"
	BlockedBand                 = "ask outside its price band"
	BlockedMaxBets              = "max bets this round"
	BlockedCoolingDown          = "cooling down"
	BlockedExitNoBook           = "an exit is wanted but no book can price it"
	BlockedExitTooLate          = "an exit is wanted but it is too late: held to settlement"
	BlockedExitNoProceeds       = "an exit is wanted but the sale could book nothing after the rounded fee: held to settlement"
	bindingKelly                = "kelly"
	bindingWindow               = "window"
	bindingCap                  = "cap"
	bindingCash                 = "cash"
	maxOrderQty           int64 = broker.MaxQty
)

// SizeInput is everything the window rule needs. All money is whole cents.
type SizeInput struct {
	PSide float64 // blended probability that the side being bought pays
	// Asks is the taker prices the order may walk, best (lowest) first. The caller hands in only
	// prices the broker can really give and the version may really pay: recorded levels showing at
	// least ONE whole contract, and none above the version's band_max. So Asks[0], where the Kelly
	// fraction is taken, is a price a contract can be had at, and the limit never leaves the band.
	Asks       []broker.Price
	StaleUnits int64   // the staleness cost of buying, in price units
	Kappa      float64 // the Kelly fraction staked
	CapBps     int64   // window_cap_bps
	SeedCents  int64

	EquityCents int64   // E_w: frozen for the window, or the bucket's cash if this would be its first buy
	KMax        float64 // the window's best Kelly fraction so far
	UsedCents   int64   // the window's OpenCents + LostCents
	CashCents   int64   // the bucket's cash now
	// HeldCents is the cost basis the account already holds in THIS market and side. Every ceiling
	// below counts it: a re-entry starts where the position stands, not from nothing, or each new
	// order would begin the per-price taper again and spend a best-ask stake at the worst prices.
	HeldCents int64
}

// Sizing is the answer: the order's size and ceilings, and the working, which is stored with the
// order so that a size can be checked afterwards.
type Sizing struct {
	Qty                              int
	Limit                            broker.Price      // the worst displayed taker price that still has an edge
	StakeCents                       int64             // Order.MaxCostCents
	Steps                            []broker.CostStep // one ceiling per displayed price up to the limit
	Kelly                            float64           // k at the best ask (Asks[0])
	KWindow                          float64           // max(the window's best so far, k)
	BudgetCents, CapCents, RoomCents int64
	Binding                          string // what set the size: kelly | window | cap | cash
	BlockedBy                        string // why nothing is bought; empty when Qty >= 1
}

// floorCents is floor(kappa * k * E) in whole cents. The Kelly fraction is a belief and so a
// float; the moment it becomes money it is floored, which can only make the stake smaller.
func floorCents(kappa, k float64, equityCents int64) int64 {
	x := math.Floor(kappa * k * float64(equityCents))
	if !(x > 0) { // NaN too
		return 0
	}
	return int64(x)
}

// Size is the window rule of plan 4.5. One 15-minute window across every coin is ONE bet, and
// the five coins are treated as perfectly correlated whatever the direction [CONVENTION: the
// conservative bound, not the measured figure]:
//
//	budget = floor(kappa * max(KMax, k) * E_w)      quarter-Kelly of the window's single best bet
//	cap    = window_cap_bps * min(E_w, seed) / 10000
//	room   = min(budget - used, cap - used, cash)
//	stake  = min(floor(kappa * k * E_w) - held, room)
//	qty    = the most whole contracts whose BOOKED cost at the best ask (premium rounded up, plus
//	         the order's fee rounded up) stays within the stake; staleness is not a ledger cost
//
// and, because the order may walk to worse prices where the edge is smaller, one ceiling per
// price up to the limit: once a fill at t is included, what the POSITION has cost may not pass
// quarter-Kelly computed AT t. k(t) falls to nothing at the limit, so the order tapers instead of
// spending a best-ask stake at the worst acceptable price. No parameter is added.
//
// Two deliberate departures from the plan's formulas, both on the side that spends less:
//
//   - The plan's ceilings are per ORDER. The broker's ceiling is "this order's cost so far", so
//     with them every re-entry after min_gap would start the taper again from nothing and the
//     position could end far past quarter-Kelly at the prices it was filled at (measured: 737c
//     spent with fills at 0.14, where the rule allows 135c). Here they are per POSITION: held,
//     what the account already holds in this market and side, comes off the stake and off every
//     step. A step of 0 is a real ceiling of nothing in the broker.
//   - The plan charges "fee(c) exactly", per contract. The ledger books the order's fee rounded
//     UP to a cent (and a sub-cent premium rounded up), which on an order of a few contracts is
//     more than the edge that let it through. So after sizing, the ORDER is tested with the
//     figures the broker will book for it at the best ask: what it is believed to pay out, less
//     staleness, must exceed premium plus fee in whole cents, or nothing is sent. (A fill cut
//     short by the book is rounded on fewer contracts and can still fall a fraction of a cent
//     short; an immediate-or-cancel order cannot name a least quantity.)
func Size(in SizeInput) Sizing {
	var s Sizing
	if len(in.Asks) == 0 {
		s.BlockedBy = BlockedNoBook
		return s
	}
	best := in.Asks[0]
	s.Kelly = kelly(in.PSide, best, in.StaleUnits)
	if !(s.Kelly > 0) {
		s.BlockedBy = BlockedNoEdge
		return s
	}
	for _, t := range in.Asks { // asks rise, u rises with them: the first failure ends it
		if !(kelly(in.PSide, t, in.StaleUnits) > 0) {
			break
		}
		s.Limit = t
	}

	s.KWindow = math.Max(in.KMax, s.Kelly)
	s.BudgetCents = floorCents(in.Kappa, s.KWindow, in.EquityCents)
	s.CapCents = in.CapBps * min(in.EquityCents, in.SeedCents) / 10000
	s.RoomCents, s.Binding = s.BudgetCents-in.UsedCents, bindingWindow
	if r := s.CapCents - in.UsedCents; r < s.RoomCents {
		s.RoomCents, s.Binding = r, bindingCap
	}
	if in.CashCents < s.RoomCents {
		s.RoomCents, s.Binding = in.CashCents, bindingCash
	}
	if s.RoomCents <= 0 {
		s.RoomCents = 0
		s.BlockedBy = map[string]string{bindingWindow: BlockedBudgetSpent, bindingCap: BlockedCapReached, bindingCash: BlockedNoCash}[s.Binding]
		return s
	}
	held := max(0, in.HeldCents)
	s.StakeCents = max(0, floorCents(in.Kappa, s.Kelly, in.EquityCents)-held)
	if s.StakeCents == 0 && held > 0 {
		s.BlockedBy = BlockedHeldKelly
		return s
	}
	if s.StakeCents <= s.RoomCents {
		s.Binding = bindingKelly
	} else {
		s.StakeCents = s.RoomCents
	}

	qty := s.StakeCents * e10PerCent / paidE10(best) // stake is at most ~1e9 cents: no overflow
	if qty > maxOrderQty {
		qty = maxOrderQty
	}
	// The division is by the unrounded cost; what is booked is rounded up, by under two cents in
	// all. So at most a contract or two come off, and the order the broker sees fits its ceiling.
	for qty >= 1 && bookedBuyCents(int(qty), best) > s.StakeCents {
		qty--
	}
	if qty < 1 {
		s.BlockedBy = BlockedUnderOne
		return s
	}
	// The order, not the contract: see the second departure above. The belief is a float and the
	// booked cost whole cents; they meet only in this comparison.
	worth := in.PSide*100*float64(qty) - float64(in.StaleUnits*qty)/100 // cents
	if !(worth > float64(bookedBuyCents(int(qty), best))) {
		s.BlockedBy = BlockedNoEdgeRounded
		return s
	}
	s.Qty = int(qty)
	for _, t := range in.Asks {
		if t > s.Limit {
			break
		}
		at := max(0, floorCents(in.Kappa, kelly(in.PSide, t, in.StaleUnits), in.EquityCents)-held)
		s.Steps = append(s.Steps, broker.CostStep{UpTo: t, MaxCostCents: min(at, s.RoomCents)})
	}
	return s
}
