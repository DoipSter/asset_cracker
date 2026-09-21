package runner

import (
	"context"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// Store3 is every database call the third engine's runner makes, and nothing else. *store.Store
// satisfies it as it is (the assertion below fails the build if a signature drifts).
//
// Why it is an interface and not the *store.Store the first two runners hold: the third runner's
// whole design is about what it does when a write is refused, times out, or commits after the
// client gave up (plan 5.2 to 5.4), and none of that can be tested by stopping Postgres, because
// the dev and the production databases live in ONE cluster on the Pi. So the failures are made
// here, on this seam: the tests put a fake ledger behind it, and a dev binary built with the tag
// "faultinject" wraps the real store in one that can refuse a write, lose its answer, or land it
// late (store3_faultinject.go). A normal build has no such code in it at all.
type Store3 interface {
	EnsureSimSetup(ctx context.Context, prefix, family string, version int, strategies []string, seedCents int64) (store.SimSetup, error)
	TradableVersions(ctx context.Context, family string, version int) ([]store.VersionRow, error)
	HeldBuckets(ctx context.Context, family string, version int) ([]store.HeldBucket, error)

	BucketCash(ctx context.Context, bucketIDs []int64) (map[int64]int64, error)
	BucketFills(ctx context.Context, bucketIDs []int64) ([]store.BucketFill, error)
	OpenQty(ctx context.Context, marketID int64, bucketIDs []int64) (map[[2]string]int, error)
	MarketResults(ctx context.Context, marketIDs []int64) (map[int64]string, error)
	OrdersRecorded(ctx context.Context, clientIDs []string) (map[string]bool, error)
	SettlementsRecorded(ctx context.Context, marketID int64, bucketIDs []int64) (map[[2]string]bool, error)

	RecordOrders(ctx context.Context, setup store.SimSetup, r store.StepRecord3) error
	RecordSettlements(ctx context.Context, setup store.SimSetup, marketID int64, at time.Time, rows []store.SettlementRow) error
	CloseBucket(ctx context.Context, setup store.SimSetup, b store.SimBucket, reason string, restake bool, life int, seedCents int64) (store.SimBucket, error)

	SaveEngineState(ctx context.Context, series string, state any) error
	LoadEngineState(ctx context.Context, series string, into any) (bool, error)
}

var _ Store3 = (*store.Store)(nil)

// Faults is what a fault-injecting build does to the third engine's database calls. It is read
// from the environment by internal/config and is honoured ONLY by a binary built with the tag
// "faultinject"; in any other build WrapStore3 hands the store back untouched whatever this says.
// Nothing here is a production setting, and every number in it is a TEST value.
type Faults struct {
	// Every is n: every nth call of the chosen kinds is made to fail. 0 switches everything off.
	Every int
	// Run is how many calls in a row fail each time, 1 if unset. Three refusals in a row is what
	// pauses the engine, so Every=50 Run=3 shows a pause and, on the fourth write, the resume.
	Run int
	// Kind is what "fail" means:
	//
	//	refuse  the write never reaches the database and a server ERROR is returned: the certain kind
	//	unknown the write never reaches the database and a deadline error is returned: outcome unknown, and nothing landed
	//	lose    the write is made and COMMITS, and then a deadline error is returned: the answer was lost
	//	delay   the caller gets its deadline error when its own budget runs out, and the write is
	//	        made, and commits, DelayCommit after it was asked for: the late commit of plan 5.2
	Kind string
	// DelayCommit is how long a "delay" write waits before it is really made.
	DelayCommit time.Duration
	// Ops is which calls count: any of "orders", "settlements", "reads". Empty means the two writes.
	Ops []string
}

// On reports whether anything was asked for.
func (f Faults) On() bool { return f.Every > 0 }
