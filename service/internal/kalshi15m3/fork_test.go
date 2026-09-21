package kalshi15m3

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/broker"
)

var v2Fixtures = []string{"2026-09-20_v2_17min", "2026-09-20_v2_50min"}

// The fork of v2's CoinState must BE v2's model: lambda is measured on v2's p_model, so a fork
// that differs in the last bit is a different model. The same observations go to both, and at
// every step sigma2, the offset and p_model must be IDENTICAL, not close. The drift gate is run
// with a tolerance of exactly zero over the whole recording and must never close.
func TestForkMatchesV2(t *testing.T) {
	for _, fixture := range v2Fixtures {
		t.Run(fixture, func(t *testing.T) {
			calls := loadCalls(t, fixture)
			r := newReplay(t, calls[0], 0)
			steps, withView := 0, 0
			for i, c := range calls[1:] {
				s := r.feed(t, c)
				if s == nil || s.Price == 0 {
					continue
				}
				steps++
				ref := r.v2Inputs(s.Coin)
				v := r.model.View(s.Coin, Market{Ticker: s.Market.Ticker, Strike: s.Market.Strike, Close: s.Market.Close}, s.Price, s.Now, ref)
				if !v.OK {
					t.Fatalf("call %d: no view for %s", i, s.Coin)
				}
				if v.Sigma2 != ref.Sigma2 || v.Offset != ref.IndexOffset || v.OffsetSource != ref.OffsetSource {
					t.Fatalf("call %d %s: sigma2 %v/%v offset %v/%v source %s/%s (v3/v2)", i, s.Coin,
						v.Sigma2, ref.Sigma2, v.Offset, ref.IndexOffset, v.OffsetSource, ref.OffsetSource)
				}
				if v.Drift || v.DriftDiff != 0 {
					t.Fatalf("call %d %s: the drift gate closed at zero tolerance: %q, diff %v", i, s.Coin, v.DriftWhy, v.DriftDiff)
				}
				// v2 keeps its p_model only where it formed a view (both asks shown).
				if view := r.trader.Account("Model").Views[s.Coin]; view != nil {
					withView++
					if v.PModel != view.PModel {
						t.Fatalf("call %d %s: p_model %v, v2 has %v", i, s.Coin, v.PModel, view.PModel)
					}
				}
			}
			if steps < 1000 || withView < 1000 {
				t.Fatalf("only %d steps, %d with a v2 view: the fixture did not exercise the fork", steps, withView)
			}
			t.Logf("%d steps identical, %d of them with p_model compared", steps, withView)
		})
	}
}

// Test 22. lambda 0 is the mid and nothing else, so an entry is impossible on any uncrossed book:
// an ask is never under the mid. lambda 1 with no staleness cost is v2's Model strategy: the
// blend must equal the probability that strategy acted on, to the bit.
func TestBlendEnds(t *testing.T) {
	calls := loadCalls(t, v2Fixtures[1])
	r := newReplay(t, calls[0], 0)
	zero := testEngine(t, NewAccount(testScalper(t, 0, 0, 0), 1, 100000, true), NewAccount(testValue(t, 0, 0), 2, 100000, true))
	one := testEngine(t, NewAccount(testValue(t, 1, 0), 3, 100000, true))
	compared, entries := 0, 0
	for i, c := range calls[1:] {
		s := r.feed(t, c)
		if s == nil || s.Price == 0 {
			continue
		}
		m := Market{Ticker: s.Market.Ticker, MarketID: marketID(s.Market.Ticker), EvaluationID: int64(i), Strike: s.Market.Strike, Close: s.Market.Close}
		v := r.model.View(s.Coin, m, s.Price, s.Now, r.v2Inputs(s.Coin))
		q := topOfBook(s.Market)

		decisions, intents := zero.Decide(s.Coin, m, q, v, s.Now)
		if len(intents) != 0 {
			t.Fatalf("call %d: lambda 0 formed an order: %+v", i, intents[0].Order)
		}
		for _, d := range decisions {
			if d.HasProb && d.P != d.MarketProb {
				t.Fatalf("call %d: lambda 0 gives p %v, the mid is %v", i, d.P, d.MarketProb)
			}
		}

		decisions, intents = one.Decide(s.Coin, m, q, v, s.Now)
		entries += len(intents)
		theirs := r.trader.Account("Model").Views[s.Coin]
		if theirs == nil || len(decisions) != 1 || !decisions[0].HasProb {
			continue
		}
		compared++
		if decisions[0].MarketProb != theirs.Mid || decisions[0].P != theirs.PUp {
			t.Fatalf("call %d %s: mid %v p %v; v2's Model has mid %v p %v", i, s.Coin, decisions[0].MarketProb, decisions[0].P, theirs.Mid, theirs.PUp)
		}
		if decisions[0].ModelProb != theirs.PModel {
			t.Fatalf("call %d: model_prob must stay the RAW p_model", i)
		}
	}
	if compared < 1000 {
		t.Fatalf("only %d steps compared with v2's Model strategy", compared)
	}
	if entries == 0 {
		t.Fatal("lambda 1 never wanted an entry on the whole recording: the comparison with lambda 0 shows nothing")
	}
	t.Logf("%d steps compared; lambda 1 wanted %d entries, lambda 0 none", compared, entries)
}

