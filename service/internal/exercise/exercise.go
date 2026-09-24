// Package exercise runs a strategy shape through the third engine on the recorded tape: what a
// version would have done, had it been seeded over a window that has already settled. It is the
// MCP tool strategy_exercise (docs/mcp-read-surface.md, "The proposal door"), the step between
// strategy_build (a shape labelled) and strategy_register (a shape in the registry).
//
// Simulated money only, and none of it real even in simulation: nothing here touches a bucket,
// the ledger, the journal or the registry. The replay is the live path with the tape for a
// venue: engine.Decide on each recorded second's book and model view, broker.Paper filling
// against the recorded depth, engine.Apply folding the fills, engine.ApplySettlement at each
// market's recorded result. The engine and the broker are the same code the runner drives; this
// package only feeds them and adds up what they did.
//
// What the answer is and is not. It is a leaderboard-style row for one shape over one window,
// cut by entry time and entry price and with every blocked decision counted, so that the reason
// a shape did or did not trade can be read. It is NOT a measurement under any protocol: the
// window is chosen, the shape is chosen, and a shape that looks good on the window it was tuned
// on has been tuned on it. Every run is recorded (an analysis_result row, key strategy.exercise)
// so that the number of shapes tried is on the record beside the number registered. And no
// figure here is one of the v3 protocol's statistics: the tool answers no L_hat and no
// model-against-mid Brier, and refuses windows past TRAIN's end until TEST has been looked at
// (guard.go).
//
// What the tape cannot give the replay, said plainly:
//
//   - Snapshots before release 87a2100 (2026-09-21 07:29 UTC) carry no depth: their book has no
//     levels, so nothing is decided on them. They are counted (SnapshotsWithoutDepth).
//   - A snapshot whose model view was not ok (no strike, no fresh price) decides nothing, as live.
//   - vol_ratio was journaled from 2026-09-22; a shape with min_vol_ratio on earlier rows is
//     blocked as a quiet market. Counted (SnapshotsWithoutVolRatio).
//   - The replay steps the snapshots the tape has at the thinning asked for (step_s, every
//     recorded second by default); a settlement is applied at the first snapshot at or after
//     the market's close, a few seconds before the live runner would have had the result.
//     min_gap and min_hold are read against the stepped clock.
//   - One account, alone in its paper world: no other bucket's holds thin its book, exactly as
//     the analysis layer assumes for the live buckets.
//   - It is not a copy of a live bucket. Checked on 2026-09-23 against two registered versions
//     over their own windows: entry seconds agreed almost everywhere and the counts matched
//     (241 against 240 bets, 121 windows both; 76 against 75, 55 both), but the stakes drifted
//     by a contract or two and the P&L by about $25 on $1,400 staked, for three reasons the
//     live path has and this one does not: the runner drops a coin's second when another
//     coin's write holds its lock (so the two coins' order inside a second can differ, and
//     with it which one takes the window's Kelly stake); the sustainment allocation lowers a
//     live bucket's cash after each new high; and the settlement timing above.
package exercise

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/doipster/asset_cracker/service/internal/analysis"
	"github.com/doipster/asset_cracker/service/internal/broker"
	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// Snapshot is one recorded second of one market as the replay needs it: the book, the model's
// view of the coin at that second, and what the market did in the end.
type Snapshot struct {
	EvaluationID int64
	At           time.Time
	MarketID     int64
	Ticker       string
	Coin         string // "BTC"
	Symbol       string // "KXBTC15M"
	Strike       float64
	Closes       time.Time
	Result       string // "yes" or "no": only settled markets are replayed
	Price        float64
	Quotes       kalshi.Quotes
	View         engine.View // rebuilt from the journaled model.v3; OK false when it was not ok
	HasDepth     bool        // the snapshot recorded bid levels
	HasVolRatio  bool        // the view carried vol_ratio
}

// Tape is where the snapshots come from, in time order. Next returns false at the end.
type Tape interface {
	Next(ctx context.Context) (Snapshot, bool, error)
}

// SliceTape is a Tape over a slice, for tests and for a reader that has already sorted.
type SliceTape struct {
	Snaps []Snapshot
	i     int
}

