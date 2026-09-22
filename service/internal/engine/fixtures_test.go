package engine

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/broker"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	k2 "github.com/doipster/asset_cracker/service/internal/legacy/kalshi15m2"
)

// PLACEHOLDER NUMBERS. Nothing in this file is a measurement. lambda, the two staleness costs and
// the drift tolerance are measured under docs/v3-measurement-protocol.md and that has not been
// run; the values the tests pass in are chosen to exercise the arithmetic (0.007 is the plan's
// own ILLUSTRATIVE figure for its worked example) and are labelled "placeholder", which
// Params.Validate refuses. They reach an engine only through NewPlumbingEngine.
func placeholder(lambda, stale, staleSell, driftTol float64) Measured {
	ph := func(v float64) Provenance {
		return Provenance{Kind: KindPlaceholder, Value: v, Note: "PLACEHOLDER for tests: not a measurement"}
	}
	return Measured{Lambda: ph(lambda), StaleCost: ph(stale), StaleCostSell: ph(staleSell), DriftTol: ph(driftTol)}
}

func testScalper(t *testing.T, lambda, stale, staleSell float64) Params {
	t.Helper()
	p, err := PlumbingScalper(placeholder(lambda, stale, staleSell, 0))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testValue(t *testing.T, lambda, stale float64) Params {
	t.Helper()
	p, err := PlumbingValue(placeholder(lambda, stale, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testEngine(t *testing.T, accounts ...*Account) *Engine {
	t.Helper()
	e, err := NewPlumbingEngine(accounts...)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The recorded DOGE book of plan 2.4 (db_samples.txt, market 102, 2026-09-21 07:41:28Z).
var dogeBook = kalshi.Quotes{
	YesBids: [][2]string{{"0.1000", "43"}, {"0.0990", "100"}, {"0.0930", "1"}, {"0.0920", "23"}, {"0.0910", "1"}},
	NoBids:  [][2]string{{"0.8800", "161"}, {"0.8700", "23.62"}, {"0.8600", "28.28"}, {"0.8500", "210.84"}, {"0.8400", "54.92"}},
}

func book(yes, no [][2]string) kalshi.Quotes { return kalshi.Quotes{YesBids: yes, NoBids: no} }

// view is a model view saying p_model, with the drift gate open.
func view(coin string, pModel float64) View {
	return View{Coin: coin, OK: true, PModel: pModel, OffsetSource: "measured", HasRef: true}
}

// harness is an engine in front of a real Paper, stepping the way the runner will: observe the
// book, decide, submit, and (the write being taken as committed) apply and commit.
type harness struct {
	t      *testing.T
	engine *Engine
	paper  *broker.Paper
	evalID int64
}

func newHarness(t *testing.T, accounts ...*Account) *harness {
	return &harness{t: t, engine: testEngine(t, accounts...), paper: broker.NewPaper(5)}
}

func (h *harness) step(coin string, m Market, q kalshi.Quotes, v View, now float64) ([]Decision, []Intent, []broker.Report, []Event) {
	h.t.Helper()
	return h.stepWith(coin, m, q, v, now, nil)
}

// stepWith is step with a hand on the orders between the decision and the broker: tamper may
// change an intent's order in place, or the book the Paper holds, so that the Paper can be made
// to give answers the engine's own orders never draw (a stale snapshot, a crossed book, a closed
// market). The report echoes the order as sent, so Apply still pairs it with its intent.
func (h *harness) stepWith(coin string, m Market, q kalshi.Quotes, v View, now float64, tamper func(m Market, intents []Intent)) ([]Decision, []Intent, []broker.Report, []Event) {
	h.t.Helper()
	h.evalID++
	m.EvaluationID = h.evalID
	h.paper.ObserveBook(m.Ticker, h.evalID, unixTime(now), unixTime(m.Close), q)
	decisions, intents := h.engine.Decide(coin, m, q, v, now)
	h.engine.AfterDecide(m.Ticker, decisions)
	if tamper != nil {
		tamper(m, intents)
	}
	var reports []broker.Report
	var ids []string
	for _, in := range intents {
		r, err := h.paper.Submit(context.Background(), in.Order)
		if err != nil {
			h.t.Fatal(err)
		}
		reports, ids = append(reports, r), append(ids, in.Order.ClientID)
	}
	events := h.engine.Apply(intents, reports)
	h.paper.Commit(ids...)
	for _, ev := range events {
		if ev.Kind == "inconsistent" {
			h.t.Fatalf("inconsistent: %+v", ev)
		}
	}
	return decisions, intents, reports, events
}

// cloneAccounts is a deep copy, for proving that Decide changed nothing.
func cloneAccounts(in []*Account) []*Account {
	out := make([]*Account, len(in))
	for i, a := range in {
		c := *a
		c.Params.Provenance = map[string]Provenance{}
		for k, v := range a.Params.Provenance {
			c.Params.Provenance[k] = v
		}
		c.Positions = map[posKey]*Position{}
		for k, p := range a.Positions {
			cp := *p
			c.Positions[k] = &cp
		}
		c.Windows = map[int64]*Window{}
		for k, w := range a.Windows {
			cw := *w
			c.Windows[k] = &cw
		}
		out[i] = &c
	}
	return out
}

// ---- the recorded parity fixtures (tools/parity), shared by the fork and the purity tests -----

type recordedCall struct {
	T    float64           `json:"t"`
	Call string            `json:"call"`
	Args []json.RawMessage `json:"args"`
	Meta *struct {
		Engine string                    `json:"engine"`
		Coins  []string                  `json:"coins"`
		Assets map[string]k2.Calibration `json:"assets"`
	} `json:"meta"`
}

func loadCalls(t *testing.T, fixture string) []recordedCall {
	t.Helper()
	path := filepath.Join("..", "..", "..", "tools", "parity", "fixtures", fixture, "calls.jsonl.gz")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("the parity fixture is not here: %v", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var out []recordedCall
	for sc.Scan() {
		var c recordedCall
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 || out[0].Meta == nil || out[0].Meta.Engine != "v2" {
		t.Fatalf("%s is not a v2 recording", fixture)
	}
	return out
}

// replay feeds one recording to the second engine and to this package's Model, call for call.
type replay struct {
	now    float64
	trader *k2.Trader
	model  *Model
}

func newReplay(t *testing.T, head recordedCall, driftTol float64) *replay {
	t.Helper()
	r := &replay{now: head.T}
	r.trader = k2.NewTrader(func() float64 { return r.now }, head.Meta.Coins, head.Meta.Assets)
	cal := map[string]Calibration{}
	for name, c := range head.Meta.Assets {
		cal[name] = Calibration{OffsetPct: c.OffsetPct, SDPct: c.SDPct, DefaultSigma: c.DefaultSigma, Decimals: c.Decimals}
	}
	var err error
	if r.model, err = NewModel(head.Meta.Coins, cal, driftTol); err != nil {
		t.Fatal(err)
	}
	return r
}

type recordedStep struct {
	Coin   string
	Market k2.Market
	Price  float64
	Now    float64
}

// feed applies one call to both. For a step it returns the step; v2's Step has then already run,
// so v2's view of the coin is the one for this second.
func (r *replay) feed(t *testing.T, c recordedCall) *recordedStep {
	t.Helper()
	r.now = c.T
	a := c.Args
	num := func(i int) float64 { var v float64; _ = json.Unmarshal(a[i], &v); return v }
	str := func(i int) string { var v string; _ = json.Unmarshal(a[i], &v); return v }
	switch c.Call {
	case "observe":
		r.trader.Observe(str(0), num(1), num(2))
		r.model.Observe(str(0), num(1), num(2))
	case "seed_vol":
		var closes []float64
		_ = json.Unmarshal(a[1], &closes)
		r.trader.SeedVol(str(0), closes)
		r.model.SeedVol(str(0), closes)
	case "seed_offsets":
		var pairs [][2]float64
		_ = json.Unmarshal(a[1], &pairs)
		r.trader.SeedOffsets(str(0), pairs)
		r.model.SeedOffsets(str(0), pairs)
	case "note_settlement":
		var final any
		_ = json.Unmarshal(a[2], &final)
		r.trader.NoteSettlement(str(0), num(1), final)
		r.model.NoteSettlement(str(0), num(1), final)
	case "step":
		s := &recordedStep{Coin: str(0), Price: num(2), Now: num(3)}
		if err := json.Unmarshal(a[1], &s.Market); err != nil {
			t.Fatal(err)
		}
		r.trader.Step(s.Coin, s.Market, s.Price, s.Now)
		return s
	}
	return nil
}

// v2Inputs is what Runner2.Inputs would publish for the coin right now.
func (r *replay) v2Inputs(coin string) *V2Inputs {
	c := r.trader.Coins[coin]
	offset, samples := c.OffsetStatus()
	source := "measured"
	if samples < 3 {
		source = "constant"
	}
	return &V2Inputs{Sigma2: c.Sigma2, IndexOffset: offset, OffsetSource: source}
}

// topOfBook turns a recording's touch (it kept no depth) into a one-level book. A Yes ask IS a
// No bid at one minus it, and its size is that bid's size.
func topOfBook(m k2.Market) kalshi.Quotes {
	price := func(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
	size := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	var q kalshi.Quotes
	if m.YesBid > 0 && m.NoAskSize > 0 {
		q.YesBids = [][2]string{{price(m.YesBid), size(m.NoAskSize)}}
	}
	if m.NoBid > 0 && m.YesAskSize > 0 {
		q.NoBids = [][2]string{{price(m.NoBid), size(m.YesAskSize)}}
	}
	return q
}

func marketID(ticker string) int64 {
	var h int64
	for _, c := range ticker {
		h = h*31 + int64(c)
	}
	if h < 0 {
		h = -h
	}
	return h%1_000_000 + 1
}
