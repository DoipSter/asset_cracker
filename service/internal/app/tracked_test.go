package app

import (
	"testing"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// The home page lists what the instrument table tracks: seeded spot products first, then the
// ones switched on from the assets page, each with its product; nothing that is not a Coinbase
// spot instrument; the widget's presentation for the coins it knew, defaults for the rest.
func TestTrackedAssetsComeFromTheTable(t *testing.T) {
	all := []store.Instrument{
		{ID: 1, Source: "coinbase", Kind: "spot", Symbol: "BTC-USD", Underlying: "BTC", Spec: map[string]any{}},
		{ID: 2, Source: "kalshi", Kind: "binary_contract", Symbol: "KXBTC15M", Underlying: "BTC", Spec: map[string]any{}},
		{ID: 3, Source: "coinbase", Kind: "spot", Symbol: "AMP-USD", Underlying: "AMP", Spec: map[string]any{"selected": true, "trade": false, "candles_only": true}},
		{ID: 4, Source: "coinbase", Kind: "spot", Symbol: "DOGE-USD", Underlying: "DOGE", Spec: map[string]any{}},
		{ID: 5, Source: "kalshi", Kind: "binary_ladder", Symbol: "KXBTCD", Underlying: "BTC", Spec: map[string]any{"selected": true}},
		{ID: 6, Source: "coinbase", Kind: "spot", Symbol: "AAVE-USD", Underlying: "", Spec: map[string]any{"selected": true, "decimals": float64(3)}},
	}
	tr := newTracked(nil, all) // the initial list stands for a minute: no database is asked
	var coins []string
	for _, a := range tr.Assets() {
		coins = append(coins, a.Coin)
	}
	if got := coins; len(got) != 4 || got[0] != "BTC" || got[1] != "DOGE" || got[2] != "AMP" || got[3] != "AAVE" {
		t.Fatalf("assets %v: want the seeded two first, then AMP and AAVE", got)
	}
	if got := tr.Products(); len(got) != 4 || got[2] != "AMP-USD" || got[3] != "AAVE-USD" {
		t.Fatalf("products %v", got)
	}
	as := tr.Assets()
	if as[0].Sign != "₿" || as[0].Decimals != 2 || as[0].Product != "BTC-USD" || as[0].Name != "Bitcoin" {
		t.Errorf("BTC keeps the widget's presentation: %+v", as[0])
	}
	if as[2].Sign != "" || as[2].Colour == "" || as[2].Decimals != 4 || as[2].Product != "AMP-USD" || as[2].Name != "AMP" {
		t.Errorf("AMP gets defaults: %+v", as[2])
	}
	if as[3].Coin != "AAVE" || as[3].Decimals != 3 {
		t.Errorf("AAVE: the coin from the symbol, the precision from the spec: %+v", as[3])
	}
}
