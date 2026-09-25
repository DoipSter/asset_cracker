// calibrate runs docs/calibration-protocol.md: the fitted belief's one look at TRAIN, then its
// one look at TEST, read-only on the record.
//
//	calibrate check                  the checkout, the spans, and what the record holds so far; reads no outcome
//	calibrate train [-retry-reason s] TRAIN: commit the attempt, read the outcomes, fit, commit the result
//	calibrate test  [-retry-reason s] TEST, once TRAIN's result is committed and on the remote
//
// It runs from a checkout (T_c comes from git), against the record over the documented tunnel:
//
//	ssh -N -L 5433:/var/run/postgresql/.s.PGSQL.5432 acdeploy@rpi-v5-1.local &
//	AC_DATABASE_URL='postgres://acdeploy@127.0.0.1:5433/assetcracker' go run ./cmd/calibrate check
//
// Every read takes assetcracker_ro inside a READ ONLY transaction (store.ReadOnly). The only
// writes are its own files under research/calibration and the commits the protocol asks for.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "calibrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: calibrate check | train [-retry-reason text] | test [-retry-reason text]")
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("calibrate "+cmd, flag.ContinueOnError)
	dbURL := fs.String("db", os.Getenv("AC_DATABASE_URL"), "the record's connection string (AC_DATABASE_URL)")
	dir := fs.String("root", ".", "a directory inside the checkout")
	retry := fs.String("retry-reason", "", "train and test: the mechanical failure a rerun follows, quoted")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	switch cmd {
	case "check", "train", "test":
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	rp, err := findRepo(*dir)
	if err != nil {
		return err
	}
	// The checkout is checked before any connection is made.
	t := &tool{repo: rp, now: time.Now().UTC(), retry: *retry, cmd: cmd}
	if err := t.preflight(); err != nil {
		return err
	}
	if *dbURL == "" {
		if cmd == "check" {
			return t.checkOffline()
		}
		return errors.New("set AC_DATABASE_URL or -db to the record's connection string")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, *dbURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()
	t.src = reader{db}
	switch cmd {
	case "check":
		return t.check(ctx)
	case "train":
		return t.logged(func() error { return t.train(ctx) })
	default:
		return t.logged(func() error { return t.test(ctx) })
	}
}

// tool is one run.
type tool struct {
	repo  repo
	src   source
	now   time.Time
	retry string
	cmd   string

	tcCommit  string
	tc        time.Time
	errata    errataState
	head      string
	trainSpan span
	testSpan  span
	out       func(format string, a ...any) // printing; stdout unless a test sets it
}

func (t *tool) printf(format string, a ...any) {
	if t.out != nil {
		t.out(format, a...)
		return
	}
	fmt.Printf(format, a...)
}

func (t *tool) preflight() error {
	var err error
	if t.tcCommit, t.tc, err = t.repo.protocolCommit(); err != nil {
		return err
	}
	if t.errata, err = t.repo.checkErrata(); err != nil {
		return err
	}
	if t.head, err = t.repo.headSHA(); err != nil {
		return err
	}
	t.trainSpan, t.testSpan = spans(t.tc)
	return nil
}

func (t *tool) provenance(s span) provenance {
	return provenance{ToolGitSHA: t.head, ProtocolSHA: protocolSHA, TC: t.tc, TCCommit: t.tcCommit, Errata: t.errata, Span: s, RunAt: t.now}
}

// logged makes sure every run, complete or not, is in runs.jsonl ("The files"). A run that
// completes wrote its lines into its own commits; one that fails appends its error here, and the
// next attempt's commit carries it.
func (t *tool) logged(fn func() error) error {
	err := fn()
	if err != nil {
		rec := runRecord{At: time.Now().UTC(), Command: t.cmd, ToolSHA: t.head, Step: "failed", Error: err.Error()}
		if lerr := appendRun(t.repo.researchPath(runsFile), rec); lerr != nil {
			return fmt.Errorf("%w (and runs.jsonl could not record it: %v)", err, lerr)
		}
	}
	return err
}

func (t *tool) checkOffline() error {
	t.printf("protocol  %s  T_c %s (commit %s)\n", protocolPath, t.tc.Format(time.RFC3339), t.tcCommit[:12])
	for _, s := range []span{t.trainSpan, t.testSpan} {
		state := "ready"
		if err := s.ready(t.now); err != nil {
			state = "not yet: runs from " + s.To.Add(runAfter).Format(time.RFC3339)
		}
		t.printf("%-5s     closes %s to %s  %s\n", s.Name, s.From.Format(time.RFC3339), s.To.Format(time.RFC3339), state)
	}
	return nil
}

// check reports what the record holds so far, counting rows and reading no outcome.
func (t *tool) check(ctx context.Context) error {
	if err := t.checkOffline(); err != nil {
		return err
	}
	for _, s := range []span{t.trainSpan, t.testSpan} {
		if !t.now.After(s.From) {
			continue
		}
		sofar := s
		if t.now.Before(s.To) {
			sofar.To = t.now
		}
		settled, unsettled, err := t.src.rounds(ctx, sofar)
		if err != nil {
			return err
		}
		obs, err := t.src.observations(ctx, sofar, roundIDs(settled))
		if err != nil {
			return err
		}
		t.printf("%-5s     so far: %d rounds with a result, %d without, %d observations\n", s.Name, len(settled), len(unsettled), len(obs))
	}
	return nil
}

func roundIDs(rs []round) []int64 {
	ids := make([]int64, len(rs))
	for i, r := range rs {
		ids[i] = r.MarketID
	}
	return ids
}

func keysOf(obs []observation) []obsKey {
	out := make([]obsKey, len(obs))
	for i, o := range obs {
		out[i] = obsKey{EvaluationID: o.EvaluationID, MarketID: o.MarketID, Bin: o.Bin}
	}
	return out
}

// begin refuses a span whose result is committed, reads the population and the rows (no outcome),
// and writes and commits the attempt. A rerun needs -retry-reason: the protocol allows one only
// for a mechanical failure, with the error quoted.
func (t *tool) begin(ctx context.Context, s span, attemptName, resultName string, h15 bool) (*attempt, []round, []observation, []h15Market, string, error) {
	if err := s.ready(t.now); err != nil {
		return nil, nil, nil, nil, "", err
	}
	if c, err := t.repo.committedFile(t.repo.researchRel(resultName)); err != nil {
		return nil, nil, nil, nil, "", err
	} else if c != "" {
		return nil, nil, nil, nil, "", fmt.Errorf("refusing: %s is committed (%s): one look", t.repo.researchRel(resultName), c[:12])
	}
	var a attempt
	exists, err := readJSON(t.repo.researchPath(attemptName), &a)
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	if exists && t.retry == "" {
		return nil, nil, nil, nil, "", fmt.Errorf("refusing: %s exists; a rerun is only for a mechanical failure, quoted with -retry-reason", t.repo.researchRel(attemptName))
	}
	if !exists && t.retry != "" {
		return nil, nil, nil, nil, "", errors.New("refusing: -retry-reason with no earlier attempt")
	}
	settled, unsettled, err := t.src.rounds(ctx, s)
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	if len(settled) == 0 {
		return nil, nil, nil, nil, "", fmt.Errorf("%s holds no settled round", s.Name)
	}
	obs, err := t.src.observations(ctx, s, roundIDs(settled))
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	var markets []h15Market
	if h15 {
		if markets, err = t.src.h15Markets(ctx, s); err != nil {
			return nil, nil, nil, nil, "", err
		}
	}
	a.Provenance = t.provenance(s)
	a.Attempts = append(a.Attempts, attemptRecord{At: t.now, Reason: t.retry})
	a.Rounds, a.LeftOut, a.Observations, a.H15Markets = settled, unsettled, keysOf(obs), markets
	if a.LeftOut == nil {
		a.LeftOut = []round{}
	}
	if err := writeJSON(t.repo.researchPath(attemptName), a); err != nil {
		return nil, nil, nil, nil, "", err
	}
	if err := appendRun(t.repo.researchPath(runsFile), runRecord{At: time.Now().UTC(), Command: t.cmd, ToolSHA: t.head, Step: "attempt written"}); err != nil {
		return nil, nil, nil, nil, "", err
	}
	commit, err := t.repo.commit(fmt.Sprintf("calibrate %s: the attempt, before any outcome is read", s.Name),
		t.repo.researchRel(attemptName), t.repo.researchRel(runsFile))
	if err != nil {
		return nil, nil, nil, nil, "", fmt.Errorf("committing the attempt: %w", err)
	}
	return &a, settled, obs, markets, commit, nil
}

// labelled pairs each observation with its outcome, read only after the attempt's commit. An
// observation whose round has no result now is a mechanical failure: the attempt said it had one.
func labelled(obs []observation, results map[int64]string) ([]float64, error) {
	y := make([]float64, len(obs))
	for i, o := range obs {
		switch results[o.MarketID] {
		case "yes":
			y[i] = 1
		case "no":
		default:
			return nil, fmt.Errorf("round %d had a result when the attempt was written and has none now", o.MarketID)
		}
	}
	return y, nil
}

func (t *tool) train(ctx context.Context) error {
	s := t.trainSpan
	_, settled, obs, _, attemptCommit, err := t.begin(ctx, s, trainAttemptFile, trainResultFile, false)
	if err != nil {
		return err
	}
	results, err := t.src.outcomes(ctx, roundIDs(settled))
	if err != nil {
		return err
	}
	y, err := labelled(obs, results)
	if err != nil {
		return err
	}
	res, err := fitTrain(obs, y)
	if err != nil {
		return err
	}
	res.Provenance, res.AttemptCommit, res.Rounds = t.provenance(s), attemptCommit, len(settled)
	if err := writeJSON(t.repo.researchPath(trainResultFile), res); err != nil {
		return err
	}
	if err := appendRun(t.repo.researchPath(runsFile), runRecord{At: time.Now().UTC(), Command: t.cmd, ToolSHA: t.head, Step: "result written"}); err != nil {
		return err
	}
	commit, err := t.repo.commit("calibrate train: the result", t.repo.researchRel(trainResultFile), t.repo.researchRel(runsFile))
	if err != nil {
		return fmt.Errorf("committing the result: %w", err)
	}
	t.printf("TRAIN: %d observations on %d rounds; b %.4f, c %.4f (monotone %v); committed %s. Push it: TEST refuses a result the remote does not hold.\n",
		res.Observations, res.Rounds, res.B, res.C, res.Monotone, commit[:12])
	return nil
}

// fitTrain fits the calibrator on TRAIN and reports it.
func fitTrain(obs []observation, y []float64) (trainResult, error) {
	var res trainResult
	sc := fitScaler(obs)
	X, err := sc.matrix(obs)
	if err != nil {
		return res, err
	}
	beta, it, err := fitRidge(X, y)
	if err != nil {
		return res, err
	}
	clusters := make([]time.Time, len(obs))
	closes := map[time.Time]bool{}
	for i, o := range obs {
		clusters[i], closes[o.Closes] = o.Closes, true
	}
	se, err := clusteredSE(X, y, beta, clusters)
	if err != nil {
		return res, err
	}
	m := model{Scaler: sc, Beta: beta, Iterations: it}
	res.Model, res.Observations, res.CloseTimes = m, len(obs), len(closes)
	res.Coefficients = []coefficient{{Name: colIntercept, Standardised: beta[0], Unstandardised: math.NaN(), ClusteredSE: se[0]}}
	for j, name := range sc.Columns {
		res.Coefficients = append(res.Coefficients, coefficient{Name: name, Standardised: beta[1+j], Unstandardised: beta[1+j] / sc.SD[j], ClusteredSE: se[1+j]})
	}
	b, okB := m.unstandardised(colMid)
	c, okC := m.unstandardised(colModel)
	res.B, res.C = b, c
	switch {
	case !okB || !okC:
		res.B, res.C = math.NaN(), math.NaN()
		res.MonotoneNote = "logit(mid) or logit(p_model) was constant on TRAIN and left out: monotonicity cannot be checked, and nothing is registered"
	case b < 0 || c < 0:
		res.MonotoneNote = "a fitted b < 0 or c < 0: reported, and the version is not registered"
	default:
		res.Monotone, res.MonotoneNote = true, "b >= 0 and c >= 0"
	}
	pcal, err := m.predict(obs)
	if err != nil {
		return res, err
	}
	sc2 := scoredOf(obs, pcal, y)
	res.InSample = figuresOf(sc2, func(scored) bool { return true })
	for k := range bins {
		k := k
		res.InSampleBins = append(res.InSampleBins, binInSample{Bin: binLabels[k], Figures: figuresOf(sc2, func(s scored) bool { return s.bin == k })})
	}
	res.Conventions = map[string]any{"lambda_ridge": lambdaRidge, "likelihood": "summed", "penalty": "half lambda sum beta^2, intercept free",
		"prob_hold": probHold, "newton_tol": newtonTol, "newton_max_iterations": newtonMaxIt, "reference_bin": binLabels[0], "reference_coin": refCoin}
	return res, nil
}

func scoredOf(obs []observation, pcal, y []float64) []scored {
	out := make([]scored, len(obs))
	for i, o := range obs {
		q := mid(o)
		out[i] = scored{bin: o.Bin, closes: o.Closes, cal: pcal[i], mid: q, blend: q + blendLambda*(o.PModel-q), y: y[i]}
	}
	return out
}

func (t *tool) test(ctx context.Context) error {
	s := t.testSpan
	trainRel := t.repo.researchRel(trainResultFile)
	trainCommit, err := t.repo.committedFile(trainRel)
	if err != nil {
		return err
	}
	if trainCommit == "" {
		return fmt.Errorf("refusing: %s is not committed; TEST runs only after TRAIN's result is", trainRel)
	}
	if held, err := t.repo.heldByRemote(trainCommit); err != nil {
		return err
	} else if !held {
		return fmt.Errorf("refusing: the remote does not hold %s (%s); push it first", trainRel, trainCommit[:12])
	}
	var tr trainResult
	if ok, err := readJSON(t.repo.researchPath(trainResultFile), &tr); err != nil || !ok {
		return fmt.Errorf("reading %s: %v", trainRel, err)
	}
	_, settled, obs, markets, attemptCommit, err := t.begin(ctx, s, testAttemptFile, testResultFile, true)
	if err != nil {
		return err
	}
	results, err := t.src.outcomes(ctx, roundIDs(settled))
	if err != nil {
		return err
	}
	y, err := labelled(obs, results)
	if err != nil {
		return err
	}
	rows, err := t.src.h15Rows(ctx, s)
	if err != nil {
		return err
	}
	longShotSHA, err := sha256File(filepath.Join(t.repo.Root, longShotPath))
	if err != nil {
		return err
	}
	res, err := judgeTest(tr, obs, y, rows, markets, longShotSHA)
	if err != nil {
		return err
	}
	res.Provenance, res.TrainResultCommit, res.AttemptCommit = t.provenance(s), trainCommit, attemptCommit
	if err := writeJSON(t.repo.researchPath(testResultFile), res); err != nil {
		return err
	}
	if err := appendRun(t.repo.researchPath(runsFile), runRecord{At: time.Now().UTC(), Command: t.cmd, ToolSHA: t.head, Step: "result written"}); err != nil {
		return err
	}
	commit, err := t.repo.commit("calibrate test: the result", t.repo.researchRel(testResultFile), t.repo.researchRel(runsFile))
	if err != nil {
		return fmt.Errorf("committing the result: %w", err)
	}
	t.printf("TEST: answer %s (useful %v, %d of 5 bins, overall improvement %.5f, p5 %.5f; monotone %v). H15: %s (t %.2f, %d blocks). Committed %s.\n",
		res.Answer, res.Verdict.Useful, res.Verdict.BinsPassing, res.Verdict.Overall.Improvement, res.Verdict.OverallP5, res.Monotone, res.H15.Verdict, res.H15.T, res.H15.Blocks, commit[:12])
	return nil
}

// judgeTest scores TEST with TRAIN's frozen calibrator and applies the pass rule and H15's row.
func judgeTest(tr trainResult, obs []observation, y []float64, rows []h15Row, markets []h15Market, longShotSHA string) (testResult, error) {
	var res testResult
	pcal, err := tr.Model.predict(obs)
	if err != nil {
		return res, err
	}
	res.Verdict = judge(scoredOf(obs, pcal, y))
	res.Monotone = tr.Monotone
	res.Registrable = res.Verdict.Useful && tr.Monotone
	res.Answer = "no"
	if res.Registrable {
		res.Answer = "yes"
	}
	res.H15 = h15(rows, markets, longShotSHA)
	return res, nil
}
