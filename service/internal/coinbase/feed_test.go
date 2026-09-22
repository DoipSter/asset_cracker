package coinbase

import (
	"reflect"
	"testing"
)

func TestSubscriptionDiff(t *testing.T) {
	current := map[string]bool{"BTC-USD": true, "ETH-USD": true, "SOL-USD": true}
	add, drop := subscriptionDiff(current, []string{"ETH-USD", "AMP-USD", "BTC-USD", "AAVE-USD"})
	if !reflect.DeepEqual(add, []string{"AAVE-USD", "AMP-USD"}) || !reflect.DeepEqual(drop, []string{"SOL-USD"}) {
		t.Fatalf("add %v drop %v", add, drop)
	}
	if add, drop := subscriptionDiff(current, []string{"BTC-USD", "ETH-USD", "SOL-USD"}); add != nil || drop != nil {
		t.Fatalf("nothing changed: add %v drop %v", add, drop)
	}
}
