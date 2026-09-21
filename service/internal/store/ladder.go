package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// The ladder recorder's writes (kalshi.LadderRecorder, migration 0014). RECORD ONLY: nothing here
// touches a bucket, an order, a settlement or the ledger. None of these statements has run yet.

// UpsertMarkets records markets as UpsertMarket does, in one round trip, and returns their ids
// by ticker.
func (s *Store) UpsertMarkets(ctx context.Context, instrumentID int64, ms []Market) (map[string]int64, error) {
	out := make(map[string]int64, len(ms))
	if len(ms) == 0 {
		return out, nil
	}
	batch := &pgx.Batch{}
	for _, m := range ms {
		batch.Queue(sqlUpsertMarket, instrumentID, m.Ticker, m.Strike, m.OpensAt, m.ClosesAt)
	}
	br := s.pool.SendBatch(ctx, batch)
	for _, m := range ms {
		var id int64
		if err := br.QueryRow().Scan(&id); err != nil {
			_ = br.Close()
			return nil, err
		}
		out[m.Ticker] = id
	}
	return out, br.Close()
}

// EvaluationRow is one evaluation row as InsertEvaluations writes it.
type EvaluationRow struct {
	At              time.Time
	MarketID        int64
	UnderlyingPrice string // decimal text, or "" for none (stored as NULL)
	Quotes          any
	Model           any
}

// InsertEvaluations appends evaluation rows in one round trip. Nothing hangs decisions off them,
// so no ids come back.
func (s *Store) InsertEvaluations(ctx context.Context, rows []EvaluationRow) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		q, err := json.Marshal(r.Quotes)
		if err != nil {
			return err
		}
		model := r.Model
		if model == nil {
			model = map[string]any{}
		}
		m, err := json.Marshal(model)
		if err != nil {
			return err
		}
		batch.Queue(`insert into evaluation (at, market_id, underlying_price, quotes, model)
		             values ($1, $2, nullif($3, '')::numeric, $4, $5)`, r.At, r.MarketID, r.UnderlyingPrice, q, m)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// UnsettledBetween lists an instrument's markets that closed in [since, before) with no result
// yet, by ticker. Unlike UnsettledMarkets it keeps no one-hour rule: a daily or weekly market's
// result is wanted however long it takes to come, up to the caller's `since`.
func (s *Store) UnsettledBetween(ctx context.Context, instrumentID int64, since, before time.Time) (map[string]UnsettledMarket, error) {
	rows, err := s.pool.Query(ctx, `
		select m.ticker, m.id, m.closes_at from market m
		 where m.instrument_id = $1 and m.result is null and m.closes_at < $2 and m.closes_at >= $3`,
		instrumentID, before, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]UnsettledMarket{}
	for rows.Next() {
		var t string
		var m UnsettledMarket
		if err := rows.Scan(&t, &m.ID, &m.Closes); err != nil {
			return nil, err
		}
		out[t] = m
	}
	return out, rows.Err()
}
