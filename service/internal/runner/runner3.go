package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/doipster/asset_cracker/service/internal/broker"
	"github.com/doipster/asset_cracker/service/internal/coinbase"
	k3 "github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// This file runs the live engine (package engine) in SIMULATION: it has no order code
// and no venue client; what "fills" is decided by the paper broker from the recorded book.
// Design: docs/honest-fills-v3.md, section 5. The rules that shape everything below:
//
//   - The live engine must never be able to halt, stall or mis-record ingest. So nothing
//     that is called from a poller or from the Coinbase stream ever WAITS on r.mu, the lock that
//     is held across a database write: Inputs and Observe touch the model only (which has its own
//     small mutex, held across arithmetic alone), and Step and Settled give up at once if r.mu is
//     busy (TryLock) and count the skip. Every such entry point recovers its own panics and
//     returns nothing to its caller.
//   - Memory is never AHEAD of the ledger: Engine.Apply runs only after RecordOrders returned nil.
//     Memory can be BEHIND it (a write timed out and committed anyway), and then the engine is
//     SUSPENDED: it writes nothing at all, settlements included, until a rebuild from the
//     database has succeeded. There is no permanent halt.
//   - Which buckets the engine holds is decided by ONE function, load: at start, and again at
//     each Reload (the buckets page's reset and its version approvals). Between two loads the
//     held set shrinks only when a bucket that ran out is closed. Every reader that runs without
//     r.mu copies the held set under r.mu first.
//   - "Is held" and "may order" are separate. A held bucket is valued and SETTLED whatever AC_V3
//     or its version's status says; those gate new orders only.

// Coin3 is one coin the third engine looks at: the same shape as Coin2, with the fork's calibration.
type Coin3 struct {
	Coin, Series, Product string
	Cal                   k3.Calibration
}

// Options3 is what NewRunner3 must be told from outside.
type Options3 struct {
	// On is AC_V3 != "off". Off means no new orders; held bets are still settled.
	On bool
	// DatabaseName is the database the service is connected to. Dev plumbing versions are
	// constructed only when it ends in "_dev" (see devPlumbing).
	DatabaseName string
	// Now is the clock, for tests. nil means time.Now.
	Now func() time.Time
	// Family is the strategy family this runner holds, and Prefix the bucket-name prefix its
	// buckets carry (also the key its engine state is saved under). Empty means the 15-minute
	// rounds, kalshi15m / kalshi15m3, as before there were two runners. The ladder runner is
	// the same code over store.FamilyLadders with prefix "kalshiladder3": a second instance, a
	// second Paper, a second held set, one engine package.
	Family, Prefix string
}

// family and prefix are the options with their defaults made explicit.
func (o Options3) family() string {
	if o.Family == "" {
		return family3
	}
	return o.Family
}

func (o Options3) prefix() string {
	if o.Prefix == "" {
		return engine3
	}
	return o.Prefix
}

const (
	engine3  = "kalshi15m3"
	family3  = "kalshi15m"
	version3 = 3

	// DevPlumbingMark is the text tools/dev-v3-plumbing.sql writes into a plumbing version's
	// hypothesis AND, because the frozen store package has no call that reads a hypothesis, into
	// its params under DevPlumbingKey, which is where this runner reads it.
	DevPlumbingMark = "DEV PLUMBING, not a trial"
	DevPlumbingKey  = "dev_plumbing"
)

// Every number here is a CONVENTION, not a measurement. None of them gates a trade: they are
// budgets and retry cadences. S5 measures whether the budgets are enough on the Pi.
const (
	seed3Cents     = 100000           // [CONVENTION] the same $1,000 as v2, so the lines compare: what the engine seeds when nobody chose. A bucket the operator deploys at another figure holds and sizes off its own seed (store.HeldBucket.SeedCents, engine.Account.Seed)
	writeBudget3   = 2 * time.Second  // [CONVENTION] one step's or one settlement's write, its last look included; a failed settlement write's lookup gets a fresh budget of the same size
	healDelay3     = 10 * time.Second // [CONVENTION] no rebuild sooner after an unknown outcome: a late commit must have landed or died
	tick3          = 10 * time.Second // [CONVENTION] Run's cadence: rebuild if suspended, probe if paused
	sweepEvery3    = time.Minute      // [CONVENTION] the sweep and the cash check
	rebuildBudget3 = 8 * time.Second  // [CONVENTION] one rebuild's reads, when Run makes it
	startBudget3   = 30 * time.Second // [CONVENTION] the first rebuild and the start-up sweep, together
	pauseAfter3    = 3                // [CONVENTION] refused writes in a row before v3 pauses
	thinSeconds3   = 15               // [INHERITED from runner2.go] an unchanged decision is journaled this often
	markersKept3   = 500              // [CONVENTION] chart markers kept in memory; they do not survive a restart
	notesFor3      = 30 * time.Second // [CONVENTION] how long /api/status reuses its read of the versions' statuses
)

const (
	stateRunning   = "running"
	statePaused    = "paused"
	stateSuspended = "suspended"
)

// bucket3 is one held bucket and what was decided about it by the load that brought it in. A
// bucket3 value is never changed; the SLICE holding them is replaced under r.mu by a Reload or
// shortened by a close, so a reader working outside the lock copies the slice under r.mu first.
type bucket3 struct {
	store.HeldBucket
	params   k3.Params
	mayOrder bool
	whyNot   string // why it may not order, for /api/status
	plumbing bool   // a dev plumbing version, accepted because the database is a *_dev one
}

// seenView is the model view Inputs computed for a coin's market at one second, kept so that
// Step decides on exactly what was journaled with that second's evaluation row.
type seenView struct {
	ticker string
	at     time.Time
	view   k3.View
}

type seenQuotes struct {
	q      kalshi.Quotes
	at     time.Time
	closes float64
}

// rebuildReport is what the last rebuild found, for /api/status.
type rebuildReport struct {
	At     time.Time
	OK     bool
	Took   time.Duration
	Fills  int
	Note   string // whether the step that suspended v3 had committed after all
	Reason string // why it failed, or was thrown away
}

// Runner3 runs the third engine. See the comment at the top of this file for its rules.
type Runner3 struct {
	db      Store3
	setup   store.SimSetup
	opts    Options3
	coins   map[string]Coin3 // by coin
	model   *k3.Model        // its own mutex; never touched under a database call
	buckets []bucket3        // the held set: replaced by a load, shortened by a close; under mu
	ids     []int64          // their ids; under mu likewise

	// What the last load decided about the approved versions, kept so that /api/status can say
	// why a version is not trading instead of guessing. Replaced whole by a load; under mu.
	refused map[int64]string // version id -> why vet refused it at the last load
	seeded  map[int64]bool   // version ids whose names were handed to EnsureSimSetup at the last load

	plumbing   bool // the engine is built with NewPlumbingEngine: dev only; under mu
	feePerFill bool // under mu

	// ordersLive is the buckets-page switch for NEW orders. It starts as opts.On and can
	// change while the process runs. Off makes the next look send nothing. On resumes
	// orders only for buckets this process was built allowed to order; a process that
	// started settle-only stays so until the next start, which reads the saved switch.
	ordersLive atomic.Bool

	// writeBudget is writeBudget3. It is a field only so that a test can make a real deadline
	// expire in milliseconds; nothing in the service changes it.
	writeBudget time.Duration

	// coinMu guards views and nothing else. It is held across a map read or write only.
	coinMu sync.Mutex
	views  map[string]seenView // by coin

	// mu guards everything below. Step and the settlement path hold it across their ledger write.
	// Lock order where both are needed: mu, then coinMu.
	mu         sync.Mutex
	engine     *k3.Engine
	paper      *broker.Paper
	state      string
	reason     string
	since      time.Time
	healAfter  time.Time
	failures   int
	gen        uint64
	pendingIDs []string           // the client ids of the step that suspended v3, for the rebuild's report
	probe      *store.StepRecord3 // a decisions-only record for the write-probe, made while paused
	quotes     map[string]seenQuotes
	last       map[string]string
	lastAt     map[string]float64
	markers    []Marker
	entryPrice map[string]float64 // bucket|ticker|side -> the coin's price at the first buy; lost on restart
	lastSweep  time.Time
	rebuilt    rebuildReport
	hwm        map[int64]int64 // bucket id -> the allocator's high-water mark, in cents

	// panicNote carries a recovered panic from an entry point that may not wait on mu to the next
	// holder of mu, which suspends v3 with it. Book() shows it at once.
	panicNote atomic.Pointer[string]

	// notes caches restartNotes, which asks the database: /api/status may be polled every second.
	notesMu sync.Mutex
	notesAt time.Time
	notes   []string

	skippedStep    atomic.Int64
	skippedSettled atomic.Int64
	stateDirty     atomic.Bool // the learned offsets changed; Run saves them
}

// Family is the market family this runner holds buckets for: store.FamilyRounds or FamilyLadders.
func (r *Runner3) Family() string { return r.opts.family() }

func (r *Runner3) now() time.Time {
	if r.opts.Now != nil {
		return r.opts.Now()
	}
	return time.Now()
}

// devPlumbing reports whether a version's params carry the dev-plumbing mark.
func devPlumbing(params json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(params, &m) != nil {
		return false
	}
	var mark string
	return json.Unmarshal(m[DevPlumbingKey], &mark) == nil && mark == DevPlumbingMark
}

// vet decides whether a version's params may ORDER. A real version must pass Params.Validate,
// which refuses any number that is not labelled with where it came from and any placeholder. A
// version whose numbers are placeholders is accepted in exactly one case: it carries the
// dev-plumbing mark AND the database's name ends in "_dev". Anything else is refused, with why.
func (o Options3) vet(raw json.RawMessage) (p k3.Params, plumbing bool, err error) {
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, false, fmt.Errorf("its params cannot be read: %w", err)
	}
	if p.SeedCents != seed3Cents {
		return p, false, fmt.Errorf("its seed convention is %d cents and the runner's is %d", p.SeedCents, seed3Cents)
	}
	if err = p.Validate(); err == nil {
		return p, false, nil
	}
	if !devPlumbing(raw) {
		return p, false, err
	}
	if !strings.HasSuffix(o.DatabaseName, "_dev") {
		return p, false, fmt.Errorf("it is a dev plumbing version and database %q is not a *_dev one", o.DatabaseName)
	}
	if perr := p.ValidatePlumbing(); perr != nil {
		return p, false, perr
	}
	return p, true, nil
}

// loaded3 is what one load decided: the held set and everything that was read about it. It is
// built outside r.mu and swapped in whole under it (applyLocked).
type loaded3 struct {
	setup      store.SimSetup
	buckets    []bucket3
	ids        []int64
	refused    map[int64]string
	seeded     map[int64]bool
	plumbing   bool
	feePerFill bool
	driftTol   float64
	hwm        map[int64]int64 // bucket id -> high-water mark in cents (allocator)
}

