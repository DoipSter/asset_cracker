package kalshi15m2

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Writes the SQL values for db/migrations/0007 from the Go parameters, so they cannot drift.
// Run with: AC_GEN=/path/to/out go test -run TestGenerateStrategySeed ./internal/kalshi15m2/
func TestGenerateStrategySeed(t *testing.T) {
	out := os.Getenv("AC_GEN")
	if out == "" {
		t.Skip("set AC_GEN=<file> to write the seed rows")
	}
	var rows []string
	for _, p := range AllStrategies() {
		b, _ := json.Marshal(p)
		rows = append(rows, fmt.Sprintf("    ('%s', '%s', %t, '%s'::jsonb)", p.Name, p.Blurb, p.Anti, strings.ReplaceAll(string(b), "'", "''")))
	}
	if err := os.WriteFile(out, []byte(strings.Join(rows, ",\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An account that has run out: an original is staked again, a twin retires, and neither is
// declared dead while it still has a bet open.
func TestRunningOut(t *testing.T) {
	now := 1000.0
	tr := NewTrader(func() float64 { return now }, []string{"BTC"}, map[string]Calibration{"BTC": {DefaultSigma: 8e-5}})
	orig, twin := tr.Account("Value"), tr.Account("Anti Value")
	orig.Cash, twin.Cash = 0.40, 0.40
	orig.Log = []*Lot{{ID: 1, Status: "open", Ticker: "T", Coin: "BTC", Close: 1900, Cost: 5}}
	if orig.Broke() {
		t.Fatal("an account with a bet still open is not finished")
	}
	orig.Log[0].Status = "lost"
	events := tr.record(nil, now)
	if len(events) != 2 {
		t.Fatalf("want two bankrupt events, got %d", len(events))
	}
	if orig.Cash != StartBalance || orig.Bankruptcies != 1 || len(orig.Log) != 0 || orig.Retired {
		t.Errorf("the original should be staked again from scratch: %+v", orig)
	}
	if !twin.Retired || twin.Cash != 0.40 || twin.Bankruptcies != 0 {
		t.Errorf("the twin should retire with what it had: %+v", twin)
	}
	if got := tr.record(nil, now); len(got) != 0 {
		t.Errorf("a retired twin must not be written up again, got %d events", len(got))
	}
	if moved := twin.Mirror([]Event{{Kind: "bet", Lot: Lot{ID: 9, Side: "UP", Cost: 10, ModelProb: 0.6}}},
		&Market{Coin: "BTC", Ticker: "T2", NoAsk: 0.4, NoAskSize: 100}, now, 100); len(moved) != 0 {
		t.Error("a retired twin must not follow its original")
	}
}

// A twin matches its original's STAKE, not its contract count.
func TestTwinMatchesStake(t *testing.T) {
	tr := NewTrader(func() float64 { return 0 }, []string{"BTC"}, map[string]Calibration{"BTC": {DefaultSigma: 8e-5}})
	twin := tr.Account("Anti Model")
	m := &Market{Coin: "BTC", Ticker: "T", Close: 900, YesAsk: 0.30, NoAsk: 0.71, YesAskSize: 1e9, NoAskSize: 1e9}
	out := twin.Mirror([]Event{{Kind: "bet", Lot: Lot{ID: 3, Side: "UP", Contracts: 100, Cost: 31.47, ModelProb: 0.4}}}, m, 0, 100)
	if len(out) != 1 {
		t.Fatalf("want one mirrored bet, got %d", len(out))
	}
	lot := out[0].Lot
	if lot.Side != "DOWN" || lot.Cost > 31.47 || lot.Contracts >= 100 || lot.MirrorOf == nil || *lot.MirrorOf != 3 {
		t.Errorf("twin bought %d %s for $%.2f (mirror_of %v); want DOWN, under $31.47, fewer than 100 contracts", lot.Contracts, lot.Side, lot.Cost, lot.MirrorOf)
	}
}
