package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The protocol's order, end to end, on a scratch checkout with a remote and a fake record: the
// attempt is committed before any outcome is read, each span is looked at once, TEST refuses until
// TRAIN's result is committed and on the remote, and a changed protocol text refuses everything.

func gitIn(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// scratchCheckout copies the protocol texts into a new repository committed at the real T_c, with
// a bare remote holding it.
func scratchCheckout(t *testing.T) repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	real, err := findRepo(".")
	if err != nil {
		t.Skipf("not run from inside the checkout: %v", err)
	}
	root, remote := t.TempDir(), t.TempDir()
	for _, rel := range []string{protocolPath, errataPath, longShotPath} {
		b, err := os.ReadFile(filepath.Join(real.Root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	at := []string{"GIT_AUTHOR_DATE=2026-09-25T08:41:45Z", "GIT_COMMITTER_DATE=2026-09-25T08:41:45Z"}
	gitIn(t, root, nil, "init", "-q")
	gitIn(t, root, nil, "config", "user.email", "t@t")
	gitIn(t, root, nil, "config", "user.name", "t")
	gitIn(t, root, nil, "add", "docs")
	gitIn(t, root, at, "commit", "-q", "-m", "the protocol")
	gitIn(t, remote, nil, "init", "-q", "--bare")
	gitIn(t, root, nil, "remote", "add", "origin", remote)
	gitIn(t, root, nil, "push", "-q", "-u", "origin", "HEAD:main")
	return repo{Root: root}
}

type fakeRecord struct {
	t       *testing.T
	r       repo
	obs     map[string][]observation // by span name
	results map[int64]string
	leftOut []round
	markets []h15Market
	rows    []h15Row
	asked   []string // for each outcomes() call, the attempt files committed when it came
}

func (f *fakeRecord) spanOf(s span) string {
	if s.From.Before(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)) {
		return "train"
	}
	return "test"
}

func (f *fakeRecord) rounds(_ context.Context, s span) (settled, unsettled []round, err error) {
	seen := map[int64]bool{}
	for _, o := range f.obs[f.spanOf(s)] {
		if !seen[o.MarketID] {
			seen[o.MarketID] = true
			settled = append(settled, round{MarketID: o.MarketID, Coin: o.Coin, Closes: o.Closes})
		}
	}
	return settled, f.leftOut, nil
}

func (f *fakeRecord) observations(_ context.Context, s span, ids []int64) ([]observation, error) {
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []observation
	for _, o := range f.obs[f.spanOf(s)] {
		if want[o.MarketID] {
			out = append(out, o)
		}
	}
	return out, nil
}

// outcomes records which attempt file was committed when the outcomes were asked for.
func (f *fakeRecord) outcomes(_ context.Context, ids []int64) (map[int64]string, error) {
	var committed []string
	for _, name := range []string{trainAttemptFile, testAttemptFile} {
		if c, _ := f.r.committedFile(f.r.researchRel(name)); c != "" {
			committed = append(committed, name)
		}
	}
	f.asked = append(f.asked, strings.Join(committed, ","))
	out := map[int64]string{}
	for _, id := range ids {
		out[id] = f.results[id]
	}
	return out, nil
}

func (f *fakeRecord) h15Markets(context.Context, span) ([]h15Market, error) { return f.markets, nil }
func (f *fakeRecord) h15Rows(context.Context, span) ([]h15Row, error)       { return f.rows, nil }

// newFake builds both spans' observations, one outcome per round.
func newFake(t *testing.T, r repo) *fakeRecord {
	f := &fakeRecord{t: t, r: r, obs: map[string][]observation{}, results: map[int64]string{}}
	train, test := spans(time.Date(2026, 9, 25, 8, 41, 45, 0, time.UTC))
	for i, s := range []span{train, test} {
		obs, y := synth(s.From, 14*96, 0.7, uint64(21+i))
		f.obs[s.Name] = obs
		for j, o := range obs {
			if _, ok := f.results[o.MarketID]; !ok {
				f.results[o.MarketID] = map[bool]string{true: "yes", false: "no"}[y[j] == 1]
			}
		}
	}
	w := test.From.Add(time.Hour)
	f.leftOut = []round{{MarketID: 1, Coin: "BTC", Closes: w}}
	f.markets = []h15Market{{MarketID: 7, Symbol: "KXBTC15M", Closes: w, Settled: true}}
	f.rows = []h15Row{{MarketID: 7, Closes: w, Result: "yes", YesBid: 0.92, YesAsk: 0.94, YesBidSize: 5, NoBidSize: 5}}
	return f
}

func newTool(t *testing.T, r repo, src source, cmd string, now time.Time, retry string) *tool {
	tl := &tool{repo: r, src: src, now: now, cmd: cmd, retry: retry, out: func(string, ...any) {}}
	if err := tl.preflight(); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	return tl
}

func TestTheProtocolsOrder(t *testing.T) {
	r := scratchCheckout(t)
	f := newFake(t, r)
	ctx := context.Background()
	trainEnd, testEnd := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)

	// TRAIN refuses before its hour is up, then runs.
	if err := newTool(t, r, f, "train", trainEnd.Add(30*time.Minute), "").train(ctx); err == nil || !strings.Contains(err.Error(), "runs no earlier") {
		t.Fatalf("TRAIN ran early: %v", err)
	}
	tl := newTool(t, r, f, "train", trainEnd.Add(2*time.Hour), "")
	if err := tl.logged(func() error { return tl.train(ctx) }); err != nil {
		t.Fatalf("train: %v", err)
	}
	if len(f.asked) != 1 || f.asked[0] != trainAttemptFile {
		t.Fatalf("outcomes were read with %q committed, want the TRAIN attempt", f.asked)
	}
	for _, name := range []string{trainAttemptFile, trainResultFile, runsFile} {
		if c, err := r.committedFile(r.researchRel(name)); err != nil || c == "" {
			t.Fatalf("%s not committed: %v", name, err)
		}
	}
	if subjects := gitIn(t, r.Root, nil, "log", "--format=%s"); !strings.Contains(subjects, "calibrate train: the attempt, before any outcome is read") || !strings.Contains(subjects, "calibrate train: the result") {
		t.Fatalf("commits: %s", subjects)
	}
	var tr trainResult
	if ok, err := readJSON(r.researchPath(trainResultFile), &tr); !ok || err != nil || tr.Observations != 14*96*2*5 || tr.Provenance.ProtocolSHA != protocolSHA || tr.AttemptCommit == "" {
		t.Fatalf("train result: ok %v err %v %+v", ok, err, tr.Provenance)
	}

	// One look: TRAIN again is refused, with or without a reason.
	for _, reason := range []string{"", "the tunnel dropped"} {
		if err := newTool(t, r, f, "train", trainEnd.Add(3*time.Hour), reason).train(ctx); err == nil || !strings.Contains(err.Error(), "one look") {
			t.Fatalf("a second TRAIN (reason %q) was not refused: %v", reason, err)
		}
	}

	// TEST refuses while the remote does not hold TRAIN's result, then runs once it does.
	if err := newTool(t, r, f, "test", testEnd.Add(2*time.Hour), "").test(ctx); err == nil || !strings.Contains(err.Error(), "remote does not hold") {
		t.Fatalf("TEST ran before TRAIN's result was pushed: %v", err)
	}
	gitIn(t, r.Root, nil, "push", "-q", "origin", "HEAD:main")
	tl = newTool(t, r, f, "test", testEnd.Add(2*time.Hour), "")
	if err := tl.logged(func() error { return tl.test(ctx) }); err != nil {
		t.Fatalf("test: %v", err)
	}
	if len(f.asked) != 2 || !strings.Contains(f.asked[1], testAttemptFile) {
		t.Fatalf("TEST's outcomes were read with %q committed", f.asked)
	}
	var res testResult
	if ok, err := readJSON(r.researchPath(testResultFile), &res); !ok || err != nil || (res.Answer != "yes" && res.Answer != "no") || res.TrainResultCommit == "" {
		t.Fatalf("test result: %v %v %+v", ok, err, res.Answer)
	}
	var a attempt
	if ok, _ := readJSON(r.researchPath(testAttemptFile), &a); !ok || len(a.LeftOut) != 1 || len(a.H15Markets) != 1 || len(a.Observations) != 14*96*2*5 {
		t.Fatalf("test attempt: %+v", a.LeftOut)
	}
	if err := newTool(t, r, f, "test", testEnd.Add(3*time.Hour), "").test(ctx); err == nil || !strings.Contains(err.Error(), "one look") {
		t.Fatalf("a second TEST was not refused: %v", err)
	}
	if out := gitIn(t, r.Root, nil, "status", "--porcelain"); out != "" {
		t.Errorf("a complete run left the checkout dirty: %s", out)
	}

	// A changed protocol text refuses before anything is read.
	if err := os.WriteFile(filepath.Join(r.Root, protocolPath), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&tool{repo: r}).preflight(); err == nil || !strings.Contains(err.Error(), "changed since its commit") {
		t.Fatalf("an edited protocol was not refused: %v", err)
	}
}

// An attempt that failed before its result is rerun only with a quoted reason, which the attempt
// file keeps; a reason with no earlier attempt is refused.
func TestARerunNeedsAReason(t *testing.T) {
	r := scratchCheckout(t)
	f := newFake(t, r)
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC)
	if err := newTool(t, r, f, "train", now, "no attempt yet").train(ctx); err == nil || !strings.Contains(err.Error(), "no earlier attempt") {
		t.Fatalf("a reason with no attempt: %v", err)
	}
	// The first attempt fails after its commit: its outcomes cannot be read.
	failing := &failOutcomes{f}
	tl := newTool(t, r, failing, "train", now, "")
	if err := tl.logged(func() error { return tl.train(ctx) }); err == nil {
		t.Fatal("the failing record did not fail the run")
	}
	if err := newTool(t, r, f, "train", now, "").train(ctx); err == nil || !strings.Contains(err.Error(), "retry-reason") {
		t.Fatalf("a rerun without a reason: %v", err)
	}
	if err := newTool(t, r, f, "train", now, "outcomes read failed: connection reset").train(ctx); err != nil {
		t.Fatalf("a rerun with a reason: %v", err)
	}
	var a attempt
	if ok, _ := readJSON(r.researchPath(trainAttemptFile), &a); !ok || len(a.Attempts) != 2 || a.Attempts[1].Reason != "outcomes read failed: connection reset" {
		t.Fatalf("attempts %+v", a.Attempts)
	}
	runs, _ := os.ReadFile(r.researchPath(runsFile))
	if !strings.Contains(string(runs), `"step":"failed"`) {
		t.Errorf("the failed run is not in runs.jsonl: %s", runs)
	}
}

type failOutcomes struct{ *fakeRecord }

func (f *failOutcomes) outcomes(context.Context, []int64) (map[int64]string, error) {
	return nil, os.ErrDeadlineExceeded
}
