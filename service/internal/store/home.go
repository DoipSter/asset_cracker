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

// SnapshotAt is the newest FULLY PRICED snapshot of a scope taken at or before t; ok is false if
// there is none. A range's earnings are measured from this row, and a row written while a bet
// had no bid counts that bet as 0: measured from it, the whole stake would read as earned a
// minute later. The writer already refuses the known case (a round closed and not yet settled);
// skipping every row with unmarked > 0 is the second line of defence, and it also passes over
// honest rows where a losing side had no bid late in a round. The cost is that the row can be
// a few minutes older than t; whoever reports the range reports this row's own time.
func (s *Store) SnapshotAt(ctx context.Context, scope, key string, t time.Time) (ValueSnapshot, bool, error) {
	return scanSnapshot(s.pool.QueryRow(ctx, `select `+snapshotColumns+` from value_snapshot
		 where mode = 'sim' and scope = $1 and key = $2 and at <= $3 and unmarked = 0 order by at desc limit 1`, scope, key, t))
}

// FirstSnapshot is the oldest fully priced snapshot of a scope (see SnapshotAt); ok is false
// before any has been written.
func (s *Store) FirstSnapshot(ctx context.Context, scope, key string) (ValueSnapshot, bool, error) {
	return scanSnapshot(s.pool.QueryRow(ctx, `select `+snapshotColumns+` from value_snapshot
		 where mode = 'sim' and scope = $1 and key = $2 and unmarked = 0 order by at limit 1`, scope, key))
}

// SnapshotsTakenAt is the snapshots of one scope written in the same batch, by key. A batch
// shares one timestamp, so the groups can be compared at exactly the moment the total was. The
// keys are named so that each is one probe of the (mode, scope, key, at) index: without them
// the index's `at` column cannot be used and every row of the scope is walked.
func (s *Store) SnapshotsTakenAt(ctx context.Context, scope string, keys []string, at time.Time) (map[string]ValueSnapshot, error) {
	if keys == nil {
		keys = []string{} // a nil slice is sent as NULL, and "= any(NULL)" matches nothing silently
	}
	rows, err := s.pool.Query(ctx, `select `+snapshotColumns+` from value_snapshot
		 where mode = 'sim' and scope = $1 and key = any($2::text[]) and at = $3`, scope, keys, at)
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
	// CashCents is the ledger's balance, read ONLY for a live bucket that no running engine holds
	// (a series switched to recording only, or the second engine switched off); 0 for every other
	// bucket. A held bucket's cash comes from its engine's book and moves with every fill, which
	// is exactly why it is not read here. An unheld bucket's cash cannot move, so this stays exact.
	CashCents int64
}

// Capital is what the value snapshots need from the ledger that the engines do not carry: where
// the set-aside money sits, what the owners have put in, and each bucket's net contribution.
// It changes only when a bucket is seeded, reaped or has an allocation taken, so it is read
// then and cached, not read every minute.
type Capital struct {
	Money   MoneyBuckets // Money.External is everything put in from outside, to date
	Buckets []BucketCapital
	ReadAt  time.Time // when this was read from the ledger; zero means it never has been
}

// ReadCapital reads it from the ledger. `held` is the ids of the buckets some running engine
// holds in memory (see BucketCapitals).
func (s *Store) ReadCapital(ctx context.Context, held []int64) (Capital, error) {
	var c Capital
	var err error
	if c.Money, err = s.MoneyBucketBalances(ctx); err != nil {
		return c, err
	}
	if c.Buckets, err = s.BucketCapitals(ctx, held); err != nil {
		return c, err
	}
	c.ReadAt = time.Now()
	return c, nil
}

