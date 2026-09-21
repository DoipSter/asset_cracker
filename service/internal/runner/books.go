package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/kalshi"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// This file is the typed view of the engines' books: what the home page and the once-a-minute
// value snapshots read. Everything in it comes from memory; nothing here asks the database.

// Position is one open bet.
type Position struct {
	Strategy   string // without the "Anti " prefix: World says which of the pair it is
	Engine     string // v1, v2 or v3
	World      string // real, or anti for an anti-world twin
	Coin       string
	Side       string // UP or DOWN
	Ticker     string
	Contracts  int
	EntryPrice float64
	CostCents  int64
	ValueCents *int64  // at the bid right now; nil when there is no bid to mark it by
	Placed     float64 // unix seconds
	Closes     float64 // when its round closes, unix seconds: past that and still open, it is waiting to be settled
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
	BucketID      int64
	Name          string // the bucket's name in the database, with its life if it has one
	Strategy      string
	Engine, World string
	// Version is the strategy version's number (1, 2 or 3), which decides the composition group the
	// bucket belongs to. It is the same number the ledger's side carries (store.BucketCapital.Version),
	// so a bucket's value and its contribution always land in the same group. 0 means whoever built
	// the book did not say, and the engine label decides (versionOf).
	Version        int
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
		b := BucketBook{BucketID: sb.ID, Name: sb.Name, Strategy: a.Params.Name, Engine: "v1", World: "real", Version: 1, Life: store.LifeOf(sb.Name),
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
				Contracts: lot.Contracts, EntryPrice: lot.Price, CostCents: cents(lot.Cost), Placed: lot.T, Closes: lot.Close, Underlying: lot.BTCPrice}
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

// BucketIDs is the ids of the buckets this series' strategies trade from. The first engine never
// replaces a bucket, so they are the same for as long as the service runs.
func (r *Runner) BucketIDs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]int64, 0, len(r.setup.Buckets))
	for _, b := range r.setup.Buckets {
		ids = append(ids, b.ID)
	}
	return ids
}

// Book is the second engine's twelve buckets and every open bet across the coins.
func (r *Runner2) Book() Book {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.book()
}

// BookAndCapital is Book and Capital under ONE hold of the lock. Taken apart, a settlement that
// takes an allocation, or a strategy running out and being staked again, can slip between the
// two, and the valuation would lay books from before it beside capital from after it: a false
// step of the whole amount, which a value snapshot would then keep for good.
func (r *Runner2) BookAndCapital() (Book, store.Capital, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.book(), r.capital, r.capitalFresh
}

