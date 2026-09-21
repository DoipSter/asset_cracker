package kalshi

import "testing"

func TestBookQuotes(t *testing.T) {
	// Shape and values taken from a live KXBTC15M book on 2026-09-20.
	yes := [][]string{{"0.0010", "142952.11"}, {"0.0040", "328.45"}, {"0.0020", "0.00"}}
	no := [][]string{{"0.0010", "722007.19"}, {"0.9950", "834.15"}, {"0.0030", "16199.00"}}
	q, err := BookQuotes(yes, no)
	if err != nil {
		t.Fatal(err)
	}
	want := Quotes{YesBid: "0.0040", YesAsk: "0.0050", NoBid: "0.9950", NoAsk: "0.9960",
		YesAskSize: "834.15", NoAskSize: "328.45", YesLevels: 2, NoLevels: 3}
	if q != want {
		t.Errorf("got %+v\nwant %+v", q, want)
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
