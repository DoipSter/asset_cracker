package kalshi15m2

import (
	"fmt"
	"math"
	"time"

	"github.com/doipster/asset_cracker/service/internal/pyfloat"
)

// Lot is a bet, from the moment it is placed. JSON keys are the ones the Python app writes.
type Lot struct {
	ID         int     `json:"id"`
	T          float64 `json:"t"`
	Time       string  `json:"time"`
	Ticker     string  `json:"ticker"`
	Coin       string  `json:"coin"`
	Side       string  `json:"side"` // "UP" or "DOWN"
	Contracts  int     `json:"contracts"`
	Price      float64 `json:"price"`
	Multiplier float64 `json:"multiplier"`
	Fee        float64 `json:"fee"`
	Cost       float64 `json:"cost"`
	Strike     float64 `json:"strike"`
	Close      float64 `json:"close"`
	BTCPrice   float64 `json:"btc_price"` // the key says btc but holds whichever coin
	ModelProb  float64 `json:"model_prob"`
	Edge       float64 `json:"edge"`
	Status     string  `json:"status"` // open, sold, won, lost

	MirrorOf   *int     `json:"mirror_of,omitempty"` // a twin's lot: the original's lot id
	Payout     *float64 `json:"payout,omitempty"`
	PnL        *float64 `json:"pnl,omitempty"`
	ExitT      *float64 `json:"exit_t,omitempty"`
	ExitPrice  *float64 `json:"exit_price,omitempty"`
	ExitBTC    *float64 `json:"exit_btc,omitempty"`
	Why        string   `json:"why,omitempty"` // an early sale's reason: value, capture, stop, mirror
	ExitTau    *int     `json:"exit_tau,omitempty"`
	Result     string   `json:"result,omitempty"`
	FinalValue any      `json:"final_value,omitempty"`
	Graded     bool     `json:"graded,omitempty"`  // an early sale already compared with holding
	GaveUp     *float64 `json:"gave_up,omitempty"` // what holding would have paid, less what selling did
}

// Option is one side of the market as a strategy sees it.
type Option struct {
	Side                           string
	Ask, Cost, P, Size, Edge, Mult float64
}

// Signal says whether the strategy would bet right now, and if not, why not.
type Signal struct {
	Side             string
	Conf, Edge, Need float64
	Bet              bool
	Why              string
}

// View is what a strategy thinks about one coin right now.
type View struct {
	PUp, PModel, Mid, Tau float64
	Best                  Option
	Signal                Signal
}

// TailContext is what the Lottery strategy needs from the coin's state.
type TailContext struct{ PTail, Spike *float64 }

// Event is something that happened to a bet, or to an account.
type Event struct {
	Kind     string // bet, sold, settled, bankrupt
	Strategy string
	Lot      Lot  // a copy taken at that moment (not for bankrupt)
	Won      bool // settled only
	SellFee  float64

	// bankrupt only
	DiedWith float64
	Rounds   int
	Life     int
	Retired  bool
}

// Account is one strategy's paper account: one balance, shared across every coin.
type Account struct {
	Params       Params
	Cash         float64
	Log          []*Lot
	Bets         int
	Wins         int
	Losses       int
	RealizedPnL  float64
	NextID       int
	Views        map[string]*View // by coin
	Bankruptcies int              // times this strategy has run out and been staked again
	Retired      bool             // a twin that ran out: it places no more bets
}

// NewAccount starts an account at the starting balance.
func NewAccount(p Params) *Account {
	return &Account{Params: p, Cash: StartBalance, NextID: 1, Views: map[string]*View{}}
}

// Broke reports an account with no money and nothing outstanding. Open lots are excluded on
// purpose: while a bet is live the strategy still has something that might pay.
func (a *Account) Broke() bool { return !a.Retired && a.Cash < BankruptAt && len(a.openLots("")) == 0 }

// Revive stakes a strategy again from scratch. Its history is preserved elsewhere first.
func (a *Account) Revive() {
	a.Cash, a.Retired, a.Log = StartBalance, false, nil
	a.Bets, a.Wins, a.Losses, a.RealizedPnL, a.NextID = 0, 0, 0, 0, 1
	a.Views = map[string]*View{}
	a.Bankruptcies++
}

