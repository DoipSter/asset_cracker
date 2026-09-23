package analysis

import (
	"fmt"
	"math"
	"sort"
)

// Coins is the order the contract lists the coins in. A coin with nothing scored still appears.
var Coins = []string{"BTC", "ETH", "SOL", "XRP", "DOGE"}

// Bucket is one strategy bucket, live or frozen. Every life of a strategy that ran out is its own
// bucket; they share a strategy version, which is what the leaderboard groups by.
type Bucket struct {
	ID, VersionID int64
	Strategy      string // "Scalper"; a twin carries its original's name and World "anti"
	Engine        string // "v" and the strategy version's number: "v1", "v2", or "v3" for the third engine
	World         string // "real" or "anti"
	Frozen        bool
	Replaced      bool // ran out and was staked again: the next life is another bucket
	// AllocatedCents is the sustainment allocation taken from this bucket to date.
	AllocatedCents int64
}

// Coverage says how much of the settled history the figures rest on. The per-market aggregates
// are filled a few windows per refresh, so after a restart the document is PARTIAL for a while
// and must say so rather than pass a part off as the whole. While Complete is false every verdict
// is "unresolved", every leaderboard row is flagged, and every "what" opens by saying so.
type Coverage struct {
	MarketsSettled    int `json:"markets_settled"`
	MarketsAggregated int `json:"markets_aggregated"`
	// Read, and held back: what their buckets held at the close does not match their settlement
	// rows (see Reconcile), or a fill has no recorded money. They are read again every refresh. A
	// number that stays above 0 is a round whose settlement was never written: a halted runner,
	// or a stop between the result and the payouts.
	MarketsUnreconciled int  `json:"markets_unreconciled"`
	WindowsIncomplete   int  `json:"windows_incomplete"` // left out whole: a market in them is unsettled, unreconciled or not yet read
	Complete            bool `json:"complete"`           // markets_aggregated has reached markets_settled
}

// Conventions is the verdict rule, stated so nobody mistakes it for a finding.
type Conventions struct {
	Note       string  `json:"note"`
	MinWindows int     `json:"min_windows"`
	MinAbsT    float64 `json:"min_abs_t"` // one hypothesis stated in advance: the scorecard's overall row
	// The correction for looking at many rows at once. Trials is MEASURED (the rows of
	// strategy_version, which the schema names as the number of trials); the thresholds follow
	// from it by CorrectedT. 0 trials means the count could not be read: leaderboard_min_abs_t is
	// then 0 and no leaderboard verdict is given.
	FamilyAlpha         float64 `json:"family_alpha"`
	Trials              int     `json:"trials"`
	LeaderboardMinAbsT  float64 `json:"leaderboard_min_abs_t"`
	ScorecardCuts       int     `json:"scorecard_cuts"`
	ScorecardCutMinAbsT float64 `json:"scorecard_cut_min_abs_t"`
	DominantWindow      float64 `json:"dominant_window_share"`
}

// ScoreRow is one line of the scorecard. Brier figures are means over WINDOWS of each window's
// own Brier score, so that diff = brier_model - brier_market is the same mean the SE is of. The
// log loss figures are made the same way from the same rows and shown beside them as a second
// reading of calibration; the verdict is decided on the Brier difference alone, the one
// hypothesis stated in advance, and no verdict is read from the log loss.
type ScoreRow struct {
	Band          string  `json:"band,omitempty"`
	Coin          string  `json:"coin,omitempty"`
	NRows         int64   `json:"n_rows"`
	NWindows      int     `json:"n_windows"`
	BrierModel    float64 `json:"brier_model"`
	BrierMarket   float64 `json:"brier_market"`
	Diff          float64 `json:"diff"`
	SE            float64 `json:"se"`
	T             float64 `json:"t"`
	Verdict       string  `json:"verdict"`
	LogLossModel  float64 `json:"logloss_model"`
	LogLossMarket float64 `json:"logloss_market"`
	LogLossDiff   float64 `json:"logloss_diff"` // model minus market: positive means the model is WORSE
	LogLossSE     float64 `json:"logloss_se"`
}

