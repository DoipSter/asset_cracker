package main

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/doipster/asset_cracker/service/internal/analysis"
	"github.com/doipster/asset_cracker/service/internal/broker"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// Everything in this file is pure: rows in, figures out. No database, no clock. main.go reads
// the rows; the tests hand-build them.

// Sale is one recorded early sale and the book it is priced against.
//
// Trade is EXACTLY what the analysis layer reads for this order (same columns, same rounding),
// so analysis.PriceSales can be run on it here and must give what GET /api/analysis gives. The
// other fields are what the paper broker needs and the analysis layer never reads: the whole
// recorded snapshot, not only the best bid's size.
type Sale struct {
	analysis.Trade
	MarketID int64
	At       time.Time // trade_order.placed_at
	Limit    string    // trade_order.limit_price as text ("0.09"); "" when the column is null

	// The latest evaluation of the market within 5 s at or before the sale: the same lookup
	// store.AnalysisWindow makes. EvaluationID is 0 when there is none.
	EvaluationID int64
	BookAt       time.Time
	Quotes       kalshi.Quotes
}

// Snapshot is one recorded second of one market's book. Only real mode reads them.
type Snapshot struct {
	ID     int64
	At     time.Time
	Quotes kalshi.Quotes
}

// MarketSales is one settled market with its early sales (any order) and, for real mode, its
// snapshots from the first sale's book to the last sale's, oldest first.
type MarketSales struct {
	ID        int64
	Ticker    string
	Closes    time.Time
	Result    string
	Sales     []Sale
	Snapshots []Snapshot
}

// Outcome is one sale priced three ways: by the analysis layer, by parity mode, and (in real
// mode only) by the broker as the service would run it.
type Outcome struct {
	OrderID, BucketID, MarketID int64
	Qty                         int
	Analysis                    analysis.PricedSale

	// Walked: parity mode sent this sale through the broker's walk, so its parity figure is a
	// second computation that can disagree with PriceSales'. False for a sale made before depth
	// was recorded (never priced) and for a depth-priced pass-through (ParityFilled is Qty by
	// construction, and PriceSales' Beyond is 0 by construction): those agree without anything
	// having been compared.
	Walked       bool
	ParityFilled int
	ParityReason string

	Real             bool // real mode priced this sale: the broker took the order and walked the book
	RealNotRun       bool // real mode could not: the limit was unreadable or the broker rejected the order (listed apart)
	RealFilledBest   int  // at the best recorded bid (level 0)
	RealFilledDeeper int  // at levels 1..4, which neither PriceSales nor parity mode can reach
	RealBucketCents  int64
	RealReason       string
}

// Apart is a sale that did not go through the walk as an ordinary order: the broker rejected it,
// its limit could not be read, or it ended on a level worth less than a cent. None is expected;
// each one is listed so that a parity difference can be explained, not guessed at.
type Apart struct {
	Mode     string `json:"mode"`
	OrderID  int64  `json:"order_id"`
	BucketID int64  `json:"bucket_id"`
	MarketID int64  `json:"market_id"`
	Why      string `json:"why"`
}

// Counters are the oddities real mode met. Each is a count of what happened, not an estimate.
type Counters struct {
	// The sale's book was older than one already observed for that bucket and market (order ids
	// and book times out of step), or was not among the snapshots read. The sale's own book was
	// then observed again so that the order could be priced at all.
	BooksObservedAgain int `json:"books_observed_again"`
	// A recorded limit at or below zero (the bid less a cent, on a one-cent bid) accepts any
	// bid. The broker's smallest limit is 0.0001, so that is what was sent.
	LimitsRaisedToMinimum int `json:"limits_raised_to_minimum"`
	// Placed at or after the market's recorded close. Parity mode prices them anyway, because
	// PriceSales has no such test; real mode's broker rejects them and they are listed apart.
	SalesAtOrAfterClose int `json:"sales_at_or_after_close"`
	// The sale's own recorded book is crossed: its best yes bid and best no bid add up to more
	// than 1.0000, which no matching venue can show (broker.Sides). The broker rejects every
	// order on such a snapshot, in both modes, and each is listed apart. PriceSales has no such
	// test, so in parity mode such a sale shows as a DIFFERENCE, with the reason beside it: a
	// fault in the recording, not in the walk. None is expected.
	SalesOnCrossedBooks int `json:"sales_on_crossed_books"`
}

