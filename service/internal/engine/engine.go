package engine

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/doipster/asset_cracker/service/internal/broker"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// Market is one coin's open round as the engine needs it. EvaluationID is the snapshot this look
// is made on: it goes into every order, where the broker checks it against the book it holds.
type Market struct {
	Ticker       string
	MarketID     int64
	EvaluationID int64
	Strike       float64
	Close        float64 // unix seconds
}

type posKey struct{ Ticker, Side string }

// Account is one version's bucket: one balance shared across every coin. CashCents IS the ledger
// balance: it changes only in Fold, ApplySettlement and Withdraw, by the figures the store booked.
type Account struct {
	Params    Params
	BucketID  int64
	CashCents int64
	// SeedCents is what THIS bucket was seeded with, from the ledger: the figure the window cap
	// and the martingale stake are measured against (Seed). Every bucket used to start at the
	// params' convention; a bucket the operator deploys at another figure sizes off its own.
	// Zero means unknown, and the params' figure stands.
	SeedCents int64
	Positions map[posKey]*Position
	Windows   map[int64]*Window // by close, unix seconds; pruned once over and empty
	// MayOrder: AC_V3 on AND the version was probation/active at start. False = settle-only: held,
	// valued and settled, never ordering. It is read when the engine is BUILT: only an account that
	// may order has its params validated, so one built settle-only stays so for the engine's life
	// (Decide skips it whatever this field says later). To let it order, build a new engine.
	MayOrder  bool
	Exhausted bool // ran out: places no more orders
	Bets      int  // buy orders with a fill
	// LossStreak is the run of consecutive SETTLED positions that paid less than they cost, for
	// martingale sizing (Params.Sizing). ApplySettlement moves it; a win resets it; an early sale
	// does not count. The rebuild sets it from the ledger's settlements (store.SettledStreak), so
	// live and rebuilt agree by the same rule.
	LossStreak int

	validated bool // the engine checked Params when it was built; without it Decide forms no order
}

// FixedStake is the martingale stake for the next entry, in cents: base times multiplier for
// each consecutive loss, up to max_doublings. 0 for a version sized by Kelly.
func (a *Account) FixedStake() int64 {
	p := a.Params
	if p.Sizing != SizingMartingale || p.BaseStakeCents <= 0 {
		return 0
	}
	n := min(a.LossStreak, p.MaxDoublings)
	stake := float64(p.BaseStakeCents)
	for i := 0; i < n; i++ {
		stake *= p.Multiplier
	}
	if stake > float64(a.Seed()) {
		stake = float64(a.Seed())
	}
	return int64(math.Floor(stake))
}

// Seed is the figure this account's sizing is measured against: the bucket's own seed when the
// rebuild read one from the ledger, else the params' convention.
func (a *Account) Seed() int64 {
	if a.SeedCents > 0 {
		return a.SeedCents
	}
	return a.Params.SeedCents
}

// NewAccount is an account holding cashCents and nothing else. The rebuild makes one per held
// bucket from the ledger balance and folds the recorded fills into it.
func NewAccount(p Params, bucketID, cashCents int64, mayOrder bool) *Account {
	return &Account{Params: p, BucketID: bucketID, CashCents: cashCents, MayOrder: mayOrder,
		Positions: map[posKey]*Position{}, Windows: map[int64]*Window{}}
}

// Position is what the account holds on one side of a market, nil if nothing.
func (a *Account) Position(ticker, side string) *Position { return a.Positions[posKey{ticker, side}] }

// Open is a copy of every open position, in a fixed order, for valuing the book.
func (a *Account) Open() []Position {
	out := make([]Position, 0, len(a.Positions))
	for _, p := range a.Positions {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ticker != out[j].Ticker {
			return out[i].Ticker < out[j].Ticker
		}
		return out[i].Side < out[j].Side
	})
	return out
}

// Withdraw lowers the cash by the sustainment allocation the store has booked out of the bucket
// (platform brief, section 6). Like Fold and ApplySettlement it is applied only AFTER the ledger
// has the transfer. It refuses more than the account holds in cash: the allocation is a share
// of a gain that is already cash, never of what is committed to an open bet.
func (a *Account) Withdraw(cents int64) error {
	if cents < 0 {
		return fmt.Errorf("withdraw %d cents from bucket %d: negative", cents, a.BucketID)
	}
	if cents > a.CashCents {
		return fmt.Errorf("withdraw %d cents from bucket %d, which has %d", cents, a.BucketID, a.CashCents)
	}
	a.CashCents -= cents
	return nil
}

// RanOut reports an account with less than the floor in cash and nothing open. An open position
// is excluded on purpose: while a bet is live the account still has something that might pay,
// and closing its bucket would strand the lot. The rebuild calls this LAST, after every fill has
// been folded, and never before (plan 5.4, step 7).
func (a *Account) RanOut() bool {
	return a.CashCents < a.Params.ExhaustedCents && len(a.Positions) == 0
}

// Engine is the accounts. It has no lock, no clock and no database: its runner guards it.
type Engine struct {
	Accounts []*Account // in a fixed order; events and intents come out in it
	plumbing bool
}

// NewEngine refuses any account that MAY ORDER and whose params may not be registered
// (Params.Validate): there is no way to trade a version whose lambda is not a measurement through
// this constructor.
//
// A settle-only account (MayOrder false) is taken whatever its params say. Holding, valuing and
// settling read no parameter but the name and the exhaustion floor, and plan 5.4 has a bucket
// held "whatever AC_V3 or the version's status says". Validation is by reflection on purpose, so
// a numeric field added to Params in a later release makes every params row stored before it
// fail; those rows are immutable history, and refusing them here would leave a retired bucket's
// open contracts unheld and unsettled. Such an account can never order: see Account.MayOrder.
func NewEngine(accounts ...*Account) (*Engine, error) { return newEngine(false, accounts) }