type Scorecard struct {
	What    string     `json:"what"`
	Overall ScoreRow   `json:"overall"`
	ByBand  []ScoreRow `json:"by_band"`
	ByCoin  []ScoreRow `json:"by_coin"`
	// Series is the overall row over time: for each settled window, [close (unix seconds), the
	// mean model Brier over every window up to it, the market's likewise]. The last point's two
	// means are the overall row's brier_model and brier_market. Thinned to at most SeriesPoints,
	// each bin giving its last window, so every point is a figure that stood at that moment.
	Series [][3]float64 `json:"series"`
}

// SeriesPoints bounds every series the document carries. [CONVENTION: a page's width in points.]
const SeriesPoints = 300

type FillRow struct {
	Strategy           string  `json:"strategy"`
	Engine             string  `json:"engine"`
	World              string  `json:"world"`
	Sells              int     `json:"sells"`        // every early sale in the windows read, priced or not, plus unsettled_sells
	SellsPriced        int     `json:"sells_priced"` // settled, with depth recorded: what the figures below cover
	ContractsSold      int     `json:"contracts_sold"`
	ContractsBeyondBid int     `json:"contracts_beyond_bid"`
	ShareBeyond        float64 `json:"share_beyond"`
	SalePnLCents       int64   `json:"sale_pnl_cents"`
	SalePnLCappedCents int64   `json:"sale_pnl_capped_cents"`
	UnsettledSells     int     `json:"unsettled_sells"`
	SellsWithoutDepth  int     `json:"sells_without_depth"`
}

type Fills struct {
	What       string    `json:"what"`
	ByStrategy []FillRow `json:"by_strategy"`
}

type LeaderRow struct {
	VersionID int64  `json:"strategy_version_id"`
	Strategy  string `json:"strategy"`
	Engine    string `json:"engine"`
	World     string `json:"world"`
	Lives     int    `json:"lives"`
	BookCents int64  `json:"book_cents"` // cash plus open bets AT COST. Not equity: api/buckets marks the same bets at the bid
	// AllocatedCents is the sustainment allocation taken from the version's buckets to date,
	// every life. lifetime_pnl_cents is what the strategy earned; less this is what its buckets
	// kept, which is what the bar on the buckets page moves by.
	AllocatedCents     int64    `json:"allocated_cents"`
	LifetimePnLCents   int64    `json:"lifetime_pnl_cents"`
	Bets               int      `json:"bets"`
	Windows            int      `json:"windows"`
	MeanWindowPnLCents int64    `json:"mean_window_pnl_cents"`
	SECents            int64    `json:"se_cents"`
	T                  float64  `json:"t"`
	TopWindowShare     float64  `json:"top_window_share"`
	Verdict            string   `json:"verdict"`
	Flags              []string `json:"flags"`
	// The gate's figures. Orders and decisions count the same settled windows as bets. The
	// return is realised P&L over money staked (fees inside both), with a standard error from
	// resampling windows; return_lower is the return less z (conventions: gate.z) standard errors,
	// and return_t the return over its standard error, both as published. windows_needed is the
	// sample floor from the power calculation, 0 if it could not be computed. first_close and
	// last_close (unix seconds) bound the windows counted.
	Orders          int     `json:"orders"`
	Decisions       int64   `json:"decisions"`
	StakedCents     int64   `json:"staked_cents"`
	ReturnPerDollar float64 `json:"return_per_dollar"`
	ReturnSE        float64 `json:"return_se"`
	ReturnLower     float64 `json:"return_lower"`
	ReturnT         float64 `json:"return_t"`
	WindowsNeeded   int     `json:"windows_needed"`
	// Series is the version's realised P&L added up window by window: [close (unix seconds),
	// cumulative cents], thinned to at most SeriesPoints, each bin its last window. The last
	// point's cents are lifetime_pnl_cents.
	Series     [][2]int64 `json:"series"`
	FirstClose int64      `json:"first_close"`
	LastClose  int64      `json:"last_close"`
	Drawdown   Drawdown   `json:"drawdown"`
	Gate       Gate       `json:"gate"`
}

