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

func TestResetHoldsThenReleases(t *testing.T) {
	f := &fakeControls{reset: store.ResetCounts{Buckets: 24, Orders: 12, Transfers: 40}}
	var steps []string
	mux := controlsMux(f, Control{
		Hold:    func() { steps = append(steps, "hold") },
		Release: func() { steps = append(steps, "release") },
		Abort:   func() { steps = append(steps, "abort") },
	})
	rec := postJSON(mux, "/api/controls/reset", `{"confirm":" reset sim "}`)
	if rec.Code != 200 || strings.Join(steps, ",") != "hold,release" || strings.Join(f.steps, ",") != "reset" {
		t.Fatalf("code %d engine %v db %v body %s", rec.Code, steps, f.steps, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"buckets":24`) {
		t.Fatalf("body %s", rec.Body.String())
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
	mux := controlsMux(f, Control{
		EnvOn:  true,
		Apply:  func(on bool) string { applied = map[bool]string{true: "on", false: "off"}[on]; return "now" },
		Status: func() (bool, string) { return false, "now" },
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
	if rec.Code != 200 || f.statusSaved != "probation" {
		t.Fatalf("approve %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(mux, "/api/controls/version", `{"id":7,"status":"draft"}`)
	if rec.Code != 400 {
		t.Fatalf("draft %d", rec.Code)
	}
	rec = postJSON(mux, "/api/controls/version", `{"id":9,"status":"retired"}`)
	if rec.Code != 404 {
		t.Fatalf("missing %d %s", rec.Code, rec.Body.String())
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