const (
	modeParity = "parity"
	modeReal   = "real"
)

// minLimit is the lowest limit the broker takes, 0.0001. In parity mode every sale carries it, so
// the limit never binds: PriceSales has no limit test, and parity mode is that rule and no more.
const minLimit broker.Price = 1

// Checker prices markets one at a time and keeps what real mode must carry between them: one
// Paper per bucket for the whole history.
type Checker struct {
	RealMode bool

	Outcomes []Outcome
	Apart    []Apart
	Counters Counters

	real map[int64]*broker.Paper // by bucket id
}

// Market prices one market's early sales.
func (c *Checker) Market(m MarketSales) {
	sells := make([]Sale, 0, len(m.Sales))
	for _, s := range m.Sales {
		if s.Sell && s.Qty > 0 { // PriceSales' own filter
			sells = append(sells, s)
		}
	}
	// PriceSales' own order: by order id, stably. Its answers come back in this order and carry
	// no order id, so the two lists are matched by position.
	sort.SliceStable(sells, func(i, j int) bool { return sells[i].OrderID < sells[j].OrderID })
	trades := make([]analysis.Trade, len(sells))
	for i, s := range sells {
		trades[i] = s.Trade
	}
	priced := analysis.PriceSales(trades, m.Result)

	first := len(c.Outcomes)
	for i, s := range sells {
		c.Outcomes = append(c.Outcomes, Outcome{OrderID: s.OrderID, BucketID: s.BucketID, MarketID: m.ID, Qty: s.Qty, Analysis: priced[i]})
		if !s.At.Before(m.Closes) {
			c.Counters.SalesAtOrAfterClose++
		}
		if s.HasDepth && !s.DepthPriced && broker.Ladders(s.Quotes).Crossed {
			c.Counters.SalesOnCrossedBooks++
		}
	}
	out := c.Outcomes[first:]
	c.parity(m, sells, out)
	if c.RealMode {
		c.realMarket(m, sells, out)
	}
}

// parity is PriceSales' rule, run through the broker: a NEW Paper that walks ONE level for every
// (bucket, second, side), the first sale's book handed to it under a synthetic evaluation id,
// sales in order id order with Commit between them so that they share the displayed size, the
// limit at its minimum and the close set past the sale so that neither test can bind. Two
// implementations of one rule: if they agree on production data, the walk, the flooring and the
// sharing within a second are right.
//
// The plan says "per (bucket, second)"; the side is in the key here because PriceSales takes the
// displayed size from the FIRST sale of each (bucket, second, side), and two sales in one second
// on different sides may have looked up different books. The two sides are different ladders, so
// with one book the result is the same either way.
func (c *Checker) parity(m MarketSales, sells []Sale, out []Outcome) {
	type key struct {
		bucket, second int64
		side           string
	}
	const syntheticEvaluation = 1
	papers := map[key]*broker.Paper{}
	for i, s := range sells {
		o := &out[i]
		switch {
		case s.DepthPriced:
			o.ParityFilled = s.Qty // already capped across the levels when it was made: passed through, as PriceSales does
			continue
		case !s.HasDepth:
			continue // counted, never priced, as in PriceSales
		}
		k := key{s.BucketID, s.Second, s.Side}
		closes := s.At.Add(time.Second)
		p := papers[k]
		if p == nil {
			p = broker.NewPaper(1)
			papers[k] = p
			p.ObserveBook(m.Ticker, syntheticEvaluation, s.At, closes, s.Quotes)
		}
		o.Walked = true
		rep := c.submit(p, modeParity, s.OrderID, broker.Order{
			ClientID: fmt.Sprintf("parity:%d", s.OrderID), BucketID: s.BucketID, MarketID: m.ID, Ticker: m.Ticker,
			Action: broker.Sell, Side: broker.Side(s.Side), Qty: s.Qty, Limit: minLimit,
			EvaluationID: syntheticEvaluation, At: s.At,
		})
		for _, f := range rep.Fills {
			o.ParityFilled += f.Qty
		}
		o.ParityReason = rep.Reason
	}
}

