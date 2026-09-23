package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	k3 "github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// ================================================================================================
// The fake ledger and the rig the tests above stand on.
// ================================================================================================

// fakeStore is a ledger in memory behind the Store3 seam: enough of the real tables' rules for
// the runner's tests, and a way to make any call fail, block, lose its answer or land late. It
// proves nothing about the real SQL (store/sim3_db_test.go and the S5 dev checklist do that).
//
// Its reads follow the real queries' RULES: BucketFills returns the fills of markets with no
// result and of lots that are NET open with no settlement row; OpenQty is bought less sold.

type fakeVersion struct {
	store.VersionRow
}

type fakeBucket struct {
	store.SimBucket
	strategy string
	seed     int64
	adjust   int64 // money moved under the engine, for the cash-check tests
	reaped   int64
}

type fakeMarket struct {
	id           int64
	ticker, coin string
	closes       time.Time
	strike       float64
	result       string
}

type fakeOrder struct {
	id       int64
	marketID int64
	at       time.Time
	row      store.OrderRow
	detail   []byte
	fillIDs  []int64
}

type fakeSettlement struct {
	marketID int64
	setup    store.SimSetup
	row      store.SettlementRow
}

// behaviour is what a hooked call does instead of simply working.
type behaviour struct {
	err   error         // returned to the caller
	land  bool          // a write: make it all the same, now (the answer was lost)
	later bool          // a write: keep it, and make it when the test calls landLater (the late commit)
	block chan struct{} // wait for this to close before doing anything
	enter chan struct{} // sent to when the call has been entered
	// expire: the call takes its caller's whole budget, and fails with the context's error when the
	// budget runs out, as the fault injector's "delay" kind and a slow database do. With land set, a
	// write is made all the same (it committed as the client gave up).
	expire bool
}

type fakeStore struct {
	mu          sync.Mutex
	versions    []fakeVersion
	buckets     []*fakeBucket
	markets     map[int64]*fakeMarket
	orders      []*fakeOrder
	settlements []fakeSettlement
	skims       []store.Skim // every recorded skim, in order; the newest hwm_after per bucket is the mark
	policy      store.SkimPolicy
	decisions   int
	state       map[string][]byte
	calls       map[string]int
	created     []string // every bucket row, deposit, seed and event EnsureSimSetup made
	setupNames  [][]string
	closed      []string
	pending     []func()
	nextID      int64
	on          map[string]func(n int) behaviour // by method name; n is the call's number, from 1
}

var fakeSetup = store.SimSetup{ActorID: 7, VenueLedgerID: 11, FeesLedgerID: 12, PoolLedgerID: 13, OwnersLedgerID: 14}

func newFakeStore() *fakeStore {
	return &fakeStore{markets: map[int64]*fakeMarket{}, state: map[string][]byte{}, calls: map[string]int{}, on: map[string]func(int) behaviour{}, nextID: 1000,
		policy: store.SkimPolicy{ID: 1}} // every rate zero, as migration 0008 seeds it
}

func (s *fakeStore) id() int64 { s.nextID++; return s.nextID }

// hook counts the call and applies its behaviour. It returns the behaviour for a write to act on.
//
// A call whose context is already done fails at once with the context's error, and does nothing,
// as pgx does (the pool's Acquire and pgconn both check ctx before sending anything). Without this
// the fake could answer a caller that the real store never would.
func (s *fakeStore) hook(ctx context.Context, op string) behaviour {
	s.mu.Lock()
	s.calls[op]++
	n, fn := s.calls[op], s.on[op]
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return behaviour{err: err}
	}
	if fn == nil {
		return behaviour{}
	}
	b := fn(n)
	if b.enter != nil {
		b.enter <- struct{}{}
	}
	if b.block != nil {
		<-b.block
	}
	if b.expire {
		<-ctx.Done()
		b.err = ctx.Err()
	}
	return b
}

func (s *fakeStore) ordersNow() []*fakeOrder {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fakeOrder{}, s.orders...)
}

