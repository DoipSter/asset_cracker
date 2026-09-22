package catalogue

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// Trimmed from GET /series?category=Crypto on 2026-09-21 (274 series): fields as sent.
const seriesBody = `{"series":[
 {"category":"Crypto","categories":["Crypto"],"frequency":"hourly","tags":["Hourly","BTC"],"ticker":"KXBTCD","title":"Bitcoin price Above/below","fee_type":"quadratic"},
 {"category":"Crypto","frequency":"fifteen_min","tags":["BTC","15 min"],"ticker":"KXBTC15M","title":"Bitcoin price up or down in next 15 mins"},
 {"category":"Crypto","frequency":"fifteen_min","tags":["15 min","15 Min Markets"],"ticker":"KXCRYPTOLEAD15M","title":"Crypto lead"},
 {"category":"Crypto","frequency":"daily","tags":null,"ticker":"KXBTCD","title":"a repeat"},
 {"category":"Crypto","frequency":"daily","ticker":"  ","title":"no ticker"},
 {"category":"Crypto","frequency":"daily","ticker":"KXODD","tags":"not a list"}
]}`

// Trimmed from GET https://api.exchange.coinbase.com/products on 2026-09-21 (838 products, 403 USD online).
const productsBody = `[
 {"id":"SOL-USD","base_currency":"SOL","quote_currency":"USD","status":"online","display_name":"SOL-USD","trading_disabled":false},
 {"id":"BTC-USD","base_currency":"BTC","quote_currency":"USD","status":"online","display_name":"BTC-USD"},
 {"id":"BTC-EUR","base_currency":"BTC","quote_currency":"EUR","status":"online","display_name":"BTC-EUR"},
 {"id":"OLD-USD","base_currency":"OLD","quote_currency":"USD","status":"delisted","display_name":"OLD-USD"},
 {"id":"BTC-USD","base_currency":"BTC","quote_currency":"USD","status":"online","display_name":"repeat"}
]`

func TestParseSeries(t *testing.T) {
	got, skipped, err := ParseSeries([]byte(seriesBody))
	if err != nil {
		t.Fatal(err)
	}
	var tickers []string
	for _, s := range got {
		tickers = append(tickers, s.Ticker)
	}
	if !reflect.DeepEqual(tickers, []string{"KXBTCD", "KXBTC15M", "KXCRYPTOLEAD15M"}) || skipped != 3 {
		t.Errorf("tickers %v, skipped %d; want KXBTCD KXBTC15M KXCRYPTOLEAD15M, 3 skipped", tickers, skipped)
	}
	if got[0].Frequency != "hourly" || got[0].Title != "Bitcoin price Above/below" || !reflect.DeepEqual(got[0].Tags, []string{"Hourly", "BTC"}) {
		t.Errorf("first series %+v", got[0])
	}
	if !strings.Contains(string(got[0].Raw), `"fee_type":"quadratic"`) {
		t.Errorf("raw is not the entry as sent: %s", got[0].Raw)
	}
	if _, _, err := ParseSeries([]byte(`{"cursor":""}`)); err == nil {
		t.Error("a list with no series field parsed")
	}
	if _, _, err := ParseSeries([]byte(`<html>`)); err == nil {
		t.Error("a page that is not JSON parsed")
	}
}

func TestParseProductsKeepsUSDOnline(t *testing.T) {
	got, err := ParseProducts([]byte(productsBody))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	if !reflect.DeepEqual(ids, []string{"BTC-USD", "SOL-USD"}) {
		t.Errorf("products %v, want BTC-USD SOL-USD", ids)
	}
	if got[0].DisplayName != "BTC-USD" || got[0].Base != "BTC" {
		t.Errorf("the first copy of a repeated product should be kept: %+v", got[0])
	}
	it := CoinbaseItem(got[1])
	if it.Recorder != RecorderCandles || it.WhyNot != "" || it.Frequency != "continuous" || !it.Checked || it.Source != Coinbase {
		t.Errorf("a USD product is recordable by candles: %+v", it)
	}
	if _, err := ParseProducts([]byte(`{"message":"rate limited"}`)); err == nil {
		t.Error("an error object parsed as a product list")
	}
}

