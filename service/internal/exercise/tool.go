package exercise

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/readsurface"
	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP tool. It reads the tape in the same read-only transaction the read surface uses,
// replays the shape in this process, and then asks the RUNNING service, through its operator
// key like strategy_register does, to write the one row that says the run happened
// (POST /api/controls/exercise/record, an analysis_result of key strategy.exercise). This
// process has no write grant, so that route is the only mark a run can leave.

// RecordKey is the analysis_result key every run is written under.
const RecordKey = "strategy.exercise"

// RecordPath is the service route that writes the record.
const RecordPath = "/api/controls/exercise/record"

// Budget is the most one call may take, reads and replay together. A 48-hour roster walk
// runs four Decide paths per snapshot (three shadows and the live seat).
const Budget = 90 * time.Second

// Input is the tool's argument: a shape, exactly as strategy_build takes it, and a window.
type Input struct {
	engine.Shape
	From      string `json:"from" jsonschema:"start of the window, inclusive: markets CLOSING from this time are replayed. RFC 3339 (2026-09-22T04:30:00Z), a date (2026-09-22), or relative to now: -24h, -2d. Their snapshots from their open are read, so a ladder window reaches back days"`
	To        string `json:"to,omitempty" jsonschema:"end of the window, exclusive; default now. Only markets that have SETTLED are replayed, so a window into the open round holds nothing of it. At most 48 hours for the 15-minute rounds, 7 days for the ladders; a longer run is several calls"`
	StepS     int    `json:"step_s,omitempty" jsonschema:"thinning: the first recorded snapshot of each market in every step_s seconds is stepped, as if the engine looked that often. Default 1 for the rounds, every recorded second, as the live engine looks; a coarser step is faster and loses the bets a flickering ask would have given a band-restricted shape (measured: one in five cost Mid-round Favourite 21 of 76 bets). Default 60 for the ladders, at least 30"`
	SeedCents int64  `json:"seed_cents,omitempty" jsonschema:"the simulated bucket's starting cash in cents; default the convention, 100000 ($1,000), so the lines compare"`
}

// Answer is what the tool returns: the run, and whether it was recorded.
type Answer struct {
	Result
	Recorded    bool   `json:"recorded" jsonschema:"true when the running service wrote the analysis_result row for this run"`
	RecordID    int64  `json:"record_id,omitempty"`
	RecordError string `json:"record_error,omitempty" jsonschema:"why the run was not recorded, when it was not; the answer stands, the count does not"`
}

// Recorder posts the record through the running service. *proposals.Door is one (its Post); the
// interface is here so this package sits beside proposals rather than on it.
type Recorder interface {
	Post(ctx context.Context, path string, body []byte, out any) error
}

// Source reads the tape for a family and window and applies the protocol guard to what it read.
// unpriced is the count of stepped snapshots of coins the model never priced, left unread.
type Source interface {
	Read(ctx context.Context, family string, from, to time.Time, stepS int, now time.Time, getenv func(string) string) (markets []MarketRow, snaps []Snapshot, unpriced int, err error)
}

// storeSource reads from the record: markets, the guard, then the snapshots, in one transaction.
type storeSource struct{ db *store.Store }

func (s storeSource) Read(ctx context.Context, family string, from, to time.Time, stepS int, now time.Time, getenv func(string) string) (markets []MarketRow, snaps []Snapshot, unpriced int, err error) {
	r := Reader{DB: s.db}
	err = s.db.ReadOnly(ctx, ReadTimeout, func(q store.Querier) error {
		var err error
		if markets, err = r.Markets(ctx, q, family, from, to); err != nil {
			return err
		}
		if err := Guard(ctx, q, family, markets, now, getenv); err != nil {
			return err
		}
		snaps, unpriced, err = r.Snapshots(ctx, q, markets, stepS)
		return err
	})
	return markets, snaps, unpriced, err
}

