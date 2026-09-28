package analysis

import (
	"math"
	"sort"
)

// The dancer is a roster version's owner election, measured on its members' shadows
// (roster_shadow, migration 0023). Each member's shadow is the one contract it would have bought
// on a market, scored against the result, whether or not the roster let it bet. The roster
// elects each clock's owner from its members' shadows over the prior clocks, on the premise that
// a member that did well lately will do well next. On a clock where the owner and at least one
// other member had a shadow the roster made a real choice, and the owner's return that clock can
// be set against the others'. Over many clocks the mean of that difference is the election's
// worth: above zero, following the recent winner beats the field; at zero, the dance adds
// switching and nothing else. Shadows are unit contracts, so this measures the picking, not the
// sizing.

// Shadow is one scored member shadow (store.ShadowRow).
type Shadow struct {
	Member   string
	Close    int64   // unix seconds: the clock
	Seen     float64 // unix seconds it was first seen; 0 when not kept
	Owner    string  // the clock's owner when it was seen
	OwnerHow string  // window (elected) or warmup; "" when not kept
	Cost     int64   // cents, one contract
	PnL      int64   // cents: 100 or 0, less Cost
}

// DanceStat is a mean over clocks with its standard error and t, and the page's verdict on it.
type DanceStat struct {
	N       int     `json:"n"`
	Mean    float64 `json:"mean"` // per dollar
	SE      float64 `json:"se"`
	T       float64 `json:"t"`
	Verdict string  `json:"verdict"`
}

// DanceMember is one member's shadows: every clock it would have bought in, and the clocks the
// roster gave it.
type DanceMember struct {
	Member         string  `json:"member"`
	Clocks         int     `json:"clocks"`  // clocks it had a shadow in
	Shadows        int     `json:"shadows"` // markets: a clock has one per coin it would have bought
	WinRate        float64 `json:"win_rate"`
	PerDollar      float64 `json:"per_dollar"`    // its shadows' P&L over their cost
	PerDollarSE    float64 `json:"per_dollar_se"` // clocks as the draws, the ratio estimator's
	Owned          int     `json:"owned"`         // elected clocks it owned
	OwnedPerDollar float64 `json:"owned_per_dollar"`
}

// Dancer is the whole measurement.
type Dancer struct {
	Since        int64                   `json:"since"`          // the first clock on record
	Clocks       int                     `json:"clocks"`         // clocks with a scored shadow
	Elected      int                     `json:"elected_clocks"` // of those, with an owner elected from prior clocks
	Choices      int                     `json:"choice_clocks"`  // of those, the owner and another member had a shadow
	OwnerVsField DanceStat               `json:"owner_vs_field"` // per choice clock: the owner's return less the others' mean
	OwnerVsSplit DanceStat               `json:"owner_vs_split"` // per choice clock: the owner's return less an even split of every member with a shadow
	BestShare    float64                 `json:"owner_best_share"`
	ChanceShare  float64                 `json:"chance_best_share"` // what picking a member at random would have scored
	Members      []DanceMember           `json:"members"`
	Series       [][3]float64            `json:"series"`        // choice clocks: [close, following the owner, splitting evenly], cumulative, per $1 a clock
	MemberSeries map[string][][2]float64 `json:"member_series"` // each member's shadows alone: [close, cumulative per $1 a clock]
	What         string                  `json:"what"`
}

// DancerWhat is the method, for the page's fold and an agent reading the document.
const DancerWhat = "Each member's shadow is the one contract it would have bought on a market, scored on the result, whether or not the roster let it bet. " +
	"A clock's owner is the member the roster elected from the prior clocks; clocks in warmup, and shadows recorded before the owner was kept, are left out of the election's figures. " +
	"On a choice clock the owner and at least one other member had a shadow: the owner's return per dollar that clock, less the others' mean (or less an even split of every member), is one draw, " +
	"and the clocks are the independent sample. A verdict needs 30 choice clocks and |t| of at least 2, one question stated in advance per roster. " +
	"Shadows are unit contracts, so this judges the picking and not the sizing, and a member that would not have bought counts as not chosen, not as a zero."

