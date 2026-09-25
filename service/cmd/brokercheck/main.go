// Command brokercheck re-prices the early sales the second engine RECORDED through the paper
// broker (service/internal/broker) and compares the result with the analysis layer's own
// re-pricing of the same sales. It only reads. Simulated money only; nothing here can place an
// order, and nothing here writes to the database.
//
//	AC_DATABASE_URL=... go run ./cmd/brokercheck -mode parity [-since 2026-09-21T00:00:00Z] [-until ...] [-json out.json]
//	AC_DATABASE_URL=... go run ./cmd/brokercheck -mode real   [-json out.json]
//
// Two modes, two questions (docs/honest-fills-v3.md, section 8).
//
// -mode parity: is the walk right? analysis.PriceSales fills an early sale with the size displayed
// at the best bid, shared by one bucket's sales on one side within one second, no limit test,
// the book taken from the latest evaluation within 5 s at or before the sale. Parity mode is that
// rule and nothing more, run through the broker: a new Paper of one level per (bucket, second,
// side). Its contracts beyond the bid must equal PriceSales' EXACTLY, sale by sale and so per
// strategy. That is the gate of step S2: exit status 0 when they agree on at least one sale, 1
// when they do not, 3 when NOTHING was compared (no settled market in the range, every window
// left out, or every sale made before depth was recorded): an empty comparison is not a pass.
// sales_compared counts the sales the broker actually walked; a sale made before depth was
// recorded and a depth-priced pass-through agree by construction and are not counted. (2 is a
// failure to run at all: a bad flag, or the database.)
//
// The analysis figures printed here are computed in this process from the same rows. Four of
// them must in turn equal the same strategy's row of "fills"."by_strategy" in GET /api/analysis
// once that document's coverage says complete: sells_priced, sells_without_depth, contracts_sold
// and contracts_beyond_bid. The sells column is different: the API's sells also counts
// unsettled_sells (early sales in rounds with no result yet, which this check does not read and
// which coverage.complete does not wait for), so this table's sells equals the API's sells LESS
// its unsettled_sells. Comparing the two is the second half of the gate and is done by eye or
// with jq; the API side of it is
//
//	jq '.fills.by_strategy[] | {strategy, engine, world, sells: (.sells - .unsettled_sells), sells_priced, sells_without_depth, contracts_sold, contracts_beyond_bid}'
//
// -mode real: what would the broker, as the service will run it, have filled? One Paper per
// bucket for the whole history, holds and displacement carried across seconds, five levels, each
// sale's recorded limit. REPORTED only: it gates nothing and its exit status is 0 whatever it
// finds. At the best bid it can only fill less than parity (the second lot of a sale split over
// consecutive seconds finds the first lot's hold); below the best bid it can find contracts the
// other two rules never look at, so both are printed.
//
// Reading only: every statement runs inside its own transaction whose first statement is
// "set transaction read only", so a bug here cannot write even under a role that may. Every read
// of a partitioned table (evaluation) carries constant time bounds, so the planner prunes to the
// partitions of the window being read. It is meant to run as the read-only role
// (assetcracker_ro), which needs select on market, instrument, bucket, strategy_version,
// strategy, trade_order, fill, settlement and evaluation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/doipster/asset_cracker/service/internal/analysis"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// Document is what -json writes. No slice in it is ever null.
type Document struct {
	Simulated  bool   `json:"simulated"` // always true
	Mode       string `json:"mode"`
	What       string `json:"what"`
	ComputedAt string `json:"computed_at"`
	Since      string `json:"since"` // markets that closed at or after this
	Until      string `json:"until"` // and before this

	MarketsSettled int       `json:"markets_settled"` // with a result, in the range
	MarketsRead    int       `json:"markets_read"`    // of those, in windows the analysis layer would aggregate
	LeftOut        []LeftOut `json:"left_out"`        // the others, and why

	Summary
	Apart    []Apart  `json:"apart"`
	Counters Counters `json:"counters"`
}

// LeftOut is a settled market whose window the analysis layer leaves out, so this check does too.
type LeftOut struct {
	MarketID int64  `json:"market_id"`
	Why      string `json:"why"`
}