// Test 14. Decide moves no money and changes nothing, across a recorded round with real fills
// going on around it; and an Engine cannot reach a CoinState at all.
func TestDecideIsPure(t *testing.T) {
	calls := loadCalls(t, v2Fixtures[0])
	r := newReplay(t, calls[0], 0)
	// PLACEHOLDER lambda 1 so that the recording produces entries, exits and partial fills.
	h := newHarness(t, NewAccount(testScalper(t, 1, 0.007, 0.006), 1, 100000, true), NewAccount(testValue(t, 1, 0.007), 2, 100000, true))
	kinds := map[string]int{}
	for i, c := range calls[1:] {
		if c.Call == "on_settled" {
			var ticker, result string
			_ = jsonArg(c, 0, &ticker)
			_ = jsonArg(c, 1, &result)
			for _, ev := range h.engine.ApplySettlement(ticker, result) {
				kinds[ev.Kind]++
			}
			h.paper.Forget(ticker)
		}
		s := r.feed(t, c)
		if s == nil || s.Price == 0 {
			continue
		}
		m := Market{Ticker: s.Market.Ticker, MarketID: marketID(s.Market.Ticker), Strike: s.Market.Strike, Close: s.Market.Close}
		v := r.model.View(s.Coin, m, s.Price, s.Now, r.v2Inputs(s.Coin))
		q := topOfBook(s.Market)

		before := cloneAccounts(h.engine.Accounts)
		m.EvaluationID = int64(i)
		d1, i1 := h.engine.Decide(s.Coin, m, q, v, s.Now)
		if !reflect.DeepEqual(before, cloneAccounts(h.engine.Accounts)) {
			t.Fatalf("call %d: Decide changed an account", i)
		}
		d2, i2 := h.engine.Decide(s.Coin, m, q, v, s.Now)
		if !reflect.DeepEqual(d1, d2) || !reflect.DeepEqual(i1, i2) {
			t.Fatalf("call %d: Decide gave two answers to one question", i)
		}
		_, _, _, events := h.step(s.Coin, m, q, v, s.Now)
		for _, ev := range events {
			kinds[ev.Kind]++
		}
		for _, a := range h.engine.Accounts {
			if a.CashCents < 0 {
				t.Fatalf("call %d: %s has negative cash", i, a.Params.Name)
			}
			for _, w := range a.Windows {
				if w.OpenCents < 0 || w.LostCents < 0 {
					t.Fatalf("call %d: window %+v", i, *w)
				}
			}
		}
	}
	if kinds["bought"] == 0 {
		t.Fatalf("nothing was bought on the recording, so the test showed nothing: %v", kinds)
	}
	t.Logf("events on the recording: %v", kinds)

	for _, typ := range []reflect.Type{reflect.TypeOf(Engine{}), reflect.TypeOf(Account{})} {
		for i := 0; i < typ.NumField(); i++ {
			if s := typ.Field(i).Type.String(); s == "*kalshi15m3.Model" || s == "*kalshi15m3.CoinState" || s == "kalshi15m3.Model" {
				t.Errorf("%s.%s reaches the model state: Engine must never touch a CoinState", typ.Name(), typ.Field(i).Name)
			}
		}
	}
}

