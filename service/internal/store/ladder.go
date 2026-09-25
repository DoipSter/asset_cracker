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

// InsertEvaluations appends evaluation rows in one round trip and returns their ids in the
// rows' order: the ladder engine's decisions hang off them, as the rounds' do.
func (s *Store) InsertEvaluations(ctx context.Context, rows []EvaluationRow) ([]int64, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		q, err := json.Marshal(r.Quotes)
		if err != nil {
			return nil, err
		}
		model := r.Model
		if model == nil {
			model = map[string]any{}
		}
		m, err := json.Marshal(model)
		if err != nil {
			return nil, err
		}
		batch.Queue(`insert into evaluation (at, market_id, underlying_price, quotes, model)
		             values ($1, $2, nullif($3, '')::numeric, $4, $5) returning id`, r.At, r.MarketID, r.UnderlyingPrice, q, m)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	ids := make([]int64, 0, len(rows))
	for range rows {
		var id int64
		if err := br.QueryRow().Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// HourlyCloses is a spot instrument's hourly candle closes since a time, oldest first, with
// each candle's start time: what the volatility forecast is fitted on.
func (s *Store) HourlyCloses(ctx context.Context, symbol string, since time.Time) ([]HourlyClose, error) {
	rows, err := s.pool.Query(ctx, `
		select c.at, c.close::float8
		  from candle c join instrument i on i.id = c.instrument_id
		 where i.symbol = $1 and c.granularity_s = 3600 and c.at >= $2
		 order by c.at`, symbol, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HourlyClose
	for rows.Next() {
		var h HourlyClose
		if err := rows.Scan(&h.At, &h.Close); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HourlyClose is one hourly candle's close at its start time.
type HourlyClose struct {
	At    time.Time
	Close float64
}

// InsertAnalysisResult appends one row to the analysis_result table (migration 0016): an
// analysis computed by the service, in its own shape, never edited; a rerun is a new row.
func (s *Store) InsertAnalysisResult(ctx context.Context, key, source, codeSHA string, params any, windowFrom, windowTo time.Time, result any) error {
	_, err := s.RecordAnalysisResult(ctx, key, source, codeSHA, params, windowFrom, windowTo, result)
	return err
}

// RecordAnalysisResult is InsertAnalysisResult returning the new row's id, for a caller that
// hands it back (the exercise record route).
func (s *Store) RecordAnalysisResult(ctx context.Context, key, source, codeSHA string, params any, windowFrom, windowTo time.Time, result any) (int64, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return 0, err
	}
	r, err := json.Marshal(result)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `insert into analysis_result (key, source, code_sha, params, window_from, window_to, result)
	                             values ($1, $2, nullif($3, ''), $4, $5, $6, $7) returning id`, key, source, codeSHA, p, windowFrom, windowTo, r).Scan(&id)
	return id, err
}

// DailyCloses is the last n daily candle closes of a spot instrument (by symbol, e.g. BTC-USD),
// oldest first: what the model's long volatility is read from at start.
func (s *Store) DailyCloses(ctx context.Context, symbol string, n int) ([]float64, error) {
	rows, err := s.pool.Query(ctx, `
		select c.close::float8
		  from candle c join instrument i on i.id = c.instrument_id
		 where i.symbol = $1 and c.granularity_s = 86400
		 order by c.at desc limit $2`, symbol, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var desc []float64
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		desc = append(desc, v)
	}
	out := make([]float64, len(desc))
	for i, v := range desc {
		out[len(desc)-1-i] = v
	}
	return out, rows.Err()
}

// UnsettledBetween lists an instrument's markets that closed before `before` with no result yet,
// by ticker: those that closed at or after `since`, and every one a bucket traded however long ago
// it closed, since until its result is stored the bets on it stay open (UnsettledMarkets' rule
// for the rounds, with the caller's `since` in place of its hour).
func (s *Store) UnsettledBetween(ctx context.Context, instrumentID int64, since, before time.Time) (map[string]UnsettledMarket, error) {
	rows, err := s.pool.Query(ctx, `
		select m.ticker, m.id, m.closes_at from market m
		 where m.instrument_id = $1 and m.result is null and m.closes_at < $2
		   and (m.closes_at >= $3 or exists (select 1 from trade_order o where o.market_id = m.id))`,
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
