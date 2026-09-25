package analysis

import (
	"strings"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// leg1 is one ladder leg with one bucket's result on it, its bet placed firstBet (unix s).
func leg1(id, closes, bucket, pnl int64, bets int, firstBet int64) MarketFacts {
	f := round1(id, closes, bucket, pnl, bets)
	f.Family = store.FamilyLadders
	r := f.Rounds[bucket]
	r.FirstBet = firstBet
	f.Rounds[bucket] = r
	return f
}

// The ladders close at 5 pm ET, which is also the close of a 15-minute round. A leg still waiting
// for its result leaves out its own window and not the round's, and the other way round.
func TestALadderWindowIsNotTheRoundWindowAtTheSameClose(t *testing.T) {
	const close = 1790110800 // 2026-09-23 21:00 UTC, 5 pm ET: a ladder close and a quarter hour
	buckets := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v3", World: "real"},
		{ID: 2, VersionID: 2, Strategy: "Day Value", Engine: "v3", World: "real", Family: store.FamilyLadders}}
	facts := []MarketFacts{round1(1, close, 1, 500, 1), leg1(2, close, 2, -300, 1, close-7200)}
	windows := func(doc Document) map[int64]int {
		out := map[int64]int{}
		for _, r := range doc.Leaderboard.Rows {
			out[r.VersionID] = r.Windows
		}
		return out
	}

	doc := Build(Inputs{Facts: facts, Buckets: buckets, Incomplete: map[Window]bool{{Family: store.FamilyLadders, Closes: close}: true}, MarketsSettled: 2, Trials: 18})
	if w := windows(doc); w[1] != 1 || w[2] != 0 || doc.WindowsRecorded != 1 || doc.Coverage.WindowsIncomplete != 1 {
		t.Errorf("a leg waiting kept the round out, or was counted: windows %v, recorded %d, %+v", w, doc.WindowsRecorded, doc.Coverage)
	}
	doc = Build(Inputs{Facts: facts, Buckets: buckets, Incomplete: map[Window]bool{{Closes: close}: true}, MarketsSettled: 2, Trials: 18})
	if w := windows(doc); w[1] != 0 || w[2] != 1 || doc.WindowsRecorded != 0 || doc.Coverage.WindowsIncomplete != 1 {
		t.Errorf("a round waiting kept the leg out, or was counted: windows %v, recorded %d, %+v", w, doc.WindowsRecorded, doc.Coverage)
	}
}

