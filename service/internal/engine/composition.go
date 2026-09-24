package engine

import (
	"fmt"
	"math"
)

// A composition is one version: a roster of named shapes plus a pick. Results attach to the
// version, not to a member (the brief: one bucket, one strategy version). The pick is two
// stages, both conventions, labelled:
//
//  1. Structural. A member is eligible only if Account.decide with that member's Params would
//     send a buy. Sit out if nobody is eligible. No new gate language.
//  2. Recent results, among the eligible. Each member is shadow-scored as a unit contract on
//     the same tape (a fixed $1 seed of one contract, not the composition's live cash). A
//     shadow settles only after that market's close is known. At time T the score uses only
//     settlements whose close is at or before T. Metric: return per dollar over the last
//     Lookback settled shadows that member entered. Before every eligible member has that
//     many, roster order (the first eligible). Lookback 0, or StructuralOnly, is roster
//     order always: the ablation's structural-only baseline.
//
// v1 members all hold. Mixing hold and ev would let a member sell a position it did not open.

const (
	// DefaultLookback is K: how many settled shadow entries the adaptive pick needs.
	DefaultLookback = 16
	MinMembers      = 2
	MaxMembers      = 8

	PickSitOut   = "sit_out"
	PickWarmup   = "warmup"
	PickAdaptive = "adaptive"
	PickRoster   = "roster"
)

// Composition is the roster and the pick's memory. It is pointed at from Account so the
// shadow ring survives across looks; Decide reads it, ApplySettlement writes settlements.
type Composition struct {
	Name           string
	Members        []Params
	Lookback       int
	StructuralOnly bool

	pending []shadowPending
	settled []shadowSettled
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
	return &Composition{
		Name:           p.Name,
		Members:        members,
		Lookback:       int(p.LookbackWindows),
		StructuralOnly: p.StructuralOnly || p.LookbackWindows <= 0,
	}
}

// HasComposition reports a version that is a roster, not a single shape.
func (p Params) HasComposition() bool { return len(p.Members) >= MinMembers }

// Pick chooses among eligible member indexes (roster order). now is unix seconds: a
// settlement whose close is after now is invisible, so a future result cannot elect a member.
func (c *Composition) Pick(now float64, eligible []int) (idx int, how string) {
	if len(eligible) == 0 {
		return -1, PickSitOut
	}
	if len(eligible) == 1 || c.StructuralOnly || c.Lookback <= 0 {
		return eligible[0], PickRoster
	}
	for _, i := range eligible {
		if _, n := c.score(c.Members[i].Name, now); n < c.Lookback {
			return eligible[0], PickWarmup
		}
	}
	best, bestR := eligible[0], math.Inf(-1)
	for _, i := range eligible {
		r, _ := c.score(c.Members[i].Name, now)
		if r > bestR {
			best, bestR = i, r
		}
	}
	return best, PickAdaptive
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

// score is return per dollar over the last Lookback settled shadows of member whose close is
// at or before now. n is how many of those there are (may be under Lookback).
func (c *Composition) score(member string, now float64) (rpd float64, n int) {
	var cost, pnl int64
	for _, s := range c.settled {
		if s.Member != member || s.Close > now {
			continue
		}
		cost += s.Cost
		pnl += s.PnL
		n++
	}
	if c.Lookback > 0 && n > c.Lookback {
		// Recount the last K only. settled is chronological; walk from the end.
		cost, pnl, n = 0, 0, 0
		for i := len(c.settled) - 1; i >= 0 && n < c.Lookback; i-- {
			s := c.settled[i]
			if s.Member != member || s.Close > now {
				continue
			}
			cost += s.Cost
			pnl += s.PnL
			n++
		}
	}
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
		return nil
	}
	if n := len(p.Members); n > MaxMembers {
		return fmt.Errorf("a roster has at most %d members, not %d", MaxMembers, n)
	}
	if p.LookbackWindows < 0 || p.LookbackWindows > 64 {
		return fmt.Errorf("lookback_windows %d is outside 0..64", p.LookbackWindows)
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
