// Package kalshi15m is the first strategy family: doipster's six strategies for Kalshi's
// 15-minute crypto markets, ported from kalshi_trader.py.
//
// The arithmetic keeps the Python's order of operations, and the account keeps the Python's
// float-dollar bookkeeping, because the port is checked by replaying a recording and comparing
// trade logs character for character (cmd/replay). The platform's own ledger is integer cents;
// connecting the two is the paper broker's job, not this package's.
//
// A note on float64(...) around products: the Go spec lets a compiler fuse x*y + z into one
// operation, and on arm64 it does. A fused result can differ from Python's in the last bit,
// and, worse, the Pi (arm64) and an Intel machine would then disagree with each other. An
// explicit conversion forces the product to be rounded first, which forbids the fusion. Every
// product that feeds an addition or subtraction in this package is wrapped for that reason.
//
// Simulated money only. Nothing here can place an order.
package kalshi15m

import (
	"math"
	"strconv"
	"strings"

	"github.com/doipster/asset_cracker/service/internal/pyfloat"
)

const (
	StartBalance = 150.0

	FeeRate     = 0.07 // Kalshi's taker fee: 7% x price x (1 - price) per contract, rounded up
	Slippage    = 0.01 // pay one cent worse than the displayed price, buying or selling
	Kelly       = 0.25 // bet this fraction of the Kelly-optimal stake
	MaxStake    = 0.20 // never put more than this share of cash into one bet
	WindowCap   = 0.35 // ...or more than this share of the account into one window
	MinGap      = 20.0 // seconds between bets in the same window
	ExitMargin  = 0.02 // an early sale must beat the estimated hold value by this
	MinTau      = 8.0  // stop trading when this few seconds remain
	LogKept     = 500
	IndexOffset = 0.000057 // BTC defaults, measured by the Python over 664 settled rounds
	IndexSD     = 0.000144
	DefaultSig  = 8e-5
)

// The Lottery strategy's settings. Kept as a documented negative result: see the README.
const (
	LotteryMaxAsk   = 0.15
	LotteryMinMult  = 3.0
	LotteryMinSpike = 1.3
	LotteryMinTau   = 180.0
	LotteryStake    = 0.01
	LotteryFatten   = 1.15
	LotterySlippage = 0.005
)

// Params is one strategy's settings. A row in strategy_version holds these as JSON.
type Params struct {
	Name    string  `json:"name"`
	Blurb   string  `json:"blurb"`
	Shrink  float64 `json:"shrink"`   // how far to trust the model over the market's own odds
	MinEdge float64 `json:"min_edge"` // edge after fees needed to bet
	MaxBets int     `json:"max_bets"` // per round
	Exit    string  `json:"exit"`     // "hold" or "ev"
	TauMin  float64 `json:"tau_min"`  // only bet with this many seconds left...
	TauMax  float64 `json:"tau_max"`  // ...or fewer than this
	BandMin float64 `json:"band_min"` // only buy at a cost inside this band
	BandMax float64 `json:"band_max"`
	Lottery bool    `json:"lottery,omitempty"`
}

// Strategies is the family, in the order the Python lists them. Order matters: events from one
// step are written in this order.
var Strategies = []Params{
	{"Value", "Model + market blend, holds", 0.5, 0.03, 1, "hold", MinTau, 900, 0.05, 0.95, false},
	{"Model", "Trusts the model, adds bets", 1.0, 0.05, 3, "hold", MinTau, 900, 0.05, 0.95, false},
	{"Late", "Only bets the last 2.5 minutes", 1.0, 0.03, 2, "hold", MinTau, 150, 0.05, 0.95, false},
	{"Scalper", "In and out, cuts losers early", 0.5, 0.03, 3, "ev", 25, 900, 0.05, 0.95, false},
	{"Favorite", "Backs the favorite late", 1.0, 0.0, 1, "hold", MinTau, 240, 0.62, 0.88, false},
	{"Lottery", "Cheap longshots after a vol spike", 1.0, 0.0, 1, "hold", LotteryMinTau, 900, 0.0, LotteryMaxAsk, true},
}

// Market is the open round and its live quotes, in dollars per contract.
type Market struct {
	Ticker     string  `json:"ticker"`
	Strike     float64 `json:"strike"`
	Close      float64 `json:"close"` // unix seconds
	YesBid     float64 `json:"yes_bid"`
	YesAsk     float64 `json:"yes_ask"`
	NoBid      float64 `json:"no_bid"`
	NoAsk      float64 `json:"no_ask"`
	YesAskSize float64 `json:"yes_ask_size"`
	NoAskSize  float64 `json:"no_ask_size"`
}

// ParseAmount reads a number from Kalshi, which sometimes arrives as text with thousands
// separators ("79,604.96"). ok is false if it is not a usable number.
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

// ProbYes is the chance the settlement value (the index averaged over the round's last 60
// seconds) ends at or above the strike. price and knownAvg are Coinbase prices; the strike is
// on Kalshi's index, which runs slightly higher, so both are lifted by offsetPct. tau is
// seconds left; knownAvg (nil if none) is our average over the part of the final minute seen.
func ProbYes(price, strike, tau, sigma2 float64, knownAvg *float64, offsetPct, sdPct float64) float64 {
	lift := 1 + offsetPct
	var mean, variance float64
	if tau >= 60 {
		// Walk to the start of the final minute, then average a 60-second walk.
		mean = price * lift
		variance = sigma2 * (price * price) * ((tau - 60) + 60.0/3)
	} else {
		seen := (60 - tau) / 60 // how much of the averaging window is already known
		m := price
		if knownAvg != nil {
			m = float64(seen**knownAvg) + float64((1-seen)*price)
		}
		mean = m * lift
		variance = ((tau / 60) * (tau / 60)) * sigma2 * (price * price) * tau / 3
	}
	sd := math.Sqrt(variance + float64((sdPct*price)*(sdPct*price))) // our price vs the index: uncertain
	return math.Min(0.999, math.Max(0.001, NormCDF((mean-strike)/sd)))
}

func round2(x float64) float64 { return pyfloat.Round(x, 2) }
