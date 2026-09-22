package engine

import (
	"fmt"

	"github.com/doipster/asset_cracker/service/internal/broker"
)

// Position is what one account holds on one side of one market. Kalshi keeps one net position
// per market and side, so this is an AVERAGE cost basis, not a list of lots [CONVENTION].
type Position struct {
	Coin, Ticker, Side string
	MarketID           int64
	Close, Strike      float64
	Contracts          int
	CostCents          int64 // premium plus fees of what is still held
	// PremiumCents is the premium alone of what is still held, released on a sale by the same rule
	// as CostCents. It is here for one reason: the capture rule measures "the way from the average
	// ENTRY PRICE to a dollar", and an entry price has no fee in it.
	PremiumCents       int64
	FirstAt, LastBuyAt float64
	Entries            int    // buy orders with a fill, toward max_bets
	Exiting            string // "" | "value" | "capture": an exit is wanted and has not finished
	ExitTries          int    // consecutive exits that filled nothing
	// BlockedSeconds is the seconds an exit was wanted and nothing filled: the fold counts those in
	// which an order was sent and came back empty, AfterDecide those in which none could be sent.
	BlockedSeconds int
}

// Window is one 15-minute close across ALL coins, which sizing treats as one bet (plan 4.5).
type Window struct {
	Close       int64   // unix seconds
	EquityCents int64   // E_w: the bucket's cash when it formed the window's first buy that filled; frozen
	KMax        float64 // the largest Kelly fraction among the window's buys that filled
	OpenCents   int64   // cost basis of what the window still holds
	LostCents   int64   // losses the window has already realised; they never come back
}

// Used is what the window has spent of its budget: what it still has open plus what it has lost.
func (w *Window) Used() int64 { return w.OpenCents + w.LostCents }

// Apply folds ONE fill into the position and into its window, and returns the cash effect on the
// bucket and the cost basis the fill added (a buy) or released (a sell). It is the only code that
// does this, live and on rebuild, so a window's account after a restart is the one it had before.
//
// Buy: the contracts and their whole cost (premium plus fee) are added.
//
// Sell of n of the N held: the basis released is CostCents * n / N rounded DOWN, except that
// selling the last contract releases whatever remains, so the parts always add up to the cost.
// The window gives back that basis, and if the sale brought in less than the basis the shortfall
// goes to LostCents and stays there: a losing round trip frees only what it recovered. A gain
// frees the basis and nothing more. The shortfall is reckoned per fill, not per order, so an
// order that sold one level above its basis and one below still books the second level's loss:
// the reading that uses the budget up faster.
//
// The cash figure is broker.BucketCents, the same function the store books, so memory and ledger
// cannot disagree by a rounding rule.
func (p *Position) Apply(a broker.Action, f broker.Fill, w *Window) (cashCents, basisCents int64, err error) {
	if f.Qty < 1 {
		return 0, 0, fmt.Errorf("a fill of %d contracts", f.Qty)
	}
	cashCents = broker.BucketCents(a, f)
	switch a {
	case broker.Buy:
		basisCents = f.PremiumCents + f.FeeCents
		p.Contracts += f.Qty
		p.CostCents += basisCents
		p.PremiumCents += f.PremiumCents
		if w != nil {
			w.OpenCents += basisCents
		}
	case broker.Sell:
		if f.Qty > p.Contracts {
			return 0, 0, fmt.Errorf("a sale of %d %s %s with %d held", f.Qty, p.Ticker, p.Side, p.Contracts)
		}
		n, held := int64(f.Qty), int64(p.Contracts)
		premium := p.PremiumCents
		basisCents = p.CostCents
		if n < held {
			basisCents = p.CostCents * n / held
			premium = p.PremiumCents * n / held
		}
		p.Contracts -= f.Qty
		p.CostCents -= basisCents
		p.PremiumCents -= premium
		if w != nil {
			w.OpenCents -= basisCents
			if short := basisCents - cashCents; short > 0 {
				w.LostCents += short
			}
		}
	default:
		return 0, 0, fmt.Errorf("a fill that is neither a buy nor a sell: %q", a)
	}
	return cashCents, basisCents, nil
}
