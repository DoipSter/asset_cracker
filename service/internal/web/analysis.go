package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/doipster/asset_cracker/service/internal/analysis"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// How hard GET /api/analysis may lean on the database. These are budgets for a Raspberry Pi, set
// by judgement and not yet measured against the live tables.
const (
	analysisEvery   = time.Minute      // the document is recomputed at most this often
	analysisTimeout = 25 * time.Second // one refresh, all queries together
	// Settled markets read per refresh, counting the ones tried again. A settled round never
	// changes once its settlement rows are in, so each is read once and kept; after a restart the
	// history is refilled this many at a time, newest first, and the document says it is partial
	// (coverage.complete, and no verdicts) until that is done. The refill does not wait for
	// anybody to open the page: see warm.
	analysisMarketsPerRefresh = 100
	// The ledger is append-only, so bucket balances are kept as a running sum up to a checkpoint
	// plus the entries after it. The checkpoint trails the newest entry by this many ids, on the
	// ASSUMPTION that no transaction is still uncommitted once that many later entries exist
	// (about half a day of trading at the September 2026 rate).
	analysisLedgerLag = 5000
	// Orders older than this in a round that never got a result are no longer looked for. It is
	// also how far back, by closes_at, newly settled rounds are looked for first. That is only a
	// short cut: the list is checked against the table's own count every refresh, and the whole
	// table is listed whenever the short cut comes up short.
	analysisUnsettledFor = 48 * time.Hour
)

// analysisCache is everything kept between requests.
type analysisCache struct {
	refresh sync.Mutex // held by the one request that is recomputing

	mu      sync.Mutex // guards doc, at and lastErr
	doc     *analysis.Document
	at      time.Time
	lastErr string // why the last refresh failed; "" if it did not

	// below: touched only while holding refresh
	markets map[int64]analysis.Market      // every settled market seen
	facts   map[int64]analysis.MarketFacts // the ones read AND reconciled, by market id
	// Settled markets read and held back: their settlement rows do not (yet) match what was held
	// at the close, with the reason. Never cached as facts; read again on later refreshes.
	unreconciled map[int64]string
	ctx          context.Context // the service's: a refresh in flight stops when the service does, so a restart is not held up by it
	listed       bool            // the whole market table has been listed once
	base         map[int64]int64 // ledger account -> balance up to checkpoint
	checkpoint   int64
	gate         analysis.GateSettings
	// The gate decisions already stored, by strategy version: the close of the last window each
	// covers. Read from metric_snapshot once, then kept up to date by what this process writes,
	// so a decision is stored once however often the document is recomputed.
	snapped      map[int64]int64
	snappedKnown bool
}

func newAnalysisCache() *analysisCache {
	return &analysisCache{markets: map[int64]analysis.Market{}, facts: map[int64]analysis.MarketFacts{}, unreconciled: map[int64]string{}, base: map[int64]int64{},
		snapped: map[int64]int64{}, ctx: context.Background()}
}

// AnalysisRoutes mounts GET /api/analysis and starts the loop that keeps its document warm. The
// route reads and nothing else. The refresh behind it writes ONE thing: each gate decision it
// makes on a complete document goes to metric_snapshot, so the bar a version was judged against
// is on record (analysis.Snapshots). ctx is the service's own context: the loop, and any refresh
// in flight, end with it. gate is the operator's settings for the promotion rule.
func AnalysisRoutes(ctx context.Context, mux *http.ServeMux, db *store.Store, gate analysis.GateSettings) {
	c := newAnalysisCache()
	c.ctx, c.gate = ctx, gate
	mux.HandleFunc("GET /api/analysis", c.handle(db))
	if db != nil {
		go warm(ctx, analysisEvery, func() { c.refreshNow(db) })
	}
}

func (c *analysisCache) handle(db *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		doc, reason := c.get(db)
		if doc != nil {
			writeJSON(w, doc)
			return
		}
		// Nothing has ever been computed and the try just now failed. Still JSON, still the whole
		// shape with every list empty, and the reason: a page that only sees "503" shows a blank
		// panel and hides the failure from the person looking.
		none := analysis.Build(analysis.Inputs{})
		none.Stale, none.StaleReason = true, "no analysis has been computed yet: "+reason
		none.Coverage.Complete = false // nothing has been read, which is not the same as nothing to read
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(none)
	}
}