func (a *Account) openLots(ticker string) []*Lot {
	var out []*Lot
	for _, lot := range a.Log {
		if lot.Status == "open" && (ticker == "" || lot.Ticker == ticker) {
			out = append(out, lot)
		}
	}
	return out
}

// Committed is what every open bet cost, across all coins.
func (a *Account) Committed() float64 {
	var costs []float64
	for _, lot := range a.openLots("") {
		costs = append(costs, lot.Cost)
	}
	return pyfloat.Sum(costs)
}

// Equity is cash plus what open bets could be sold for right now, after the selling fee (or
// their cost, if there is no bid to price them by). markets is keyed by coin.
func (a *Account) Equity(markets map[string]*Market) float64 {
	total := a.Cash
	for _, lot := range a.openLots("") {
		bid := 0.0
		if m := markets[lot.Coin]; m != nil && m.Ticker == lot.Ticker {
			bid = m.YesBid
			if lot.Side != "UP" {
				bid = m.NoBid
			}
		}
		if bid > 0 {
			total += float64(float64(lot.Contracts)*bid) - KalshiFee(lot.Contracts, bid)
		} else {
			total += lot.Cost
		}
	}
	return total
}

// Participated is how many 15-minute rounds this strategy has bet in. Three coins in the same
// quarter hour is one round: they all settle together.
func (a *Account) Participated() int {
	seen := map[float64]bool{}
	for _, lot := range a.Log {
		seen[lot.Close] = true
	}
	return len(seen)
}

func quote(side string, ask, p, size, slip, maxCost float64) Option {
	c := math.Min(maxCost, ask+slip)
	return Option{Side: side, Ask: ask, Cost: c, P: p, Size: size, Edge: p - c - float64(FeeRate*c*(1-c)), Mult: p / ask}
}

func sideQuotes(m *Market, side string) (bid float64) {
	if side == "UP" {
		return m.YesBid
	}
	return m.NoBid
}

// Step is one original strategy's look at one coin's open round: maybe sell, maybe bet.
func (a *Account) Step(m *Market, price, now, pModel float64, paused bool, ctx TailContext) []Event {
	prm := a.Params
	tau := m.Close - now
	ya, na, yb := m.YesAsk, m.NoAsk, m.YesBid
	if price == 0 || ya <= 0 || na <= 0 {
		a.Views[m.Coin] = nil
		return nil
	}
	if prm.Lottery {
		return a.lottery(m, price, now, pModel, paused, ctx)
	}
	mid := ya
	if yb > 0 {
		mid = (yb + ya) / 2
	}
	pUp := mid + float64(prm.Shrink*(pModel-mid)) // blend with what the market believes
	options := []Option{quote("UP", ya, pUp, m.YesAskSize, Slippage, 0.99), quote("DOWN", na, 1-pUp, m.NoAskSize, Slippage, 0.99)}

	var events []Event
	if !paused && tau >= MinTau && prm.Exit == "ev" {
		events = append(events, a.exits(m, now, pUp, price)...)
	}
	// Kalshi keeps one net position per market, so once a strategy holds a side in this round it
	// may only add to that side, or sell it.
	here := a.openLots(m.Ticker)
	held := map[string]bool{}
	for _, lot := range here {
		held[lot.Side] = true
	}
	var best *Option
	for i := range options { // the first of equals wins, as Python's max()
		if o := &options[i]; (len(held) == 0 || held[o.Side]) && (best == nil || o.Edge > best.Edge) {
			best = o
		}
	}
	a.Views[m.Coin] = &View{PUp: pUp, PModel: pModel, Mid: mid, Tau: tau, Best: *best, Signal: a.signal(here, now, *best, tau, paused)}
	if paused || tau < MinTau {
		return events
	}

	c := best.Cost
	lastBet := math.Inf(-1)
	var costs []float64
	for _, lot := range here {
		lastBet = math.Max(lastBet, lot.T)
		costs = append(costs, lot.Cost)
	}
	if !(prm.TauMin <= tau && tau <= prm.TauMax) || best.Edge < prm.MinEdge || !(prm.BandMin <= c && c <= prm.BandMax) ||
		len(here) >= prm.MaxBets || (len(here) > 0 && now-lastBet < prm.MinGap) {
		return events
	}
	committed := pyfloat.Sum(costs)
	room := float64(WindowCap*(a.Cash+committed)) - committed
	// every coin draws on the same balance, so the overall exposure is capped in dollars, as a
	// share of the STARTING balance: a winning run must not raise the ceiling on its own stakes
	room = math.Min(room, TotalCap*StartBalance-a.Committed())
	unit := c + float64(FeeRate*c*(1-c))
	kelly := (best.P - unit) / (1 - unit)
	stake := math.Min(math.Min(Kelly*kelly, prm.MaxStake)*a.Cash, room)
	if n := affordable(min(int(pyfloat.FloorDiv(stake, unit)), int(best.Size)), c, a.Cash); n >= 1 {
		events = append(events, a.bet(m, *best, n, price, now))
	}
	return events
}

