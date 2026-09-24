package engine

import (
	"fmt"
	"math"
	"sort"
)

// A composition is one version: a roster of named shapes plus an assignment. Results attach to
// the version, not to a member (the brief: one bucket, one strategy version). Assignment is two
// stages, both conventions, labelled:
//
//  1. Window owner. Each 15-minute clock is assigned to one member from prior settled clocks
//     only (shadow unit contracts on a fixed $1 seed, not the composition's live cash). A
//     settlement whose close is after now, or at or after this clock's close, is invisible.
//     Score: return per dollar over the last Lookback clocks in which that member had a shadow.
//     Sit-outs do not dilute a late specialist. Before Lookback clocks have settled, or when
//     StructuralOnly, the owner is roster[0]. AssignReserve has no owner.
//  2. Reservation. The owner gets first refusal on a ticker (they claim if they would buy).
//     Unclaimed seats may go to a later specialist: strictly smaller tau_max than the owner.
//     Among several, the latest (smallest tau_max). A ticker is reserved at first claim and
//     never reassigned. AssignWindow has no leftovers. AssignReserve sits while any member's
//     window has not started (tau > their tau_max), then the latest specialist who would buy.
//
// Eligibility is still "decideOnce would send a buy". No new gate language. v1 members all hold.

const (
	// DefaultLookback is K: prior 15-minute clocks the adaptive window owner needs.
	DefaultLookback = 16
	MinMembers      = 2
	MaxMembers      = 8

	AssignBoth    = "both"
	AssignWindow  = "window"
	AssignReserve = "reserve"

	PickSitOut  = "sit_out"
	PickWarmup  = "warmup"
	PickWindow  = "window"
	PickReserve = "reserve"
)

// Composition is the roster and the assignment's memory. It is pointed at from Account so the
// shadow ring and the reservation map survive across looks; Decide reads it, ApplySettlement
// writes settlements.
type Composition struct {
	Name           string
	Members        []Params
	Lookback       int
	StructuralOnly bool
	Assign         string // both | window | reserve

	pending  []shadowPending
	settled  []shadowSettled
	reserved map[string]reservation
}

type reservation struct {
	idx int
	how string
}

type shadowPending struct {
	Member string
	Ticker string
	Side   string
	Close  float64
	Cost   int64 // cents for one contract
}

type shadowSettled struct {
	Member string
	Close  float64
	Cost   int64
	PnL    int64
}

// newComposition is the live picker for a version that carries members. Nil when it does not.
func newComposition(p Params) *Composition {
	if !p.HasComposition() {
		return nil
	}
	members := make([]Params, 0, len(p.Members))
	for _, m := range p.Members {
		mp, err := FromShape(m.asShape())
		if err != nil {
			return nil
		}
		members = append(members, mp)
	}
	assign := p.Assign
	if assign == "" {
		assign = AssignBoth
	}
	return &Composition{
		Name:           p.Name,
		Members:        members,
		Lookback:       int(p.LookbackWindows),
		StructuralOnly: p.StructuralOnly,
		Assign:         assign,
		reserved:       map[string]reservation{},
	}
}

// HasComposition reports a version that is a roster, not a single shape.
func (p Params) HasComposition() bool { return len(p.Members) >= MinMembers }

// WindowOwner is the member index that owns the clock closing at windowClose, and how that
// owner was chosen (window or warmup). now is unix seconds: a settlement whose close is after
// now, or at or after windowClose, cannot elect this clock. -1 when assign is reserve (no owner).
func (c *Composition) WindowOwner(windowClose, now float64) (idx int, how string) {
	if c == nil || c.Assign == AssignReserve {
		return -1, ""
	}
	if len(c.Members) == 0 {
		return -1, ""
	}
	if c.StructuralOnly || c.Lookback <= 0 {
		return 0, PickWindow
	}
	if c.clocksSettled(windowClose, now) < c.Lookback {
		return 0, PickWarmup
	}
	best, bestR := -1, math.Inf(-1)
	for i, m := range c.Members {
		r, n := c.windowScore(m.Name, windowClose, now)
		if n == 0 {
			continue
		}
		if r > bestR || (r == bestR && (best < 0 || i < best)) {
			best, bestR = i, r
		}
	}
	if best < 0 {
		return 0, PickWarmup
	}
	return best, PickWindow
}