// warm keeps the document fresh without anybody asking for it: one refresh now, then one every
// `every` until ctx ends. The refresh used to run only when the page was open, so after a
// restart the history refilled at 100 markets per minute OF PAGE VIEWING, and a glance at the
// page after a deploy showed the newest few windows for as long as nobody kept it open. Now the
// refill starts with the process and goes on until coverage is complete, and the same loop then
// picks up each newly settled window.
//
// It stays out of the traders' way. It only reads. It runs one query at a time, so it holds at
// most one of the pool's connections. Each refresh is bounded (analysisMarketsPerRefresh markets
// and analysisTimeout), it shares no lock with the runners, and a panic in it is caught here: an
// unrecovered panic in a goroutine would take the whole process down, traders included.
func warm(ctx context.Context, every time.Duration, refresh func()) {
	once := func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("analysis refresh panicked", "panic", fmt.Sprint(r))
			}
		}()
		refresh()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		once()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *analysisCache) current() (*analysis.Document, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.doc, !c.at.IsZero() && time.Since(c.at) < analysisEvery
}

// get serves the cached document, recomputing it first if it is over a minute old (with the warm
// loop running that is rare: a request normally finds a fresh one). Only one caller recomputes;
// the others get the document as it stands. If the refresh fails the old document is served,
// marked stale with the reason; if there is no old one the answer is nil and the reason (a 503
// that says why) until the next minute's try.
func (c *analysisCache) get(db *store.Store) (*analysis.Document, string) {
	doc, fresh := c.current()
	if fresh {
		return doc, c.reason()
	}
	if !c.refresh.TryLock() {
		if doc != nil {
			return doc, ""
		}
		c.refresh.Lock() // nothing to serve yet: wait for the first one
	}
	defer c.refresh.Unlock()
	if doc, fresh = c.current(); fresh {
		return doc, c.reason()
	}
	return c.recompute(db), c.reason()
}

// refreshNow is the warm loop's turn: recompute whatever the document's age, unless a request is
// already doing so.
func (c *analysisCache) refreshNow(db *store.Store) {
	if !c.refresh.TryLock() {
		return
	}
	defer c.refresh.Unlock()
	c.recompute(db)
}

func (c *analysisCache) reason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// recompute runs one refresh and stores what came of it. The caller holds c.refresh.
func (c *analysisCache) recompute(db *store.Store) *analysis.Document {
	// Not a request's context: a refresh should finish even if the browser that asked goes away.
	ctx, cancel := context.WithTimeout(c.ctx, analysisTimeout)
	defer cancel()
	next, err := c.compute(ctx, db)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = time.Now() // a failure is not retried until the next minute either
	if err != nil {
		slog.Warn("analysis refresh failed", "err", err)
		c.lastErr = err.Error()
		if c.doc == nil {
			return nil
		}
		stale := *c.doc
		stale.Stale, stale.StaleReason = true, "the last refresh failed; this was computed earlier: "+err.Error()
		c.doc = &stale
		return c.doc
	}
	c.doc, c.lastErr = &next, ""
	return c.doc
}

