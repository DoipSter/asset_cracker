package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Coinbase candles (migration 0015). RECORD ONLY: nothing here touches a bucket, an order or the
// ledger. None of these statements has run yet.

// Candle is one stored candle. Prices and volume are decimal text: exactly as Coinbase sent them
// on the way in, Postgres's numeric text on the way out.
type Candle struct {
	InstrumentID int64
	GranularityS int
	At           time.Time // the candle's START
	Open, High   string
	Low, Close   string
	Volume       string
	FetchedAt    time.Time
}

// sqlInsertCandle appends one candle unless it is already stored, and says whether it was new and
// whether the copy already stored differs from this one. The select reads the table as it was
// before the insert, so a new candle never counts as differing.
const sqlInsertCandle = `
	with ins as (
	    insert into candle (instrument_id, granularity_s, at, open, high, low, close, volume, fetched_at)
	    values ($1, $2, $3, $4::numeric, $5::numeric, $6::numeric, $7::numeric, $8::numeric, $9)
	    on conflict (instrument_id, granularity_s, at) do nothing
	    returning 1
	)
	select exists (select 1 from ins),
	       exists (select 1 from candle c
	                where c.instrument_id = $1 and c.granularity_s = $2 and c.at = $3
	                  and (c.open, c.high, c.low, c.close, c.volume)
	                      is distinct from ($4::numeric, $5::numeric, $6::numeric, $7::numeric, $8::numeric))`

// InsertCandles appends candles in one round trip, skipping any already stored: a stored candle
// is never updated. It returns how many were new, and how many were already stored with values
// that differ from these (a sign a candle was stored before Coinbase had finished it).
func (s *Store) InsertCandles(ctx context.Context, cs []Candle) (inserted, differing int, err error) {
	if len(cs) == 0 {
		return 0, 0, nil
	}
	batch := &pgx.Batch{}
	for _, c := range cs {
		batch.Queue(sqlInsertCandle, c.InstrumentID, c.GranularityS, c.At,
			c.Open, c.High, c.Low, c.Close, c.Volume, c.FetchedAt)
	}
	br := s.pool.SendBatch(ctx, batch)
	for range cs {
		var isNew, differs bool
		if err := br.QueryRow().Scan(&isNew, &differs); err != nil {
			_ = br.Close()
			return 0, 0, err
		}
		if isNew {
			inserted++
		}
		if differs {
			differing++
		}
	}
	return inserted, differing, br.Close()
}

// LatestCandle is the start of the newest stored candle of an instrument and granularity; ok is
// false when there is none.
func (s *Store) LatestCandle(ctx context.Context, instrumentID int64, granularityS int) (at time.Time, ok bool, err error) {
	var t *time.Time
	if err := s.pool.QueryRow(ctx,
		`select max(at) from candle where instrument_id = $1 and granularity_s = $2`,
		instrumentID, granularityS).Scan(&t); err != nil {
		return time.Time{}, false, err
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return *t, true, nil
}

// Candles reads the stored candles of an instrument and granularity that start in [from, to),
// oldest first.
func (s *Store) Candles(ctx context.Context, instrumentID int64, granularityS int, from, to time.Time) ([]Candle, error) {
	rows, err := s.pool.Query(ctx, `
		select at, open::text, high::text, low::text, close::text, volume::text, fetched_at
		  from candle
		 where instrument_id = $1 and granularity_s = $2 and at >= $3 and at < $4
		 order by at`, instrumentID, granularityS, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candle
	for rows.Next() {
		c := Candle{InstrumentID: instrumentID, GranularityS: granularityS}
		if err := rows.Scan(&c.At, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume, &c.FetchedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
