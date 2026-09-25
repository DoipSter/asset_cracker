package store

import "encoding/json"

// These types are what the analysis reads return. The analysis package maps them into its own
// figures (Reconcile, Settle, the scorecard). This package does not import analysis.

// AnalysisMarket is one settled round, or one traded ladder leg, as listed for the promotion-gate
// reads.
type AnalysisMarket struct {
	ID     int64
	Coin   string
	Closes int64 // unix seconds
	Result string
	Family string // FamilyRounds or FamilyLadders, as strategy.family names them
}

// AnalysisWindowKey names one window: every market of one family closing at one instant. A
// ladder leg can close at the same instant as a 15-minute round (5 pm ET is a quarter hour), and
// the two are different windows.
type AnalysisWindowKey struct {
	Family string
	Closes int64 // unix seconds
}

// AnalysisBucket is one sim bucket of either family as listed for those reads.
type AnalysisBucket struct {
	ID, VersionID, LedgerAccount int64
	Strategy                     string
	Family                       string // FamilyRounds or FamilyLadders
	Version                      int
	Anti, Frozen, Replaced       bool
	// AllocatedCents is the sustainment allocation taken from this bucket to date (bucket_skim):
	// the bridge between what a strategy earned and what its bucket kept.
	AllocatedCents int64
}

// AnalysisTrade is one filled order of a window, plus the bid ladder at that second for a sale.
type AnalysisTrade struct {
	MarketID, OrderID, BucketID int64
	Sell                        bool
	Side                        string
	Qty                         int
	Second                      int64
	CostCents, PayoutCents      int64
	MoneyMissing, HasDepth      bool
	Displayed                   float64
	DepthPriced                 bool
}

// AnalysisSettlement is what one bucket was paid on one side of one market.
type AnalysisSettlement struct {
	MarketID, BucketID int64
	Side               string
	Qty                int
	PayoutCents        int64
}

// AnalysisScore is one market's scored rows in one time-to-close band.
type AnalysisScore struct {
	MarketID            int64
	Band                int
	N                   int64
	Model, Market       float64
	ModelLog, MarketLog float64
}

// AnalysisDecisionCount is how many journal rows one version wrote on one market.
type AnalysisDecisionCount struct {
	MarketID, VersionID, N int64
}

// AnalysisWindowData is the four reads of one settled window, before Reconcile.
type AnalysisWindowData struct {
	Trades      map[int64][]AnalysisTrade
	Settlements []AnalysisSettlement
	Scores      []AnalysisScore
	Decisions   []AnalysisDecisionCount
}

// MetricSnapshot is one row for metric_snapshot: a gate decision on one version. The period it
// covers runs from PeriodStart to LastClose (unix seconds), both included.
type MetricSnapshot struct {
	VersionID   int64
	PeriodStart int64
	FirstClose  int64
	LastClose   int64
	Decisions   int64
	Orders      int
	Trials      int
	Metrics     json.RawMessage
	GateConfig  json.RawMessage
	GatePassed  bool
}
