// Package proposals is the agents' door to the strategy builder: the MCP tools of
// `assetcracker mcp` that describe, try and register a version-3 strategy (platform brief §9,
// "proposal tools: draft a strategy version"). It sits beside the read surface on the same
// server (docs/mcp-read-surface.md) and is the only part of that server that changes anything.
//
// What it may change is one thing: it can add a DRAFT row to the trials registry. It does so by
// asking the RUNNING SERVICE, over HTTP on the Pi's loopback, through the same route the
// buckets page uses (POST /api/controls/version/new), with the same operator key, the same
// engine validation and the same labelling. This process has no write grant of its own
// (acdeploy reads the record as assetcracker_ro), so the service's gate is the gate. A draft
// trades nothing: the Approve click on the buckets page seeds it, and that click stays a
// person's. No tool here approves, retires, moves money or places an order.
//
// The dry run (strategy_build) and the presets (strategy_presets) are computed here from the
// engine in this binary, which is the release the service runs; they touch nothing.
package proposals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Environment the tools read. The key is read at call time, so it can be put in place while
// the server runs; it is never logged, echoed or returned.
const (
	EnvServiceURL = "AC_SERVICE_URL"       // the running service; default http://127.0.0.1:8377
	EnvKey        = "AC_OPERATOR_KEY"      // the operator key itself, when the environment carries it
	EnvKeyFile    = "AC_OPERATOR_KEY_FILE" // else a file holding it; default ~/.config/assetcracker/operator_key
	defaultURL    = "http://127.0.0.1:8377"
	keyHeader     = "X-Operator-Key"
	timeout       = 15 * time.Second
	via           = "mcp" // recorded in the version's code_ref by the service
)

// Door is what the tools need: where the service is, and how to find the key.
type Door struct {
	URL     string
	Client  *http.Client
	Getenv  func(string) string
	HomeDir func() (string, error)
}

// New makes a door from the environment.
func New() *Door {
	return &Door{URL: "", Client: &http.Client{Timeout: timeout}, Getenv: os.Getenv, HomeDir: os.UserHomeDir}
}

// Register adds the four tools to an MCP server.
func Register(s *mcp.Server) { New().register(s) }

func (d *Door) register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "strategy_presets", Description: descPresets}, d.presets)
	mcp.AddTool(s, &mcp.Tool{Name: "strategy_build", Description: descBuild}, d.build)
	mcp.AddTool(s, &mcp.Tool{Name: "strategy_register", Description: descRegister}, d.registerVersion)
	mcp.AddTool(s, &mcp.Tool{Name: "strategy_versions", Description: descVersions}, d.versions)
}

const descPresets = `The standard shapes the strategy builder offers, each a starting point for strategy_build or ` +
	`strategy_register: key, title, what it tests, and the shape (the builder's fields, zero meaning "inherit the ` +
	`parent's"). Value, Late, Favourite, Model, Scalper, Calm Scalper, Tail, and a Martingale negative control. ` +
	`Computed from the engine in this binary; reads and changes nothing. Takes no input.`

const descBuild = `A dry run of the builder: the shape in, the version the engine would register out, with every number ` +
	`labelled by its provenance (convention: you set it; inherited: the parent's; limit; fact), the parent it descends ` +
	`from, and its blurb. Refuses exactly as registration would, with the same reason. Nothing is written. Use it to ` +
	`see what a shape means before strategy_register; every registration is one more trial the leaderboard corrects for.`

const descRegister = `Register a version-3 strategy as a DRAFT from a shape, through the running service's builder ` +
	`route (the same one the buckets page uses): the engine validates and labels it, the registry gains a row with ` +
	`status draft, code_ref says it was proposed via mcp. A draft trades nothing until a person clicks Approve on the ` +
	`buckets page, which seeds a $1,000 simulated bucket. hypothesis is required: say what the version is meant to ` +
	`test. The operator key is taken from the server's environment or key file on the Pi, never from this call. ` +
	`One version 3 per name; a name already registered is refused, not replaced.`

const descVersions = `The strategy registry as the buckets page shows it: every version-3 strategy with id, name, status ` +
	`(draft, probation, active, retired) and hypothesis; the new-orders switch and whether this process is placing; ` +
	`the allocation rule in force. Read from the running service. Takes no input.`

type noInput struct{}

// ---- presets and the dry run: the engine in this binary ------------------------------------

func (d *Door) presets(_ context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, PresetList, error) {
	return nil, PresetList{Presets: engine.Presets(),
		Note: "Each shape is a starting point, not a recommendation. A field at zero inherits the parent's setting " +
			"(exit hold: Value; exit ev: Scalper). lambda and stale_cost are the owner's conventions of 2026-09-22."}, nil
}

// PresetList is what strategy_presets answers.
type PresetList struct {
	Presets []engine.Preset `json:"presets"`
	Note    string          `json:"note"`
}

// Built is what strategy_build answers: the version as the registry would hold it.
type Built struct {
	Name    string        `json:"name"`
	Blurb   string        `json:"blurb"`
	Parent  string        `json:"parent" jsonschema:"whose version 2 it descends from: Scalper (exit ev) or Value (exit hold)"`
	Control bool          `json:"control"`
	Params  engine.Params `json:"params" jsonschema:"the engine's parameters with provenance: every knob's kind, value and note"`
	Note    string        `json:"note"`
}

