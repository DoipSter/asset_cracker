package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// The trade stream carries every tracked product's prints, but only the seeded products are
// recorded: a print of a watched-only coin must not reach the batch, where its missing
// instrument id would fail the insert and lose the seeded prints with it.
func TestOnlySeededPrintsAreRecorded(t *testing.T) {
	var got [][]store.Tick
	insert := func(_ context.Context, b []store.Tick) error {
		got = append(got, append([]store.Tick{}, b...))
		return nil
	}
	in := make(chan coinbase.Trade, 8)
	now := time.Now()
	for _, p := range []string{"BTC-USD", "AMP-USD", "ETH-USD", "AAVE-USD"} {
		in <- coinbase.Trade{Product: p, At: now, ReceivedAt: now, Price: "1", Size: "1"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var written atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		writeTicks(ctx, insert, map[string]int64{"BTC-USD": 1, "ETH-USD": 2}, in, &written)
	}()
	time.Sleep(50 * time.Millisecond) // the four prints are taken up
	cancel()
	<-done
	if len(got) != 1 || len(got[0]) != 2 || got[0][0].InstrumentID != 1 || got[0][1].InstrumentID != 2 || written.Load() != 2 {
		t.Fatalf("batches %+v, written %d; want one batch of the two seeded prints", got, written.Load())
	}
}
