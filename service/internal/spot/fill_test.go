package spot

import "testing"

func TestBuyAtLastPrint(t *testing.T) {
	// $10.60 to spend at $100, increment 0.01: one 0.10 lot is $10 + 6c fee, 54c left.
	got, err := Buy("100", "0.01", 1060)
	if err != nil {
		t.Fatal(err)
	}
	if got.Qty != "0.1" || got.PremiumCents != 1000 || got.FeeCents != 6 || got.LeftoverCents != 54 {
		t.Fatalf("buy: %+v", got)
	}

	none, err := Buy("100000", "1", 100)
	if err != nil || none.Qty != "" || none.PremiumCents != 0 || none.LeftoverCents != 100 {
		t.Fatalf("too small for one increment: %+v %v", none, err)
	}

	if _, err := Buy("0", "0.01", 1000); err == nil {
		t.Fatal("a zero price must be refused")
	}
	if _, err := Buy("100", "0.01", 0); err == nil {
		t.Fatal("a zero stake must be refused")
	}
}

func TestSellAtLastPrint(t *testing.T) {
	got, err := Sell("100", "0.01", "0.10")
	if err != nil {
		t.Fatal(err)
	}
	if got.Qty != "0.1" || got.PremiumCents != 1000 || got.FeeCents != 6 || got.LeftoverCents != 0 {
		t.Fatalf("sell: %+v", got)
	}
	if _, err := Sell("100", "0.01", "0.015"); err == nil {
		t.Fatal("qty not a whole increment must be refused")
	}
}

func TestFeeRoundsUp(t *testing.T) {
	if got := feeCents(1); got != 1 {
		t.Fatalf("1c of notional still pays a cent of fee, got %d", got)
	}
	if got := feeCents(0); got != 0 {
		t.Fatalf("no notional, no fee: %d", got)
	}
	if got := feeCents(1000); got != 6 {
		t.Fatalf("60 bps of $10 is 6c, got %d", got)
	}
}

func TestMarkFloorsToCents(t *testing.T) {
	cents, err := Mark("100.009", "0.10")
	if err != nil || cents != 1000 {
		t.Fatalf("mark floors: %d %v", cents, err)
	}
	if _, err := Mark("-1", "1"); err == nil {
		t.Fatal("a negative price must be refused")
	}
}
