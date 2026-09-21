package broker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// PaperModel names the fill rule. It is stored with every order, because what a result means
// depends on how its fills were decided; a different rule is a different name and a new version.
const PaperModel = "paper-1"

// Reasons an order is left unfilled, in whole or in part. Plain text, stored with the order.
const (
	ReasonNoBids       = "no bids on this side"
	ReasonOutsideLimit = "the best price is outside the limit"
	ReasonAlreadyTaken = "already taken by this bucket"
	ReasonNothingMore  = "nothing more displayed within the limit"
	ReasonCeiling      = "the cost ceiling is reached"
	ReasonDust         = "the next level is worth less than a cent"

	ReasonNoClientID  = "the order has no client id"
	ReasonBadOrder    = "the order is not a buy or sell of yes or no"
	ReasonBadQty      = "the quantity is not between 1 and the most an order may be"
	ReasonBadLimit    = "the limit is outside 0.0001..0.9999"
	ReasonBadCeiling  = "the cost ceiling is negative"
	ReasonNoBook      = "no book for this market"
	ReasonStaleBook   = "stale book: the order was decided on a snapshot the broker does not hold"
	ReasonClosed      = "the market has closed"
	ReasonUnreadable  = "this side of the recorded book could not be read"
	ReasonCrossed     = "the recorded book is crossed: the best yes bid and the best no bid add up to more than 1.0000"
	reasonNothingLeft = ""
)

// Paper fills simulated immediate-or-cancel orders from the recorded book of that second, and
// from nothing else. It walks the recorded bid levels best first, never beyond them, in whole
// contracts, and remembers per bucket what it took at each resting price (a "hold"), because a
// paper order removes nothing from the real book and the same displayed contracts must not be
// sold twice. When the display at a price FALLS below what was taken there, the difference is
// not forgotten: it moves down to the next displayed level (see ObserveBook).
//
// Two rules about what it will not fill from, both written out where they are coded:
//
//   - a level with whole contracts left is never skipped for a worse one, and a buy that its cost
//     ceiling cuts short inside a level ends there. A level showing LESS than one whole contract
//     is passed over, as a real sweep would go past it after taking the fraction, and the
//     contracts are booked at the worse price [CONVENTION, see fill].
//   - a CROSSED snapshot (best yes bid + best no bid above 1.0000) is no book a venue can show:
//     it fills nothing and touches no hold [CONVENTION, see Sides].
//
// Levels is how many recorded levels an order may walk: 5 in the service, all that is recorded;
// only brokercheck's parity mode sets 1. Zero or anything above 5 means 5.
//
// FeePerFill rounds every fill's fee up on its own instead of once per order: the pessimistic
// reading of an unknown venue rule (see feeShare). It belongs to the strategy version's frozen
// params; whoever builds the Paper copies it in.
//
// What the paper broker CANNOT know, so that nobody reads its fills as more than they are:
//
//  1. Whether the displayed size was still there when the order would have arrived. The snapshot
//     is up to a second old. paper-1 fills at the displayed price with no guessed slippage; the
//     engine charges a measured staleness cost in its own tests, and the analysis layer re-prices
//     every fill on the next snapshot. A stricter rule (fill only what two consecutive snapshots
//     both show) would be a new model name and a new version.
//  2. Market impact, and anybody else's reaction to our order. The hold rule is a floor on this,
//     not a model of it.
//  3. Anything between snapshots. A level that emptied and refilled within a second stays held,
//     which errs against us. Whether a fall in the display was a cancel or a take: every fall is
//     charged as a take [CONVENTION: the pessimistic reading], which errs against us and will
//     under-fill when market makers merely requote.
//  4. Depth beyond five levels, and hidden or iceberg size if Kalshi has any [ASSUMED none]. A
//     hold displaced past the last shown level is dropped, so against a book deeper than five
//     levels the rule is slightly generous there.
//  5. Kalshi's own rounding of sub-cent fills and of multi-level fees [ASSUMED: see PremiumCents
//     and feeShare].
//  6. Venue rules: size and position limits, rate limits, self-trade prevention, rejects, outages.
//  7. Other buckets. Each bucket is its own paper world, as the analysis layer already assumes.
//     On one real account they would compete for the same contracts and opposite sides would net.
//  8. Adverse selection at real latency. Only simulated and live fills compared on tiny size can
//     close these; nothing in this package can.
//
// A Paper is safe for use from several goroutines. Its one mutex is never held across anything
// but arithmetic.
type Paper struct {
	Levels     int
	FeePerFill bool

	mu      sync.Mutex
	markets map[string]*market // by ticker
	orders  map[string]*record // by client id
}

