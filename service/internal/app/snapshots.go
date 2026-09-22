package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/runner"
	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/doipster/asset_cracker/service/internal/web"
)

// ledgerCapital is the ledger's side of the balance sheet, read at most once a minute. held is
// the ids of the buckets the live engine holds.
func ledgerCapital(db *store.Store, held []int64) func() (store.Capital, bool) {
	var (
		mu   sync.Mutex
		last store.Capital
		at   time.Time
		good bool
	)
	return func() (store.Capital, bool) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) > time.Minute {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c, err := db.ReadCapital(ctx, held)
			if at, good = time.Now(), err == nil; good {
				last = c
			} else {
				slog.Warn("could not read the capital behind the value snapshots", "err", err)
			}
		}
		return last, good
	}
}

// snapshotValues appends one minute's value snapshots: the total and the groups every minute,
// every bucket and coin on each fifth minute of the hour. It refuses to write a row that might
// be wrong, because a wrong row is there for good: the table is append-only, and a gap in the
// chart is honest where a false step is not.
func snapshotValues(ctx context.Context, db *store.Store, src web.Sources, run *runner.Runner3, now time.Time) error {
	if run != nil {
		hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		safely("heal", func() { run.Heal(hctx) })
		cancel()
	}
	v, books, _, fresh := src.Valuation()
	if err := runner.SnapshotRefusal(books, time.Now()); err != nil {
		return err
	}
	if !fresh {
		return fmt.Errorf("the ledger's side of the balance sheet could not be read")
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	at := now.Truncate(time.Second)
	return db.InsertSnapshots(wctx, v.Snapshots(at, at.Minute()%5 == 0))
}

// writeTicks batches trade prints into the database: every second, or sooner when busy.
func writeTicks(ctx context.Context, db *store.Store, products map[string]int64, in <-chan coinbase.Trade, written *atomic.Int64) {
	var batch []store.Tick
	flush := func() {
		if len(batch) == 0 {
			return
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := db.InsertTicks(wctx, batch); err != nil {
			slog.Error("writing ticks", "n", len(batch), "err", err)
		} else {
			written.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case <-t.C:
			flush()
		case tr := <-in:
			batch = append(batch, store.Tick{InstrumentID: products[tr.Product], At: tr.At, ReceivedAt: tr.ReceivedAt, Price: tr.Price, Size: tr.Size})
			if len(batch) >= 500 {
				flush()
			}
		}
	}
}
