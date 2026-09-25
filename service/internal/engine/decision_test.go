package engine

import (
	"math"
	"testing"
)

// A decision says which probabilities it formed. The market's needs a two-sided book and a model
// view; the model's needs only the view. The journal stores a figure that was not formed as NULL,
// and the scorecard scores only seconds with a market price, so neither can be read as a 0.
func TestADecisionSaysWhichProbabilitiesItFormed(t *testing.T) {
	p, err := FromShape(Shape{Name: "Flags", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012})
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(NewAccount(p, 1, 100000, true))
	if err != nil {
		t.Fatal(err)
	}
	m := Market{Ticker: "T", MarketID: 1, Strike: 100, Close: 1000}
	two := book([][2]string{{"0.4900", "100"}}, [][2]string{{"0.4900", "100"}}) // yes bid 0.49, yes ask 0.51
	one := book([][2]string{{"0.4900", "100"}}, nil)                            // nobody bids No: no yes ask, no mid
	for i, c := range []struct {
		name              string
		v                 View
		q                 bool // two-sided
		hasModel, hasProb bool
		model, market     float64
	}{
		{name: "two-sided, with a view", v: view("BTC", 0.95), q: true, hasModel: true, hasProb: true, model: 0.95, market: 0.5},
		{name: "one-sided, with a view", v: view("BTC", 0.95), q: false, hasModel: true, hasProb: false, model: 0.95},
		{name: "two-sided, no view", v: View{Coin: "BTC"}, q: true},
		{name: "a view whose model says exactly 0", v: view("BTC", 0), q: true, hasModel: true, hasProb: true, model: 0, market: 0.5},
	} {
		m.EvaluationID = int64(i + 1)
		q := one
		if c.q {
			q = two
		}
		ds, _ := e.Decide("BTC", m, q, c.v, 1000-300)
		if len(ds) != 1 {
			t.Fatalf("%s: %d decisions", c.name, len(ds))
		}
		d := ds[0]
		if d.HasModel != c.hasModel || d.HasProb != c.hasProb || math.Abs(d.ModelProb-c.model) > 1e-12 || math.Abs(d.MarketProb-c.market) > 1e-12 {
			t.Errorf("%s: has model %v, has prob %v, model %v, market %v", c.name, d.HasModel, d.HasProb, d.ModelProb, d.MarketProb)
		}
	}
}
