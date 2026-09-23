package proposals

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRegister builds the server with every tool: the SDK infers each schema from the Go types
// at registration and panics on one it cannot describe, so that mistake shows here. Then a
// client speaks to it over the protocol, so the answers are checked against the output schemas
// the SDK derived (a Params with its provenance map among them).
func TestRegister(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	(&Door{URL: "http://127.0.0.1:1", Client: http.DefaultClient, Getenv: func(string) string { return "" }, HomeDir: os.UserHomeDir}).register(s)

	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil)
	sess, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"strategy_presets", "strategy_build", "strategy_register", "strategy_versions"} {
		if !names[want] {
			t.Fatalf("tool %s is not listed: %v", want, names)
		}
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "strategy_presets"})
	if err != nil || res.IsError {
		t.Fatalf("presets: %v %+v", err, res)
	}
	var pl PresetList
	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &pl); err != nil || len(pl.Presets) != 11 {
		t.Fatalf("presets answer: %v %d", err, len(pl.Presets))
	}
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "strategy_build", Arguments: map[string]any{"name": "Tail", "exit": "hold", "lambda": 0.5, "side": "longshot", "min_vol_ratio": 1.5}})
	if err != nil || res.IsError {
		t.Fatalf("build: %v %s", err, mustText(res))
	}
	var b Built
	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &b); err != nil || b.Name != "Tail (conventions)" || b.Params.Side != "longshot" {
		t.Fatalf("build answer: %v %+v", err, b)
	}
	// A refusal is a tool error with the engine's reason, not a protocol error.
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "strategy_build", Arguments: map[string]any{"name": "Zero", "exit": "hold"}})
	if err != nil || !res.IsError || !strings.Contains(mustText(res), "lambda") {
		t.Fatalf("refusal: %v %+v", err, res)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func tail(t *testing.T) engine.Shape {
	for _, p := range engine.Presets() {
		if p.Key == "tail" {
			return p.Shape
		}
	}
	t.Fatal("no tail preset")
	return engine.Shape{}
}

// The dry run labels what the shape set as conventions, inherits the rest, names the parent,
// and writes nothing (it has no service to write to).
func TestBuildIsADryRun(t *testing.T) {
	d := &Door{URL: "http://127.0.0.1:1", Client: http.DefaultClient, Getenv: func(string) string { return "" }, HomeDir: os.UserHomeDir}
	_, b, err := d.build(context.Background(), nil, tail(t))
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "Tail (conventions)" || b.Parent != "Value" || b.Control {
		t.Fatalf("built %+v", b)
	}
	// tau_min is set by the Tail shape, a convention; tau_max is left at zero, the parent's.
	if vol, tmax := b.Params.Provenance["min_vol_ratio"], b.Params.Provenance["tau_max"]; vol.Kind != engine.KindConvention || tmax.Kind != engine.KindInherited {
		t.Fatalf("provenance: vol %v tau_max %v", vol, tmax)
	}
	if _, _, err := d.build(context.Background(), nil, engine.Shape{Name: "Zero", Exit: "hold"}); err == nil || !strings.Contains(err.Error(), "lambda") {
		t.Fatalf("lambda 0 must be refused with the reason: %v", err)
	}
}

// Registration goes through the running service: the shape and via on the wire, the key in the
// header, the service's answer back.
func TestRegisterPostsToTheService(t *testing.T) {
	var got struct {
		Body   map[string]any
		Key    string
		Path   string
		Method string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Method, got.Path, got.Key = r.Method, r.URL.Path, r.Header.Get("X-Operator-Key")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"name":"Tail (conventions)","status":"draft","control":false}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k"), []byte("open-sesame\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{EnvKeyFile: filepath.Join(dir, "k")}
	d := &Door{URL: srv.URL, Client: srv.Client(), Getenv: func(k string) string { return env[k] }, HomeDir: os.UserHomeDir}

	s := tail(t)
	s.Hypothesis = "longshots pay when the market moves"
	_, out, err := d.registerVersion(context.Background(), nil, s)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != 42 || out.Status != "draft" || !strings.Contains(out.Note, "Approve") {
		t.Fatalf("answer %+v", out)
	}
	if got.Method != http.MethodPost || got.Path != "/api/controls/version/new" || got.Key != "open-sesame" {
		t.Fatalf("request %+v", got)
	}
	if got.Body["via"] != "mcp" || got.Body["name"] != "Tail" || got.Body["side"] != "longshot" || got.Body["hypothesis"] != s.Hypothesis {
		t.Fatalf("body %v", got.Body)
	}

	// Without a hypothesis nothing leaves the process.
	got.Path = ""
	s.Hypothesis = ""
	if _, _, err := d.registerVersion(context.Background(), nil, s); err == nil || got.Path != "" {
		t.Fatalf("a shape without a hypothesis must be refused here: %v %q", err, got.Path)
	}
	// A shape the engine refuses is refused here too, with the engine's reason, before any call.
	s.Hypothesis = "x"
	s.Lambda = 0
	if _, _, err := d.registerVersion(context.Background(), nil, s); err == nil || got.Path != "" || !strings.Contains(err.Error(), "lambda") {
		t.Fatalf("an unbuildable shape must be refused before the call: %v %q", err, got.Path)
	}
}

// The service's refusals come back as the tool's error, and a 401 says where the key goes.
func TestServiceRefusals(t *testing.T) {
	code := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":"The operator key is missing or wrong."}`))
	}))
	defer srv.Close()
	d := &Door{URL: srv.URL, Client: srv.Client(), Getenv: func(string) string { return "" }, HomeDir: func() (string, error) { return "/home/acdeploy", nil }}
	s := tail(t)
	s.Hypothesis = "h"
	_, _, err := d.registerVersion(context.Background(), nil, s)
	if err == nil || !strings.Contains(err.Error(), "/home/acdeploy/.config/assetcracker/operator_key") || !strings.Contains(err.Error(), "missing or wrong") {
		t.Fatalf("401: %v", err)
	}
	code = http.StatusConflict
	_, _, err = d.registerVersion(context.Background(), nil, s)
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("409: %v", err)
	}
	// The env key wins over the file, and a GET carries none.
	env := map[string]string{EnvKey: " from-env "}
	d.Getenv = func(k string) string { return env[k] }
	if d.key() != "from-env" {
		t.Fatalf("key %q", d.key())
	}
}