func jsonArg(c recordedCall, i int, into any) error {
	if i >= len(c.Args) {
		return nil
	}
	return json.Unmarshal(c.Args[i], into)
}

// Model's own lock: prints, settlements and views from several goroutines at once. Run with -race.
func TestModelIsSafeAcrossGoroutines(t *testing.T) {
	m, err := NewModel([]string{"BTC"}, map[string]Calibration{"BTC": {DefaultSigma: 8e-5, SDPct: 0.000144}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				ts := float64(1000 + i)
				switch g {
				case 0:
					m.Observe("BTC", 80000+float64(i), ts)
				case 1:
					m.View("BTC", Market{Ticker: "T", Strike: 80100, Close: 2000}, 80000, ts, nil)
				case 2:
					m.NoteSettlement("BTC", ts, "80010.5")
				default:
					m.Offsets("BTC")
					m.Observe("nothing", 1, ts) // an unknown coin is ignored, not a panic
				}
			}
		}(g)
	}
	wg.Wait()
}

// The drift gate (plan 4.3): absent inputs, a different offset source, or a difference above the
// tolerance close it; and a closed gate stops entries but not exits.
func TestDriftGate(t *testing.T) {
	m, err := NewModel([]string{"BTC"}, map[string]Calibration{"BTC": {DefaultSigma: 8e-5, SDPct: 0.000144}}, 0.001)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		m.Observe("BTC", 80000+float64(i), float64(1000+i))
	}
	mk := Market{Ticker: "T", Strike: 80020, Close: 1500}
	base := m.View("BTC", mk, 80029, 1030, nil)
	if !base.OK || !base.Drift || base.DriftWhy != "v2's inputs are absent" {
		t.Fatalf("absent inputs must close the gate: %+v", base)
	}
	same := &V2Inputs{Sigma2: base.Sigma2, IndexOffset: base.Offset, OffsetSource: base.OffsetSource}
	if v := m.View("BTC", mk, 80029, 1030, same); v.Drift || v.DriftDiff != 0 {
		t.Fatalf("identical inputs must leave the gate open: %+v", v)
	}
	other := *same
	other.OffsetSource = "measured"
	if v := m.View("BTC", mk, 80029, 1030, &other); !v.Drift || v.DriftWhy != "offset source differs" {
		t.Fatalf("a different offset source must close the gate: %+v", v)
	}
	far := *same
	far.Sigma2 *= 4
	if v := m.View("BTC", mk, 80029, 1030, &far); !v.Drift || v.DriftWhy != "p_model differs" || !(v.DriftDiff > 0.001) {
		t.Fatalf("a different volatility must close the gate: %+v", v)
	}
	if _, err := NewModel(nil, nil, -1); err == nil {
		t.Error("a negative tolerance must be refused")
	}

	// Closed gate: no entry, blocked_by "model drift"; an exit still goes.
	a := NewAccount(testScalper(t, 1, 0.007, 0.006), 1, 100000, true)
	h := newHarness(t, a)
	market := Market{Ticker: "D", MarketID: 7, Strike: 1, Close: 1000}
	drifting := view("DOGE", 0.16)
	drifting.Drift, drifting.DriftWhy = true, "p_model differs"
	decisions, intents, _, _ := h.step("DOGE", market, dogeBook, drifting, 500)
	if len(intents) != 0 || len(decisions) != 1 || decisions[0].BlockedBy != BlockedDrift {
		t.Fatalf("a closed gate must block the entry: %+v", decisions)
	}
	h.step("DOGE", market, dogeBook, view("DOGE", 0.16), 501) // buys 58 yes
	if a.Position("D", "yes") == nil {
		t.Fatal("the entry with the gate open did not fill")
	}
	rich := book([][2]string{{"0.9000", "500"}}, [][2]string{{"0.0900", "500"}})
	drifting.PModel = 0.99
	_, intents, _, _ = h.step("DOGE", market, rich, drifting, 510)
	if len(intents) != 1 || intents[0].Order.Action != broker.Sell {
		t.Fatalf("an exit must go on while the gate is closed: %+v", intents)
	}
}
