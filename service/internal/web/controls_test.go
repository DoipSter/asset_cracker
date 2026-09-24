package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

type fakeControls struct {
	on           bool
	set          bool
	versions     []store.Version3
	policy       store.SkimPolicy
	reset        store.ResetCounts
	resetErr     error
	ordersSaved  *bool
	statusSaved  string
	policySaved  bool
	steps        []string
	created      []store.NewVersion3
	closed       []int64
	bank         store.Bank
	paydays      []store.Payday
	scheduled    []store.Payday
	stopped      []int64
	bucketOrders []bool
	archived     []bool
	recorded     []recordedResult
}

// recordedResult is one analysis_result the fake was asked to write.
type recordedResult struct {
	key, source, sha string
	params, result   any
	from, to         time.Time
}

func (f *fakeControls) RecordAnalysisResult(_ context.Context, key, source, sha string, params any, from, to time.Time, result any) (int64, error) {
	f.recorded = append(f.recorded, recordedResult{key, source, sha, params, result, from, to})
	return int64(len(f.recorded)), nil
}

func (f *fakeControls) OrdersSetting(context.Context) (bool, bool, error) {
	return f.on, f.set, nil
}
func (f *fakeControls) SetOrdersSetting(_ context.Context, on bool) error {
	f.ordersSaved = &on
	f.on, f.set = on, true
	return nil
}
func (f *fakeControls) ListVersion3(context.Context) ([]store.Version3, error) {
	if f.versions == nil {
		return []store.Version3{}, nil
	}
	return f.versions, nil
}
func (f *fakeControls) SetVersionStatus(_ context.Context, id int64, status, _ string) error {
	if id != 7 {
		return store.ErrVersionNotFound
	}
	f.statusSaved = status
	return nil
}
func (f *fakeControls) CreateVersion3(_ context.Context, v store.NewVersion3) (int64, error) {
	if v.Name == "Taken (conventions)" {
		return 0, store.ErrVersionExists
	}
	f.created = append(f.created, v)
	return int64(100 + len(f.created)), nil
}
func (f *fakeControls) SetSimPolicy(context.Context, int, int, int, int, string) error {
	f.policySaved = true
	return nil
}
func (f *fakeControls) CurrentSkimPolicy(context.Context) (store.SkimPolicy, error) {
	return f.policy, nil
}
func (f *fakeControls) ResetSim(context.Context) (store.ResetCounts, error) {
	f.steps = append(f.steps, "reset")
	return f.reset, f.resetErr
}
func (f *fakeControls) SimAccounts(context.Context) ([]store.SimAccount, error) {
	return []store.SimAccount{{ID: 1, Kind: "common_pool", Name: "common pool (sim)", BalanceCents: 5000}, {ID: 7, Kind: "bucket", Name: "kalshi15m3 Value v3 cash", BalanceCents: 93242}}, nil
}
func (f *fakeControls) LookupBucket(_ context.Context, id int64) (store.BucketRef, error) {
	switch id {
	case 30:
		return store.BucketRef{ID: 30, VersionID: 4, Version: 2, Name: "kalshi15m2 Model v2", Status: "active"}, nil
	case 31:
		return store.BucketRef{ID: 31, VersionID: 4, Version: 2, Name: "still open", Status: "active"}, nil
	case 10:
		return store.BucketRef{ID: 10, VersionID: 4, Version: 2, Name: "old", Status: "frozen"}, nil
	case 50:
		return store.BucketRef{ID: 50, VersionID: 19, Version: 3, Name: "kalshi15m3 Scalper v3", Status: "active"}, nil
	}
	return store.BucketRef{}, store.ErrNoSuchBucket
}
func (f *fakeControls) CloseOutBucket(_ context.Context, id int64) (store.ClosedOut, error) {
	f.closed = append(f.closed, id)
	switch id {
	case 10:
		return store.ClosedOut{}, store.ErrAlreadyClosed
	case 31:
		return store.ClosedOut{}, store.OpenContracts{Name: "still open", Lots: 2}
	}
	return store.ClosedOut{Name: "kalshi15m2 Model v2", ReapedCents: 4400}, nil
}
func (f *fakeControls) SetBucketOrders(_ context.Context, id int64, on bool) error {
	if id != 50 {
		return store.ErrNoSuchBucket
	}
	f.bucketOrders = append(f.bucketOrders, on)
	return nil
}
func (f *fakeControls) SetVersionArchived(_ context.Context, id int64, archived bool) error {
	switch id {
	case 19: // retired, no bucket
		f.archived = append(f.archived, archived)
		return nil
	case 7: // still held
		return store.ErrNotArchivable
	}
	return store.ErrVersionNotFound
}
func (f *fakeControls) OpenBank(context.Context) (store.Bank, error) {
	if f.bank.ID == 0 && f.bank.Name == "" {
		return store.Bank{ID: 1, Name: "House"}, nil
	}
	return f.bank, nil
}
func (f *fakeControls) ListPaydays(context.Context) ([]store.Payday, error) {
	if f.paydays == nil {
		return []store.Payday{}, nil
	}
	return f.paydays, nil
}
func (f *fakeControls) SchedulePayday(_ context.Context, amountCents int64, everyDays int, note string, now time.Time) (store.Payday, error) {
	if amountCents <= 0 {
		return store.Payday{}, store.PaydayRefused{Why: "The amount must be above zero."}
	}
	if everyDays < 1 || everyDays > 366 {
		return store.Payday{}, store.PaydayRefused{Why: "The rhythm is a whole number of days, from 1 to 366."}
	}
	if f.bank.Name == "closed" {
		return store.Payday{}, store.ErrNoOpenBank
	}
	p := store.Payday{ID: int64(len(f.scheduled) + 1), BankID: 1, AmountCents: amountCents, EveryDays: everyDays, NextAt: now.AddDate(0, 0, everyDays), Note: note}
	f.scheduled = append(f.scheduled, p)
	return p, nil
}
func (f *fakeControls) StopPayday(_ context.Context, id int64) error {
	if id != 4 {
		return store.ErrNoPayday
	}
	f.stopped = append(f.stopped, id)
	return nil
}
func (f *fakeControls) Transfer(_ context.Context, t store.ManualTransfer) (store.Transferred, error) {
	if t.To == 7 {
		return store.Transferred{}, store.TransferRefused{Why: "Money is not moved into a bucket by hand."}
	}
	f.steps = append(f.steps, "transfer")
	return store.Transferred{ID: 55, Reason: "adjustment", FromName: "kalshi15m3 Value v3 cash", ToName: "tax reserve (sim)", Cents: t.Cents, FromBucket: t.From == 7}, nil
}