// NewPlumbingEngine lets placeholder numbers through, for tests and the dev plumbing versions of
// step S5. Every order such an engine forms carries "plumbing": true in its detail.
func NewPlumbingEngine(accounts ...*Account) (*Engine, error) { return newEngine(true, accounts) }

func newEngine(plumbing bool, accounts []*Account) (*Engine, error) {
	seen := map[int64]bool{}
	for _, a := range accounts {
		a.validated = false
		if a.MayOrder { // only ordering needs parameters
			if err := a.Params.validate(plumbing); err != nil {
				return nil, err
			}
			a.validated = true
		}
		if seen[a.BucketID] {
			return nil, fmt.Errorf("engine: bucket %d appears twice", a.BucketID)
		}
		seen[a.BucketID] = true
		if a.Positions == nil {
			a.Positions = map[posKey]*Position{}
		}
		if a.Windows == nil {
			a.Windows = map[int64]*Window{}
		}
	}
	return &Engine{Accounts: accounts, plumbing: plumbing}, nil
}

// Plumbing reports an engine built with placeholder numbers allowed.
func (e *Engine) Plumbing() bool { return e.plumbing }

// Account finds an account by bucket.
func (e *Engine) Account(bucketID int64) *Account {
	for _, a := range e.Accounts {
		if a.BucketID == bucketID {
			return a
		}
	}
	return nil
}

// Decision is one account's answer for one coin this second, as journaled. Action is the INTENT
// (enter, exit or none); whether an order filled is on the order. BlockedBy is never set on a
// decision that sent an order.
type Decision struct {
	BucketID   int64
	Strategy   string
	HasProb    bool    // false: there was no model view or no two-sided book, and the three figures below are empty
	ModelProb  float64 // the RAW p_model, so the scorecard means the same for every engine
	MarketProb float64 // the mid
	P          float64 // the blend the engine acted on
	Edge       float64
	Side       string
	Action     string // none | enter | exit
	BlockedBy  string
	Why        string
	Requested  int // contracts asked for; 0 when no order
	Intent     int // index into the intents returned beside it, or -1
	// ClearExit names a side whose exit is no longer wanted: the rule was evaluated afresh on this
	// second's book and did not fire. Decide changes nothing, so it only says so; AfterDecide acts.
	ClearExit string
}

// Intent is an order the engine would like sent, and what it knew when it formed it.
type Intent struct {
	Order    broker.Order
	Strategy string
	Decision int    // index into the decisions returned beside it
	Why      string // entry | value | capture
	Coin     string
	Close    float64
	Strike   float64
	Kelly    float64 // buys: k at the best ask that shows a whole contract
	Window   Window  // the window as it stood BEFORE this order; for a first buy, what it would start as
	Detail   map[string]any
}

// Decide is one look at one coin's open round by every account that may order. It is PURE: it
// moves no money, changes no account and reads no CoinState (the view was computed by Model). It
// returns what to journal and what to send.
//
// Per account (plan 4.4): first, a position here that wants out gets one sell intent for ALL it
// holds on that side, and then there is no entry in the same step. Otherwise, maybe an entry. An
// account that may not order (settle-only) or has run out is skipped before either.
func (e *Engine) Decide(coin string, m Market, book kalshi.Quotes, v View, now float64) ([]Decision, []Intent) {
	sides := broker.Ladders(book)
	var decisions []Decision
	var intents []Intent
	for _, a := range e.Accounts {
		if !a.MayOrder || !a.validated || a.Exhausted {
			continue
		}
		ds, ins := a.decide(coin, m, sides, v, now)
		for i := range ins {
			ins[i].Decision += len(decisions)
			ins[i].Order.ClientID = fmt.Sprintf("v3:%d:%d:%d", a.BucketID, m.EvaluationID, i+1)
			if e.plumbing {
				ins[i].Detail["plumbing"] = true
			}
		}
		for i := range ds {
			if ds[i].Intent >= 0 {
				ds[i].Intent += len(intents)
			}
		}
		decisions, intents = append(decisions, ds...), append(intents, ins...)
	}
	return decisions, intents
}

// unixTime is the clock reading as a time, to the NEAREST whole microsecond: that is what
// Postgres keeps of placed_at, so the time a rebuild reads back is the time the live fold used.
// Nearest, not cut: a float64 near 1.79e9 seconds resolves about a quarter of a microsecond, so a
// whole-microsecond time that went through UnixSeconds comes back as 123456.99996 as often as
// not, and cutting it would put the order a microsecond before the evaluation it was decided on.
// unixTime(UnixSeconds(t)) == t for every whole-microsecond t (tested over a whole second).
func unixTime(ts float64) time.Time {
	sec := math.Floor(ts)
	usec := int64(math.Round((ts - sec) * 1e6)) // ts - sec is exact; its error is the float's own, under half a microsecond
	return time.Unix(int64(sec), 0).Add(time.Duration(usec) * time.Microsecond).UTC()
}

// UnixSeconds is a stored time as the fold's clock reads it. Live and rebuild must turn a time
// into a float the same way or LastBuyAt differs in its last bit after a restart, so both use
// this: BookedFrom on the order's At, and the rebuild on trade_order.placed_at.
func UnixSeconds(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }

func windowKey(closeAt float64) int64 { return int64(math.Round(closeAt)) }

func ladderOf(s broker.Sides, l broker.Ladder, levels int) ([]broker.Level, error) {
	if s.Crossed {
		return nil, fmt.Errorf("%s", broker.ReasonCrossed)
	}
	out, err := s.Yes, s.YesErr
	if l == broker.NoBids {
		out, err = s.No, s.NoErr
	}
	if err != nil {
		return nil, err
	}
	if levels >= 1 && len(out) > levels {
		out = out[:levels]
	}
	return out, nil
}

// wholeLevels drops the levels that show less than one whole contract. The broker passes over
// them (paper.go, fill), so no order is ever filled at their price.
func wholeLevels(levels []broker.Level) []broker.Level {
	out := make([]broker.Level, 0, len(levels))
	for _, lv := range levels {
		if lv.Size >= 1 {
			out = append(out, lv)
		}
	}
	return out
}