// The shapes below were read from the public API on 2026-09-21.
func TestClassifyKalshi(t *testing.T) {
	up := func(closes string) Market {
		return Market{StrikeType: "greater_or_equal", HasFloor: true, Closes: closes}
	}
	gt := Market{StrikeType: "greater", HasFloor: true, Closes: "2026-09-22T02:00:00Z"}
	cases := []struct {
		name, frequency string
		open            []Market
		readErr         error
		recorder        string
		why             string
	}{
		{"KXBTC15M: one greater_or_equal market with a floor", "fifteen_min", []Market{up("05:15")}, nil, RecorderRound, ""},
		{"next round listed early", "fifteen_min", []Market{up("05:15"), up("05:30")}, nil, RecorderRound, ""},
		{"KXCRYPTOLEAD15M: five custom markets, one close, no floor", "fifteen_min",
			[]Market{{StrikeType: "custom", Closes: "x"}, {StrikeType: "custom", Closes: "x"}}, nil, "", "strike types: custom"},
		{"two markets closing together", "fifteen_min", []Market{up("05:15"), up("05:15")}, nil, "", "not one above/below market"},
		{"KXCRYPTOCOMP15M: nothing open", "fifteen_min", nil, nil, "", "no open markets"},
		{"KXBTCD: every market greater", "hourly", []Market{gt, gt, gt}, nil, RecorderLadder, ""},
		{"a 15-minute ladder", "fifteen_min", []Market{gt, gt}, nil, RecorderLadder, ""},
		{"KXBTC: greater, less and between", "hourly",
			[]Market{gt, {StrikeType: "less"}, {StrikeType: "between", HasFloor: true}}, nil, "", "strike type between, greater, less"},
		{"a floor-less greater_or_equal daily", "daily", []Market{{StrikeType: "greater_or_equal"}}, nil, "", "greater_or_equal"},
		{"open markets unreadable", "daily", nil, errors.New("HTTP 429"), "", "could not be read"},
	}
	for _, c := range cases {
		it := ClassifyKalshi(Series{Ticker: "KXT", Title: "t", Frequency: c.frequency}, c.open, c.readErr)
		if it.Recorder != c.recorder || !strings.Contains(it.WhyNot, c.why) {
			t.Errorf("%s: recorder %q why %q; want %q, why containing %q", c.name, it.Recorder, it.WhyNot, c.recorder, c.why)
		}
		// migration 0018: check ((recorder = '') = (why_not <> ''))
		if (it.Recorder == "") != (it.WhyNot != "") {
			t.Errorf("%s: recorder %q with why_not %q breaks the table's check", c.name, it.Recorder, it.WhyNot)
		}
		if it.Checked != (c.readErr == nil) || it.What == "" || it.Source != Kalshi {
			t.Errorf("%s: %+v", c.name, it)
		}
	}
}

func TestBuild(t *testing.T) {
	var asked []string
	f := Fetchers{
		SeriesList: func(_ context.Context, category string) ([]byte, error) {
			switch category {
			case "Crypto":
				return []byte(seriesBody), nil
			case "Also":
				return []byte(`{"series":[{"ticker":"KXBTC15M","frequency":"fifteen_min"},{"ticker":"KXNEW","frequency":"weekly"}]}`), nil
			}
			return nil, errors.New("HTTP 500")
		},
		OpenSample: func(_ context.Context, series string) ([]Market, error) {
			asked = append(asked, series)
			if series == "KXNEW" {
				return nil, errors.New("timeout")
			}
			return []Market{{StrikeType: "greater", HasFloor: true, Closes: "a"}}, nil
		},
		Products: func(context.Context) ([]byte, error) { return nil, errors.New("HTTP 503") },
	}
	items, problems := Build(context.Background(), f, []string{"Crypto", "Broken", "Also"})
	var codes []string
	for _, it := range items {
		codes = append(codes, it.Code)
	}
	if !reflect.DeepEqual(codes, []string{"KXBTCD", "KXBTC15M", "KXCRYPTOLEAD15M", "KXNEW"}) {
		t.Errorf("items %v", codes)
	}
	if !reflect.DeepEqual(asked, codes) {
		t.Errorf("open markets asked for %v, want each series once: %v", asked, codes)
	}
	if items[3].Checked || items[3].Recorder != "" {
		t.Errorf("a series whose markets could not be read must be unchecked and not recordable: %+v", items[3])
	}
	msgs := ""
	for _, p := range problems {
		msgs += p.Error() + "\n"
	}
	for _, want := range []string{`"Crypto": 3 entries skipped`, `"Broken": HTTP 500`, "coinbase: HTTP 503"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("problems %q lack %q", msgs, want)
		}
	}
}

