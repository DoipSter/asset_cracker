// measure3 runs docs/v3-measurement-protocol.md: the pre-registered measurement of the third
// engine's four numbers on the record, read-only.
//
//	measure3 train                  walk to TRAIN's 480th eligible window; measure; freeze once complete
//	measure3 test [-retry-reason s] the one look at TEST, once TRAIN is frozen with lambda > 0
//	measure3 emit-migration         write the version-3 rows' migration from frozen-params.json
//
// It runs from a checkout (T_c comes from git), against the record over the documented tunnel:
//
//	ssh -N -L 5433:/var/run/postgresql/.s.PGSQL.5432 acdeploy@rpi-v5-1.local &
//	AC_DATABASE_URL='postgres://acdeploy@127.0.0.1:5433/assetcracker' go run ./cmd/measure3 train
//
// The connection arrives at the Pi's socket as acdeploy, and every read takes assetcracker_ro
// inside a READ ONLY transaction (store.ReadOnly). Nothing here can write to the database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// t0 is fixed by the protocol's second amendment (section 1).
var t0 = time.Date(2026, 9, 22, 4, 17, 4, 0, time.UTC)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "measure3:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: measure3 train | test [-retry-reason text] | emit-migration")
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("measure3 "+cmd, flag.ContinueOnError)
	dbURL := fs.String("db", os.Getenv("AC_DATABASE_URL"), "the record's connection string (AC_DATABASE_URL)")
	dir := fs.String("root", ".", "a directory inside the checkout")
	retry := fs.String("retry-reason", "", "test only: why an attempt is being retried")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	rp, err := findRepo(*dir)
	if err != nil {
		return err
	}
	switch cmd {
	case "emit-migration":
		return emitMigration(rp)
	case "train", "test":
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	// The checkout is checked before any connection is made: a wrong protocol text or a dirty
	// errata file refuses the run whether or not the record can be reached.
	m := &measurer{repo: rp, now: time.Now().UTC()}
	if _, err := m.preflight(); err != nil {
		return err
	}
	if *dbURL == "" {
		return errors.New("set AC_DATABASE_URL or -db to the record's connection string")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, *dbURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()
	m.r = reader{db}
	if cmd == "train" {
		return m.train(ctx)
	}
	return m.test(ctx, *retry)
}

type measurer struct {
	repo repo
	r    reader
	now  time.Time
	prov *provenanceRecord // preflight's answer, made once
}

// preflight is what both commands check before any read: T_c, the protocol text, the errata.
// It runs once; a second call returns the first answer.
func (m *measurer) preflight() (prov provenanceRecord, err error) {
	if m.prov != nil {
		return *m.prov, nil
	}
	defer func() {
		if err == nil {
			m.prov = &prov
		}
	}()
	hash, tc, err := m.repo.protocolCommit()
	if err != nil {
		return prov, err
	}
	errata, err := m.repo.checkErrata()
	if err != nil {
		return prov, err
	}
	head, err := m.repo.headSHA()
	if err != nil {
		return prov, err
	}
	prov = provenanceRecord{ToolGitSHA: head, ProtocolSHA: protocolSHA, Errata: errata, TC: tc, TCCommit: hash, T0: t0, RunAt: m.now}
	fmt.Printf("protocol %s at T_c %s (commit %s); errata entries in force: %d; tool %s\n", protocolSHA[:12], tc.Format(time.RFC3339), hash[:12], len(errata.Entries), head[:12])
	return prov, nil
}

// filterRows keeps the rows of the listed windows whose coin is covered in that window.
func filterRows(rows []row, keys []int64, coveredAt map[int64][]string, cov coverage) []row {
	in := map[int64]bool{}
	for _, k := range keys {
		in[k] = true
	}
	symbolOf := map[string]string{"BTC": "KXBTC15M", "ETH": "KXETH15M", "SOL": "KXSOL15M", "XRP": "KXXRP15M", "DOGE": "KXDOGE15M"}
	var out []row
	for _, r := range rows {
		if !in[r.W] {
			continue
		}
		coins := coveredAt[r.W]
		if coins == nil {
			coins = cov.covered(time.Unix(r.W, 0).UTC())
		}
		sym := r.Coin
		if s, ok := symbolOf[r.Coin]; ok {
			sym = s
		}
		for _, c := range coins {
			if c == sym || c == r.Coin {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

func (m *measurer) train(ctx context.Context) error {
	prov, err := m.preflight()
	if err != nil {
		return err
	}
	runs := m.repo.researchPath(runsFile)
	note := func(complete, printed bool, text string) {
		_ = appendRun(runs, runRecord{At: m.now, Command: "train", ToolGitSHA: prov.ToolGitSHA, Complete: complete, FiguresPrinted: printed, Note: text})
	}

	trials, err := m.r.trials(ctx)
	if err != nil {
		note(false, false, "trials count failed: "+err.Error())
		return err
	}
	fmt.Printf("trials registered: %d\n", trials)
	cov, err := m.r.coverage(ctx, t0)
	if err != nil {
		note(false, false, "coverage failed: "+err.Error())
		return err
	}
	for _, s := range symbols {
		if sc, ok := cov[s]; ok {
			fmt.Printf("coverage %s from %s\n", s, sc.Format(time.RFC3339))
		} else {
			fmt.Printf("coverage %s: no forecast, never covered\n", s)
		}
	}
	markets, err := m.r.markets(ctx, t0)
	if err != nil {
		note(false, false, "markets failed: "+err.Error())
		return err
	}
	wins := judge(markets, cov, t0, m.now)

	// The frozen list, if there is one: reruns use it and never re-derive it.
	var frozen frozenParams
	hasFrozen, err := readJSON(m.repo.researchPath(frozenFile), &frozen)
	if err != nil {
		return err
	}
	var sp split
	var late []int64
	if hasFrozen {
		if frozen.ProtocolSHA != protocolSHA {
			return fmt.Errorf("refusing: %s was frozen under protocol %s, not this text", frozenFile, frozen.ProtocolSHA[:12])
		}
		sp = split{Train: frozen.TrainWindows, Ineligible: map[int64]string{}, CoveredAtW: map[int64][]string{}, Complete: true,
			FirstClose: frozen.TrainFirstClose, LastClose: frozen.TrainLastClose}
		for k, reason := range frozen.Ineligible {
			t, err := time.Parse(time.RFC3339, k)
			if err != nil {
				return fmt.Errorf("%s: bad window key %q", frozenFile, k)
			}
			sp.Ineligible[t.Unix()] = reason
		}
		for _, w := range wins { // a window frozen out that is eligible now is reported, and stays out
			if _, out := sp.Ineligible[w.W]; out && w.Eligible {
				late = append(late, w.W)
			}
		}
		for _, k := range sp.Train {
			sp.CoveredAtW[k] = cov.covered(time.Unix(k, 0).UTC())
		}
		first := sp.FirstClose.Add(-windowSeconds * time.Second)
		if first.Before(t0) {
			first = t0
		}
		sp.FirstUnix, sp.LastUnix = float64(first.UnixNano())/1e9, float64(sp.LastClose.UnixNano())/1e9
	} else {
		sp = walkTrain(wins, cov, t0)
	}
	eligibleSoFar := 0
	for _, w := range wins {
		if w.Eligible {
			eligibleSoFar++
		}
	}
	fmt.Printf("windows judged: %d complete so far, %d eligible; TRAIN holds %d of %d\n", len(wins), eligibleSoFar, len(sp.Train), trainWindows)
	ineligibleKeys := make([]int64, 0, len(sp.Ineligible))
	for k := range sp.Ineligible {
		ineligibleKeys = append(ineligibleKeys, k)
	}
	sort.Slice(ineligibleKeys, func(i, j int) bool { return ineligibleKeys[i] < ineligibleKeys[j] })
	for _, k := range ineligibleKeys {
		fmt.Printf("  ineligible %s: %s\n", keyString(k), sp.Ineligible[k])
	}
	if len(sp.Train) == 0 {
		note(false, false, "no eligible window yet")
		fmt.Println("no eligible window yet; nothing to measure")
		return nil
	}
	if !sp.Complete {
		fmt.Printf("TRAIN is not complete: figures below are on %d windows and FREEZE NOTHING\n", len(sp.Train))
	}

	// The rows, then the self-check before any other figure prints.
	rows, err := m.r.m1Rows(ctx, sp.FirstUnix, sp.LastUnix)
	if err != nil {
		note(sp.Complete, false, "M1 query failed: "+err.Error())
		return err
	}
	rows = filterRows(rows, sp.Train, sp.CoveredAtW, cov)
	var a []row
	for _, r := range rows {
		if r.InA() {
			a = append(a, r)
		}
	}
	l, ok := lambdaHat(a)
	if !ok {
		note(sp.Complete, false, "no row in A")
		fmt.Printf("%d scored rows, none in A: L_hat cannot be computed\n", len(rows))
		return nil
	}
	lhs, rhs, residual, tol, pass := selfCheck(a, l)
	if !pass {
		note(sp.Complete, true, fmt.Sprintf("self-check FAILED: lhs %.17g rhs %.17g residual %.3g tol %.3g", lhs, rhs, residual, tol))
		fmt.Printf("self-check FAILED\n  lhs Brier(model) - Brier(mid) = %.17g\n  rhs V (1 - 2 L_hat)          = %.17g\n  residual %.3g, tolerance %.3g\n", lhs, rhs, residual, tol)
		return errors.New("the self-check failed; no other figure is printed")
	}

	// 5.3 first, on six-hour blocks: the block decision. Then everything under the decided block.
	ac := autocorrelations(a)
	doubled := ac.Doubled
	m1 := measureM1(rows, doubled)
	next, err := m.r.nextQuotes(ctx, rows)
	if err != nil {
		note(sp.Complete, true, "next snapshots failed: "+err.Error())
		return err
	}
	lookup := func(r row) nextQuote { return next[nextKey{r.MarketID, r.AtUnix}] }
	m2 := staleCost(rows, lookup, false, doubled)
	m2s := staleCost(rows, lookup, true, doubled)
	m3 := outcomeCorrelation(rows, doubled)
	power := powerAtTrain(rows, m1.Lambda)

	res := trainResult{Provenance: prov, Trials: trials, Coverage: map[string]time.Time(cov), Complete: sp.Complete, WindowsFound: eligibleSoFar,
		M1: m1, Autocorrelation: ac, M2: m2, M2s: m2s, M3: m3, Power: power, Notes: []string{}}
	res.Split.FirstClose, res.Split.LastClose, res.Split.Windows = sp.FirstClose, sp.LastClose, sp.Train
	res.Split.Ineligible = map[string]string{}
	for k, v := range sp.Ineligible {
		res.Split.Ineligible[keyString(k)] = v
	}
	res.Split.LateEligible = late
	if late == nil {
		res.Split.LateEligible = []int64{}
	}
	if sp.Complete {
		res.Split.EmbargoEnd = time.Unix(embargoEnd(sp.Train[len(sp.Train)-1], doubled), 0).UTC()
	}
	printTrain(res)

	if sp.Complete {
		blockSeconds, embargo := int64(21600), embargoWindows
		if doubled {
			blockSeconds, embargo = 43200, 2*embargoWindows
		}
		fp := frozenParams{Lambda: m1.Lambda, StaleCost: m2.Frozen, StaleCostSell: m2s.Frozen, BlockSeconds: blockSeconds, EmbargoWin: embargo,
			TrainFirstClose: sp.FirstClose, TrainLastClose: sp.LastClose, EmbargoEnd: res.Split.EmbargoEnd, TrainWindows: sp.Train,
			Ineligible: res.Split.Ineligible, TC: prov.TC, TCCommit: prov.TCCommit, ProtocolSHA: protocolSHA, ErrataSHA: prov.Errata.SHA,
			FrozenAt: m.now, ToolGitSHA: prov.ToolGitSHA}
		switch {
		case !hasFrozen:
			if err := writeJSON(m.repo.researchPath(frozenFile), fp); err != nil {
				return err
			}
			res.Frozen = &fp
			fmt.Printf("FROZEN: lambda %.2f, stale_cost %.4f, stale_cost_sell %.4f, block %d s, embargo %d windows -> %s\n",
				fp.Lambda, fp.StaleCost, fp.StaleCostSell, fp.BlockSeconds, fp.EmbargoWin, m.repo.researchPath(frozenFile))
			if fp.Lambda == 0 {
				fmt.Println("R1 FAILS: lambda is 0. Nothing is registered and TEST is not opened (section 8).")
			} else {
				fmt.Println("R1 passes: lambda > 0. measure3 test WILL be run once TEST is complete (section 8, no way out).")
			}
		default:
			same := frozen.Lambda == fp.Lambda && frozen.StaleCost == fp.StaleCost && frozen.StaleCostSell == fp.StaleCostSell && frozen.BlockSeconds == fp.BlockSeconds
			res.Reproduced = &same
			res.Frozen = &frozen
			if !same {
				note(true, true, "rerun did NOT reproduce the frozen digits")
				_ = writeJSON(m.repo.researchPath(trainResultFile), res)
				return fmt.Errorf("the rerun does not reproduce %s: lambda %.2f vs %.2f, stale_cost %.4f vs %.4f, stale_cost_sell %.4f vs %.4f",
					frozenFile, fp.Lambda, frozen.Lambda, fp.StaleCost, frozen.StaleCost, fp.StaleCostSell, frozen.StaleCostSell)
			}
			fmt.Println("rerun reproduces the frozen digits")
		}
	}
	// A complete run is the result; an early run is progress, kept apart so that the result
	// file only ever holds figures on the frozen list.
	out := trainResultFile
	if !sp.Complete {
		out = trainProgressFile
	}
	if err := writeJSON(m.repo.researchPath(out), res); err != nil {
		return err
	}
	fmt.Printf("written: %s\n", m.repo.researchPath(out))
	note(sp.Complete, true, fmt.Sprintf("L_hat %.4f lambda %.2f on %d windows", m1.LHat.Value, m1.Lambda, len(sp.Train)))
	return nil
}

func printTrain(res trainResult) {
	m1 := res.M1
	fmt.Printf("M1: %d scored rows, %d in A, %d windows (%d without an A row)\n", m1.Rows, m1.RowsInA, m1.Windows, m1.WindowsWithoutA)
	fmt.Printf("    self-check lhs %.12g rhs %.12g residual %.3g (tolerance %.3g)\n", m1.SelfCheck.LHS, m1.SelfCheck.RHS, m1.SelfCheck.Residual, m1.SelfCheck.Tolerance)
	fmt.Printf("    L_hat %.4f, SE %.4f, B %d, t_{0.95,B-1} %.3f -> lambda %.2f\n", m1.LHat.Value, m1.LHat.SE, m1.LHat.B, m1.TQuantile, m1.Lambda)
	for _, k := range sortedKeys(m1.ByCoin) {
		e := m1.ByCoin[k]
		fmt.Printf("    by coin %s: L_hat %.4f SE %.4f n %d\n", k, e.Value, e.SE, e.N)
	}
	fmt.Printf("5.3: %s\n", res.Autocorrelation.Why)
	lag := func(name string, l lagReport) {
		if !l.OK {
			fmt.Printf("    %s lag %d: no pair yet\n", name, l.Lag)
			return
		}
		fmt.Printf("    %s lag %d: r %.3f SE %.3f B %d\n", name, l.Lag, l.R, l.SE, l.B)
	}
	for _, l := range res.Autocorrelation.Numerator {
		if l.Lag == 24 || l.Lag == 1 {
			lag("numerator", l)
		}
	}
	for _, l := range res.Autocorrelation.Brier {
		if l.Lag == 24 || l.Lag == 1 {
			lag("brier", l)
		}
	}
	fmt.Printf("M2 stale_cost: %d observations (no side %d, crossed %d, no next %d, vanished %d); mean %.5f SE %.5f; without vanished %.5f -> frozen %.4f\n",
		res.M2.Observations, res.M2.NoSide, res.M2.BothSides, res.M2.NoNext, res.M2.Vanished, res.M2.Mean.Value, res.M2.Mean.SE, res.M2.MeanNoVanished.Value, res.M2.Frozen)
	fmt.Printf("M2s stale_cost_sell: %d observations (no side %d, crossed %d, no next %d, vanished %d); mean %.5f SE %.5f; without vanished %.5f -> frozen %.4f\n",
		res.M2s.Observations, res.M2s.NoSide, res.M2s.BothSides, res.M2s.NoNext, res.M2s.Vanished, res.M2s.Mean.Value, res.M2s.Mean.SE, res.M2s.MeanNoVanished.Value, res.M2s.Frozen)
	for _, k := range sortedKeys(res.M3.Pairs) {
		e := res.M3.Pairs[k]
		fmt.Printf("M3 %s: r %.3f SE %.3f over %d windows\n", k, e.Value, e.SE, e.N)
	}
	fmt.Printf("R2 power at 192 windows (reported only): %.2f (mean b_w %.5f, sd %.5f)\n", res.Power.Power192, res.Power.MeanBW, res.Power.SDBW)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *measurer) test(ctx context.Context, retryReason string) error {
	prov, err := m.preflight()
	if err != nil {
		return err
	}
	runs := m.repo.researchPath(runsFile)
	note := func(complete, printed bool, text string) {
		_ = appendRun(runs, runRecord{At: m.now, Command: "test", ToolGitSHA: prov.ToolGitSHA, Complete: complete, FiguresPrinted: printed, Note: text})
	}
	var frozen frozenParams
	hasFrozen, err := readJSON(m.repo.researchPath(frozenFile), &frozen)
	if err != nil {
		return err
	}
	if !hasFrozen {
		return fmt.Errorf("refusing: %s does not exist; run train to completion first", frozenFile)
	}
	if frozen.ProtocolSHA != protocolSHA {
		return fmt.Errorf("refusing: %s was frozen under protocol %s, not this text", frozenFile, frozen.ProtocolSHA[:12])
	}
	if frozen.Lambda <= 0 {
		return errors.New("refusing: lambda is 0; R1 failed and TEST is not opened (section 8)")
	}
	if exists, _ := readJSON(m.repo.researchPath(testResultFile), &struct{}{}); exists {
		return fmt.Errorf("refusing: %s exists; the one look at TEST has been taken", testResultFile)
	}
	doubled := frozen.BlockSeconds == 43200

	cov, err := m.r.coverage(ctx, t0)
	if err != nil {
		return err
	}
	markets, err := m.r.markets(ctx, t0)
	if err != nil {
		return err
	}
	wins := judge(markets, cov, t0, m.now)
	keys, ineligible, coveredAt, n := walkTest(wins, cov, frozen.EmbargoEnd.Unix(), frozen.TC)
	if n < testWindows {
		note(false, false, fmt.Sprintf("TEST not complete: %d of %d eligible windows", n, testWindows))
		fmt.Printf("TEST is not complete: %d of %d eligible windows closing after the embargo (%s) and T_c (%s). Nothing written.\n",
			n, testWindows, frozen.EmbargoEnd.Format(time.RFC3339), frozen.TC.Format(time.RFC3339))
		return nil
	}

	// Written down BEFORE the look.
	attemptPath := m.repo.researchPath(testAttemptFile)
	var attempt testAttempt
	hasAttempt, err := readJSON(attemptPath, &attempt)
	if err != nil {
		return err
	}
	if hasAttempt && retryReason == "" {
		return fmt.Errorf("refusing: %s exists; a retry needs -retry-reason", testAttemptFile)
	}
	if !hasAttempt {
		attempt = testAttempt{ToolGitSHA: prov.ToolGitSHA, ProtocolSHA: protocolSHA, ErrataSHA: prov.Errata.SHA, Windows: keys, Ineligible: map[string]string{}}
		for k, v := range ineligible {
			attempt.Ineligible[keyString(k)] = v
		}
	} else {
		keys = attempt.Windows // frozen at the first attempt; never re-derived
	}
	attempt.Attempts = append(attempt.Attempts, struct {
		At     time.Time `json:"at"`
		Reason string    `json:"reason,omitempty"`
	}{At: m.now, Reason: retryReason})
	if err := writeJSON(attemptPath, attempt); err != nil {
		return err
	}
	fmt.Printf("TEST: %d windows, %s to %s; attempt %d written to %s\n", len(keys), keyString(keys[0]), keyString(keys[len(keys)-1]), len(attempt.Attempts), attemptPath)

	first := time.Unix(keys[0], 0).Add(-windowSeconds * time.Second)
	rows, err := m.r.m1Rows(ctx, float64(first.UnixNano())/1e9, float64(keys[len(keys)-1]))
	if err != nil {
		note(true, false, "M1 query on TEST failed: "+err.Error())
		return err
	}
	for _, k := range keys {
		if coveredAt[k] == nil {
			coveredAt[k] = cov.covered(time.Unix(k, 0).UTC())
		}
	}
	rows = filterRows(rows, keys, coveredAt, cov)
	r2 := blendTest(rows, frozen.Lambda, doubled)
	byCoin := map[string]r2Report{}
	perCoin := map[string][]row{}
	for _, r := range rows {
		perCoin[r.Coin] = append(perCoin[r.Coin], r)
	}
	for c, rs := range perCoin {
		byCoin[c] = blendTest(rs, frozen.Lambda, doubled)
	}
	res := testResult{Provenance: prov, Attempt: attempt, Frozen: frozen, R2: r2, ByCoin: byCoin,
		Notes: []string{"R2 is a gate, not a strategy: a pass is permission to register two hypotheses for live judgement at the corrected threshold, not evidence that either makes money (section 7)."}}
	if err := writeJSON(m.repo.researchPath(testResultFile), res); err != nil {
		return err
	}
	fmt.Printf("R2: %d windows with an A row (%d without); statistic %.6f, SE %.6f, B %d, t %.3f, threshold %.3f\n  %s\n",
		r2.WindowsWithA, r2.WindowsWithoutA, r2.Statistic.Value, r2.Statistic.SE, r2.Statistic.B, r2.T, r2.Threshold, r2.Verdict)
	note(true, true, r2.Verdict)
	return nil
}