// wholeAsks is the taker prices a buy of side can really be filled at, best first: the recorded
// levels an order may walk that show at least one whole contract. Nil if the side cannot be read.
func wholeAsks(sides broker.Sides, side broker.Side, levels int) []broker.Price {
	shown, err := ladderOf(sides, broker.LadderFor(broker.Buy, side), levels)
	if err != nil {
		return nil
	}
	var out []broker.Price
	for _, lv := range wholeLevels(shown) {
		out = append(out, broker.TakerPrice(broker.Buy, lv.Bid))
	}
	return out
}

func (a *Account) decide(coin string, m Market, sides broker.Sides, v View, now float64) ([]Decision, []Intent) {
	prm := a.Params
	tau := m.Close - now
	tp, two := touch(sides)
	blank := Decision{BucketID: a.BucketID, Strategy: prm.Name, Action: "none", Intent: -1}
	if v.OK {
		blank.ModelProb = v.PModel
	}
	if two && v.OK {
		// The weight on the model is the version's, or its late-window one inside lambda_late_tau
		// seconds of the close (Params.LambdaAt): one number for a version without a late window.
		blank.HasProb, blank.MarketProb, blank.P = true, tp.Mid, Blend(tp.Mid, v.PModel, prm.LambdaAt(tau))
	}

	// 1. Exits.
	var decisions []Decision
	var intents []Intent
	for _, side := range []broker.Side{broker.Yes, broker.No} {
		pos := a.Positions[posKey{m.Ticker, string(side)}]
		if pos == nil || pos.Contracts < 1 {
			continue
		}
		d, in := a.exit(pos, side, coin, m, sides, v, blank, tau, now)
		if d == nil {
			continue
		}
		if in != nil {
			d.Intent, in.Decision = len(intents), len(decisions)
			intents = append(intents, *in)
		}
		decisions = append(decisions, *d)
	}
	if len(decisions) > 0 { // an exit was sent, or is wanted and blocked: no entry in this step
		return decisions, intents
	}

	// 2. Maybe an entry.
	d := blank
	switch {
	case !v.OK:
		d.BlockedBy = BlockedNoModel
	case !two:
		d.BlockedBy = BlockedNoBook
	}
	if d.BlockedBy != "" {
		d.Why = d.BlockedBy
		return []Decision{d}, nil
	}

	// Kalshi keeps one net position per market, so once the account holds a side in this round it
	// may only add to that side, or sell it.
	var held *Position
	for _, side := range []broker.Side{broker.Yes, broker.No} {
		if p := a.Positions[posKey{m.Ticker, string(side)}]; p != nil && p.Contracts > 0 && held == nil {
			held = p
		}
	}
	// The ask of a side is the best price the broker will really GIVE: the first recorded level
	// that shows at least one whole contract. A top level showing 0.62 of a contract is a resting
	// order and moves the mid, but no contract can be had there (the broker walks past it), so the
	// edge, the band test, the Kelly fraction and the k_max the window records are all taken at the
	// first price with a contract behind it. Otherwise the window's budget for every other coin
	// would be quarter-Kelly of a bet that could not be had.
	staleUnits, _ := priceUnits(prm.StaleCost)
	var bestSide broker.Side
	var bestAsks []broker.Price
	bestEdge := math.Inf(-1)
	sideBlocked := false                                        // a side the version does not buy had the edge
	for _, side := range []broker.Side{broker.Yes, broker.No} { // the first of equals wins
		if held != nil && held.Side != string(side) {
			continue
		}
		asks := wholeAsks(sides, side, prm.Levels)
		if len(asks) == 0 {
			continue
		}
		edge := sideProb(d.P, side) - dollars(buyUnitE10(asks[0], staleUnits))
		if !sideAllowed(prm.Side, side, tp.Mid) {
			sideBlocked = sideBlocked || edge > 0
			continue
		}
		if edge > bestEdge {
			bestSide, bestAsks, bestEdge = side, asks, edge
		}
	}
	var bestAsk broker.Price
	if len(bestAsks) > 0 { // otherwise the side and the edge stay empty: there is no price to have one at
		bestAsk = bestAsks[0]
		d.Side, d.Edge = string(bestSide), bestEdge
	}
	pSide := sideProb(d.P, bestSide)

	var sz Sizing
	var heldCents int64
	switch c := priceDollars(bestAsk); {
	case v.Drift:
		d.BlockedBy = BlockedDrift
	case tau < prm.MinTau || tau < prm.TauMin:
		d.BlockedBy = BlockedTooLate
	case tau > prm.TauMax:
		d.BlockedBy = BlockedTooEarly
	case prm.MinVolRatio > 0 && !(v.VolRatio >= prm.MinVolRatio):
		d.BlockedBy = BlockedQuietMarket
	case len(bestAsks) == 0 && sideBlocked:
		d.BlockedBy = BlockedSide
	case len(bestAsks) == 0:
		d.BlockedBy = BlockedNoSize
	case !(prm.BandMin <= c && c <= prm.BandMax):
		d.BlockedBy = BlockedBand
	case held != nil && held.Entries >= prm.MaxBets:
		d.BlockedBy = BlockedMaxBets
	case held != nil && now-held.LastBuyAt < prm.MinGap:
		d.BlockedBy = BlockedCoolingDown
	case !(bestEdge > 0):
		d.BlockedBy = BlockedNoEdge
	default:
		// The band holds for every price the order may walk to, not only the touch: the limit is
		// found among these, so no fill can land above band_max. (The plan tests the best ask only;
		// "the ask must lie in this band to be bought" is read here as every ask bought at.) The
		// best ask has just passed the same comparison, so asks is never empty.
		asks := bestAsks
		for i, t := range asks {
			if priceDollars(t) > prm.BandMax {
				asks = asks[:i]
				break
			}
		}
		w := a.windowOrFresh(m.Close)
		in := SizeInput{PSide: pSide, Asks: asks, StaleUnits: staleUnits, Kappa: prm.Kappa, CapBps: prm.WindowCapBps,
			SeedCents: a.Seed(), EquityCents: w.EquityCents, KMax: w.KMax, UsedCents: w.Used(), CashCents: a.CashCents,
			FixedStakeCents: a.FixedStake()}
		if held != nil { // the same side: the loop above lets the account add only to the side it holds
			in.HeldCents = held.CostCents
		}
		heldCents = in.HeldCents
		sz = Size(in)
		d.BlockedBy = sz.BlockedBy
	}
	if d.BlockedBy != "" {
		d.Why = d.BlockedBy
		return []Decision{d}, nil
	}

	w := a.windowOrFresh(m.Close)
	d.Action, d.Why, d.Requested, d.Intent = "enter", "entry", sz.Qty, 0
	steps := make([][2]int64, len(sz.Steps))
	for i, s := range sz.Steps {
		steps[i] = [2]int64{int64(s.UpTo), s.MaxCostCents}
	}
	in := Intent{Strategy: prm.Name, Why: "entry", Coin: coin, Close: m.Close, Strike: m.Strike, Kelly: sz.Kelly, Window: w,
		Order: broker.Order{BucketID: a.BucketID, MarketID: m.MarketID, Ticker: m.Ticker, Action: broker.Buy, Side: bestSide,
			Qty: sz.Qty, Limit: sz.Limit, MaxCostCents: sz.StakeCents, CostSteps: sz.Steps, EvaluationID: m.EvaluationID, At: unixTime(now)}}
	in.Detail = a.detail(in, d, v)
	in.Detail["kelly"], in.Detail["binding"], in.Detail["cost_steps"] = sz.Kelly, sz.Binding, steps
	in.Detail["budget_cents"], in.Detail["cap_cents"], in.Detail["room_cents"] = sz.BudgetCents, sz.CapCents, sz.RoomCents
	in.Detail["held_cents"] = heldCents // what the ceilings counted as already spent in this market
	if fixed := a.FixedStake(); fixed > 0 {
		in.Detail["martingale"] = map[string]any{"stake_cents": fixed, "loss_streak": a.LossStreak}
	}
	return []Decision{d}, []Intent{in}
}