// affordable trims a contract count until its cost with the fee fits the budget.
func affordable(n int, c, budget float64) int {
	for n > 0 && float64(float64(n)*c)+KalshiFee(n, c) > budget {
		n--
	}
	return n
}

func (a *Account) lottery(m *Market, price, now, pModel float64, paused bool, ctx TailContext) []Event {
	tau := m.Close - now
	ya, na, yb := m.YesAsk, m.NoAsk, m.YesBid
	pUp := pModel
	if ctx.PTail != nil {
		pUp = *ctx.PTail
	}
	up := quote("UP", ya, pUp, m.YesAskSize, LotterySlippage, 0.999)
	down := quote("DOWN", na, 1-pUp, m.NoAskSize, LotterySlippage, 0.999)
	cheap := up // the longshot; the first of equals, as Python's min()
	if down.Ask < up.Ask {
		cheap = down
	}
	already := false
	for _, lot := range a.Log {
		if lot.Ticker == m.Ticker {
			already = true
		}
	}
	why := ""
	switch {
	case paused:
		why = "paused"
	case ctx.PTail == nil:
		why = "collecting volatility history"
	case tau < LotteryMinTau:
		why = fmt.Sprintf("only bets with %d+ min left", int(LotteryMinTau)/60)
	case cheap.Ask > LotteryMaxAsk:
		why = fmt.Sprintf("no cheap side (needs %.0f¢ or less)", LotteryMaxAsk*100)
	case already:
		why = "already bet this round"
	case *ctx.Spike < LotteryMinSpike:
		why = fmt.Sprintf("volatility calm (%.1fx, needs %.1fx)", *ctx.Spike, LotteryMinSpike)
	case cheap.Mult < LotteryMinMult:
		why = fmt.Sprintf("only %.1fx likelier (needs %.0fx)", cheap.Mult, LotteryMinMult)
	}
	mid := ya
	if yb > 0 {
		mid = (yb + ya) / 2
	}
	sig := Signal{Side: cheap.Side, Conf: cheap.P * 100, Edge: cheap.Edge * 100, Bet: why == "", Why: why}
	if why == "" {
		sig.Why = "will bet"
	}
	a.Views[m.Coin] = &View{PUp: pUp, PModel: pModel, Mid: mid, Tau: tau, Best: cheap, Signal: sig}
	if why != "" || tau < MinTau {
		return nil
	}
	unit := cheap.Cost + float64(FeeRate*cheap.Cost*(1-cheap.Cost))
	if n := affordable(min(int(pyfloat.FloorDiv(LotteryStake*a.Cash, unit)), int(cheap.Size)), cheap.Cost, a.Cash); n >= 1 {
		return []Event{a.bet(m, cheap, n, price, now)}
	}
	return nil
}