// Reap, transfer and the versions' shapes go through the controls with the engine's answers
// mapped to the page's statuses.
func TestReapTransferAndShapes(t *testing.T) {
	f := &fakeControls{on: true, set: true, versions: []store.Version3{{ID: 19, Name: "Scalper (conventions)", Status: "retired", Params: []byte(`{"name":"Scalper (conventions)","exit":"ev"}`), Held: true, Lives: 1}}}
	reloads := 0
	mux := controlsMux(f, Control{
		Shape: func(p []byte) ([]byte, error) { return []byte(`{"name":"Scalper","exit":"ev"}`), nil },
		Reap: func(_ context.Context, id int64, restake bool) (Reaped, error) {
			switch id {
			case 19:
				if restake {
					return Reaped{Bucket: "kalshi15m3 Scalper v3", ReapedCents: 71540, Next: "kalshi15m3 Scalper v3 life 2", Held: 3, Ordering: 2}, nil
				}
				return Reaped{}, ReapRefused{"kalshi15m3 Scalper v3 has 1 open position(s); it is reaped once they settle"}
			case 21:
				return Reaped{}, store.ErrNoBucketEver
			}
			return Reaped{}, errors.New("boom")
		},
		Reload: func(context.Context) (int, int, error) { reloads++; return 3, 2, nil },
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/controls", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"shape":{"name":"Scalper","exit":"ev"}`) || !strings.Contains(rec.Body.String(), `"held":true`) || !strings.Contains(rec.Body.String(), `"lives":1`) {
		t.Fatalf("versions %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/version/reap", `{"id":19}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "open position") {
		t.Fatalf("reap with a bet open %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/version/reap", `{"id":19,"restake":true}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"next":"kalshi15m3 Scalper v3 life 2"`) || !strings.Contains(rec.Body.String(), `"reaped_cents":71540`) {
		t.Fatalf("reap and restake %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/version/reap", `{"id":21,"restake":true}`); rec.Code != 404 {
		t.Fatalf("never had a bucket %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/version/reap", `{"id":99}`); rec.Code != 500 {
		t.Fatalf("engine failure %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/controls/accounts", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"common pool (sim)"`) {
		t.Fatalf("accounts %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/transfer", `{"from":1,"to":7,"cents":100}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "not moved into a bucket") {
		t.Fatalf("into a bucket %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/transfer", `{"from":1,"to":3,"cents":100,"memo":"m"}`); rec.Code != 200 || reloads != 0 {
		t.Fatalf("pool to reserve %d %s reloads %d", rec.Code, rec.Body.String(), reloads)
	}
	if rec = postJSON(mux, "/api/controls/transfer", `{"from":7,"to":3,"cents":20000,"memo":"tax"}`); rec.Code != 200 || reloads != 1 || !strings.Contains(rec.Body.String(), `"reloaded":true`) {
		t.Fatalf("out of a bucket %d %s reloads %d", rec.Code, rec.Body.String(), reloads)
	}
	none := controlsMux(&fakeControls{}, Control{})
	if rec := postJSON(none, "/api/controls/version/reap", `{"id":19}`); rec.Code != 503 {
		t.Fatalf("no engine in the process: %d", rec.Code)
	}
}

// Deploy: the page names the version, the seed and the source; the route finds the version's
// family and hands the engine the deploy. A version that holds a bucket is refused before the
// engine is asked, replenishment short is the store's refusal, and a bad figure never leaves the route.
func TestDeployAChosenSeed(t *testing.T) {
	f := &fakeControls{versions: []store.Version3{
		{ID: 19, Name: "Scalper (conventions)", Status: "probation", Family: "kalshi15m", Held: true, Lives: 1},
		{ID: 21, Name: "Day Value (conventions)", Status: "draft", Family: "kalshiladder"},
	}}
	var asked []string
	dropped := 0
	mux := controlsMux(f, Control{
		Drop: func() { dropped++ },
		Deploy: func(_ context.Context, family string, d store.Deploy) (Deployed, error) {
			asked = append(asked, family)
			if d.Source == store.SeedFromReplenishment {
				return Deployed{}, store.DeployRefused{Why: "Replenishment holds $12.00; the deploy asks for $500.00. Pull from the bank instead, or deploy less."}
			}
			return Deployed{Bucket: "kalshiladder3 Day Value (conventions) v3", SeedCents: d.SeedCents, Source: d.Source, Held: 2, Ordering: 2}, nil
		},
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/controls", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"seed_sources":["replenishment","bank"]`) || !strings.Contains(rec.Body.String(), `"default_seed_cents":100000`) {
		t.Fatalf("the form's choices %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/bucket/deploy", `{"version_id":21,"cents":50000,"source":"bank"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"bucket":"kalshiladder3 Day Value (conventions) v3"`) || !strings.Contains(rec.Body.String(), `"seed_cents":50000`) || dropped != 1 {
		t.Fatalf("deploy %d %s dropped %d", rec.Code, rec.Body.String(), dropped)
	}
	if len(asked) != 1 || asked[0] != "kalshiladder" {
		t.Fatalf("the engine was asked for family %v; the version is a ladder", asked)
	}
	if rec = postJSON(mux, "/api/controls/bucket/deploy", `{"version_id":21,"cents":50000,"source":"replenishment"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "Pull from the bank instead") {
		t.Fatalf("replenishment short %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/bucket/deploy", `{"version_id":19,"cents":50000,"source":"bank"}`); rec.Code != 409 || len(asked) != 2 {
		t.Fatalf("held %d %s asked %v", rec.Code, rec.Body.String(), asked)
	}
	if rec = postJSON(mux, "/api/controls/bucket/deploy", `{"version_id":99,"cents":50000,"source":"bank"}`); rec.Code != 404 {
		t.Fatalf("missing %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/bucket/deploy", `{"version_id":21,"cents":0,"source":"bank"}`); rec.Code != 400 || len(asked) != 2 {
		t.Fatalf("no seed %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/bucket/deploy", `{"version_id":21,"cents":100,"source":"owners"}`); rec.Code != 400 || len(asked) != 2 {
		t.Fatalf("bad source %d %s", rec.Code, rec.Body.String())
	}
	none := controlsMux(&fakeControls{}, Control{})
	if rec := postJSON(none, "/api/controls/bucket/deploy", `{"version_id":21,"cents":100,"source":"bank"}`); rec.Code != 503 {
		t.Fatalf("no engine in the process: %d", rec.Code)
	}
}

// The two pages divide the work: the home page holds the bank (its accounts' menus move money and
// schedule a payday, the rule and the reset hang off the bank), and the buckets page deploys strategy
// accounts (a strategy, a seed, and where the seed is drawn from). The money forms no longer live on buckets.
func TestHomeHoldsTheBankAndBucketsDeploys(t *testing.T) {
	page := string(homePage)
	for _, want := range []string{`id="bankform"`, `id="paydays"`, `id="rule"`, `data-menu="`, `"form-payday"`, `"form-rule"`, `"form-reset"`, `"form-out"`, `"form-in"`,
		`api/controls/transfer`, `api/controls/payday`, `api/controls/policy`, `api/controls/reset`,
		`id="j-deploy"`, `id="dp-version"`, `id="dp-seed"`, `id="dp-source"`, `api/controls/bucket/deploy`, `seed_sources`, `default_seed_cents`, `b.source`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	for _, gone := range []string{`transferHTML`, `paydayHTML`, `controlsHTML`, `data-act="restake"`, `data-act="reap"`, `id="j-money"`, `id="j-rule"`, `mountBuckets`} {
		if strings.Contains(page, gone) {
			t.Errorf("the buckets page still carries %s", gone)
		}
	}
}

// Close-out is per bucket. An old engine is closed in the ledger and its record stays. A live-engine
// bucket is handed to that engine's reap, and is not restaked.
func TestCloseOutOneBucket(t *testing.T) {
	f := &fakeControls{}
	var reaped []int64
	var dropped int
	mux := controlsMux(f, Control{
		Drop: func() { dropped++ },
		Reap: func(_ context.Context, id int64, restake bool) (Reaped, error) {
			if restake {
				t.Fatal("close out must not restake")
			}
			reaped = append(reaped, id)
			return Reaped{Bucket: "kalshi15m3 Scalper v3", ReapedCents: 100}, nil
		},
	})
	rec := postJSON(mux, "/api/controls/bucket/close", `{"id":30}`)
	if rec.Code != 200 || len(reaped) != 0 || len(f.closed) != 1 || f.closed[0] != 30 || dropped != 1 || !strings.Contains(rec.Body.String(), `"kept":true`) || !strings.Contains(rec.Body.String(), `"reaped_cents":4400`) {
		t.Fatalf("old engine %d %s reaped %v closed %v", rec.Code, rec.Body.String(), reaped, f.closed)
	}
	if rec = postJSON(mux, "/api/controls/bucket/close", `{"id":50}`); rec.Code != 200 || len(reaped) != 1 || reaped[0] != 19 || len(f.closed) != 1 {
		t.Fatalf("live engine %d %s reaped %v closed %v", rec.Code, rec.Body.String(), reaped, f.closed)
	}
	if rec = postJSON(mux, "/api/controls/bucket/close", `{"id":10}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "already closed") {
		t.Fatalf("already closed %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/bucket/close", `{"id":31}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "still holds") {
		t.Fatalf("open contracts %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/bucket/close", `{"id":99}`); rec.Code != 404 {
		t.Fatalf("missing %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/bucket/close", `{}`); rec.Code != 400 {
		t.Fatalf("no id %d %s", rec.Code, rec.Body.String())
	}
}

// A × on a live-engine bucket with positions open is not refused any more: the engine marks it
// and the answer says closing, with how many are open. Flat, it is reaped at once as before.
func TestCloseOutWaitsForTheLastSettlement(t *testing.T) {
	f := &fakeControls{}
	var dropped int
	open := 2
	mux := controlsMux(f, Control{
		Drop: func() { dropped++ },
		CloseWhenFlat: func(_ context.Context, id int64) (Reaped, *Closing, error) {
			if id != 19 {
				return Reaped{}, nil, ReapRefused{"that version holds no bucket"}
			}
			if open > 0 {
				return Reaped{}, &Closing{Bucket: "kalshi15m3 Scalper v3", Open: open}, nil
			}
			return Reaped{Bucket: "kalshi15m3 Scalper v3", ReapedCents: 100}, nil, nil
		},
		Reap: func(context.Context, int64, bool) (Reaped, error) {
			t.Fatal("Reap must not be used when CloseWhenFlat is there")
			return Reaped{}, nil
		},
	})
	rec := postJSON(mux, "/api/controls/bucket/close", `{"id":50}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"closing":true`) || !strings.Contains(rec.Body.String(), `"open":2`) || len(f.closed) != 0 || dropped != 0 {
		t.Fatalf("with positions open: %d %s closed %v dropped %d", rec.Code, rec.Body.String(), f.closed, dropped)
	}
	open = 0
	if rec = postJSON(mux, "/api/controls/bucket/close", `{"id":50}`); rec.Code != 200 || strings.Contains(rec.Body.String(), `"closing"`) || !strings.Contains(rec.Body.String(), `"reaped_cents":100`) || dropped != 1 {
		t.Fatalf("flat: %d %s dropped %d", rec.Code, rec.Body.String(), dropped)
	}
	// An old engine's bucket never goes through the live engine.
	if rec = postJSON(mux, "/api/controls/bucket/close", `{"id":30}`); rec.Code != 200 || len(f.closed) != 1 || f.closed[0] != 30 {
		t.Fatalf("old engine: %d %s closed %v", rec.Code, rec.Body.String(), f.closed)
	}
}

// A bucket's own orders switch is saved and the engine reloaded; a version is archived only when
// the store allows it, and can be listed again.
func TestBucketOrdersAndArchive(t *testing.T) {
	f := &fakeControls{}
	reloads := 0
	mux := controlsMux(f, Control{Reload: func(context.Context) (int, int, error) { reloads++; return 2, 1, nil }})
	rec := postJSON(mux, "/api/controls/bucket/orders", `{"id":50,"on":false}`)
	if rec.Code != 200 || reloads != 1 || len(f.bucketOrders) != 1 || f.bucketOrders[0] || !strings.Contains(rec.Body.String(), `"on":false`) || !strings.Contains(rec.Body.String(), `"held":2`) {
		t.Fatalf("off: %d %s reloads %d saved %v", rec.Code, rec.Body.String(), reloads, f.bucketOrders)
	}
	if rec = postJSON(mux, "/api/controls/bucket/orders", `{"id":99,"on":true}`); rec.Code != 404 || reloads != 1 {
		t.Fatalf("unknown bucket: %d %s reloads %d", rec.Code, rec.Body.String(), reloads)
	}
	if rec = postJSON(mux, "/api/controls/bucket/orders", `{"on":true}`); rec.Code != 400 {
		t.Fatalf("no id: %d %s", rec.Code, rec.Body.String())
	}

	if rec = postJSON(mux, "/api/controls/version/archive", `{"id":19}`); rec.Code != 200 || len(f.archived) != 1 || !f.archived[0] || !strings.Contains(rec.Body.String(), `"archived":true`) {
		t.Fatalf("archive: %d %s %v", rec.Code, rec.Body.String(), f.archived)
	}
	if rec = postJSON(mux, "/api/controls/version/archive", `{"id":19,"archived":false}`); rec.Code != 200 || len(f.archived) != 2 || f.archived[1] {
		t.Fatalf("unarchive: %d %s %v", rec.Code, rec.Body.String(), f.archived)
	}
	if rec = postJSON(mux, "/api/controls/version/archive", `{"id":7}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "retired") {
		t.Fatalf("held: %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/version/archive", `{"id":99}`); rec.Code != 404 {
		t.Fatalf("unknown: %d %s", rec.Code, rec.Body.String())
	}
}

func controlsMux(f *fakeControls, ctl Control) *http.ServeMux {
	mux := http.NewServeMux()
	controlRoutes(mux, f, &bucketList{}, ctl)
	return mux
}

func postJSON(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	mux.ServeHTTP(rec, req)
	return rec
}

func TestResetRequiresTheWords(t *testing.T) {
	f := &fakeControls{}
	var steps []string
	mux := controlsMux(f, Control{
		Hold:    func() { steps = append(steps, "hold") },
		Release: func() { steps = append(steps, "release") },
		Abort:   func() { steps = append(steps, "abort") },
	})
	rec := postJSON(mux, "/api/controls/reset", `{"confirm":"please"}`)
	if rec.Code != 400 || len(f.steps) != 0 || len(steps) != 0 {
		t.Fatalf("code %d db %v engine %v body %s", rec.Code, f.steps, steps, rec.Body.String())
	}
}

func TestResetHoldsThenReleasesThenReloads(t *testing.T) {
	f := &fakeControls{reset: store.ResetCounts{Buckets: 24, Orders: 12, Transfers: 40}}
	var steps []string
	mux := controlsMux(f, Control{
		Hold:    func() { steps = append(steps, "hold") },
		Release: func() { steps = append(steps, "release") },
		Abort:   func() { steps = append(steps, "abort") },
		Reload:  func(context.Context) (int, int, error) { steps = append(steps, "reload"); return 2, 2, nil },
	})
	rec := postJSON(mux, "/api/controls/reset", `{"confirm":" reset sim "}`)
	if rec.Code != 200 || strings.Join(steps, ",") != "hold,release,reload" || strings.Join(f.steps, ",") != "reset" {
		t.Fatalf("code %d engine %v db %v body %s", rec.Code, steps, f.steps, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"buckets":24`) || !strings.Contains(body, `"reloaded":true`) || !strings.Contains(body, `"held":2`) {
		t.Fatalf("body %s", body)
	}
}

// The builder: the engine (a fake here) builds and labels; the store registers as draft; a shape
// the engine refuses, a missing hypothesis, and a taken name are refused with their reasons.
// The exercise record: one append-only analysis_result row per run, key strategy.exercise, the
// shape and window as params and the summary as result; behind the operator key like every
// other POST here; a record without a shape, a window or a summary is refused and writes nothing.
func TestExerciseRecord(t *testing.T) {
	f := &fakeControls{}
	mux := controlsMux(f, Control{Version: "rel1", Key: "open-sesame"})
	body := `{"shape":{"name":"Late","exit":"hold","lambda":0.5,"tau_max":150},"family":"kalshi15m","from":"2026-09-22T04:30:00Z","to":"2026-09-23T04:30:00Z",` +
		`"step_s":5,"seed_cents":100000,"release":"abc1234","summary":{"bets":12,"pnl_cents":-431}}`
	req := httptest.NewRequest(http.MethodPost, ExerciseRecordPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 || len(f.recorded) != 0 {
		t.Fatalf("without the key: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, ExerciseRecordPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(operatorHeader, "open-sesame")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || len(f.recorded) != 1 || !strings.Contains(rec.Body.String(), `"id":1`) || !strings.Contains(rec.Body.String(), `"recorded":true`) {
		t.Fatalf("record %d %s recorded %+v", rec.Code, rec.Body.String(), f.recorded)
	}
	got := f.recorded[0]
	params, _ := got.params.(map[string]any)
	if got.key != ExerciseRecordKey || got.source != ExerciseRecordSource || got.sha != "rel1" || params["family"] != "kalshi15m" || params["release"] != "abc1234" ||
		!got.from.Equal(time.Date(2026, 9, 22, 4, 30, 0, 0, time.UTC)) || !got.to.Equal(time.Date(2026, 9, 23, 4, 30, 0, 0, time.UTC)) {
		t.Fatalf("recorded %+v", got)
	}
	if shape, _ := params["shape"].(json.RawMessage); !strings.Contains(string(shape), `"tau_max":150`) {
		t.Fatalf("the shape is the record: %s", shape)
	}
	if summary, _ := got.result.(json.RawMessage); !strings.Contains(string(summary), `"pnl_cents":-431`) {
		t.Fatalf("the summary is the result: %s", summary)
	}
	for _, bad := range []string{
		`{"family":"kalshi15m","from":"2026-09-22T04:30:00Z","to":"2026-09-23T04:30:00Z","summary":{}}`, // no shape
		`{"shape":{"name":"x"},"from":"2026-09-23T04:30:00Z","to":"2026-09-22T04:30:00Z","summary":{}}`, // the window ends first
		`{"shape":{"name":"x"},"from":"2026-09-22T04:30:00Z","to":"2026-09-23T04:30:00Z"}`,              // no summary
		`{"shape":{"name":"x"},"from":"2026-09-22T04:30:00Z","to":"2026-09-23T04:30:00Z","summary":{},"release":"a\nb"}`,
		`not json`,
	} {
		req = httptest.NewRequest(http.MethodPost, ExerciseRecordPath, strings.NewReader(bad))
		req.Header.Set(operatorHeader, "open-sesame")
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 400 || len(f.recorded) != 1 {
			t.Fatalf("must be refused: %s -> %d %s", bad, rec.Code, rec.Body.String())
		}
	}
}

func TestBuilderRegistersADraft(t *testing.T) {
	f := &fakeControls{}
	built := 0
	mux := controlsMux(f, Control{
		Version: "test",
		Presets: func() []byte { return []byte(`[{"key":"value","title":"Value"}]`) },
		Build: func(shape []byte) (Built, error) {
			built++
			var s struct {
				Name   string  `json:"name"`
				Lambda float64 `json:"lambda"`
			}
			_ = json.Unmarshal(shape, &s)
			if s.Lambda <= 0 {
				return Built{}, BuildRefused{"lambda must be above 0"}
			}
			return Built{Name: s.Name + " (conventions)", Blurb: "b", Params: []byte(`{"name":"x"}`), Parent: "Value", Family: "kalshi15m"}, nil
		},
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/controls/presets", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"key":"value"`) {
		t.Fatalf("presets %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(mux, "/api/controls/version/new", `{"name":"Late","exit":"hold","lambda":0.5,"hypothesis":"the market lags spot late"}`)
	if rec.Code != 200 || len(f.created) != 1 || f.created[0].Name != "Late (conventions)" || f.created[0].Hypothesis != "the market lags spot late" || f.created[0].Parent != "Value" {
		t.Fatalf("register %d %s created %+v", rec.Code, rec.Body.String(), f.created)
	}
	if !strings.Contains(rec.Body.String(), `"status":"draft"`) || f.created[0].CodeRef != "built on the buckets page; release test" || f.created[0].Family != "kalshi15m" {
		t.Fatalf("body %s created %+v", rec.Body.String(), f.created[0])
	}
	rec = postJSON(mux, "/api/controls/version/new", `{"name":"Zero","exit":"hold","lambda":0,"hypothesis":"x"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "lambda must be above 0") || len(f.created) != 1 {
		t.Fatalf("refused shape %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(mux, "/api/controls/version/new", `{"name":"Quiet","exit":"hold","lambda":0.5}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "hypothesis") || len(f.created) != 1 {
		t.Fatalf("no hypothesis %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(mux, "/api/controls/version/new", `{"name":"Taken","exit":"hold","lambda":0.5,"hypothesis":"x"}`)
	if rec.Code != 409 || len(f.created) != 1 {
		t.Fatalf("taken name %d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(mux, "/api/controls/version/new", `not json`); rec.Code != 400 {
		t.Fatalf("bad json %d", rec.Code)
	}
	if built != 3 {
		t.Fatalf("the engine was asked %d times", built)
	}
	// A registration from the MCP proposal tool says so in code_ref; a via that is not one
	// short word is refused before the engine is asked.
	rec = postJSON(mux, "/api/controls/version/new", `{"name":"Tail","exit":"hold","lambda":0.5,"hypothesis":"h","via":"mcp"}`)
	if rec.Code != 200 || len(f.created) != 2 || f.created[1].CodeRef != "proposed via mcp; release test" {
		t.Fatalf("via %d %s created %+v", rec.Code, rec.Body.String(), f.created)
	}
	rec = postJSON(mux, "/api/controls/version/new", `{"name":"Tail2","exit":"hold","lambda":0.5,"hypothesis":"h","via":"a\nb"}`)
	if rec.Code != 400 || len(f.created) != 2 || built != 4 {
		t.Fatalf("a via with a newline must be refused: %d %s", rec.Code, rec.Body.String())
	}
	none := controlsMux(&fakeControls{}, Control{})
	if rec := postJSON(none, "/api/controls/version/new", `{"name":"x","lambda":0.5,"hypothesis":"x"}`); rec.Code != 503 {
		t.Fatalf("no builder in the process: %d", rec.Code)
	}
}

// With an operator key set, every change needs it in the header; reads never do. Without a key
// set, nothing is asked (localhost, a tailnet).
func TestOperatorKeyGatesChanges(t *testing.T) {
	f := &fakeControls{}
	mux := controlsMux(f, Control{Key: "open sesame", Apply: func(bool) string { return "now" }})
	get := httptest.NewRecorder()
	mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/controls", nil))
	if get.Code != 200 || !strings.Contains(get.Body.String(), `"locked":true`) {
		t.Fatalf("a read needs no key and says the page is locked: %d %s", get.Code, get.Body.String())
	}
	rec := postJSON(mux, "/api/controls/orders", `{"on":false}`)
	if rec.Code != 401 || f.ordersSaved != nil {
		t.Fatalf("no key: %d saved %v", rec.Code, f.ordersSaved)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/controls/orders", strings.NewReader(`{"on":false}`))
	req.Header.Set(operatorHeader, "wrong")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 || f.ordersSaved != nil {
		t.Fatalf("wrong key: %d saved %v", rec.Code, f.ordersSaved)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/controls/orders", strings.NewReader(`{"on":false}`))
	req.Header.Set(operatorHeader, " open sesame ")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || f.ordersSaved == nil {
		t.Fatalf("the key: %d %s", rec.Code, rec.Body.String())
	}
	open := controlsMux(&fakeControls{}, Control{Apply: func(bool) string { return "now" }})
	if rec := postJSON(open, "/api/controls/orders", `{"on":true}`); rec.Code != 200 {
		t.Fatalf("no key set means no gate: %d", rec.Code)
	}
}

// With no engine in the process the reset still empties the books; the next start seeds.
func TestResetWithoutAnEngine(t *testing.T) {
	f := &fakeControls{reset: store.ResetCounts{Buckets: 1}}
	rec := postJSON(controlsMux(f, Control{}), "/api/controls/reset", `{"confirm":"reset sim"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"effective":"next-start"`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

// A reload that fails after the wipe is not a failed reset: the books are empty, the engine
// heals on its own, and the page is told.
func TestResetReportsAReloadThatDidNotFinish(t *testing.T) {
	f := &fakeControls{reset: store.ResetCounts{Buckets: 3}}
	mux := controlsMux(f, Control{
		Reload: func(context.Context) (int, int, error) { return 0, 0, errors.New("the ledger could not be read") },
	})
	rec := postJSON(mux, "/api/controls/reset", `{"confirm":"reset sim"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"effective":"heal"`) || !strings.Contains(rec.Body.String(), `"buckets":3`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestResetFailureLetsTheEngineRebuild(t *testing.T) {
	f := &fakeControls{resetErr: errors.New("boom")}
	var steps []string
	mux := controlsMux(f, Control{
		Hold:    func() { steps = append(steps, "hold") },
		Release: func() { steps = append(steps, "release") },
		Abort:   func() { steps = append(steps, "abort") },
	})
	rec := postJSON(mux, "/api/controls/reset", `{"confirm":"reset sim"}`)
	if rec.Code != 500 || strings.Join(steps, ",") != "hold,abort" {
		t.Fatalf("code %d engine %v body %s", rec.Code, steps, rec.Body.String())
	}
}

func TestOrdersVersionAndPolicy(t *testing.T) {
	f := &fakeControls{}
	applied := ""
	reloads := 0
	mux := controlsMux(f, Control{
		EnvOn:  true,
		Apply:  func(on bool) string { applied = map[bool]string{true: "on", false: "off"}[on]; return "now" },
		Status: func() (bool, string) { return false, "now" },
		Reload: func(context.Context) (int, int, error) { reloads++; return 1, 1, nil },
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/controls", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"source":"environment"`) || !strings.Contains(rec.Body.String(), `"versions":[]`) {
		t.Fatalf("get %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(mux, "/api/controls/orders", `{"on":false}`)
	if rec.Code != 200 || f.ordersSaved == nil || *f.ordersSaved || applied != "off" {
		t.Fatalf("orders %d saved %v applied %s", rec.Code, f.ordersSaved, applied)
	}
	rec = postJSON(mux, "/api/controls/version", `{"id":7,"status":"probation"}`)
	if rec.Code != 200 || f.statusSaved != "probation" || reloads != 1 || !strings.Contains(rec.Body.String(), `"ordering":1`) {
		t.Fatalf("approve %d reloads %d %s", rec.Code, reloads, rec.Body.String())
	}
	rec = postJSON(mux, "/api/controls/version", `{"id":7,"status":"draft"}`)
	if rec.Code != 400 || reloads != 1 {
		t.Fatalf("draft %d reloads %d", rec.Code, reloads)
	}
	rec = postJSON(mux, "/api/controls/version", `{"id":9,"status":"retired"}`)
	if rec.Code != 404 || reloads != 1 {
		t.Fatalf("missing %d reloads %d %s", rec.Code, reloads, rec.Body.String())
	}
	rec = postJSON(mux, "/api/controls/policy", `{"winnings_bps":10001,"replenish_bps":0,"tax_bps":0,"fees_bps":0}`)
	if rec.Code != 400 || f.policySaved {
		t.Fatalf("policy %d saved %v", rec.Code, f.policySaved)
	}
	rec = postJSON(mux, "/api/controls/policy", `{"winnings_bps":2000,"replenish_bps":1000,"tax_bps":0,"fees_bps":0,"note":"first"}`)
	if rec.Code != 200 || !f.policySaved {
		t.Fatalf("policy save %d %s", rec.Code, rec.Body.String())
	}
}

func TestPaydaySchedule(t *testing.T) {
	next := time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)
	f := &fakeControls{paydays: []store.Payday{{ID: 4, BankID: 1, AmountCents: 50000, EveryDays: 14, NextAt: next, Note: "wages"}}}
	mux := controlsMux(f, Control{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/controls", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"House"`) || !strings.Contains(rec.Body.String(), `"amount_cents":50000`) {
		t.Fatalf("bank %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/payday", `{"cents":25000,"every_days":14,"note":"payday"}`); rec.Code != 200 || len(f.scheduled) != 1 || f.scheduled[0].AmountCents != 25000 || !strings.Contains(rec.Body.String(), `"every_days":14`) {
		t.Fatalf("schedule %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/payday", `{"cents":0,"every_days":14}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "above zero") {
		t.Fatalf("zero %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/payday", `{"cents":100,"every_days":0}`); rec.Code != 400 {
		t.Fatalf("rhythm %d %s", rec.Code, rec.Body.String())
	}
	closed := &fakeControls{bank: store.Bank{Name: "closed"}}
	if rec = postJSON(controlsMux(closed, Control{}), "/api/controls/payday", `{"cents":100,"every_days":7}`); rec.Code != 409 {
		t.Fatalf("no bank %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/payday/stop", `{"id":4}`); rec.Code != 200 || len(f.stopped) != 1 || f.stopped[0] != 4 {
		t.Fatalf("stop %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/payday/stop", `{"id":9}`); rec.Code != 404 {
		t.Fatalf("missing payday %d %s", rec.Code, rec.Body.String())
	}
	if rec = postJSON(mux, "/api/controls/payday/stop", `{}`); rec.Code != 400 {
		t.Fatalf("no id %d %s", rec.Code, rec.Body.String())
	}
}