// load is steps 1 and 2 of a start: vet the approved versions, seed a bucket for each (once),
// and read every held bucket. NewRunner3 runs it first; Reload runs it again while the service
// runs. It creates nothing but the buckets EnsureSimSetup seeds for the names it is handed.
func (o Options3) load(ctx context.Context, db Store3, on bool) (*loaded3, error) {
	l := &loaded3{refused: map[int64]string{}, seeded: map[int64]bool{}, hwm: map[int64]int64{}}

	// 1. The names to seed: the approved versions, and only with new orders on. Each is vetted
	//    BEFORE EnsureSimSetup, because that function seeds $1,000 into a bucket for any name it is given.
	ordering := map[int64]bool{} // strategy_version.id -> may order
	plumb := map[int64]bool{}
	var names []string
	if on {
		versions, err := db.TradableVersions(ctx, o.family(), version3)
		if err != nil {
			return nil, fmt.Errorf("v3 tradable versions: %w", err)
		}
		for _, v := range versions {
			_, plumbing, err := o.vet(v.Params)
			if err != nil {
				l.refused[v.ID] = err.Error()
				slog.Error("v3 will NOT trade this version", "strategy", v.Name, "version_id", v.ID, "why", err)
				continue
			}
			ordering[v.ID], plumb[v.ID], l.seeded[v.ID] = true, plumbing, true
			names = append(names, v.Name)
		}
	}
	// ALWAYS, in every mode: a settlement cannot be written without the service actor and the
	// venue's ledger id, and this is the only function that returns them.
	setup, err := db.EnsureSimSetup(ctx, o.prefix(), o.family(), version3, names, seed3Cents)
	if err != nil {
		return nil, fmt.Errorf("v3 sim setup: %w", err)
	}
	l.setup = setup

	// 2. What is held: every sim bucket of the family's version 3 that is not frozen.
	held, err := db.HeldBuckets(ctx, o.family(), version3)
	if err != nil {
		return nil, fmt.Errorf("v3 held buckets: %w", err)
	}
	// The model takes ONE drift tolerance (it is a property of the model, the same for every
	// version): a real version's if there is one, else a plumbing version's, else 0, under which
	// every difference reads as drift. Nothing may order then, and drift_diff is journaled all the same.
	var realTol, plumbTol *float64
	for _, h := range held {
		b := bucket3{HeldBucket: h}
		switch {
		case !on:
			b.whyNot = "new orders are off: settle-only"
		case h.VersionStatus != "probation" && h.VersionStatus != "active":
			b.whyNot = "its version is " + h.VersionStatus + ": settle-only"
		case !h.OrdersOn:
			b.whyNot = "its own orders switch is off: settle-only"
		case !ordering[h.VersionID]:
			b.whyNot = "refused: " + l.refused[h.VersionID]
		default:
			b.mayOrder, b.plumbing = true, plumb[h.VersionID]
		}
		if err := json.Unmarshal(h.Params, &b.params); err != nil || b.params.Name == "" {
			// Holding and settling read only the name and the exhaustion floor.
			b.params = k3.Params{Name: h.Strategy, ExhaustedCents: 100} // [CONVENTION, inherited: v2's BankruptAt]
		}
		if b.mayOrder {
			tol := b.params.DriftTol
			if b.plumbing && plumbTol == nil {
				plumbTol = &tol
			} else if !b.plumbing && realTol == nil {
				realTol = &tol
			}
			l.plumbing = l.plumbing || b.plumbing
			l.feePerFill = l.feePerFill || b.params.FeePerFill // one Paper serves every bucket: the pessimistic reading wins
		}
		// The allocator's mark: the bucket's last recorded high, else what it was seeded with. The
		// seed is the ledger's, because the operator may deploy a bucket at a figure of their own
		// (DeployBucket); a bucket with no seed on record is marked at the convention.
		seed := h.SeedCents
		if seed <= 0 {
			seed = seed3Cents
		}
		mark, err := db.HighWaterMark(ctx, h.ID, seed)
		if err != nil {
			return nil, fmt.Errorf("v3 high-water mark of bucket %d: %w", h.ID, err)
		}
		l.hwm[h.ID] = mark
		l.buckets, l.ids = append(l.buckets, b), append(l.ids, h.ID)
	}
	if realTol != nil {
		l.driftTol = *realTol
	} else if plumbTol != nil {
		l.driftTol = *plumbTol
	}
	return l, nil
}

// applyLocked swaps a load in: the held set, what was decided about the versions, and an EMPTY
// engine and Paper for the rebuild to fill. Everything that named a bucket or a round is
// cleared; the model and the chart markers are kept. Callers hold r.mu and have suspended v3.
func (r *Runner3) applyLocked(l *loaded3) {
	r.setup = l.setup
	r.buckets, r.ids = l.buckets, l.ids
	r.refused, r.seeded = l.refused, l.seeded
	r.plumbing, r.feePerFill = l.plumbing, l.feePerFill
	r.hwm = l.hwm
	r.engine, _ = k3.NewEngine()
	r.paper = r.newPaper()
	r.pendingIDs, r.probe = nil, nil
	r.entryPrice = map[string]float64{}
	r.quotes = map[string]seenQuotes{}
	r.last = map[string]string{}
	r.lastAt = map[string]float64{}
	r.gen++
}

// NewRunner3 decides what the third engine holds and what may order (load), and rebuilds its
// memory from the database. It always returns a runner: with nothing held and orders off the
// engine is observe-only, and the buckets page can still reload it into a trial.
//
// It returns an error, and the service does not start, only when it cannot FIND OUT what it
// holds (plan 5.4, D19): a v3 bucket that may hold a bet must not run unheld.
//
// A first rebuild that fails is not an error: the runner is returned SUSPENDED with its bucket
// ids known, Run heals it, and nothing is swept or closed at this start. A PANIC in the first
// rebuild, the start-up sweep or the close is treated exactly the same way (firstRebuild): the
// same rows that at run time would merely suspend v3 must not crash the process at every start
// and so keep it down in a restart loop (a held bucket is rebuilt whatever the switch says).
func NewRunner3(ctx context.Context, db Store3, coins []Coin3, opts Options3) (*Runner3, error) {
	r := &Runner3{db: db, opts: opts, coins: map[string]Coin3{}, views: map[string]seenView{}, quotes: map[string]seenQuotes{},
		last: map[string]string{}, lastAt: map[string]float64{}, entryPrice: map[string]float64{},
		refused: map[int64]string{}, seeded: map[int64]bool{}, hwm: map[int64]int64{}, writeBudget: writeBudget3,
		state: stateSuspended, reason: "starting: memory has not been rebuilt from the database yet"}
	r.ordersLive.Store(opts.On)
	r.since = r.now()

	l, err := opts.load(ctx, db, opts.On)
	if err != nil {
		return nil, err
	}
	var order []string
	cal := map[string]k3.Calibration{}
	for _, c := range coins {
		order = append(order, c.Coin)
		cal[c.Coin], r.coins[c.Coin] = c.Cal, c
	}
	if r.model, err = k3.NewModel(order, cal, l.driftTol); err != nil {
		return nil, fmt.Errorf("v3 model: %w", err)
	}
	var saved savedState3
	if found, err := db.LoadEngineState(ctx, opts.prefix(), &saved); err != nil {
		slog.Warn("v3 could not load its learned offsets; it starts from the constants", "err", err) // costs no money (plan 5.4, step 6)
	} else if found {
		for coin, offsets := range saved.Offsets {
			r.model.SeedOffsets(coin, offsets)
		}
	}
	r.applyLocked(l) // nothing else can see r yet: no lock needed

	// 3 to 5: the rebuild, the start-up sweep and the close, fenced against a panic.
	sctx, cancel := context.WithTimeout(ctx, startBudget3)
	defer cancel()
	r.firstRebuild(sctx)
	slog.Info("v3 ready", "on", opts.On, "held", len(r.buckets), "may_order", r.mayOrderCount(), "plumbing", r.plumbing, "state", r.state)
	return r, nil
}

// ReloadReport is what a Reload found: how many buckets are held and how many may order.
type ReloadReport struct {
	Held, MayOrder int
}

// Reload is a start without a process start: the buckets page calls it after the simulated
// books were reset, and after a version was approved or retired. It suspends v3, runs load
// again with the orders switch as it is now, swaps the held set in, and rebuilds, sweeps and
// closes as NewRunner3 does. A version approved a moment ago is seeded and trades on the next
// look; a version retired a moment ago holds its bucket settle-only.
//
// While it runs no rebuild can start from the old ids (healAfter is pushed an hour out, as the
// reset does), Step and Settled see "suspended" and do nothing, and Book reports Halted, so the
// value snapshots refuse the minute rather than write a book that is between two loads. If the
// load fails the old held set stays and Tick heals it; if the rebuild fails the new set stays,
// suspended, and Tick heals that. The model keeps its learned offsets and its drift tolerance
// from the first load (the gate is open with no reference engine, so the number decides nothing).
func (r *Runner3) Reload(ctx context.Context) (ReloadReport, error) {
	return r.reload(ctx, "reloading the books", nil, nil)
}

// reload is Reload with two hooks for the operations that change the books between suspending
// and loading. check runs under r.mu before v3 is suspended and may refuse (nothing has changed
// then); between runs while v3 is suspended, before the load, and its writes are what the load
// reads back. Either error leaves the old held set in place; Tick heals a suspension.
func (r *Runner3) reload(ctx context.Context, why string, check func() error, between func(context.Context) error) (report ReloadReport, err error) {
	defer func() {
		if r.caught("reload", recover()) {
			err = errors.New("a panic during the reload; v3 rebuilds from what it holds")
		}
	}()
	r.mu.Lock()
	if check != nil {
		if err := check(); err != nil {
			r.mu.Unlock()
			return report, err
		}
	}
	r.absorbLocked()
	r.suspendLocked(why)
	r.healAfter = r.now().Add(time.Hour)
	r.mu.Unlock()
	done := false
	defer func() {
		if !done {
			r.mu.Lock()
			if r.state == stateSuspended {
				r.healAfter = r.now()
			}
			r.mu.Unlock()
		}
		r.notesMu.Lock()
		r.notes = nil
		r.notesMu.Unlock()
	}()
	if between != nil {
		if err := between(ctx); err != nil {
			return report, err
		}
	}
	l, err := r.opts.load(ctx, r.db, r.ordersLive.Load())
	if err != nil {
		return report, fmt.Errorf("reload: %w", err)
	}
	r.mu.Lock()
	r.applyLocked(l)
	report = ReloadReport{Held: len(r.buckets), MayOrder: r.mayOrderCount()}
	r.mu.Unlock()
	if err := r.rebuild(ctx); err != nil {
		return report, fmt.Errorf("reload: %w", err)
	}
	r.sweep(ctx)
	r.closeExhausted(ctx)
	r.mu.Lock()
	report = ReloadReport{Held: len(r.buckets), MayOrder: r.mayOrderCount()}
	done = r.state != stateSuspended
	r.mu.Unlock()
	slog.Info("v3 reloaded", "held", report.Held, "may_order", report.MayOrder, "on", r.ordersLive.Load())
	return report, nil
}