// realMarket is what the service's broker would have done with these sales: one Paper per bucket
// for the whole history, five levels, each sale's true limit, and EVERY recorded snapshot between
// a bucket's first and last sale in the market observed in order, so that holds fall and are
// displaced exactly as they would have been live. It is a report. It is not required to match
// anything.
//
// What it does NOT model: the engine would have kept the contracts that did not fill and tried
// again the next second. The recorded history has no such retries, so only the recorded sales are
// priced. [a limit of this check, not a measurement of the broker]
func (c *Checker) realMarket(m MarketSales, sells []Sale, out []Outcome) {
	byBucket := map[int64][]int{}
	var buckets []int64
	for i, s := range sells {
		if s.DepthPriced || !s.HasDepth {
			continue
		}
		if _, seen := byBucket[s.BucketID]; !seen {
			buckets = append(buckets, s.BucketID)
		}
		byBucket[s.BucketID] = append(byBucket[s.BucketID], i)
	}
	if c.real == nil {
		c.real = map[int64]*broker.Paper{}
	}
	for _, bucket := range buckets {
		p := c.real[bucket]
		if p == nil {
			p = broker.NewPaper(broker.RecordedLevels)
			c.real[bucket] = p
		}
		next, observed := 0, int64(0) // the next snapshot to observe, and the id of the last one observed
		for _, i := range byBucket[bucket] {
			s, o := sells[i], &out[i]
			limit, err := parseLimit(s.Limit)
			if err != nil {
				o.RealNotRun, o.RealReason = true, err.Error()
				c.Apart = append(c.Apart, Apart{modeReal, s.OrderID, s.BucketID, m.ID, err.Error()})
				continue
			}
			if limit < minLimit {
				limit = minLimit
				c.Counters.LimitsRaisedToMinimum++
			}
			for next < len(m.Snapshots) && !after(m.Snapshots[next], s) {
				sn := m.Snapshots[next]
				p.ObserveBook(m.Ticker, sn.ID, sn.At, m.Closes, sn.Quotes)
				next, observed = next+1, sn.ID
			}
			if observed != s.EvaluationID {
				p.ObserveBook(m.Ticker, s.EvaluationID, s.BookAt, m.Closes, s.Quotes)
				observed = s.EvaluationID
				c.Counters.BooksObservedAgain++
			}
			rep := c.submit(p, modeReal, s.OrderID, broker.Order{
				ClientID: fmt.Sprintf("real:%d", s.OrderID), BucketID: s.BucketID, MarketID: m.ID, Ticker: m.Ticker,
				Action: broker.Sell, Side: broker.Side(s.Side), Qty: s.Qty, Limit: limit,
				EvaluationID: s.EvaluationID, At: s.At,
			})
			o.RealReason = rep.Reason
			if rep.Status == "" || rep.Status == broker.Rejected {
				// Never walked (submit listed it apart). Its contracts are not "unfilled": the
				// book was never asked for them.
				o.RealNotRun = true
				continue
			}
			o.Real = true
			for _, f := range rep.Fills {
				if f.Level == 0 {
					o.RealFilledBest += f.Qty
				} else {
					o.RealFilledDeeper += f.Qty
				}
				o.RealBucketCents += broker.BucketCents(broker.Sell, f)
			}
		}
		p.Forget(m.Ticker) // a settled market's holds are never needed again
	}
}

// after says whether a snapshot is later than the book a sale was decided on, by (time, id).
func after(sn Snapshot, s Sale) bool {
	if !sn.At.Equal(s.BookAt) {
		return sn.At.After(s.BookAt)
	}
	return sn.ID > s.EvaluationID
}

// submit sends one order and commits it at once: a recorded sale is in the ledger already.
func (c *Checker) submit(p *broker.Paper, mode string, orderID int64, o broker.Order) broker.Report {
	rep, err := p.Submit(context.Background(), o) // Paper's only error is a cancelled context
	if err != nil {
		c.Apart = append(c.Apart, Apart{mode, orderID, o.BucketID, o.MarketID, err.Error()})
		return broker.Report{}
	}
	p.Commit(o.ClientID)
	if rep.Status == broker.Rejected || rep.Reason == broker.ReasonDust {
		c.Apart = append(c.Apart, Apart{mode, orderID, o.BucketID, o.MarketID, string(rep.Status) + ": " + rep.Reason})
	}
	return rep
}

