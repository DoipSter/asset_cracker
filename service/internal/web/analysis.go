package web

import (
	"context"
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
	// Settled markets read per refresh. A settled round never changes, so each is read once and
	// kept; after a restart the history is refilled this many at a time, newest first, and the
	// document says it is partial (coverage.complete) until that is done.
	analysisMarketsPerRefresh = 100
	// The ledger is append-only, so bucket balances are kept as a running sum up to a checkpoint
	// plus the entries after it. The checkpoint trails the newest entry by this many ids, on the
	// ASSUMPTION that no transaction is still uncommitted once that many later entries exist
	// (about half a day of trading at the September 2026 rate).
	analysisLedgerLag = 5000
	// Orders older than this in a round that never got a result are no longer looked for.
	analysisUnsettledFor = 48 * time.Hour
)

// analysisCache is everything kept between requests.
type analysisCache struct {
	refresh sync.Mutex // held by the one request that is recomputing

	mu  sync.Mutex // guards doc and at
	doc *analysis.Document
	at  time.Time

	// below: touched only while holding refresh
	markets      map[int64]analysis.Market      // every settled market seen
	facts        map[int64]analysis.MarketFacts // the ones already read, by market id
	settledSince time.Time
	base         map[int64]int64 // ledger account -> balance up to checkpoint
	checkpoint   int64
}

// AnalysisRoutes mounts GET /api/analysis. It reads and nothing else.
func AnalysisRoutes(mux *http.ServeMux, db *store.Store) {
	c := &analysisCache{markets: map[int64]analysis.Market{}, facts: map[int64]analysis.MarketFacts{}, base: map[int64]int64{}}
	mux.HandleFunc("GET /api/analysis", func(w http.ResponseWriter, r *http.Request) {
		doc := c.get(db)
		if doc == nil {
			http.Error(w, "analysis unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, doc)
	})
}

func (c *analysisCache) current() (*analysis.Document, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.doc, !c.at.IsZero() && time.Since(c.at) < analysisEvery
}

// get serves the cached document, recomputing it first if it is over a minute old. Only one
// request recomputes; the others get the document as it stands. If the refresh fails the old
// document is served, marked stale with the reason; if there is no old one the answer is nil
// (503) until the next minute's try.
func (c *analysisCache) get(db *store.Store) *analysis.Document {
	doc, fresh := c.current()
	if fresh {
		return doc
	}
	if !c.refresh.TryLock() {
		if doc != nil {
			return doc
		}
		c.refresh.Lock() // nothing to serve yet: wait for the first one
	}
	defer c.refresh.Unlock()
	if doc, fresh = c.current(); fresh {
		return doc
	}
	// Not the request's context: a refresh should finish even if the browser that asked goes away.
	ctx, cancel := context.WithTimeout(context.Background(), analysisTimeout)
	defer cancel()
	next, err := c.compute(ctx, db)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = time.Now() // a failure is not retried until the next minute either
	if err != nil {
		slog.Warn("analysis refresh failed", "err", err)
		if c.doc == nil {
			return nil
		}
		stale := *c.doc
		stale.Stale, stale.StaleReason = true, "the last refresh failed; this was computed earlier: "+err.Error()
		c.doc = &stale
		return c.doc
	}
	c.doc = &next
	return c.doc
}

// compute reads what is new and combines it with what is cached. Whatever it manages to read
// before an error stays cached: a settled market is never read twice.
func (c *analysisCache) compute(ctx context.Context, db *store.Store) (analysis.Document, error) {
	var none analysis.Document
	now := time.Now()
	// Results are looked for again from an hour before the newest one seen, in case two were
	// stored out of order.
	since := time.Unix(0, 0)
	if !c.settledSince.IsZero() {
		since = c.settledSince.Add(-time.Hour)
	}
	settled, latest, err := db.AnalysisMarkets(ctx, since)
	if err != nil {
		return none, err
	}
	for _, m := range settled {
		c.markets[m.ID] = m
	}
	c.settledSince = latest
	incomplete, err := db.AnalysisOpenWindows(ctx, now)
	if err != nil {
		return none, err
	}

	// Read the windows not yet read, newest first, whole windows at a time.
	pending := map[int64][]analysis.Market{}
	for id, m := range c.markets {
		if _, done := c.facts[id]; !done {
			pending[m.Closes] = append(pending[m.Closes], m)
		}
	}
	windows := make([]int64, 0, len(pending))
	for w := range pending {
		windows = append(windows, w)
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i] > windows[j] })
	if len(windows) > 0 {
		versions, err := db.AnalysisModelVersions(ctx)
		if err != nil {
			return none, err
		}
		read := 0
		for _, w := range windows {
			if read >= analysisMarketsPerRefresh {
				break
			}
			facts, err := db.AnalysisWindow(ctx, versions, pending[w])
			if err != nil {
				return none, err
			}
			for _, f := range facts {
				c.facts[f.ID] = f
			}
			read += len(facts)
			delete(pending, w)
		}
	}
	for w := range pending { // settled but not read yet: leave the whole window out rather than score part of it
		incomplete[w] = true
	}

	buckets, accounts, err := db.AnalysisBuckets(ctx)
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
		UnsettledSells: unsettledSells, MarketsSettled: len(c.markets)}
	for _, f := range c.facts {
		in.Facts = append(in.Facts, f)
	}
	return analysis.Build(in), nil
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