const (
	whatParity = "SIMULATED money. Early sales recorded by the older engines, re-priced twice from the same rows: by analysis.PriceSales (contracts_beyond_bid) and by the paper broker in parity mode (parity_beyond_bid: one level, a fresh book per bucket, second and side, no limit). The two must be equal for every sale. They are two implementations of one rule; agreement says the broker's walk, flooring and sharing within a second are right, and says nothing about whether the rule is a good model of the venue."
	whatReal   = whatParity + " The real_* columns are the broker as the service will run it (five levels, holds carried across seconds, the recorded limit). They are a report and gate nothing. The engine's retries of what did not fill are not in the recorded history and are not modelled. " + whatRealCents + " " + whatRealUnfilled

	// The two real-mode figures that are easy to misread. Printed under the table and carried in
	// the JSON's "what".
	whatRealCents    = "real_bucket_cents (\"real proceeds c (at bid)\") is sale PROCEEDS: premium less fee of the contracts real mode filled, priced at the recorded bids. It is not profit or loss, because what the contracts cost is not in it, and it is not comparable with the payouts the second engine booked, because that engine booked every sale a cent under the bid and in full whatever the book showed."
	whatRealUnfilled = "real_unfilled is contracts of the sales the broker walked that the recorded book could not take within the sale's limit; the engine would have kept them and tried again. real_not_run is contracts of sales the broker never walked (it rejected the order, or the recorded limit could not be read; each is listed apart): they are in neither the filled nor the unfilled figure."
)