type Leaderboard struct {
	What string      `json:"what"`
	Rows []LeaderRow `json:"rows"`
}

// Document is the body of GET /api/analysis.
type Document struct {
	Simulated       bool        `json:"simulated"`
	ComputedAt      float64     `json:"computed_at"`
	Stale           bool        `json:"stale"` // true when the last refresh failed and this is the one before it
	StaleReason     string      `json:"stale_reason,omitempty"`
	WindowsRecorded int         `json:"windows_recorded"`
	Coverage        Coverage    `json:"coverage"`
	Conventions     Conventions `json:"conventions"`
	Gate            GateConfig  `json:"gate"`
	Scorecard       Scorecard   `json:"scorecard"`
	Fills           Fills       `json:"fills"`
	Leaderboard     Leaderboard `json:"leaderboard"`
}

// Inputs is everything Build needs, already read.
type Inputs struct {
	ComputedAt     float64
	Facts          []MarketFacts  // every settled market aggregated so far
	Incomplete     map[int64]bool // windows (closes, unix s) to leave out: see Coverage.WindowsIncomplete
	Buckets        []Bucket
	BookCents      map[int64]int64 // live buckets: ledger cash plus open bets at cost
	UnsettledSells map[int64]int   // by bucket: early sales in rounds with no result yet
	MarketsSettled int
	Unreconciled   int // settled markets held back: see Coverage.MarketsUnreconciled
	Trials         int // select count(*) from strategy_version; 0 if it could not be read
	// Gate is the operator's settings. The zero value is not valid and Build then uses
	// DefaultGate, so a caller that never heard of the gate gets the stated defaults.
	Gate GateSettings
}

