package analysis

import (
	"fmt"
	"strings"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// MarketsFrom maps the store's listed rounds into this package's Market.
func MarketsFrom(rows []store.AnalysisMarket) []Market {
	out := make([]Market, len(rows))
	for i, r := range rows {
		out[i] = Market{ID: r.ID, Coin: r.Coin, Closes: r.Closes, Result: r.Result}
	}
	return out
}

// StoreMarkets maps this package's Market back to the store's listed-round type.
func StoreMarkets(markets []Market) []store.AnalysisMarket {
	out := make([]store.AnalysisMarket, len(markets))
	for i, m := range markets {
		out[i] = store.AnalysisMarket{ID: m.ID, Coin: m.Coin, Closes: m.Closes, Result: m.Result}
	}
	return out
}

// BucketsFrom maps the store's listed buckets into this package's Bucket: a twin is reported
// under its original's name, world "anti".
func BucketsFrom(rows []store.AnalysisBucket) ([]Bucket, map[int64]int64) {
	out := make([]Bucket, 0, len(rows))
	ledger := map[int64]int64{}
	for _, r := range rows {
		b := Bucket{ID: r.ID, VersionID: r.VersionID, Strategy: r.Strategy, Frozen: r.Frozen, Replaced: r.Replaced,
			Engine: fmt.Sprintf("v%d", r.Version), World: "real"}
		if r.Anti {
			b.World, b.Strategy = "anti", strings.TrimPrefix(r.Strategy, "Anti ")
		}
		ledger[r.ID] = r.LedgerAccount
		out = append(out, b)
	}
	return out, ledger
}

// MetricRows maps this package's Snapshots into the store's insert type.
func MetricRows(snaps []Snapshot) []store.MetricSnapshot {
	out := make([]store.MetricSnapshot, len(snaps))
	for i, sn := range snaps {
		out[i] = store.MetricSnapshot{VersionID: sn.VersionID, FirstClose: sn.FirstClose, LastClose: sn.LastClose,
			Decisions: sn.Decisions, Orders: sn.Orders, Trials: sn.Trials, Metrics: sn.Metrics, GateConfig: sn.GateConfig, GatePassed: sn.GatePassed}
	}
	return out
}

// AssembleWindow turns one window's store reads into facts, or says why each market is not
// ready. A market that is not ready must not be cached.
func AssembleWindow(markets []Market, data store.AnalysisWindowData) (facts []MarketFacts, unready map[int64]string) {
	trades := map[int64][]Trade{}
	for id, list := range data.Trades {
		for _, t := range list {
			trades[id] = append(trades[id], Trade{OrderID: t.OrderID, BucketID: t.BucketID, Sell: t.Sell, Side: t.Side,
				Qty: t.Qty, Second: t.Second, CostCents: t.CostCents, PayoutCents: t.PayoutCents, MoneyMissing: t.MoneyMissing,
				HasDepth: t.HasDepth, Displayed: t.Displayed, DepthPriced: t.DepthPriced})
		}
	}
	settled := map[int64]map[Holding]Paid{}
	for _, row := range data.Settlements {
		if settled[row.MarketID] == nil {
			settled[row.MarketID] = map[Holding]Paid{}
		}
		settled[row.MarketID][Holding{BucketID: row.BucketID, Side: row.Side}] = Paid{Qty: row.Qty, PayoutCents: row.PayoutCents}
	}

	unready = map[int64]string{}
	var ready []int64
	for _, m := range markets {
		if orders := MoneyMissing(trades[m.ID]); len(orders) > 0 {
			unready[m.ID] = fmt.Sprintf("orders %v have no recorded cost or payout", orders)
		} else if bad := Reconcile(trades[m.ID], settled[m.ID]); len(bad) > 0 {
			why := make([]string, len(bad))
			for i, b := range bad {
				why[i] = fmt.Sprintf("bucket %d held %d %s at the close and its settlement rows cover %d", b.BucketID, b.Held, b.Side, b.Settled)
			}
			unready[m.ID] = strings.Join(why, "; ")
		} else {
			ready = append(ready, m.ID)
		}
	}

	scores := map[int64]*[5]BandSum{}
	for _, row := range data.Scores {
		if scores[row.MarketID] == nil {
			scores[row.MarketID] = &[5]BandSum{}
		}
		if row.Band >= 0 && row.Band < len(scores[row.MarketID]) {
			scores[row.MarketID][row.Band] = BandSum{N: row.N, Model: row.Model, Market: row.Market, ModelLog: row.ModelLog, MarketLog: row.MarketLog}
		}
	}
	decisions := map[int64]map[int64]int64{}
	for _, row := range data.Decisions {
		if decisions[row.MarketID] == nil {
			decisions[row.MarketID] = map[int64]int64{}
		}
		decisions[row.MarketID][row.VersionID] = row.N
	}

	facts = make([]MarketFacts, 0, len(ready))
	for _, m := range markets {
		if _, held := unready[m.ID]; held {
			continue
		}
		f := Settle(m, trades[m.ID], settled[m.ID])
		if sc := scores[m.ID]; sc != nil {
			f.Score = *sc
		}
		f.Decisions = decisions[m.ID]
		facts = append(facts, f)
	}
	return facts, unready
}