// NewPaper is a Paper that walks at most levels recorded levels.
func NewPaper(levels int) *Paper { return &Paper{Levels: levels} }

// group is one bucket's view of one ladder. Holds are per bucket: what Scalper took does not
// make Value's book thinner.
type group struct {
	bucket int64
	ladder Ladder
}

// market is everything remembered about one ticker.
type market struct {
	hasBook      bool
	evaluationID int64
	at, closes   time.Time
	sides        Sides

	holds   map[group]map[Price]int // committed: the fills are in the ledger
	pending map[group]map[Price]int // filled by Submit, not yet committed or voided
}

type holdDelta struct {
	g   group
	bid Price
	qty int
}

// record is one order as first answered. A repeated client id gets the same answer.
type record struct {
	ticker    string
	report    Report
	deltas    []holdDelta
	committed bool
}

// Name goes in trade_order.broker.
func (p *Paper) Name() string { return "paper" }

func (p *Paper) levels() int {
	if p.Levels < 1 || p.Levels > RecordedLevels {
		return RecordedLevels
	}
	return p.Levels
}

// market returns the ticker's state, making it if need be. Callers hold p.mu.
func (p *Paper) market(ticker string) *market {
	if p.markets == nil {
		p.markets = map[string]*market{}
	}
	m := p.markets[ticker]
	if m == nil {
		m = &market{holds: map[group]map[Price]int{}, pending: map[group]map[Price]int{}}
		p.markets[ticker] = m
	}
	return m
}

func add(to map[group]map[Price]int, g group, bid Price, qty int) {
	byPrice := to[g]
	if byPrice == nil {
		byPrice = map[Price]int{}
		to[g] = byPrice
	}
	byPrice[bid] += qty
	if byPrice[bid] <= 0 {
		delete(byPrice, bid)
	}
	if len(byPrice) == 0 {
		delete(to, g)
	}
}

// ObserveBook takes this second's recorded book and refreshes every bucket's holds against it:
// the replenishment rule.
//
// Why there is a rule at all: our paper order removed nothing from the real book, so the real
// book keeps showing the contracts we "took". Under price-time priority the resting orders we
// took were at the front of the queue. While the display stays at or above the hold, those same
// orders may still be what is shown, so only the excess is new: available = displayed - hold.
//
// When the display FALLS below the hold, the difference has really gone. It was either cancelled
// or taken by a real taker, and the recording cannot tell which. If it was taken, then in the
// book we had already eaten that taker would have found the level gone and eaten the same number
// from the NEXT level instead. So the difference is moved down, not forgotten: forgetting it
// would hand the bucket the displaced taker's liquidity for nothing, at exactly the moments the
// book is being hit. Treating every fall as a take is a [CONVENTION], the conservative one of
// the two readings; a cancel is charged as if it were a take.
//
// The rule, for each bucket and ladder, going down the prices best first and carrying the
// number of contracts displaced so far:
//
//   - a held price that is listed: the hold becomes min(hold, displayed) and the rest is
//     displaced. If instead the level shows more than is held, it absorbs displaced contracts
//     carried down from BETTER prices, up to its free room.
//   - a held price that is not listed but lies inside the visible range (fewer than five levels
//     are shown, or the price is above the worst shown bid): the book is visible down past it
//     and it is not there, so it displays 0 and the whole hold is displaced.
//   - a held price below the worst of five shown levels cannot be seen: the hold is left alone.
//   - what is still displaced after the last shown level is dropped: it would sit on levels that
//     cannot be seen (item 4 of what Paper cannot know).
//
// A displaced hold is an ordinary hold from then on: it falls, and is displaced again, by the
// same rule. For one bucket and ladder the TOTAL held never rises on an observation, and a
// level's hold rises only by what a better level released. This is the counterfactual book under
// the single assumption that nobody else reacts to us.
//
// A side of the snapshot that cannot be read leaves that side's holds untouched, and orders on
// it are rejected until a readable book arrives. A CROSSED snapshot (see Sides) is unusable on
// both sides: no hold is touched and every order is rejected with ReasonCrossed.
func (p *Paper) ObserveBook(ticker string, evaluationID int64, at, closes time.Time, q kalshi.Quotes) {
	sides := Ladders(q)
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.market(ticker)
	m.hasBook, m.evaluationID, m.at, m.closes, m.sides = true, evaluationID, at, closes, sides
	for g, byPrice := range m.holds {
		shown, err := sides.side(g.ladder)
		if err != nil {
			continue
		}
		refresh(byPrice, m.pending[g], shown)
		if len(byPrice) == 0 {
			delete(m.holds, g)
		}
	}
}