// Tool is the strategy_exercise tool with what it needs.
type Tool struct {
	Source   Source
	Recorder Recorder // nil: runs are not recorded, and every answer says so
	Version  string   // the release, written into the record
	Now      func() time.Time
	Getenv   func(string) string
}

// Register adds strategy_exercise to an MCP server, reading the record through db and
// recording through door (the proposals door, which posts with the operator key; nil records
// nothing and every answer says so).
func Register(s *mcp.Server, db *store.Store, door Recorder, version string) {
	NewTool(db, door, version).register(s)
}

// NewTool is the same tool the MCP server holds, for a stdin run on the Pi without replacing
// the live mcp process.
func NewTool(db *store.Store, door Recorder, version string) *Tool {
	return &Tool{Source: storeSource{db}, Recorder: door, Version: version, Now: time.Now, Getenv: os.Getenv}
}

// Run is one replay: the same path as the MCP tool.
func (t *Tool) Run(ctx context.Context, in Input) (Answer, error) {
	_, ans, err := t.exercise(ctx, nil, in)
	return ans, err
}

func (t *Tool) register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "strategy_exercise", Description: Description}, t.exercise)
}

// Description is the tool's, as the MCP client shows it.
const Description = `Run a strategy shape through the third engine on the recorded tape: what the version strategy_build ` +
	`describes would have done, seeded over a window of markets that have already settled. The same engine.Decide, the same ` +
	`paper broker filling only what the recorded book displayed, the same fold and settlement as the live runner; the tape ` +
	`for a venue. Answers a leaderboard-style row (bets, windows, P&L, staked, return per dollar, t, top-window share, ` +
	`drawdown), P&L cut by the entry's time to close and by its price, every blocked decision counted by reason, and ` +
	`orders asked against filled. Simulated money; nothing is registered, seeded or placed. NOT a measurement: a shape ` +
	`that looks good on the window it was tuned on has been tuned on it, and every registration is still judged live at ` +
	`the corrected threshold. Every run is recorded as an analysis_result row (key strategy.exercise) so the shapes tried ` +
	`are counted beside the shapes registered. Refuses 15-minute windows past the v3 protocol's TRAIN until TEST has been ` +
	`looked at. A roster (members: 2 to 8 shapes) is one version: assign=both (default) gives the ` +
	`window owner first refusal then later specialists on unclaimed seats; assign=window is owner only; ` +
	`assign=reserve sits until the latest specialist's clock. lookback_windows (default 16) is prior ` +
	`clocks, not entries; structural_only makes the first member the owner. The answer then includes ` +
	`by_member, by_owner and pick counts (sit_out / warmup / window / reserve). ` +
	`Reads at most 48 hours of rounds (7 days of ladders) per call; longer runs are several calls.`

