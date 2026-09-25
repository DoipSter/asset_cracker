package runner

import (
	"errors"
	"fmt"
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

// markCents is what contracts are worth at a bid, in cents: no selling fee is taken off, because
// the home page says "at the bid". ok is false when there is no bid.
func markCents(contracts int, bid float64) (int64, bool) {
	if bid <= 0 {
		return 0, false
	}
	return int64(math.Round(float64(contracts) * bid * 100)), true
}

// The groups the home page's composition is made of, in the order it lists them.
//
// "v3" is the live engine's group: the key is kept so earned-over-a-range still lines up with
// snapshots already stored. Frozen v1/v2 buckets (no longer traded) sit in "legacy". "money" is
// the four set-aside buckets.
var Groups = []string{"v3", "legacy", "money"}

// groupOf says which group a bucket belongs to. Version 3 is the live engine; everything else
// (archived v1/v2, including twins) is legacy.
func groupOf(version int, anti bool) string {
	if version == 3 {
		return "v3"
	}
	return "legacy"
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
//     number of seconds. It is ErrAwaitingSettlement until the result is late (kalshi.LateAfter);
//     past that the poller keeps asking for a traded round's result, every minute is refused
//     until it comes, and the error is a plain one so that it is logged as a fault while it waits.
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
	case longest > kalshi.LateAfter.Seconds():
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