// parseLimit reads trade_order.limit_price ("0.09") as exact ten-thousandths of a dollar.
// big.Rat, not ParseFloat: 0.09 has no exact float, and the broker's prices are integers.
func parseLimit(s string) (broker.Price, error) {
	if s == "" {
		return 0, fmt.Errorf("the order has no limit price")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("limit price %q is not a number", s)
	}
	r.Mul(r, big.NewRat(10000, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, fmt.Errorf("limit price %q is finer than a hundredth of a cent", s)
	}
	if r.Num().Int64() > 9999 {
		return 0, fmt.Errorf("limit price %q is a dollar or more", s)
	}
	return broker.Price(r.Num().Int64()), nil
}

// ---- which markets are read ----

// Complete keeps the markets the analysis layer would aggregate, and says why each of the others
// is left out. GET /api/analysis leaves a WHOLE window (one closes_at, all coins) out when any
// market in it is unsettled, or does not reconcile with its settlement rows, or has an order
// with no recorded money (analysis.Build, web/analysis.go compute). The per-strategy counts can
// only be compared if this check leaves out the same windows.
func Complete(markets []analysis.Market, unready map[int64]string, openWindows map[int64]bool) (kept []analysis.Market, leftOut map[int64]string) {
	leftOut = map[int64]string{}
	bad := map[int64]bool{}
	for w := range openWindows {
		bad[w] = true
	}
	for _, m := range markets {
		if _, held := unready[m.ID]; held {
			bad[m.Closes] = true
		}
	}
	for _, m := range markets {
		switch why, held := unready[m.ID]; {
		case held:
			leftOut[m.ID] = why
		case openWindows[m.Closes]:
			leftOut[m.ID] = "another market of its window has no result yet"
		case bad[m.Closes]:
			leftOut[m.ID] = "another market of its window is held back"
		default:
			kept = append(kept, m)
		}
	}
	return kept, leftOut
}

// ---- the report ----

// Row is one strategy version's early sales. The first block is the analysis layer's figures.
// Its sells_priced, sells_without_depth, contracts_sold and contracts_beyond_bid must equal the
// same strategy's row in GET /api/analysis ("fills"."by_strategy") when that document's coverage
// says complete. Its sells is NOT the API's sells: the API adds unsettled_sells to that figure
// (early sales in rounds with no result yet, analysis.buildFills), and only settled, kept
// windows are read here, so sells here is the API's sells less its unsettled_sells. The second
// block is parity mode, the third real mode.
type Row struct {
	VersionID int64  `json:"strategy_version_id"`
	Strategy  string `json:"strategy"`
	Engine    string `json:"engine"`
	World     string `json:"world"`

	Sells              int `json:"sells"`
	SellsPriced        int `json:"sells_priced"`
	SellsWithoutDepth  int `json:"sells_without_depth"`
	ContractsSold      int `json:"contracts_sold"`
	ContractsBeyondBid int `json:"contracts_beyond_bid"` // analysis.PriceSales

	SalesCompared   int  `json:"sales_compared"` // of sells_priced, the sales the broker walked; the rest are depth-priced pass-throughs, equal by construction
	ParityFilled    int  `json:"parity_filled"`
	ParityBeyondBid int  `json:"parity_beyond_bid"` // must equal contracts_beyond_bid
	ParityAgrees    bool `json:"parity_agrees"`     // the two beyond figures are equal; with sales_compared 0 that says nothing

	// Real mode only; all 0 in parity mode. real_filled_best can never pass parity_filled (a
	// hold only ever takes away from the best bid). real_filled_deeper is contracts the five
	// level walk found below the best bid, which the other two rules cannot see, so the total
	// CAN pass parity_filled on a thin top level.
	RealFilledBest   int `json:"real_filled_best"`
	RealFilledDeeper int `json:"real_filled_deeper"`
	// Contracts of the sales the broker WALKED that the recorded book could not take within the
	// sale's limit. The engine would have kept them and tried again; no retry is modelled.
	RealUnfilled int `json:"real_unfilled"`
	// Contracts of sales the broker never walked: it rejected the order (placed at or after the
	// close, a crossed book) or the recorded limit could not be read. Each is listed apart. They
	// are in neither real_filled_* nor real_unfilled: the book was never asked for them.
	RealNotRun int `json:"real_not_run"`
	// Sale PROCEEDS of what real mode filled: premium less fee, priced at the recorded BIDS. Not
	// profit (what the contracts cost is not in it), and not comparable with the payouts v2
	// booked: v2 booked every sale a cent under the bid, and in full whatever the book showed.
	RealBucketCents int64 `json:"real_bucket_cents"`
}

// Mismatch is one sale on which parity mode and PriceSales disagree.
type Mismatch struct {
	OrderID      int64  `json:"order_id"`
	BucketID     int64  `json:"bucket_id"`
	MarketID     int64  `json:"market_id"`
	Qty          int    `json:"qty"`
	AnalysisFill int    `json:"analysis_filled"`
	ParityFill   int    `json:"parity_filled"`
	ParityReason string `json:"parity_reason"`
}

// Summary is the per-strategy table and the verdict of the parity gate.
type Summary struct {
	ByStrategy []Row      `json:"by_strategy"`
	Mismatches []Mismatch `json:"mismatches"`
	// ParityOK: no sale and no strategy row differs. It is also true when NOTHING was compared,
	// so it is never read alone: the gate is ParityOK with SalesCompared above zero (Gate).
	ParityOK bool `json:"parity_ok"`
	// SalesCompared is the sales the broker actually walked and whose result was compared with
	// PriceSales'. Sales made before depth was recorded, depth-priced pass-throughs and sales of
	// unlisted buckets are not in it: they cannot disagree, so they are no evidence of agreement.
	SalesCompared   int `json:"sales_compared"`
	UnlistedBuckets int `json:"sales_of_unlisted_buckets"` // buildFills skips these too
}

// Exit statuses of the parity gate.
const (
	exitAgree           = 0
	exitDiffer          = 1
	exitNothingCompared = 3 // 2 is a failure to run at all (a bad flag, the database)
)

// Gate is the exit status of the S2 gate. A pass needs a measurement: at least one sale walked
// and compared, and none differing. An empty comparison (no settled market in the range, every
// window left out, every sale made before depth was recorded) is NOT a pass. Real mode gates
// nothing and is always 0.
func (s Summary) Gate(mode string) int {
	switch {
	case mode != modeParity:
		return exitAgree
	case !s.ParityOK:
		return exitDiffer
	case s.SalesCompared == 0:
		return exitNothingCompared
	}
	return exitAgree
}

// Summarise groups the outcomes by strategy version, exactly as analysis.buildFills does: a sale
// before depth was recorded is counted and never priced; a bucket that is not a sim bucket of the
// kalshi15m family is skipped.
//
// The gate is per SALE, which is stronger than the plan's per-strategy equality: totals could
// agree by two errors cancelling.
func Summarise(outcomes []Outcome, buckets []analysis.Bucket) Summary {
	byBucket := map[int64]analysis.Bucket{}
	for _, b := range buckets {
		byBucket[b.ID] = b
	}
	rows := map[int64]*Row{}
	sum := Summary{ByStrategy: []Row{}, Mismatches: []Mismatch{}, ParityOK: true}
	for _, o := range outcomes {
		b, ok := byBucket[o.BucketID]
		if !ok {
			sum.UnlistedBuckets++
			continue
		}
		r := rows[b.VersionID]
		if r == nil {
			r = &Row{VersionID: b.VersionID, Strategy: b.Strategy, Engine: b.Engine, World: b.World}
			rows[b.VersionID] = r
		}
		r.Sells++
		if o.Analysis.WithoutDepth {
			r.SellsWithoutDepth++
			continue
		}
		r.SellsPriced++
		if o.Walked {
			r.SalesCompared++
			sum.SalesCompared++
		}
		r.ContractsSold += o.Qty
		r.ContractsBeyondBid += o.Analysis.Beyond
		r.ParityFilled += o.ParityFilled
		r.ParityBeyondBid += o.Qty - o.ParityFilled
		if o.Qty-o.ParityFilled != o.Analysis.Beyond {
			sum.ParityOK = false
			sum.Mismatches = append(sum.Mismatches, Mismatch{o.OrderID, o.BucketID, o.MarketID, o.Qty, o.Qty - o.Analysis.Beyond, o.ParityFilled, o.ParityReason})
		}
		if o.Real {
			r.RealFilledBest += o.RealFilledBest
			r.RealFilledDeeper += o.RealFilledDeeper
			r.RealUnfilled += o.Qty - o.RealFilledBest - o.RealFilledDeeper
			r.RealBucketCents += o.RealBucketCents
		}
		if o.RealNotRun {
			r.RealNotRun += o.Qty
		}
	}
	ids := make([]int64, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		r := *rows[id]
		r.ParityAgrees = r.ParityBeyondBid == r.ContractsBeyondBid
		sum.ByStrategy = append(sum.ByStrategy, r)
	}
	for _, r := range sum.ByStrategy {
		if !r.ParityAgrees {
			sum.ParityOK = false
		}
	}
	return sum
}