func main() {
	mode := flag.String("mode", "", "parity (the gate) or real (a report)")
	sinceFlag := flag.String("since", "", "only markets that closed at or after this (RFC 3339); default: the first settled market's close, read from the database")
	untilFlag := flag.String("until", "", "only markets that closed before this (RFC 3339); default: now")
	jsonPath := flag.String("json", "", "also write the whole report as JSON to this file (- for standard output, in place of the table)")
	flag.Parse()
	if *mode != modeParity && *mode != modeReal {
		fmt.Fprintln(os.Stderr, "brokercheck: -mode must be parity or real")
		flag.Usage()
		os.Exit(2)
	}
	// Both time flags are read before anything is connected to, so a bad flag is reported as a
	// bad flag and never as a connection error, and never costs the database a connection.
	now := time.Now().UTC()
	since, until, err := parseRange(*sinceFlag, *untilFlag, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "brokercheck:", err)
		os.Exit(2)
	}
	url := os.Getenv("AC_DATABASE_URL")
	if url == "" {
		url = "postgres:///assetcracker?host=/var/run/postgresql" // the service's own default (config.go)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	doc, err := run(ctx, url, *mode, now, since, until)
	if err != nil {
		fmt.Fprintln(os.Stderr, "brokercheck:", err)
		os.Exit(2)
	}
	if *jsonPath == "-" {
		err = writeJSON(os.Stdout, doc)
	} else {
		printTable(os.Stdout, doc)
		if *jsonPath != "" {
			var f *os.File
			if f, err = os.Create(*jsonPath); err == nil {
				err = errors.Join(writeJSON(f, doc), f.Close())
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "brokercheck:", err)
		os.Exit(2)
	}
	status := doc.Gate(*mode)
	if status == exitNothingCompared {
		// On standard error as well, so that it is seen when the table is not printed (-json -).
		fmt.Fprintln(os.Stderr, "brokercheck: PARITY: NOTHING COMPARED. No sale went through the broker's walk, so the gate has not passed. Exit status 3.")
	}
	if status != exitAgree {
		os.Exit(status)
	}
}

// parseRange reads -since and -until. until defaults to now. since is the zero time when the
// flag is absent: run then reads the first settled market's close from the database. A range
// that is empty by its flags alone is refused here: it could only compare nothing.
func parseRange(sinceArg, untilArg string, now time.Time) (since, until time.Time, err error) {
	until = now
	if untilArg != "" {
		if until, err = time.Parse(time.RFC3339, untilArg); err != nil {
			return since, until, fmt.Errorf("-until: %w", err)
		}
	}
	if sinceArg != "" {
		if since, err = time.Parse(time.RFC3339, sinceArg); err != nil {
			return since, until, fmt.Errorf("-since: %w", err)
		}
		if !since.Before(until) {
			return since, until, fmt.Errorf("-since %s is not before -until %s: no market can lie in that range",
				since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
		}
	}
	return since, until, nil
}

func writeJSON(w io.Writer, doc Document) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// ---- reading ----

// reader runs every statement in its own read-only transaction.
type reader struct{ conn *pgx.Conn }

// readOnly runs fn inside a transaction whose first statement is "set transaction read only".
// From then on Postgres refuses every write in it, whatever the role may do. It always rolls
// back: there is nothing to keep.
func (r reader) readOnly(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, sqlReadOnly); err != nil {
		return fmt.Errorf("set transaction read only: %w", err)
	}
	return fn(tx)
}

// The SQL, in one place so that it can be listed and run by hand. None of it has been run.
const (
	sqlReadOnly = `set transaction read only`

	// The close of the first settled market: the default lower bound, read and not guessed.
	// market is not partitioned. The 15-minute series only (store.FifteenMinuteSeries): the
	// sales this check re-prices are the rounds', and the analysis' fills are the rounds' alone.
	// The ladder legs the analysis also reads (its leaderboard) are held to settlement and have
	// no sale to compare.
	sqlFirstClose = `select min(m.closes_at) from market m join instrument i on i.id = m.instrument_id
		 where m.result in ('yes', 'no') and m.closes_at is not null and ` + store.FifteenMinuteSeries

	// The 15-minute part of store.AnalysisMarkets' listing, with the ticker and an upper bound.
	sqlMarkets = `
		select m.id, m.ticker, i.underlying, m.closes_at, m.result
		  from market m join instrument i on i.id = m.instrument_id
		 where m.result in ('yes', 'no') and m.closes_at is not null and m.closes_at >= $1 and m.closes_at < $2
		   and ` + store.FifteenMinuteSeries + `
		 order by m.closes_at, m.id`

	// The 15-minute part of store.AnalysisOpenWindows' rule, word for word, bounded to the range
	// ($1 = now).
	sqlOpenWindows = `
		select distinct extract(epoch from m.closes_at)::bigint
		  from market m join instrument i on i.id = m.instrument_id
		 where m.result is null and m.closes_at < $1
		   and (m.closes_at > $1 - interval '1 hour'
		        or exists (select 1 from trade_order o where o.market_id = m.id))
		   and ` + store.FifteenMinuteSeries + `
		   and m.closes_at >= $2 and m.closes_at < $3`

	// The kalshi15m part of store.AnalysisBuckets' listing, without the ledger account.
	sqlBuckets = `
		select b.id, b.strategy_version_id, st.name, v.version, coalesce((v.params->>'anti')::boolean, false)
		  from bucket b join strategy_version v on v.id = b.strategy_version_id join strategy st on st.id = v.strategy_id
		 where b.mode = 'sim' and st.family = 'kalshi15m' order by b.id`

	// store.AnalysisWindow's first query with the same columns, the same 5 s book lookup and the
	// same bounds ($2, $3 = the window's close less 20 minutes and plus one), and four more
	// columns the broker needs: the limit, and the evaluation's id, time and WHOLE quotes.
	// $4, $5 are constants around the window so that evaluation's partitions prune at plan time;
	// they are wider than any book the lateral can pick ($2 - 5 s .. $3), so they change nothing.
	sqlTrades = `
		select o.market_id, o.id, o.bucket_id, o.action = 'sell', o.side, f.qty, extract(epoch from o.placed_at)::float8,
		       coalesce((o.detail->>'cost')::float8, 0), coalesce((o.detail->>'payout')::float8, 0),
		       (o.detail->>'cost') is null or (o.action = 'sell' and (o.detail->>'payout') is null),
		       coalesce(jsonb_typeof(book.bids) = 'array', false), coalesce((book.bids->0->>1)::float8, 0),
		       coalesce(o.detail->>'v' = '3', false),
		       o.placed_at, o.limit_price::text, book.id, book.at, book.quotes::text
		  from trade_order o
		  join lateral (select sum(x.qty)::float8 as qty from fill x where x.order_id = o.id) f on f.qty > 0
		  left join lateral (
			select case o.side when 'yes' then e.quotes->'yes_bids' else e.quotes->'no_bids' end as bids, e.id, e.at, e.quotes
			  from evaluation e
			 where o.action = 'sell' and e.market_id = o.market_id and e.at <= o.placed_at and e.at >= o.placed_at - interval '5 seconds'
			   and e.at >= $4 and e.at < $5
			 order by e.at desc limit 1) book on true
		 where o.market_id = any($1) and o.placed_at >= $2 and o.placed_at < $3
		 order by o.id`

	// store.AnalysisWindow's second query, unchanged. settlement is not partitioned.
	sqlSettlements = `select market_id, bucket_id, side, sum(qty)::bigint, sum(payout_cents)::bigint
	                    from settlement where market_id = any($1) group by 1, 2, 3`

	// Real mode only: every recorded second of one market between its first sale's book and its
	// last sale's, oldest first. Walks evaluation's (market_id, at) index inside one partition
	// (two at a month's end).
	sqlSnapshots = `select e.id, e.at, e.quotes::text from evaluation e
	                 where e.market_id = $1 and e.at >= $2 and e.at <= $3 order by e.at, e.id`
)

// run reads and prices. since is the zero time when -since was not given.
func run(ctx context.Context, url, mode string, now, since, until time.Time) (Document, error) {
	var none Document
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return none, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	r := reader{conn}

	if since.IsZero() {
		var first *time.Time
		if err := r.readOnly(ctx, func(tx pgx.Tx) error { return tx.QueryRow(ctx, sqlFirstClose).Scan(&first) }); err != nil {
			return none, fmt.Errorf("first settled market: %w", err)
		}
		since = until // no settled market at all: an empty range
		if first != nil {
			since = *first
		}
	}

	doc := Document{Simulated: true, Mode: mode, What: whatParity, ComputedAt: now.Format(time.RFC3339),
		Since: since.UTC().Format(time.RFC3339), Until: until.UTC().Format(time.RFC3339), LeftOut: []LeftOut{}}
	if mode == modeReal {
		doc.What = whatReal
	}

	// The open windows are listed BEFORE the settled markets, as web/analysis.go compute does.
	// Each statement sees its own moment. If a result lands between the two, this order leaves
	// its window marked open, so the whole window is left out and said so. The other order would
	// keep the window with that coin missing: not in the settled list (read too early) and no
	// longer an open window (it has its result), and its sales would silently go uncounted.
	open, err := r.openWindows(ctx, now, since, until)
	if err != nil {
		return none, fmt.Errorf("open windows: %w", err)
	}
	markets, tickers, err := r.markets(ctx, since, until)
	if err != nil {
		return none, fmt.Errorf("markets: %w", err)
	}
	buckets, err := r.buckets(ctx)
	if err != nil {
		return none, fmt.Errorf("buckets: %w", err)
	}

	// Read window by window, as the analysis layer does, oldest first.
	byWindow := map[int64][]analysis.Market{}
	var windows []int64
	for _, m := range markets {
		if _, seen := byWindow[m.Closes]; !seen {
			windows = append(windows, m.Closes)
		}
		byWindow[m.Closes] = append(byWindow[m.Closes], m)
	}
	check := Checker{RealMode: mode == modeReal}
	for _, w := range windows {
		sales, unready, err := r.window(ctx, byWindow[w])
		if err != nil {
			return none, fmt.Errorf("window closing %s: %w", time.Unix(w, 0).UTC().Format(time.RFC3339), err)
		}
		kept, leftOut := Complete(byWindow[w], unready, open)
		for _, m := range byWindow[w] {
			if why, out := leftOut[m.ID]; out {
				doc.LeftOut = append(doc.LeftOut, LeftOut{m.ID, why})
			}
		}
		for _, m := range kept {
			ms := MarketSales{ID: m.ID, Ticker: tickers[m.ID], Closes: time.Unix(m.Closes, 0), Result: m.Result, Sales: sales[m.ID]}
			if check.RealMode {
				if ms.Snapshots, err = r.snapshots(ctx, ms); err != nil {
					return none, fmt.Errorf("snapshots of market %d: %w", m.ID, err)
				}
			}
			check.Market(ms)
			doc.MarketsRead++
		}
	}
	doc.MarketsSettled = len(markets)
	doc.Summary = Summarise(check.Outcomes, buckets)
	doc.Apart, doc.Counters = check.Apart, check.Counters
	if doc.Apart == nil {
		doc.Apart = []Apart{}
	}
	return doc, nil
}

func (r reader) markets(ctx context.Context, since, until time.Time) (out []analysis.Market, tickers map[int64]string, err error) {
	tickers = map[int64]string{}
	err = r.readOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sqlMarkets, since, until)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m analysis.Market
			var ticker string
			var closes time.Time
			if err := rows.Scan(&m.ID, &ticker, &m.Coin, &closes, &m.Result); err != nil {
				return err
			}
			m.Closes = closes.Unix()
			tickers[m.ID] = ticker
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, tickers, err
}

