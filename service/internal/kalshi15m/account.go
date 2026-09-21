package kalshi15m

import (
	"math"
	"time"

	"github.com/doipster/asset_cracker/service/internal/pyfloat"
)

// Event is something that happened to a bet: placed, sold early, or settled. Lot is a copy
// taken at that moment.
type Event struct {
	Kind     string // bet, sold, settled
	Strategy string
	Lot      Lot
	Won      bool // settled only
}

// Account is one strategy's paper account, with the Python's float-dollar bookkeeping.
type Account struct {
	Params      Params
	Cash        float64
	Log         []*Lot
	Bets        int
	Wins        int
	Losses      int
	RealizedPnL float64
	NextID      int
	View        *View
}

// NewAccount starts an account at the starting balance.
func NewAccount(p Params) *Account { return &Account{Params: p, Cash: StartBalance, NextID: 1} }

func (a *Account) openLots(ticker string) []*Lot {
	var out []*Lot
	for _, lot := range a.Log {
		if lot.Status == "open" && (ticker == "" || lot.Ticker == ticker) {
			out = append(out, lot)
		}
	}
	return out
}

// Equity is cash plus what open bets could be sold for right now, after the selling fee (or
// their cost, if there is no bid to price them by).
func (a *Account) Equity(m *Market) float64 {
	total := a.Cash
	for _, lot := range a.openLots("") {
		bid := 0.0
		if m != nil && m.Ticker == lot.Ticker {
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

// Participated is how many different rounds this strategy has bet in.
func (a *Account) Participated() int {
	seen := map[string]bool{}
	for _, lot := range a.Log {
		seen[lot.Ticker] = true
	}
	return len(seen)
}

// Step lets the strategy decide, then applies the decision: exits first, then the entry.
func (a *Account) Step(m Market, price, now, pModel float64, paused bool, ctx TailContext) []Event {
	d := a.Params.Decide(m, price, now, pModel, paused, ctx, AccountState{Cash: a.Cash, Lots: a.Log})
	a.View = d.View
	var events []Event
	for _, ex := range d.Exits {
		for _, lot := range a.Log {
			if lot.ID == ex.LotID {
				exitPrice, exitBTC := round2(ex.SellC), price
				lot.ExitPrice, lot.ExitBTC = &exitPrice, &exitBTC
				a.close(lot, "sold", ex.Proceeds, now)
				events = append(events, Event{Kind: "sold", Strategy: a.Params.Name, Lot: *lot})
			}
		}
	}
	if e := d.Entry; e != nil {
		a.Cash -= e.Cost
		lot := &Lot{ID: a.NextID, T: now, Time: ISOSeconds(now), Ticker: m.Ticker, Side: e.Option.Side,
			Contracts: e.N, Price: round2(e.Option.Cost), Multiplier: round2(1 / e.Option.Cost), Fee: e.Fee,
			Cost: e.Cost, Strike: m.Strike, Close: m.Close, BTCPrice: price,
			ModelProb: pyfloat.Round(e.Option.P, 3), Edge: pyfloat.Round(e.Option.Edge, 3), Status: "open"}
		a.NextID++
		a.Bets++
		a.Log = append(a.Log, lot)
		events = append(events, Event{Kind: "bet", Strategy: a.Params.Name, Lot: *lot})
	}
	return events
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

// OnSettled pays out or writes off every open bet in a round that has settled.
func (a *Account) OnSettled(ticker, result string, finalValue any, now float64, price *float64) []Event {
	var events []Event
	for _, lot := range a.openLots(ticker) {
		won := (result == "yes") == (lot.Side == "UP")
		lot.Result, lot.FinalValue, lot.ExitBTC = result, finalValue, price
		payout := 0.0
		if won {
			payout = float64(lot.Contracts)
		}
		a.close(lot, map[bool]string{true: "won", false: "lost"}[won], payout, now)
		events = append(events, Event{Kind: "settled", Strategy: a.Params.Name, Lot: *lot, Won: won})
	}
	return events
}

// ISOSeconds formats a unix time as local YYYY-MM-DDTHH:MM:SS, as Python's
// datetime.fromtimestamp(ts).isoformat(timespec="seconds").
func ISOSeconds(ts float64) string {
	micros := int64(math.RoundToEven(ts * 1e6))
	return time.UnixMicro(micros).Local().Format("2006-01-02T15:04:05")
}