func (d *Door) build(_ context.Context, _ *mcp.CallToolRequest, s engine.Shape) (*mcp.CallToolResult, Built, error) {
	b, err := buildShape(s)
	if err != nil {
		return nil, Built{}, err
	}
	b.Note = "A dry run: nothing was registered. strategy_register with the same shape registers it as a draft. " +
		"provenance kinds: convention = set in this shape by the owner; inherited = the parent's number; limit = a cap; fact = fixed by the protocol."
	return nil, b, nil
}

// buildShape is the builder's engine half, the same as the page's (app.buildVersion).
func buildShape(s engine.Shape) (Built, error) {
	p, err := engine.FromShape(s)
	if err != nil {
		return Built{}, fmt.Errorf("the engine refused the shape: %w", err)
	}
	parent := "Value"
	if p.Exit == "ev" {
		parent = "Scalper"
	}
	return Built{Name: p.Name, Blurb: p.Blurb, Parent: parent, Control: s.Control, Params: p}, nil
}

// ---- registration and the registry: the running service -----------------------------------

// Registered is what strategy_register answers.
type Registered struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Control bool   `json:"control"`
	Note    string `json:"note"`
}

func (d *Door) registerVersion(ctx context.Context, _ *mcp.CallToolRequest, s engine.Shape) (*mcp.CallToolResult, Registered, error) {
	if strings.TrimSpace(s.Hypothesis) == "" {
		return nil, Registered{}, errors.New("hypothesis is required: say what this version is meant to test; it goes in the trials registry")
	}
	// Build here first: the same engine, so a refusal comes back with its reason before anything
	// leaves this process. The service builds again; the two agree because they are one binary.
	if _, err := buildShape(s); err != nil {
		return nil, Registered{}, err
	}
	body, err := json.Marshal(struct {
		engine.Shape
		Via string `json:"via"`
	}{s, via})
	if err != nil {
		return nil, Registered{}, err
	}
	var out Registered
	if err := d.call(ctx, http.MethodPost, "/api/controls/version/new", body, &out); err != nil {
		return nil, Registered{}, err
	}
	out.Note = "Registered as a draft: it trades nothing yet. Approve on the buckets page seeds it $1,000 in simulation " +
		"and it trades on the engine's next look. One more trial in the registry."
	return nil, out, nil
}

// Registry is what strategy_versions answers: the buckets page's controls document.
type Registry struct {
	Simulated bool      `json:"simulated"`
	Locked    bool      `json:"locked" jsonschema:"true when the service requires the operator key for changes"`
	Orders    Orders    `json:"orders"`
	Versions  []Version `json:"versions"`
	Policy    Policy    `json:"policy"`
	Note      string    `json:"note"`
}

// Orders is the new-orders switch and the engine's state.
type Orders struct {
	On        bool   `json:"on"`
	Source    string `json:"source"`
	Placing   bool   `json:"placing"`
	Effective string `json:"effective"`
}

// Version is one version-3 row.
type Version struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Version    int    `json:"version"`
	Status     string `json:"status"`
	Hypothesis string `json:"hypothesis"`
}

// Policy is the sustainment allocation rule in force.
type Policy struct {
	ID           int64  `json:"id"`
	SinceAt      string `json:"since_at"`
	Note         string `json:"note"`
	WinningsBps  int    `json:"winnings_bps"`
	ReplenishBps int    `json:"replenish_bps"`
	TaxBps       int    `json:"tax_bps"`
	FeesBps      int    `json:"fees_bps"`
}

func (d *Door) versions(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, Registry, error) {
	var out Registry
	if err := d.call(ctx, http.MethodGet, "/api/controls", nil, &out); err != nil {
		return nil, Registry{}, err
	}
	out.Note = "Simulated money. draft: registered, not seeded. probation/active: holds a bucket and trades. " +
		"retired: its bucket is held settle-only. Approval and retirement are clicks on the buckets page."
	return nil, out, nil
}

// call sends one request to the running service and decodes its JSON answer. A POST carries the
// operator key when one is found. The service's error text is passed on as the tool's error;
// a 401 adds where the key goes.
func (d *Door) call(ctx context.Context, method, path string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base()+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		if key := d.key(); key != "" {
			req.Header.Set(keyHeader, key)
		}
	}
	res, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf("the service at %s did not answer (%v): is assetcracker.service running on the Pi?", d.base(), unwrapURL(err))
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		if res.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%s The service is locked with AC_OPERATOR_KEY; this server takes the key from %s, or from the file %s (default %s on the Pi, owned by the user running the server, mode 600). Put it there; nothing restarts.",
				e.Error, EnvKey, EnvKeyFile, d.keyPath())
		}
		return fmt.Errorf("the service refused (%d): %s", res.StatusCode, e.Error)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the service's answer could not be read: %w", err)
	}
	return nil
}

func (d *Door) base() string {
	if d.URL != "" {
		return strings.TrimRight(d.URL, "/")
	}
	if u := strings.TrimSpace(d.Getenv(EnvServiceURL)); u != "" {
		return strings.TrimRight(u, "/")
	}
	return defaultURL
}

// key finds the operator key: the environment, else the key file. Empty means none, and the
// request goes without one, which the service accepts only when it has no key itself.
func (d *Door) key() string {
	if k := strings.TrimSpace(d.Getenv(EnvKey)); k != "" {
		return k
	}
	b, err := os.ReadFile(d.keyPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (d *Door) keyPath() string {
	if p := strings.TrimSpace(d.Getenv(EnvKeyFile)); p != "" {
		return p
	}
	home, err := d.HomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "assetcracker", "operator_key")
	}
	return filepath.Join(home, ".config", "assetcracker", "operator_key")
}

// unwrapURL strips the request's URL from a transport error so the message reads once.
func unwrapURL(err error) error {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err
		}
		err = u
	}
}