func (t *Tool) exercise(ctx context.Context, _ *mcp.CallToolRequest, in Input) (*mcp.CallToolResult, Answer, error) {
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	now := t.Now().UTC()

	p, err := engine.FromShape(in.Shape)
	if err != nil {
		return nil, Answer{}, fmt.Errorf("the engine refused the shape: %w", err)
	}
	family := p.FamilyOf()
	lim := LimitsOf(family)
	if strings.TrimSpace(in.From) == "" {
		return nil, Answer{}, fmt.Errorf("from is required: the replay is a window of settled markets")
	}
	from, err := readsurface.ParseTime(in.From, now)
	if err != nil {
		return nil, Answer{}, fmt.Errorf("from: %w", err)
	}
	to := now
	if strings.TrimSpace(in.To) != "" {
		if to, err = readsurface.ParseTime(in.To, now); err != nil {
			return nil, Answer{}, fmt.Errorf("to: %w", err)
		}
	}
	if !to.After(from) {
		return nil, Answer{}, fmt.Errorf("the window ends (%s) before it starts (%s)", to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	if to.Sub(from) > lim.MaxSpan {
		return nil, Answer{}, fmt.Errorf("the window is %s long; a %s replay reads at most %s per call: run it in pieces", to.Sub(from).Round(time.Minute), family, lim.MaxSpan)
	}
	step := in.StepS
	switch {
	case step == 0:
		step = lim.DefaultStep
	case step < lim.MinStep:
		return nil, Answer{}, fmt.Errorf("step_s %d is under the least a %s replay may step, %d seconds", step, family, lim.MinStep)
	}
	if in.SeedCents < 0 {
		return nil, Answer{}, fmt.Errorf("seed_cents must not be negative")
	}

	markets, snaps, unpriced, err := t.Source.Read(ctx, family, from, to, step, now, t.getenv())
	if err != nil {
		return nil, Answer{}, err
	}
	res, err := Run(ctx, p, in.SeedCents, &SliceTape{Snaps: snaps}, step)
	if err != nil {
		return nil, Answer{}, fmt.Errorf("the replay failed: %w", err)
	}
	res.From, res.To, res.SnapshotsUnpriced = from, to, unpriced
	if len(markets) == 0 {
		res.Note = "No settled market of this family closes in the window: nothing was replayed. " +
			"A round settles a few minutes after its close; the ladders close at 5 pm ET. "
	}
	res.Note += "A replay over the tape, not a measurement: the window and the shape were chosen, and a shape read on the window it was " +
		"tuned on has been tuned on it. It is the live engine's path on the recorded book, not a copy of a live bucket: the live runner " +
		"drops a coin's second when another coin's write holds its lock, no sustainment allocation is taken here, and settlement is " +
		"applied at the first snapshot at or after the close; against a registered version over its own window (2026-09-23) the entry " +
		"seconds agreed and the stakes drifted by a contract or two as the cash paths parted. Snapshots without depth (before release " +
		"87a2100) decide nothing. A step above 1 drops the bets a flickering ask would have given a band-restricted shape. A roster is " +
		"one version: assign is window owner then later specialists on unclaimed seats, and by_member / by_owner / picks say who fired. Registering " +
		"the shape is strategy_register; it is then judged live at the corrected threshold."

	ans := Answer{Result: res}
	ans.Recorded, ans.RecordID, ans.RecordError = t.record(ctx, in, res)
	return nil, ans, nil
}

func (t *Tool) getenv() func(string) string {
	if t.Getenv != nil {
		return t.Getenv
	}
	return func(string) string { return "" }
}

// Record is the body the tool posts, and the service writes as the analysis_result's params and
// result: the shape and window (params, what makes it reproducible) and the summary (result).
type Record struct {
	Shape     engine.Shape `json:"shape"`
	Family    string       `json:"family"`
	From      time.Time    `json:"from"`
	To        time.Time    `json:"to"`
	StepS     int          `json:"step_s"`
	SeedCents int64        `json:"seed_cents"`
	Release   string       `json:"release"` // the MCP process's build, which is the engine that replayed
	Summary   Result       `json:"summary"`
}

// record posts the run through the service. A run that could not be recorded is still answered;
// the answer says so, because the count is the point of the record.
func (t *Tool) record(ctx context.Context, in Input, res Result) (ok bool, id int64, why string) {
	if t.Recorder == nil {
		return false, 0, "this server has no door to the running service: the run was not recorded"
	}
	summary := res
	summary.Params = nil // the shape is the record; the params are its build
	rec := Record{Shape: in.Shape, Family: res.Family, From: res.From, To: res.To, StepS: res.StepS, SeedCents: res.SeedCents, Release: t.Version, Summary: summary}
	body, err := json.Marshal(rec)
	if err != nil {
		return false, 0, err.Error()
	}
	var out struct {
		ID int64 `json:"id"`
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := t.Recorder.Post(rctx, RecordPath, body, &out); err != nil {
		return false, 0, err.Error()
	}
	return true, out.ID, ""
}