func (t *SliceTape) Next(_ context.Context) (Snapshot, bool, error) {
	if t.i >= len(t.Snaps) {
		return Snapshot{}, false, nil
	}
	s := t.Snaps[t.i]
	t.i++
	return s, true, nil
}

// Cut is one slice of the result: the markets entered in this band, what they cost and returned.
type Cut struct {
	Band            string  `json:"band"`
	Markets         int     `json:"markets"`
	StakedCents     int64   `json:"staked_cents"`
	PnLCents        int64   `json:"pnl_cents"`
	ReturnPerDollar float64 `json:"return_per_dollar"`
	Won             int     `json:"won"` // markets whose P&L was positive
	AvgEntry        float64 `json:"avg_entry_price,omitempty"`
}

// Blocked is one reason a decision sent nothing, and how often.
type Blocked struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// Orders is what the broker did with one kind of order.
type Orders struct {
	Orders    int `json:"orders"`
	Requested int `json:"requested_contracts"`
	Filled    int `json:"filled_contracts"`
	Whole     int `json:"filled_whole"` // orders that filled everything asked
	Partial   int `json:"partial"`
	Cancelled int `json:"cancelled"` // reached the book, nothing filled
	Rejected  int `json:"rejected"`  // never reached a book
}

// Result is what a run answers.
type Result struct {
	Simulated bool   `json:"simulated"`
	Name      string `json:"name"`
	Family    string `json:"family"`
	Blurb     string `json:"blurb"`

	From                     time.Time `json:"from"`
	To                       time.Time `json:"to"`
	StepS                    int       `json:"step_s"`
	Markets                  int       `json:"markets"`   // settled markets in the window
	Windows                  int       `json:"windows"`   // distinct closes among them
	Snapshots                int       `json:"snapshots"` // stepped snapshots of the coins the model prices
	SnapshotsWithoutDepth    int       `json:"snapshots_without_depth"`
	SnapshotsWithoutView     int       `json:"snapshots_without_view"` // the model had no view that second (no strike, no fresh price)
	SnapshotsWithoutVolRatio int       `json:"snapshots_without_vol_ratio"`
	SnapshotsUnpriced        int       `json:"snapshots_unpriced"` // of coins the model never priced (no v3 key): counted, not read, never entered

	SeedCents          int64             `json:"seed_cents"`
	FinalCashCents     int64             `json:"final_cash_cents"`
	Exhausted          bool              `json:"exhausted"`
	Bets               int               `json:"bets"`  // buy orders with a fill
	Sells              int               `json:"sells"` // sell orders with a fill
	WindowsWithBet     int               `json:"windows_with_bet"`
	PnLCents           int64             `json:"pnl_cents"`    // realised: payouts and sale proceeds less what the bets cost, fees inside
	StakedCents        int64             `json:"staked_cents"` // what the bets cost, fees inside
	FeesCents          int64             `json:"fees_cents"`   // venue fees on buys and sales
	ReturnPerDollar    float64           `json:"return_per_dollar"`
	MeanWindowPnLCents float64           `json:"mean_window_pnl_cents"`
	SECents            float64           `json:"se_cents"`
	T                  float64           `json:"t"`
	TopWindowShare     float64           `json:"top_window_share"`
	Drawdown           analysis.Drawdown `json:"drawdown"`

	ByEntryBand      []Cut      `json:"by_entry_band"`
	ByEntryPrice     []Cut      `json:"by_entry_price"`
	Blocked          []Blocked  `json:"blocked"`
	Decisions        int        `json:"decisions"`
	Buys             Orders     `json:"buys"`
	SellOrders       Orders     `json:"sell_orders"`
	Series           [][2]int64 `json:"series"`  // [close, cumulative P&L cents], at most 300 points
	Entries          []Entry    `json:"entries"` // every order that filled, oldest first, at most MaxEntries
	EntriesTruncated bool       `json:"entries_truncated,omitempty"`

	Params *engine.Params `json:"params,omitempty"` // the version as built, every number labelled; left out of the record, whose shape is its build
	Note   string         `json:"note"`
}

// Price bands of the first fill of a market, as the attribution of 2026-09-23 cut them.
var priceBands = []struct {
	Name string
	Max  float64 // exclusive
}{{"under 0.20", 0.20}, {"0.20 to 0.40", 0.40}, {"0.40 to 0.60", 0.60}, {"0.60 to 0.80", 0.80}, {"0.80 and above", math.Inf(1)}}