// sideAllowed is the version's side filter (Params.Side) against the market's mid: the
// favourite is the side priced above one half, the longshot the side under it. At exactly one
// half there is no favourite, and neither filter lets a side through.
func sideAllowed(rule string, side broker.Side, mid float64) bool {
	pSide := sideProb(mid, side)
	switch rule {
	case SideFavourite:
		return pSide > 0.5
	case SideLongshot:
		return pSide < 0.5
	}
	return true
}

// windowOrFresh is the window of this close as it stands, or what it would start as: E_w is the
// bucket's cash now, frozen by Fold when the window's first buy fills.
func (a *Account) windowOrFresh(closeAt float64) Window {
	if w := a.Windows[windowKey(closeAt)]; w != nil {
		return *w
	}
	return Window{Close: windowKey(closeAt), EquityCents: a.CashCents}
}

// detail is the part of trade_order.detail known before the broker answers (plan section 3).
// Engine.Detail completes it with what filled.
func (a *Account) detail(in Intent, d Decision, v View) map[string]any {
	prm := a.Params
	// lambda is the weight this order's belief was formed with: the late-window one when the
	// order fell inside it, so the stored detail says what the engine did, not only what it holds.
	out := map[string]any{"v": 3, "coin": in.Coin, "ticker": in.Order.Ticker, "close": windowKey(in.Close), "strike": in.Strike,
		"requested": in.Order.Qty, "why": in.Why, "lambda": prm.LambdaAt(in.Close - UnixSeconds(in.Order.At)), "stale_cost": prm.StaleCost, "stale_cost_sell": prm.StaleCostSell,
		"edge": d.Edge, "limit": int64(in.Order.Limit),
		"window": map[string]any{"close": in.Window.Close, "equity_cents": in.Window.EquityCents, "k_max": in.Window.KMax,
			"open_cents": in.Window.OpenCents, "lost_cents": in.Window.LostCents}}
	if v.OK {
		out["p_model"] = v.PModel
	}
	if d.HasProb {
		out["mid"], out["p"] = d.MarketProb, d.P
	}
	return out
}

