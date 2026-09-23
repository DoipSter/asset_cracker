package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/catalogue"
	"github.com/doipster/asset_cracker/service/internal/store"
)

type catalogueFake struct {
	q, source, frequency string
	limit                int
	change               store.AssetChange
	atLimit              error // what the rules passed in say of a new item with ten selected
	result               store.AssetResult
	err                  error
}

func (f *catalogueFake) SearchCatalogue(_ context.Context, q, source, frequency string, limit int) ([]store.CatalogueHit, error) {
	f.q, f.source, f.frequency, f.limit = q, source, frequency, limit
	return []store.CatalogueHit{{Source: "kalshi", Code: "KXBNB15M", Title: `<img src=x onerror=alert(1)>`, Recordable: true}}, nil
}

func (f *catalogueFake) CatalogueSummary(context.Context) (store.CatalogueSummary, error) {
	return store.CatalogueSummary{Items: 677, Recordable: 420}, nil
}

func (f *catalogueFake) RecordedInstruments(context.Context) ([]store.Recorded, error) {
	return []store.Recorded{
		{ID: 3, Source: "kalshi", Symbol: "KXBTC15M", Kind: "binary_contract"},
		{ID: 40, Source: "kalshi", Symbol: "KXBNB15M", Kind: "binary_contract", Selected: true},
		{ID: 42, Source: "coinbase", Symbol: "AVAX-USD", Kind: "spot", Selected: true},
	}, nil
}

func (f *catalogueFake) SetAssetSelection(_ context.Context, c store.AssetChange, decide func(store.AssetState) (store.AssetPlan, error)) (store.AssetResult, error) {
	f.change = c
	_, f.atLimit = decide(store.AssetState{Item: store.CatalogueRow{Code: c.Code, Recorder: "round"}, Record: true, Selected: 10})
	return f.result, f.err
}

func catalogueServer(f *catalogueFake, changed *int) *http.ServeMux {
	mux := http.NewServeMux()
	since := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	CatalogueRoutes(mux, f, Catalogue{Max: 10,
		Running: func() map[int64]RecorderState { return map[int64]RecorderState{40: {Recorder: "round", Since: since}} },
		Changed: func() { *changed++ }})
	return mux
}

func TestCatalogueSearch(t *testing.T) {
	f, changed := &catalogueFake{}, 0
	mux := catalogueServer(f, &changed)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/catalogue?q=+bnb+&source=kalshi&frequency=fifteen_min", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if f.q != "bnb" || f.source != "kalshi" || f.frequency != "fifteen_min" || f.limit != catalogue.SearchLimit {
		t.Errorf("searched %+v", f)
	}
	var doc struct {
		Results   []store.CatalogueHit `json:"results"`
		Selected  int                  `json:"selected"`
		Max       int                  `json:"max"`
		Recording []struct {
			Symbol  string         `json:"symbol"`
			Running *RecorderState `json:"running"`
		} `json:"recording"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Results) != 1 || doc.Selected != 2 || doc.Max != 10 || len(doc.Recording) != 3 {
		t.Errorf("doc %+v", doc)
	}
	if doc.Recording[0].Running != nil || doc.Recording[1].Running == nil || doc.Recording[1].Running.Recorder != "round" || doc.Recording[2].Running != nil {
		t.Errorf("running states %+v %+v %+v: only the selected one being recorded has one", doc.Recording[0].Running, doc.Recording[1].Running, doc.Recording[2].Running)
	}

	for _, bad := range []string{"source=binance", "frequency=every_second", "q=" + strings.Repeat("x", 65)} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/catalogue?"+bad, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, rec.Code)
		}
	}
}

func TestCatalogueSwitch(t *testing.T) {
	post := func(f *catalogueFake, changed *int, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		catalogueServer(f, changed).ServeHTTP(rec, httptest.NewRequest("POST", "/api/controls/asset", strings.NewReader(body)))
		return rec
	}

	f, changed := &catalogueFake{result: store.AssetResult{InstrumentID: 40, Record: true, Changed: true, Selected: 3}}, 0
	rec := post(f, &changed, `{"source":"kalshi","code":" KXBNB15M ","record":true}`)
	if rec.Code != 200 || changed != 1 || !strings.Contains(rec.Body.String(), `"effective":"now"`) {
		t.Errorf("switch on: %d %s, supervisor told %d times", rec.Code, rec.Body, changed)
	}
	if f.change.Code != "KXBNB15M" || !f.change.Record || f.change.Via == "" {
		t.Errorf("change %+v", f.change)
	}
	if f.atLimit == nil || !strings.Contains(f.atLimit.Error(), "AC_MAX_SELECTED=10") {
		t.Errorf("the rules handed to the store do not hold the limit of 10: %v", f.atLimit)
	}

	f, changed = &catalogueFake{result: store.AssetResult{InstrumentID: 40}}, 0
	if rec := post(f, &changed, `{"source":"kalshi","code":"KXBNB15M","record":false}`); rec.Code != 200 || changed != 0 ||
		!strings.Contains(rec.Body.String(), `"effective":"unchanged"`) {
		t.Errorf("a switch that changed nothing: %d %s, supervisor told %d times", rec.Code, rec.Body, changed)
	}

	for body, code := range map[string]int{
		`{"source":"kalshi","code":"KXBNB15M"}`:           400, // record missing, not false
		`{"source":"binance","code":"BNB","record":true}`: 400,
		`{"source":"coinbase","code":"","record":true}`:   400,
		`not json`: 400,
		`{"source":"kalshi","code":"KXBNB15M","record":"yes"}`: 400,
	} {
		if rec := post(&catalogueFake{}, &changed, body); rec.Code != code {
			t.Errorf("%s: status %d, want %d", body, rec.Code, code)
		}
	}

	f = &catalogueFake{err: catalogue.Refused{Why: "10 instruments are selected already"}}
	if rec := post(f, &changed, `{"source":"kalshi","code":"KXBNB15M","record":true}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "10 instruments are selected already") {
		t.Errorf("over the limit: %d %s", rec.Code, rec.Body)
	}
	f = &catalogueFake{err: store.ErrNotCatalogued}
	if rec := post(f, &changed, `{"source":"kalshi","code":"KXNOPE","record":true}`); rec.Code != http.StatusNotFound {
		t.Errorf("not catalogued: %d", rec.Code)
	}
}

