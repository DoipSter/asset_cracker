package exercise

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A book with a mid of exactly one half (yes bid 0.49, yes ask 0.51) and a thousand contracts
// showing on each side, so a stake fills whole.
var evenBook = Quotes([][2]string{{"0.4900", "1000"}}, [][2]string{{"0.4900", "1000"}})

func view(coin string, pModel float64) engine.View {
	return engine.View{Coin: coin, OK: true, PModel: pModel, OffsetSource: "measured", VolRatio: 1, Horizon: "fast"}
}

// tapeOf lays down snapshots for one market at the given seconds to its close.
func tapeOf(evalID *int64, marketID int64, coin, symbol, result string, closes time.Time, pModel float64, taus ...float64) []Snapshot {
	var out []Snapshot
	for _, tau := range taus {
		*evalID++
		out = append(out, Snapshot{EvaluationID: *evalID, At: closes.Add(-time.Duration(tau * float64(time.Second))), MarketID: marketID,
			Ticker: symbol + "-" + closes.Format("0215"), Coin: coin, Symbol: symbol, Strike: 100, Closes: closes, Result: result, Price: 100,
			Quotes: evenBook, View: view(coin, pModel), HasDepth: true, HasVolRatio: true})
	}
	return out
}

func sortTape(s []Snapshot) []Snapshot {
	t := &SliceTape{Snaps: s}
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].At.Before(s[j-1].At); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return t.Snaps
}