// ReapReport is what a Reap did.
type ReapReport struct {
	Bucket      string // the bucket closed
	ReapedCents int64  // what it held, now in the common pool
	Next        string // the fresh bucket, when restaked; "" otherwise
	Held        int    // buckets held after the reload
	MayOrder    int    // of them, how many may order
}

// ErrNoHeldBucket is a Reap of a version that holds no bucket.
var ErrNoHeldBucket = errors.New("that version holds no bucket")

// OpenPositions is a Reap refused because the bucket still has a bet on.
type OpenPositions struct {
	Bucket string
	Lots   int
}

func (e OpenPositions) Error() string {
	return fmt.Sprintf("%s has %d open position(s); it is reaped once they settle", e.Bucket, e.Lots)
}

// Reap closes a version's held bucket by the operator's hand: what it holds is reaped into the
// common pool and the bucket is frozen with its whole record; with restake, a fresh life is
// seeded from the pool in its place ("<name> life N"), trading if its version is approved and
// held settle-only if not. It is refused while the bucket has an open position, because
// CloseBucket does not look for one and a settlement into a frozen bucket has nowhere to go.
// With restake and NO held bucket (reaped earlier, or run out) it opens the next life alone.
// The writes happen while v3 is suspended and the load that follows reads them back.
func (r *Runner3) Reap(ctx context.Context, versionID int64, restake bool) (ReapReport, error) {
	var target *bucket3
	var life int
	var cash int64
	check := func() error {
		if r.state == stateSuspended {
			return errors.New("v3 is suspended and its cash is not known; try again after it heals")
		}
		for i := range r.buckets {
			b := &r.buckets[i]
			if b.VersionID != versionID {
				continue
			}
			a := r.engine.Account(b.ID)
			if a == nil {
				return errors.New("the bucket has no account in the engine; try again after a reload")
			}
			if open := a.Open(); len(open) > 0 {
				return OpenPositions{Bucket: b.Name, Lots: len(open)}
			}
			copy := *b
			target, cash, life = &copy, a.CashCents, lifeOf(b.Name)+1
			return nil
		}
		if !restake {
			return ErrNoHeldBucket
		}
		return nil // nothing held: the restake opens the next life on its own
	}
	var next store.SimBucket
	between := func(ctx context.Context) error {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.writeBudget)
		defer cancel()
		var err error
		if target == nil {
			next, err = r.db.RestakeBucket(wctx, r.setup, versionID, seed3Cents)
			if err != nil {
				return fmt.Errorf("restaking version %d: %w", versionID, err)
			}
			return nil
		}
		reason := "reaped by the operator"
		if restake {
			reason = "reaped and restaked by the operator"
		}
		next, err = r.db.CloseBucket(wctx, r.setup, target.SimBucket, reason, restake, life, seed3Cents)
		if err != nil {
			return fmt.Errorf("closing %s: %w", target.Name, err)
		}
		return nil
	}
	rep, err := r.reload(ctx, "reaping a bucket", check, between)
	if err != nil {
		return ReapReport{}, err
	}
	out := ReapReport{Held: rep.Held, MayOrder: rep.MayOrder}
	if target != nil {
		out.Bucket, out.ReapedCents = target.Name, cash
	}
	if restake {
		out.Next = next.Name
	}
	slog.Info("v3 bucket reaped or restaked by the operator", "bucket", out.Bucket, "reaped_cents", out.ReapedCents, "restaked_as", out.Next)
	return out, nil
}

// ClosePlan is a close-out the engine has taken on and finishes itself: the bucket had a position
// open when × was pressed, so it is marked and closed by the pass after its last settlement
// (closeExhaustedLocked), or by the next start.
type ClosePlan struct {
	Bucket string
	Open   int // positions open at the moment it was marked
}

// CloseWhenFlat is Reap without restake for a bucket that may still have a position on. Flat, it
// is reaped now and the report says so. Not flat, it is marked, in the database (RequestClose) and
// in the held set, and the plan says so; the reap follows the last settlement. Every other refusal
// is the error Reap gives.
func (r *Runner3) CloseWhenFlat(ctx context.Context, versionID int64) (ReapReport, *ClosePlan, error) {
	rep, err := r.Reap(ctx, versionID, false)
	var open OpenPositions
	if !errors.As(err, &open) {
		return rep, nil, err
	}
	r.mu.Lock()
	r.absorbLocked()
	var id int64
	for _, b := range r.buckets {
		if b.VersionID == versionID {
			id = b.ID
		}
	}
	r.mu.Unlock()
	if id == 0 {
		return ReapReport{}, nil, err // gone between the two looks: the refusal stands
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.writeBudget)
	defer cancel()
	if werr := r.db.RequestClose(wctx, id); werr != nil {
		return ReapReport{}, nil, fmt.Errorf("marking %s to close once flat: %w", open.Bucket, werr)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.fenceLocked("close when flat")
	r.absorbLocked()
	for i := range r.buckets {
		if r.buckets[i].ID == id {
			r.buckets[i].CloseRequested = true
		}
	}
	slog.Info("v3 bucket marked to close once flat", "bucket", open.Bucket, "open", open.Lots)
	// It may have gone flat while the mark was written: then this is the pass that closes it.
	r.closeExhaustedLocked(ctx)
	return ReapReport{}, &ClosePlan{Bucket: open.Bucket, Open: open.Lots}, nil
}

// DeployReport is what a Deploy did.
type DeployReport struct {
	Bucket    string // the bucket opened
	SeedCents int64
	Held      int // buckets held after the reload
	MayOrder  int // of them, how many may order
}

// Deploy opens a bucket for a version by the operator's hand, seeded with the amount and from
// the source they chose (store.DeployBucket): the first life, or the next after a reap. It is
// refused while the version holds a bucket in this runner, and the store refuses a version of
// another family with store.ErrOtherFamily, so app can ask the next runner. The write happens
// while v3 is suspended and the load that follows reads the bucket back as held; a draft or
// retired version was put on probation with it, so it may order on the next look if it vets.
func (r *Runner3) Deploy(ctx context.Context, d store.Deploy) (DeployReport, error) {
	check := func() error {
		if r.state == stateSuspended {
			return errors.New("v3 is suspended and its books are not known; try again after it heals")
		}
		for i := range r.buckets {
			if r.buckets[i].VersionID == d.VersionID {
				return store.ErrBucketHeld
			}
		}
		return nil
	}
	var next store.SimBucket
	between := func(ctx context.Context) error {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.writeBudget)
		defer cancel()
		var err error
		if next, err = r.db.DeployBucket(wctx, r.setup, r.opts.prefix(), r.opts.family(), version3, d); err != nil {
			return fmt.Errorf("deploying version %d: %w", d.VersionID, err)
		}
		return nil
	}
	rep, err := r.reload(ctx, "deploying a bucket", check, between)
	if err != nil {
		return DeployReport{}, err
	}
	out := DeployReport{Bucket: next.Name, SeedCents: d.SeedCents, Held: rep.Held, MayOrder: rep.MayOrder}
	slog.Info("v3 bucket deployed by the operator", "bucket", out.Bucket, "seed_cents", out.SeedCents, "source", d.Source, "version", d.VersionID)
	return out, nil
}

// lifeOf reads N from "<name> life N"; a first life has no suffix and is 1.
func lifeOf(name string) int {
	i := strings.LastIndex(name, " life ")
	if i < 0 {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(name[i+len(" life "):]))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// firstRebuild is steps 3 to 5 of NewRunner3. It recovers a panic with the same mechanism as
// every run-time entry point (caught, deferred first): the runner is then left SUSPENDED with the
// panic as its reason and its ids known, so heldElsewhere is still right, the value snapshots are
// refused while it lasts, and Run tries to heal it. The steps after a panic do not run.
func (r *Runner3) firstRebuild(ctx context.Context) {
	defer func() { r.caught("start", recover()) }()
	// 3. The rebuild: this, and nothing before it, establishes what each bucket still holds.
	if err := r.rebuild(ctx); err != nil {
		slog.Error("v3 starts SUSPENDED: its first rebuild failed; nothing is swept or closed at this start", "err", err)
		return
	}
	// 4. The start-up sweep, which may settle an old lot and change a bucket's cash.
	r.sweep(ctx)
	// 5. Only now, and only if v3 is still not suspended: close what has run out. Never on the cash
	//    test alone (CloseBucket does not look for open positions; RanOut does).
	r.closeExhausted(ctx)
}

func (r *Runner3) newPaper() *broker.Paper {
	p := broker.NewPaper(5) // [FACT] five levels are recorded
	p.FeePerFill = r.feePerFill
	return p
}

func (r *Runner3) mayOrderCount() int {
	n := 0
	for _, b := range r.buckets {
		if b.mayOrder {
			n++
		}
	}
	return n
}

// closeExhausted reaps and freezes, without restaking, every bucket that has run out NOW: ledger
// cash under the floor AND nothing open, after the rebuild and the sweep. NewRunner3 and Reload
// call it at the end of a load; settleLocked calls the locked half after every settlement, so
// a bucket that loses its last bet is closed then and not at the next start (platform brief,
// section 5: closed, never topped up; frozen, not deleted; not replaced until a different
// strategy is waiting). A close that fails leaves the bucket held, exhausted, until the next try.
//
// The same pass closes a bucket whose close the operator asked for while it had a position open
// (CloseWhenFlat; bucket.close_requested_at), the first time it holds nothing: so the × is
// honoured by the sweep after the last settlement, or by the next start, and nobody has to come
// back and press it again.
func (r *Runner3) closeExhausted(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.fenceLocked("start-up close")
	r.closeExhaustedLocked(ctx)
}

// closeExhaustedLocked is closeExhausted under a lock the caller holds. Never while suspended:
// an account's cash is then not known to be the ledger's.
func (r *Runner3) closeExhaustedLocked(ctx context.Context) {
	if r.state == stateSuspended {
		return
	}
	var keep []bucket3
	var accounts []*k3.Account
	var closed bool
	for _, b := range r.buckets {
		a := r.engine.Account(b.ID)
		asked := a != nil && b.CloseRequested && len(a.Positions) == 0
		if a == nil || (!a.RanOut() && !asked) {
			keep = append(keep, b)
			if a != nil {
				accounts = append(accounts, a)
			}
			continue
		}
		reason := fmt.Sprintf("ran out: %d cents left and nothing open", a.CashCents)
		if asked {
			reason = "closed out by the operator, once its last position settled"
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.writeBudget)
		_, err := r.db.CloseBucket(wctx, r.setup, b.SimBucket, reason, false, 0, 0)
		cancel()
		if err != nil {
			slog.Error("v3 could not close a bucket; it stays held", "bucket", b.Name, "why", reason, "err", err)
			keep, accounts = append(keep, b), append(accounts, a)
			continue
		}
		closed = true
		delete(r.hwm, b.ID)
		if asked {
			slog.Info("v3 bucket closed out as the operator asked: reaped, frozen, not replaced", "bucket", b.Name, "reaped_cents", a.CashCents)
		} else {
			slog.Warn("v3 bucket ran out: reaped, frozen, not replaced", "bucket", b.Name, "left_cents", a.CashCents)
		}
	}
	if !closed {
		return
	}
	engine, err := r.newEngine(accounts)
	if err != nil { // cannot happen: these accounts were accepted a moment ago
		r.suspendLocked("rebuilding the engine after closing a bucket: " + err.Error())
		return
	}
	r.buckets, r.engine = keep, engine
	r.ids = r.ids[:0:0]
	for _, b := range keep {
		r.ids = append(r.ids, b.ID)
	}
}

func (r *Runner3) newEngine(accounts []*k3.Account) (*k3.Engine, error) {
	if r.plumbing {
		return k3.NewPlumbingEngine(accounts...)
	}
	return k3.NewEngine(accounts...)
}

// BucketIDs is the ids of the buckets v3 holds right now. It waits on r.mu.
func (r *Runner3) BucketIDs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64{}, r.ids...)
}

