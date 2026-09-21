package runner

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// This file is the typed view of the engines' books: what the home page and the once-a-minute
// value snapshots read. Everything in it comes from memory; nothing here asks the database.

// Position is one open bet.
type Position struct {
	Strategy   string // without the "Anti " prefix: World says which of the pair it is
	Engine     string // v1 or v2
	World      string // real, or anti for an anti-world twin
	Coin       string
	Side       string // UP or DOWN
	Ticker     string
	Contracts  int
	EntryPrice float64
	CostCents  int64
	ValueCents *int64  // at the bid right now; nil when there is no bid to mark it by
	Placed     float64 // unix seconds
	Underlying float64 // the coin's price when the bet was placed
}

// Marker is a bet, or the early sale of one, as a point on a coin's price chart.
type Marker struct {
	T, Price                            float64 // when, and the coin's price then
	Coin, Side, Strategy, Engine, World string
	Kind                                string // bet or sold
}

// BucketBook is one live bucket as its engine sees it right now.
type BucketBook struct {
	BucketID       int64
	Name           string // the bucket's name in the database, with its life if it has one
	Strategy       string
	Engine, World  string
	Retired        bool // a twin that ran out: its bucket is frozen and it places no more bets
	Life           int
	CashCents      int64
	AtRiskCents    int64  // open bets at cost
	MarkedCents    int64  // open bets at the bid; a bet with no bid counts 0
	Unmarked       int    // open bets with no bid
	HighWaterCents *int64 // nil for the first engine, which takes no sustainment allocation
	Bets           int
}

// ValueCents is the bucket marked to market.
func (b BucketBook) ValueCents() int64 { return b.CashCents + b.MarkedCents }

// Book is everything one engine holds.
type Book struct {
	Engine    string
	Series    string // the first engine runs one book per series; the second has one book and leaves this empty
	Halted    string // why it stopped deciding, or ""
	Buckets   []BucketBook
	Positions []Position
}

// world and plain split an engine's strategy name into which of the pair it is and the name
// the pair shares.
func world(anti bool) string {
	if anti {
		return "anti"
	}
	return "real"
}

func plain(name string) string { return strings.TrimPrefix(name, "Anti ") }

// markCents is what contracts are worth at a bid, in cents: no selling fee is taken off, because
// the home page says "at the bid". ok is false when there is no bid.
func markCents(contracts int, bid float64) (int64, bool) {
	if bid <= 0 {
		return 0, false
	}
	return int64(math.Round(float64(contracts) * bid * 100)), true
}

// Book is the first engine's buckets and open bets on this series.
func (r *Runner) Book() Book {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Book{Engine: "v1", Series: r.Series, Halted: r.halted}
	m := r.trader.Market
	for _, a := range r.trader.Accounts {
		sb := r.setup.Buckets[a.Params.Name]
		b := BucketBook{BucketID: sb.ID, Name: sb.Name, Strategy: a.Params.Name, Engine: "v1", World: "real", Life: store.LifeOf(sb.Name),
			CashCents: cents(a.Cash), Bets: a.Bets}
		for _, lot := range a.Log {
			if lot.Status != "open" {
				continue
			}
			bid := 0.0
			if m != nil && m.Ticker == lot.Ticker {
				bid = m.YesBid
				if lot.Side != "UP" {
					bid = m.NoBid
				}
			}
			p := Position{Strategy: a.Params.Name, Engine: "v1", World: "real", Coin: r.Coin, Side: lot.Side, Ticker: lot.Ticker,
				Contracts: lot.Contracts, EntryPrice: lot.Price, CostCents: cents(lot.Cost), Placed: lot.T, Underlying: lot.BTCPrice}
			b.AtRiskCents += p.CostCents
			if v, ok := markCents(lot.Contracts, bid); ok {
				p.ValueCents, b.MarkedCents = &v, b.MarkedCents+v
			} else {
				b.Unmarked++
			}
			out.Positions = append(out.Positions, p)
		}
		out.Buckets = append(out.Buckets, b)
	}
	return out
}