// Dance measures a roster's election from its scored shadows. Nil when there are none.
func Dance(shadows []Shadow) *Dancer {
	if len(shadows) == 0 {
		return nil
	}
	type acc struct{ cost, pnl int64 }
	type clock struct {
		by        map[string]*acc // member -> its shadows this clock
		owner     string
		ownerSeen float64
		elected   bool
	}
	clocks := map[int64]*clock{}
	for _, s := range shadows {
		c := clocks[s.Close]
		if c == nil {
			c = &clock{by: map[string]*acc{}}
			clocks[s.Close] = c
		}
		a := c.by[s.Member]
		if a == nil {
			a = &acc{}
			c.by[s.Member] = a
		}
		a.cost, a.pnl = a.cost+s.Cost, a.pnl+s.PnL
		// The owner the clock was first seen under: an election can move within a clock when a
		// settlement lands in it, and the first is the one the roster offered the clock to.
		if s.OwnerHow == PickWindowName && s.Owner != "" && (!c.elected || (s.Seen > 0 && s.Seen < c.ownerSeen)) {
			c.owner, c.ownerSeen, c.elected = s.Owner, s.Seen, true
			if s.Seen <= 0 {
				c.ownerSeen = math.Inf(1)
			}
		}
	}
	closes := make([]int64, 0, len(clocks))
	for cl := range clocks {
		closes = append(closes, cl)
	}
	sort.Slice(closes, func(i, j int) bool { return closes[i] < closes[j] })
	ret := func(a *acc) float64 { return float64(a.pnl) / float64(a.cost) }

	d := &Dancer{Since: closes[0], Clocks: len(closes), Members: []DanceMember{}, Series: [][3]float64{}, MemberSeries: map[string][][2]float64{}, What: DancerWhat}
	var vsField, vsSplit []float64
	var best, chance float64
	var cumOwner, cumSplit float64
	type mem struct {
		clocks, shadows, won int
		cost, pnl            int64
		perClock             [][2]float64 // [cost, pnl] per clock, for the ratio SE
		owned                int
		ownedCost, ownedPnL  int64
		cum                  float64
	}
	members := map[string]*mem{}
	for _, s := range shadows {
		m := members[s.Member]
		if m == nil {
			m = &mem{}
			members[s.Member] = m
		}
		m.shadows++
		if s.PnL > 0 {
			m.won++
		}
	}
	for _, cl := range closes {
		c := clocks[cl]
		for name, a := range c.by {
			m := members[name]
			m.clocks++
			m.cost, m.pnl = m.cost+a.cost, m.pnl+a.pnl
			m.perClock = append(m.perClock, [2]float64{float64(a.cost), float64(a.pnl)})
			m.cum += ret(a)
			d.MemberSeries[name] = append(d.MemberSeries[name], [2]float64{float64(cl), round(m.cum, 4)})
		}
		if !c.elected {
			continue
		}
		d.Elected++
		if m := members[c.owner]; m != nil {
			m.owned++
			if a := c.by[c.owner]; a != nil {
				m.ownedCost, m.ownedPnL = m.ownedCost+a.cost, m.ownedPnL+a.pnl
			}
		}
		own := c.by[c.owner]
		if own == nil || len(c.by) < 2 {
			continue // the owner would not have bought, or nobody else would: no choice was made
		}
		d.Choices++
		ro := ret(own)
		var others, all float64
		top, atTop := math.Inf(-1), 0
		for name, a := range c.by {
			r := ret(a)
			all += r
			if name != c.owner {
				others += r
			}
			switch {
			case r > top+1e-12:
				top, atTop = r, 1
			case math.Abs(r-top) <= 1e-12:
				atTop++
			}
		}
		k := float64(len(c.by))
		split := all / k
		vsField = append(vsField, ro-others/(k-1))
		vsSplit = append(vsSplit, ro-split)
		if math.Abs(ro-top) <= 1e-12 {
			best++
		}
		chance += float64(atTop) / k
		cumOwner, cumSplit = cumOwner+ro, cumSplit+split
		d.Series = append(d.Series, [3]float64{float64(cl), round(cumOwner, 4), round(cumSplit, 4)})
	}
	d.OwnerVsField, d.OwnerVsSplit = danceStat(vsField), danceStat(vsSplit)
	if d.Choices > 0 {
		d.BestShare, d.ChanceShare = round(best/float64(d.Choices), 4), round(chance/float64(d.Choices), 4)
	}
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := members[name]
		row := DanceMember{Member: name, Clocks: m.clocks, Shadows: m.shadows, Owned: m.owned}
		if m.shadows > 0 {
			row.WinRate = round(float64(m.won)/float64(m.shadows), 4)
		}
		if m.cost > 0 {
			r := float64(m.pnl) / float64(m.cost)
			row.PerDollar = round(r, 4)
			if n := len(m.perClock); n >= 2 {
				var ss float64
				for _, pc := range m.perClock {
					e := pc[1] - r*pc[0]
					ss += e * e
				}
				row.PerDollarSE = round(finite(math.Sqrt(float64(n)/float64(n-1)*ss)/float64(m.cost)), 4)
			}
		}
		if m.ownedCost > 0 {
			row.OwnedPerDollar = round(float64(m.ownedPnL)/float64(m.ownedCost), 4)
		}
		d.Members = append(d.Members, row)
		d.MemberSeries[name] = thin2(d.MemberSeries[name], SeriesPoints)
	}
	if len(d.Series) > SeriesPoints {
		step := int(math.Ceil(float64(len(d.Series)) / float64(SeriesPoints)))
		thinned := [][3]float64{}
		for i := step - 1; i < len(d.Series); i += step {
			thinned = append(thinned, d.Series[i])
		}
		if last := d.Series[len(d.Series)-1]; thinned[len(thinned)-1] != last {
			thinned = append(thinned, last)
		}
		d.Series = thinned
	}
	return d
}

// PickWindowName is the engine's name for an elected owner (engine.PickWindow), kept here so this
// package need not import the engine.
const PickWindowName = "window"

func danceStat(xs []float64) DanceStat {
	s := WindowStat(xs)
	out := DanceStat{N: s.N, Mean: round(s.Mean, 4), SE: round(s.SE, 4), T: round(s.T, 2)}
	out.Verdict = Verdict(Stat{N: s.N, Mean: s.Mean, SE: s.SE, T: out.T}, MinAbsT, "owner better", "owner worse")
	return out
}

// thin2 keeps at most max points, each bin's last, and always the last point.
func thin2(pts [][2]float64, max int) [][2]float64 {
	if len(pts) <= max {
		return pts
	}
	step := int(math.Ceil(float64(len(pts)) / float64(max)))
	out := [][2]float64{}
	for i := step - 1; i < len(pts); i += step {
		out = append(out, pts[i])
	}
	if last := pts[len(pts)-1]; out[len(out)-1] != last {
		out = append(out, last)
	}
	return out
}
