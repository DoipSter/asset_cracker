package app

import (
	"reflect"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// The ladders go to the ladder recorder and to nothing else. run() builds its pollers, runners,
// the health check's rounds and the home page's assets from `instruments`, the first result of
// splitLadders, each keeping only kalshi 'binary_contract' rows; this pins that no ladder is in it.
func TestLaddersAreSplitOffFromEverythingElse(t *testing.T) {
	all := []store.Instrument{
		{ID: 1, Source: "coinbase", Kind: "spot", Symbol: "BTC-USD", Underlying: "BTC", Spec: map[string]any{}},
		{ID: 3, Source: "kalshi", Kind: "binary_contract", Symbol: "KXBTC15M", Underlying: "BTC",
			Spec: map[string]any{"price_from": "coinbase:BTC-USD", "round_seconds": 900.0}},
		{ID: 5, Source: "kalshi", Kind: "binary_contract", Symbol: "KXSOL15M", Underlying: "SOL",
			Spec: map[string]any{"round_seconds": 900.0, "trade": false}},
		{ID: 20, Source: "kalshi", Kind: "binary_ladder", Symbol: "KXBTCD", Underlying: "BTC",
			Spec: map[string]any{"price_from": "coinbase:BTC-USD", "ladder": true, "trade": false}},
		// Marked by spec alone, as a later migration might: still a ladder.
		{ID: 21, Source: "kalshi", Kind: "binary_contract", Symbol: "KXETHD", Underlying: "ETH",
			Spec: map[string]any{"ladder": true, "trade": false}},
	}
	rest, ladders := splitLadders(all)
	var got []string
	for _, in := range rest {
		if isLadder(in) {
			t.Errorf("ladder %s left in the instruments run() builds pollers and runners from", in.Symbol)
		}
		if in.Source == "kalshi" && in.Kind == "binary_contract" { // the pollers', runners' and feeds' filter
			got = append(got, in.Symbol)
		}
	}
	if !reflect.DeepEqual(got, []string{"KXBTC15M", "KXSOL15M"}) {
		t.Errorf("15-minute series = %v, want KXBTC15M KXSOL15M in order", got)
	}
	if len(rest) != 3 || len(ladders) != 2 || ladders[0].Symbol != "KXBTCD" || ladders[1].Symbol != "KXETHD" {
		t.Errorf("rest %d, ladders %v", len(rest), ladders)
	}
}

// The ladder sink can reach the database and its instrument, and no engine: nothing it does can
// call Settled or Step on a runner. A field added here must be justified against that.
func TestLadderSinkHoldsNoEngine(t *testing.T) {
	ty := reflect.TypeOf(ladderSink{})
	allowed := map[reflect.Type]bool{reflect.TypeOf(&store.Store{}): true, reflect.TypeOf(int64(0)): true}
	for i := 0; i < ty.NumField(); i++ {
		if f := ty.Field(i); !allowed[f.Type] {
			t.Errorf("ladderSink.%s is a %s", f.Name, f.Type)
		}
	}
}

// A series or product switched on from the assets page is recorded by the supervisor alone. A
// 15-minute one carries round_seconds 900 like the five, so this pins that Run's own list, which
// feeds the pollers, runners, the trade stream, the health check and the home page's five assets,
// never holds it.
func TestSelectedInstrumentsStayOutOfRunsList(t *testing.T) {
	all := []store.Instrument{
		{ID: 1, Source: "coinbase", Kind: "spot", Symbol: "BTC-USD", Underlying: "BTC", Spec: map[string]any{}},
		{ID: 3, Source: "kalshi", Kind: "binary_contract", Symbol: "KXBTC15M", Underlying: "BTC",
			Spec: map[string]any{"price_from": "coinbase:BTC-USD", "round_seconds": 900.0}},
		{ID: 20, Source: "kalshi", Kind: "binary_ladder", Symbol: "KXBTCD", Underlying: "BTC", Spec: map[string]any{"ladder": true, "trade": false}},
		{ID: 40, Source: "kalshi", Kind: "binary_contract", Symbol: "KXBNB15M", Underlying: "BNB",
			Spec: map[string]any{"round_seconds": 900.0, "selected": true, "trade": false, "price_from": "coinbase:BNB-USD"}},
		{ID: 41, Source: "kalshi", Kind: "binary_ladder", Symbol: "KXZZZD", Spec: map[string]any{"ladder": true, "selected": true, "trade": false}},
		{ID: 42, Source: "coinbase", Kind: "spot", Symbol: "AVAX-USD", Underlying: "AVAX", Spec: map[string]any{"selected": true, "trade": false}},
		// Even one of the five, if a row were ever marked selected, is the supervisor's, not Run's.
		{ID: 43, Source: "kalshi", Kind: "binary_contract", Symbol: "KXETH15M", Spec: map[string]any{"round_seconds": 900.0, "selected": true}},
	}
	rest, ladders := splitLadders(all)
	var got []string
	for _, in := range append(rest, ladders...) {
		got = append(got, in.Symbol)
	}
	if !reflect.DeepEqual(got, []string{"BTC-USD", "KXBTC15M", "KXBTCD"}) {
		t.Errorf("Run's instruments %v, want only the seeded BTC-USD KXBTC15M KXBTCD", got)
	}
}

// The selected 15-minute poller's sink holds no runner and no coin: sink.SaveQuotes and SaveResult
// reach an engine only when both are set.
func TestRecordOnlySinkReachesNoEngine(t *testing.T) {
	s := recordOnlySink(nil, 40, nil, "BNB-USD")
	if s.run != nil || s.coin != "" || s.instrumentID != 40 || s.product != "BNB-USD" {
		t.Errorf("record-only sink %+v", s)
	}
}
