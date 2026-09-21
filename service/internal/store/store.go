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

// UnsettledMarket is a closed round with no result stored yet.
type UnsettledMarket struct {
	ID     int64
	Closes time.Time
}

// UnsettledMarkets lists markets of an instrument that closed before `before` and have no
// result yet, so a restart picks up where the last run stopped.
func (s *Store) UnsettledMarkets(ctx context.Context, instrumentID int64, before time.Time) (map[string]UnsettledMarket, error) {
	rows, err := s.pool.Query(ctx, `
		select ticker, id, closes_at from market
		 where instrument_id = $1 and result is null and closes_at < $2 and closes_at > $2 - interval '1 hour'`,
		instrumentID, before)
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

// InsertEvaluation records what was known about a market at one moment and returns the row's
// id, so every strategy family that looked at it can hang its decisions off the same row.
func (s *Store) InsertEvaluation(ctx context.Context, at time.Time, marketID int64, underlyingPrice string, quotes, model any) (int64, error) {
	q, err := json.Marshal(quotes)
	if err != nil {
		return 0, err
	}
	if model == nil {
		model = map[string]any{}
	}
	m, err := json.Marshal(model)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `
		insert into evaluation (at, market_id, underlying_price, quotes, model)
		values ($1, $2, nullif($3, '')::numeric, $4, $5) returning id`, at, marketID, underlyingPrice, q, m).Scan(&id)
	return id, err
}

// RoundSummary is one round as the status page lists it.
type RoundSummary struct {
	Series          string     `json:"series"`
	Ticker          string     `json:"ticker"`
	Strike          *float64   `json:"strike"`
	ClosesAt        *time.Time `json:"closes_at"`
	Result          *string    `json:"result"`
	SettlementValue *float64   `json:"settlement_value"`
	Snapshots       int64      `json:"snapshots"`
}

// RecentRounds lists the newest rounds across every series, newest first.
func (s *Store) RecentRounds(ctx context.Context, limit int) ([]RoundSummary, error) {
	rows, err := s.pool.Query(ctx, `
		select i.symbol, m.ticker, m.strike::float8, m.closes_at, m.result, m.settlement_value::float8,
		       (select count(*) from evaluation e where e.market_id = m.id and e.at >= m.closes_at - interval '1 hour')
		  from market m join instrument i on i.id = m.instrument_id
		 order by m.closes_at desc nulls last, m.ticker limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoundSummary
	for rows.Next() {
		var r RoundSummary
		if err := rows.Scan(&r.Series, &r.Ticker, &r.Strike, &r.ClosesAt, &r.Result, &r.SettlementValue, &r.Snapshots); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeriesPoint is one moment of a round: the underlying price and the market's quotes.
type SeriesPoint struct {
	At     time.Time `json:"at"`
	Price  *float64  `json:"price"`
	YesBid *float64  `json:"yes_bid"`
	YesAsk *float64  `json:"yes_ask"`
}

// RoundSeries returns a round's recorded history, thinned to one point per `every`.
func (s *Store) RoundSeries(ctx context.Context, ticker string, every time.Duration) ([]SeriesPoint, error) {
	rows, err := s.pool.Query(ctx, `
		select date_bin($2::interval, e.at, 'epoch'::timestamptz) as t,
		       avg(e.underlying_price)::float8,
		       avg(nullif((e.quotes->>'yes_bid')::numeric, 0))::float8,
		       avg(nullif((e.quotes->>'yes_ask')::numeric, 0))::float8
		  from evaluation e join market m on m.id = e.market_id
		 where m.ticker = $1 and e.at >= m.closes_at - interval '1 hour'
		 group by 1 order by 1`, ticker, every)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.At, &p.Price, &p.YesBid, &p.YesAsk); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// StrategyRow is one entry of the trials registry.
type StrategyRow struct {
	Family  string `json:"family"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	Status  string `json:"status"`
	Blurb   string `json:"blurb"`
}

// StrategyVersions lists the trials registry.
func (s *Store) StrategyVersions(ctx context.Context) ([]StrategyRow, error) {
	rows, err := s.pool.Query(ctx, `
		select st.family, st.name, v.version, v.status, st.description
		  from strategy_version v join strategy st on st.id = v.strategy_id order by v.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StrategyRow
	for rows.Next() {
		var r StrategyRow
		if err := rows.Scan(&r.Family, &r.Name, &r.Version, &r.Status, &r.Blurb); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LedgerEntryCount says how many ledger entries exist: zero until something trades.
func (s *Store) LedgerEntryCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `select count(*) from ledger_entry`).Scan(&n)
	return n, err
}

// LastMinute returns a product's last trade price in each of the last sixty seconds that had
// one, oldest first: what the 1M chart draws, where a missing second is a gap in the feed.
func (s *Store) LastMinute(ctx context.Context, product string) ([][2]float64, error) {
	rows, err := s.pool.Query(ctx, `
		select extract(epoch from date_trunc('second', t.at))::float8,
		       ((array_agg(t.price order by t.at desc))[1])::float8
		  from price_tick t join instrument i on i.id = t.instrument_id
		 where i.symbol = $1 and t.at > now() - interval '60 seconds'
		 group by 1 order by 1`, product)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := [][2]float64{}
	for rows.Next() {
		var p [2]float64
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