// signal mirrors the entry checks for the display. Like the Python, it uses the default gap
// between bets even for a strategy that overrides it; only the entry check uses the override.
func (a *Account) signal(here []*Lot, now float64, best Option, tau float64, paused bool) Signal {
	prm := a.Params
	s := Signal{Side: best.Side, Conf: best.P * 100, Edge: best.Edge * 100, Need: prm.MinEdge * 100}
	lastBet := math.Inf(-1)
	for _, lot := range here {
		lastBet = math.Max(lastBet, lot.T)
	}
	switch {
	case paused:
		s.Why = "paused"
	case tau < MinTau || tau < prm.TauMin:
		s.Why = "too late this round"
	case tau > prm.TauMax:
		s.Why = fmt.Sprintf("waits for last %d:%02d", int(prm.TauMax)/60, int(prm.TauMax)%60)
	case len(here) >= prm.MaxBets:
		s.Why = "max bets this round"
	case len(here) > 0 && now-lastBet < MinGap:
		s.Why = "cooling down"
	case !(prm.BandMin <= best.Cost && best.Cost <= prm.BandMax):
		s.Why = fmt.Sprintf("%.0f¢ outside its range", best.Cost*100)
	case best.Edge < prm.MinEdge:
		s.Why = fmt.Sprintf("edge %+.1f¢ of %.0f¢ needed", best.Edge*100, prm.MinEdge*100)
	default:
		s.Bet, s.Why = true, "will bet"
	}
	return s
}

// exits sells early for either of two reasons: the market is paying more than the bet is
// thought to be worth, or the position is up enough to bank. A third, cutting a loser, exists
// and ships switched off: measured, every setting was worse.
func (a *Account) exits(m *Market, now, pUp, price float64) []Event {
	prm := a.Params
	var events []Event
	for _, lot := range a.openLots(m.Ticker) {
		bid := sideQuotes(m, lot.Side)
		if bid <= 0 || now-lot.T < prm.MinHold {
			continue
		}
		pSide := pUp
		if lot.Side != "UP" {
			pSide = 1 - pUp
		}
		sellC := math.Max(0.01, bid-Slippage)
		n := lot.Contracts
		fee := KalshiFee(n, sellC)
		proceeds := round2(float64(float64(n)*sellC) - fee)
		overpriced := sellC-float64(FeeRate*sellC*(1-sellC)) > pSide+ExitMargin
		// Banking is measured against the upside, not as a flat percentage: a 7c contract pays
		// 100c, so "up 12%" is under a cent. Sell once the bid has covered this much of the way
		// from entry to a dollar, and only if proceeds genuinely beat cost.
		banking := prm.TakeCapture != nil && proceeds > lot.Cost && sellC >= lot.Price+float64(*prm.TakeCapture*(1-lot.Price))
		cutting := prm.StopLoss != nil && proceeds <= lot.Cost*(1-*prm.StopLoss)
		if prm.StopTau != nil && m.Close-now <= *prm.StopTau {
			cutting = cutting || proceeds < lot.Cost
		}
		if !(overpriced || banking || cutting) {
			continue
		}
		why := "value"
		if cutting && !(banking || overpriced) {
			why = "stop"
		} else if banking && !overpriced {
			why = "capture"
		}
		a.sell(lot, m, sellC, proceeds, why, now, price)
		events = append(events, Event{Kind: "sold", Strategy: prm.Name, Lot: *lot, SellFee: fee})
	}
	return events
}

func (a *Account) sell(lot *Lot, m *Market, sellC, proceeds float64, why string, now, price float64) {
	exitPrice, exitBTC, exitTau := round2(sellC), price, pyRoundInt(m.Close-now)
	lot.ExitPrice, lot.ExitBTC, lot.Why, lot.ExitTau = &exitPrice, &exitBTC, why, &exitTau
	a.close(lot, "sold", proceeds, now)
}

func (a *Account) bet(m *Market, o Option, n int, price, now float64) Event {
	c := o.Cost
	fee := KalshiFee(n, c)
	cost := round2(float64(float64(n)*c) + fee)
	a.Cash -= cost
	lot := &Lot{ID: a.NextID, T: now, Time: ISOSeconds(now), Ticker: m.Ticker, Coin: m.Coin, Side: o.Side, Contracts: n,
		Price: round2(c), Multiplier: round2(1 / c), Fee: fee, Cost: cost, Strike: m.Strike, Close: m.Close, BTCPrice: price,
		ModelProb: pyfloat.Round(o.P, 3), Edge: pyfloat.Round(o.Edge, 3), Status: "open"}
	a.NextID++
	a.Bets++
	a.Log = append(a.Log, lot)
	return Event{Kind: "bet", Strategy: a.Params.Name, Lot: *lot}
}