// refresh applies the replenishment rule to one bucket's holds on one ladder, in place. pending
// is normally empty here (the runner commits or voids a step's orders before the next snapshot);
// it is counted against a level's room so that a level can never be held past its display.
func refresh(holds, pending map[Price]int, shown []Level) {
	displayed := make(map[Price]int, len(shown))
	prices := make([]Price, 0, len(shown)+len(holds))
	for _, lv := range shown {
		displayed[lv.Bid] = lv.Size
		prices = append(prices, lv.Bid)
	}
	for bid := range holds {
		if _, listed := displayed[bid]; !listed {
			prices = append(prices, bid)
		}
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i] > prices[j] })

	full := len(shown) >= RecordedLevels
	var worst Price
	if len(shown) > 0 {
		worst = shown[len(shown)-1].Bid
	}
	displaced := 0
	for _, bid := range prices {
		size, listed := displayed[bid]
		if !listed && full && bid < worst {
			continue // below the visible range: it cannot be seen, so it is left alone
		}
		// Not listed inside the visible range means size 0: the whole hold is released.
		hold := holds[bid]
		kept := min(hold, size)
		released := hold - kept
		room := max(0, size-kept-pending[bid])
		moved := min(displaced, room) // room is 0 whenever this level released anything
		displaced += released - moved
		if kept+moved > 0 {
			holds[bid] = kept + moved
		} else {
			delete(holds, bid)
		}
	}
	// Whatever is still displaced here would sit below the last shown level: dropped.
}

// Submit tries the order against the last observed book of its ticker. Every market reason is a
// Report; the only error is a context that was already cancelled, in which case nothing happened.
//
// Nothing it fills is permanent until Commit. Until then the fills are pending holds, which
// already count against the book, so two orders in one step cannot take the same contracts.
func (p *Paper) Submit(ctx context.Context, o Order) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if rec := p.orders[o.ClientID]; rec != nil && o.ClientID != "" {
		return rec.report, nil // the idempotency key: the first answer, again
	}
	rep, deltas := p.fill(o)
	if o.ClientID != "" {
		if p.orders == nil {
			p.orders = map[string]*record{}
		}
		p.orders[o.ClientID] = &record{ticker: o.Ticker, report: rep, deltas: deltas}
		for _, d := range deltas { // deltas exist only when the ticker has a book
			add(p.market(o.Ticker).pending, d.g, d.bid, d.qty)
		}
	}
	return rep, nil
}

