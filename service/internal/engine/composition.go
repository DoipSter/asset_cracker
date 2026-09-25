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

	// The memory: every member's shadows and the tickers handed out. It outlives the engine it
	// was learned in (Memory and Restore): the runner saves it across a restart and hands it to
	// the engine a rebuild makes, so neither sends the roster back to warmup.
	pending  []ShadowPending
	settled  []ShadowSettled
	reserved map[string]reservation
	dirty    bool // the memory changed since the last TakeDirty
}

type reservation struct {
	idx   int
	how   string
	close float64 // the ticker's close: past it nobody claims, and Prune drops the reservation
}

// ShadowPending is a member's would-be unit entry on one market, waiting for its result.
type ShadowPending struct {
	Member   string  `json:"member"`
	Ticker   string  `json:"ticker"`
	MarketID int64   `json:"market_id"` // the sweep reads the result by it
	Side     string  `json:"side"`
	Close    float64 `json:"close"`
	Cost     int64   `json:"cost"` // cents for one contract
}

// ShadowSettled is a scored shadow: what a member's unit entry on one clock cost and made.
type ShadowSettled struct {
	Member string  `json:"member"`
	Close  float64 `json:"close"`
	Cost   int64   `json:"cost"`
	PnL    int64   `json:"pnl"`
}

// RosterClaim is a reservation as it is saved: the member by name.
type RosterClaim struct {
	Member string  `json:"member"`
	How    string  `json:"how"`
	Close  float64 `json:"close"`
}

// RosterMemory is a composition's memory in the form it is saved and handed on. Members are by
// name, so memory restored into a roster keeps only what names one of its members.
type RosterMemory struct {
	Pending  []ShadowPending        `json:"pending,omitempty"`
	Settled  []ShadowSettled        `json:"settled,omitempty"`
	Reserved map[string]RosterClaim `json:"reserved,omitempty"` // by ticker
}

// shadowGiveUp is how long after its close a shadow may wait for its result before Prune drops
// it unscored. [CONVENTION] The rounds' results come within minutes; one the poller never gets
// (it lets an untraded round go an hour after its close) would otherwise wait for ever.
const shadowGiveUp = 72 * 3600.0

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
		c.reserve(ticker, owner, ownerHow, windowClose)
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
		c.reserve(ticker, idx, PickReserve, windowClose)
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
	c.reserve(ticker, idx, PickReserve, windowClose)
	return idx, PickReserve
}