func priceBandOf(p float64) int {
	for i, b := range priceBands {
		if p < b.Max {
			return i
		}
	}
	return len(priceBands) - 1
}

// Entry is one order that filled, as the replay booked it: the evidence behind the row.
type Entry struct {
	Ticker    string  `json:"ticker"`
	At        int64   `json:"at"`  // unix seconds
	Tau       float64 `json:"tau"` // seconds to the close when the order was formed
	Action    string  `json:"action"`
	Side      string  `json:"side"`
	Why       string  `json:"why"` // entry | value | capture
	Requested int     `json:"requested"`
	Filled    int     `json:"filled"`
	Price     float64 `json:"price"`      // the first fill's taker price
	CashCents int64   `json:"cash_cents"` // what left (negative) or reached the bucket, fees inside
	Kelly     float64 `json:"kelly,omitempty"`
	Binding   string  `json:"binding,omitempty"` // which limit set a buy's stake
}

// MaxEntries is how many fills an answer lists; the counts cover them all.
const MaxEntries = 400

// perMarket is what one market cost and returned, and where it was entered.
type perMarket struct {
	closes     int64
	firstTau   float64
	firstPrice float64
	entered    bool
	cost, back int64 // cents out on buys (fees inside); cents in on sales and settlement
}

// Run replays one shape over a tape. seedCents is the bucket's starting cash; 0 takes the
// params' convention. The tape must be in time order; a snapshot out of order is an error, so a
// reader's mistake cannot become a quietly wrong replay.
func Run(ctx context.Context, p engine.Params, seedCents int64, tape Tape, stepS int) (Result, error) {
	if seedCents <= 0 {
		seedCents = p.SeedCents
	}
	acct := engine.NewAccount(p, 1, seedCents, true)
	acct.SeedCents = seedCents
	eng, err := engine.NewEngine(acct)
	if err != nil {
		return Result{}, err
	}
	paper := broker.NewPaper(p.Levels)
	paper.FeePerFill = p.FeePerFill

	res := Result{Simulated: true, Name: p.Name, Family: p.FamilyOf(), Blurb: p.Blurb, StepS: stepS, SeedCents: seedCents, Params: &p}
	markets := map[int64]*perMarket{}  // by market id
	byTicker := map[string]*Snapshot{} // the last snapshot of each market, for its settlement
	pending := map[string]bool{}       // markets seen and not yet settled
	blocked := map[string]int{}
	var last time.Time

	settleDue := func(now time.Time, all bool) {
		var due []string
		for ticker := range pending {
			if s := byTicker[ticker]; all || !s.Closes.After(now) {
				due = append(due, ticker)
			}
		}
		sort.Strings(due) // a fixed order, so a rerun reproduces the events
		for _, ticker := range due {
			s := byTicker[ticker]
			for _, ev := range eng.ApplySettlement(ticker, s.Result) {
				if ev.Kind == "settled" {
					markets[s.MarketID].back += ev.CashCents
				}
			}
			paper.Forget(ticker)
			delete(pending, ticker)
		}
		eng.Prune(engine.UnixSeconds(now))
	}

	for {
		s, ok, err := tape.Next(ctx)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			break
		}
		if s.At.Before(last) {
			return Result{}, fmt.Errorf("the tape is out of order: %s after %s", s.At.Format(time.RFC3339Nano), last.Format(time.RFC3339Nano))
		}
		last = s.At
		res.Snapshots++
		if !s.HasDepth {
			res.SnapshotsWithoutDepth++
		}
		if !s.View.OK {
			res.SnapshotsWithoutView++
		} else if !s.HasVolRatio {
			res.SnapshotsWithoutVolRatio++
		}
		settleDue(s.At, false)
		if _, seen := markets[s.MarketID]; !seen {
			markets[s.MarketID] = &perMarket{closes: s.Closes.Unix()}
		}
		snap := s
		byTicker[s.Ticker] = &snap
		pending[s.Ticker] = true

		m := engine.Market{Ticker: s.Ticker, MarketID: s.MarketID, EvaluationID: s.EvaluationID, Strike: s.Strike, Close: engine.UnixSeconds(s.Closes)}
		now := engine.UnixSeconds(s.At)
		paper.ObserveBook(s.Ticker, s.EvaluationID, s.At, s.Closes, s.Quotes)
		if acct.Exhausted {
			continue // it ran out: nothing more is decided, the rest settles
		}
		v := s.View
		v.Tau = m.Close - now
		decisions, intents := eng.Decide(s.Coin, m, s.Quotes, v, now)
		eng.AfterDecide(s.Ticker, decisions)
		res.Decisions += len(decisions)
		for _, d := range decisions {
			if d.Intent >= 0 {
				continue
			}
			why := d.BlockedBy
			if why == "" {
				why = d.Why
			}
			if why == "" {
				why = "nothing to do"
			}
			blocked[why]++
		}
		if len(intents) == 0 {
			continue
		}
		reports := make([]broker.Report, 0, len(intents))
		ids := make([]string, 0, len(intents))
		for _, in := range intents {
			rep, err := paper.Submit(ctx, in.Order)
			if err != nil {
				return Result{}, err
			}
			reports, ids = append(reports, rep), append(ids, in.Order.ClientID)
		}
		events := eng.Apply(intents, reports)
		paper.Commit(ids...)
		for i, in := range intents {
			tallyOrder(&res, markets[s.MarketID], in, reports[i], m.Close-now)
		}
		for _, ev := range events {
			switch ev.Kind {
			case "bought":
				markets[s.MarketID].cost -= ev.CashCents // CashCents is negative on a buy
			case "sold":
				markets[s.MarketID].back += ev.CashCents
			case "inconsistent":
				return Result{}, fmt.Errorf("the engine reports an inconsistency: %s", ev.Note)
			}
		}
		res.noteEntries(intents, reports, events, m.Close-now)
	}
	settleDue(last, true)

	res.FinalCashCents = acct.CashCents
	res.Exhausted = acct.Exhausted
	res.Bets = acct.Bets
	summarise(&res, markets, blocked)
	if res.Entries == nil {
		res.Entries = []Entry{}
	}
	return res, nil
}

