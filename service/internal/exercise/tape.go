package exercise

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/doipster/asset_cracker/service/internal/engine"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// The tape: the recorded snapshots of the settled markets of one family inside a window, read
// from the record in a READ ONLY transaction (store.ReadOnly). Two statements: the markets
// closing in the window that have a result, then their evaluations thinned to the first
// snapshot per market per step_s seconds, in time order. Only the columns the replay reads
// travel: the two bid ladders, the model's journaled view and the spot price, not the whole
// quotes document.

// Caps on a read, so that one call cannot occupy the record or outlive the client's patience.
const (
	// MaxSpanRounds is the longest window of 15-minute rounds one call may replay: two days is
	// about 340,000 snapshots of the two coins the model prices, enough for a TRAIN walk from
	// t0 through the current settled clocks without resetting the window owner. The ladders
	// hold legs open for days, so theirs is a week and their thinning is coarser.
	MaxSpanRounds  = 48 * time.Hour
	MaxSpanLadders = 7 * 24 * time.Hour
	// DefaultStepRounds is every recorded second. Measured on 2026-09-23 against the record:
	// thinning to one snapshot in five cost Mid-round Favourite 21 of its 76 bets (its band,
	// 0.70 to 0.90, is one the ask flickers across within a second) while Value lost 5 of 240,
	// so a coarser step is the caller's choice, never the default. MinStepLadders is the finest
	// thinning a ladder replay may ask for: a leg is open for days, and every second of it
	// would be a million rows.
	DefaultStepRounds  = 1
	DefaultStepLadders = 60
	MinStepLadders     = 30
	// ReadTimeout is the statement timeout of the tape's reads.
	ReadTimeout = 25 * time.Second
)

// kindOf is the instrument kind each family trades, as the instrument table spells it.
func kindOf(family string) (string, error) {
	switch family {
	case engine.FamilyRounds:
		return "binary_contract", nil
	case engine.FamilyLadders:
		return "binary_ladder", nil
	}
	return "", fmt.Errorf("family %q is not %s or %s", family, engine.FamilyRounds, engine.FamilyLadders)
}

// Limits are a family's caps: how long a window, how fine a step.
type Limits struct {
	MaxSpan     time.Duration
	DefaultStep int
	MinStep     int
}

// LimitsOf are the caps for a family.
func LimitsOf(family string) Limits {
	if family == engine.FamilyLadders {
		return Limits{MaxSpan: MaxSpanLadders, DefaultStep: DefaultStepLadders, MinStep: MinStepLadders}
	}
	return Limits{MaxSpan: MaxSpanRounds, DefaultStep: DefaultStepRounds, MinStep: 1}
}

// MarketRow is one settled market of the window.
type MarketRow struct {
	ID     int64
	Ticker string
	Coin   string
	Symbol string
	Strike float64
	Opens  time.Time // when unknown, the close less the family's usual life
	Closes time.Time
	Result string
}

// Reader reads the tape from the record.
type Reader struct{ DB *store.Store }