// fill decides the order and changes nothing. Callers hold p.mu.
func (p *Paper) fill(o Order) (Report, []holdDelta) {
	rep := Report{Order: o, Model: PaperModel, Final: true, Fills: []Fill{}, Seen: []LevelSeen{}, Unfilled: max(0, o.Qty)}
	reject := func(reason string) (Report, []holdDelta) {
		rep.Status, rep.Reason = Rejected, reason
		return rep, nil
	}
	switch {
	case o.ClientID == "":
		return reject(ReasonNoClientID) // without the key a retry could fill twice
	case (o.Action != Buy && o.Action != Sell) || (o.Side != Yes && o.Side != No):
		return reject(ReasonBadOrder)
	case o.Qty < 1 || o.Qty > MaxQty:
		return reject(ReasonBadQty)
	case o.Limit < 1 || o.Limit > priceScale-1:
		return reject(ReasonBadLimit)
	case o.Action == Buy && o.MaxCostCents < 0:
		return reject(ReasonBadCeiling)
	}
	m := p.markets[o.Ticker]
	switch {
	case m == nil || !m.hasBook:
		return reject(ReasonNoBook)
	case m.evaluationID != o.EvaluationID:
		return reject(ReasonStaleBook)
	case !o.At.Before(m.closes):
		return reject(ReasonClosed)
	}
	ladder := LadderFor(o.Action, o.Side)
	levels, err := m.sides.side(ladder)
	switch {
	case errors.Is(err, errCrossed):
		return reject(ReasonCrossed) // see Sides: a snapshot no venue can show fills nothing
	case err != nil:
		return reject(fmt.Sprintf("%s: %v", ReasonUnreadable, err))
	}
	if len(levels) > p.levels() {
		levels = levels[:p.levels()]
	}

	g := group{o.BucketID, ladder}
	for _, lv := range levels {
		held := m.holds[g][lv.Bid] + m.pending[g][lv.Bid]
		rep.Seen = append(rep.Seen, LevelSeen{Bid: lv.Bid, Displayed: lv.Size, Held: held})
	}

	var (
		deltas      []holdDelta
		remaining   = o.Qty
		premium     int64 // of the fills so far
		fees        int64 // of the fills so far
		numerator   int64 // fee numerators of the fills so far
		withinLimit int   // levels that passed the price test
		blocked     bool  // some level within the limit was held in full by this bucket
		stop        string
	)
	for i, lv := range levels {
		if remaining == 0 {
			break
		}
		taker := TakerPrice(o.Action, lv.Bid)
		if (o.Action == Buy && taker > o.Limit) || (o.Action == Sell && taker < o.Limit) {
			break // the first failing level ends the walk: every later one is worse
		}
		withinLimit++
		held := rep.Seen[i].Held
		switch {
		case held > 0 && held >= lv.Size:
			// This bucket took every whole contract shown here earlier. The level is not
			// untouched: it was taken, and the walk passes over it.
			blocked = true
			continue
		case lv.Size == 0:
			// Less than one whole contract is shown here (0.62, say) and this bucket holds none
			// of it. The walk passes over it and goes on to the next level. [CONVENTION]
			//
			// Why, by the rule "fill what a real sweep would": a real sweep does not stop at
			// such a level. It takes the fraction and goes on to the next bid, so the contracts
			// at the worse levels ARE reached. [ASSUMED: how Kalshi matches an order against a
			// resting fraction has not been measured. If it cannot take the fraction at all
			// there is still nothing here to stop a sweep.] An order here is whole contracts
			// and a level showing under one cannot give one, so the fraction is left out and
			// every contract is booked at the worse price, which errs against the bucket (a
			// sale receives less, a buy pays more). It is the same thing that happens at every
			// fractional level: 23.62 shown gives 23, the 0.62 stays, and the walk goes on.
			// Ending the walk here would instead turn an exit a venue would fill into no fill
			// at all.
			//
			// The plan's sentence "no fill is ever made beyond a level that was left untouched"
			// (docs/honest-fills-v3.md, 2.3, Submit step 3) names only a level held in full as
			// the exception; read to the letter it would end the walk here. The rule it
			// protects is that a level with WHOLE contracts is never skipped for a worse one,
			// and that still holds.
			continue
		}
		avail := lv.Size - held // at least 1: the two cases above are all the ways to have none
		take := min(remaining, avail)
		capped := false // the cost ceiling, not the display, cut this fill short
		if o.Action == Buy {
			if ceiling, has := ceilingAt(o, taker); has {
				within := p.mostWithin(ceiling, take, taker, premium, fees, numerator)
				if within == 0 {
					stop = ReasonCeiling // ceilings only tighten as the price worsens
					break
				}
				capped, take = within < take, within
			}
		}
		own := feeNumerator(take, taker)
		f := Fill{
			Seq: len(rep.Fills) + 1, Level: i, Qty: take, Price: taker,
			PremiumCents: PremiumCents(o.Action, take, taker),
			FeeCents:     feeShare(numerator, own, p.FeePerFill),
		}
		if f.PremiumCents == 0 && f.FeeCents == 0 {
			// A fill row must point at a ledger transfer and a ledger entry may not be zero, so
			// a fill with no cash effect at all cannot be booked. A level is never skipped
			// either (a real sweep cannot reach a worse bid without taking the better one), so
			// the walk ends here.
			stop = ReasonDust
			break
		}
		rep.Fills = append(rep.Fills, f)
		rep.Seen[i].Taken = take
		deltas = append(deltas, holdDelta{g, lv.Bid, take})
		remaining -= take
		premium += f.PremiumCents
		fees += f.FeeCents
		numerator += own
		if capped {
			// Better-priced contracts are still showing at this level, and the ceiling is why
			// they were not taken. A real immediate-or-cancel sweep cannot reach a worse bid
			// past them. Without this the walk could: premium is rounded per fill and the fee
			// once per order, so ONE contract at the next level can come in a cent under the
			// ceiling that one more contract here would break. The walk ends.
			stop = ReasonCeiling
			break
		}
	}

	rep.Unfilled = remaining
	switch {
	case remaining == 0:
		rep.Status, rep.Reason = Filled, reasonNothingLeft
		return rep, deltas
	case len(rep.Fills) > 0:
		rep.Status = Partial
	default:
		rep.Status = Cancelled
	}
	switch {
	case stop != "":
		rep.Reason = stop
	case len(levels) == 0:
		rep.Reason = ReasonNoBids
	case withinLimit == 0:
		rep.Reason = ReasonOutsideLimit
	case len(rep.Fills) == 0 && blocked:
		rep.Reason = ReasonAlreadyTaken
	default:
		rep.Reason = ReasonNothingMore
	}
	return rep, deltas
}