const (
	scorecardWhat = "Brier score of the model's probability against the market's own mid price, on settled rounds. Lower is better. " +
		"The unit is one 15-minute window (all coins together), because bets inside a window are not independent: each window is scored on its own, " +
		"and brier_model, brier_market and diff are means over windows. " +
		"The model figure is decision.model_prob of the second engine (v2): the RAW model probability of YES, before any strategy blends it with the market " +
		"(the blended figure is not journaled). The market figure is decision.market_prob: the middle of the Yes bid and ask, or the ask alone when there is no bid. " +
		"One row is one second of one round in which at least one v2 strategy journaled a decision; idle strategies journal only on a change and every 15 s, " +
		"so seconds are not evenly covered. " +
		"logloss_model and logloss_market are the same rows' log loss (-ln of the probability given to what happened, probabilities held at least 1e-6 from 0 and 1), " +
		"made the same way: per window, then a mean over windows, with logloss_diff = model - market and its SE. They are a second reading of calibration; " +
		"the verdict is decided on the Brier difference alone, and none is read from the log loss."
	fillsWhat = "The simulator sells any size at the bid. This re-prices every early sale as if only the size actually displayed at that second had filled, the rest riding to settlement " +
		"(1.00 a contract if its side won, else nothing). Sales by one bucket in the same second share one displayed size. " +
		"The displayed size is the BEST BID LEVEL ALONE. The simulator books a sale one cent under the bid, and size bid within that cent is not counted, " +
		"so contracts_beyond_bid is an upper bound on the size that was not there. " +
		"contracts_sold, contracts_beyond_bid, share_beyond and both P&L figures cover only the sells_priced sales: settled rounds where depth was recorded. " +
		"Proceeds and cost are the recorded ones pro rata, fees inside; the fee's round-up to a cent is not recomputed, so a sale can be off by under a cent."
	leaderboardWhat = "Per strategy version. Windows are the independent sample, not bets. Lifetime includes every earlier life of a strategy that ran out. " +
		"lifetime_pnl_cents is realised trading P&L on settled windows (payouts and sale proceeds less what the bets cost, fees inside), before any sustainment allocation; " +
		"allocated_cents is that allocation, taken from the version's buckets to date, so lifetime_pnl_cents less allocated_cents is what the buckets kept; " +
		"bets and windows count the same settled windows. It is not book or equity less the seed. " +
		"book_cents is the live buckets' ledger cash plus their open bets AT COST; 0 if no bucket is live. It is not equity: api/buckets equity_cents marks the same bets at the bid. " +
		"A first-engine version spans its BTC and ETH buckets, which api/buckets lists apart. " +
		"The verdict threshold is conventions.leaderboard_min_abs_t, raised for the conventions.trials versions compared here at once. " +
		"staked_cents is what the bets cost (fees inside), and return_per_dollar is lifetime_pnl_cents over it: the after-fee return on every dollar put at risk. " +
		"return_se is its standard error from resampling the windows with replacement (gate.bootstrap_resamples draws, seeded, so the figure reproduces); " +
		"return_lower is the return less gate.z standard errors. drawdown walks the realised P&L window by window from the first life's start. " +
		"gate is the promotion rule of `gate` applied to this row, every check decided on the figures as published; " +
		"it is not evaluated while coverage.complete is false, while trials is unknown, or for a row with no window. " +
		"The verdict and the gate's edge check are two readings of the same windows, mean window P&L with its plain standard error and return per dollar with its bootstrap one, " +
		"and can differ; the gate is the rule capital follows."
	conventionsNote = "The verdict rule is a convention chosen in advance, not a measurement: unresolved unless n_windows >= min_windows AND |t| reaches a threshold, " +
		"with t as published here, to two places. For ONE hypothesis stated in advance the threshold is min_abs_t: that is the scorecard's overall row (does the model beat the market's own price?). " +
		"The leaderboard compares `trials` strategy versions at once (trials is measured: the row count of strategy_version). With that many rows and no edge anywhere, " +
		"some row would pass min_abs_t by chance far more often than one time in twenty, so its threshold is leaderboard_min_abs_t: the two-sided Bonferroni cut " +
		"sqrt(2) x erfinv(1 - family_alpha / trials), never below min_abs_t, which holds the chance of ANY false verdict among the rows to family_alpha. " +
		"The scorecard's by_band and by_coin rows are scorecard_cuts exploratory cuts of the same rows, looked at together to see where the model is better or worse; " +
		"they use the same correction with their own count: scorecard_cut_min_abs_t. Bonferroni is a convention too: it assumes nothing about how rows depend on each other " +
		"and is conservative when they move together, as a twin and its original do. It corrects for the rows shown side by side, and for nothing else " +
		"(not for strategies tried and never registered). While coverage.complete is false every verdict is unresolved."
)

// partialNote is the plain sentence every part of a partial document carries, "" when every
// settled market is in the figures.
func partialNote(c Coverage) string {
	if c.Complete {
		return ""
	}
	s := fmt.Sprintf("partial: only %d of %d settled markets read", c.MarketsAggregated, c.MarketsSettled)
	if c.MarketsUnreconciled > 0 {
		s += fmt.Sprintf(" (%d held back: what was held at the close does not match their settlement rows; they are read again every minute)", c.MarketsUnreconciled)
	}
	return s
}

