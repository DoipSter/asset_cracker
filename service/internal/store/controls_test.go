package store

import "testing"

func TestRatesOK(t *testing.T) {
	if err := RatesOK(0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := RatesOK(2000, 1000, 2500, 0); err != nil {
		t.Fatal(err)
	}
	if err := RatesOK(10000, 1, 0, 0); err == nil {
		t.Fatal("rates past 100% were accepted")
	}
	if err := RatesOK(-1, 0, 0, 0); err == nil {
		t.Fatal("a negative rate was accepted")
	}
	if err := RatesOK(10001, 0, 0, 0); err == nil {
		t.Fatal("a rate past 100% was accepted")
	}
}