// noteEntries lists the step's fills, up to MaxEntries. Events come out in intent order, one per
// intent that filled ("bought" / "sold"), so the cash figure is read from the matching event.
func (r *Result) noteEntries(intents []engine.Intent, reports []broker.Report, events []engine.Event, tau float64) {
	filled := map[string]int64{} // client id -> cash
	for _, ev := range events {
		if ev.Kind == "bought" || ev.Kind == "sold" {
			filled[ev.Ticker+"|"+ev.Side+"|"+ev.Kind] += ev.CashCents
		}
	}
	for i, in := range intents {
		n := 0
		for _, f := range reports[i].Fills {
			n += f.Qty
		}
		if n == 0 {
			continue
		}
		if len(r.Entries) >= MaxEntries {
			r.EntriesTruncated = true
			return
		}
		kind := "bought"
		if in.Order.Action == broker.Sell {
			kind = "sold"
		}
		e := Entry{Ticker: in.Order.Ticker, At: in.Order.At.Unix(), Tau: math.Round(tau), Action: string(in.Order.Action), Side: string(in.Order.Side), Why: in.Why,
			Requested: in.Order.Qty, Filled: n, Price: float64(reports[i].Fills[0].Price) / 10000, CashCents: filled[in.Order.Ticker+"|"+string(in.Order.Side)+"|"+kind]}
		if in.Order.Action == broker.Buy {
			e.Kelly = round(in.Kelly, 4)
			if b, ok := in.Detail["binding"].(string); ok {
				e.Binding = b
			}
		}
		r.Entries = append(r.Entries, e)
	}
}