// Build combines the cached per-market facts into the document. It never returns a nil slice
// (the page iterates them) and never a NaN or Inf.
func Build(in Inputs) Document {
	var facts []MarketFacts
	windows, left := map[int64]bool{}, map[int64]bool{}
	for _, f := range in.Facts {
		if in.Incomplete[f.Closes] {
			left[f.Closes] = true
			continue
		}
		windows[f.Closes] = true
		facts = append(facts, f)
	}
	for w := range in.Incomplete {
		left[w] = true
	}
	cov := Coverage{MarketsSettled: in.MarketsSettled, MarketsAggregated: len(in.Facts), MarketsUnreconciled: max(in.Unreconciled, 0),
		WindowsIncomplete: len(left), Complete: len(in.Facts) >= in.MarketsSettled}
	// The thresholds are applied exactly as published (four places), against t as published (two).
	rule := thresholds{overall: MinAbsT, cut: round(CorrectedT(len(Bands)+len(Coins)), 4), leaderboard: round(CorrectedT(in.Trials), 4)}
	settings := in.Gate
	if !settings.Valid() {
		settings = DefaultGate
	}
	gate := GateConfig{Note: gateNote, MinWindows: MinWindows, FamilyAlpha: FamilyAlpha, Trials: max(in.Trials, 0), Z: rule.leaderboard,
		MinEdgePerDollar: settings.MinEdgePerDollar, Power: settings.Power, MaxDrawdownCents: settings.MaxDrawdownCents,
		BootstrapResamples: BootstrapResamples, BootstrapSeed: BootstrapSeed}
	doc := Document{Simulated: true, ComputedAt: in.ComputedAt, WindowsRecorded: len(windows), Coverage: cov,
		Conventions: Conventions{Note: conventionsNote, MinWindows: MinWindows, MinAbsT: MinAbsT, FamilyAlpha: FamilyAlpha, Trials: max(in.Trials, 0),
			LeaderboardMinAbsT: rule.leaderboard, ScorecardCuts: len(Bands) + len(Coins), ScorecardCutMinAbsT: rule.cut, DominantWindow: DominantWindow},
		Gate: gate}
	partial := partialNote(cov)
	if partial != "" {
		// Part of the history, read newest first, is not the history. Its t is a fair figure for
		// the windows it covers, but the labels say "lifetime" and "overall", and they would flip
		// as older windows load. So no verdict is given until everything is read. The figures
		// themselves are still shown: a zero would be a number that is not the real one either.
		rule = thresholds{}
	}
	doc.Scorecard = buildScorecard(facts, rule)
	doc.Fills = buildFills(facts, in.Buckets, in.UnsettledSells)
	// The gate is decided only when a verdict could be: everything read, and the trials known.
	// Its z is the leaderboard threshold, which the partial rule has just set to 0.
	decide := ""
	switch {
	case partial != "":
		decide = partial + "; no gate decision until all are read"
	case in.Trials <= 0:
		decide = "the number of strategy versions could not be read, so there is no threshold to decide against"
	}
	doc.Leaderboard = buildLeaderboard(facts, in.Buckets, in.BookCents, rule, in.Trials > 0, partial, gate, doc.Scorecard.Overall, decide)
	if partial != "" {
		lead := "P" + partial[1:] + ". Every figure here covers those markets only, and every verdict is unresolved until all are read. "
		doc.Scorecard.What, doc.Fills.What, doc.Leaderboard.What = lead+doc.Scorecard.What, lead+doc.Fills.What, lead+doc.Leaderboard.What
	}
	return doc
}

// thresholds is the |t| each kind of row must reach for a verdict; 0 means none can be given.
//
// Which rows are corrected, and for what. The scorecard's overall row is ONE hypothesis, stated
// before any data: does the model beat the market's own price? It keeps MinAbsT. The five bands
// and five coins are ten exploratory cuts of those same rows. Nobody said in advance which of
// them would differ, they are read together, and whichever stands out will be read as a finding,
// so they get the family correction with their own count (len(Bands) + len(Coins), taken from
// the lists themselves). The leaderboard's family is the registry of strategy versions, which
// the schema names as the number of trials every significance figure must be corrected for
// (0001_init.sql). The two families answer different questions and are corrected separately.
type thresholds struct{ overall, cut, leaderboard float64 }

