package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/web"
)

// Every preset the page offers builds through the same function the page posts to, and a shape
// the engine refuses comes back as a refusal the page can show.
func TestBuilderBuildsEveryPreset(t *testing.T) {
	var presets []struct {
		Key   string          `json:"key"`
		Shape json.RawMessage `json:"shape"`
	}
	if err := json.Unmarshal(presetsJSON(), &presets); err != nil {
		t.Fatal(err)
	}
	if len(presets) < 6 {
		t.Fatalf("%d presets", len(presets))
	}
	for _, pr := range presets {
		b, err := buildVersion(pr.Shape)
		if err != nil {
			t.Errorf("%s: %v", pr.Key, err)
			continue
		}
		if !strings.HasSuffix(b.Name, " (conventions)") || len(b.Params) == 0 || (b.Parent != "Value" && b.Parent != "Scalper") {
			t.Errorf("%s: %+v", pr.Key, b)
		}
		if pr.Key == "martingale-control" && !b.Control {
			t.Error("the martingale preset is a control")
		}
	}
	_, err := buildVersion([]byte(`{"name":"x","exit":"hold","lambda":0}`))
	var refused web.BuildRefused
	if err == nil || !strings.Contains(err.Error(), "lambda") {
		t.Fatalf("lambda 0: %v", err)
	}
	if _, ok := err.(web.BuildRefused); !ok {
		t.Fatalf("not a refusal: %T %v", err, refused)
	}
}