func (r reader) openWindows(ctx context.Context, now, since, until time.Time) (map[int64]bool, error) {
	out := map[int64]bool{}
	err := r.readOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sqlOpenWindows, now, since, until)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var w int64
			if err := rows.Scan(&w); err != nil {
				return err
			}
			out[w] = true
		}
		return rows.Err()
	})
	return out, err
}

// buckets labels each bucket as store.AnalysisBuckets does: a twin is reported under its
// original's name, world "anti".
func (r reader) buckets(ctx context.Context) ([]analysis.Bucket, error) {
	var out []analysis.Bucket
	err := r.readOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sqlBuckets)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b analysis.Bucket
			var version int
			var anti bool
			if err := rows.Scan(&b.ID, &b.VersionID, &b.Strategy, &version, &anti); err != nil {
				return err
			}
			b.Engine, b.World = fmt.Sprintf("v%d", version), "real"
			if anti {
				b.World, b.Strategy = "anti", strings.TrimPrefix(b.Strategy, "Anti ")
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// window reads one window's orders and settlement rows and applies the analysis layer's own two
// tests (MoneyMissing, Reconcile) to say which of its markets are not ready. It returns the
// early sales of every market, ready or not; the caller drops the window if any is not.
func (r reader) window(ctx context.Context, markets []analysis.Market) (sales map[int64][]Sale, unready map[int64]string, err error) {
	ids := make([]int64, len(markets))
	for i, m := range markets {
		ids[i] = m.ID
	}
	closes := time.Unix(markets[0].Closes, 0)
	from, to := closes.Add(-20*time.Minute), closes.Add(time.Minute) // AnalysisWindow's bounds
	trades := map[int64][]analysis.Trade{}
	sales = map[int64][]Sale{}
	settled := map[int64]map[analysis.Holding]analysis.Paid{}
	err = r.readOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sqlTrades, ids, from, to, from.Add(-time.Minute), to.Add(time.Minute))
		if err != nil {
			return fmt.Errorf("fills: %w", err)
		}
		for rows.Next() {
			var s Sale
			var qty, at, cost, payout float64
			var limit, quotes *string
			var evaluation *int64
			var bookAt *time.Time
			if err := rows.Scan(&s.MarketID, &s.OrderID, &s.BucketID, &s.Sell, &s.Side, &qty, &at, &cost, &payout, &s.MoneyMissing,
				&s.HasDepth, &s.Displayed, &s.DepthPriced, &s.At, &limit, &evaluation, &bookAt, &quotes); err != nil {
				rows.Close()
				return err
			}
			// The same four conversions as store.AnalysisWindow, so the Trade is the same Trade.
			s.Qty, s.Second, s.CostCents, s.PayoutCents = int(math.Round(qty)), int64(math.Floor(at)), int64(math.Round(cost*100)), int64(math.Round(payout*100))
			trades[s.MarketID] = append(trades[s.MarketID], s.Trade)
			if !s.Sell {
				continue
			}
			if limit != nil {
				s.Limit = *limit
			}
			if evaluation != nil && bookAt != nil && quotes != nil {
				s.EvaluationID, s.BookAt = *evaluation, *bookAt
				if err := json.Unmarshal([]byte(*quotes), &s.Quotes); err != nil {
					rows.Close()
					return fmt.Errorf("quotes of evaluation %d: %w", *evaluation, err)
				}
			}
			sales[s.MarketID] = append(sales[s.MarketID], s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("fills: %w", err)
		}

		rows, err = tx.Query(ctx, sqlSettlements, ids)
		if err != nil {
			return fmt.Errorf("settlements: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var market, qty int64
			var k analysis.Holding
			var p analysis.Paid
			if err := rows.Scan(&market, &k.BucketID, &k.Side, &qty, &p.PayoutCents); err != nil {
				return err
			}
			p.Qty = int(qty)
			if settled[market] == nil {
				settled[market] = map[analysis.Holding]analysis.Paid{}
			}
			settled[market][k] = p
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}
	unready = map[int64]string{}
	for _, m := range markets {
		if orders := analysis.MoneyMissing(trades[m.ID]); len(orders) > 0 {
			unready[m.ID] = fmt.Sprintf("orders %v have no recorded cost or payout", orders)
		} else if bad := analysis.Reconcile(trades[m.ID], settled[m.ID]); len(bad) > 0 {
			why := make([]string, len(bad))
			for i, b := range bad {
				why[i] = fmt.Sprintf("bucket %d held %d %s at the close and its settlement rows cover %d", b.BucketID, b.Held, b.Side, b.Settled)
			}
			unready[m.ID] = strings.Join(why, "; ")
		}
	}
	return sales, unready, nil
}

// snapshots reads a market's recorded seconds from its first sale's book to its last sale's.
// A market with no priced sale needs none, and none is read.
func (r reader) snapshots(ctx context.Context, m MarketSales) ([]Snapshot, error) {
	var from, to time.Time
	for _, s := range m.Sales {
		if !s.HasDepth || s.DepthPriced || s.EvaluationID == 0 {
			continue
		}
		if from.IsZero() || s.BookAt.Before(from) {
			from = s.BookAt
		}
		if s.BookAt.After(to) {
			to = s.BookAt
		}
	}
	if from.IsZero() {
		return nil, nil
	}
	var out []Snapshot
	err := r.readOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sqlSnapshots, m.ID, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sn Snapshot
			var quotes string
			if err := rows.Scan(&sn.ID, &sn.At, &quotes); err != nil {
				return err
			}
			var q kalshi.Quotes
			if err := json.Unmarshal([]byte(quotes), &q); err != nil {
				return fmt.Errorf("quotes of evaluation %d: %w", sn.ID, err)
			}
			sn.Quotes = q
			out = append(out, sn)
		}
		return rows.Err()
	})
	return out, err
}