// scoreRow scores the rows that keep() admits: a Brier pair per window, then the window stat of
// their difference.
func scoreRow(facts []MarketFacts, minAbsT float64, keep func(coin string, band int) bool) ScoreRow {
	type sums struct{ n, model, market, modelLog, marketLog float64 }
	per := map[int64]*sums{}
	var rows int64
	for _, f := range facts {
		for band, b := range f.Score {
			if b.N <= 0 || !keep(f.Coin, band) {
				continue
			}
			s := per[f.Closes]
			if s == nil {
				s = &sums{}
				per[f.Closes] = s
			}
			s.n, s.model, s.market = s.n+float64(b.N), s.model+b.Model, s.market+b.Market
			s.modelLog, s.marketLog = s.modelLog+finite(b.ModelLog), s.marketLog+finite(b.MarketLog)
			rows += b.N
		}
	}
	var model, market, diff, modelLog, marketLog, diffLog []float64
	for _, w := range sortedKeys(per) {
		s := per[w]
		model, market = append(model, s.model/s.n), append(market, s.market/s.n)
		diff = append(diff, (s.model-s.market)/s.n)
		modelLog, marketLog = append(modelLog, s.modelLog/s.n), append(marketLog, s.marketLog/s.n)
		diffLog = append(diffLog, (s.modelLog-s.marketLog)/s.n)
	}
	st := WindowStat(diff)
	t := round(st.T, 2) // the verdict is decided on the figure the page shows
	lg := WindowStat(diffLog)
	return ScoreRow{NRows: rows, NWindows: st.N, BrierModel: round(WindowStat(model).Mean, 4), BrierMarket: round(WindowStat(market).Mean, 4),
		Diff: round(st.Mean, 4), SE: round(st.SE, 4), T: t, Verdict: Verdict(Stat{N: st.N, T: t}, minAbsT, "model worse", "model better"),
		LogLossModel: round(WindowStat(modelLog).Mean, 4), LogLossMarket: round(WindowStat(marketLog).Mean, 4), LogLossDiff: round(lg.Mean, 4), LogLossSE: round(lg.SE, 4)}
}

// scoreSeries is the overall row window by window: the running mean of each window's Brier, model
// and market, in the order the windows closed. Every window counts once whatever its size, as in
// scoreRow, so the last point is the overall row.
func scoreSeries(facts []MarketFacts) [][3]float64 {
	type sums struct{ n, model, market float64 }
	per := map[int64]*sums{}
	for _, f := range facts {
		for _, b := range f.Score {
			if b.N <= 0 {
				continue
			}
			s := per[f.Closes]
			if s == nil {
				s = &sums{}
				per[f.Closes] = s
			}
			s.n, s.model, s.market = s.n+float64(b.N), s.model+b.Model, s.market+b.Market
		}
	}
	out := make([][3]float64, 0, len(per))
	var model, market float64
	for i, w := range sortedKeys(per) {
		s := per[w]
		model, market = model+s.model/s.n, market+s.market/s.n
		n := float64(i + 1)
		out = append(out, [3]float64{float64(w), round(model/n, 4), round(market/n, 4)})
	}
	return thin(out, SeriesPoints)
}

// thin keeps at most max points of a series in order: equal runs of points, each giving its last.
// A series already within the bound is returned as it is.
func thin[T any](pts []T, max int) []T {
	if max < 2 || len(pts) <= max {
		if pts == nil {
			return []T{}
		}
		return pts
	}
	out := make([]T, 0, max)
	for i := 1; i <= max; i++ {
		out = append(out, pts[i*len(pts)/max-1])
	}
	return out
}

func buildScorecard(facts []MarketFacts, rule thresholds) Scorecard {
	sc := Scorecard{What: scorecardWhat, ByBand: []ScoreRow{}, ByCoin: []ScoreRow{}, Series: scoreSeries(facts)}
	sc.Overall = scoreRow(facts, rule.overall, func(string, int) bool { return true })
	for i, b := range Bands {
		r := scoreRow(facts, rule.cut, func(_ string, band int) bool { return band == i })
		r.Band = b.Label
		sc.ByBand = append(sc.ByBand, r)
	}
	for _, c := range Coins {
		r := scoreRow(facts, rule.cut, func(coin string, _ int) bool { return coin == c })
		r.Coin = c
		sc.ByCoin = append(sc.ByCoin, r)
	}
	return sc
}

// scoredEngine is the engine whose journal the scorecard scores (store.AnalysisModelVersions:
// the live engine's version-3 originals). A version of any other engine, or a twin, trades a
// model the scorecard does not see, and the gate's calibration check says so.
const scoredEngine = "v3"

// version is what the fills and leaderboard group by: a strategy version, whatever bucket or
// life the money was in.
type version struct {
	id                      int64
	strategy, engine, world string
}

func versionsOf(buckets []Bucket) (byBucket map[int64]int64, versions map[int64]version) {
	byBucket, versions = map[int64]int64{}, map[int64]version{}
	for _, b := range buckets {
		byBucket[b.ID] = b.VersionID
		versions[b.VersionID] = version{b.VersionID, b.Strategy, b.Engine, b.World}
	}
	return
}