// BucketCapitals is every simulated bucket's net contribution, and the ledger cash of the live
// ones that no engine holds. `held` is the ids of the buckets some running engine holds.
//
// Fills, fees and settlements are trading. Everything else that touches a bucket's cash (seed,
// reap, sustainment, and the older 'tax' and 'take') is money moved in or out of it. The query
// starts from those few transfers, through the partial index ledger_transfer_capital, whose
// predicate the where clause below must match word for word: starting from the buckets' entries
// instead would read every fill ever made, under the engine's lock. The cash column is a full
// sum of one account's entries, which is why it is taken only for unheld buckets: their entries
// stopped growing when their engine was switched off.
func (s *Store) BucketCapitals(ctx context.Context, held []int64) ([]BucketCapital, error) {
	if held == nil {
		held = []int64{} // a nil slice is sent as NULL, and "<> all(NULL)" is never true: every bucket would read as held
	}
	rows, err := s.pool.Query(ctx, `
		select b.id, b.name, b.status, v.version, coalesce((v.params->>'anti')::boolean, false),
		       coalesce(c.cents, 0)::bigint,
		       (case when b.status <> 'frozen' and b.id <> all($1::bigint[])
		             then coalesce((select sum(e.amount_cents) from ledger_entry e where e.account_id = b.ledger_account_id), 0)
		             else 0 end)::bigint
		  from bucket b
		  join strategy_version v on v.id = b.strategy_version_id
		  left join (
		        select e.account_id, sum(e.amount_cents) as cents
		          from ledger_transfer t
		          join ledger_entry e on e.transfer_id = t.id
		         where t.reason not in ('fill', 'fee', 'settlement')
		         group by e.account_id
		       ) c on c.account_id = b.ledger_account_id
		 where b.mode = 'sim'
		 order by b.id`, held)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BucketCapital{}
	for rows.Next() {
		var b BucketCapital
		if err := rows.Scan(&b.ID, &b.Name, &b.Status, &b.Version, &b.Anti, &b.ContributedCents, &b.CashCents); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
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
//
// Its bet count is the buy orders that FILLED something. A buy that was cancelled with no fill
// bought nothing and is not a bet; one that was partly filled is one bet. The first two engines
// fill every order whole, so for them this is every buy order, as it always was.
func (s *Store) Buckets(ctx context.Context) ([]BucketRow, error) {
	rows, err := s.pool.Query(ctx, `
		select b.id, b.name, b.status, st.name, v.version, coalesce((v.params->>'anti')::boolean, false),
		       coalesce((select sum(e.amount_cents) from ledger_entry e where e.account_id = b.ledger_account_id), 0)::bigint,
		       coalesce((select sum(e.amount_cents) from ledger_entry e join ledger_transfer t on t.id = e.transfer_id
		                  where e.account_id = b.ledger_account_id and t.reason = 'seed'), 0)::bigint,
		       coalesce((select sum(k.winnings_cents + k.replenish_cents + k.tax_cents + k.fees_cents)
		                   from bucket_skim k where k.bucket_id = b.id), 0)::bigint,
		       (select count(*) from trade_order o where o.bucket_id = b.id and o.action = 'buy'
		           and exists (select 1 from fill f where f.order_id = o.id))
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
	// dollars reads the first of the keys that the detail really has; ok is false when it has none.
	dollars := func(keys ...string) (string, bool) {
		for _, key := range keys {
			if c, ok := d[key].(float64); ok {
				return fmt.Sprintf("$%.2f", c/100), true
			}
		}
		return "", false
	}
	switch {
	case d["cents"] != nil:
		if seed, ok := dollars("cents"); ok {
			return "seeded with " + seed
		}
	case kind == "allocated" || kind == "taxed":
		// 'taxed' rows were written before the rename, with the reserves under "tax" and "fees".
		// A part the detail does not have is left out: $0.00 would be a figure nobody recorded.
		var parts []string
		for _, part := range []struct {
			label string
			keys  []string
		}{{"winnings", []string{"winnings"}}, {"replenishment", []string{"replenishment"}},
			{"tax reserve", []string{"tax_reserve", "tax"}}, {"fee reserve", []string{"fee_reserve", "fees"}}} {
			if amount, ok := dollars(part.keys...); ok {
				parts = append(parts, part.label+" "+amount)
			}
		}
		return strings.Join(parts, ", ")
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
//
// A round counts only once every bucket that still held contracts in it has its settlement row.
// market.settled_at is written before the engines book their payouts, each in its own later
// transaction, so for a moment a round has its stakes in the ledger and not its winnings: read
// then, it would show as a loss of the whole stake. Both engines write one settlement row per
// bucket and side that held to the end, losers included (payout 0), and a position sold early
// nets to no contracts and needs none. A round an engine never booked (it was halted) stays out
// for good, which is honest: its payouts are not in the ledger either.
//
// Contracts held are the FILL rows added up (bought less sold), never trade_order.qty, and no
// order is picked by its status: a partly filled order counts for what it filled, and one that
// filled nothing has no fill row and counts for nothing. The money is the ledger entries of each
// fill's own transfer, which is why every fill must have a transfer of its own.
//
// The query starts from the few markets settled in the range and reaches their orders through
// trade_order (market_id), so a short range does not read every fill ever made.
func (s *Store) RealisedByCoin(ctx context.Context, since time.Time) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `
		with settled as materialized (
		    select m.id, i.underlying
		      from market m
		      join instrument i on i.id = m.instrument_id
		     where m.settled_at >= $1
		       and not exists (
		           select 1
		             from trade_order o
		             join bucket b on b.id = o.bucket_id and b.mode = 'sim'
		             join fill f on f.order_id = o.id
		            where o.market_id = m.id
		              and not exists (select 1 from settlement x
		                               where x.market_id = o.market_id and x.bucket_id = o.bucket_id and x.side = o.side)
		            group by o.bucket_id, o.side
		           having sum(case when o.action = 'buy' then f.qty else -f.qty end) > 0)
		),
		flow as (
		    select s.underlying, e.amount_cents as cents
		      from settled s
		      join trade_order o on o.market_id = s.id
		      join fill f on f.order_id = o.id
		      join ledger_entry e on e.transfer_id = f.transfer_id
		      join ledger_account a on a.id = e.account_id and a.kind = 'bucket' and a.mode = 'sim'
		    union all
		    select s.underlying, x.payout_cents
		      from settled s
		      join settlement x on x.market_id = s.id
		      join bucket b on b.id = x.bucket_id and b.mode = 'sim'
		)
		select underlying, sum(cents)::bigint from flow group by underlying`, since)
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
