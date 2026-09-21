// Package broker is the one door an engine sends orders through, and the rule that decides what
// an order is filled with.
//
// NOTHING IN THIS PACKAGE CAN PLACE A REAL ORDER. It has no network code, no credentials and no
// venue client. The only implementation is Paper, which fills simulated orders from the book the
// service recorded for that second. The interface is shaped so that a live broker could stand
// behind it one day (that is why Report has Final and Void may fail), but none is written here
// and none is proposed.
//
// Why it exists: the second engine books a sale at any size, whatever the book displayed. The
// 2026-09-21 review found a simulated sale of 348 contracts against a displayed bid of 8. A
// result that rests on fills nobody could have had is not a result. Paper fills only what the
// recorded book showed, remembers what each bucket already took, and says plainly what it still
// cannot know (see the comment on Paper).
//
// The package is pure: no store, no clock, no goroutines. Time comes in on the order and on the
// book, so the same code can later replay recorded history. It imports service/internal/kalshi
// for the Quotes type and nothing else of ours.
//
// Design and worked examples: docs/honest-fills-v3.md, section 2.
package broker

import (
	"context"
	"errors"
	"time"

	"github.com/doipster/asset_cracker/service/internal/kalshi"
)

// Price is ten-thousandths of a dollar, the unit kalshi/quotes.go uses, so a YES ask is exactly
// 10000 - a NO bid. Sub-cent prices are real (a bid of 0.0990 is in the recorded books).
type Price int64

// Action is what the order does with the contract.
type Action string

// Side is the contract bought or sold.
type Side string

const (
	Buy  Action = "buy"
	Sell Action = "sell"

	Yes Side = "yes"
	No  Side = "no"
)

// Order is all an engine may say to a broker. Every order is immediate-or-cancel: nothing rests,
// because a resting order needs a place in the queue and a one-second snapshot cannot give one.
type Order struct {
	ClientID     string // "v3:<bucket id>:<evaluation id>:<n>", unique for ever: the idempotency key
	BucketID     int64
	MarketID     int64
	Ticker       string
	Action       Action
	Side         Side
	Qty          int        // whole contracts, >= 1
	Limit        Price      // buy: most to pay per contract. sell: least to accept
	MaxCostCents int64      // buys only, 0 = none: premium plus fee never passes this
	CostSteps    []CostStep // buys only, may be empty: a ceiling that tightens as the walk reaches worse prices
	EvaluationID int64      // the snapshot the decision was made on
	At           time.Time
}

// CostStep: once a fill at taker price UpTo or worse is included, the order's premium plus fee SO
// FAR may not pass MaxCostCents. The engine sends them sorted by rising UpTo with falling
// MaxCostCents; Paper does not depend on the order, it takes the smallest ceiling that applies.
//
// Why: an order sized for the best ask may walk to worse prices, where the edge is smaller and
// the right stake with it. One venue order cannot say this. A live broker would have to send one
// immediate-or-cancel order per step, each with the remaining ceiling. [ASSUMED, plan section 14]
//
// Unlike Order.MaxCostCents, a step's MaxCostCents of 0 is a real ceiling of nothing: the stake
// formula reaches zero at the limit price, and that must stop the walk, not lift the ceiling.
type CostStep struct {
	UpTo         Price
	MaxCostCents int64
}

// Fill is one level's worth of an order. One fill becomes one ledger transfer.
type Fill struct {
	Seq          int   // 1.. within the order
	Level        int   // the recorded level it came from, 0 = best
	Qty          int   // whole contracts
	Price        Price // the taker's price per contract
	PremiumCents int64 // rounded against the bucket (see PremiumCents)
	FeeCents     int64 // this fill's share of the order's fee
}

// Status is how the order ended.
type Status string

const (
	Filled    Status = "filled"
	Partial   Status = "partial"
	Cancelled Status = "cancelled" // reached the book, nothing filled
	Rejected  Status = "rejected"  // never reached a book
)

// LevelSeen is what the broker saw at one level when the order arrived: the evidence kept with
// the order, so a fill can be checked against the recording afterwards.
type LevelSeen struct {
	Bid       Price // the resting bid, on the ladder the order drew from
	Displayed int   // whole contracts shown
	Held      int   // already taken at this price by this bucket, before this order
	Taken     int   // taken by this order
}

// Report is the broker's whole answer to one order.
type Report struct {
	Order    Order
	Status   Status
	Fills    []Fill // never nil, so it is stored as [] and not null
	Unfilled int
	Reason   string      // why anything is unfilled; empty when filled
	Model    string      // "paper-1"; stored with the order, part of what a result means
	Seen     []LevelSeen // never nil
	Final    bool        // always true for Paper; a live broker could answer false and finish later
}

// ErrCannotVoid is what Void returns when the fills cannot be taken back. A live broker always
// returns it: a real fill that could not be recorded is a halt, not an undo. Paper returns it
// only for an order that was already committed.
var ErrCannotVoid = errors.New("broker: the fills cannot be taken back")

// Broker is the order side.
type Broker interface {
	// Name goes in trade_order.broker: "paper".
	Name() string
	// Submit tries the order now. A market reason (no bids, outside the limit, already taken)
	// is a Report, never an error.
	Submit(ctx context.Context, o Order) (Report, error)
	// Commit: these orders' fills are in the ledger. Paper makes its holds permanent.
	Commit(clientIDs ...string)
	// Void: the fills were NOT recorded; forget them. A live broker cannot un-fill: it must
	// return ErrCannotVoid, and the runner then suspends and reconciles from the venue. Written
	// down here so the failure path is coded once.
	Void(clientIDs ...string) error
}

// BookObserver is the market-data side, kept apart because a real venue has its own book. The
// runner calls it once per snapshot, BEFORE the engine decides, so the holds are refreshed
// against the very book the decision is made on.
type BookObserver interface {
	ObserveBook(ticker string, evaluationID int64, at, closes time.Time, q kalshi.Quotes)
}

var (
	_ Broker       = (*Paper)(nil)
	_ BookObserver = (*Paper)(nil)
)