// Markers lists this series' bets and early sales made at or after `since`.
func (r *Runner) Markers(coin string, since float64) []Marker {
	if coin != r.Coin {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Marker
	for _, a := range r.trader.Accounts {
		for _, lot := range a.Log {
			out = appendMarkers(out, since, Marker{Coin: r.Coin, Side: lot.Side, Strategy: a.Params.Name, Engine: "v1", World: "real"},
				lot.T, lot.BTCPrice, lot.Status, lot.ExitT, lot.ExitBTC)
		}
	}
	return out
}

// appendMarkers adds a lot's bet, and its early sale if it had one, when they fall in the span.
func appendMarkers(out []Marker, since float64, m Marker, placed, price float64, status string, exitT, exitPrice *float64) []Marker {
	if placed >= since {
		m.T, m.Price, m.Kind = placed, price, "bet"
		out = append(out, m)
	}
	if status == "sold" && exitT != nil && exitPrice != nil && *exitT >= since {
		m.T, m.Price, m.Kind = *exitT, *exitPrice, "sold"
		out = append(out, m)
	}
	return out
}

// Book is the second engine's twelve buckets and every open bet across the coins.
func (r *Runner2) Book() Book {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := Book{Engine: "v2", Halted: r.halted}
	mk := r.trader.Markets()
	for _, a := range r.trader.Accounts {
		sb := r.setup.Buckets[a.Params.Name]
		w := world(a.Params.Anti)
		b := BucketBook{BucketID: sb.ID, Name: sb.Name, Strategy: plain(a.Params.Name), Engine: "v2", World: w, Retired: a.Retired,
			Life: store.LifeOf(sb.Name), CashCents: cents(a.Cash), Bets: a.Bets}
		if mark, ok := r.hwm[a.Params.Name]; ok {
			b.HighWaterCents = &mark
		}
		for _, lot := range a.Log {
			if lot.Status != "open" {
				continue
			}
			bid := 0.0
			if m := mk[lot.Coin]; m != nil && m.Ticker == lot.Ticker {
				bid = m.YesBid
				if lot.Side != "UP" {
					bid = m.NoBid
				}
			}
			p := Position{Strategy: b.Strategy, Engine: "v2", World: w, Coin: lot.Coin, Side: lot.Side, Ticker: lot.Ticker,
				Contracts: lot.Contracts, EntryPrice: lot.Price, CostCents: cents(lot.Cost), Placed: lot.T, Underlying: lot.BTCPrice}
			b.AtRiskCents += p.CostCents
			if v, ok := markCents(lot.Contracts, bid); ok {
				p.ValueCents, b.MarkedCents = &v, b.MarkedCents+v
			} else {
				b.Unmarked++
			}
			out.Positions = append(out.Positions, p)
		}
		out.Buckets = append(out.Buckets, b)
	}
	return out
}

// Markers lists one coin's bets and early sales made at or after `since`, in both worlds. Only
// the current life of each strategy is in memory: a life that ran out took its log with it.
func (r *Runner2) Markers(coin string, since float64) []Marker {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Marker
	for _, a := range r.trader.Accounts {
		for _, lot := range a.Log {
			if lot.Coin != coin {
				continue
			}
			out = appendMarkers(out, since, Marker{Coin: coin, Side: lot.Side, Strategy: plain(a.Params.Name), Engine: "v2", World: world(a.Params.Anti)},
				lot.T, lot.BTCPrice, lot.Status, lot.ExitT, lot.ExitBTC)
		}
	}
	return out
}

// Capital is the ledger's side of the balance sheet as last read, and whether that read worked.
// It is re-read whenever this engine seeds, reaps or takes an allocation, which is the only time
// it changes, so between those moments the cached copy is exact and not merely recent. (Venue
// fees paid and deployed cash do move with every fill; the snapshots take neither from here.)
func (r *Runner2) Capital() (store.Capital, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capital, r.capitalFresh
}

// RefreshCapital reads the ledger's side again. The snapshot writer calls it only after a read
// has failed; it holds the engine's lock, as every other refresh does, so that a read can never
// land on top of a newer one.
func (r *Runner2) RefreshCapital(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshMoney(ctx)
}

// Policy is the sustainment allocation's rates as last read.
func (r *Runner2) Policy() store.SkimPolicy {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy
}

// The four groups the home page's composition is made of, in the order it lists them.
var Groups = []string{"strategies", "anti", "v1", "money"}

// groupOf says which group a bucket belongs to.
func groupOf(version int, anti bool) string {
	switch {
	case version == 1:
		return "v1"
	case anti:
		return "anti"
	}
	return "strategies"
}

// Line is one scope's figures at one moment: a row of value_snapshot before it has a time.
type Line struct {
	Scope, Key       string
	ValueCents       int64
	CashCents        int64
	AtRiskCents      int64
	ContributedCents int64
	Unmarked         int
	Count            int // the live buckets it covers; for a coin, the bets open on it
}

// Valuation is the whole balance sheet at one moment.
type Valuation struct {
	Total   Line
	Groups  []Line // in Groups order; their values, and their contributions, add up to Total's
	Buckets []Line // live buckets
	Coins   []Line // open bets per coin: cash and contributed are always 0
}

// Value marks every book to market and lays it beside what the ledger says was put in.
//
// The three bucket groups are the live buckets' cash and marked bets. The money group is the
// four set-aside buckets (winnings, replenishment and the two reserves). A group's contribution
// covers every bucket it has ever had, frozen ones too: a strategy that ran out and was staked
// again shows its first life's loss as a loss, not as a fresh $1,000 earned. The money group's
// contribution is whatever came from outside and is not in a bucket group, so the four always
// add up to the total's, and the four earned figures add up to the total earned.
func Value(books []Book, capital store.Capital, coins []string) Valuation {
	groups := map[string]*Line{}
	for _, g := range Groups {
		groups[g] = &Line{Scope: "group", Key: g}
	}
	contributed := map[int64]int64{}
	for _, b := range capital.Buckets {
		contributed[b.ID] = b.ContributedCents
		groups[groupOf(b.Version, b.Anti)].ContributedCents += b.ContributedCents
	}
	byCoin := map[string]*Line{}
	for _, c := range coins {
		byCoin[c] = &Line{Scope: "coin", Key: c}
	}
	v := Valuation{}
	for _, book := range books {
		for _, b := range book.Buckets {
			if b.Retired { // its bucket is frozen and empty; only its contribution, above, remains
				continue
			}
			version := 2
			if b.Engine == "v1" {
				version = 1
			}
			g := groups[groupOf(version, b.World == "anti")]
			g.ValueCents, g.CashCents, g.AtRiskCents = g.ValueCents+b.ValueCents(), g.CashCents+b.CashCents, g.AtRiskCents+b.AtRiskCents
			g.Unmarked, g.Count = g.Unmarked+b.Unmarked, g.Count+1
			v.Buckets = append(v.Buckets, Line{Scope: "bucket", Key: b.Name, ValueCents: b.ValueCents(), CashCents: b.CashCents,
				AtRiskCents: b.AtRiskCents, ContributedCents: contributed[b.BucketID], Unmarked: b.Unmarked, Count: 1})
		}
		for _, p := range book.Positions {
			c := byCoin[p.Coin]
			if c == nil {
				c = &Line{Scope: "coin", Key: p.Coin}
				byCoin[p.Coin], coins = c, append(coins, p.Coin)
			}
			c.AtRiskCents += p.CostCents
			c.Count++
			if p.ValueCents != nil {
				c.ValueCents += *p.ValueCents
			} else {
				c.Unmarked++
			}
		}
	}
	m := capital.Money
	money := groups["money"]
	money.ValueCents = m.Winnings + m.Replenishment + m.TaxReserve + m.FeeReserve
	money.CashCents, money.Count = money.ValueCents, 4
	money.ContributedCents = m.External - groups["strategies"].ContributedCents - groups["anti"].ContributedCents - groups["v1"].ContributedCents

	v.Total = Line{Scope: "total", Key: "all"}
	for _, key := range Groups {
		g := *groups[key]
		v.Groups = append(v.Groups, g)
		v.Total.ValueCents, v.Total.CashCents, v.Total.AtRiskCents = v.Total.ValueCents+g.ValueCents, v.Total.CashCents+g.CashCents, v.Total.AtRiskCents+g.AtRiskCents
		v.Total.ContributedCents, v.Total.Unmarked, v.Total.Count = v.Total.ContributedCents+g.ContributedCents, v.Total.Unmarked+g.Unmarked, v.Total.Count+g.Count
	}
	for _, c := range coins {
		v.Coins = append(v.Coins, *byCoin[c])
	}
	return v
}

// Snapshots turns a valuation into rows to append. The total and the groups go in every time;
// buckets and coins only when `detail` is set, which the writer does every fifth minute.
func (v Valuation) Snapshots(at time.Time, detail bool) []store.ValueSnapshot {
	lines := append([]Line{v.Total}, v.Groups...)
	if detail {
		lines = append(append(lines, v.Buckets...), v.Coins...)
	}
	rows := make([]store.ValueSnapshot, 0, len(lines))
	for _, l := range lines {
		rows = append(rows, store.ValueSnapshot{At: at, Scope: l.Scope, Key: l.Key, ValueCents: l.ValueCents, CashCents: l.CashCents,
			AtRiskCents: l.AtRiskCents, ContributedCents: l.ContributedCents, Unmarked: l.Unmarked})
	}
	return rows
}
