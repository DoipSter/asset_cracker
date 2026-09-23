package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/store"
)

type fakeControls struct {
	on          bool
	set         bool
	versions    []store.Version3
	policy      store.SkimPolicy
	reset       store.ResetCounts
	resetErr    error
	ordersSaved *bool
	statusSaved string
	policySaved bool
	steps       []string
	created     []store.NewVersion3
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
			return Built{Name: s.Name + " (conventions)", Blurb: "b", Params: []byte(`{"name":"x"}`), Parent: "Value"}, nil
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
	if !strings.Contains(rec.Body.String(), `"status":"draft"`) || f.created[0].CodeRef != "built on the buckets page; release test" {
		t.Fatalf("body %s coderef %s", rec.Body.String(), f.created[0].CodeRef)
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