// heldLocked is a copy of the held set for a reader that goes on without the lock.
func (r *Runner3) heldLocked() ([]int64, []bucket3) {
	return append([]int64{}, r.ids...), append([]bucket3{}, r.buckets...)
}

// savedState3 is all the third engine saves: what the ledger cannot know.
type savedState3 struct {
	Offsets map[string][][2]float64 `json:"offsets"` // per coin: {close, measured index offset}
}

// Seed primes each coin's volatility and index offset from the exchanges, as the second engine's
// runner does, with the same helper. The two engines ask separately, so their seeds can differ
// by a minute's candle; the drift gate (View.Drift) sees that and blocks entries until they agree.
func (r *Runner3) Seed(ctx context.Context, client *kalshi.Client, userAgent string) {
	for _, c := range r.coins {
		closes, measured := seedData(ctx, client, userAgent, c.Series, c.Product)
		if len(closes) > 0 {
			r.model.SeedVol(c.Coin, closes)
		}
		var fresh [][2]float64
		for _, m := range measured {
			if !r.model.HasOffsetAt(c.Coin, m[0]) {
				fresh = append(fresh, m)
			}
		}
		r.model.SeedOffsets(c.Coin, fresh)
	}
}

// ---- entry points called from someone else's goroutine ----------------------------------------

// caught is given recover()'s answer by a function every entry point defers FIRST, so that it
// runs after the entry point's own deferred Unlock. A panic is written down for the next holder of r.mu, which suspends v3 with
// it (memory may now be anything; the rebuild reads the truth). It never waits for r.mu itself.
//
// It is the OUTER fence, for code that runs without r.mu (Inputs, Observe, a rebuild's reads).
// A panic while r.mu is held is stopped earlier, by fenceLocked, so that no other goroutine can
// take the lock and act on half-changed memory before v3 is marked suspended.
func (r *Runner3) caught(where string, p any) bool {
	if p == nil {
		return false
	}
	note := fmt.Sprintf("a panic in v3's %s: %v", where, p)
	slog.Error("v3 recovered a panic; it will suspend and rebuild", "where", where, "panic", p, "stack", string(debug.Stack()))
	r.panicNote.Store(&note)
	if r.mu.TryLock() {
		defer r.mu.Unlock()
		r.absorbLocked()
	}
	return true
}

// fenceLocked is the INNER fence. It is deferred straight after r.mu.Lock() and its deferred
// Unlock, so it runs BEFORE the Unlock: a panic under the lock suspends v3 while the lock is still
// held, and the next holder (Book from the snapshot writer, a sweep, the cash check) sees
// "suspended", never half-folded positions or cash reported as true. The panic stops here; the
// function it was deferred in returns its zero values, which is why a function whose result means
// "it worked" (rebuild) sets its own result instead of using this.
func (r *Runner3) fenceLocked(where string) {
	if p := recover(); p != nil {
		r.panickedLocked(where, p)
	}
}

// panickedLocked logs a recovered panic and suspends v3 with it. Callers hold r.mu.
func (r *Runner3) panickedLocked(where string, p any) string {
	note := fmt.Sprintf("a panic in v3's %s: %v", where, p)
	slog.Error("v3 recovered a panic under its lock; it suspends and will rebuild", "where", where, "panic", p, "stack", string(debug.Stack()))
	r.suspendLocked(note)
	return note
}

// absorbLocked turns a recovered panic into a suspension. Every holder of r.mu calls it first.
func (r *Runner3) absorbLocked() {
	if note := r.panicNote.Swap(nil); note != nil {
		r.suspendLocked(*note)
	}
}

// suspendLocked: memory may be behind the ledger. Nothing is written until a rebuild succeeds,
// and gen moves so that a rebuild whose reads began before this is thrown away.
//
// healAfter is set when v3 BECOMES suspended and is not pushed later by a further suspension:
// healDelay3 exists to let a write that timed out land or die, and v3 writes nothing while
// suspended, so no later suspension can start a new such wait. Pushing it on every repeat would
// let a panic that recurs every second keep v3 (and every engine's snapshots) waiting for ever
// with no rebuild ever tried.
func (r *Runner3) suspendLocked(reason string) {
	if r.state != stateSuspended {
		r.since, r.healAfter = r.now(), r.now().Add(healDelay3)
	}
	r.state, r.reason = stateSuspended, reason
	r.gen++
	r.probe = nil
	slog.Error("v3 SUSPENDED: it writes nothing until it has rebuilt itself from the database", "why", reason)
}

func (r *Runner3) marketOf(info kalshi.MarketInfo, closes time.Time) k3.Market {
	m := k3.Market{Ticker: info.Ticker, Close: k3.UnixSeconds(closes)}
	if info.FloorStrike != nil {
		m.Strike = *info.FloorStrike
	}
	return m
}

// Inputs computes the model's view of one coin's open round and returns it as the journal map
// stored with that second's evaluation row. It takes the model's mutex and coinMu only, never
// r.mu. nil on a panic, or for a coin the engine does not look at.
func (r *Runner3) Inputs(coin string, info kalshi.MarketInfo, closes, at time.Time, price string) (out map[string]any) {
	defer func() {
		if r.caught("Inputs", recover()) {
			out = nil
		}
	}()
	if _, ok := r.coins[coin]; !ok {
		return nil
	}
	view := r.model.View(coin, r.marketOf(info, closes), f(price), k3.UnixSeconds(at), nil)
	r.coinMu.Lock()
	r.views[coin] = seenView{ticker: info.Ticker, at: at, view: view}
	r.coinMu.Unlock()
	return view.Journal()
}

// SetLongSigma gives a coin's model its long volatility (engine.LongSigmaFromDaily), the one a
// market more than an hour from its close is priced with. The model's mutex only.
func (r *Runner3) SetLongSigma(coin string, sigma float64) { r.model.SetLongSigma(coin, sigma) }

// Observe takes a trade print for whichever coin uses that product. The model's mutex only.
func (r *Runner3) Observe(t coinbase.Trade) {
	defer func() { r.caught("Observe", recover()) }()
	price, err := strconv.ParseFloat(t.Price, 64)
	if err != nil {
		return
	}
	for _, c := range r.coins {
		if c.Product == t.Product {
			r.model.Observe(c.Coin, price, unix(t.At)) // the same clock reading the second engine's fork takes
		}
	}
}

func (r *Runner3) viewFor(coin, ticker string, at time.Time) k3.View {
	r.coinMu.Lock()
	defer r.coinMu.Unlock()
	if s, ok := r.views[coin]; ok && s.ticker == ticker && s.at.Equal(at) {
		return s.view
	}
	return k3.View{Coin: coin} // not OK: nothing is decided on a view that was not journaled
}