func buildFills(facts []MarketFacts, buckets []Bucket, unsettled map[int64]int) Fills {
	byBucket, versions := versionsOf(buckets)
	rows := map[int64]*FillRow{}
	row := func(bucket int64) *FillRow {
		v, ok := versions[byBucket[bucket]]
		if !ok {
			return nil // a bucket this refresh has not listed yet: it will be there next minute
		}
		if rows[v.id] == nil {
			rows[v.id] = &FillRow{Strategy: v.strategy, Engine: v.engine, World: v.world}
		}
		return rows[v.id]
	}
	for _, f := range facts {
		for _, s := range f.Sales {
			r := row(s.BucketID)
			if r == nil {
				continue
			}
			r.Sells++
			if s.WithoutDepth {
				r.SellsWithoutDepth++
				continue
			}
			r.SellsPriced++
			r.ContractsSold += s.Qty
			r.ContractsBeyondBid += s.Beyond
			r.SalePnLCents += s.PnLCents
			r.SalePnLCappedCents += s.CappedCents
		}
	}
	for bucket, n := range unsettled {
		if n <= 0 { // the unsettled map lists every bucket with a bet open; one that sold nothing must not gain a row
			continue
		}
		if r := row(bucket); r != nil {
			r.Sells += n
			r.UnsettledSells += n
		}
	}
	out := Fills{What: fillsWhat, ByStrategy: []FillRow{}}
	for _, id := range sortedKeys(rows) {
		r := *rows[id]
		if r.ContractsSold > 0 {
			r.ShareBeyond = round(float64(r.ContractsBeyondBid)/float64(r.ContractsSold), 2)
		}
		out.ByStrategy = append(out.ByStrategy, r)
	}
	// the strategies whose booked profit leans hardest on size that was not there come first
	sort.SliceStable(out.ByStrategy, func(i, j int) bool {
		a, b := out.ByStrategy[i], out.ByStrategy[j]
		return a.SalePnLCents-a.SalePnLCappedCents > b.SalePnLCents-b.SalePnLCappedCents
	})
	return out
}

