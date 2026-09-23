package web

import (
	"context"
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
