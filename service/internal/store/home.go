package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Everything in this file serves the home page. One function appends (value snapshots, which
// are a record of what things were marked at, not money); the rest only read.

// ValueSnapshot is one row of value_snapshot: what one scope was worth at one moment.
type ValueSnapshot struct {
	At               time.Time
	Scope, Key       string
	ValueCents       int64 // cash plus open bets at the bid; a bet with no bid counts 0
	CashCents        int64
	AtRiskCents      int64 // open bets at cost
	ContributedCents int64 // net money put in that trading did not earn
	Unmarked         int   // open bets that had no bid
}

// EarnedBetween is what was earned between two snapshots of the same scope: the change in value
// that is not explained by money being put in or taken out. A restake or a top-up raises value
// and contributed together and earns nothing; a sustainment allocation lowers both and loses
// nothing.
func EarnedBetween(then, now ValueSnapshot) int64 {
	return (now.ValueCents - now.ContributedCents) - (then.ValueCents - then.ContributedCents)
}

// InsertSnapshots appends a batch of snapshots, all or nothing.
func (s *Store) InsertSnapshots(ctx context.Context, rows []ValueSnapshot) error {
	if len(rows) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(`insert into value_snapshot (at, mode, scope, key, value_cents, cash_cents, at_risk_cents, contributed_cents, unmarked)
		             values ($1, 'sim', $2, $3, $4, $5, $6, $7, $8)`,
			r.At, r.Scope, r.Key, r.ValueCents, r.CashCents, r.AtRiskCents, r.ContributedCents, r.Unmarked)
	}
	return s.pool.SendBatch(ctx, batch).Close()
}

const snapshotColumns = `at, scope, key, value_cents, cash_cents, at_risk_cents, contributed_cents, unmarked`

