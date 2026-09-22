package main

import (
	"math"
	"testing"
)

func TestMeasureM1ReportsAndFreezes(t *testing.T) {
	rows := synthetic(40*480, 0.6, 11)
	rep := measureM1(rows, false)
	if rep.Rows != 40*480 || rep.Windows != 480 || rep.RowsInA == 0 || !rep.LHat.OK {
		t.Fatalf("%+v", rep)
	}
	if !rep.SelfCheck.Pass {
		t.Fatalf("self-check %+v", rep.SelfCheck)
	}
	if rep.LHat.B < 19 || rep.LHat.B > 21 || math.IsNaN(rep.LHat.SE) {
		t.Fatalf("blocks %d se %v", rep.LHat.B, rep.LHat.SE)
	}
	if rep.Lambda <= 0 || rep.Lambda > rep.LHat.Value {
		t.Fatalf("lambda %.2f from L_hat %.3f", rep.Lambda, rep.LHat.Value)
	}
	if len(rep.ByCoin) != 2 || len(rep.ByDTercile) != 3 || len(rep.ByPriceBand) == 0 {
		t.Fatalf("slices: coins %d terciles %d bands %d", len(rep.ByCoin), len(rep.ByDTercile), len(rep.ByPriceBand))
	}
}

func TestStaleCostRules(t *testing.T) {
	// One row per case. The book is two-sided; the forecast alone decides the side.
	base := row{W: 1_790_000_900, YesAsk: 0.61, YesBid: 0.59, NoAsk: 0.41, NoBid: 0.39}
	buyYes := base                                                                          // p 0.70 > yes ask: buys yes
	buyYes.P = 0.70                                                                         //
	buyNo := base                                                                           // 1 - p = 0.75 > no ask 0.41: buys no
	buyNo.P = 0.25                                                                          //
	neither := base                                                                         // p 0.60 < yes ask 0.61 and 1-p 0.40 < no ask 0.41
	neither.P = 0.60                                                                        //
	crossed := row{W: base.W, P: 0.5, YesAsk: 0.30, NoAsk: 0.30, YesBid: 0.29, NoBid: 0.29} // both sides buy
	noNext := buyYes
	noNext.MarketID = 99
	nexts := map[int64]nextQuote{
		0:  {Found: true, YesAsk: 0.63, NoAsk: 0.41, YesBid: 0.59, NoBid: 0.39}, // yes ask moved 0.02 against the buyer
		1:  {Found: true, YesAsk: 0.61, NoAsk: 0.00, YesBid: 0.59, NoBid: 0.39}, // the no ask vanished: read as 1.00
		99: {Found: false},
	}
	buyNo.MarketID = 1
	rows := []row{buyYes, buyNo, neither, crossed, noNext}
	rep := staleCost(rows, func(r row) nextQuote { return nexts[r.MarketID] }, false, false)
	if rep.Observations != 2 || rep.NoSide != 1 || rep.BothSides != 1 || rep.NoNext != 1 || rep.Vanished != 1 {
		t.Fatalf("%+v", rep)
	}
	// observations: 0.02 and (1.00 - 0.41) = 0.59; mean 0.305
	if math.Abs(rep.Mean.Value-0.305) > 1e-12 || rep.Frozen != 0.305 || math.Abs(rep.MeanNoVanished.Value-0.02) > 1e-12 {
		t.Fatalf("mean %v frozen %v without vanished %v", rep.Mean.Value, rep.Frozen, rep.MeanNoVanished.Value)
	}
	if !rep.UnderMinimumObs {
		t.Error("two observations are under the minimum and the note must say so")
	}

	// Selling: p 0.50 under a yes bid of 0.59 sells yes; the next bid is 0.55: the loss is 0.04.
	sellYes := row{W: base.W, P: 0.50, YesAsk: 0.61, YesBid: 0.59, NoAsk: 0.41, NoBid: 0.39}
	gone := sellYes
	gone.MarketID = 2
	rep = staleCost([]row{sellYes, gone}, func(r row) nextQuote {
		if r.MarketID == 2 {
			return nextQuote{Found: true, YesBid: 0, NoBid: 0.39, YesAsk: 0.61, NoAsk: 0.41} // the bid vanished: 0
		}
		return nextQuote{Found: true, YesBid: 0.55, NoBid: 0.39, YesAsk: 0.61, NoAsk: 0.41}
	}, true, false)
	if rep.Observations != 2 || rep.Vanished != 1 || math.Abs(rep.Mean.Value-(0.04+0.59)/2) > 1e-12 {
		t.Fatalf("sell: %+v", rep)
	}
	// A negative mean freezes as 0.
	better := staleCost([]row{buyYes}, func(row) nextQuote { return nextQuote{Found: true, YesAsk: 0.58, NoAsk: 0.41} }, false, false)
	if better.Frozen != 0 || better.Mean.Value >= 0 {
		t.Fatalf("a favourable move: %+v", better)
	}
}

func TestOutcomeCorrelationAndBlend(t *testing.T) {
	rows := synthetic(40*480, 0.6, 5)
	m3 := outcomeCorrelation(rows, false)
	e, ok := m3.Pairs["BTC/ETH"]
	if !ok || !e.OK || e.N != 480 || math.IsNaN(e.SE) {
		t.Fatalf("%+v", m3)
	}
	ac := autocorrelations(inA(rows))
	if len(ac.Numerator) != 9 || len(ac.Brier) != 9 || ac.Numerator[8].Lag != 24 {
		t.Fatalf("%+v", ac)
	}
	// Made-up independent rows: with the doubling rule firing about 9% of the time on such
	// data, a fixed seed gives a fixed answer; the report says which and why.
	if ac.Why == "" {
		t.Error("the decision must say why")
	}
	// R2 on TEST-shaped rows drawn at the same lambda: the blend beats the mid.
	test := synthetic(40*192, 0.6, 9)
	r2 := blendTest(test, 0.6, false)
	if r2.Windows != 192 || r2.Statistic.B < 7 || r2.Statistic.B > 9 || !r2.Pass {
		t.Fatalf("%+v", r2)
	}
	// And with lambda 0 the blend IS the mid: b_w = 0 everywhere, no SE, no pass.
	if z := blendTest(test, 0, false); z.Pass || z.Statistic.Value != 0 {
		t.Fatalf("lambda 0: %+v", z)
	}
	p := powerAtTrain(rows, 0.6)
	if p.Windows != 480 || math.IsNaN(p.Power192) || p.Power192 <= 0.5 {
		t.Fatalf("power %+v", p)
	}
}
