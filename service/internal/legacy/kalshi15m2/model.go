// Package kalshi15m2 is the second version of doipster's strategy family, ported from
// kalshi_trader.py on main at dc10fd4.
//
// What changed from the first version (package kalshi15m, which stays as it was so its results
// remain comparable): every strategy has ONE balance shared across all coins; the bank is
// $1,000 with at most $250 at risk at once, measured against the starting balance; the Scalper
// trades often and banks gains; a strategy that runs out is written up and staked again; and
// every strategy has an anti-world twin that takes the other side of each bet for the same
// stake and retires when it runs out.
//
// As in the first port, arithmetic keeps the Python's order of operations and the accounts
// keep its float-dollar bookkeeping, because the port is checked by replaying a recording
// (cmd/replay2). Every product that feeds an addition is wrapped in float64() so the compiler
// cannot fuse it: see the note in package kalshi15m.
//
// Simulated money only. Nothing here can place an order.
package kalshi15m2

import (
	"math"
	"strconv"
	"strings"

	"github.com/doipster/asset_cracker/service/internal/pyfloat"
)

const (
	StartBalance = 1000.0
	BankruptAt   = 1.0 // under this with nothing open, an account cannot place another bet

	FeeRate    = 0.07
	Slippage   = 0.01
	Kelly      = 0.25
	MaxStake   = 0.20 // default share of cash for one bet; a strategy may override
	WindowCap  = 0.35 // ...or more than this share of the account into one window
	TotalCap   = 0.25 // at most this share of the STARTING balance at risk across every coin
	MinGap     = 20.0 // default seconds between bets in a round; a strategy may override
	MinHold    = 15.0 // default seconds before a bet may be sold again
	ExitMargin = 0.02
	MinTau     = 8.0
	LogKept    = 500

	LotteryMaxAsk   = 0.15
	LotteryMinMult  = 3.0
	LotteryMinSpike = 1.3
	LotteryMinTau   = 180.0
	LotteryStake    = 0.01
	LotteryFatten   = 1.15
	LotterySlippage = 0.005
)

// Params is one strategy's settings. Pointers are settings a strategy may leave unset.
type Params struct {
	Name        string   `json:"name"`
	Blurb       string   `json:"blurb"`
	Shrink      float64  `json:"shrink"`
	MinEdge     float64  `json:"min_edge"`
	MaxBets     int      `json:"max_bets"`
	Exit        string   `json:"exit"` // "hold" or "ev"
	TauMin      float64  `json:"tau_min"`
	TauMax      float64  `json:"tau_max"`
	BandMin     float64  `json:"band_min"`
	BandMax     float64  `json:"band_max"`
	Lottery     bool     `json:"lottery,omitempty"`
	MinGap      float64  `json:"min_gap"`
	MaxStake    float64  `json:"max_stake"`
	MinHold     float64  `json:"min_hold"`
	TakeCapture *float64 `json:"take_capture,omitempty"` // bank a gain once the bid covers this much of the way to $1
	StopLoss    *float64 `json:"stop_loss,omitempty"`    // measured and found worse at every setting: ships unset
	StopTau     *float64 `json:"stop_tau,omitempty"`     // likewise
	Anti        bool     `json:"anti,omitempty"`         // a twin: no opinions, mirrors its original
}

func base(name, blurb string, shrink, minEdge float64, maxBets int, exit string, tauMin, tauMax, bandMin, bandMax float64) Params {
	return Params{Name: name, Blurb: blurb, Shrink: shrink, MinEdge: minEdge, MaxBets: maxBets, Exit: exit,
		TauMin: tauMin, TauMax: tauMax, BandMin: bandMin, BandMax: bandMax, MinGap: MinGap, MaxStake: MaxStake, MinHold: MinHold}
}