func TestUnderlying(t *testing.T) {
	bases := Bases([]string{"BTC-USD", "BCH-USD", "T-USD", "SOL-USD", "BTC-EUR", "-USD"})
	if !reflect.DeepEqual(bases, []string{"BTC", "BCH", "T", "SOL"}) {
		t.Fatalf("bases %v", bases)
	}
	cases := []struct {
		code  string
		tags  []string
		want  string
		known bool
	}{
		{"KXBTC15M", []string{"BTC", "15 min"}, "BTC", true},
		{"KXBTCD", []string{"Hourly", "BTC"}, "BTC", true},
		{"KXBCH15M", []string{"15 min", "15 Min Markets"}, "BCH", true}, // no coin tag: the ticker's stem
		{"KXSOLD", nil, "SOL", true},
		{"KXTON15M", []string{"15 min"}, "", false}, // TON is not a base; T is, and must not be guessed
		{"KXCRYPTOLEAD15M", []string{"15 min"}, "", false},
	}
	for _, c := range cases {
		if got, known := Underlying(c.code, c.tags, bases); got != c.want || known != c.known {
			t.Errorf("Underlying(%s) = %q, %v; want %q, %v", c.code, got, known, c.want, c.known)
		}
	}
}

// The spec each recorder reads: round_seconds 900 for the poller, kind binary_ladder for the ladder
// recorder, kind spot for candles; and every one selected and not traded.
func TestInstrumentFor(t *testing.T) {
	bases := []string{"BNB", "BTC"}
	round, err := InstrumentFor(Item{Code: "KXBNB15M", Recorder: RecorderRound, Raw: json.RawMessage(`{"tags":["BNB","15 min"]}`)}, bases)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(round.Spec)
	var back map[string]any // as app.go and the supervisor read it back from jsonb
	_ = json.Unmarshal(spec, &back)
	if round.Kind != "binary_contract" || round.Underlying != "BNB" || back["round_seconds"] != 900.0 ||
		back["selected"] != true || back["trade"] != false || back["price_from"] != "coinbase:BNB-USD" {
		t.Errorf("15-minute instrument %+v, spec %s", round, spec)
	}
	ladder, _ := InstrumentFor(Item{Code: "KXZZZD", Recorder: RecorderLadder}, bases)
	if ladder.Kind != "binary_ladder" || ladder.Spec["ladder"] != true || ladder.Spec["selected"] != true ||
		ladder.Spec["round_seconds"] != nil || ladder.Spec["price_from"] != nil || ladder.Underlying != "" {
		t.Errorf("ladder instrument %+v", ladder)
	}
	spot, _ := InstrumentFor(Item{Code: "AVAX-USD", Recorder: RecorderCandles, Raw: json.RawMessage(`{"base_currency":"AVAX"}`)}, nil)
	if spot.Kind != "spot" || spot.Underlying != "AVAX" || spot.Spec["selected"] != true || spot.Spec["candles_only"] != true {
		t.Errorf("spot instrument %+v", spot)
	}
	var refused Refused
	if _, err := InstrumentFor(Item{Code: "KXBTC", WhyNot: "mixed strike types"}, bases); !errors.As(err, &refused) || !strings.Contains(refused.Why, "mixed strike types") {
		t.Errorf("a series no recorder follows: err %v, want a refusal saying why", err)
	}
	for kind, want := range map[string]string{"binary_contract": RecorderRound, "binary_ladder": RecorderLadder, "spot": RecorderCandles, "other": ""} {
		if got := RecorderFor(kind); got != want {
			t.Errorf("RecorderFor(%s) = %q, want %q", kind, got, want)
		}
	}
}

