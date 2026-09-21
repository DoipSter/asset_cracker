// Package analysis is the evidence behind GET /api/analysis: whether the model beats the market's
// own price, what the early sales would have made if only the displayed bid had filled, and
// whether any strategy's result can be told from zero.
//
// Everything here is a pure function of rows already read. Nothing touches the database, and
// nothing here can return NaN or Inf: an undefined figure is reported as 0 with the verdict
// "unresolved", because the page must never be handed a number that is not one.
//
// The unit of inference throughout is the 15-minute WINDOW (every market sharing one closes_at,
// all coins together). Bets inside a window, and the five coins in the same minutes, are not
// independent, so the sample size is windows, never bets or rows.
package analysis

import (
	"math"
	"sort"
)

// The verdict rule. These are CONVENTIONS, chosen in advance and stated in the JSON as such. They
// are not measurements of anything.
const (
	MinWindows     = 30   // fewer independent windows than this is always "unresolved"
	MinAbsT        = 2.0  // and so is a mean within two standard errors of zero, for ONE hypothesis stated in advance
	FamilyAlpha    = 0.05 // rows compared side by side: the chance that ANY of them gets a false verdict is held to this
	DominantWindow = 0.5  // one window carrying at least this share of a result is flagged
)

// CorrectedT is the |t| a row must reach when `trials` rows are compared side by side: the
// two-sided Bonferroni cut at FamilyAlpha, sqrt(2) x erfinv(1 - FamilyAlpha/trials), and never
// below MinAbsT. 18 trials give 2.99; 10 give 2.81; one gives 1.96, so MinAbsT stands.
//
// Why: with 18 strategy versions and no edge anywhere, some row passes |t| >= 2 by chance far
// more often than one time in twenty, and the label then reads as a finding. Bonferroni is itself
// a convention. It assumes nothing about how the rows depend on each other, and it is
// conservative when they move together, as a twin and its original do.
//
// trials is a MEASURED count (the rows of strategy_version for the leaderboard). 0 or less means
// it could not be read, and the answer is 0: no threshold, which Verdict reports as "unresolved".
func CorrectedT(trials int) float64 {
	if trials <= 0 {
		return 0
	}
	x := 1 - FamilyAlpha/float64(trials)
	if x >= 1 { // more trials than a float can tell from certainty: the largest cut there is, not +Inf
		x = math.Nextafter(1, 0)
	}
	return math.Max(MinAbsT, finite(math.Sqrt2*math.Erfinv(x)))
}

// Band is a slice of a round by the seconds left to its close.
type Band struct {
	Label  string
	MinTau float64 // a row belongs to the first band whose MinTau its seconds-to-close reaches
}

// Bands, in the order the contract lists them. Each runs from its MinTau up to the band before
// it, lower end included: exactly 600 s left is "over 10 min", exactly 60 s is "1 to 2 min".
var Bands = []Band{{"over 10 min", 600}, {"5 to 10 min", 300}, {"2 to 5 min", 120}, {"1 to 2 min", 60}, {"under 60 s", math.Inf(-1)}}

// BandOf is the index into Bands for a number of seconds to the close.
func BandOf(tau float64) int {
	for i, b := range Bands {
		if tau >= b.MinTau {
			return i
		}
	}
	return len(Bands) - 1
}

// Market is one settled round.
type Market struct {
	ID     int64
	Coin   string
	Closes int64  // unix seconds: the window it belongs to
	Result string // "yes" or "no"
}

// BandSum is one market's scored rows in one band: how many, and the summed squared errors of
// the model's probability and of the market's mid against the result.
type BandSum struct {
	N             int64
	Model, Market float64
}

// Trade is one simulated fill as the ledger recorded it. Cost and payout are the lot's own
// figures (trade_order.detail.cost and .payout), both with their fee already inside: cost is
// contracts x entry price PLUS the buying fee, payout is contracts x sale price LESS the selling
// fee. A sale's row carries the cost of the lot it closed.
type Trade struct {
	OrderID, BucketID int64
	Sell              bool
	Side              string // "yes" or "no": the contract held
	Qty               int
	Second            int64 // unix second it was placed
	CostCents         int64
	PayoutCents       int64   // sales only
	MoneyMissing      bool    // the order's detail has no cost (or, for a sale, no payout): it cannot be booked
	HasDepth          bool    // sales only: the market snapshot at that second recorded the bid ladder
	Displayed         float64 // sales only: size shown at the best bid on that side; 0 if the ladder was empty
}

// BucketRound is what one bucket made on one market, and how many bets it placed there.
type BucketRound struct {
	PnLCents int64
	Bets     int
}