// compute reads what is new and combines it with what is cached. Whatever it manages to read
// before an error stays cached: a settled market whose money is all there is never read twice.
func (c *analysisCache) compute(ctx context.Context, db *store.Store) (analysis.Document, error) {
	var none analysis.Document
	now := time.Now()
	// The unsettled windows are listed BEFORE the settled markets. Each statement sees its own
	// moment, so a result landing between the two leaves its window marked unsettled for one more
	// minute. The other order would score that window with a coin missing.
	incomplete, err := db.AnalysisOpenWindows(ctx, now)
	if err != nil {
		return none, err
	}
	if err := c.listSettled(ctx, db, now); err != nil {
		return none, err
	}
	// The buckets come before the windows: a window's read counts every listed version's
	// journal rows on it, so it needs their ids. A bucket seeded between here and the leaderboard
	// is read next minute, as before.
	buckets, accounts, err := db.AnalysisBuckets(ctx)
	if err != nil {
		return none, err
	}

	// Read the windows not yet read, whole windows at a time, newest first; the windows tried
	// before and held back come after those, so that a round whose settlement is never written
	// cannot use up the budget every minute and starve the refill behind it.
	pending := map[int64][]analysis.Market{}
	for id, m := range c.markets {
		if _, done := c.facts[id]; !done {
			pending[m.Closes] = append(pending[m.Closes], m)
		}
	}
	windows := readOrder(pending, c.unreconciled)
	if len(windows) > 0 {
		versions, err := db.AnalysisModelVersions(ctx)
		if err != nil {
			return none, err
		}
		all := versionIDs(buckets)
		read := 0
		for _, w := range windows {
			if read >= analysisMarketsPerRefresh {
				break
			}
			facts, unready, err := db.AnalysisWindow(ctx, versions, all, pending[w])
			if err != nil {
				return none, err
			}
			read += len(pending[w]) // a market tried and held back cost its queries too
			for _, f := range facts {
				c.facts[f.ID] = f
				delete(c.unreconciled, f.ID)
			}
			for id, why := range unready {
				if _, known := c.unreconciled[id]; !known {
					slog.Warn("analysis: a settled market does not reconcile with its settlement rows; its window is left out and it will be read again", "market", id, "why", why)
				}
				c.unreconciled[id] = why
			}
			if len(unready) == 0 {
				delete(pending, w)
			}
		}
	}
	for w := range pending { // settled, but not read yet or held back: leave the whole window out rather than score part of it
		incomplete[w] = true
	}

	trials, err := db.AnalysisTrials(ctx)
	if err != nil {
		return none, err
	}
	unsettledSells, openCost, err := db.AnalysisUnsettled(ctx, now.Add(-analysisUnsettledFor))
	if err != nil {
		return none, err
	}
	cash, err := c.balances(ctx, db, buckets, accounts)
	if err != nil {
		return none, err
	}
	book := map[int64]int64{}
	for _, b := range buckets {
		if !b.Frozen {
			book[b.ID] = cash[accounts[b.ID]] + openCost[b.ID]
		}
	}

	in := analysis.Inputs{ComputedAt: float64(now.UnixMilli()) / 1000, Incomplete: incomplete, Buckets: buckets, BookCents: book,
		UnsettledSells: unsettledSells, MarketsSettled: len(c.markets), Unreconciled: len(c.unreconciled), Trials: trials, Gate: c.gate}
	for _, f := range c.facts {
		in.Facts = append(in.Facts, f)
	}
	doc := analysis.Build(in)
	c.storeDecisions(ctx, db, doc)
	return doc, nil
}

// storeDecisions writes the gate decisions of a complete document that are new: a version's is
// new when the last window it covers has moved past the last one stored for it. A failure here
// is logged and does not fail the refresh (the document is still right; the record is behind by
// a minute), and nothing is marked stored until the database says it is, so the next refresh
// tries again. Which decisions exist is read from the table the first time, and then kept here.
func (c *analysisCache) storeDecisions(ctx context.Context, db *store.Store, doc analysis.Document) {
	snaps := analysis.Snapshots(doc)
	if len(snaps) == 0 {
		return
	}
	if !c.snappedKnown {
		latest, err := db.MetricSnapshotLatest(ctx)
		if err != nil {
			slog.Warn("analysis: could not read which gate decisions are stored; none written this refresh", "err", err)
			return
		}
		c.snapped, c.snappedKnown = latest, true
	}
	fresh := newDecisions(snaps, c.snapped)
	if len(fresh) == 0 {
		return
	}
	n, err := db.InsertMetricSnapshots(ctx, fresh)
	if err != nil {
		slog.Warn("analysis: gate decisions not stored; they will be tried again next refresh", "decisions", len(fresh), "err", err)
		return
	}
	for _, s := range fresh {
		c.snapped[s.VersionID] = s.LastClose
	}
	slog.Info("analysis: gate decisions stored", "written", n, "decided", len(fresh))
}

