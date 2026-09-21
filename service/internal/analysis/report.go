package analysis

import (
	"fmt"
	"sort"
)

// Coins is the order the contract lists the coins in. A coin with nothing scored still appears.
var Coins = []string{"BTC", "ETH", "SOL", "XRP", "DOGE"}

// Bucket is one strategy bucket, live or frozen. Every life of a strategy that ran out is its own
// bucket; they share a strategy version, which is what the leaderboard groups by.
type Bucket struct {
	ID, VersionID int64
	Strategy      string // "Scalper"; a twin carries its original's name and World "anti"
	Engine        string // "v1" or "v2"
	World         string // "real" or "anti"
	Frozen        bool
	Replaced      bool // ran out and was staked again: the next life is another bucket
}

// Coverage says how much of the settled history the figures rest on. The per-market aggregates
// are filled a few windows per refresh, so after a restart the document is PARTIAL for a while
// and must say so rather than pass a part off as the whole.
type Coverage struct {
	MarketsSettled    int  `json:"markets_settled"`
	MarketsAggregated int  `json:"markets_aggregated"`
	WindowsIncomplete int  `json:"windows_incomplete"` // left out: a market in them is unsettled or not yet aggregated
	Complete          bool `json:"complete"`
}

// Conventions is the verdict rule, stated so nobody mistakes it for a finding.
type Conventions struct {
	Note           string  `json:"note"`
	MinWindows     int     `json:"min_windows"`
	MinAbsT        float64 `json:"min_abs_t"`
	DominantWindow float64 `json:"dominant_window_share"`
}

// ScoreRow is one line of the scorecard. Brier figures are means over WINDOWS of each window's
// own Brier score, so that diff = brier_model - brier_market is the same mean the SE is of.
type ScoreRow struct {
	Band        string  `json:"band,omitempty"`
	Coin        string  `json:"coin,omitempty"`
	NRows       int64   `json:"n_rows"`
	NWindows    int     `json:"n_windows"`
	BrierModel  float64 `json:"brier_model"`
	BrierMarket float64 `json:"brier_market"`
	Diff        float64 `json:"diff"`
	SE          float64 `json:"se"`
	T           float64 `json:"t"`
	Verdict     string  `json:"verdict"`
}

type Scorecard struct {
	What    string     `json:"what"`
	Overall ScoreRow   `json:"overall"`
	ByBand  []ScoreRow `json:"by_band"`
	ByCoin  []ScoreRow `json:"by_coin"`
}

type FillRow struct {
	Strategy           string  `json:"strategy"`
	Engine             string  `json:"engine"`
	World              string  `json:"world"`
	Sells              int     `json:"sells"`        // every early sale, priced or not
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
	Strategy           string   `json:"strategy"`
	Engine             string   `json:"engine"`
	World              string   `json:"world"`
	Lives              int      `json:"lives"`
	EquityCents        int64    `json:"equity_cents"`
	LifetimePnLCents   int64    `json:"lifetime_pnl_cents"`
	Bets               int      `json:"bets"`
	Windows            int      `json:"windows"`
	MeanWindowPnLCents int64    `json:"mean_window_pnl_cents"`
	SECents            int64    `json:"se_cents"`
	T                  float64  `json:"t"`
	TopWindowShare     float64  `json:"top_window_share"`
	Verdict            string   `json:"verdict"`
	Flags              []string `json:"flags"`
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
}

const (
	scorecardWhat = "Brier score of the model's probability against the market's own mid price, on settled rounds. Lower is better. " +
		"The unit is one 15-minute window (all coins together), because bets inside a window are not independent: each window is scored on its own, " +
		"and brier_model, brier_market and diff are means over windows. " +
		"The model figure is decision.model_prob of the second engine (v2): the RAW model probability of YES, before any strategy blends it with the market " +
		"(the blended figure is not journaled). The market figure is decision.market_prob: the middle of the Yes bid and ask, or the ask alone when there is no bid. " +
		"One row is one second of one round in which at least one v2 strategy journaled a decision; idle strategies journal only on a change and every 15 s, " +
		"so seconds are not evenly covered."
	fillsWhat = "The simulator sells any size at the bid. This re-prices every early sale as if only the size actually displayed at that second had filled, the rest riding to settlement " +
		"(1.00 a contract if its side won, else nothing). Sales by one bucket in the same second share one displayed size. " +
		"contracts_sold, contracts_beyond_bid, share_beyond and both P&L figures cover only the sells_priced sales: settled rounds where depth was recorded. " +
		"Proceeds and cost are the recorded ones pro rata, fees inside; the fee's round-up to a cent is not recomputed, so a sale can be off by under a cent."
	leaderboardWhat = "Per strategy version. Windows are the independent sample, not bets. Lifetime includes every earlier life of a strategy that ran out. " +
		"lifetime_pnl_cents is realised trading P&L on settled windows (payouts and sale proceeds less what the bets cost, fees inside), before any sustainment allocation; " +
		"bets and windows count the same settled windows. equity_cents is the live bucket's ledger cash plus its open bets at cost (book value, not marked to market); 0 if no bucket is live."
)

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
	doc := Document{Simulated: true, ComputedAt: in.ComputedAt, WindowsRecorded: len(windows),
		Coverage:    Coverage{MarketsSettled: in.MarketsSettled, MarketsAggregated: len(in.Facts), WindowsIncomplete: len(left), Complete: len(in.Facts) >= in.MarketsSettled},
		Conventions: Conventions{Note: "The verdict rule is a convention chosen in advance, not a measurement: unresolved unless n_windows >= min_windows AND |t| >= min_abs_t.", MinWindows: MinWindows, MinAbsT: MinAbsT, DominantWindow: DominantWindow}}
	doc.Scorecard = buildScorecard(facts)
	doc.Fills = buildFills(facts, in.Buckets, in.UnsettledSells)
	doc.Leaderboard = buildLeaderboard(facts, in.Buckets, in.BookCents)
	return doc
}

