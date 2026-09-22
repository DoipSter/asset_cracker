package kalshi15m

import (
	"fmt"
	"math"

	"github.com/doipster/asset_cracker/service/internal/pyfloat"
)

// Lot is a bet, from the moment it is placed. JSON keys are the ones the Python app writes.
type Lot struct {
	ID         int     `json:"id"`
	T          float64 `json:"t"`
	Time       string  `json:"time"`
	Ticker     string  `json:"ticker"`
	Side       string  `json:"side"` // "UP" or "DOWN"
	Contracts  int     `json:"contracts"`
	Price      float64 `json:"price"`
	Multiplier float64 `json:"multiplier"`
	Fee        float64 `json:"fee"`
	Cost       float64 `json:"cost"`
	Strike     float64 `json:"strike"`
	Close      float64 `json:"close"`
	BTCPrice   float64 `json:"btc_price"`
	ModelProb  float64 `json:"model_prob"`
	Edge       float64 `json:"edge"`
	Status     string  `json:"status"` // open, sold, won, lost

	Payout     *float64 `json:"payout,omitempty"`
	PnL        *float64 `json:"pnl,omitempty"`
	ExitT      *float64 `json:"exit_t,omitempty"`
	ExitPrice  *float64 `json:"exit_price,omitempty"`
	ExitBTC    *float64 `json:"exit_btc,omitempty"`
	Result     string   `json:"result,omitempty"`
	FinalValue any      `json:"final_value,omitempty"` // as Kalshi sent it: often text like "81094.00"
}

// Option is one side of the market as a strategy sees it: what it costs and what it is worth.
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

// View is what the strategy thinks right now. It is journaled whether or not anything is done.
type View struct {
	PUp, PModel, Mid, Tau float64
	Best                  Option
	Signal                Signal
}

// TailContext is what the Lottery strategy needs from the trader. Nil until there is history.
type TailContext struct{ PTail, Spike *float64 }

// Exit is a decision to sell a lot early; Entry is a decision to buy.
type Exit struct {
	LotID           int
	SellC, Proceeds float64
}
type Entry struct {
	Option    Option
	N         int
	Fee, Cost float64
}

// Decision is everything one strategy concluded from one look at the market. Deciding changes
// nothing: the account applies it afterwards.
type Decision struct {
	View  *View
	Exits []Exit
	Entry *Entry
}

// AccountState is what a strategy may know about its own account when deciding.
type AccountState struct {
	Cash float64
	Lots []*Lot // every bet ever placed, oldest first
}

func quote(side string, ask, p, size, slip, maxCost float64) Option {
	c := math.Min(maxCost, ask+slip)
	return Option{Side: side, Ask: ask, Cost: c, P: p, Size: size, Edge: p - c - float64(FeeRate*c*(1-c)), Mult: p / ask}
}