func TestDecideAndTheLimit(t *testing.T) {
	round := Item{Code: "KXBNB15M", Recorder: RecorderRound}
	no := Item{Code: "KXBTC", WhyNot: "open markets are strike type between, greater, less"}
	cases := []struct {
		name   string
		it     Item
		st     State
		record bool
		want   Action
		refuse string
	}{
		{"new, under the limit", round, State{Selected: 9}, true, Create, ""},
		{"new, at the limit", round, State{Selected: 10}, true, Nothing, "10 instruments are selected already"},
		{"old, off, under the limit", round, State{Exists: true, Kind: "binary_contract", Selected: 3}, true, Activate, ""},
		{"old, off, at the limit", round, State{Exists: true, Kind: "binary_contract", Selected: 10}, true, Nothing, "switch one off first"},
		{"already on, at the limit", round, State{Exists: true, Active: true, Kind: "binary_contract", Selected: 10}, true, Nothing, ""},
		{"off", round, State{Exists: true, Active: true, Kind: "binary_contract", Selected: 10}, false, Deactivate, ""},
		{"off, already off", round, State{Exists: true, Kind: "binary_contract"}, false, Nothing, ""},
		{"off, never on", round, State{}, false, Nothing, ""},
		{"seeded, on already", round, State{Exists: true, Active: true, Seeded: true, Kind: "binary_contract"}, true, Nothing, ""},
		{"seeded, off: the owner's to switch", round, State{Exists: true, Active: true, Seeded: true, Kind: "binary_contract"}, false, Deactivate, ""},
		{"seeded, back on, over the selected limit: seeded rows do not count", round, State{Exists: true, Seeded: true, Kind: "binary_contract", Selected: 10}, true, Activate, ""},
		{"seeded, the engine trades it: off refused", round, State{Exists: true, Active: true, Seeded: true, Kind: "binary_contract", Depended: "the live engine trades this series"}, false, Nothing, "cannot be switched off: the live engine trades this series"},
		{"seeded, the engine prices from it: off refused", Item{Code: "BTC-USD", Recorder: RecorderCandles}, State{Exists: true, Active: true, Seeded: true, Kind: "spot", Depended: "the live engine prices KXBTC15M from this product"}, false, Nothing, "prices KXBTC15M from this product"},
		{"not recordable", no, State{}, true, Nothing, "not recordable yet: open markets are strike type"},
		{"not recordable any more, still on, off", no, State{Exists: true, Active: true, Kind: "binary_ladder"}, false, Deactivate, ""},
		{"kind changed", round, State{Exists: true, Kind: "binary_ladder"}, true, Nothing, "was recorded as binary_ladder"},
	}
	for _, c := range cases {
		got, err := Decide(c.it, c.st, c.record, 10)
		var refused Refused
		switch {
		case c.refuse == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.refuse != "" && (!errors.As(err, &refused) || !strings.Contains(refused.Why, c.refuse)):
			t.Errorf("%s: err %v, want a refusal containing %q", c.name, err, c.refuse)
		case got != c.want:
			t.Errorf("%s: action %d, want %d", c.name, got, c.want)
		}
	}
}