// ---- printing ----

func printTable(w io.Writer, doc Document) {
	fmt.Fprintf(w, "brokercheck -mode %s   SIMULATED money, recorded sales re-priced, nothing written\n", doc.Mode)
	fmt.Fprintf(w, "markets closed %s .. %s: %d settled, %d read, %d left out with their windows\n\n",
		doc.Since, doc.Until, doc.MarketsSettled, doc.MarketsRead, len(doc.LeftOut))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	head := "strategy\tengine\tworld\tsells\tpriced\tno depth\tcompared\tsold\tbeyond (analysis)\tbeyond (parity)\tagrees\t"
	if doc.Mode == modeReal {
		head += "real at best\treal deeper\treal unfilled\treal not run\treal proceeds c (at bid)\t"
	}
	fmt.Fprintln(tw, head)
	rows := append([]Row{}, doc.ByStrategy...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ContractsBeyondBid > rows[j].ContractsBeyondBid })
	for _, r := range rows {
		agrees := "yes"
		switch {
		case !r.ParityAgrees:
			agrees = "NO"
		case r.SalesCompared == 0:
			agrees = "nothing compared" // 0 = 0 is not agreement
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t", r.Strategy, r.Engine, r.World, r.Sells, r.SellsPriced,
			r.SellsWithoutDepth, r.SalesCompared, r.ContractsSold, r.ContractsBeyondBid, r.ParityBeyondBid, agrees)
		if doc.Mode == modeReal {
			line += fmt.Sprintf("%d\t%d\t%d\t%d\t%d\t", r.RealFilledBest, r.RealFilledDeeper, r.RealUnfilled, r.RealNotRun, r.RealBucketCents)
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	fmt.Fprintln(w)
	switch {
	case doc.ParityOK && doc.SalesCompared == 0:
		fmt.Fprintln(w, "PARITY: NOTHING COMPARED. No sale in this range went through the broker's walk: no settled market, every window left out, or every sale made before depth was recorded (see the priced, no depth and compared columns). This is NOT a pass; in parity mode the exit status is 3.")
	case doc.ParityOK:
		fmt.Fprintf(w, "PARITY: the broker's one-level walk and analysis.PriceSales agree on all %d sales compared.\n", doc.SalesCompared)
	default:
		fmt.Fprintf(w, "PARITY FAILS: %d of the %d sales compared differ.\n", len(doc.Mismatches), doc.SalesCompared)
		for i, m := range doc.Mismatches {
			if i == 20 {
				fmt.Fprintf(w, "  ... and %d more (all of them are in the JSON)\n", len(doc.Mismatches)-i)
				break
			}
			fmt.Fprintf(w, "  order %d (bucket %d, market %d) sold %d: analysis fills %d, parity fills %d (%s)\n",
				m.OrderID, m.BucketID, m.MarketID, m.Qty, m.AnalysisFill, m.ParityFill, m.ParityReason)
		}
	}
	fmt.Fprintln(w, "compared: of the priced sales, those the broker walked. The others were priced by the depth-aware broker when they were made and are passed through, so they agree by construction and are no evidence.")
	fmt.Fprintln(w, "Next: priced, no depth, sold and beyond (analysis) (in the JSON: sells_priced, sells_without_depth, contracts_sold, contracts_beyond_bid) must also equal the same strategy's row in fills.by_strategy of GET /api/analysis, once its coverage.complete is true. The sells column is the exception: the API's sells also counts unsettled_sells (early sales in rounds with no result yet, which this check does not read), so compare this table's sells with the API's sells MINUS its unsettled_sells.")
	if doc.Mode == modeReal {
		fmt.Fprintln(w, "\nThe real columns are a report and gate nothing. Retries of what did not fill are not modelled.")
		fmt.Fprintln(w, whatRealCents)
		fmt.Fprintln(w, whatRealUnfilled)
	}
	if n := len(doc.Apart); n > 0 {
		fmt.Fprintf(w, "\n%d sales listed apart (rejected by the broker, an unreadable limit, or a level worth less than a cent):\n", n)
		for i, a := range doc.Apart {
			if i == 20 {
				fmt.Fprintf(w, "  ... and %d more (all of them are in the JSON)\n", n-i)
				break
			}
			fmt.Fprintf(w, "  %s: order %d (bucket %d, market %d): %s\n", a.Mode, a.OrderID, a.BucketID, a.MarketID, a.Why)
		}
	}
	c := doc.Counters
	if c.BooksObservedAgain+c.LimitsRaisedToMinimum+c.SalesAtOrAfterClose+c.SalesOnCrossedBooks+doc.UnlistedBuckets > 0 {
		fmt.Fprintf(w, "\ncounted: %d books observed again, %d limits raised to 0.0001, %d sales at or after the close, %d sales on a crossed book (rejected by the broker), %d sales of buckets not listed\n",
			c.BooksObservedAgain, c.LimitsRaisedToMinimum, c.SalesAtOrAfterClose, c.SalesOnCrossedBooks, doc.UnlistedBuckets)
	}
	for i, l := range doc.LeftOut {
		if i == 0 {
			fmt.Fprintln(w, "\nleft out, as the analysis layer leaves them out:")
		}
		if i == 10 {
			fmt.Fprintf(w, "  ... and %d more (all of them are in the JSON)\n", len(doc.LeftOut)-i)
			break
		}
		fmt.Fprintf(w, "  market %d: %s\n", l.MarketID, l.Why)
	}
}