// Strategies are the six originals, in the Python's order. Order matters: events from one step
// are written in this order, each original followed by its twin.
var Strategies = func() []Params {
	capture := 0.80
	scalper := base("Scalper", "Trades often, banks small gains", 0.5, 0.03, 25, "ev", 25, 900, 0.05, 0.95)
	scalper.MinGap, scalper.MaxStake, scalper.MinHold, scalper.TakeCapture = 8, 0.015, 5, &capture
	lottery := base("Lottery", "Cheap longshots after a vol spike", 1.0, 0.0, 1, "hold", LotteryMinTau, 900, 0.0, LotteryMaxAsk)
	lottery.Lottery = true
	return []Params{
		base("Value", "Model + market blend, holds", 0.5, 0.03, 1, "hold", MinTau, 900, 0.05, 0.95),
		base("Model", "Trusts the model, adds bets", 1.0, 0.05, 3, "hold", MinTau, 900, 0.05, 0.95),
		base("Late", "Only bets the last 2.5 minutes", 1.0, 0.03, 2, "hold", MinTau, 150, 0.05, 0.95),
		scalper,
		base("Favorite", "Backs the favorite late", 1.0, 0.0, 1, "hold", MinTau, 240, 0.62, 0.88),
		lottery,
	}
}()

// Mirrored is a strategy's anti-world twin.
func Mirrored(p Params) Params {
	t := p
	t.Name, t.Blurb, t.Anti = "Anti "+p.Name, "Takes the other side of "+p.Name, true
	return t
}

// AllStrategies is the six originals followed by their six twins, as the Python orders them.
func AllStrategies() []Params {
	out := append([]Params(nil), Strategies...)
	for _, p := range Strategies {
		out = append(out, Mirrored(p))
	}
	return out
}

// Market is one coin's open round and its live quotes.
type Market struct {
	Coin       string  `json:"coin"`
	Ticker     string  `json:"ticker"`
	Strike     float64 `json:"strike"`
	Close      float64 `json:"close"`
	YesBid     float64 `json:"yes_bid"`
	YesAsk     float64 `json:"yes_ask"`
	NoBid      float64 `json:"no_bid"`
	NoAsk      float64 `json:"no_ask"`
	YesAskSize float64 `json:"yes_ask_size"`
	NoAskSize  float64 `json:"no_ask_size"`
}

// ParseAmount reads a number from Kalshi, which sometimes arrives as text ("79,604.96").
func ParseAmount(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(x, ",", "")), 64)
		return f, err == nil
	}
	return 0, false
}

// KalshiFee is the taker fee in dollars, rounded up to the next cent.
func KalshiFee(contracts int, price float64) float64 {
	return math.Ceil(float64(FeeRate*float64(contracts)*price*(1-price)*100)-1e-9) / 100
}

// NormCDF is the standard normal distribution function.
func NormCDF(x float64) float64 { return 0.5 * (1 + math.Erf(x/math.Sqrt(2))) }

// ProbYes is the chance the settlement value ends at or above the strike. Unchanged from v1.
func ProbYes(price, strike, tau, sigma2 float64, knownAvg *float64, offsetPct, sdPct float64) float64 {
	lift := 1 + offsetPct
	var mean, variance float64
	if tau >= 60 {
		mean = price * lift
		variance = sigma2 * (price * price) * ((tau - 60) + 60.0/3)
	} else {
		seen := (60 - tau) / 60
		m := price
		if knownAvg != nil {
			m = float64(seen**knownAvg) + float64((1-seen)*price)
		}
		mean = m * lift
		variance = ((tau / 60) * (tau / 60)) * sigma2 * (price * price) * tau / 3
	}
	sd := math.Sqrt(variance + float64((sdPct*price)*(sdPct*price)))
	return math.Min(0.999, math.Max(0.001, NormCDF((mean-strike)/sd)))
}

func round2(x float64) float64 { return pyfloat.Round(x, 2) }

// pyRoundInt is Python's round(x) with no digits: to the nearest integer, ties to even.
func pyRoundInt(x float64) int { return int(math.RoundToEven(x)) }