// tallyOrder counts one order's answer and, for a first buy, notes where the market was entered.
func tallyOrder(res *Result, pm *perMarket, in engine.Intent, r broker.Report, tau float64) {
	o := &res.Buys
	if in.Order.Action == broker.Sell {
		o = &res.SellOrders
	}
	o.Orders++
	o.Requested += in.Order.Qty
	filled := 0
	for _, f := range r.Fills {
		filled += f.Qty
		res.FeesCents += f.FeeCents
	}
	o.Filled += filled
	switch {
	case r.Status == broker.Rejected:
		o.Rejected++
	case filled == 0:
		o.Cancelled++
	case r.Unfilled > 0:
		o.Partial++
	default:
		o.Whole++
	}
	if in.Order.Action == broker.Sell {
		if filled > 0 {
			res.Sells++
		}
		return
	}
	if filled > 0 && !pm.entered {
		pm.entered = true
		pm.firstTau = tau
		pm.firstPrice = float64(r.Fills[0].Price) / 10000
	}
}

// summarise turns the per-market ledger into the row, the cuts and the series.
func summarise(res *Result, markets map[int64]*perMarket, blocked map[string]int) {
	res.Markets = len(markets)
	closes := map[int64]bool{}
	byClose := map[int64]int64{} // window P&L
	betClose := map[int64]bool{} // windows with a bet
	bands := make([]Cut, len(analysis.Bands))
	for i, b := range analysis.Bands {
		bands[i].Band = b.Label
	}
	prices := make([]Cut, len(priceBands))
	sumPrice := make([]float64, len(priceBands))
	for i, b := range priceBands {
		prices[i].Band = b.Name
	}
	for _, pm := range markets {
		closes[pm.closes] = true
		pnl := pm.back - pm.cost
		byClose[pm.closes] += pnl
		if !pm.entered {
			continue
		}
		betClose[pm.closes] = true
		res.StakedCents += pm.cost
		res.PnLCents += pnl
		for _, c := range []*Cut{&bands[analysis.BandOf(pm.firstTau)], &prices[priceBandOf(pm.firstPrice)]} {
			c.Markets++
			c.StakedCents += pm.cost
			c.PnLCents += pnl
			if pnl > 0 {
				c.Won++
			}
		}
		sumPrice[priceBandOf(pm.firstPrice)] += pm.firstPrice
	}
	res.Windows = len(closes)
	res.WindowsWithBet = len(betClose)
	for i := range bands {
		bands[i].ReturnPerDollar = ratio(bands[i].PnLCents, bands[i].StakedCents)
	}
	for i := range prices {
		prices[i].ReturnPerDollar = ratio(prices[i].PnLCents, prices[i].StakedCents)
		if prices[i].Markets > 0 {
			prices[i].AvgEntry = round(sumPrice[i]/float64(prices[i].Markets), 3)
		}
	}
	res.ByEntryBand, res.ByEntryPrice = bands, prices
	res.ReturnPerDollar = ratio(res.PnLCents, res.StakedCents)

	// Windows with a bet, in time order: the leaderboard's unit.
	keys := make([]int64, 0, len(betClose))
	for k := range betClose {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	per := make([]float64, len(keys))
	var cum int64
	series := make([][2]int64, 0, len(keys))
	for i, k := range keys {
		per[i] = float64(byClose[k])
		cum += byClose[k]
		series = append(series, [2]int64{k, cum})
	}
	st := analysis.WindowStat(per)
	res.MeanWindowPnLCents, res.SECents, res.T = round(st.Mean, 0), round(st.SE, 0), round(st.T, 2)
	res.TopWindowShare = round(analysis.TopWindowShare(per), 2)
	res.Drawdown = analysis.DrawdownOf(per)
	res.Series = thin(series, 300)

	res.Blocked = make([]Blocked, 0, len(blocked))
	for why, n := range blocked {
		res.Blocked = append(res.Blocked, Blocked{why, n})
	}
	sort.Slice(res.Blocked, func(i, j int) bool {
		if res.Blocked[i].Count != res.Blocked[j].Count {
			return res.Blocked[i].Count > res.Blocked[j].Count
		}
		return res.Blocked[i].Reason < res.Blocked[j].Reason
	})
}

func ratio(num, den int64) float64 {
	if den == 0 {
		return 0
	}
	return round(float64(num)/float64(den), 4)
}

func round(v float64, places int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// thin keeps at most n points of a series: each bin's LAST point, so every point kept is a
// figure that stood at that moment, and the last point is always the last.
func thin(s [][2]int64, n int) [][2]int64 {
	if len(s) <= n {
		return s
	}
	out := make([][2]int64, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, s[i*len(s)/n-1])
	}
	return out
}