// newDecisions is the snapshots whose last window is past what is stored for their version.
func newDecisions(snaps []analysis.Snapshot, stored map[int64]int64) []analysis.Snapshot {
	var out []analysis.Snapshot
	for _, s := range snaps {
		if s.LastClose > stored[s.VersionID] {
			out = append(out, s)
		}
	}
	return out
}

// versionIDs is the distinct strategy versions of some buckets, in order.
func versionIDs(buckets []analysis.Bucket) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, b := range buckets {
		if !seen[b.VersionID] {
			seen[b.VersionID] = true
			out = append(out, b.VersionID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// readOrder is the order the pending windows (by closes) are read in: the ones never tried, newest
// first, then the ones with a market that was tried before and held back, newest first.
func readOrder(pending map[int64][]analysis.Market, heldBack map[int64]string) []int64 {
	again := map[int64]bool{}
	windows := make([]int64, 0, len(pending))
	for w, markets := range pending {
		windows = append(windows, w)
		for _, m := range markets {
			if _, held := heldBack[m.ID]; held {
				again[w] = true
			}
		}
	}
	sort.Slice(windows, func(i, j int) bool {
		if again[windows[i]] != again[windows[j]] {
			return again[windows[j]]
		}
		return windows[i] > windows[j]
	})
	return windows
}

// listSettled brings c.markets up to date with the rounds that have a result. The table's own
// count is the test: while it equals what is held nothing is listed at all, which is fourteen
// minutes in fifteen. When it does not, the rounds that closed in the last analysisUnsettledFor
// are listed first (everything, the first time), and if the count is still not reached the
// whole table is. No result can be missed for having been stored late or out of order.
func (c *analysisCache) listSettled(ctx context.Context, db *store.Store, now time.Time) error {
	n, err := db.AnalysisSettledCount(ctx)
	if err != nil {
		return err
	}
	if c.listed && n == len(c.markets) {
		return nil
	}
	since := time.Unix(0, 0)
	if c.listed {
		since = now.Add(-analysisUnsettledFor)
	}
	for {
		settled, err := db.AnalysisMarkets(ctx, since)
		if err != nil {
			return err
		}
		for _, m := range settled {
			c.markets[m.ID] = m
		}
		if since.Unix() == 0 {
			c.listed = true
			return nil
		}
		if len(c.markets) >= n {
			return nil
		}
		since = time.Unix(0, 0)
	}
}

// balances is the ledger cash of every live bucket, without re-adding the whole ledger each
// minute: see analysisLedgerLag.
func (c *analysisCache) balances(ctx context.Context, db *store.Store, buckets []analysis.Bucket, accounts map[int64]int64) (map[int64]int64, error) {
	var live, unseen []int64
	for _, b := range buckets {
		if b.Frozen {
			continue
		}
		a := accounts[b.ID]
		live = append(live, a)
		if _, ok := c.base[a]; !ok {
			unseen = append(unseen, a)
		}
	}
	newest, err := db.LedgerMaxEntryID(ctx)
	if err != nil {
		return nil, err
	}
	first, err := db.LedgerSums(ctx, unseen, 0, c.checkpoint) // a bucket new to us: catch it up to the checkpoint
	if err != nil {
		return nil, err
	}
	for _, a := range unseen {
		c.base[a] = first[a]
	}
	if target := newest - analysisLedgerLag; target > c.checkpoint {
		more, err := db.LedgerSums(ctx, live, c.checkpoint, target)
		if err != nil {
			return nil, err
		}
		for a, cents := range more {
			c.base[a] += cents
		}
		c.checkpoint = target
	}
	tail, err := db.LedgerSums(ctx, live, c.checkpoint, newest)
	if err != nil {
		return nil, err
	}
	out := map[int64]int64{}
	for _, a := range live {
		out[a] = c.base[a] + tail[a]
	}
	return out, nil
}