// exit decides what to do about one held position (plan 4.4, step 1). It returns nil, nil when
// nothing is wanted and nothing needs saying.
//
// The rule is evaluated FROM SCRATCH on this second's book whenever the position's side shows a
// bid: if it fires an order is sent for everything held; if it does not and an exit was pending,
// the want is dropped (Decision.ClearExit) and the rest is held. When the side shows NO bid the
// rule cannot be evaluated either way, so a pending want stands and the order IS still sent: the
// cancelled row is the evidence that the exit could not happen. A bid here is a level showing at
// least one whole contract: the broker walks past anything less, so it is no price to sell at.
//
// Both rules are tested on the ORDER, with the figures the ledger will book for it: all the
// contracts held, sold at the bid in question, premium rounded down and the order's fee rounded
// UP (bookedSaleCents), less the staleness cost. The plan writes the tests per contract with the
// unrounded fee; for a remainder of a few contracts at a low bid that calls a sale profitable
// which books no cash or costs cash. (A sale the book cuts short is rounded on fewer contracts
// and can still fall a fraction of a cent short of the rule; an immediate-or-cancel order cannot
// name a least quantity.)
//
// No sale is sent that could book nothing or less: see saleCanBookNothing. The walk down the bids
// stops above such a price, and when even the best bid is one, nothing is sent, the position is
// held to settlement (where it is never worth less than nothing) and the decision says why
// (BlockedExitNoProceeds). The same is said when the plan's per-contract rule would have sold and
// the whole order would book nothing, which is the case the review measured: one contract at
// 0.0091, 0c of premium and 1c of fee. So every sale this engine forms brings cash IN, and a sale
// can never take the bucket below zero.
func (a *Account) exit(pos *Position, side broker.Side, coin string, m Market, sides broker.Sides, v View, blank Decision, tau, now float64) (*Decision, *Intent) {
	prm := a.Params
	if prm.Exit != "ev" {
		return nil, nil
	}
	d := blank
	d.Side, d.Requested = string(side), 0
	if tau < prm.MinTau { // no more orders: it settles, booked as held to settlement
		if pos.Exiting == "" {
			return nil, nil
		}
		d.BlockedBy, d.Why = BlockedExitTooLate, pos.Exiting
		return &d, nil
	}
	if now-pos.LastBuyAt < prm.MinHold {
		return nil, nil
	}
	bids, err := ladderOf(sides, broker.LadderFor(broker.Sell, side), prm.Levels)
	if err != nil { // unreadable or crossed: no price in it can be trusted, and the broker would reject
		if pos.Exiting == "" {
			return nil, nil
		}
		d.BlockedBy, d.Why = BlockedExitNoBook, pos.Exiting
		return &d, nil
	}
	bids = wholeLevels(bids)

	// The probability this side pays, if one can be had. With both sides showing it is the blend.
	// With only the OTHER side showing, the mid falls back to the one price there is, which is
	// v2's rule for a missing bid (account.go: mid = ask) [CONVENTION]; it is used for exits only,
	// never for an entry.
	var pSide *float64
	if v.OK && d.HasProb {
		p := sideProb(d.P, side)
		pSide = &p
	} else if v.OK && len(bids) == 0 && !sides.Crossed {
		if other, oerr := ladderOf(sides, broker.LadderFor(broker.Buy, side), prm.Levels); oerr == nil && len(other) > 0 {
			ask := priceDollars(broker.TakerPrice(broker.Buy, other[0].Bid)) // this side's ask
			p := Blend(ask, sideProb(v.PModel, side), prm.LambdaAt(tau))
			pSide = &p
		}
	}
	staleSell, _ := priceUnits(prm.StaleCostSell)
	n := int64(pos.Contracts)
	// netUnits is what selling ALL n at b is worth to the tests, in price units (a cent is 100):
	// what the ledger would book, less the staleness cost. Whole numbers throughout.
	netUnits := func(b broker.Price) int64 { return bookedSaleCents(pos.Contracts, b)*100 - staleSell*n }
	// value: the sale is worth more than the contracts are believed to pay. The belief is a float
	// and meets the money only in this comparison.
	valueOK := func(b broker.Price) bool {
		return pSide != nil && float64(netUnits(b)) > *pSide*float64(priceScale*n)
	}
	captureOK := func(b broker.Price) bool {
		if !(prm.TakeCapture > 0) {
			return false
		}
		entry := pos.PremiumCents * 100 // the average entry price times the contracts, in price units
		covers := float64(int64(b)*n) >= float64(entry)+prm.TakeCapture*float64(priceScale*n-entry)
		return covers && netUnits(b) > pos.CostCents*100 // beats what the contracts cost, exactly
	}
	// planWants is the plan's own per-contract test with the unrounded fee. It sends nothing. It is
	// kept for one purpose: to SAY so when the plan's rule calls a sale profitable for which the
	// ledger would book nothing or less, instead of passing over that second in silence.
	planWants := func(b broker.Price) string {
		net := sellNetE10(b, staleSell)
		switch entry := pos.PremiumCents * 100; {
		case pSide != nil && dollars(net) > *pSide:
			return "value"
		case prm.TakeCapture > 0 && net > pos.CostCents*e10PerCent/n &&
			float64(int64(b)*n) >= float64(entry)+prm.TakeCapture*float64(priceScale*n-entry):
			return "capture"
		}
		return ""
	}

	var why string
	var limit broker.Price
	if len(bids) > 0 {
		best := bids[0].Bid
		var test func(broker.Price) bool
		switch { // value wins when both fire, as in v2
		case valueOK(best):
			why, test = "value", valueOK
		case captureOK(best):
			why, test = "capture", captureOK
		default:
			if plan := planWants(best); plan != "" && bookedSaleCents(pos.Contracts, best) <= 0 {
				d.BlockedBy, d.Why = BlockedExitNoProceeds, plan
				return &d, nil
			}
			if pos.Exiting == "" {
				return nil, nil
			}
			d.Why, d.ClearExit = "the exit rule no longer fires: the rest is held", string(side)
			return &d, nil
		}
		if pSide != nil { // per contract and unrounded, like an entry's: the journal's figure, not the test
			d.Edge = dollars(sellNetE10(best, staleSell)) - *pSide
		}
		if saleCanBookNothing(best) {
			d.BlockedBy, d.Why = BlockedExitNoProceeds, why
			return &d, nil
		}
		for _, lv := range bids { // the lowest displayed bid at which the rule still holds
			if !test(lv.Bid) || saleCanBookNothing(lv.Bid) {
				break
			}
			limit = lv.Bid
		}
	} else {
		if pos.Exiting == "" {
			return nil, nil
		}
		why = pos.Exiting
		test := captureOK
		if why == "value" {
			if pSide == nil {
				d.BlockedBy, d.Why = BlockedExitNoBook, why
				return &d, nil
			}
			test = valueOK
		}
		// No bid is displayed, so the limit is a low price at which the rule would hold and a sale
		// would book something. The tests rise with the bid but for the rounding of a cent or two,
		// so a binary search finds the lowest such price or one just above it; either way the rule
		// holds AT the limit, because hi only ever moves to a price that passed.
		ok := func(b broker.Price) bool { return test(b) && !saleCanBookNothing(b) }
		lo, hi := broker.Price(1), broker.Price(maxPrice)
		if !ok(hi) {
			d.Why, d.ClearExit = "the exit rule cannot fire at any price: the rest is held", string(side)
			return &d, nil
		}
		for lo < hi {
			if mid := (lo + hi) / 2; ok(mid) {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		limit = hi
	}

	d.Action, d.Why, d.Requested = "exit", why, pos.Contracts
	in := Intent{Strategy: prm.Name, Why: why, Coin: coin, Close: m.Close, Strike: m.Strike, Window: a.windowOrFresh(m.Close),
		Order: broker.Order{BucketID: a.BucketID, MarketID: m.MarketID, Ticker: m.Ticker, Action: broker.Sell, Side: side,
			Qty: pos.Contracts, Limit: limit, EvaluationID: m.EvaluationID, At: unixTime(now)}}
	in.Detail = a.detail(in, d, v)
	in.Detail["exit_tries"], in.Detail["blocked_seconds"] = pos.ExitTries, pos.BlockedSeconds
	return &d, &in
}

// AfterDecide notes what one look at the book changed that is not money. It is kept apart from
// Decide so that Decide stays pure; the runner calls it ONCE after every Decide, with that
// Decide's decisions, whatever becomes of the step's write. Two things:
//
//   - a pending exit that Decide found no longer wanted is dropped (Decision.ClearExit);
//   - a second in which an exit was wanted and NO order was sent (too late, no book that can price
//     it, or a bid at which a sale books nothing) is counted in the position's BlockedSeconds.
//     Seconds in which an order was sent and came back empty are counted by the fold. Together
//     they are the definition: seconds an exit was wanted and nothing filled.
//
// All of it is soft state, left out of the comparison of live with rebuilt memory: a restart
// simply evaluates the rule afresh.
func (e *Engine) AfterDecide(ticker string, decisions []Decision) {
	for _, d := range decisions {
		a := e.Account(d.BucketID)
		if a == nil {
			continue
		}
		if d.ClearExit != "" {
			if pos := a.Positions[posKey{ticker, d.ClearExit}]; pos != nil {
				pos.Exiting, pos.ExitTries = "", 0
			}
		}
		switch d.BlockedBy {
		case BlockedExitTooLate, BlockedExitNoBook, BlockedExitNoProceeds:
			if pos := a.Positions[posKey{ticker, d.Side}]; pos != nil {
				pos.BlockedSeconds++
			}
		}
	}
}

// ---- the fold ---------------------------------------------------------------------------------

// Booked is one recorded order as the fold needs it. Live it is made from the intent and the
// broker's report (BookedFrom); on rebuild from a trade_order row, its detail and its fills
// (BookedFromRow). Both roads end in Fold, so memory after a restart is memory before it.
type Booked struct {
	BucketID, MarketID int64
	Coin, Ticker       string
	Close, Strike      float64
	Action             broker.Action
	Side               broker.Side
	Why                string
	At                 float64 // unix seconds the order was placed
	Kelly              float64 // buys
	Window             Window  // as it stood before the order: E_w and k_max for a window not yet in memory
	Fills              []broker.Fill
	Unfilled           int
	Rejected           bool
}

// BookedFrom pairs an intent with the broker's answer.
func BookedFrom(in Intent, r broker.Report) Booked {
	o := in.Order
	return Booked{BucketID: o.BucketID, MarketID: o.MarketID, Coin: in.Coin, Ticker: o.Ticker, Close: in.Close, Strike: in.Strike,
		Action: o.Action, Side: o.Side, Why: in.Why, At: UnixSeconds(o.At), Kelly: in.Kelly, Window: in.Window,
		Fills: r.Fills, Unfilled: r.Unfilled, Rejected: r.Status == broker.Rejected}
}

// BookedFromRow reads a recorded order back: detail is trade_order.detail decoded from JSON, and
// fills its fill rows, oldest first. It refuses a detail it cannot read; the rebuild then stays
// suspended, which is the safe side.
func BookedFromRow(bucketID, marketID int64, action, side string, at float64, detail map[string]any, fills []broker.Fill) (Booked, error) {
	num := func(m map[string]any, key string) (float64, bool) { f, ok := m[key].(float64); return f, ok }
	b := Booked{BucketID: bucketID, MarketID: marketID, Action: broker.Action(action), Side: broker.Side(side), At: at, Fills: fills}
	if version, _ := num(detail, "v"); version != 3 {
		return b, fmt.Errorf("engine: order detail is not version 3")
	}
	var ok [3]bool
	b.Coin, ok[0] = detail["coin"].(string)
	b.Ticker, ok[1] = detail["ticker"].(string)
	b.Why, ok[2] = detail["why"].(string)
	closeAt, okClose := num(detail, "close")
	strike, okStrike := num(detail, "strike")
	w, okWindow := detail["window"].(map[string]any)
	if !ok[0] || !ok[1] || !ok[2] || !okClose || !okStrike || !okWindow {
		return b, fmt.Errorf("engine: order detail lacks coin, ticker, why, close, strike or window")
	}
	b.Close, b.Strike = closeAt, strike
	equity, okEquity := num(w, "equity_cents")
	kMax, okKMax := num(w, "k_max")
	if !okEquity || !okKMax {
		return b, fmt.Errorf("engine: order detail's window lacks equity_cents or k_max")
	}
	b.Window = Window{Close: windowKey(closeAt), EquityCents: int64(math.Round(equity)), KMax: kMax}
	if b.Action == broker.Buy {
		k, okKelly := num(detail, "kelly")
		if !okKelly {
			return b, fmt.Errorf("engine: a buy's detail lacks kelly")
		}
		b.Kelly = k
	}
	if unfilled, has := num(detail, "unfilled"); has {
		b.Unfilled = int(unfilled)
	}
	return b, nil
}

// Event is something that happened to an account's memory, for the runner's log and journal.
type Event struct {
	Kind      string // bought | sold | entry_unfilled | exit_unfilled | settled | exhausted | inconsistent
	BucketID  int64
	Strategy  string
	Coin      string
	Ticker    string
	Side      string
	Why       string
	Contracts int   // filled, or settled
	Left      int   // contracts still held afterwards
	CashCents int64 // the bucket's cash effect
	// BasisCents is the cost basis added (bought) or released (sold, settled).
	BasisCents     int64
	Won            bool // settled only
	ExitTries      int
	BlockedSeconds int // settled: seconds an exit was wanted and nothing filled, before it rode to the close
	Note           string
}

// Apply folds a step's answers into memory. The runner calls it ONLY after the ledger write of
// exactly these orders has committed, so memory is never ahead of the ledger. intents and reports
// are the step's, in the same order.
//
// (The plan writes Apply(reports). A report does not carry the Kelly fraction or the window's
// frozen equity, which the fold needs and the rebuild reads from the stored detail; so the
// intents come too. Decide keeps no memory of what it formed, because it changes nothing.)
func (e *Engine) Apply(intents []Intent, reports []broker.Report) []Event {
	var events []Event
	if len(intents) != len(reports) {
		return []Event{{Kind: "inconsistent", Note: fmt.Sprintf("%d intents and %d reports", len(intents), len(reports))}}
	}
	touched := map[int64]bool{}
	for i, in := range intents {
		if reports[i].Order.ClientID != in.Order.ClientID {
			events = append(events, Event{Kind: "inconsistent", BucketID: in.Order.BucketID,
				Note: "report " + reports[i].Order.ClientID + " does not answer intent " + in.Order.ClientID})
			continue
		}
		events = append(events, e.Fold(BookedFrom(in, reports[i]))...)
		touched[in.Order.BucketID] = true
	}
	// Live only: a sale can leave an account with nothing open and less than the floor. The
	// rebuild derives this itself, last, once every fill is folded (Account.RanOut).
	for _, a := range e.Accounts {
		if touched[a.BucketID] {
			events = append(events, a.noteExhausted()...)
		}
	}
	return events
}

func (a *Account) noteExhausted() []Event {
	if a.Exhausted || !a.RanOut() {
		return nil
	}
	a.Exhausted = true
	return []Event{{Kind: "exhausted", BucketID: a.BucketID, Strategy: a.Params.Name, CashCents: a.CashCents}}
}

// Fold is the single place memory changes for an order, live (through Apply) and on rebuild.
//
// What each answer does (plan 4.4):
//
//	buy, anything filled:  the position and the window's used grow; ONE bet toward max_bets,
//	                       however many levels it took and whether or not it was cut short; the
//	                       gap starts; the window's best Kelly fraction is raised.
//	buy, nothing filled:   nothing happened. No bet counted, no gap started, and the window is
//	                       neither created nor its Kelly raised: the rebuild reads only orders
//	                       with fills, so anything else would make memory differ after a restart.
//	sell, all filled:      the position closes.
//	sell, part filled:     the position shrinks and the exit stays wanted.
//	sell, nothing filled:  the exit stays wanted; BlockedSeconds grows, and ExitTries too unless
//	                       the order was rejected before it reached a book. The position rides.
func (e *Engine) Fold(b Booked) []Event { return e.fold(b, true) }

// FoldRecorded is Fold for the rebuild. The one difference: cash is left alone, because a rebuilt
// account is made with the LEDGER's balance, which already has every recorded fill in it. There
// is no saved cash for the fold to disagree with.
func (e *Engine) FoldRecorded(b Booked) []Event { return e.fold(b, false) }

func (e *Engine) fold(b Booked, moveCash bool) []Event {
	a := e.Account(b.BucketID)
	ev := Event{BucketID: b.BucketID, Coin: b.Coin, Ticker: b.Ticker, Side: string(b.Side), Why: b.Why}
	if a == nil {
		ev.Kind, ev.Note = "inconsistent", "an order for a bucket the engine does not hold"
		return []Event{ev}
	}
	ev.Strategy = a.Params.Name
	key := posKey{b.Ticker, string(b.Side)}
	pos := a.Positions[key]
	filled := 0
	for _, f := range b.Fills {
		filled += f.Qty
	}

	if filled == 0 {
		switch {
		case b.Action == broker.Buy:
			ev.Kind = "entry_unfilled"
		case pos == nil:
			ev.Kind, ev.Note = "inconsistent", "an exit answered for a position that is not held"
		default:
			if b.Why == "value" || b.Why == "capture" {
				pos.Exiting = b.Why
			}
			pos.BlockedSeconds++
			if !b.Rejected {
				pos.ExitTries++
			}
			ev.Kind, ev.Left, ev.ExitTries, ev.BlockedSeconds = "exit_unfilled", pos.Contracts, pos.ExitTries, pos.BlockedSeconds
		}
		return []Event{ev}
	}

	if b.Action == broker.Sell && pos == nil {
		ev.Kind, ev.Note = "inconsistent", "a sale of a position that is not held"
		return []Event{ev}
	}
	w := a.Windows[windowKey(b.Close)]
	if w == nil {
		// E_w is frozen here, from what the order itself recorded. A detail without it leaves the
		// equity at zero, so the window's budget is zero: the side that places no more bets.
		w = &Window{Close: windowKey(b.Close), EquityCents: b.Window.EquityCents, KMax: b.Window.KMax}
		a.Windows[w.Close] = w
	}
	if pos == nil {
		pos = &Position{Coin: b.Coin, Ticker: b.Ticker, Side: string(b.Side), MarketID: b.MarketID, Close: b.Close, Strike: b.Strike, FirstAt: b.At}
		a.Positions[key] = pos
	}
	for _, f := range b.Fills {
		cash, basis, err := pos.Apply(b.Action, f, w)
		if err != nil {
			bad := ev
			bad.Kind, bad.Note = "inconsistent", err.Error()
			return []Event{bad}
		}
		if moveCash {
			a.CashCents += cash
		}
		ev.CashCents += cash
		ev.BasisCents += basis
	}
	ev.Contracts, ev.Left = filled, pos.Contracts
	if b.Action == broker.Buy {
		ev.Kind = "bought"
		pos.Entries++
		pos.LastBuyAt = b.At
		a.Bets++
		w.KMax = math.Max(w.KMax, b.Kelly)
		return []Event{ev}
	}
	ev.Kind = "sold"
	if pos.Contracts == 0 {
		delete(a.Positions, key)
		return []Event{ev}
	}
	pos.ExitTries = 0
	if b.Why == "value" || b.Why == "capture" {
		pos.Exiting = b.Why // the rest is offered again next second if the rule still fires
	}
	return []Event{ev}
}

// Detail is trade_order.detail for one answered order (plan section 3): what the intent knew,
// plus what filled, plus the broker's evidence, "seen": per recorded level [bid e4, displayed,
// held, taken], so that a fill can be checked afterwards against the book it was taken from
// (plan 7.5's fill_checks reads it). It changes nothing. cost and payout are in dollars because the analysis
// layer reads them so and turns them back with round(x * 100), exact for the two-decimal image
// of whole cents; the cents themselves are stored beside them and are what this package reads.
//
//	buy:  cost = what left the bucket, fee inside
//	sell: cost = the cost basis of the contracts SOLD; payout = what reached the bucket, fee inside
func (e *Engine) Detail(in Intent, r broker.Report) map[string]any {
	out := make(map[string]any, len(in.Detail)+10)
	for k, v := range in.Detail {
		out[k] = v
	}
	filled := 0
	var cash, basis int64
	var scratch Position // the basis a sale releases, worked on a copy
	if a := e.Account(in.Order.BucketID); a != nil {
		if pos := a.Positions[posKey{in.Order.Ticker, string(in.Order.Side)}]; pos != nil {
			scratch = *pos
		}
	}
	for _, f := range r.Fills {
		filled += f.Qty
		c, b, err := scratch.Apply(in.Order.Action, f, nil)
		if err != nil {
			out["inconsistent"] = err.Error()
			c, b = broker.BucketCents(in.Order.Action, f), 0
		}
		cash, basis = cash+c, basis+b
	}
	out["model"], out["status"], out["filled"], out["unfilled"], out["reason"] = r.Model, string(r.Status), filled, r.Unfilled, r.Reason
	out["cost_cents"], out["cost"] = basis, float64(basis)/100
	if in.Order.Action == broker.Sell {
		out["payout_cents"], out["payout"] = cash, float64(cash)/100
	}
	seen := make([][4]int64, 0, len(r.Seen)) // never nil, so it is stored as [] and not null
	for _, lv := range r.Seen {
		seen = append(seen, [4]int64{int64(lv.Bid), int64(lv.Displayed), int64(lv.Held), int64(lv.Taken)})
	}
	out["seen"] = seen
	return out
}

// ---- settlement -------------------------------------------------------------------------------

// SettleRow is what one account held on one side of a settled market, and what it is paid: a
// dollar a contract on the winning side, nothing on the other.
type SettleRow struct {
	BucketID, MarketID int64
	Ticker, Side       string
	Qty                int
	PayoutCents        int64
	CostCents          int64
	Won                bool
}

// SettleRows is what to record for a market's result. It changes nothing, and it covers EVERY
// account, settle-only and exhausted ones too: holding and settling do not depend on being
// allowed to order. A position sold out earlier is not in memory and yields no row.
func (e *Engine) SettleRows(ticker, result string) []SettleRow {
	if result != "yes" && result != "no" {
		return nil
	}
	var rows []SettleRow
	for _, a := range e.Accounts {
		for _, side := range []string{"yes", "no"} {
			pos := a.Positions[posKey{ticker, side}]
			if pos == nil || pos.Contracts < 1 {
				continue
			}
			row := SettleRow{BucketID: a.BucketID, MarketID: pos.MarketID, Ticker: ticker, Side: side, Qty: pos.Contracts,
				CostCents: pos.CostCents, Won: side == result}
			if row.Won {
				row.PayoutCents = int64(pos.Contracts) * 100
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// ApplySettlement pays out and closes the positions SettleRows listed. The runner calls it only
// after RecordSettlements has committed those very rows.
func (e *Engine) ApplySettlement(ticker, result string) []Event {
	var events []Event
	for _, row := range e.SettleRows(ticker, result) {
		a := e.Account(row.BucketID)
		key := posKey{ticker, row.Side}
		pos := a.Positions[key]
		a.CashCents += row.PayoutCents
		if row.PayoutCents < row.CostCents { // the martingale's streak: settled outcomes only
			a.LossStreak++
		} else {
			a.LossStreak = 0
		}
		if w := a.Windows[windowKey(pos.Close)]; w != nil {
			w.OpenCents -= row.CostCents
			if short := row.CostCents - row.PayoutCents; short > 0 {
				w.LostCents += short
			}
		}
		events = append(events, Event{Kind: "settled", BucketID: a.BucketID, Strategy: a.Params.Name, Coin: pos.Coin, Ticker: ticker,
			Side: row.Side, Why: "held to settlement", Contracts: row.Qty, CashCents: row.PayoutCents, BasisCents: row.CostCents,
			Won: row.Won, ExitTries: pos.ExitTries, BlockedSeconds: pos.BlockedSeconds})
		closeAt := pos.Close
		delete(a.Positions, key)
		a.prune(closeAt)
		events = append(events, a.noteExhausted()...)
	}
	return events
}

// Prune forgets every window that closed at or before now and holds nothing. A window whose
// positions were all sold early never sees a settlement, so the runner calls this too.
func (e *Engine) Prune(now float64) {
	for _, a := range e.Accounts {
		a.prune(now)
	}
}

func (a *Account) prune(upTo float64) {
	open := map[int64]bool{}
	for _, p := range a.Positions {
		open[windowKey(p.Close)] = true
	}
	for key := range a.Windows {
		if float64(key) <= upTo && !open[key] {
			delete(a.Windows, key)
		}
	}
}
