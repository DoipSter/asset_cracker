package kalshi

import (
	"reflect"
	"testing"
)

func TestBookQuotes(t *testing.T) {
	// Shape and values taken from a live KXBTC15M book on 2026-09-20.
	yes := [][]string{{"0.0010", "142952.11"}, {"0.0040", "328.45"}, {"0.0020", "0.00"}}
	no := [][]string{{"0.0010", "722007.19"}, {"0.9950", "834.15"}, {"0.0030", "16199.00"}}
	q, err := BookQuotes(yes, no)
	if err != nil {
		t.Fatal(err)
	}
	want := Quotes{YesBid: "0.0040", YesAsk: "0.0050", NoBid: "0.9950", NoAsk: "0.9960",
		YesAskSize: "834.15", NoAskSize: "328.45", YesLevels: 2, NoLevels: 3,
		YesBids:     [][2]string{{"0.0040", "328.45"}, {"0.0010", "142952.11"}},
		NoBids:      [][2]string{{"0.9950", "834.15"}, {"0.0030", "16199.00"}, {"0.0010", "722007.19"}},
		YesBidDepth: Depth{C1: 143280.56, C3: 143280.56, C5: 143280.56}, // both Yes bids sit within a cent of each other
		NoBidDepth:  Depth{C1: 834.15, C3: 834.15, C5: 834.15},          // the next No bid is 99c away
	}
	if !reflect.DeepEqual(q, want) {
		t.Errorf("got %+v\nwant %+v", q, want)
	}
}

// Depth is cumulative: what is bid within 1c is also within 3c and 5c.
func TestDepthIsCumulative(t *testing.T) {
	q, err := BookQuotes([][]string{{"0.5000", "10"}, {"0.4900", "20"}, {"0.4750", "30"}, {"0.4500", "40"}, {"0.4000", "50"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Depth{C1: 30, C3: 60, C5: 100}); q.YesBidDepth != want {
		t.Errorf("got %+v, want %+v", q.YesBidDepth, want)
	}
	if len(q.YesBids) != 5 || q.YesBids[0] != [2]string{"0.5000", "10"} {
		t.Errorf("top levels: %v", q.YesBids)
	}
}

func TestBookQuotesEmptySide(t *testing.T) {
	q, err := BookQuotes(nil, [][]string{{"0.4000", "10"}})
	if err != nil {
		t.Fatal(err)
	}
	if q.YesAsk != "0.6000" || q.NoAsk != "0.0000" || q.YesBid != "0.0000" {
		t.Errorf("got %+v", q)
	}
}

func TestParsePriceRejectsFinerThanScale(t *testing.T) {
	if _, err := parsePrice("0.12345"); err == nil {
		t.Error("expected an error for a price finer than the scale")
	}
}