// buildLeaderboard makes one row per strategy version. `decide` is "" when the gate can be
// evaluated and otherwise the reason it cannot; `gate` and `overall` (the scorecard's overall
// row) are what the gate decides against.
func buildLeaderboard(facts []MarketFacts, buckets []Bucket, book map[int64]int64, rule thresholds, trialsKnown bool, partial string,
	gate GateConfig, overall ScoreRow, decide string) Leaderboard {
	byBucket, versions := versionsOf(buckets)
	type window struct{ pnl, staked float64 }
	type tally struct {
		windows      map[int64]*window // closes -> the version's P&L and stake in cents, windows with at least one bet
		bets, orders int
	}
	tallies := map[int64]*tally{}
	for id := range versions {
		tallies[id] = &tally{windows: map[int64]*window{}}
	}
	for _, f := range facts {
		for bucket, r := range f.Rounds {
			t := tallies[byBucket[bucket]]
			if t == nil || r.Bets == 0 {
				continue
			}
			w := t.windows[f.Closes]
			if w == nil {
				w = &window{}
				t.windows[f.Closes] = w
			}
			w.pnl, w.staked = w.pnl+float64(r.PnLCents), w.staked+float64(r.StakedCents)
			t.bets, t.orders = t.bets+r.Bets, t.orders+r.Orders
		}
	}
	out := Leaderboard{What: leaderboardWhat, Rows: []LeaderRow{}}
	for _, id := range sortedKeys(tallies) {
		v, t := versions[id], tallies[id]
		var per, staked []float64
		var total float64
		var first, last int64
		series := make([][2]int64, 0, len(t.windows))
		for _, w := range sortedKeys(t.windows) { // time order: the drawdown is a sequence
			per, staked = append(per, t.windows[w].pnl), append(staked, t.windows[w].staked)
			total += t.windows[w].pnl
			series = append(series, [2]int64{w, int64(math.Round(total))})
			if first == 0 || w < first {
				first = w
			}
			last = max(last, w)
		}
		st := WindowStat(per)
		// t and the share are rounded once, and the verdict and the flags are decided on the rounded
		// figures, so what the row shows and what it says can never disagree.
		tt, share := round(st.T, 2), round(TopWindowShare(per), 2)
		row := LeaderRow{VersionID: id, Strategy: v.strategy, Engine: v.engine, World: v.world, Lives: 1, LifetimePnLCents: int64(total), Bets: t.bets,
			Windows: st.N, MeanWindowPnLCents: int64(round(st.Mean, 0)), SECents: int64(round(st.SE, 0)), T: tt,
			TopWindowShare: share, Verdict: Verdict(Stat{N: st.N, T: tt}, rule.leaderboard, "ahead", "behind"), Flags: []string{},
			Orders: t.orders, FirstClose: first, LastClose: last, Drawdown: DrawdownOf(per), Series: thin(series, SeriesPoints)}
		// The period is the windows with a bet; the journal rows counted are every one this version
		// wrote on the settled markets inside it, whether or not it bet on them.
		for _, f := range facts {
			if f.Closes >= first && f.Closes <= last {
				row.Decisions += f.Decisions[id]
			}
		}
		ret := WindowRatio(per, staked)
		row.StakedCents = int64(math.Round(sum(staked)))
		row.ReturnPerDollar, row.ReturnSE = round(ret.Value, 4), round(ret.SE, 4)
		if ret.SE > 0 {
			row.ReturnT = round(ret.Value/ret.SE, 2)
		}
		if gate.Z > 0 {
			row.ReturnLower = round(ret.Value-gate.Z*ret.SE, 4)
		}
		row.WindowsNeeded = WindowsNeeded(ret.SE*math.Sqrt(float64(ret.N)), gate.MinEdgePerDollar, gate.Z, gate.Power)
		switch {
		case decide != "":
			row.Gate = notEvaluated(decide)
		case st.N == 0:
			row.Gate = notEvaluated("no settled window with a bet")
		default:
			row.Gate = evaluateGate(gate, gateInputs{windows: st.N, needed: row.WindowsNeeded, ret: Ratio{N: ret.N, Value: row.ReturnPerDollar, SE: row.ReturnSE},
				lower: row.ReturnLower, drawdown: row.Drawdown, modelScored: v.engine == scoredEngine && v.world == "real", overall: overall})
		}
		if partial != "" {
			row.Flags = append(row.Flags, partial+"; lifetime_pnl_cents, bets, windows and t cover those only, and no verdict is given until all are read")
		} else if !trialsKnown {
			row.Flags = append(row.Flags, "the number of strategy versions compared could not be read, so no verdict can be given")
		}
		live := false
		for _, b := range buckets {
			if b.VersionID != id {
				continue
			}
			if b.Replaced {
				row.Lives++
			}
			row.AllocatedCents += b.AllocatedCents
			if !b.Frozen {
				live = true
				row.BookCents += book[b.ID]
			}
		}
		if row.Lives > 1 {
			row.Flags = append(row.Flags, fmt.Sprintf("ran out and was staked again: this is life %d, and every life is counted here", row.Lives))
		}
		if !live {
			row.Flags = append(row.Flags, "ran out and was not staked again")
		}
		switch {
		case st.N == 1:
			row.Flags = append(row.Flags, "only one window: nothing can be said yet")
		case share > 1:
			row.Flags = append(row.Flags, "one window is more than the whole result: without it the sign flips")
		case share >= DominantWindow:
			row.Flags = append(row.Flags, fmt.Sprintf("one window is %.0f%% of the result", share*100))
		}
		out.Rows = append(out.Rows, row)
	}
	sort.SliceStable(out.Rows, func(i, j int) bool { return out.Rows[i].LifetimePnLCents > out.Rows[j].LifetimePnLCents })
	return out
}

func sum(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += finite(x)
	}
	return s
}

func sortedKeys[V any](m map[int64]V) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