func scanSnapshot(row pgx.Row) (ValueSnapshot, bool, error) {
	var v ValueSnapshot
	err := row.Scan(&v.At, &v.Scope, &v.Key, &v.ValueCents, &v.CashCents, &v.AtRiskCents, &v.ContributedCents, &v.Unmarked)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

// SnapshotAt is the newest snapshot of a scope taken at or before t; ok is false if there is none.
func (s *Store) SnapshotAt(ctx context.Context, scope, key string, t time.Time) (ValueSnapshot, bool, error) {
	return scanSnapshot(s.pool.QueryRow(ctx, `select `+snapshotColumns+` from value_snapshot
		 where mode = 'sim' and scope = $1 and key = $2 and at <= $3 order by at desc limit 1`, scope, key, t))
}

// FirstSnapshot is the oldest snapshot of a scope; ok is false before any has been written.
func (s *Store) FirstSnapshot(ctx context.Context, scope, key string) (ValueSnapshot, bool, error) {
	return scanSnapshot(s.pool.QueryRow(ctx, `select `+snapshotColumns+` from value_snapshot
		 where mode = 'sim' and scope = $1 and key = $2 order by at limit 1`, scope, key))
}

// SnapshotsTakenAt is every snapshot of one scope written in the same batch, by key. A batch
// shares one timestamp, so the groups can be compared at exactly the moment the total was.
func (s *Store) SnapshotsTakenAt(ctx context.Context, scope string, at time.Time) (map[string]ValueSnapshot, error) {
	rows, err := s.pool.Query(ctx, `select `+snapshotColumns+` from value_snapshot
		 where mode = 'sim' and scope = $1 and at = $2`, scope, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ValueSnapshot{}
	for rows.Next() {
		var v ValueSnapshot
		if err := rows.Scan(&v.At, &v.Scope, &v.Key, &v.ValueCents, &v.CashCents, &v.AtRiskCents, &v.ContributedCents, &v.Unmarked); err != nil {
			return nil, err
		}
		out[v.Key] = v
	}
	return out, rows.Err()
}

// BinSeconds is how wide each bin must be for a span to come back as at most maxPoints points.
// Bins are aligned to the epoch, not to the span, so a span can touch one more bin than it is
// bins long: hence maxPoints-1. Never narrower than a minute, which is how often rows are written.
func BinSeconds(span time.Duration, maxPoints int) int64 {
	if maxPoints < 2 {
		maxPoints = 2
	}
	w := int64(math.Ceil(span.Seconds() / float64(maxPoints-1)))
	if w < 60 {
		w = 60
	}
	return w
}

// SnapshotSeries is a scope's value since a moment, oldest first, as [unix seconds, cents], thinned
// in the database to at most maxPoints: the span is cut into equal bins and each bin gives its
// LAST snapshot, so every point is a value that was really recorded, not an average.
func (s *Store) SnapshotSeries(ctx context.Context, scope, key string, since time.Time, maxPoints int) ([][2]int64, error) {
	rows, err := s.pool.Query(ctx, `
		select (array_agg(extract(epoch from at)::bigint order by at desc))[1],
		       (array_agg(value_cents order by at desc))[1]
		  from value_snapshot
		 where mode = 'sim' and scope = $1 and key = $2 and at >= $3
		 group by floor(extract(epoch from at) / $4::float8)
		 order by 1`, scope, key, since, float64(BinSeconds(time.Since(since), maxPoints)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := [][2]int64{}
	for rows.Next() {
		var p [2]int64
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if len(out) > maxPoints { // cannot happen while BinSeconds is right; the page's bound is kept regardless
		out = out[len(out)-maxPoints:]
	}
	return out, rows.Err()
}

// BucketCapital is one bucket's place in the balance sheet, live or frozen.
type BucketCapital struct {
	ID               int64
	Name, Status     string
	Version          int   // the strategy version's number: 1 is the first engine, 2 the second
	Anti             bool  // an anti-world twin
	ContributedCents int64 // seeds in, less what was reaped, less the sustainment allocation taken
}

// Capital is what the value snapshots need from the ledger that the engines do not carry: where
// the set-aside money sits, what the owners have put in, and each bucket's net contribution.
// It changes only when a bucket is seeded, reaped or has an allocation taken, so it is read
// then and cached, not read every minute.
type Capital struct {
	Money   MoneyBuckets // Money.External is everything put in from outside, to date
	Buckets []BucketCapital
}

// ReadCapital reads it from the ledger.
func (s *Store) ReadCapital(ctx context.Context) (Capital, error) {
	var c Capital
	var err error
	if c.Money, err = s.MoneyBucketBalances(ctx); err != nil {
		return c, err
	}
	// Fills, fees and settlements are trading. Everything else that touches a bucket's cash
	// (seed, reap, sustainment, and the older 'tax' and 'take') is money moved in or out of it.
	rows, err := s.pool.Query(ctx, `
		select b.id, b.name, b.status, v.version, coalesce((v.params->>'anti')::boolean, false),
		       coalesce(sum(e.amount_cents) filter (where t.reason not in ('fill', 'fee', 'settlement')), 0)::bigint
		  from bucket b
		  join strategy_version v on v.id = b.strategy_version_id
		  left join ledger_entry e on e.account_id = b.ledger_account_id
		  left join ledger_transfer t on t.id = e.transfer_id
		 where b.mode = 'sim'
		 group by b.id, v.id
		 order by b.id`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var b BucketCapital
		if err := rows.Scan(&b.ID, &b.Name, &b.Status, &b.Version, &b.Anti, &b.ContributedCents); err != nil {
			return c, err
		}
		c.Buckets = append(c.Buckets, b)
	}
	return c, rows.Err()
}

// BucketRow is one bucket as the buckets page lists it: what the database knows. What it is
// worth right now comes from the engine's book, not from here.
type BucketRow struct {
	ID             int64
	Name, Status   string
	Strategy       string // the strategy's registered name, e.g. "Scalper" or "Anti Scalper"
	Version        int
	Anti           bool
	Life           int   // 1, or N for "<name> life N"
	CashCents      int64 // the ledger's balance for the bucket
	SeedCents      int64
	AllocatedCents int64 // the sustainment allocation taken from it to date
	Bets           int64 // simulated buys filled
}

// LifeOf reads a bucket's life number from its name: "<name> life N" is life N, anything else 1.
func LifeOf(name string) int {
	if i := strings.LastIndex(name, " life "); i >= 0 {
		if n, err := strconv.Atoi(name[i+len(" life "):]); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// Buckets lists every simulated bucket, live ones first, each in the order it was created.
func (s *Store) Buckets(ctx context.Context) ([]BucketRow, error) {
	rows, err := s.pool.Query(ctx, `
		select b.id, b.name, b.status, st.name, v.version, coalesce((v.params->>'anti')::boolean, false),
		       coalesce((select sum(e.amount_cents) from ledger_entry e where e.account_id = b.ledger_account_id), 0)::bigint,
		       coalesce((select sum(e.amount_cents) from ledger_entry e join ledger_transfer t on t.id = e.transfer_id
		                  where e.account_id = b.ledger_account_id and t.reason = 'seed'), 0)::bigint,
		       coalesce((select sum(k.winnings_cents + k.replenish_cents + k.tax_cents + k.fees_cents)
		                   from bucket_skim k where k.bucket_id = b.id), 0)::bigint,
		       (select count(*) from trade_order o where o.bucket_id = b.id and o.action = 'buy')
		  from bucket b
		  join strategy_version v on v.id = b.strategy_version_id
		  join strategy st on st.id = v.strategy_id
		 where b.mode = 'sim'
		 order by (b.status = 'frozen'), b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BucketRow{}
	for rows.Next() {
		var b BucketRow
		if err := rows.Scan(&b.ID, &b.Name, &b.Status, &b.Strategy, &b.Version, &b.Anti, &b.CashCents, &b.SeedCents, &b.AllocatedCents, &b.Bets); err != nil {
			return nil, err
		}
		b.Life = LifeOf(b.Name)
		out = append(out, b)
	}
	return out, rows.Err()
}

// BucketEventRow is one thing that happened to a bucket.
type BucketEventRow struct {
	At     time.Time `json:"at"`
	Bucket string    `json:"bucket"`
	Kind   string    `json:"kind"`
	Note   string    `json:"note"`
}

// EventNote puts a bucket event's stored detail into words. The detail is whatever the writer
// recorded: a note, the cents a bucket was seeded with, or where an allocation went.
func EventNote(kind string, detail []byte) string {
	var d map[string]any
	if json.Unmarshal(detail, &d) != nil {
		return ""
	}
	if note, ok := d["note"].(string); ok {
		return note
	}
	dollars := func(key string) string {
		c, _ := d[key].(float64)
		return fmt.Sprintf("$%.2f", c/100)
	}
	switch {
	case d["cents"] != nil:
		return "seeded with " + dollars("cents")
	case kind == "allocated" || kind == "taxed":
		return fmt.Sprintf("winnings %s, replenishment %s, tax reserve %s, fee reserve %s",
			dollars("winnings"), dollars("replenishment"), dollars("tax_reserve"), dollars("fee_reserve"))
	}
	return ""
}

// RecentBucketEvents lists the newest bucket events, newest first.
func (s *Store) RecentBucketEvents(ctx context.Context, limit int) ([]BucketEventRow, error) {
	rows, err := s.pool.Query(ctx, `
		select e.at, b.name, e.kind, e.detail
		  from bucket_event e join bucket b on b.id = e.bucket_id
		 where b.mode = 'sim' order by e.id desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BucketEventRow{}
	for rows.Next() {
		var e BucketEventRow
		var detail []byte
		if err := rows.Scan(&e.At, &e.Bucket, &e.Kind, &detail); err != nil {
			return nil, err
		}
		e.Note = EventNote(e.Kind, detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RealisedByCoin is what was realised on each coin, in cents, on rounds that SETTLED at or after
// `since`: everything those rounds paid the buckets (settlements and early sales) less everything
// the buckets paid for them, fees included. A round still open, or closed and not yet settled, is
// not in it. Keyed by the instrument's underlying, e.g. "BTC".
func (s *Store) RealisedByCoin(ctx context.Context, since time.Time) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `
		with flow as (
		    select o.market_id, e.amount_cents as cents
		      from fill f
		      join trade_order o on o.id = f.order_id
		      join ledger_entry e on e.transfer_id = f.transfer_id
		      join ledger_account a on a.id = e.account_id and a.kind = 'bucket' and a.mode = 'sim'
		    union all
		    select x.market_id, x.payout_cents
		      from settlement x join bucket b on b.id = x.bucket_id and b.mode = 'sim'
		)
		select i.underlying, sum(flow.cents)::bigint
		  from flow
		  join market m on m.id = flow.market_id
		  join instrument i on i.id = m.instrument_id
		 where m.settled_at is not null and m.settled_at >= $1
		 group by i.underlying`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var coin string
		var cents int64
		if err := rows.Scan(&coin, &cents); err != nil {
			return nil, err
		}
		out[coin] = cents
	}
	return out, rows.Err()
}