// book is Book for a caller that holds the lock.
func (r *Runner2) book() Book {
	out := Book{Engine: "v2", Halted: r.halted}
	mk := r.trader.Markets()
	for _, a := range r.trader.Accounts {
		sb := r.setup.Buckets[a.Params.Name]
		w := world(a.Params.Anti)
		b := BucketBook{BucketID: sb.ID, Name: sb.Name, Strategy: plain(a.Params.Name), Engine: "v2", World: w, Version: 2, Retired: a.Retired,
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
				Contracts: lot.Contracts, EntryPrice: lot.Price, CostCents: cents(lot.Cost), Placed: lot.T, Closes: lot.Close, Underlying: lot.BTCPrice}
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
// it changes, so between those moments the cached copy is exact against the LEDGER and not
// merely recent. (Venue fees paid and deployed cash do move with every fill; the snapshots take
// neither from here.) It is the engine's memory that can run ahead of the ledger, when a write
// failed and the engine halted: that is why a halted engine's books are never written down as a
// value snapshot (SnapshotRefusal).
func (r *Runner2) Capital() (store.Capital, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capital, r.capitalFresh
}

// RefreshCapital reads the ledger's side again. The snapshot writer calls it only after a read
// has failed. It never holds the engine's lock across the database: that lock is what every
// step, settlement and price print waits on, and this is only a display figure. The lock is
// taken twice, briefly, to note the generation before the read and to keep the result after it,
// and the result is dropped if the engine re-read the capital (or halted) in between, because
// the engine's read is then the newer one.
func (r *Runner2) RefreshCapital(ctx context.Context) {
	r.mu.Lock()
	gen, held := r.capitalGen, r.held()
	r.mu.Unlock()

	m, moneyErr := r.db.MoneyBucketBalances(ctx)
	var b []store.BucketCapital
	var bucketErr error
	if moneyErr == nil {
		b, bucketErr = r.db.BucketCapitals(ctx, held)
	}
	at := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.capitalGen != gen:
	case moneyErr != nil:
		slog.Warn("could not read where the money sits", "err", moneyErr)
	case bucketErr != nil:
		r.money = m
		slog.Warn("could not read the capital behind the value snapshots", "err", bucketErr)
	default:
		r.money, r.capital, r.capitalFresh = m, store.Capital{Money: m, Buckets: b, ReadAt: at}, true
	}
}

// Policy is the sustainment allocation's rates as last read.
func (r *Runner2) Policy() store.SkimPolicy {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.policy
}

// The five groups the home page's composition is made of, in the order it lists them.
//
// The third engine has a group of its own (owner decision D5 of docs/honest-fills-v3.md).
// "strategies" against "anti" is a PAIRED comparison of the same six names, each with its twin;
// the third engine registers no twins, so two unpaired buckets inside "strategies" would break
// the pairing and make that group's history incomparable with its own past. The group is listed
// and written down whether or not a third-engine bucket exists: with none it is all zeros.
var Groups = []string{"strategies", "anti", "v1", "v3", "money"}

// groupOf says which group a bucket belongs to, from its strategy version's number and whether
// the version is an anti-world twin. Version 1 is the first engine and 3 the third, whatever
// their params say; everything else (today only version 2) is one of the paired six or its twin.
func groupOf(version int, anti bool) string {
	switch {
	case version == 1:
		return "v1"
	case version == 3:
		return "v3"
	case anti:
		return "anti"
	}
	return "strategies"
}

// versionOf is a held bucket's strategy version. The engine that built the book says it in
// BucketBook.Version. A book that does not (one built by hand in a test, or by code older than
// the field) is read from its engine label, which is how every book was read before the third
// engine: "v1" is 1, "v3" is 3, anything else 2.
func versionOf(b BucketBook) int {
	switch {
	case b.Version != 0:
		return b.Version
	case b.Engine == "v1":
		return 1
	case b.Engine == "v3":
		return 3
	}
	return 2
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

// ErrAwaitingSettlement marks the routine reason for refusing a value snapshot: it clears by
// itself within seconds, so whoever logs it need not raise an alarm.
var ErrAwaitingSettlement = errors.New("between their round's close and its settlement")

// SnapshotRefusal says why these books must not be written down as a value snapshot right now,
// or nil if they may be. A snapshot row is there for good, so a minute with no row is honest
// where a row with a false step in it is not. Two states are refused, and either refuses the
// whole batch, because the total and the groups must add up and both include the engine at fault:
//
//   - an engine is halted: its memory and the ledger disagree (a bucket staked again in memory
//     whose seed never reached the ledger reads as $1,000 earned), and nothing here can say which
//     is right;
//   - an open bet's round has already closed: until it settles, which takes some seconds, nothing
//     can price it, it counts 0, and a range measured from such a row reads the whole stake as
//     earned. This is the same test the sustainment allocation waits on, a condition and not a
//     number of seconds. It is ErrAwaitingSettlement for as long as the poller is still asking
//     for the round's result; past that the bet will stay open, every minute will be refused,
//     and the error is a plain one so that it is logged as a fault until someone looks.
//
// A bet with no bid in a round still open (a losing side late on) is not refused: it is fairly
// worth about nothing, and the row says how many such bets it holds.
func SnapshotRefusal(books []Book, now time.Time) error {
	at := unix(now)
	waiting, longest := 0, 0.0
	for _, book := range books {
		if book.Halted != "" {
			return fmt.Errorf("%s is halted, so its books and the ledger disagree: %s", strings.TrimSpace(book.Engine+" "+book.Series), book.Halted)
		}
		for _, p := range book.Positions {
			if p.Closes <= at {
				waiting++
				longest = math.Max(longest, at-p.Closes)
			}
		}
	}
	switch {
	case waiting == 0:
		return nil
	case longest > kalshi.GiveUpAfter.Seconds():
		return fmt.Errorf("%d open bets belong to rounds that closed up to %.0f s ago and were never settled: they cannot be priced, and no snapshot will be written until they are resolved", waiting, longest)
	}
	return fmt.Errorf("%d open bets are %w (the longest for %.0f s) and cannot be priced", waiting, ErrAwaitingSettlement, longest)
}

// Value marks every book to market and lays it beside what the ledger says was put in.
//
// The bucket groups (every group but "money") are the live buckets' cash and marked bets. The money group is the
// four set-aside buckets (winnings, replenishment and the two reserves). A group's contribution
// covers every bucket it has ever had, frozen ones too: a strategy that ran out and was staked
// again shows its first life's loss as a loss, not as a fresh $1,000 earned. The money group's
// contribution is whatever came from outside and is not in a bucket group, so the groups always
// add up to the total's, and their earned figures add up to the total earned.
//
// A live bucket that no engine holds (its series was switched to recording only, or the second
// engine is off) still has its cash in the ledger and its seed in the contributions, so it is
// counted at that cash. Left out, switching an engine off would read as losing its buckets.
// A bet such a bucket still had open is not counted: nothing in memory can mark it.
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
	held := map[int64]bool{}
	for _, book := range books {
		for _, b := range book.Buckets {
			held[b.BucketID] = true
			if b.Retired { // its bucket is frozen and empty; only its contribution, above, remains
				continue
			}
			g := groups[groupOf(versionOf(b), b.World == "anti")]
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
	for _, b := range capital.Buckets {
		if b.Status == "frozen" || held[b.ID] {
			continue
		}
		g := groups[groupOf(b.Version, b.Anti)]
		g.ValueCents, g.CashCents, g.Count = g.ValueCents+b.CashCents, g.CashCents+b.CashCents, g.Count+1
		v.Buckets = append(v.Buckets, Line{Scope: "bucket", Key: b.Name, ValueCents: b.CashCents, CashCents: b.CashCents,
			ContributedCents: b.ContributedCents, Count: 1})
	}
	m := capital.Money
	money := groups["money"]
	money.ValueCents = m.Winnings + m.Replenishment + m.TaxReserve + m.FeeReserve
	money.CashCents, money.Count = money.ValueCents, 4
	// Everything from outside that is not in a bucket group. Taken off in a loop over Groups, so a
	// group added later cannot be forgotten here and counted twice in the total.
	money.ContributedCents = m.External
	for _, key := range Groups {
		if key != "money" {
			money.ContributedCents -= groups[key].ContributedCents
		}
	}

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