// Pick chooses who may claim this ticker among eligible member indexes (those who would
// send a buy). Locks the ticker on first claim. now is unix seconds; tau is seconds to close.
func (c *Composition) Pick(ticker string, windowClose, now, tau float64, eligible []int) (idx int, how string) {
	if c.reserved == nil {
		c.reserved = map[string]reservation{}
	}
	if r, ok := c.reserved[ticker]; ok {
		if containsIdx(eligible, r.idx) {
			return r.idx, r.how
		}
		return -1, PickSitOut
	}
	owner, ownerHow := c.WindowOwner(windowClose, now)
	if owner >= 0 && containsIdx(eligible, owner) {
		c.reserve(ticker, owner, ownerHow)
		return owner, ownerHow
	}
	if c.Assign == AssignWindow {
		return -1, PickSitOut
	}
	if c.Assign == AssignReserve {
		if c.stillWaiting(tau) {
			return -1, PickSitOut
		}
		later := eligible
		idx = c.latestAmong(later)
		if idx < 0 {
			return -1, PickSitOut
		}
		c.reserve(ticker, idx, PickReserve)
		return idx, PickReserve
	}
	// both: leftovers to a later specialist than the owner.
	var later []int
	for _, i := range eligible {
		if c.laterThan(i, owner) {
			later = append(later, i)
		}
	}
	idx = c.latestAmong(later)
	if idx < 0 {
		return -1, PickSitOut
	}
	c.reserve(ticker, idx, PickReserve)
	return idx, PickReserve
}

func (c *Composition) reserve(ticker string, idx int, how string) {
	if ticker == "" || idx < 0 {
		return
	}
	if _, ok := c.reserved[ticker]; ok {
		return
	}
	c.reserved[ticker] = reservation{idx: idx, how: how}
}

func (c *Composition) laterThan(i, owner int) bool {
	if owner < 0 || owner >= len(c.Members) || i < 0 || i >= len(c.Members) {
		return false
	}
	return c.Members[i].TauMax < c.Members[owner].TauMax
}

func (c *Composition) latestAmong(idxs []int) int {
	best, bestTau := -1, math.Inf(1)
	for _, i := range idxs {
		if i < 0 || i >= len(c.Members) {
			continue
		}
		t := c.Members[i].TauMax
		if t < bestTau || (t == bestTau && (best < 0 || i < best)) {
			best, bestTau = i, t
		}
	}
	return best
}

// stillWaiting is assign=reserve: sit while a later member's window has not started.
func (c *Composition) stillWaiting(tau float64) bool {
	if len(c.Members) == 0 {
		return false
	}
	latest := c.Members[0].TauMax
	for _, m := range c.Members[1:] {
		if m.TauMax < latest {
			latest = m.TauMax
		}
	}
	return tau > latest
}

func containsIdx(idxs []int, want int) bool {
	for _, i := range idxs {
		if i == want {
			return true
		}
	}
	return false
}

// Observe notes a member's would-be unit entry on ticker, once. Pending until Settle.
func (c *Composition) Observe(member, ticker, side string, closeAt float64, costCents int64) {
	if member == "" || ticker == "" || costCents <= 0 {
		return
	}
	for _, p := range c.pending {
		if p.Ticker == ticker && p.Member == member {
			return
		}
	}
	for _, s := range c.settled {
		if s.Member == member && s.Close == closeAt {
			return
		}
	}
	c.pending = append(c.pending, shadowPending{Member: member, Ticker: ticker, Side: side, Close: closeAt, Cost: costCents})
}

// Settle scores every pending shadow on ticker against the market's result. Called from
// ApplySettlement whether or not the composition itself held a position there.
func (c *Composition) Settle(ticker, result string) {
	if c == nil {
		return
	}
	kept := c.pending[:0]
	for _, p := range c.pending {
		if p.Ticker != ticker {
			kept = append(kept, p)
			continue
		}
		payout := int64(0)
		if (result == "yes" && p.Side == "yes") || (result == "no" && p.Side == "no") {
			payout = 100
		}
		c.settled = append(c.settled, shadowSettled{Member: p.Member, Close: p.Close, Cost: p.Cost, PnL: payout - p.Cost})
	}
	c.pending = kept
}