// PricedSale is an early sale re-priced as if only the displayed size had filled.
type PricedSale struct {
	BucketID     int64
	Qty, Beyond  int   // contracts sold, and how many of them were more than the bid displayed
	PnLCents     int64 // as the simulator booked it: payout less cost
	CappedCents  int64 // the displayed size at the sale price, the rest held to settlement
	WithoutDepth bool  // sold before depth was recorded: counted, never priced
}

// MarketFacts is everything the analysis keeps about one settled market. A settled round never
// changes ONCE ITS SETTLEMENT ROWS ARE IN (see Reconcile), so this is computed once and cached by
// market id.
type MarketFacts struct {
	Market
	Score  [5]BandSum
	Rounds map[int64]BucketRound // by bucket id
	Sales  []PricedSale
}

// Holding is one bucket's contracts on one side of one market.
type Holding struct {
	BucketID int64
	Side     string // "yes" or "no"
}

// Paid is one bucket's settlement on one side: how many contracts were settled, and what they
// paid. A losing hold has a row too, with 0 cents (settlement 174 in the samples: qty 60, paid 0).
type Paid struct {
	Qty         int
	PayoutCents int64
}

// Mismatch is a holding whose settlement rows do not cover what was held at the close.
type Mismatch struct {
	Holding
	Held, Settled int
}

// HeldAtClose is what each bucket still held on each side when the round closed: contracts bought
// less contracts sold. A sale always closes a whole lot, and both sides of that are lot.Contracts
// in trade_order.qty, so the subtraction is exact. Holdings of zero are left out.
func HeldAtClose(trades []Trade) map[Holding]int {
	held := map[Holding]int{}
	for _, t := range trades {
		k := Holding{t.BucketID, t.Side}
		if t.Sell {
			held[k] -= t.Qty
		} else {
			held[k] += t.Qty
		}
	}
	for k, n := range held {
		if n == 0 {
			delete(held, k)
		}
	}
	return held
}

// Reconcile checks a settled market's fills against its settlement rows, per bucket AND side, and
// lists what does not add up. An empty answer means the market's money is all there.
//
// Why: the round's result and its settlement rows are written in separate transactions (the
// result first; then the first engine's rows; then the second engine's, after it has waited for
// its mutex), and a runner that is halted, or a process that dies in between, never writes them
// at all. A market read in that gap has held bets and no payout, which reads as a total loss. The
// engine writes a settlement row for EVERY lot held to the close, lost ones included, so "held
// and no row" always means "not written", never "lost": the test is for the quantity, not for a
// non-zero payout. It is per bucket because the two engines commit their rows separately for the
// same market, so "some settlement row exists" proves nothing about the other engine's buckets.
//
// The match is exact both ways: a settlement for contracts no fill accounts for (a bet placed
// outside the minutes that were read) is as unbookable as a holding with no settlement.
func Reconcile(trades []Trade, settled map[Holding]Paid) []Mismatch {
	held := HeldAtClose(trades)
	var out []Mismatch
	for k, n := range held {
		if settled[k].Qty != n {
			out = append(out, Mismatch{k, n, settled[k].Qty})
		}
	}
	for k, p := range settled {
		if _, ok := held[k]; !ok && p.Qty != 0 {
			out = append(out, Mismatch{k, 0, p.Qty})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BucketID != out[j].BucketID {
			return out[i].BucketID < out[j].BucketID
		}
		return out[i].Side < out[j].Side
	})
	return out
}

// MoneyMissing lists the orders whose recorded detail lacks the money figure they are booked by.
// Such a fill would otherwise be booked as a free bet or a worthless sale, so its market is held
// back and reported exactly as an unreconciled one is.
func MoneyMissing(trades []Trade) []int64 {
	var out []int64
	for _, t := range trades {
		if t.MoneyMissing {
			out = append(out, t.OrderID)
		}
	}
	return out
}

// Settle turns a settled market's fills and payouts into each bucket's result on it and its
// re-priced early sales. It books what it is given: the caller must have checked Reconcile first.
func Settle(m Market, trades []Trade, settled map[Holding]Paid) MarketFacts {
	f := MarketFacts{Market: m, Rounds: map[int64]BucketRound{}}
	for _, t := range trades {
		r := f.Rounds[t.BucketID]
		if t.Sell {
			r.PnLCents += t.PayoutCents
		} else {
			r.PnLCents -= t.CostCents
			r.Bets++
		}
		f.Rounds[t.BucketID] = r
	}
	for k, p := range settled {
		r := f.Rounds[k.BucketID]
		r.PnLCents += p.PayoutCents
		f.Rounds[k.BucketID] = r
	}
	f.Sales = PriceSales(trades, m.Result)
	return f
}