// Step is one look at one coin's open round, AFTER v1 and v2 have stepped. It returns nothing:
// no v3 outcome may reach the poller. If r.mu is busy (another coin's write, a settlement, a
// swap) it gives up at once; that costs one second of one coin, in which the book was not
// observed, which can only leave the paper holds too high.
func (r *Runner3) Step(ctx context.Context, coin string, evalID int64, at time.Time, marketID int64, info kalshi.MarketInfo, closes time.Time, q kalshi.Quotes, price string) {
	defer func() { r.caught("Step", recover()) }()
	if !r.mu.TryLock() {
		r.skippedStep.Add(1)
		return
	}
	defer r.mu.Unlock()
	defer r.fenceLocked("Step")
	r.absorbLocked()
	m := r.marketOf(info, closes)
	m.MarketID, m.EvaluationID = marketID, evalID
	r.quotes[m.Ticker] = seenQuotes{q: q, at: at, closes: m.Close} // memory only: what Book() marks a bet by
	if r.state == stateSuspended {
		return // Run heals. The Paper is replaced by the rebuild, so there is no hold to refresh.
	}
	// The holds are refreshed first, on the very book the decision is made on. (The plan returns on
	// an empty price before this; observing the book needs no price, and skipping it could only
	// leave the holds too high.)
	r.paper.ObserveBook(m.Ticker, evalID, at, closes, q)
	if f(price) == 0 || !r.ordersLive.Load() || r.mayOrderCount() == 0 {
		return // no fresh price, orders switched off, or observe-only / settle-only
	}
	now := k3.UnixSeconds(at)
	decisions, intents := r.engine.Decide(coin, m, q, r.viewFor(coin, m.Ticker, at), now)
	// Once after EVERY Decide, whatever becomes of the step (kalshi15m3/doc.go): also while paused,
	// when nothing is sent. It touches soft state only (a dropped exit, the seconds an exit was
	// wanted and no order went out), never money, so it needs no ledger write to have happened.
	r.engine.AfterDecide(m.Ticker, decisions)
	if r.state == statePaused {
		r.probe = r.probeRecord(evalID, at, marketID, decisions)
		return
	}

	reports := make([]broker.Report, 0, len(intents))
	ids := make([]string, 0, len(intents))
	for _, in := range intents {
		rep, err := r.paper.Submit(ctx, in.Order)
		if err != nil { // only a cancelled context: the service is stopping, and nothing happened
			_ = r.paper.Void(ids...)
			return
		}
		reports, ids = append(reports, rep), append(ids, in.Order.ClientID)
	}

	rec := store.StepRecord3{EvaluationID: evalID, At: at, MarketID: marketID}
	index := make([]int, len(decisions))
	for i, d := range decisions {
		// Thinned exactly as runner2.go does it: a decision is journaled when it sent an order, when
		// its answer changed, and every fifteen seconds regardless. The evaluation row, with v3's
		// model inputs in it, is stored every second, so any second can be recomputed.
		key, sig := strconv.FormatInt(d.BucketID, 10)+"|"+coin, d.Side+"|"+d.BlockedBy+"|"+d.Why
		index[i] = -1
		if d.Intent < 0 && r.last[key] == sig && now-r.lastAt[key] < thinSeconds3 {
			continue
		}
		r.last[key], r.lastAt[key] = sig, now
		index[i] = len(rec.Decisions)
		rec.Decisions = append(rec.Decisions, r.decisionRow(d))
	}
	for i, in := range intents {
		b := r.bucket(in.Order.BucketID)
		if b == nil {
			_ = r.paper.Void(ids...)
			r.suspendLocked(fmt.Sprintf("the engine formed an order for bucket %d, which v3 does not hold", in.Order.BucketID))
			return
		}
		row := store.OrderRow{DecisionIndex: index[in.Decision], BucketID: b.ID, BucketLedgerID: b.LedgerAccountID, ClientID: in.Order.ClientID,
			Action: string(in.Order.Action), Side: string(in.Order.Side), Qty: in.Order.Qty, Limit: store.PriceText(int64(in.Order.Limit)),
			Status: string(reports[i].Status), Detail: r.engine.Detail(in, reports[i])}
		for _, fl := range reports[i].Fills {
			row.Fills = append(row.Fills, store.FillRow{Qty: fl.Qty, Price: store.PriceText(int64(fl.Price)), PremiumCents: fl.PremiumCents, FeeCents: fl.FeeCents})
		}
		rec.Orders = append(rec.Orders, row)
	}
	if len(rec.Decisions) == 0 && len(rec.Orders) == 0 {
		return
	}

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.writeBudget)
	defer cancel()
	if !r.saleLookLocked(wctx, marketID, intents, ids) {
		return
	}
	err := r.db.RecordOrders(wctx, r.setup, rec)
	switch {
	case err == nil:
		r.failures = 0
		if len(intents) == 0 {
			return
		}
		events := r.engine.Apply(intents, reports) // ONLY here: the ledger has these very orders
		r.paper.Commit(ids...)
		r.gen++
		r.noteEvents(events, f(price), now)
	case len(intents) == 0:
		// Decisions only: no money was in it, so there is nothing to be unsure about, whatever the
		// error. Dropped and counted, exactly as v2 tolerates a lost decision-only write.
		r.refusedLocked(err)
	case store.DefinitelyRolledBack(err):
		// The server said no (or the store refused before COMMIT): memory still equals the ledger.
		if verr := r.paper.Void(ids...); verr != nil {
			r.suspendLocked("the broker could not take back fills that were not recorded: " + verr.Error())
			return
		}
		r.refusedLocked(err)
	default:
		// A deadline or a connection error: the server may still commit. NOT asked about now: asked
		// straight after a timeout, the database can answer "none" about a commit that lands a
		// moment later. v3 suspends, and the rebuild, not before healAfter, reads what is true.
		_ = r.paper.Void(ids...)
		r.pendingIDs = ids
		r.suspendLocked("the outcome of a write is unknown: " + err.Error())
	}
}

// saleLookLocked is a sale's last look at the ledger, the same look the settlement path takes
// (plan 5.3), made inside the step's write budget and only on a second whose step sells. It
// reports whether the step may be written.
//
// Why a sale needs it and a buy does not: a sale is recorded straight from memory, and its detail
// (cost_cents, the basis it releases) is what every later rebuild's self-check takes as the truth.
// If memory is SHORT when the sale is formed (a write that timed out committed after the rebuild
// that followed healAfter), that detail is wrong for ever, the rows are append-only, and no
// rebuild can pass again; if memory holds a lot whose sale already committed late, the step would
// record a second sale of contracts never held. Both were reproduced on the fake ledger. So for
// every (bucket, side) being sold, the ledger's net-open quantity must equal memory's contracts:
// on any difference the orders are voided, v3 suspends, and nothing is written.
//
// What it cannot close: a late commit that lands AFTER this read and before the step's own
// commit. Only healDelay3 [CONVENTION] narrows that; the minute's cash check then suspends v3.
//
// A read that fails voids the orders and counts as a refused write: nothing was written, so
// memory still equals the ledger, and three in a row pause v3 as any refusal does.
func (r *Runner3) saleLookLocked(ctx context.Context, marketID int64, intents []k3.Intent, ids []string) bool {
	type lot struct {
		key    [2]string
		ticker string
	}
	var sold []lot
	memory := map[[2]string]int{}
	for _, in := range intents {
		if in.Order.Action != broker.Sell {
			continue
		}
		key := store.LotKey(in.Order.BucketID, string(in.Order.Side))
		if _, seen := memory[key]; seen {
			continue
		}
		held := 0
		if a := r.engine.Account(in.Order.BucketID); a != nil {
			if p := a.Position(in.Order.Ticker, string(in.Order.Side)); p != nil {
				held = p.Contracts
			}
		}
		memory[key] = held
		sold = append(sold, lot{key, in.Order.Ticker})
	}
	if len(sold) == 0 {
		return true
	}
	open, err := r.db.OpenQty(ctx, marketID, r.ids)
	if err != nil {
		_ = r.paper.Void(ids...)
		r.refusedLocked(fmt.Errorf("a sale's last look at the ledger could not be read, so nothing was sent: %w", err))
		return false
	}
	for _, l := range sold {
		if got := open[l.key]; got != memory[l.key] {
			_ = r.paper.Void(ids...)
			r.suspendLocked(fmt.Sprintf("selling %s %s for bucket %s: memory holds %d contracts and the ledger %d; nothing was written",
				l.ticker, l.key[1], l.key[0], memory[l.key], got))
			return false
		}
	}
	return true
}

// refusedLocked counts a write that certainly did not happen; the third in a row pauses v3.
// Paused is NOT halted: every refused write rolled back, so memory equals the ledger, the book
// is true and the value snapshots go on.
func (r *Runner3) refusedLocked(err error) {
	r.failures++
	slog.Warn("v3 write refused; memory still equals the ledger", "in_a_row", r.failures, "err", err)
	if r.failures >= pauseAfter3 && r.state == stateRunning {
		r.state, r.reason, r.since = statePaused, "the database is refusing v3's writes: "+err.Error(), r.now()
		slog.Error("v3 PAUSED: no orders until a write-probe succeeds", "why", r.reason)
	}
}

func (r *Runner3) bucket(id int64) *bucket3 {
	for i := range r.buckets {
		if r.buckets[i].ID == id {
			return &r.buckets[i]
		}
	}
	return nil
}

func (r *Runner3) decisionRow(d k3.Decision) store.DecisionRow {
	row := store.DecisionRow{BucketID: d.BucketID, ModelProb: d.ModelProb, MarketProb: d.MarketProb, Edge: d.Edge, Side: d.Side,
		Action: d.Action, BlockedBy: d.BlockedBy, Why: d.Why}
	if b := r.bucket(d.BucketID); b != nil {
		row.VersionID = b.VersionID
	}
	if d.Requested > 0 {
		n := d.Requested
		row.SizeAlone = &n
	}
	return row
}

// probeRecord is a decisions-only record of what v3 would have journaled this second, kept while
// paused for Run's write-probe. It carries the real probabilities, so a probe that succeeds
// leaves an honest journal row, and it says that nothing was sent.
func (r *Runner3) probeRecord(evalID int64, at time.Time, marketID int64, decisions []k3.Decision) *store.StepRecord3 {
	if len(decisions) == 0 {
		return nil
	}
	rec := &store.StepRecord3{EvaluationID: evalID, At: at, MarketID: marketID}
	for _, d := range decisions {
		row := r.decisionRow(d)
		if row.Action != "none" {
			row.Action, row.BlockedBy, row.Why, row.SizeAlone = "none", "v3 paused", "v3 is paused: it wanted to "+d.Action+" ("+d.Why+") and sent nothing", nil
		}
		rec.Decisions = append(rec.Decisions, row)
	}
	return rec
}

func (r *Runner3) noteEvents(events []k3.Event, price, now float64) {
	for _, e := range events {
		switch e.Kind {
		case "inconsistent":
			r.suspendLocked("the engine could not fold an answer it was given: " + e.Note)
		case "bought", "sold":
			key := entryKey(e.BucketID, e.Ticker, e.Side)
			kind := "bet"
			if e.Kind == "sold" {
				kind = "sold"
			} else if _, ok := r.entryPrice[key]; !ok {
				r.entryPrice[key] = price
			}
			r.markers = append(r.markers, Marker{T: now, Price: price, Coin: e.Coin, Side: upDown(e.Side), Strategy: e.Strategy, Engine: "v3", World: "real", Kind: kind})
			if len(r.markers) > markersKept3 {
				r.markers = r.markers[len(r.markers)-markersKept3:]
			}
			slog.Info("sim trade", "engine", "v3", "strategy", e.Strategy, "coin", e.Coin, "kind", e.Kind, "side", e.Side,
				"contracts", e.Contracts, "left", e.Left, "cash_cents", e.CashCents, "why", e.Why)
		case "settled":
			slog.Info("sim settlement", "engine", "v3", "strategy", e.Strategy, "coin", e.Coin, "ticker", e.Ticker, "side", e.Side,
				"contracts", e.Contracts, "won", e.Won, "payout_cents", e.CashCents, "blocked_seconds", e.BlockedSeconds)
		case "exhausted":
			slog.Warn("v3 account ran out: it orders no more, stays held, and is closed at the next start", "strategy", e.Strategy, "cash_cents", e.CashCents)
		}
	}
}

func entryKey(bucketID int64, ticker, side string) string {
	return fmt.Sprintf("%d|%s|%s", bucketID, ticker, side)
}

func upDown(side string) string {
	if side == "yes" {
		return "UP"
	}
	return "DOWN"
}

// Settled is the poller's one call about a round's result. The model learns the index gap from
// it whatever happens next (the model's mutex only). The money side gives up at once if r.mu is
// busy or v3 is suspended: market.result was stored before this call, and the sweep reads it.
func (r *Runner3) Settled(ctx context.Context, coin string, marketID int64, info kalshi.MarketInfo, closes time.Time) {
	defer func() { r.caught("Settled", recover()) }()
	if _, ok := r.coins[coin]; ok {
		if at := k3.UnixSeconds(closes); !r.model.HasOffsetAt(coin, at) {
			r.model.NoteSettlement(coin, at, info.ExpirationValue)
			if r.model.HasOffsetAt(coin, at) {
				r.stateDirty.Store(true) // Run saves it: no database write of v3's state on the poller's goroutine
			}
		}
	}
	if !r.mu.TryLock() {
		r.skippedSettled.Add(1)
		return
	}
	defer r.mu.Unlock()
	defer r.fenceLocked("Settled")
	r.absorbLocked()
	if r.state == stateSuspended {
		return
	}
	r.settleLocked(ctx, marketID, info.Ticker, info.Result)
}