func TestPlan(t *testing.T) {
	sel := func(id int64, kind string) Selected { return Selected{ID: id, Symbol: "S", Kind: kind} }
	ids := func(ws []Selected) []int64 {
		var out []int64
		for _, w := range ws {
			out = append(out, w.ID)
		}
		return out
	}
	cases := []struct {
		name              string
		running           map[int64]string
		wanted            []Selected
		max               int
		start, stop, left []int64
	}{
		{"nothing", nil, nil, 10, nil, nil, nil},
		{"first selections", nil, []Selected{sel(7, "spot"), sel(3, "binary_contract")}, 10, []int64{3, 7}, nil, nil},
		{"steady", map[int64]string{3: RecorderRound, 7: RecorderCandles}, []Selected{sel(3, "binary_contract"), sel(7, "spot")}, 10, nil, nil, nil},
		{"one switched off", map[int64]string{3: RecorderRound, 7: RecorderCandles}, []Selected{sel(7, "spot")}, 10, nil, []int64{3}, nil},
		{"all switched off", map[int64]string{3: RecorderRound, 9: RecorderLadder}, nil, 10, nil, []int64{3, 9}, nil},
		{"kind changed", map[int64]string{3: RecorderRound}, []Selected{sel(3, "binary_ladder")}, 10, []int64{3}, []int64{3}, nil},
		{"over the limit: oldest first", map[int64]string{5: RecorderRound}, []Selected{sel(9, "spot"), sel(5, "binary_contract"), sel(2, "spot")}, 2,
			[]int64{2}, nil, []int64{9}},
		{"limit lowered below what runs", map[int64]string{2: RecorderCandles, 5: RecorderRound}, []Selected{sel(2, "spot"), sel(5, "binary_contract")}, 1,
			nil, []int64{5}, []int64{5}},
		{"a kind no recorder follows", nil, []Selected{sel(4, "future")}, 10, nil, nil, []int64{4}},
		{"listed twice", nil, []Selected{sel(4, "spot"), sel(4, "spot")}, 10, []int64{4}, nil, nil},
	}
	for _, c := range cases {
		start, stop, left := Plan(c.running, c.wanted, c.max)
		if !reflect.DeepEqual(ids(start), c.start) || !reflect.DeepEqual(stop, c.stop) || !reflect.DeepEqual(ids(left), c.left) {
			t.Errorf("%s: start %v stop %v left %v; want %v %v %v", c.name, ids(start), stop, ids(left), c.start, c.stop, c.left)
		}
	}
}

// Rules is what the store is handed: Decide, then the row InstrumentFor makes.
func TestRules(t *testing.T) {
	rules := Rules(3)
	item := store.CatalogueRow{Source: Kalshi, Code: "KXBNB15M", Recorder: RecorderRound, Checked: true, Raw: []byte(`{"tags":["15 min"]}`)}
	plan, err := rules(store.AssetState{Item: item, Record: true, Selected: 2, Coinbase: []string{"BNB-USD", "BTC-USD"}})
	if err != nil || plan.Action != "create" || plan.Kind != "binary_contract" || plan.Underlying != "BNB" ||
		plan.Spec["round_seconds"] != RoundSeconds || plan.Spec["selected"] != true || plan.Spec["price_from"] != "coinbase:BNB-USD" {
		t.Errorf("create: %+v, %v", plan, err)
	}
	if _, err := rules(store.AssetState{Item: item, Record: true, Selected: 3}); err == nil {
		t.Error("a fourth selection passed a limit of 3")
	}
	if plan, err := rules(store.AssetState{Item: item, Record: false, Exists: true, Active: true, Kind: "binary_contract", Selected: 3}); err != nil || plan.Action != "deactivate" {
		t.Errorf("off: %+v, %v", plan, err)
	}
	if plan, err := rules(store.AssetState{Item: item, Record: true, Exists: true, Kind: "binary_contract", Selected: 1}); err != nil || plan.Action != "activate" {
		t.Errorf("on again: %+v, %v", plan, err)
	}
	if plan, err := rules(store.AssetState{Item: item, Record: false, Exists: true, Active: true, Seeded: true, Kind: "binary_contract"}); err != nil || plan.Action != "deactivate" {
		t.Errorf("a seeded series the engine does not trade is the owner's to switch off: %+v, %v", plan, err)
	}
	if _, err := rules(store.AssetState{Item: item, Record: false, Exists: true, Active: true, Seeded: true, Kind: "binary_contract", Depended: "the live engine trades this series"}); err == nil {
		t.Error("a series the engine trades was switched off")
	}
}