// Decide is one strategy's look at the open round: maybe sell, maybe bet. It is a pure function
// of its arguments, which is what lets a recording be replayed.
func (prm Params) Decide(m Market, price, now, pModel float64, paused bool, ctx TailContext, st AccountState) Decision {
	tau := m.Close - now
	ya, na, yb := m.YesAsk, m.NoAsk, m.YesBid
	if price == 0 || ya <= 0 || na <= 0 {
		return Decision{}
	}
	if prm.Lottery {
		return prm.decideLottery(m, price, now, pModel, paused, ctx, st)
	}

	mid := ya
	if yb > 0 {
		mid = (yb + ya) / 2
	}
	pUp := mid + float64(prm.Shrink*(pModel-mid)) // blend with what the market believes
	options := []Option{
		quote("UP", ya, pUp, m.YesAskSize, Slippage, 0.99),
		quote("DOWN", na, 1-pUp, m.NoAskSize, Slippage, 0.99),
	}

	var d Decision
	cash := st.Cash
	sold := map[int]bool{}
	if !paused && tau >= MinTau && prm.Exit == "ev" {
		// Sell early when the market's bid beats what we think the bet is worth.
		for _, lot := range st.Lots {
			if lot.Status != "open" || lot.Ticker != m.Ticker {
				continue
			}
			bid, pSide := m.YesBid, pUp
			if lot.Side != "UP" {
				bid, pSide = m.NoBid, 1-pUp
			}
			if bid <= 0 || now-lot.T < 15 {
				continue
			}
			sellC := math.Max(0.01, bid-Slippage)
			if sellC-float64(FeeRate*sellC*(1-sellC)) > pSide+ExitMargin {
				n := lot.Contracts
				proceeds := round2(float64(float64(n)*sellC) - KalshiFee(n, sellC))
				d.Exits = append(d.Exits, Exit{LotID: lot.ID, SellC: sellC, Proceeds: proceeds})
				sold[lot.ID] = true
				cash += proceeds
			}
		}
	}

	// Kalshi keeps one net position per market: you cannot hold UP and DOWN at once. Once a
	// strategy holds a side in this round it may only add to that side, or sell it.
	var here []*Lot
	held := map[string]bool{}
	for _, lot := range st.Lots {
		if lot.Status == "open" && lot.Ticker == m.Ticker && !sold[lot.ID] {
			here = append(here, lot)
			held[lot.Side] = true
		}
	}
	var best *Option
	for i := range options { // the first of equals wins, as Python's max()
		o := &options[i]
		if (len(held) == 0 || held[o.Side]) && (best == nil || o.Edge > best.Edge) {
			best = o
		}
	}
	d.View = &View{PUp: pUp, PModel: pModel, Mid: mid, Tau: tau, Best: *best,
		Signal: prm.signal(here, now, *best, tau, paused)}
	if paused || tau < MinTau {
		return d
	}

	c := best.Cost
	lastBet := math.Inf(-1)
	committed := make([]float64, 0, len(here))
	for _, lot := range here {
		lastBet = math.Max(lastBet, lot.T)
		committed = append(committed, lot.Cost)
	}
	if !(prm.TauMin <= tau && tau <= prm.TauMax) || best.Edge < prm.MinEdge ||
		!(prm.BandMin <= c && c <= prm.BandMax) || len(here) >= prm.MaxBets ||
		(len(here) > 0 && now-lastBet < MinGap) {
		return d
	}
	spent := pyfloat.Sum(committed)
	room := float64(WindowCap*(cash+spent)) - spent
	unit := c + float64(FeeRate*c*(1-c)) // cost of one contract, fee included
	kelly := (best.P - unit) / (1 - unit)
	stake := math.Min(math.Min(Kelly*kelly, MaxStake)*cash, room)
	d.Entry = sized(*best, stake, unit, cash)
	return d
}

// sized turns a stake into whole contracts that the book can fill and the cash can pay for.
func sized(o Option, stake, unit, cash float64) *Entry {
	n := min(int(pyfloat.FloorDiv(stake, unit)), int(o.Size))
	for n > 0 && float64(float64(n)*o.Cost)+KalshiFee(n, o.Cost) > cash {
		n--
	}
	if n < 1 {
		return nil
	}
	fee := KalshiFee(n, o.Cost)
	return &Entry{Option: o, N: n, Fee: fee, Cost: round2(float64(float64(n)*o.Cost) + fee)}
}

// decideLottery buys the cheap side when it is much likelier than its price and volatility
// has just spiked. One small bet per round, held to settlement.
func (prm Params) decideLottery(m Market, price, now, pModel float64, paused bool, ctx TailContext, st AccountState) Decision {
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
	for _, lot := range st.Lots {
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
	d := Decision{View: &View{PUp: pUp, PModel: pModel, Mid: mid, Tau: tau, Best: cheap, Signal: sig}}
	if why != "" || tau < MinTau {
		return d
	}
	unit := cheap.Cost + float64(FeeRate*cheap.Cost*(1-cheap.Cost))
	d.Entry = sized(cheap, LotteryStake*st.Cash, unit, st.Cash)
	return d
}

// signal mirrors the entry checks, so what the display says is what the strategy will do.
func (prm Params) signal(here []*Lot, now float64, best Option, tau float64, paused bool) Signal {
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