// The assets page became a dialog on the home page: its old address sends the reader there, and
// the home page carries the dialog, its search and its switch, all through relative URLs. The way
// in is the rail's own header, not a tab of its own; the way out is a click beside it or Escape,
// not a button.
func TestAssetsPageIsTheHomeDialog(t *testing.T) {
	changed := 0
	rec := httptest.NewRecorder()
	catalogueServer(&catalogueFake{}, &changed).ServeHTTP(rec, httptest.NewRequest("GET", "/assets", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/#assets" {
		t.Fatalf("status %d, location %q", rec.Code, rec.Header().Get("Location"))
	}
	page := string(homePage)
	for _, want := range []string{`id="dlg-assets"`, `data-act="assets-open"`, `h==="assets"`, `api/catalogue`, `api/controls/asset`, `role="switch"`, `"asset-off":"asset-on"`, `act==="asset-on"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the home page lacks %s", want)
		}
	}
	for _, gone := range []string{`data-pane="assets"`, `assets-close`} {
		if strings.Contains(page, gone) {
			t.Errorf("the home page still carries %s", gone)
		}
	}
	if strings.Contains(page, `fetch("/`) || strings.Contains(page, `href="/`) {
		t.Error("the home page must ask by relative URL: it is read through an SSH tunnel at any path")
	}
}

// A seeded row's switch says when it takes effect: a spot product soon (the feed and the rail
// follow the table), a series at the next start.
func TestSeededSwitchSaysWhen(t *testing.T) {
	post := func(f *catalogueFake, body string) *httptest.ResponseRecorder {
		changed := 0
		rec := httptest.NewRecorder()
		catalogueServer(f, &changed).ServeHTTP(rec, httptest.NewRequest("POST", "/api/controls/asset", strings.NewReader(body)))
		if changed != 0 {
			t.Error("a seeded row is not the supervisor's")
		}
		return rec
	}
	f := &catalogueFake{result: store.AssetResult{InstrumentID: 4, Changed: true, Seeded: true, Kind: "spot"}}
	if rec := post(f, `{"source":"coinbase","code":"DOGE-USD","record":false}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"effective":"soon"`) {
		t.Errorf("seeded spot: %d %s", rec.Code, rec.Body)
	}
	f = &catalogueFake{result: store.AssetResult{InstrumentID: 5, Changed: true, Seeded: true, Kind: "binary_ladder"}}
	if rec := post(f, `{"source":"kalshi","code":"KXBTCD","record":false}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"effective":"next-start"`) {
		t.Errorf("seeded ladder: %d %s", rec.Code, rec.Body)
	}
	f = &catalogueFake{err: catalogue.Refused{Why: "BTC-USD cannot be switched off: the live engine prices KXBTC15M from this product"}}
	if rec := post(f, `{"source":"coinbase","code":"BTC-USD","record":false}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "prices KXBTC15M") {
		t.Errorf("depended: %d %s", rec.Code, rec.Body)
	}
}