// Markets is every market of the family closing in [from, to) that has a result, in close order.
func (r Reader) Markets(ctx context.Context, q store.Querier, family string, from, to time.Time) ([]MarketRow, error) {
	kind, err := kindOf(family)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		select m.id, m.ticker, i.underlying, i.symbol, coalesce(m.strike, 0)::float8, m.opens_at, m.closes_at, m.result
		  from market m join instrument i on i.id = m.instrument_id
		 where i.kind = $1 and m.closes_at >= $2 and m.closes_at < $3 and m.result in ('yes', 'no')
		 order by m.closes_at, i.symbol, m.ticker`, kind, from, to)
	if err != nil {
		return nil, fmt.Errorf("markets: %w", err)
	}
	defer rows.Close()
	var out []MarketRow
	for rows.Next() {
		var m MarketRow
		var opens *time.Time
		if err := rows.Scan(&m.ID, &m.Ticker, &m.Coin, &m.Symbol, &m.Strike, &opens, &m.Closes, &m.Result); err != nil {
			return nil, err
		}
		m.Closes = m.Closes.UTC()
		if opens != nil {
			m.Opens = opens.UTC()
		} else if family == engine.FamilyLadders {
			m.Opens = m.Closes.Add(-MaxSpanLadders)
		} else {
			m.Opens = m.Closes.Add(-15 * time.Minute)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// journaled is model.v3 as engine.View.Journal writes it.
type journaled struct {
	OK            bool     `json:"ok"`
	Sigma2        float64  `json:"sigma2"`
	Offset        float64  `json:"index_offset"`
	OffsetSamples int      `json:"offset_samples"`
	OffsetSource  string   `json:"offset_source"`
	PModel        float64  `json:"p_model"`
	Drift         bool     `json:"drift"`
	VolRatio      *float64 `json:"vol_ratio"`
	Horizon       string   `json:"horizon"`
}

// Snapshots reads the thinned evaluations of the markets and returns them in time order. stepS
// is the thinning: the first snapshot of each market in each stepS-second bin.
//
// Only snapshots that carry the model's key travel: a coin the model never priced (SOL, XRP,
// DOGE at this writing) can never be entered, so its rows would be read, shipped and answered
// "no model view" one by one, and they are half the tape. They are counted instead (unpriced,
// in the same bins) and the answer says so. A snapshot of a priced coin whose view was not ok
// that second still travels, so the book is observed and the count is honest.
func (r Reader) Snapshots(ctx context.Context, q store.Querier, markets []MarketRow, stepS int) (snaps []Snapshot, unpriced int, err error) {
	if len(markets) == 0 {
		return nil, 0, nil
	}
	if stepS < 1 {
		stepS = 1
	}
	byID := make(map[int64]MarketRow, len(markets))
	ids := make([]int64, 0, len(markets))
	first, last := markets[0].Opens, markets[0].Closes
	for _, m := range markets {
		byID[m.ID] = m
		ids = append(ids, m.ID)
		if m.Opens.Before(first) {
			first = m.Opens
		}
		if m.Closes.After(last) {
			last = m.Closes
		}
	}
	if err := q.QueryRow(ctx, `
		select count(*) from (
			select distinct e.market_id, floor(extract(epoch from e.at) / $3)
			  from evaluation e
			 where e.market_id = any($1) and e.at >= $2 and e.at < $4 and not (e.model ? 'v3')) x`, ids, first, stepS, last).Scan(&unpriced); err != nil {
		return nil, 0, fmt.Errorf("unpriced snapshots: %w", err)
	}
	rows, err := q.Query(ctx, `
		select distinct on (e.market_id, floor(extract(epoch from e.at) / $3))
		       e.id, e.at, e.market_id, e.underlying_price::float8, e.quotes->'yes_bids', e.quotes->'no_bids', e.model->'v3'
		  from evaluation e
		 where e.market_id = any($1) and e.at >= $2 and e.at < $4 and e.model ? 'v3'
		 order by e.market_id, floor(extract(epoch from e.at) / $3), e.at, e.id`, ids, first, stepS, last)
	if err != nil {
		return nil, 0, fmt.Errorf("snapshots: %w", err)
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var s Snapshot
		var price *float64
		var yes, no, v3 []byte
		if err := rows.Scan(&s.EvaluationID, &s.At, &s.MarketID, &price, &yes, &no, &v3); err != nil {
			return nil, 0, err
		}
		m := byID[s.MarketID]
		s.At, s.Ticker, s.Coin, s.Symbol, s.Strike, s.Closes, s.Result = s.At.UTC(), m.Ticker, m.Coin, m.Symbol, m.Strike, m.Closes, m.Result
		if price != nil {
			s.Price = *price
		}
		if len(yes) > 0 && string(yes) != "null" {
			if err := json.Unmarshal(yes, &s.Quotes.YesBids); err != nil {
				return nil, 0, fmt.Errorf("evaluation %d: yes_bids: %w", s.EvaluationID, err)
			}
		}
		if len(no) > 0 && string(no) != "null" {
			if err := json.Unmarshal(no, &s.Quotes.NoBids); err != nil {
				return nil, 0, fmt.Errorf("evaluation %d: no_bids: %w", s.EvaluationID, err)
			}
		}
		s.HasDepth = len(s.Quotes.YesBids) > 0 || len(s.Quotes.NoBids) > 0
		s.View = viewOf(m.Coin, s.Price, v3)
		s.HasVolRatio = s.View.OK && s.View.VolRatio > 0
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].EvaluationID < out[j].EvaluationID
	})
	return out, unpriced, nil
}

// viewOf rebuilds the engine's view from the journaled model.v3. A missing or not-ok journal is a
// view that is not OK, on which the engine decides nothing, as it did live.
func viewOf(coin string, price float64, raw []byte) engine.View {
	v := engine.View{Coin: coin, Price: price}
	if len(raw) == 0 || string(raw) == "null" {
		return v
	}
	var j journaled
	if err := json.Unmarshal(raw, &j); err != nil || !j.OK {
		return v
	}
	v.OK, v.Sigma2, v.Offset, v.OffsetSamples, v.OffsetSource, v.PModel, v.Drift = true, j.Sigma2, j.Offset, j.OffsetSamples, j.OffsetSource, j.PModel, j.Drift
	if j.VolRatio != nil {
		v.VolRatio = *j.VolRatio
	}
	v.Horizon = j.Horizon
	if v.Horizon == "" {
		v.Horizon = "fast"
	}
	return v
}

// Read is the statements in one read-only transaction: the markets, then their snapshots.
func (r Reader) Read(ctx context.Context, family string, from, to time.Time, stepS int) (markets []MarketRow, snaps []Snapshot, unpriced int, err error) {
	err = r.DB.ReadOnly(ctx, ReadTimeout, func(q store.Querier) error {
		var err error
		if markets, err = r.Markets(ctx, q, family, from, to); err != nil {
			return err
		}
		snaps, unpriced, err = r.Snapshots(ctx, q, markets, stepS)
		return err
	})
	return markets, snaps, unpriced, err
}

// Quotes is here so a test can build a book the way the recorder writes one.
func Quotes(yes, no [][2]string) kalshi.Quotes { return kalshi.Quotes{YesBids: yes, NoBids: no} }