// settleLocked books one market's result for every bucket that holds it. Callers hold r.mu and
// have checked that v3 is NOT suspended: a settlement written from a memory that is short can
// never be corrected (unique (market_id, bucket_id, side) refuses a second row).
func (r *Runner3) settleLocked(ctx context.Context, marketID int64, ticker, result string) {
	rows := r.engine.SettleRows(ticker, result)
	if len(rows) == 0 {
		if result == "yes" || result == "no" {
			r.forgetLocked(ticker)
		}
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.writeBudget)
	defer cancel()
	// The last look: what the LEDGER says is net open, against what memory is about to settle.
	open, err := r.db.OpenQty(wctx, marketID, r.ids)
	if err != nil {
		slog.Warn("v3 could not take its last look before a settlement; nothing written, the next sweep tries again", "ticker", ticker, "err", err)
		return
	}
	var write []store.SettlementRow
	for _, row := range rows {
		if got := open[store.LotKey(row.BucketID, row.Side)]; got != row.Qty {
			r.suspendLocked(fmt.Sprintf("settling %s %s for bucket %d: memory holds %d contracts and the ledger %d; nothing was written", ticker, row.Side, row.BucketID, row.Qty, got))
			return
		}
		b := r.bucket(row.BucketID)
		if b == nil {
			r.suspendLocked(fmt.Sprintf("a position in bucket %d, which v3 does not hold", row.BucketID))
			return
		}
		write = append(write, store.SettlementRow{BucketID: b.ID, BucketLedgerID: b.LedgerAccountID, Side: row.Side, Qty: row.Qty, PayoutCents: row.PayoutCents})
	}
	err = r.db.RecordSettlements(wctx, r.setup, marketID, r.now(), write)
	if err != nil {
		// Unlike an order, a settlement is idempotent by its unique key, so the database MAY be
		// asked: a late commit, or a retry that hit the unique index, makes the rows findable.
		//
		// The lookup and the partial re-write get a FRESH budget of the same size: when the write
		// failed by running out of time, wctx is already spent, and asking with it would fail at
		// once and turn every settlement timeout into a suspension. The lookup and the re-write share
		// the fresh budget, so r.mu may be held for up to about two budgets in all. Nothing on a
		// poller's path waits on r.mu (Step and Settled use TryLock); Book, Heal and Snapshot may.
		qctx, qcancel := context.WithTimeout(context.WithoutCancel(ctx), r.writeBudget)
		defer qcancel()
		found, qerr := r.db.SettlementsRecorded(qctx, marketID, r.ids)
		if qerr != nil {
			r.suspendLocked("a settlement's outcome is unknown and could not be looked up: " + qerr.Error())
			return
		}
		var missing []store.SettlementRow
		for _, row := range write {
			if !found[store.LotKey(row.BucketID, row.Side)] {
				missing = append(missing, row)
			}
		}
		switch {
		case len(missing) == 0: // it had committed after all
		case len(missing) == len(write):
			slog.Warn("v3 settlement not written; the next sweep tries again", "ticker", ticker, "err", err)
			return
		default:
			// Some rows exist and some do not (RecordSettlements is all or nothing, so this takes a
			// second writer). Writing the whole set again would fail on the unique key for ever, so
			// only what is missing is written.
			if err := r.db.RecordSettlements(qctx, r.setup, marketID, r.now(), missing); err != nil {
				slog.Warn("v3 settlement partly written; the next sweep tries the rest again", "ticker", ticker, "err", err)
				return
			}
		}
	}
	events := r.engine.ApplySettlement(ticker, result)
	r.gen++
	r.forgetLocked(ticker)
	r.noteEvents(events, 0, unix(r.now()))
	// The lifecycle, after the money has moved: the allocation on any gain above high water,
	// then the close of a bucket that has run out.
	r.allocateLocked(ctx)
	r.closeExhaustedLocked(ctx) // returns at once if the allocation suspended v3
}

// openCost is what an account has committed to open bets, at cost.
func openCost(a *k3.Account) int64 {
	var cents int64
	for _, p := range a.Open() {
		cents += p.CostCents
	}
	return cents
}

// allocateLocked is the sustainment allocation (platform brief, section 6), ported from the
// second engine's runner: the newest policy's share of each bucket's gain above its high-water
// mark, taken at settlement. Book value is cash plus open bets at cost, so money merely tied up
// is not skimmed, and a bucket climbing back from a loss is not charged twice on the same
// dollars. A bucket is skimmed only once every round that has CLOSED is settled for it: the coins
// settle seconds apart, and a gain after the first can be a loss after the last. What is taken
// leaves the account's cash AFTER the ledger has the transfer, never before; with every rate at
// zero the new mark is still recorded and nothing moves. Callers hold r.mu.
func (r *Runner3) allocateLocked(ctx context.Context) {
	now := k3.UnixSeconds(r.now())
	var due []*k3.Account
	for _, a := range r.engine.Accounts {
		pending := false
		for _, p := range a.Open() {
			if p.Close <= now {
				pending = true
				break
			}
		}
		if pending || a.CashCents+openCost(a) <= r.hwm[a.BucketID] {
			continue
		}
		due = append(due, a)
	}
	if len(due) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.writeBudget)
	defer cancel()
	policy, err := r.db.CurrentSkimPolicy(wctx)
	if err != nil {
		slog.Warn("v3 could not read the allocation rates; the allocation waits for the next settlement", "err", err)
		return
	}
	for _, a := range due {
		b := r.bucket(a.BucketID)
		if b == nil {
			continue
		}
		book, mark := a.CashCents+openCost(a), r.hwm[a.BucketID]
		gain := book - mark
		k := store.Skim{Bucket: b.SimBucket, Policy: policy, BookCents: book, HWMBefore: mark,
			Winnings: gain * policy.Winnings / 10000, Replenish: gain * policy.Replenish / 10000, Tax: gain * policy.Tax / 10000, Fees: gain * policy.Fees / 10000}
		if k.Taken() > a.CashCents {
			continue // the gain is tied up in open bets: take it when it is cash
		}
		if err := r.db.RecordSkim(wctx, r.setup, k); err != nil {
			if k.Taken() == 0 {
				slog.Warn("v3 could not record a high-water mark; no money was in it", "bucket", b.Name, "err", err)
				continue
			}
			// Money was in it and the outcome is not known: memory may now be behind the ledger.
			// Suspend; the rebuild reads the cash and the mark from the ledger.
			r.suspendLocked(fmt.Sprintf("recording the sustainment allocation from %s: %v", b.Name, err))
			return
		}
		if err := a.Withdraw(k.Taken()); err != nil {
			r.suspendLocked("after the sustainment allocation: " + err.Error())
			return
		}
		r.hwm[a.BucketID] = book - k.Taken()
		r.gen++
		if k.Taken() > 0 {
			slog.Info("sustainment allocation", "engine", "v3", "bucket", b.Name, "gain_cents", gain, "winnings", k.Winnings, "replenishment", k.Replenish, "tax", k.Tax, "fees", k.Fees)
		}
	}
}

func (r *Runner3) forgetLocked(ticker string) {
	r.paper.Forget(ticker)
	delete(r.quotes, ticker)
	for key := range r.entryPrice {
		if strings.Contains(key, "|"+ticker+"|") {
			delete(r.entryPrice, key)
		}
	}
}

// ---- v3's own goroutine: heal, probe, sweep, cash check, prune --------------------------------

// Run is v3's goroutine. It is started whenever v3 holds a bucket or is on, also with AC_V3 off.
// It and Heal are the only callers, besides Book and Snapshot, that may WAIT on r.mu.
func (r *Runner3) Run(ctx context.Context) {
	t := time.NewTicker(tick3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick(ctx)
		}
	}
}

// Tick is one pass of Run: rebuild if suspended (and due), probe if paused, and once a minute
// the sweep, the cash check and the pruning. In a tick that finds v3 suspended the order is
// rebuild FIRST, and the sweep follows in the same tick only if that rebuild succeeded.
func (r *Runner3) Tick(ctx context.Context) {
	defer func() { r.caught("Run", recover()) }()
	state, due := r.stateNow()
	healed := false
	switch {
	case state == stateSuspended && due:
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebuildBudget3)
		err := r.rebuild(rctx)
		cancel()
		if err != nil {
			slog.Warn("v3 rebuild did not succeed; it stays suspended and tries again", "err", err)
		}
		healed = err == nil
	case state == statePaused:
		r.writeProbe(ctx)
	}
	if state, _ = r.stateNow(); state == stateSuspended {
		return // nothing else runs while suspended
	}
	r.mu.Lock()
	minute := healed || r.now().Sub(r.lastSweep) >= sweepEvery3
	if minute {
		r.lastSweep = r.now()
	}
	r.mu.Unlock()
	if minute {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebuildBudget3)
		defer cancel()
		r.sweep(sctx)
		r.cashCheck(sctx)
		r.prune()
	}
	if r.stateDirty.Swap(false) {
		r.saveState(ctx)
	}
}

func (r *Runner3) stateNow() (state string, healDue bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.absorbLocked()
	return r.state, !r.now().Before(r.healAfter)
}

// Heal is called by the value-snapshot writer just before it reads the books: rebuild FIRST if
// suspended (and due); then, only if v3 is not or no longer suspended, sweep, so that a round
// that closed during the suspension is settled in the same call that lifts the halt. A failed
// rebuild means no sweep. Every read is made outside r.mu.
func (r *Runner3) Heal(ctx context.Context) {
	defer func() { r.caught("Heal", recover()) }()
	if state, due := r.stateNow(); state == stateSuspended {
		if !due {
			return
		}
		if err := r.rebuild(ctx); err != nil {
			slog.Warn("v3 could not heal before the value snapshot", "err", err)
			return
		}
	}
	r.sweep(ctx)
}

// writeProbe tries one decisions-only write while paused; the first that succeeds resumes v3.
func (r *Runner3) writeProbe(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.fenceLocked("write-probe")
	r.absorbLocked()
	if r.state != statePaused || r.probe == nil {
		return // no step has offered an evaluation row to hang the probe on yet
	}
	rec := *r.probe
	r.probe = nil
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.writeBudget)
	defer cancel()
	if err := r.db.RecordOrders(wctx, r.setup, rec); err != nil {
		slog.Warn("v3 write-probe failed; it stays paused", "err", err)
		return
	}
	r.state, r.reason, r.failures = stateRunning, "", 0
	slog.Info("v3 RESUMED: the database accepted a write-probe")
}