func (c *Composition) reserve(ticker string, idx int, how string, closeAt float64) {
	if ticker == "" || idx < 0 {
		return
	}
	if _, ok := c.reserved[ticker]; ok {
		return
	}
	c.reserved[ticker] = reservation{idx: idx, how: how, close: closeAt}
	c.dirty = true
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

// Observe notes a member's would-be unit entry on market m, once. Pending until Settle.
func (c *Composition) Observe(member string, m Market, side string, costCents int64) {
	if member == "" || m.Ticker == "" || costCents <= 0 {
		return
	}
	for _, p := range c.pending {
		if p.Ticker == m.Ticker && p.Member == member {
			return
		}
	}
	for _, s := range c.settled {
		if s.Member == member && s.Close == m.Close {
			return
		}
	}
	c.pending = append(c.pending, ShadowPending{Member: member, Ticker: m.Ticker, MarketID: m.MarketID, Side: side, Close: m.Close, Cost: costCents})
	c.dirty = true
}

// Settle scores every pending shadow on ticker against the market's result, yes or no; any
// other result scores nothing and the shadows wait on. Called from ApplySettlement whether or
// not the composition itself held a position there, and from SettleShadows when no bucket did.
func (c *Composition) Settle(ticker, result string) {
	if c == nil || (result != "yes" && result != "no") {
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
		c.settled = append(c.settled, ShadowSettled{Member: p.Member, Close: p.Close, Cost: p.Cost, PnL: payout - p.Cost})
		c.dirty = true
	}
	c.pending = kept
}

// Memory is a copy of what the composition remembers.
func (c *Composition) Memory() RosterMemory {
	m := RosterMemory{Pending: append([]ShadowPending(nil), c.pending...), Settled: append([]ShadowSettled(nil), c.settled...)}
	for ticker, r := range c.reserved {
		if r.idx < 0 || r.idx >= len(c.Members) {
			continue
		}
		if m.Reserved == nil {
			m.Reserved = map[string]RosterClaim{}
		}
		m.Reserved[ticker] = RosterClaim{Member: c.Members[r.idx].Name, How: r.how, Close: r.close}
	}
	return m
}

// Restore replaces what the composition remembers with m, keeping only what names one of its
// members. It marks the memory changed, so the runner saves it once more.
func (c *Composition) Restore(m RosterMemory) {
	idx := make(map[string]int, len(c.Members))
	for i, mp := range c.Members {
		idx[mp.Name] = i
	}
	c.pending, c.settled = nil, nil
	for _, p := range m.Pending {
		if _, ok := idx[p.Member]; ok {
			c.pending = append(c.pending, p)
		}
	}
	for _, s := range m.Settled {
		if _, ok := idx[s.Member]; ok {
			c.settled = append(c.settled, s)
		}
	}
	c.reserved = map[string]reservation{}
	for ticker, r := range m.Reserved {
		if i, ok := idx[r.Member]; ok {
			c.reserved[ticker] = reservation{idx: i, how: r.How, close: r.Close}
		}
	}
	c.dirty = true
}

// TakeDirty reports whether the memory changed since the last call.
func (c *Composition) TakeDirty() bool {
	d := c.dirty
	c.dirty = false
	return d
}

// Prune forgets what no later pick can read, and returns how many shadows it dropped unscored.
// A reservation goes once its ticker has closed. A settled shadow goes once it is not among its
// member's Lookback latest clocks closed by now: a member's score reads only those, and the
// warmup count (distinct clocks across members) is still at least Lookback whenever it was.
// None is kept when the owner is not adaptive, since nothing reads them. A pending shadow goes
// shadowGiveUp after its close, unscored.
func (c *Composition) Prune(now float64) (dropped int) {
	for ticker, r := range c.reserved {
		if r.close < now {
			delete(c.reserved, ticker)
			c.dirty = true
		}
	}
	keptPending := c.pending[:0]
	for _, p := range c.pending {
		if p.Close+shadowGiveUp < now {
			dropped++
			continue
		}
		keptPending = append(keptPending, p)
	}
	c.pending = keptPending
	keep := 0
	if c.Assign != AssignReserve && !c.StructuralOnly && c.Lookback > 0 {
		keep = c.Lookback
	}
	latest := map[string][]float64{} // per member, its distinct closes by now
	for _, s := range c.settled {
		if s.Close <= now && !containsClose(latest[s.Member], s.Close) {
			latest[s.Member] = append(latest[s.Member], s.Close)
		}
	}
	oldest := map[string]float64{} // per member, the oldest close it keeps
	for member, closes := range latest {
		if len(closes) > keep {
			sort.Sort(sort.Reverse(sort.Float64Slice(closes)))
			if keep > 0 {
				oldest[member] = closes[keep-1]
			} else {
				oldest[member] = math.Inf(1)
			}
		}
	}
	keptSettled := c.settled[:0]
	for _, s := range c.settled {
		if cut, ok := oldest[s.Member]; ok && s.Close <= now && s.Close < cut {
			continue
		}
		keptSettled = append(keptSettled, s)
	}
	if dropped > 0 || len(keptSettled) != len(c.settled) {
		c.dirty = true
	}
	c.settled = keptSettled
	return dropped
}

func containsClose(closes []float64, want float64) bool {
	for _, c := range closes {
		if c == want {
			return true
		}
	}
	return false
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
