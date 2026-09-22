package app

import (
	"context"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// splitLadders takes the above/below ladders (migration 0014) out of the instrument list. They
// are RECORD ONLY and go to kalshi.LadderRecorder and nowhere else: the rest of run() is handed
// exactly the instruments it had before they existed, so a ladder gets no 15-minute poller, no
// runner, no engine, no place in the health check or the status document, and no asset on the
// home page.
func splitLadders(all []store.Instrument) (rest, ladders []store.Instrument) {
	for _, in := range all {
		if isLadder(in) {
			ladders = append(ladders, in)
		} else {
			rest = append(rest, in)
		}
	}
	return rest, ladders
}

func isLadder(in store.Instrument) bool {
	flag, _ := in.Spec["ladder"].(bool)
	return in.Kind == "binary_ladder" || flag
}

// ladderSink writes one ladder series' markets, rows and results. It holds the database and the
// instrument and NOTHING else: no engine can be reached from here, so a result is stored with the
// first-writer-wins RecordResult and never settled against a bet.
type ladderSink struct {
	db           *store.Store
	instrumentID int64
}

func (s *ladderSink) SaveMarkets(ctx context.Context, ms []kalshi.LadderNew) (map[string]int64, error) {
	out := make([]store.Market, len(ms))
	for i, m := range ms {
		closes := m.Closes
		out[i] = store.Market{Ticker: m.Ticker, Strike: m.Strike, ClosesAt: &closes}
		if !m.Opens.IsZero() {
			opens := m.Opens
			out[i].OpensAt = &opens
		}
	}
	return s.db.UpsertMarkets(ctx, s.instrumentID, out)
}

func (s *ladderSink) SaveRows(ctx context.Context, rows []kalshi.LadderRow) error {
	out := make([]store.EvaluationRow, len(rows))
	for i, r := range rows {
		out[i] = store.EvaluationRow{At: r.At, MarketID: r.MarketID, UnderlyingPrice: r.Price, Quotes: r.Quotes, Model: r.Model}
	}
	return s.db.InsertEvaluations(ctx, out)
}

func (s *ladderSink) SaveResult(ctx context.Context, marketID int64, m kalshi.MarketInfo) (bool, error) {
	settled, err := time.Parse(time.RFC3339Nano, m.SettlementTS)
	if err != nil {
		settled = time.Now()
	}
	return s.db.RecordResult(ctx, marketID, m.Result, m.ExpirationValue, settled)
}

func (s *ladderSink) Unsettled(ctx context.Context, since, before time.Time) (map[string]kalshi.Pending, error) {
	found, err := s.db.UnsettledBetween(ctx, s.instrumentID, since, before)
	out := map[string]kalshi.Pending{}
	for ticker, m := range found {
		out[ticker] = kalshi.Pending{ID: m.ID, Closes: m.Closes}
	}
	return out, err
}

// freshPrice is a coin's latest trade price, or "" if it is stale, by the rule sink.price uses.
func freshPrice(latest *coinbase.Latest, product string) func() string {
	return func() string {
		if t, ok := latest.Get(product); ok && time.Since(t.ReceivedAt) < 30*time.Second {
			return t.Price
		}
		return ""
	}
}