// A ladder version gets its row and its gate like any other, on windows of legs closing together;
// its calibration check fails because no scorecard scores the ladder model, and nothing about it
// reaches the scorecard, the fills or windows_recorded. The rounds version beside it, on the same
// figures, passes as before.
func TestALadderVersionIsScoredAndNeverPassesCalibration(t *testing.T) {
	buckets := []Bucket{{ID: 1, VersionID: 1, Strategy: "Value", Engine: "v3", World: "real"},
		{ID: 2, VersionID: 2, Strategy: "Day Value", Engine: "v3", World: "real", Family: store.FamilyLadders}}
	facts := gated(1)
	const day, fivePM = 86400, 75600
	for i := range 40 {
		closes := int64(day*(i+1) + fivePM)
		f := leg1(int64(100+i), closes, 2, int64(100+200*(i%2)), 1, closes-int64(3600*(3+i%20)))
		f.Score[2] = BandSum{N: 1000, Model: 900, Market: 1, ModelLog: 5000, MarketLog: 1} // never read: a ladder is not scored
		f.Sales = []PricedSale{{BucketID: 2, Qty: 5, Beyond: 5, PnLCents: 50, CappedCents: -50}}
		facts = append(facts, f)
	}
	doc := Build(Inputs{Facts: facts, Buckets: buckets, MarketsSettled: len(facts), Trials: 18, Gate: GateSettings{MinEdgePerDollar: 0.2, Power: 0.8, MaxDrawdownCents: 25_000}})

	if doc.WindowsRecorded != 40 || doc.Scorecard.Overall.NWindows != 40 || doc.Scorecard.Overall.Verdict == "model worse" {
		t.Errorf("the ladder reached the scorecard: recorded %d, %+v", doc.WindowsRecorded, doc.Scorecard.Overall)
	}
	for _, r := range doc.Fills.ByStrategy {
		if r.Strategy == "Day Value" {
			t.Errorf("the ladder reached the fills: %+v", r)
		}
	}
	rows := map[int64]LeaderRow{}
	for _, r := range doc.Leaderboard.Rows {
		rows[r.VersionID] = r
	}
	rounds, ladder := rows[1], rows[2]
	if !rounds.Gate.Passed || rounds.Family != store.FamilyRounds || rounds.PeriodStart != rounds.FirstClose-900 {
		t.Errorf("the rounds version changed: %+v", rounds)
	}
	if ladder.Family != store.FamilyLadders || ladder.Windows != 40 || ladder.Bets != 40 || ladder.Decisions != 0 || ladder.ReturnPerDollar != 0.2 {
		t.Fatalf("ladder row: %+v", ladder)
	}
	if first := int64(day+fivePM) - 3*3600; ladder.PeriodStart != first || ladder.FirstClose != day+fivePM {
		t.Errorf("ladder period: starts %d (want the first bet, %d), first close %d", ladder.PeriodStart, first, ladder.FirstClose)
	}
	if !ladder.Gate.Evaluated || ladder.Gate.Passed || len(ladder.Gate.Checks) != 4 {
		t.Fatalf("ladder gate: %+v", ladder.Gate)
	}
	for _, c := range ladder.Gate.Checks {
		if c.Name == "calibration" {
			if c.Passed || !strings.Contains(c.Why, "ladder model is not scored") {
				t.Errorf("calibration: %+v", c)
			}
		} else if !c.Passed {
			t.Errorf("%s failed on figures that pass for the rounds: %+v", c.Name, c)
		}
	}
	if !strings.Contains(strings.Join(ladder.Flags, " "), "a ladder version") {
		t.Errorf("flags: %v", ladder.Flags)
	}
	snaps := Snapshots(doc)
	for _, s := range snaps {
		if s.VersionID == 2 && (s.PeriodStart != ladder.PeriodStart || s.GatePassed) {
			t.Errorf("ladder snapshot: %+v", s)
		}
	}
}

// A market's first bet is its earliest buy; a sale is not a bet.
func TestSettleRecordsTheFirstBet(t *testing.T) {
	f := Settle(Market{ID: 1, Closes: 90000, Result: "yes", Family: store.FamilyLadders}, []Trade{
		{OrderID: 3, BucketID: 7, Side: "yes", Qty: 2, Second: 300, CostCents: 100},
		{OrderID: 2, BucketID: 7, Side: "yes", Qty: 2, Second: 200, CostCents: 100},
		{OrderID: 1, BucketID: 7, Sell: true, Side: "yes", Qty: 1, Second: 100, CostCents: 50, PayoutCents: 60},
		{OrderID: 4, BucketID: 8, Sell: true, Side: "no", Qty: 1, Second: 50, CostCents: 50, PayoutCents: 40},
	}, nil)
	if r := f.Rounds[7]; r.FirstBet != 200 || r.Bets != 2 {
		t.Errorf("bucket 7: %+v", r)
	}
	if r := f.Rounds[8]; r.FirstBet != 0 || r.Bets != 0 {
		t.Errorf("bucket 8 only sold: %+v", r)
	}
}

// A family the store did not name is the rounds', as every market was before the ladders were read.
func TestWindowOf(t *testing.T) {
	for family, want := range map[string]string{"": store.FamilyRounds, store.FamilyRounds: store.FamilyRounds, store.FamilyLadders: store.FamilyLadders} {
		if got := WindowOf(Market{Closes: 900, Family: family}); got != (Window{want, 900}) {
			t.Errorf("%q: %+v", family, got)
		}
	}
}
