// Package store is the service's access to Postgres.
//
// Everything written here is market data. Nothing in this package can move money: the ledger
// has its own package, and does not exist yet.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects and checks the connection works.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Instrument is one row of the instrument table, with its source's code.
type Instrument struct {
	ID         int64
	Source     string
	Kind       string
	Symbol     string
	Underlying string
	Spec       map[string]any
}

// ActiveInstruments lists what the service should watch.
func (s *Store) ActiveInstruments(ctx context.Context) ([]Instrument, error) {
	rows, err := s.pool.Query(ctx, `
		select i.id, s.code, i.kind, i.symbol, i.underlying, i.spec
		  from instrument i join source s on s.id = i.source_id
		 where i.active order by i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instrument
	for rows.Next() {
		var in Instrument
		var spec []byte
		if err := rows.Scan(&in.ID, &in.Source, &in.Kind, &in.Symbol, &in.Underlying, &spec); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(spec, &in.Spec); err != nil {
			return nil, fmt.Errorf("instrument %s spec: %w", in.Symbol, err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// EnsurePartitions makes sure this month's and next month's partitions exist.
func (s *Store) EnsurePartitions(ctx context.Context, now time.Time) error {
	for _, d := range []time.Time{now, now.AddDate(0, 1, 0)} {
		if _, err := s.pool.Exec(ctx, `select ensure_month_partitions($1::date)`, d); err != nil {
			return fmt.Errorf("partitions for %s: %w", d.Format("2006-01"), err)
		}
	}
	return nil
}

// Tick is one trade print.
type Tick struct {
	InstrumentID int64
	At           time.Time // the exchange's timestamp
	ReceivedAt   time.Time
	Price        string // decimal text exactly as the venue sent it; Postgres parses it as numeric
	Size         string
}

// InsertTicks writes a batch of trade prints.
func (s *Store) InsertTicks(ctx context.Context, ticks []Tick) error {
	if len(ticks) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, t := range ticks {
		batch.Queue(`insert into price_tick (instrument_id, at, received_at, price, size)
		             values ($1, $2, $3, $4::numeric, nullif($5, '')::numeric)`,
			t.InstrumentID, t.At, t.ReceivedAt, t.Price, t.Size)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

// Market is one tradable round of an instrument.
type Market struct {
	Ticker   string
	Strike   *float64
	OpensAt  *time.Time
	ClosesAt *time.Time
}

// UpsertMarket records a market and returns its id. Strike and times are filled in if they
// were missing before; a result is never touched here.
func (s *Store) UpsertMarket(ctx context.Context, instrumentID int64, m Market) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		insert into market (instrument_id, ticker, strike, opens_at, closes_at)
		values ($1, $2, $3, $4, $5)
		on conflict (instrument_id, ticker) do update
		   set strike    = coalesce(market.strike, excluded.strike),
		       opens_at  = coalesce(market.opens_at, excluded.opens_at),
		       closes_at = coalesce(market.closes_at, excluded.closes_at)
		returning id`, instrumentID, m.Ticker, m.Strike, m.OpensAt, m.ClosesAt).Scan(&id)
	return id, err
}

// RecordResult stores how a market settled. It writes only if no result is stored yet, and
// reports whether this call was the one that stored it, so a settlement is acted on once.
func (s *Store) RecordResult(ctx context.Context, marketID int64, result, settlementValue string, settledAt time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		update market set result = $2, settlement_value = nullif($3, '')::numeric, settled_at = $4
		 where id = $1 and result is null`, marketID, result, settlementValue, settledAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// UnsettledMarkets lists markets of an instrument that closed before `before` and have no
// result yet, so a restart picks up where the last run stopped.
func (s *Store) UnsettledMarkets(ctx context.Context, instrumentID int64, before time.Time) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `
		select ticker, id from market
		 where instrument_id = $1 and result is null and closes_at < $2 and closes_at > $2 - interval '1 hour'`,
		instrumentID, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var t string
		var id int64
		if err := rows.Scan(&t, &id); err != nil {
			return nil, err
		}
		out[t] = id
	}
	return out, rows.Err()
}

// InsertEvaluation records what was known about a market at one moment.
func (s *Store) InsertEvaluation(ctx context.Context, at time.Time, marketID int64, underlyingPrice string, quotes any) error {
	q, err := json.Marshal(quotes)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		insert into evaluation (at, market_id, underlying_price, quotes)
		values ($1, $2, nullif($3, '')::numeric, $4)`, at, marketID, underlyingPrice, q)
	return err
}