// sweep settles, from market.result, every open position whose close has passed, in EVERY held
// bucket. The poller tells an engine about a result exactly once; v3 does not depend on that
// call. Never while suspended. It needs no book, no model and no version status, so it runs with
// AC_V3 off and for a bucket whose version was benched or retired with a bet open.
func (r *Runner3) sweep(ctx context.Context) {
	due := func() map[int64]string { // market id -> ticker
		r.mu.Lock()
		defer r.mu.Unlock()
		defer r.fenceLocked("sweep")
		r.absorbLocked()
		if r.state == stateSuspended {
			return nil
		}
		out := map[int64]string{}
		at := k3.UnixSeconds(r.now())
		for _, a := range r.engine.Accounts {
			for _, p := range a.Open() {
				if p.Close <= at {
					out[p.MarketID] = p.Ticker
				}
			}
		}
		return out
	}()
	if len(due) == 0 {
		r.closeAsked(ctx)
		return
	}
	ids := make([]int64, 0, len(due))
	for id := range due {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	results, err := r.db.MarketResults(ctx, ids) // outside r.mu
	if err != nil {
		slog.Warn("v3 sweep could not read results; it tries again", "err", err)
		return
	}
	for _, id := range ids {
		result, ok := results[id]
		if !ok {
			continue
		}
		if ctx.Err() != nil { // the caller's budget is spent (Heal has 3 s): the rest waits for the next sweep
			return
		}
		func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			defer r.fenceLocked("sweep")
			r.absorbLocked()
			if r.state != stateSuspended { // memory is settled from under the lock, so the gap since the read is harmless
				r.settleLocked(ctx, id, due[id], result)
			}
		}()
	}
	r.closeAsked(ctx)
}

// closeAsked is the minute's look at buckets whose close the operator asked for: if one holds
// nothing now it is closed. Settlement already does this; this covers a bucket that went flat
// some other way (a position sold out, a mark that landed after the last settlement), so a ×
// never waits for the next start. Nothing to do when no bucket is marked.
func (r *Runner3) closeAsked(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.fenceLocked("close asked")
	r.absorbLocked()
	for _, b := range r.buckets {
		if b.CloseRequested {
			r.closeExhaustedLocked(ctx)
			return
		}
	}
}

// cashCheck compares every held bucket's cash in memory with the ledger's sum. The read is made
// outside r.mu and compared under it only if gen has not moved meanwhile; if it has, the check
// is simply repeated next minute. A difference suspends, and the rebuild reads the truth: this
// is what catches a commit that landed later than every other guard looked.
func (r *Runner3) cashCheck(ctx context.Context) {
	r.mu.Lock()
	r.absorbLocked()
	gen, suspended := r.gen, r.state == stateSuspended
	ids, _ := r.heldLocked()
	r.mu.Unlock()
	if suspended || len(ids) == 0 {
		return
	}
	ledger, err := r.db.BucketCash(ctx, ids)
	if err != nil {
		slog.Warn("v3 cash check could not read the ledger; it tries again next minute", "err", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.fenceLocked("cash check")
	r.absorbLocked()
	if r.state == stateSuspended || r.gen != gen {
		return
	}
	for _, a := range r.engine.Accounts {
		if a.CashCents != ledger[a.BucketID] {
			r.suspendLocked(fmt.Sprintf("cash check: bucket %d has %d cents in memory and %d in the ledger", a.BucketID, a.CashCents, ledger[a.BucketID]))
			return
		}
	}
}

// prune forgets windows that are over and empty, the journal-thinning keys nobody has used for an
// hour, and every round that closed more than an hour ago [CONVENTION: only a memory bound] in
// which no account holds a position.
//
// Forgetting a round includes the Paper's book, holds and order records for it. Settlement is
// the only other place that does that, and a round whose position was sold out early, or which
// never had one, is never settled; nor is one whose single Settled call was skipped. Without this
// the Paper would grow by a market (and its orders) every round for as long as the service runs.
// A round in which a position is still held is left alone: the sweep settles it, and forgets it then.
func (r *Runner3) prune() {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.fenceLocked("prune")
	now := k3.UnixSeconds(r.now())
	r.engine.Prune(now)
	for key, at := range r.lastAt {
		if now-at > 3600 {
			delete(r.lastAt, key)
			delete(r.last, key)
		}
	}
	held := map[string]bool{}
	for _, a := range r.engine.Accounts {
		for _, p := range a.Open() {
			held[p.Ticker] = true
		}
	}
	for ticker, q := range r.quotes {
		if q.closes+3600 < now && !held[ticker] {
			r.forgetLocked(ticker) // the Paper's market and orders, the quotes, the entry prices
		}
	}
}

func (r *Runner3) saveState(ctx context.Context) {
	state := savedState3{Offsets: map[string][][2]float64{}}
	for coin := range r.coins {
		state.Offsets[coin] = r.model.Offsets(coin)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeBudget3)
	defer cancel()
	if err := r.db.SaveEngineState(wctx, r.opts.prefix(), state); err != nil {
		r.stateDirty.Store(true)                                                                 // try again next tick
		slog.Warn("v3 could not save its learned offsets; no money depends on them", "err", err) // plan 5.4, step 6
	}
}

// ---- the rebuild ------------------------------------------------------------------------------

var errGenMoved = errors.New("v3's state moved while the rebuild was reading; the result was thrown away")

// fresh is a rebuilt memory, not yet swapped in.
type fresh struct {
	engine *k3.Engine
	paper  *broker.Paper
	fills  int
	note   string
}

// rebuild reads everything about money from the database into a FRESH engine and Paper, outside
// r.mu, and swaps them in under r.mu only if gen is what it was when the reads began (the
// pattern of Runner2.RefreshCapital). It never creates, adds or drops a bucket, and it reads no
// version status: what is held and what may order were fixed by NewRunner3. It does not touch
// the model.
//
// Two rebuilds may run at once (Run's Tick and the snapshot writer's Heal both see "suspended and
// due"). The one that swaps second finds gen moved by the first. If the first HEALED v3 (it is no
// longer suspended), the second returns nil and leaves the first one's report alone: v3 is healed,
// the caller may go on to sweep, and /api/status keeps the report that says what was found. Only
// while v3 is still suspended is a thrown-away rebuild an error.
func (r *Runner3) rebuild(ctx context.Context) (err error) {
	started := r.now()
	var gen uint64
	var pending []string
	var ids []int64
	var buckets []bucket3
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		defer r.fenceLocked("rebuild")
		r.absorbLocked()
		gen, pending = r.gen, append([]string{}, r.pendingIDs...)
		ids, buckets = r.heldLocked() // a Reload that swaps the set meanwhile also moves gen
	}()

	got, err := r.read(ctx, pending, ids, buckets)

	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() { // the inner fence, with a result: a panic here is a rebuild that did not succeed
		if p := recover(); p != nil {
			err = errors.New(r.panickedLocked("rebuild", p))
		}
	}()
	r.absorbLocked()
	report := rebuildReport{At: r.now(), Took: r.now().Sub(started)}
	switch {
	case r.gen != gen && r.state != stateSuspended: // whatever this one's reads found
		slog.Info("v3 rebuild overtaken by another that already healed it; this one's reads are thrown away", "its_err", err)
		return nil
	case err != nil:
		report.Reason = err.Error()
	case r.gen != gen:
		err = errGenMoved
		report.Reason = err.Error()
	default:
		report.OK, report.Fills, report.Note = true, got.fills, got.note
		r.engine, r.paper = got.engine, got.paper
		r.state, r.reason, r.failures, r.pendingIDs, r.probe = stateRunning, "", 0, nil, nil
		r.gen++
		slog.Info("v3 rebuilt from the database", "fills", got.fills, "took", report.Took.String(), "note", got.note)
	}
	r.rebuilt = report
	return err
}

// read is the rebuild's reads and its fold (plan 5.4, steps 1 to 7). It works on the copy of
// the held set its caller took under r.mu, and touches nothing of r that can change.
func (r *Runner3) read(ctx context.Context, pending []string, ids []int64, buckets []bucket3) (*fresh, error) {
	out := &fresh{paper: r.newPaper()}
	// 1. Cash: the ledger's sum IS CashCents. There is no saved cash to disagree with.
	cash, err := r.db.BucketCash(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading the buckets' cash: %w", err)
	}
	// 2. Fills: of markets with no result yet, and of lots that are NET open and never settled.
	fills, err := r.db.BucketFills(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading the fills: %w", err)
	}
	// The two reads are not one snapshot. If the cash moved between them (a late commit landing
	// right now), the fills may or may not contain it: this attempt proves nothing. Try again.
	again, err := r.db.BucketCash(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading the buckets' cash again: %w", err)
	}
	for _, id := range ids {
		if cash[id] != again[id] {
			return nil, fmt.Errorf("cash check: bucket %d's ledger cash moved from %d to %d cents while the rebuild was reading", id, cash[id], again[id])
		}
	}
	accounts := make([]*k3.Account, 0, len(buckets))
	for _, b := range buckets {
		a := k3.NewAccount(b.params, b.ID, cash[b.ID], b.mayOrder)
		a.SeedCents = b.SeedCents // the ledger's: a bucket deployed at its own figure sizes off that figure, not the convention
		if b.params.Sizing == k3.SizingMartingale && b.mayOrder {
			// The martingale's state is the ledger's, like everything else in this rebuild.
			streak, err := r.db.SettledStreak(ctx, b.ID)
			if err != nil {
				return nil, fmt.Errorf("reading bucket %d's settled streak: %w", b.ID, err)
			}
			a.LossStreak = streak
		}
		accounts = append(accounts, a)
	}
	if out.engine, err = r.newEngine(accounts); err != nil {
		return nil, err
	}

	// Group the fills by order, oldest first, and fold each order through the engine's one fold.
	type lot struct {
		bucket       int64
		ticker, side string
	}
	type order struct {
		first store.BucketFill
		fills []broker.Fill
	}
	var orders []*order
	byID := map[int64]*order{}
	now := r.now()
	for _, fl := range fills {
		units, err := store.ParsePrice4(fl.Price)
		if err != nil {
			return nil, fmt.Errorf("fill %d: %w", fl.FillID, err)
		}
		premium := -fl.BucketCents - fl.FeeCents // a buy's entry is -(premium + fee)
		if fl.Action == "sell" {
			premium = fl.BucketCents + fl.FeeCents // a sale's is premium - fee; 0 when the fill has no entry
		}
		o := byID[fl.OrderID]
		if o == nil {
			o = &order{first: fl}
			byID[fl.OrderID], orders = o, append(orders, o)
		}
		o.fills = append(o.fills, broker.Fill{Seq: len(o.fills) + 1, Qty: fl.Qty, Price: broker.Price(units), PremiumCents: premium, FeeCents: fl.FeeCents})
		// 5. Paper holds, for markets still open: every contract ever taken starts held at the price
		//    it was taken at, which errs toward holding too much.
		if fl.Result == "" && fl.ClosesAt.After(now) {
			out.paper.RestoreHold(fl.BucketID, fl.Ticker, broker.Action(fl.Action), broker.Side(fl.Side), broker.Price(units), fl.Qty)
		}
	}
	want := map[lot]int64{} // 4. what each position's cost must fold to, from the rows alone
	for _, o := range orders {
		var detail map[string]any
		if err := json.Unmarshal(o.first.Detail, &detail); err != nil {
			return nil, fmt.Errorf("order %d: its detail cannot be read: %w", o.first.OrderID, err)
		}
		booked, err := k3.BookedFromRow(o.first.BucketID, o.first.MarketID, o.first.Action, o.first.Side, k3.UnixSeconds(o.first.At), detail, o.fills)
		if err != nil {
			return nil, fmt.Errorf("order %d: %w", o.first.OrderID, err)
		}
		for _, e := range out.engine.FoldRecorded(booked) {
			if e.Kind == "inconsistent" {
				return nil, fmt.Errorf("order %d does not fold: %s", o.first.OrderID, e.Note)
			}
		}
		key := lot{o.first.BucketID, booked.Ticker, o.first.Side}
		if o.first.Action == "buy" {
			for _, fl := range o.fills {
				want[key] += fl.PremiumCents + fl.FeeCents
			}
		} else {
			released, ok := detail["cost_cents"].(float64)
			if !ok {
				return nil, fmt.Errorf("order %d: a sale's detail lacks cost_cents", o.first.OrderID)
			}
			want[key] -= int64(released)
		}
		out.fills += len(o.fills)
	}
	// 4. Self-check: v2's rule, kept, with a way back (a failure leaves v3 suspended, and it tries again).
	for key, cents := range want {
		var got int64
		if a := out.engine.Account(key.bucket); a != nil {
			if p := a.Position(key.ticker, key.side); p != nil {
				got = p.CostCents
			}
		}
		if got != cents {
			return nil, fmt.Errorf("self-check: bucket %d %s %s folds to a cost of %d cents and its rows say %d", key.bucket, key.ticker, key.side, got, cents)
		}
	}
	// 7. Exhausted, LAST, from what the steps above found: never from cash alone.
	for _, a := range out.engine.Accounts {
		a.Exhausted = a.RanOut()
	}
	// The report: did the step that suspended v3 commit after all? It decides nothing.
	if len(pending) > 0 {
		if found, err := r.db.OrdersRecorded(ctx, pending); err != nil {
			out.note = "whether the step that suspended v3 had committed could not be read: " + err.Error()
		} else {
			out.note = fmt.Sprintf("the step that suspended v3 had %d of its %d orders in the ledger", len(found), len(pending))
		}
	}
	return out, nil
}

// SetOrders is the buckets-page switch. Off stops the next look. On resumes orders for
// buckets this process was built allowed to order, and otherwise waits for the next start.
// "now" means this process honours the switch on its next look. "next-start" means the
// saved value is what the next start will use, and this process has no bucket it can order.
func (r *Runner3) SetOrders(on bool) string {
	r.ordersLive.Store(on)
	r.mu.Lock()
	n := r.mayOrderCount()
	r.mu.Unlock()
	r.notesMu.Lock()
	r.notes = nil
	r.notesMu.Unlock()
	if n > 0 {
		return "now"
	}
	return "next-start"
}

// OrdersStatus is what the buckets page shows: whether a look right now would send an
// order, and whether the switch applies to this process or to the next start.
func (r *Runner3) OrdersStatus() (placing bool, effective string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.mayOrderCount()
	if n == 0 {
		return false, "next-start"
	}
	return r.ordersLive.Load() && r.state == stateRunning, "now"
}

// HoldForReset stops writes while the simulated books are deleted. A rebuild will not
// start for an hour, so a heal cannot put the deleted buckets back into memory.
func (r *Runner3) HoldForReset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suspendLocked("sim books are being reset")
	r.healAfter = r.now().Add(time.Hour)
}