func (s *fakeStore) count(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// landLater makes every write that was kept back.
func (s *fakeStore) landLater() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, do := range s.pending {
		do()
	}
	s.pending = nil
}

func (s *fakeStore) addVersion(name, status string, params json.RawMessage) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := fakeVersion{store.VersionRow{Name: name, ID: s.id(), Status: status, Params: params}}
	s.versions = append(s.versions, v)
	return v.ID
}

func (s *fakeStore) setStatus(name, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.versions {
		if s.versions[i].Name == name {
			s.versions[i].Status = status
		}
	}
}

func (s *fakeStore) version(id int64) *fakeVersion {
	for i := range s.versions {
		if s.versions[i].ID == id {
			return &s.versions[i]
		}
	}
	return nil
}

func (s *fakeStore) addMarket(id int64, ticker, coin string, closes time.Time, strike float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markets[id] = &fakeMarket{id: id, ticker: ticker, coin: coin, closes: closes, strike: strike}
}

func (s *fakeStore) setResult(marketID int64, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markets[marketID].result = result
}

func bucketCents(action string, f store.FillRow) int64 {
	if action == "buy" {
		return -(f.PremiumCents + f.FeeCents)
	}
	return f.PremiumCents - f.FeeCents
}

// cash is a bucket's ledger sum. Callers hold s.mu.
func (s *fakeStore) cash(b *fakeBucket) int64 {
	c := b.seed + b.adjust - b.reaped
	for _, o := range s.orders {
		if o.row.BucketID == b.ID {
			for _, f := range o.row.Fills {
				c += bucketCents(o.row.Action, f)
			}
		}
	}
	for _, x := range s.settlements {
		if x.row.BucketID == b.ID {
			c += x.row.PayoutCents
		}
	}
	for _, k := range s.skims {
		if k.Bucket.ID == b.ID {
			c -= k.Taken()
		}
	}
	return c
}

// wipe is reset_sim as the fake sees it: every sim bucket, order, settlement and skim goes;
// the versions and the markets stay.
func (s *fakeStore) wipe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets, s.orders, s.settlements, s.skims, s.closed = nil, nil, nil, nil, nil
}

// setPolicy is the owner setting the four rates on the buckets page.
func (s *fakeStore) setPolicy(winnings, replenish, tax, fees int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = store.SkimPolicy{ID: int64(len(s.skims) + 2), Winnings: winnings, Replenish: replenish, Tax: tax, Fees: fees}
}

func (s *fakeStore) ledgerCash(bucketID int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.buckets {
		if b.ID == bucketID {
			return s.cash(b)
		}
	}
	return 0
}

func (s *fakeStore) adjust(bucketID, cents int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.buckets {
		if b.ID == bucketID {
			b.adjust += cents
		}
	}
}

type lotKey struct {
	bucket, market int64
	side           string
}

// net is bought less sold per lot, and which lots have a settlement row. Callers hold s.mu.
func (s *fakeStore) net() (map[lotKey]int, map[lotKey]bool) {
	net, settled := map[lotKey]int{}, map[lotKey]bool{}
	for _, o := range s.orders {
		for _, f := range o.row.Fills {
			k := lotKey{o.row.BucketID, o.marketID, o.row.Side}
			if o.row.Action == "buy" {
				net[k] += f.Qty
			} else {
				net[k] -= f.Qty
			}
		}
	}
	for _, x := range s.settlements {
		settled[lotKey{x.row.BucketID, x.marketID, x.row.Side}] = true
	}
	return net, settled
}

