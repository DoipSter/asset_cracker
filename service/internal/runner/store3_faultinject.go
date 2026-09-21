//go:build faultinject

package runner

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// FaultInjectionBuilt says whether this binary can inject database faults into the third engine.
// THIS IS THE DEV-ONLY BUILD (go build -tags faultinject). It must never be deployed to prod:
// deploy.sh builds without the tag, and /api/status shows this flag in the v3 block.
const FaultInjectionBuilt = true

// WrapStore3 puts the fault injector in front of the store when faults were asked for.
//
// Why it exists: the dev and the production databases share one Postgres cluster, so "stop
// Postgres and see what v3 does" would stop production too. Every database failure the third
// runner has a rule for is therefore produced here, in the client, against the third engine's
// calls ONLY: the first two engines hold the plain *store.Store and never pass through this.
func WrapStore3(db Store3, f Faults) Store3 {
	if !f.On() {
		return db
	}
	if f.Run < 1 {
		f.Run = 1
	}
	if f.Kind == "" {
		f.Kind = "refuse"
	}
	if f.Kind == "delay" && f.DelayCommit <= 0 {
		f.DelayCommit = 3 * time.Second // TEST value: past the runner's 2 s write budget
	}
	ops := map[string]bool{}
	for _, op := range f.Ops {
		ops[op] = true
	}
	if len(ops) == 0 {
		ops["orders"], ops["settlements"] = true, true
	}
	slog.Warn("FAULT INJECTION IS ON for the third engine's database calls: a dev-only build", "every", f.Every, "run", f.Run, "kind", f.Kind,
		"delay_commit", f.DelayCommit.String(), "ops", f.Ops)
	return &faultStore{Store3: db, f: f, ops: ops}
}

type faultStore struct {
	Store3
	f   Faults
	ops map[string]bool

	mu    sync.Mutex
	calls int
	left  int // failures still to come in the current run
}

// trip counts one call of kind op and says whether it is to fail.
func (s *faultStore) trip(op string) bool {
	if !s.ops[op] {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.left == 0 && s.calls%s.f.Every == 0 {
		s.left = s.f.Run
	}
	if s.left > 0 {
		s.left--
		return true
	}
	return false
}

// refused is what the server's own ERROR answer looks like to store.DefinitelyRolledBack.
func refused(op string) error {
	return &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "XX000", Message: "fault injection: " + op + " refused"}
}

// write runs one write under the chosen fault.
func (s *faultStore) write(ctx context.Context, op string, do func(context.Context) error) error {
	if !s.trip(op) {
		return do(ctx)
	}
	slog.Warn("fault injection: failing a v3 write", "op", op, "kind", s.f.Kind)
	switch s.f.Kind {
	case "unknown":
		return context.DeadlineExceeded
	case "lose":
		if err := do(context.WithoutCancel(ctx)); err != nil {
			return err
		}
		return context.DeadlineExceeded
	case "delay":
		go func() {
			time.Sleep(s.f.DelayCommit)
			dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			slog.Warn("fault injection: the delayed v3 write is being made now", "op", op, "err", do(dctx))
		}()
		<-ctx.Done() // the caller waits out its own budget, as it would on a slow server
		return ctx.Err()
	}
	return refused(op)
}

func (s *faultStore) RecordOrders(ctx context.Context, setup store.SimSetup, r store.StepRecord3) error {
	return s.write(ctx, "orders", func(c context.Context) error { return s.Store3.RecordOrders(c, setup, r) })
}

func (s *faultStore) RecordSettlements(ctx context.Context, setup store.SimSetup, marketID int64, at time.Time, rows []store.SettlementRow) error {
	if len(rows) == 0 {
		return s.Store3.RecordSettlements(ctx, setup, marketID, at, rows)
	}
	return s.write(ctx, "settlements", func(c context.Context) error { return s.Store3.RecordSettlements(c, setup, marketID, at, rows) })
}

// The reads the rebuild and the settlement path lean on. A failed read is a connection error: it
// is never the server's ERROR, so nothing is inferred from it.

func (s *faultStore) BucketCash(ctx context.Context, ids []int64) (map[int64]int64, error) {
	if s.trip("reads") {
		return nil, context.DeadlineExceeded
	}
	return s.Store3.BucketCash(ctx, ids)
}

func (s *faultStore) BucketFills(ctx context.Context, ids []int64) ([]store.BucketFill, error) {
	if s.trip("reads") {
		return nil, context.DeadlineExceeded
	}
	return s.Store3.BucketFills(ctx, ids)
}

func (s *faultStore) OpenQty(ctx context.Context, marketID int64, ids []int64) (map[[2]string]int, error) {
	if s.trip("reads") {
		return nil, context.DeadlineExceeded
	}
	return s.Store3.OpenQty(ctx, marketID, ids)
}

func (s *faultStore) SettlementsRecorded(ctx context.Context, marketID int64, ids []int64) (map[[2]string]bool, error) {
	if s.trip("reads") {
		return nil, context.DeadlineExceeded
	}
	return s.Store3.SettlementsRecorded(ctx, marketID, ids)
}