// strategy_versions reads the page's controls document from the service; the base URL comes
// from the environment when the door has none.
func TestVersionsReadsTheService(t *testing.T) {
	var sawKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawKey = r.Header.Get("X-Operator-Key") != ""
		if r.URL.Path != "/api/controls" || r.Method != http.MethodGet {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`{"simulated":true,"locked":true,"orders":{"on":true,"source":"database","placing":true,"effective":"now"},
			"versions":[{"id":19,"name":"Value (conventions)","version":3,"status":"probation","hypothesis":"h"}],
			"policy":{"id":1,"since_at":"2026-09-22T00:00:00Z","note":"","winnings_bps":0,"replenish_bps":0,"tax_bps":0,"fees_bps":0}}`))
	}))
	defer srv.Close()
	env := map[string]string{EnvServiceURL: srv.URL + "/", EnvKey: "k"}
	d := &Door{Client: srv.Client(), Getenv: func(k string) string { return env[k] }, HomeDir: os.UserHomeDir}
	_, out, err := d.versions(context.Background(), nil, noInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Locked || len(out.Versions) != 1 || out.Versions[0].ID != 19 || !out.Orders.Placing || sawKey {
		t.Fatalf("registry %+v key sent %v", out, sawKey)
	}
	d.URL = "http://127.0.0.1:1"
	if _, _, err := d.versions(context.Background(), nil, noInput{}); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("a service that is down must be said so: %v", err)
	}
}