// scoreRow scores the rows that keep() admits: a Brier pair per window, then the window stat of
// their difference.
func scoreRow(facts []MarketFacts, keep func(coin string, band int) bool) ScoreRow {
	type sums struct{ n, model, market float64 }
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
			rows += b.N
		}
	}
	var model, market, diff []float64
	for _, w := range sortedKeys(per) {
		s := per[w]
		model, market = append(model, s.model/s.n), append(market, s.market/s.n)
		diff = append(diff, (s.model-s.market)/s.n)
	}
	st := WindowStat(diff)
	return ScoreRow{NRows: rows, NWindows: st.N, BrierModel: round(WindowStat(model).Mean, 4), BrierMarket: round(WindowStat(market).Mean, 4),
		Diff: round(st.Mean, 4), SE: round(st.SE, 4), T: round(st.T, 2), Verdict: Verdict(st, "model worse", "model better")}
}

func buildScorecard(facts []MarketFacts) Scorecard {
	sc := Scorecard{What: scorecardWhat, ByBand: []ScoreRow{}, ByCoin: []ScoreRow{}}
	sc.Overall = scoreRow(facts, func(string, int) bool { return true })
	for i, b := range Bands {
		r := scoreRow(facts, func(_ string, band int) bool { return band == i })
		r.Band = b.Label
		sc.ByBand = append(sc.ByBand, r)
	}
	for _, c := range Coins {
		r := scoreRow(facts, func(coin string, _ int) bool { return coin == c })
		r.Coin = c
		sc.ByCoin = append(sc.ByCoin, r)
	}
	return sc
}

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
		if r := row(bucket); r != nil && n > 0 {
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

func buildLeaderboard(facts []MarketFacts, buckets []Bucket, book map[int64]int64) Leaderboard {
	byBucket, versions := versionsOf(buckets)
	type tally struct {
		windows map[int64]float64 // closes -> P&L in cents, windows with at least one bet
		bets    int
	}
	tallies := map[int64]*tally{}
	for id := range versions {
		tallies[id] = &tally{windows: map[int64]float64{}}
	}
	for _, f := range facts {
		for bucket, r := range f.Rounds {
			t := tallies[byBucket[bucket]]
			if t == nil || r.Bets == 0 {
				continue
			}
			t.windows[f.Closes] += float64(r.PnLCents)
			t.bets += r.Bets
		}
	}
	out := Leaderboard{What: leaderboardWhat, Rows: []LeaderRow{}}
	for _, id := range sortedKeys(tallies) {
		v, t := versions[id], tallies[id]
		var per []float64
		var total float64
		for _, w := range sortedKeys(t.windows) {
			per = append(per, t.windows[w])
			total += t.windows[w]
		}
		st := WindowStat(per)
		row := LeaderRow{Strategy: v.strategy, Engine: v.engine, World: v.world, Lives: 1, LifetimePnLCents: int64(total), Bets: t.bets,
			Windows: st.N, MeanWindowPnLCents: int64(round(st.Mean, 0)), SECents: int64(round(st.SE, 0)), T: round(st.T, 2),
			TopWindowShare: round(TopWindowShare(per), 2), Verdict: Verdict(st, "ahead", "behind"), Flags: []string{}}
		live := false
		for _, b := range buckets {
			if b.VersionID != id {
				continue
			}
			if b.Replaced {
				row.Lives++
			}
			if !b.Frozen {
				live = true
				row.EquityCents += book[b.ID]
			}
		}
		if row.Lives > 1 {
			row.Flags = append(row.Flags, fmt.Sprintf("ran out and was staked again: this is life %d, and every life is counted here", row.Lives))
		}
		if !live {
			row.Flags = append(row.Flags, "ran out and was not staked again")
		}
		switch share := TopWindowShare(per); {
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

func sortedKeys[V any](m map[int64]V) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