// AbortReset lets the runner rebuild from the books, which are still there.
func (r *Runner3) AbortReset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateSuspended {
		r.healAfter = r.now()
	}
}

// ReleaseAfterReset drops every held bucket. The database no longer has them, so a
// rebuild of the old ids would invent an empty account and then try to trade it.
func (r *Runner3) ReleaseAfterReset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.fenceLocked("reset")
	r.buckets = nil
	r.ids = nil
	r.engine, _ = k3.NewEngine()
	r.paper = r.newPaper()
	r.pendingIDs = nil
	r.probe = nil
	r.markers = nil
	r.entryPrice = map[string]float64{}
	r.quotes = map[string]seenQuotes{}
	r.last = map[string]string{}
	r.lastAt = map[string]float64{}
	r.state = stateRunning
	r.reason = ""
	r.failures = 0
	r.healAfter = time.Time{}
	r.since = r.now()
	r.gen++
}

// ---- /api/status ------------------------------------------------------------------------------

// Snapshot is the "v3" block of /api/status. It waits on r.mu (a status page may wait out a
// write; a poller may not), and it asks the database, outside the lock, what the versions'
// statuses are NOW, to compare them with what the last load found (restartNotes): a status
// change takes effect at the next load, which the buckets page runs after each of its own
// approvals (Reload; D18 as amended).
func (r *Runner3) Snapshot(ctx context.Context) map[string]any {
	notes := r.restartNotes(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.absorbLocked()
	mode := r.state
	if r.state == stateRunning {
		switch {
		case len(r.buckets) == 0:
			mode = "observe-only"
		case !r.ordersLive.Load() || r.mayOrderCount() == 0:
			mode = "settle-only"
		}
	}
	doc := map[string]any{"engine": "v3", "on": r.ordersLive.Load(), "state": r.state, "mode": mode, "reason": r.reason, "plumbing": r.plumbing,
		"fault_injection_built": FaultInjectionBuilt, "failures_in_a_row": r.failures,
		"skipped_busy": map[string]any{"step": r.skippedStep.Load(), "settled": r.skippedSettled.Load()}, "restart_notes": notes}
	if r.state != stateRunning {
		doc["since"] = r.since
	}
	if r.state == stateSuspended {
		doc["heal_after"] = r.healAfter
	}
	rb := r.rebuilt
	doc["last_rebuild"] = map[string]any{"at": rb.At, "ok": rb.OK, "took_ms": rb.Took.Milliseconds(), "fills": rb.Fills, "note": rb.Note, "problem": rb.Reason}
	buckets := []map[string]any{}
	for _, b := range r.buckets {
		doc := map[string]any{"id": b.ID, "name": b.Name, "strategy": b.Strategy, "status_loaded": b.VersionStatus, "may_order": b.mayOrder,
			"why_not": b.whyNot, "plumbing": b.plumbing}
		if a := r.engine.Account(b.ID); a != nil {
			positions, holds := []map[string]any{}, map[string]any{}
			for _, p := range a.Open() {
				positions = append(positions, map[string]any{"coin": p.Coin, "ticker": p.Ticker, "side": p.Side, "contracts": p.Contracts,
					"cost_cents": p.CostCents, "closes": p.Close, "exiting": p.Exiting, "blocked_seconds": p.BlockedSeconds})
			}
			for ticker := range r.quotes {
				for _, l := range []broker.Ladder{broker.YesBids, broker.NoBids} {
					if n := r.paper.TotalHeld(b.ID, ticker, l); n > 0 {
						holds[ticker+" "+string(l)] = n
					}
				}
			}
			windows := []map[string]any{}
			for _, w := range a.Windows {
				windows = append(windows, map[string]any{"close": w.Close, "equity_cents": w.EquityCents, "k_max": w.KMax, "open_cents": w.OpenCents,
					"lost_cents": w.LostCents, "used_cents": w.Used()})
			}
			sort.Slice(windows, func(i, j int) bool { return windows[i]["close"].(int64) < windows[j]["close"].(int64) })
			doc["cash_cents"], doc["bets"], doc["exhausted"], doc["positions"], doc["windows"], doc["holds"] = a.CashCents, a.Bets, a.Exhausted, positions, windows, holds
		}
		buckets = append(buckets, doc)
	}
	doc["buckets"] = buckets
	return doc
}

// restartNotes compares the versions' stored statuses with what the last load found, and says,
// for each approved version that is not trading, WHY, from what load recorded rather than by
// inference:
//
//	"<name>: approved, but new orders are off"
//	"<name>: refused at the last load: <why>: a reload alone will not change this"  vet said no
//	"<name>: its bucket ran out and is frozen: not traded, not replaced"  seeded by the load, not held
//	"<name>: approved, but the next load will refuse it: <why>"          approved since; vet says no
//	"<name>: approved, waiting for a reload"                             approved since, outside the page; vet says yes
//	"<strategy>: no longer tradable: settle-only after a reload"         ordering now, no longer approved
//
// The buckets page reloads after every approval it makes, so the last two are seen only when a
// status was changed by hand in the database. A version seeded by a load with no bucket that may
// order can only have had its bucket frozen (reaped by that load, or an earlier one: EnsureSimSetup
// leaves a frozen bucket alone and HeldBuckets skips it). A bucket whose close failed stays held.
func (r *Runner3) restartNotes(ctx context.Context) (notes []string) {
	r.notesMu.Lock() // its own lock, held across this read: only status pages wait on it
	defer r.notesMu.Unlock()
	if r.notes != nil && r.now().Sub(r.notesAt) < notesFor3 {
		return r.notes
	}
	defer func() { r.notes, r.notesAt = notes, r.now() }()
	qctx, cancel := context.WithTimeout(ctx, writeBudget3)
	defer cancel()
	notes = []string{}
	tradable, err := r.db.TradableVersions(qctx, r.opts.family(), version3)
	if err != nil {
		return append(notes, "the versions' statuses could not be read: "+err.Error())
	}
	now := map[int64]bool{}
	loaded := map[int64]bool{}
	r.mu.Lock()
	_, buckets := r.heldLocked()
	refused, seeded := r.refused, r.seeded // replaced whole by a load, never written into
	r.mu.Unlock()
	for _, b := range buckets {
		loaded[b.VersionID] = loaded[b.VersionID] || b.mayOrder
	}
	for _, v := range tradable {
		now[v.ID] = true
		switch {
		case loaded[v.ID]:
		case !r.ordersLive.Load():
			notes = append(notes, v.Name+": approved, but new orders are off")
		case refused[v.ID] != "":
			notes = append(notes, v.Name+": refused at the last load: "+refused[v.ID]+": a reload alone will not change this")
		case seeded[v.ID]:
			notes = append(notes, v.Name+": its bucket ran out and is frozen: not traded, not replaced")
		default:
			if _, _, err := r.opts.vet(v.Params); err != nil {
				notes = append(notes, v.Name+": approved, but the next load will refuse it: "+err.Error())
			} else {
				notes = append(notes, v.Name+": approved, waiting for a reload")
			}
		}
	}
	for _, b := range buckets {
		if b.mayOrder && !now[b.VersionID] {
			notes = append(notes, b.Strategy+": no longer tradable: settle-only after a reload")
		}
	}
	return notes
}
