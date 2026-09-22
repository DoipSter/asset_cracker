package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/runner"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// sink connects one Kalshi series' poller to the database and to the live engine.
type sink struct {
	db           *store.Store
	instrumentID int64
	latest       *coinbase.Latest
	product      string
	run          *runner.Runner3
	coin         string
}

// safely runs one call into the live engine on someone else's goroutine. The runner's entry
// points recover their own panics and suspend the engine; this is the second fence, so that
// whatever goes wrong there the poller, the price stream and the snapshot writer never see it.
func safely(what string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("a panic escaped the live engine and was stopped here", "in", what, "panic", p)
		}
	}()
	fn()
}

// price is the latest trade price, or "" if it is stale: a stale price is worse than none.
func (s *sink) price() string {
	if t, ok := s.latest.Get(s.product); ok && time.Since(t.ReceivedAt) < 30*time.Second {
		return t.Price
	}
	return ""
}

func (s *sink) SaveMarket(ctx context.Context, m kalshi.MarketInfo, closes time.Time) (int64, error) {
	mk := store.Market{Ticker: m.Ticker, Strike: m.FloorStrike, ClosesAt: &closes}
	if opens := m.Opens(); !opens.IsZero() {
		mk.OpensAt = &opens
	}
	return s.db.UpsertMarket(ctx, s.instrumentID, mk)
}

func (s *sink) SaveQuotes(ctx context.Context, at time.Time, marketID int64, m kalshi.MarketInfo, closes time.Time, q kalshi.Quotes) error {
	price := s.price()
	model := map[string]any{}
	if t, ok := s.latest.Get(s.product); ok {
		model["price_age_s"] = at.Sub(t.At).Seconds()
	}
	if s.run != nil && s.coin != "" {
		safely("inputs", func() {
			if in := s.run.Inputs(s.coin, m, closes, at, price); in != nil {
				model["v3"] = in
			}
		})
	}
	evalID, err := s.db.InsertEvaluation(ctx, at, marketID, price, q, model)
	if err != nil {
		return err
	}
	if s.run != nil && s.coin != "" {
		safely("step", func() { s.run.Step(ctx, s.coin, evalID, at, marketID, m, closes, q, price) })
	}
	return nil
}

func (s *sink) SaveResult(ctx context.Context, marketID int64, m kalshi.MarketInfo, closes time.Time) (bool, error) {
	settled, err := time.Parse(time.RFC3339Nano, m.SettlementTS)
	if err != nil {
		settled = time.Now()
	}
	first, err := s.db.RecordResult(ctx, marketID, m.Result, m.ExpirationValue, settled)
	if err != nil || !first {
		return first, err
	}
	if s.run != nil {
		safely("settled", func() { s.run.Settled(ctx, s.coin, marketID, m, closes) })
	}
	return true, nil
}

func (s *sink) Unsettled(ctx context.Context, before time.Time) (map[string]kalshi.Pending, error) {
	found, err := s.db.UnsettledMarkets(ctx, s.instrumentID, before)
	out := map[string]kalshi.Pending{}
	for ticker, m := range found {
		out[ticker] = kalshi.Pending{ID: m.ID, Closes: m.Closes}
	}
	return out, err
}