// Value, lambda 0.5, over two windows: a round it wins and a round it loses, each entered at its
// first snapshot. The replay's money adds up: P&L is final cash less the seed, staked is what the
// bets cost with fees inside, both cuts see both markets, and the blocked decisions are the
// seconds it already held a bet.
func TestRunReplaysAShape(t *testing.T) {
	p, err := engine.FromShape(engine.Shape{Name: "Value", Exit: "hold", Lambda: 0.5, StaleCost: 0.0012})
	if err != nil {
		t.Fatal(err)
	}
	var evalID int64
	c1 := time.Date(2026, 9, 22, 5, 0, 0, 0, time.UTC)
	c2 := c1.Add(15 * time.Minute)
	snaps := tapeOf(&evalID, 1, "BTC", "KXBTC15M", "yes", c1, 0.9, 620, 400, 100)
	snaps = append(snaps, tapeOf(&evalID, 2, "BTC", "KXBTC15M", "no", c2, 0.9, 620, 400, 100)...)
	res, err := Run(context.Background(), p, 0, &SliceTape{Snaps: sortTape(snaps)}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Markets != 2 || res.Windows != 2 || res.Snapshots != 6 || res.Bets != 2 || res.Sells != 0 || res.WindowsWithBet != 2 || res.Exhausted {
		t.Fatalf("shape of the run: %+v", res)
	}
	if res.PnLCents != res.FinalCashCents-res.SeedCents || res.SeedCents != 100000 {
		t.Fatalf("P&L %d must be final cash %d less the seed %d", res.PnLCents, res.FinalCashCents, res.SeedCents)
	}
	if res.StakedCents <= 0 || res.FeesCents <= 0 || res.Buys.Orders != 2 || res.Buys.Whole != 2 || res.Buys.Filled != res.Buys.Requested {
		t.Fatalf("stakes and fills: staked %d fees %d buys %+v", res.StakedCents, res.FeesCents, res.Buys)
	}
	// Both entries were at 620 s: the "over 10 min" band, in the 0.40 to 0.60 price band (the ask was 0.51).
	if res.ByEntryBand[0].Markets != 2 || res.ByEntryBand[0].Won != 1 || res.ByEntryBand[0].PnLCents != res.PnLCents || res.ByEntryBand[1].Markets != 0 {
		t.Fatalf("entry bands: %+v", res.ByEntryBand)
	}
	if res.ByEntryPrice[2].Markets != 2 || res.ByEntryPrice[2].AvgEntry != 0.51 || res.ByEntryPrice[2].StakedCents != res.StakedCents {
		t.Fatalf("price bands: %+v", res.ByEntryPrice)
	}
	// A round won pays a dollar a contract; a round lost pays nothing: the winner's P&L is positive, the loser's is its whole cost.
	if len(res.Series) != 2 || res.Series[0][0] != c1.Unix() || res.Series[1][0] != c2.Unix() || res.Series[1][1] != res.PnLCents || res.Series[0][1] <= 0 {
		t.Fatalf("series: %v", res.Series)
	}
	if res.TopWindowShare == 0 || res.Drawdown.WorstWindowCents >= 0 || res.T == 0 {
		t.Fatalf("statistics: share %v drawdown %+v t %v", res.TopWindowShare, res.Drawdown, res.T)
	}
	// The four seconds after each entry were blocked as "max bets this round": Value bets once.
	var maxBets int
	for _, b := range res.Blocked {
		if b.Reason == engine.BlockedMaxBets {
			maxBets = b.Count
		}
	}
	if maxBets != 4 || res.Decisions != 6 {
		t.Fatalf("blocked: %+v (decisions %d)", res.Blocked, res.Decisions)
	}
	if res.Params == nil || res.Params.Name != "Value (conventions)" || res.Family != engine.FamilyRounds {
		t.Fatalf("the params travel with the answer: %+v", res.Params)
	}
}

// A snapshot without depth, or without a model view, decides nothing and is counted; a tape out
// of order is an error, not a quietly wrong replay.
func TestRunCountsWhatTheTapeLacks(t *testing.T) {
	p, _ := engine.FromShape(engine.Shape{Name: "Value", Exit: "hold", Lambda: 0.5})
	c := time.Date(2026, 9, 22, 5, 0, 0, 0, time.UTC)
	var evalID int64
	snaps := tapeOf(&evalID, 1, "BTC", "KXBTC15M", "yes", c, 0.9, 620, 400)
	snaps[0].Quotes, snaps[0].HasDepth = kalshi.Quotes{}, false
	snaps[1].View, snaps[1].HasVolRatio = engine.View{Coin: "BTC"}, false
	res, err := Run(context.Background(), p, 50000, &SliceTape{Snaps: snaps}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bets != 0 || res.SnapshotsWithoutDepth != 1 || res.SnapshotsWithoutView != 1 || res.SeedCents != 50000 || res.FinalCashCents != 50000 || res.Markets != 1 {
		t.Fatalf("%+v", res)
	}
	got := map[string]int{}
	for _, b := range res.Blocked {
		got[b.Reason] = b.Count
	}
	if got[engine.BlockedNoBook] != 1 || got[engine.BlockedNoModel] != 1 {
		t.Fatalf("blocked: %+v", res.Blocked)
	}
	snaps[0], snaps[1] = snaps[1], snaps[0]
	if _, err := Run(context.Background(), p, 0, &SliceTape{Snaps: snaps}, 1); err == nil || !strings.Contains(err.Error(), "out of order") {
		t.Fatalf("an unordered tape must be refused: %v", err)
	}
}

// The late lambda through the replay: a version that barely trusts the model outside two minutes
// and trusts it inside enters only at the late snapshot, and its answer says so by band.
func TestRunWithLateLambda(t *testing.T) {
	p, err := engine.FromShape(engine.Shape{Name: "Horizon", Exit: "hold", Lambda: 0.01, LambdaLate: 1, LambdaLateTau: 120, StaleCost: 0.0012})
	if err != nil {
		t.Fatal(err)
	}
	c := time.Date(2026, 9, 22, 5, 0, 0, 0, time.UTC)
	var evalID int64
	res, err := Run(context.Background(), p, 0, &SliceTape{Snaps: tapeOf(&evalID, 1, "BTC", "KXBTC15M", "yes", c, 0.95, 620, 400, 100)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bets != 1 || res.ByEntryBand[3].Markets != 1 || res.ByEntryBand[0].Markets != 0 || res.PnLCents <= 0 { // 100 s out: "1 to 2 min"
		t.Fatalf("late entry: bets %d bands %+v pnl %d", res.Bets, res.ByEntryBand, res.PnLCents)
	}
}

// The protocol's walk on rows: the 480th eligible window's close is TRAIN's end; one short and
// TRAIN is not complete; a window missing a covered coin, or with one unscored, is not counted;
// a coin not yet covered is not required.
func TestTrainEndOf(t *testing.T) {
	start := t0.Add(13 * time.Minute) // the first close after t0: 04:30
	cov := map[string]time.Time{"KXBTC15M": t0, "KXETH15M": t0}
	now := start.Add(600 * 15 * time.Minute)
	var rows []guardMarket
	for i := 0; i < 500; i++ {
		closes := start.Add(time.Duration(i) * 15 * time.Minute)
		rows = append(rows, guardMarket{"KXBTC15M", closes, "yes", true}, guardMarket{"KXETH15M", closes, "no", true})
	}
	end, complete := trainEndOf(cov, rows, now)
	if !complete || !end.Equal(start.Add(479*15*time.Minute)) {
		t.Fatalf("480 eligible windows: %v %v", end, complete)
	}
	// Window 10 loses its ETH market, window 20 an ETH result, window 30 a scored row: three
	// ineligible windows push the end three windows later.
	var thinned []guardMarket
	for _, r := range rows {
		i := int(r.Closes.Sub(start) / (15 * time.Minute))
		switch {
		case i == 10 && r.Symbol == "KXETH15M":
			continue
		case i == 20 && r.Symbol == "KXETH15M":
			r.Result = ""
		case i == 30 && r.Symbol == "KXETH15M":
			r.Scored = false
		}
		thinned = append(thinned, r)
	}
	end, complete = trainEndOf(cov, thinned, now)
	if !complete || !end.Equal(start.Add(482*15*time.Minute)) {
		t.Fatalf("three ineligible windows: %v %v", end, complete)
	}
	// A coin whose coverage begins later is not required before it is covered.
	cov["KXSOL15M"] = start.Add(200 * 15 * time.Minute)
	if end2, ok := trainEndOf(cov, thinned, now); ok || !end2.IsZero() {
		t.Fatalf("SOL covered from window 200 with no SOL markets: every window from then is ineligible, so TRAIN cannot complete: %v %v", end2, ok)
	}
	delete(cov, "KXSOL15M")
	// Fewer than 480: not complete. Windows closed under fifteen minutes ago are not judged.
	if _, ok := trainEndOf(cov, rows[:2*479], now); ok {
		t.Fatal("479 windows cannot complete TRAIN")
	}
	if _, ok := trainEndOf(cov, rows, start.Add(479*15*time.Minute+time.Minute)); ok {
		t.Fatal("the 480th window closed a moment ago and is not judged yet")
	}
}

// Guard through a stub: TRAIN complete, a market past its end is refused unless the environment
// says the look has been taken; the ladders are not guarded.
func TestGuardMessage(t *testing.T) {
	end := time.Date(2026, 9, 27, 4, 15, 0, 0, time.UTC)
	markets := []MarketRow{{Closes: end.Add(-15 * time.Minute)}, {Closes: end.Add(15 * time.Minute)}}
	err := refuseIf(engine.FamilyRounds, markets, end, true, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "TRAIN") || !strings.Contains(err.Error(), EnvPastTrain) {
		t.Fatalf("must refuse past TRAIN: %v", err)
	}
	if err := refuseIf(engine.FamilyRounds, markets, end, true, func(k string) string { return "yes" }); err != nil {
		t.Fatalf("the owner's word lifts it: %v", err)
	}
	if err := refuseIf(engine.FamilyRounds, markets[:1], end, true, func(string) string { return "" }); err != nil {
		t.Fatalf("at or before TRAIN's end is allowed: %v", err)
	}
	if err := refuseIf(engine.FamilyRounds, markets, end, false, func(string) string { return "" }); err != nil {
		t.Fatalf("TRAIN incomplete: nothing settled is past it: %v", err)
	}
	if err := refuseIf(engine.FamilyLadders, markets, end, true, func(string) string { return "" }); err != nil {
		t.Fatalf("the ladders are under no protocol: %v", err)
	}
}

// ---- the tool ----------------------------------------------------------------------------------

type fakeSource struct {
	snaps   []Snapshot
	markets []MarketRow
	err     error
	got     struct {
		family   string
		from, to time.Time
		step     int
	}
}

func (f *fakeSource) Read(_ context.Context, family string, from, to time.Time, step int, _ time.Time, _ func(string) string) ([]MarketRow, []Snapshot, int, error) {
	f.got.family, f.got.from, f.got.to, f.got.step = family, from, to, step
	return f.markets, f.snaps, 7, f.err
}

type fakeRecorder struct {
	posts []string
	body  []byte
	err   error
}

func (r *fakeRecorder) Post(_ context.Context, path string, body []byte, out any) error {
	r.posts = append(r.posts, path)
	r.body = body
	if r.err != nil {
		return r.err
	}
	return json.Unmarshal([]byte(`{"id": 41}`), out)
}

// The tool over the protocol: the SDK derives the schema from Input and Answer (an embedded
// Shape and an embedded Result), the window forms are the read surface's, the family's caps
// apply, the run is recorded through the door, and a run the door could not record is answered
// and says so.
func TestToolOverProtocol(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	c := time.Date(2026, 9, 22, 5, 0, 0, 0, time.UTC)
	var evalID int64
	src := &fakeSource{snaps: tapeOf(&evalID, 1, "BTC", "KXBTC15M", "yes", c, 0.9, 620, 400), markets: []MarketRow{{ID: 1, Closes: c}}}
	recd := &fakeRecorder{}
	tool := &Tool{Source: src, Recorder: recd, Version: "rel1", Now: func() time.Time { return now }, Getenv: func(string) string { return "" }}
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tool.register(s)

	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	tools, err := sess.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "strategy_exercise" {
		t.Fatalf("tools: %v %+v", err, tools)
	}
	schema, _ := json.Marshal(tools.Tools[0].InputSchema)
	for _, want := range []string{`"from"`, `"lambda"`, `"lambda_late"`, `"step_s"`, `"family"`, `"seed_cents"`} {
		if !strings.Contains(string(schema), want) {
			t.Fatalf("the input schema lacks %s: %s", want, schema)
		}
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "strategy_exercise",
		Arguments: map[string]any{"name": "Value", "exit": "hold", "lambda": 0.5, "from": "2026-09-22T04:30:00Z", "to": "2026-09-23"}})
	if err != nil || res.IsError {
		t.Fatalf("exercise: %v %s", err, text(res))
	}
	var ans Answer
	if err := json.Unmarshal(mustJSON(t, res.StructuredContent), &ans); err != nil {
		t.Fatal(err)
	}
	if ans.Bets != 1 || !ans.Recorded || ans.RecordID != 41 || ans.StepS != DefaultStepRounds || ans.Name != "Value (conventions)" || !ans.Simulated || ans.SnapshotsUnpriced != 7 {
		t.Fatalf("answer: %+v", ans)
	}
	if src.got.family != engine.FamilyRounds || src.got.step != DefaultStepRounds || !src.got.from.Equal(time.Date(2026, 9, 22, 4, 30, 0, 0, time.UTC)) || !src.got.to.Equal(time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the read: %+v", src.got)
	}
	if len(recd.posts) != 1 || recd.posts[0] != RecordPath {
		t.Fatalf("recorded through the door: %v", recd.posts)
	}
	var rec Record
	if err := json.Unmarshal(recd.body, &rec); err != nil || rec.Shape.Name != "Value" || rec.Release != "rel1" || rec.Summary.Bets != 1 || rec.Summary.Params != nil || rec.StepS != DefaultStepRounds {
		t.Fatalf("the record: %v %+v", err, rec)
	}

	// The door refuses (no key, say): the answer stands and says it was not recorded.
	recd.err = errors.New("The operator key is missing or wrong.")
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "strategy_exercise",
		Arguments: map[string]any{"name": "Value", "exit": "hold", "lambda": 0.5, "from": "-20h"}})
	if err != nil || res.IsError {
		t.Fatalf("unrecorded exercise: %v %s", err, text(res))
	}
	_ = json.Unmarshal(mustJSON(t, res.StructuredContent), &ans)
	if ans.Recorded || !strings.Contains(ans.RecordError, "operator key") || ans.Bets != 1 {
		t.Fatalf("must say it was not recorded: %+v", ans)
	}

	// Refusals are tool errors with their reason: the engine's, the window's, the step's.
	for name, args := range map[string]map[string]any{
		"lambda":      {"name": "Zero", "exit": "hold", "from": "-1d"},
		"from":        {"name": "Value", "exit": "hold", "lambda": 0.5},
		"long":        {"name": "Value", "exit": "hold", "lambda": 0.5, "from": "-3d"},
		"step":        {"name": "Day", "exit": "hold", "lambda": 0.5, "family": "kalshiladder", "tau_min": 10800, "tau_max": 108000, "from": "-2d", "step_s": 5},
		"before":      {"name": "Value", "exit": "hold", "lambda": 0.5, "from": "2026-09-23", "to": "2026-09-22"},
		"lambda_late": {"name": "Value", "exit": "hold", "lambda": 0.5, "lambda_late": 0.9, "from": "-1d"},
	} {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "strategy_exercise", Arguments: args})
		if err != nil || !res.IsError || !strings.Contains(text(res), name) {
			t.Fatalf("%s must be refused with its reason: %v %s", name, err, text(res))
		}
	}
	// A ladder shape takes the ladder caps: a week, a step of a minute by default.
	src.got.step = 0
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "strategy_exercise",
		Arguments: map[string]any{"name": "Day", "exit": "hold", "lambda": 0.5, "family": "kalshiladder", "tau_min": 10800, "tau_max": 108000, "from": "-6d"}})
	if err != nil || res.IsError || src.got.family != engine.FamilyLadders || src.got.step != DefaultStepLadders {
		t.Fatalf("ladders: %v %s %+v", err, text(res), src.got)
	}
	// A source that refuses (the guard) is the tool's error, verbatim.
	src.err = errors.New("refused: the window reaches past TRAIN's last close")
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "strategy_exercise", Arguments: map[string]any{"name": "Value", "exit": "hold", "lambda": 0.5, "from": "-1d"}})
	if err != nil || !res.IsError || !strings.Contains(text(res), "TRAIN") {
		t.Fatalf("the guard's refusal: %v %s", err, text(res))
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

func text(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