func has(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// ---- Store3 -----------------------------------------------------------------------------------

func (s *fakeStore) EnsureSimSetup(ctx context.Context, prefix, family string, version int, names []string, seedCents int64) (store.SimSetup, error) {
	if b := s.hook(ctx, "EnsureSimSetup"); b.err != nil {
		return store.SimSetup{}, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setupNames = append(s.setupNames, append([]string{}, names...))
	out := fakeSetup
	out.Buckets = map[string]store.SimBucket{}
	for _, name := range names {
		bucketName := fmt.Sprintf("%s %s v%d", prefix, name, version)
		var found *fakeBucket
		for _, b := range s.buckets {
			if b.Name == bucketName {
				found = b
			}
		}
		if found == nil {
			var versionID int64
			for _, v := range s.versions {
				if v.Name == name {
					versionID = v.ID
				}
			}
			if versionID == 0 {
				return out, fmt.Errorf("strategy %s is not registered", name)
			}
			found = &fakeBucket{SimBucket: store.SimBucket{ID: s.id(), Name: bucketName, LedgerAccountID: s.id(), VersionID: versionID}, strategy: name, seed: seedCents}
			s.buckets = append(s.buckets, found)
			s.created = append(s.created, "bucket "+bucketName, "deposit "+bucketName, "seed "+bucketName, "seeded "+bucketName)
		}
		sb := found.SimBucket
		sb.CashCents = s.cash(found)
		out.Buckets[name] = sb
	}
	return out, nil
}

func (s *fakeStore) TradableVersions(ctx context.Context, family string, version int) ([]store.VersionRow, error) {
	if b := s.hook(ctx, "TradableVersions"); b.err != nil {
		return nil, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []store.VersionRow{}
	for _, v := range s.versions {
		if v.Status == "probation" || v.Status == "active" {
			out = append(out, v.VersionRow)
		}
	}
	return out, nil
}

func (s *fakeStore) HeldBuckets(ctx context.Context, family string, version int) ([]store.HeldBucket, error) {
	if b := s.hook(ctx, "HeldBuckets"); b.err != nil {
		return nil, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []store.HeldBucket{}
	for _, b := range s.buckets {
		if b.Frozen {
			continue
		}
		v := s.version(b.VersionID)
		h := store.HeldBucket{SimBucket: b.SimBucket, Strategy: b.strategy, VersionStatus: v.Status, Params: v.Params}
		h.CashCents = s.cash(b)
		out = append(out, h)
	}
	return out, nil
}

func (s *fakeStore) BucketCash(ctx context.Context, ids []int64) (map[int64]int64, error) {
	if b := s.hook(ctx, "BucketCash"); b.err != nil {
		return nil, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64]int64{}
	for _, b := range s.buckets {
		if has(ids, b.ID) {
			out[b.ID] = s.cash(b)
		}
	}
	return out, nil
}

func (s *fakeStore) BucketFills(ctx context.Context, ids []int64) ([]store.BucketFill, error) {
	if b := s.hook(ctx, "BucketFills"); b.err != nil {
		return nil, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	net, settled := s.net()
	out := []store.BucketFill{}
	for _, o := range s.orders {
		if !has(ids, o.row.BucketID) {
			continue
		}
		m := s.markets[o.marketID]
		k := lotKey{o.row.BucketID, o.marketID, o.row.Side}
		if m.result != "" && !(net[k] > 0 && !settled[k]) {
			continue
		}
		strike := m.strike
		for i, f := range o.row.Fills {
			out = append(out, store.BucketFill{BucketID: o.row.BucketID, MarketID: o.marketID, Ticker: m.ticker, Underlying: m.coin, ClosesAt: m.closes,
				Strike: &strike, Result: m.result, OrderID: o.id, ClientID: o.row.ClientID, Action: o.row.Action, Side: o.row.Side, Detail: o.detail,
				FillID: o.fillIDs[i], At: o.at, Qty: f.Qty, Price: f.Price, FeeCents: f.FeeCents, BucketCents: bucketCents(o.row.Action, f)})
		}
	}
	return out, nil
}

func (s *fakeStore) OpenQty(ctx context.Context, marketID int64, ids []int64) (map[[2]string]int, error) {
	if b := s.hook(ctx, "OpenQty"); b.err != nil {
		return nil, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	net, _ := s.net()
	out := map[[2]string]int{}
	for k, n := range net {
		if k.market == marketID && has(ids, k.bucket) && n > 0 {
			out[store.LotKey(k.bucket, k.side)] = n
		}
	}
	return out, nil
}

func (s *fakeStore) MarketResults(ctx context.Context, ids []int64) (map[int64]string, error) {
	if b := s.hook(ctx, "MarketResults"); b.err != nil {
		return nil, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64]string{}
	for _, id := range ids {
		if m := s.markets[id]; m != nil && m.result != "" {
			out[id] = m.result
		}
	}
	return out, nil
}

func (s *fakeStore) OrdersRecorded(ctx context.Context, clientIDs []string) (map[string]bool, error) {
	if b := s.hook(ctx, "OrdersRecorded"); b.err != nil {
		return nil, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for _, o := range s.orders {
		for _, id := range clientIDs {
			if o.row.ClientID == id {
				out[id] = true
			}
		}
	}
	return out, nil
}

func (s *fakeStore) SettlementsRecorded(ctx context.Context, marketID int64, ids []int64) (map[[2]string]bool, error) {
	if b := s.hook(ctx, "SettlementsRecorded"); b.err != nil {
		return nil, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[[2]string]bool{}
	for _, x := range s.settlements {
		if x.marketID == marketID && has(ids, x.row.BucketID) {
			out[store.LotKey(x.row.BucketID, x.row.Side)] = true
		}
	}
	return out, nil
}

func unique(what string) error {
	return &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint: " + what}
}

func serverSaysNo() error {
	return &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "53100", Message: "the fake server refuses this write"}
}

func (s *fakeStore) RecordOrders(ctx context.Context, setup store.SimSetup, r store.StepRecord3) error {
	b := s.hook(ctx, "RecordOrders")
	do := func() error { return s.writeOrders(setup, r) }
	switch {
	case b.later:
		s.mu.Lock()
		s.pending = append(s.pending, func() { _ = s.writeOrdersLocked(setup, r) })
		s.mu.Unlock()
		return b.err
	case b.land:
		if err := do(); err != nil {
			return err
		}
		return b.err
	case b.err != nil:
		return b.err
	}
	return do()
}

func (s *fakeStore) writeOrders(setup store.SimSetup, r store.StepRecord3) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeOrdersLocked(setup, r)
}

func (s *fakeStore) writeOrdersLocked(setup store.SimSetup, r store.StepRecord3) error {
	if len(r.Decisions) == 0 && len(r.Orders) == 0 {
		return fmt.Errorf("%w: nothing to record", store.ErrInvalidStep)
	}
	if len(r.Orders) > 0 && (setup.ActorID == 0 || setup.VenueLedgerID == 0 || setup.FeesLedgerID == 0) {
		return fmt.Errorf("%w: the sim setup is empty", store.ErrInvalidStep)
	}
	var add []*fakeOrder
	for _, o := range r.Orders {
		for _, old := range s.orders {
			if old.row.ClientID == o.ClientID {
				return unique("trade_order_client_order_id")
			}
		}
		want, err := store.StatusFromFills(o.Qty, o.Fills)
		if err != nil || (o.Status != want && !(o.Status == "rejected" && want == "cancelled")) {
			return fmt.Errorf("%w: %s status %q against %q (%v)", store.ErrInvalidStep, o.ClientID, o.Status, want, err)
		}
		detail, err := json.Marshal(o.Detail)
		if err != nil {
			return fmt.Errorf("%w: %v", store.ErrInvalidStep, err)
		}
		fo := &fakeOrder{id: s.id(), marketID: r.MarketID, at: r.At, row: o, detail: detail}
		for range o.Fills {
			fo.fillIDs = append(fo.fillIDs, s.id())
		}
		add = append(add, fo)
	}
	s.orders = append(s.orders, add...)
	s.decisions += len(r.Decisions)
	return nil
}

func (s *fakeStore) RecordSettlements(ctx context.Context, setup store.SimSetup, marketID int64, at time.Time, rows []store.SettlementRow) error {
	if len(rows) == 0 {
		return nil
	}
	b := s.hook(ctx, "RecordSettlements")
	if b.err != nil && !b.land {
		return b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The real foreign keys: created_by references actor, and the venue and pool accounts must exist.
	if setup.ActorID == 0 || setup.VenueLedgerID == 0 || setup.PoolLedgerID == 0 {
		return &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "23503", Message: "the fake foreign key: a settlement needs the service actor and the venue"}
	}
	for _, row := range rows {
		for _, x := range s.settlements {
			if x.marketID == marketID && x.row.BucketID == row.BucketID && x.row.Side == row.Side {
				return unique("settlement (market_id, bucket_id, side)")
			}
		}
	}
	for _, row := range rows {
		s.settlements = append(s.settlements, fakeSettlement{marketID: marketID, setup: setup, row: row})
	}
	return b.err
}

func (s *fakeStore) CloseBucket(ctx context.Context, setup store.SimSetup, b store.SimBucket, reason string, restake bool, life int, seedCents int64) (store.SimBucket, error) {
	if hb := s.hook(ctx, "CloseBucket"); hb.err != nil {
		return b, hb.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if setup.ActorID == 0 || setup.PoolLedgerID == 0 {
		return b, errors.New("the fake foreign key: closing a bucket needs the service actor and the pool")
	}
	var closed *fakeBucket
	for _, fb := range s.buckets {
		if fb.ID == b.ID {
			fb.reaped += s.cash(fb)
			fb.Frozen = true
			s.closed = append(s.closed, fb.Name)
			closed = fb
		}
	}
	b.Frozen = true
	if !restake {
		return b, nil
	}
	if closed == nil {
		return b, errors.New("restake of a bucket the fake does not hold")
	}
	return s.seedLife(closed, life, seedCents), nil
}

// seedLife is the store's: "<base> life N", the same version, a fresh seed from the pool.
func (s *fakeStore) seedLife(prev *fakeBucket, life int, seedCents int64) store.SimBucket {
	base := prev.Name
	if i := strings.Index(base, " life "); i >= 0 {
		base = base[:i]
	}
	next := &fakeBucket{SimBucket: store.SimBucket{ID: s.id(), Name: fmt.Sprintf("%s life %d", base, life), LedgerAccountID: s.id(), VersionID: prev.VersionID}, strategy: prev.strategy, seed: seedCents}
	s.buckets = append(s.buckets, next)
	s.created = append(s.created, "bucket "+next.Name, "seed "+next.Name)
	sb := next.SimBucket
	sb.CashCents = seedCents
	return sb
}

func (s *fakeStore) RestakeBucket(ctx context.Context, setup store.SimSetup, versionID int64, seedCents int64) (store.SimBucket, error) {
	if hb := s.hook(ctx, "RestakeBucket"); hb.err != nil {
		return store.SimBucket{}, hb.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var prev *fakeBucket
	for _, fb := range s.buckets { // appended in order: the last is the newest
		if fb.VersionID == versionID {
			prev = fb
		}
	}
	if prev == nil {
		return store.SimBucket{}, store.ErrNoBucketEver
	}
	if !prev.Frozen {
		return store.SimBucket{}, store.ErrBucketHeld
	}
	return s.seedLife(prev, lifeOf(prev.Name)+1, seedCents), nil
}

func (s *fakeStore) SettledStreak(ctx context.Context, bucketID int64) (int, error) {
	if b := s.hook(ctx, "SettledStreak"); b.err != nil {
		return 0, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := len(s.settlements) - 1; i >= 0; i-- { // appended in time order
		x := s.settlements[i]
		if x.row.BucketID != bucketID {
			continue
		}
		if x.row.PayoutCents > 0 {
			break
		}
		n++
	}
	return n, nil
}

func (s *fakeStore) HighWaterMark(ctx context.Context, bucketID int64, seedCents int64) (int64, error) {
	if b := s.hook(ctx, "HighWaterMark"); b.err != nil {
		return 0, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.skims) - 1; i >= 0; i-- {
		if k := s.skims[i]; k.Bucket.ID == bucketID {
			return k.BookCents - k.Taken(), nil
		}
	}
	return seedCents, nil
}

func (s *fakeStore) CurrentSkimPolicy(ctx context.Context) (store.SkimPolicy, error) {
	if b := s.hook(ctx, "CurrentSkimPolicy"); b.err != nil {
		return store.SkimPolicy{}, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy, nil
}

func (s *fakeStore) RecordSkim(ctx context.Context, setup store.SimSetup, k store.Skim) error {
	if b := s.hook(ctx, "RecordSkim"); b.err != nil {
		return b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if setup.ActorID == 0 || setup.PoolLedgerID == 0 {
		return errors.New("the fake foreign key: a skim needs the service actor and the pools")
	}
	if k.BookCents <= k.HWMBefore {
		return errors.New("the fake check constraint: gain_cents > 0")
	}
	s.skims = append(s.skims, k)
	return nil
}

func (s *fakeStore) SaveEngineState(ctx context.Context, series string, state any) error {
	if b := s.hook(ctx, "SaveEngineState"); b.err != nil {
		return b.err
	}
	blob, err := json.Marshal(state)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state[series] = blob
	return err
}

func (s *fakeStore) LoadEngineState(ctx context.Context, series string, into any) (bool, error) {
	if b := s.hook(ctx, "LoadEngineState"); b.err != nil {
		return false, b.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	blob, ok := s.state[series]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(blob, into)
}

// ---- the rig ----------------------------------------------------------------------------------

// PLACEHOLDER NUMBERS. Nothing here is a measurement: lambda 1 and staleness 0 are chosen so that
// the model's view decides alone and the arithmetic is easy to follow. They are labelled
// "placeholder", reach an engine only through NewPlumbingEngine, and the runner accepts them only
// because the rig's database is called "rig_dev".
func plumbing(t *testing.T, name string) json.RawMessage {
	t.Helper()
	ph := func(v float64) k3.Provenance {
		return k3.Provenance{Kind: k3.KindPlaceholder, Value: v, Note: "PLACEHOLDER for tests: not a measurement"}
	}
	m := k3.Measured{Lambda: ph(1), StaleCost: ph(0), StaleCostSell: ph(0), DriftTol: ph(0.05)}
	p, err := k3.PlumbingScalper(m)
	if name == "Value" {
		p, err = k3.PlumbingValue(m)
	}
	if err != nil {
		t.Fatal(err)
	}
	return marked(t, p)
}

func marked(t *testing.T, p k3.Params) json.RawMessage {
	t.Helper()
	blob, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatal(err)
	}
	m[DevPlumbingKey] = DevPlumbingMark
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var t0 = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

const (
	mktA   = int64(501) // BTC, closes t0 + 15 min
	mktB   = int64(502) // ETH, the same close
	strike = 100.0
)

type rig struct {
	t      *testing.T
	s      *fakeStore
	r      *Runner3
	mu     sync.Mutex
	clock  time.Time
	evalID int64
}

func (g *rig) now() time.Time { g.mu.Lock(); defer g.mu.Unlock(); return g.clock }
func (g *rig) advance(d time.Duration) {
	g.mu.Lock()
	g.clock = g.clock.Add(d)
	g.mu.Unlock()
}

var rigCoins = []Coin3{
	{Coin: "BTC", Series: "KXBTC15M", Product: "BTC-USD", Cal: k3.Calibration{OffsetPct: 0.000057, SDPct: 0.000144, DefaultSigma: 8e-5, Decimals: 2}},
	{Coin: "ETH", Series: "KXETH15M", Product: "ETH-USD", Cal: k3.Calibration{OffsetPct: 0.000057, SDPct: 0.000144, DefaultSigma: 8e-5, Decimals: 2}},
}

// newRig is a fake ledger with the Scalper plumbing version on probation and two open markets.
func newRig(t *testing.T) *rig {
	t.Helper()
	g := &rig{t: t, s: newFakeStore(), clock: t0.Add(5 * time.Minute)}
	g.s.addVersion("Scalper", "probation", plumbing(t, "Scalper"))
	g.s.addMarket(mktA, "KXBTC15M-A", "BTC", t0.Add(15*time.Minute), strike)
	g.s.addMarket(mktB, "KXETH15M-A", "ETH", t0.Add(15*time.Minute), strike)
	return g
}

func (g *rig) options(on bool) Options3 {
	return Options3{On: on, DatabaseName: "rig_dev", Now: g.now}
}

// start builds a runner over the rig's store, as main would.
func (g *rig) start(on bool) *Runner3 {
	g.t.Helper()
	r, err := NewRunner3(context.Background(), g.s, rigCoins, g.options(on))
	if err != nil {
		g.t.Fatal(err)
	}
	g.r = r
	return r
}

func book(yesBid string, yesSize string, noBid string, noSize string) kalshi.Quotes {
	return kalshi.Quotes{YesBid: yesBid, NoBid: noBid, YesBids: [][2]string{{yesBid, yesSize}}, NoBids: [][2]string{{noBid, noSize}}}
}

func (g *rig) info(marketID int64) (kalshi.MarketInfo, time.Time, string) {
	m := g.s.markets[marketID]
	k := strike
	return kalshi.MarketInfo{Ticker: m.ticker, FloorStrike: &k}, m.closes, m.coin
}

// look is one second of one market as the sink drives it: Inputs (with the second engine's
// then Step.
func (g *rig) look(marketID int64, price string, q kalshi.Quotes) {
	g.t.Helper()
	g.lookWith(g.r, marketID, price, q)
}

func (g *rig) lookWith(r *Runner3, marketID int64, price string, q kalshi.Quotes) {
	g.t.Helper()
	info, closes, coin := g.info(marketID)
	g.evalID++
	at := g.now()
	r.Inputs(coin, info, closes, at, price)
	r.Step(context.Background(), coin, g.evalID, at, marketID, info, closes, q, price)
}

// The standing books: a Yes ask of 0.60 (the No bid of 0.40) and a Yes bid of 0.58.
func up(noSize string) kalshi.Quotes { return book("0.5800", "500", "0.4000", noSize) }

const (
	above = "100.3" // well above the strike: the model says Yes, the rig buys Yes
	below = "99.5"  // well below: a held Yes is worth selling at 0.58
)

func (g *rig) bucketID() int64 {
	g.t.Helper()
	ids := g.r.BucketIDs()
	if len(ids) == 0 {
		g.t.Fatal("no bucket is held")
	}
	return ids[0]
}

// memory reads an account under the runner's lock.
func (g *rig) memory(r *Runner3) (cash int64, positions []k3.Position, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.engine.Accounts {
		cash += a.CashCents
		positions = append(positions, a.Open()...)
	}
	return cash, positions, r.state
}

func (g *rig) contracts(r *Runner3) int {
	_, ps, _ := g.memory(r)
	n := 0
	for _, p := range ps {
		n += p.Contracts
	}
	return n
}

func (g *rig) wantState(want string) {
	g.t.Helper()
	if _, _, got := g.memory(g.r); got != want {
		g.r.mu.Lock()
		reason := g.r.reason
		g.r.mu.Unlock()
		g.t.Fatalf("state %q (%s), want %q", got, reason, want)
	}
}

// equalsLedger fails unless memory's cash equals the fake ledger's for every held bucket.
func (g *rig) equalsLedger(r *Runner3) {
	g.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.engine.Accounts {
		if got := g.s.ledgerCash(a.BucketID); got != a.CashCents {
			g.t.Fatalf("bucket %d: memory has %d cents, the ledger %d", a.BucketID, a.CashCents, got)
		}
	}
}

func (g *rig) settle(marketID int64, result string) {
	info, closes, coin := g.info(marketID)
	info.Result = result
	g.r.Settled(context.Background(), coin, marketID, info, closes)
}

func trade(product, price string, at time.Time) coinbase.Trade {
	return coinbase.Trade{Product: product, Price: price, At: at}
}

// within fails the test if fn has not returned in a second: "returns at once", with room for a
// loaded machine.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s did not return: it is waiting on a lock held across a database call", what)
	}
}