// clocksSettled is how many distinct prior clocks have a shadow whose close is at or before now
// and strictly before windowClose.
func (c *Composition) clocksSettled(windowClose, now float64) int {
	seen := map[float64]bool{}
	n := 0
	for _, s := range c.settled {
		if s.Close > now || s.Close >= windowClose || seen[s.Close] {
			continue
		}
		seen[s.Close] = true
		n++
	}
	return n
}

// windowScore is return per dollar over the last Lookback clocks in which member had a settled
// shadow whose close is at or before now and strictly before windowClose. n is those clocks.
func (c *Composition) windowScore(member string, windowClose, now float64) (rpd float64, n int) {
	type acc struct{ cost, pnl int64 }
	by := map[float64]acc{}
	for _, s := range c.settled {
		if s.Member != member || s.Close > now || s.Close >= windowClose {
			continue
		}
		a := by[s.Close]
		a.cost += s.Cost
		a.pnl += s.PnL
		by[s.Close] = a
	}
	closes := make([]float64, 0, len(by))
	for cl := range by {
		closes = append(closes, cl)
	}
	sort.Float64s(closes)
	if c.Lookback > 0 && len(closes) > c.Lookback {
		closes = closes[len(closes)-c.Lookback:]
	}
	var cost, pnl int64
	for _, cl := range closes {
		a := by[cl]
		cost += a.cost
		pnl += a.pnl
	}
	n = len(closes)
	if cost > 0 {
		rpd = float64(pnl) / float64(cost)
	}
	return rpd, n
}

func validateComposition(p Params) error {
	if !p.HasComposition() {
		if len(p.Members) == 1 {
			return fmt.Errorf("a roster needs at least %d members", MinMembers)
		}
		if p.StructuralOnly {
			return fmt.Errorf("structural_only is for a roster")
		}
		if p.LookbackWindows != 0 {
			return fmt.Errorf("lookback_windows is for a roster")
		}
		if p.Assign != "" {
			return fmt.Errorf("assign is for a roster")
		}
		return nil
	}
	if n := len(p.Members); n > MaxMembers {
		return fmt.Errorf("a roster has at most %d members, not %d", MaxMembers, n)
	}
	switch p.Assign {
	case "", AssignBoth, AssignWindow, AssignReserve:
	default:
		return fmt.Errorf("assign %q is not both, window or reserve", p.Assign)
	}
	if p.LookbackWindows < 0 || p.LookbackWindows > 64 {
		return fmt.Errorf("lookback_windows %d is outside 0..64", p.LookbackWindows)
	}
	if p.Assign == AssignReserve {
		if p.LookbackWindows != 0 {
			return fmt.Errorf("assign=reserve does not use lookback_windows")
		}
		if p.StructuralOnly {
			return fmt.Errorf("assign=reserve has no window owner; structural_only is for window or both")
		}
	}
	if p.StructuralOnly && p.LookbackWindows != 0 {
		return fmt.Errorf("structural_only does not use lookback_windows")
	}
	seen := map[string]bool{}
	var exit, family string
	for i, m := range p.Members {
		mp, err := FromShape(m.asShape())
		if err != nil {
			return fmt.Errorf("member %d: %w", i+1, err)
		}
		if i == 0 {
			exit, family = mp.Exit, mp.FamilyOf()
		}
		if mp.Exit != "hold" {
			return fmt.Errorf("member %q exits %s: a roster's members all hold (v1)", mp.Name, mp.Exit)
		}
		if mp.Exit != exit {
			return fmt.Errorf("members must share an exit; %q is %s, the first is %s", mp.Name, mp.Exit, exit)
		}
		if mp.FamilyOf() != family {
			return fmt.Errorf("members must share a family; %q is %s, the first is %s", mp.Name, mp.FamilyOf(), family)
		}
		if seen[mp.Name] {
			return fmt.Errorf("two members are named %q", mp.Name)
		}
		seen[mp.Name] = true
	}
	if p.Exit != "hold" {
		return fmt.Errorf("a roster holds to settlement; exit is %q", p.Exit)
	}
	return nil
}