func (a *Account) close(lot *Lot, status string, payout, now float64) {
	pnl := round2(payout - lot.Cost)
	lot.Status, lot.Payout, lot.PnL, lot.ExitT = status, &payout, &pnl, &now
	a.Cash += payout
	a.RealizedPnL += pnl
	if pnl > 0 {
		a.Wins++
	} else {
		a.Losses++
	}
}

// Mirror is a twin taking the other side of whatever its original just did, for the same MONEY.
// Matching contracts instead would have the twin commit far more capital, because the two
// sides of a market are different prices, straight through the cap its original had respected.
func (a *Account) Mirror(events []Event, m *Market, now, price float64) []Event {
	if a.Retired {
		return nil // out of money and not staked again; its original carries on alone
	}
	var out []Event
	for _, e := range events {
		switch e.Kind {
		case "bet":
			side := "UP"
			ask, size := m.YesAsk, m.YesAskSize
			if e.Lot.Side == "UP" {
				side, ask, size = "DOWN", m.NoAsk, m.NoAskSize
			}
			if ask <= 0 {
				continue
			}
			c := math.Min(0.99, ask+Slippage)
			// The caps are not re-checked: matching the stake means the twin commits what its
			// original committed, and that already passed them. Cash still binds.
			budget := math.Min(e.Lot.Cost, a.Cash)
			n := affordable(min(int(pyfloat.FloorDiv(budget, c)), int(size)), c, budget)
			if n < 1 {
				continue
			}
			pSide := 1 - e.Lot.ModelProb
			ev := a.bet(m, Option{Side: side, Cost: c, Size: size, P: pSide, Edge: pSide - c - float64(FeeRate*c*(1-c))}, n, price, now)
			id := e.Lot.ID
			a.Log[len(a.Log)-1].MirrorOf, ev.Lot.MirrorOf = &id, &id
			out = append(out, ev)
		case "sold": // the original closed early, so the twin closes what it opened against it
			for _, lot := range a.openLots(m.Ticker) {
				if lot.MirrorOf == nil || *lot.MirrorOf != e.Lot.ID {
					continue
				}
				bid := sideQuotes(m, lot.Side)
				if bid <= 0 {
					continue // nothing to sell into; it will settle instead
				}
				sellC := math.Max(0.01, bid-Slippage)
				fee := KalshiFee(lot.Contracts, sellC)
				a.sell(lot, m, sellC, round2(float64(float64(lot.Contracts)*sellC)-fee), "mirror", now, price)
				out = append(out, Event{Kind: "sold", Strategy: a.Params.Name, Lot: *lot, SellFee: fee})
			}
		}
	}
	return out
}

// OnSettled pays out or writes off every open bet in a round that has settled.
func (a *Account) OnSettled(ticker, result string, finalValue any, now float64, price *float64) []Event {
	var events []Event
	for _, lot := range a.openLots(ticker) {
		won := (result == "yes") == (lot.Side == "UP")
		lot.Result, lot.FinalValue, lot.ExitBTC = result, finalValue, price
		payout, status := 0.0, "lost"
		if won {
			payout, status = float64(lot.Contracts), "won"
		}
		a.close(lot, status, payout, now)
		events = append(events, Event{Kind: "settled", Strategy: a.Params.Name, Lot: *lot, Won: won})
	}
	return events
}

// ISOSeconds formats a unix time as local YYYY-MM-DDTHH:MM:SS, as Python's
// datetime.fromtimestamp(ts).isoformat(timespec="seconds").
func ISOSeconds(ts float64) string {
	return time.UnixMicro(int64(math.RoundToEven(ts * 1e6))).Local().Format("2006-01-02T15:04:05")
}