// PriceSales re-prices every early sale in one market. The simulator sells any size at the bid;
// here only the size displayed at the best bid fills, at the price the sale was booked at, and
// the remainder rides to settlement: 1.00 a contract if its side won, else nothing, and no fee,
// which is how the engine settles a held lot.
//
// Sales by one bucket on one side in the same second saw the same book, so they share one
// displayed size, first come (lowest order id) first served. Different buckets do not share:
// each is its own paper world, as in the simulator.
//
// The filled part's proceeds and the cost basis are the sale's recorded payout and cost pro
// rata, so both fees are inside exactly as booked. The selling fee's round-up to a whole cent is
// NOT recomputed for the smaller fill, so one sale can be off by under a cent. Displayed sizes
// can be fractional; only whole contracts fill.
func PriceSales(trades []Trade, result string) []PricedSale {
	var sells []Trade
	for _, t := range trades {
		if t.Sell && t.Qty > 0 {
			sells = append(sells, t)
		}
	}
	sort.SliceStable(sells, func(i, j int) bool { return sells[i].OrderID < sells[j].OrderID })
	type book struct {
		bucket, second int64
		side           string
	}
	left := map[book]int{}
	out := make([]PricedSale, 0, len(sells))
	for _, t := range sells {
		p := PricedSale{BucketID: t.BucketID, Qty: t.Qty, PnLCents: t.PayoutCents - t.CostCents}
		if !t.HasDepth {
			p.WithoutDepth = true
			out = append(out, p)
			continue
		}
		k := book{t.BucketID, t.Second, t.Side}
		if _, seen := left[k]; !seen {
			left[k] = int(math.Floor(math.Max(finite(t.Displayed), 0)))
		}
		filled := min(t.Qty, left[k])
		left[k] -= filled
		p.Beyond = t.Qty - filled
		capped := float64(t.PayoutCents)*float64(filled)/float64(t.Qty) - float64(t.CostCents)
		if t.Side == result {
			capped += 100 * float64(p.Beyond)
		}
		p.CappedCents = int64(math.Round(capped))
		out = append(out, p)
	}
	return out
}

// Stat is a mean over independent windows with its standard error.
type Stat struct {
	N           int
	Mean, SE, T float64
}

// WindowStat is the mean of one value per window, SE = sd / sqrt(n) with the n-1 sample sd, and
// t = mean / SE. With no windows everything is 0; with one, or when every window is identical,
// there is no spread to measure and SE and t are 0: undefined is reported as 0, never as NaN.
func WindowStat(perWindow []float64) Stat {
	n := len(perWindow)
	if n == 0 {
		return Stat{}
	}
	var sum float64
	for _, v := range perWindow {
		sum += finite(v)
	}
	s := Stat{N: n, Mean: sum / float64(n)}
	if n < 2 {
		return s
	}
	var ss float64
	for _, v := range perWindow {
		d := finite(v) - s.Mean
		ss += d * d
	}
	s.SE = finite(math.Sqrt(ss/float64(n-1)) / math.Sqrt(float64(n)))
	if s.SE <= 1e-12*math.Max(1, math.Abs(s.Mean)) { // identical windows: rounding dust is not a spread
		s.SE = 0
	}
	if s.SE > 0 {
		s.T = finite(s.Mean / s.SE)
	}
	return s
}

// Verdict applies the rule: "unresolved" unless there are at least MinWindows windows AND
// |t| >= minAbsT, then whenPositive or whenNegative by the sign of t. minAbsT is MinAbsT for a
// single hypothesis stated in advance and CorrectedT(n) for n rows compared side by side. A
// minAbsT of 0 or less means no verdict can be given at all (the figures are partial, or the
// number of trials is unknown), and the answer is "unresolved" whatever t is.
//
// The caller passes t AS PUBLISHED (rounded), so the number on the page and the label beside it
// can never disagree: a row showing t = 2.00 against a threshold of 2 is never "unresolved".
func Verdict(s Stat, minAbsT float64, whenPositive, whenNegative string) string {
	if minAbsT <= 0 || s.N < MinWindows || math.Abs(s.T) < minAbsT {
		return "unresolved"
	}
	if s.T > 0 {
		return whenPositive
	}
	return whenNegative
}

// TopWindowShare is how much of a total one window accounts for: the largest window P&L in the
// direction of the total, divided by the total. 0.97 means one window is 97% of the result.
// Above 1 means that window is MORE than the whole result: every other window together went
// the other way. 0 when there are no windows or the total is exactly zero.
func TopWindowShare(perWindow []float64) float64 {
	var total, top float64
	for _, v := range perWindow {
		total += finite(v)
	}
	if len(perWindow) == 0 || total == 0 {
		return 0
	}
	top = finite(perWindow[0])
	for _, v := range perWindow {
		if (total > 0 && v > top) || (total < 0 && v < top) {
			top = v
		}
	}
	return finite(top / total)
}

func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return finite(math.Round(finite(v)*p) / p)
}