// ceilingAt is the most a buy may have cost once a fill at this taker price is included: the
// smallest of the order's MaxCostCents (if set) and the MaxCostCents of every CostStep whose
// UpTo is at or better than this price. A negative step is a ceiling of nothing.
func ceilingAt(o Order, taker Price) (ceiling int64, has bool) {
	if o.MaxCostCents > 0 {
		ceiling, has = o.MaxCostCents, true
	}
	for _, s := range o.CostSteps {
		if s.UpTo <= taker && (!has || s.MaxCostCents < ceiling) {
			ceiling, has = max(0, s.MaxCostCents), true
		}
	}
	return ceiling, has
}

// mostWithin is the largest n <= take for which the order's premium plus fee so far, this fill
// of n at taker included, stays within the ceiling. The cost never falls as n grows, so a
// binary search finds it exactly, in about thirty exact integer evaluations at most, rather
// than by taking contracts away one at a time.
func (p *Paper) mostWithin(ceiling int64, take int, taker Price, premium, fees, numerator int64) int {
	cost := func(n int) int64 {
		own := feeNumerator(n, taker)
		return premium + PremiumCents(Buy, n, taker) + fees + feeShare(numerator, own, p.FeePerFill)
	}
	if cost(take) <= ceiling {
		return take
	}
	n := sort.Search(take, func(i int) bool { return cost(i+1) > ceiling }) // first i with i+1 too dear
	return n
}

// Commit: these orders' fills are in the ledger, so their holds become permanent. An id that is
// unknown (or already committed) is ignored.
func (p *Paper) Commit(clientIDs ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range clientIDs {
		rec := p.orders[id]
		if rec == nil || rec.committed {
			continue
		}
		rec.committed = true
		if m := p.markets[rec.ticker]; m != nil {
			for _, d := range rec.deltas {
				add(m.pending, d.g, d.bid, -d.qty)
				add(m.holds, d.g, d.bid, d.qty)
			}
		}
	}
}

// Void: the fills were NOT recorded. The pending holds and the stored report are deleted, so
// availability is exactly what it was before the order and the client id may be used again.
// An order that was already committed cannot be voided: its fills are in the ledger, and
// ErrCannotVoid says so (the other ids are still voided). An unknown id is ignored.
func (p *Paper) Void(clientIDs ...string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var err error
	for _, id := range clientIDs {
		rec := p.orders[id]
		if rec == nil {
			continue
		}
		if rec.committed {
			err = fmt.Errorf("%w: order %s is already committed", ErrCannotVoid, id)
			continue
		}
		if m := p.markets[rec.ticker]; m != nil {
			for _, d := range rec.deltas {
				add(m.pending, d.g, d.bid, -d.qty)
			}
		}
		delete(p.orders, id)
	}
	return err
}

// Forget drops everything about a settled market: its book, its holds and its orders' reports.
func (p *Paper) Forget(ticker string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.markets, ticker)
	for id, rec := range p.orders {
		if rec.ticker == ticker {
			delete(p.orders, id)
		}
	}
}

// RestoreHold puts back, after a restart, a hold for one recorded fill: qty contracts this
// bucket took with the given action and side at the given TAKER price. The next ObserveBook
// applies the fall-and-displace rule to it. Releases and displacements from before the restart
// are not remembered, so every contract ever taken starts held at the price it was taken at,
// which errs toward holding too much.
func (p *Paper) RestoreHold(bucketID int64, ticker string, a Action, s Side, taker Price, qty int) {
	if qty < 1 {
		return
	}
	bid := taker
	if a == Buy {
		bid = priceScale - taker
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	add(p.market(ticker).holds, group{bucketID, LadderFor(a, s)}, bid, qty)
}

// Held is what this bucket holds (committed) at one resting bid.
func (p *Paper) Held(bucketID int64, ticker string, l Ladder, bid Price) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.markets[ticker]; m != nil {
		return m.holds[group{bucketID, l}][bid]
	}
	return 0
}

// TotalHeld is what this bucket holds (committed) on one ladder, over all prices.
func (p *Paper) TotalHeld(bucketID int64, ticker string, l Ladder) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	if m := p.markets[ticker]; m != nil {
		for _, qty := range m.holds[group{bucketID, l}] {
			total += qty
		}
	}
	return total
}
