package runner

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

func ptr(v int64) *int64 { return &v }

// A small world: one v1 bucket, a v2 original on its second life (the first is frozen in the
// ledger), a live twin, and a twin that ran out and was retired.
func world1() ([]Book, store.Capital) {
	books := []Book{
		{Engine: "v1", Series: "KXBTC15M", Buckets: []BucketBook{{BucketID: 1, Name: "KXBTC15M Value v1", Engine: "v1", World: "real", CashCents: 14_000, AtRiskCents: 500, MarkedCents: 620}},
			Positions: []Position{{Coin: "BTC", CostCents: 500, ValueCents: ptr(620)}}},
		{Engine: "v2", Buckets: []BucketBook{
			{BucketID: 30, Name: "kalshi15m2 Model v2 life 2", Engine: "v2", World: "real", CashCents: 98_000, AtRiskCents: 2_000, MarkedCents: 900, Unmarked: 1},
			{BucketID: 20, Name: "kalshi15m2 Anti Model v2", Engine: "v2", World: "anti", CashCents: 101_000},
			{BucketID: 21, Name: "kalshi15m2 Anti Late v2", Engine: "v2", World: "anti", Retired: true, CashCents: 37},
		}, Positions: []Position{{Coin: "ETH", CostCents: 1_200, ValueCents: ptr(900)}, {Coin: "DOGE", CostCents: 800}}},
	}
	capital := store.Capital{
		Money: store.MoneyBuckets{Winnings: 1_000, Replenishment: 41, TaxReserve: 300, FeeReserve: 0, External: 415_000},
		Buckets: []store.BucketCapital{
			{ID: 1, Version: 1, ContributedCents: 15_000},
			{ID: 10, Version: 2, Status: "frozen", ContributedCents: 100_000 - 41}, // Model's first life: seeded, 41 cents reaped
			{ID: 30, Version: 2, ContributedCents: 100_000 - 1_300},                // its second: seeded, 1,300 allocated away
			{ID: 20, Version: 2, Anti: true, ContributedCents: 100_000},
			{ID: 21, Version: 2, Anti: true, Status: "frozen", ContributedCents: 100_000},
		},
	}
	return books, capital
}

func TestCompositionAddsUpToTheTotal(t *testing.T) {
	books, capital := world1()
	v := Value(books, capital, []string{"BTC", "ETH", "SOL", "XRP", "DOGE"})

	if len(v.Groups) != 3 {
		t.Fatalf("%d groups, want 3", len(v.Groups))
	}
	var sum Line
	for i, g := range v.Groups {
		if g.Key != Groups[i] {
			t.Errorf("group %d is %q, want %q", i, g.Key, Groups[i])
		}
		sum.ValueCents, sum.CashCents, sum.AtRiskCents = sum.ValueCents+g.ValueCents, sum.CashCents+g.CashCents, sum.AtRiskCents+g.AtRiskCents
		sum.ContributedCents, sum.Unmarked = sum.ContributedCents+g.ContributedCents, sum.Unmarked+g.Unmarked
	}
	if sum.ValueCents != v.Total.ValueCents || sum.CashCents != v.Total.CashCents || sum.AtRiskCents != v.Total.AtRiskCents ||
		sum.ContributedCents != v.Total.ContributedCents || sum.Unmarked != v.Total.Unmarked {
		t.Errorf("groups add up to %+v, total is %+v", sum, v.Total)
	}
	if v.Total.ContributedCents != capital.Money.External {
		t.Errorf("total contributed %d, want what came from outside, %d", v.Total.ContributedCents, capital.Money.External)
	}
	want := map[string]Line{
		"v3":     {}, // no live-engine bucket exists: the group is there, and all zeros
		"legacy": {ValueCents: 214_520, CashCents: 213_000, AtRiskCents: 2_500, ContributedCents: 413_659, Unmarked: 1, Count: 3},
		"money":  {ValueCents: 1_341, CashCents: 1_341, ContributedCents: 1_341, Count: 4},
	}
	for _, g := range v.Groups {
		w := want[g.Key]
		w.Scope, w.Key = "group", g.Key
		if g != w {
			t.Errorf("%s: %+v, want %+v", g.Key, g, w)
		}
	}
	if len(v.Buckets) != 3 {
		t.Errorf("%d bucket lines, want the 3 live ones", len(v.Buckets))
	}
}

// A bet with no bid is worth nothing and is counted, for the bucket and for its coin.
func TestUnmarkedBetsCountZero(t *testing.T) {
	books, capital := world1()
	v := Value(books, capital, []string{"BTC", "ETH", "SOL", "XRP", "DOGE"})
	if len(v.Coins) != 5 {
		t.Fatalf("%d coins, want all 5 whether or not anything is open on them", len(v.Coins))
	}
	doge := v.Coins[4]
	if doge.Key != "DOGE" || doge.ValueCents != 0 || doge.AtRiskCents != 800 || doge.Unmarked != 1 || doge.Count != 1 {
		t.Errorf("DOGE: %+v", doge)
	}
	if sol := v.Coins[2]; sol.ValueCents != 0 || sol.AtRiskCents != 0 || sol.Count != 0 {
		t.Errorf("SOL has nothing open: %+v", sol)
	}
	if n, ok := markCents(12, 0); ok || n != 0 {
		t.Errorf("no bid marked at %d", n)
	}
	if n, _ := markCents(12, 0.41); n != 492 {
		t.Errorf("12 at 41 cents marked at %d, want 492", n)
	}
}

// Group earnings add up to the total's across a restake and an allocation, and neither counts
// as profit or loss.
func TestEarnedAddsUpAcrossARestake(t *testing.T) {
	books, capital := world1()
	coins := []string{"BTC"}
	before := Value(books, capital, coins)

	// Model's second life runs out with 50 cents, is reaped, and a third life is seeded from
	// the replenishment pool, which is 99,909 cents short: that much comes from outside.
	books[1].Buckets[0] = BucketBook{BucketID: 40, Name: "kalshi15m2 Model v2 life 3", Engine: "v2", World: "real", CashCents: 100_000}
	books[1].Positions = nil
	capital.Buckets[2].Status, capital.Buckets[2].ContributedCents = "frozen", 98_700-50
	capital.Buckets = append(capital.Buckets, store.BucketCapital{ID: 40, Version: 2, ContributedCents: 100_000})
	capital.Money.Replenishment, capital.Money.External = 0, capital.Money.External+99_909
	after := Value(books, capital, coins)

	at := time.Unix(1_790_000_000, 0)
	var groups int64
	for i := range after.Groups {
		groups += store.EarnedBetween(before.Snapshots(at, false)[1+i], after.Snapshots(at, false)[1+i])
	}
	total := store.EarnedBetween(before.Snapshots(at, false)[0], after.Snapshots(at, false)[0])
	// What really happened: life 2 went from 98,900 marked to 50, and the v1 bet is unchanged.
	if want := int64(50 - 98_900); total != want || groups != want {
		t.Errorf("earned: total %d, groups %d, want %d", total, groups, want)
	}
	if n := len(after.Snapshots(at, true)); n != 1+len(Groups)+len(after.Buckets)+len(after.Coins) {
		t.Errorf("a detailed batch has %d rows", n)
	}
}

// Switching an engine off must not read as losing its buckets: a live bucket no engine holds
// still has its cash in the ledger and is counted at it, so value and contributed go on
// covering the same buckets.
func TestUnheldLiveBucketsCountAtTheirLedgerCash(t *testing.T) {
	coins := []string{"BTC"}
	books, capital := world1()
	capital.Buckets = append(capital.Buckets, store.BucketCapital{ID: 2, Name: "KXETH15M Value v1", Status: "active", Version: 1, ContributedCents: 15_000})
	capital.Money.External += 15_000
	held := append([]Book{{Engine: "v1", Series: "KXETH15M", Buckets: []BucketBook{{BucketID: 2, Name: "KXETH15M Value v1", Engine: "v1", World: "real", CashCents: 14_937}}}}, books...)
	on := Value(held, capital, coins)

	// KXETH15M is set to recording only and the service restarts: no book holds bucket 2 any
	// more, and the capital read brings its ledger cash instead.
	capital.Buckets[len(capital.Buckets)-1].CashCents = 14_937
	// Cash the ledger might report for a bucket that IS held, or for a frozen one, is not counted:
	// the book is the record of the first, and the second was reaped.
	capital.Buckets[0].CashCents, capital.Buckets[1].CashCents = 99_999, 99_999
	off := Value(books, capital, coins)

	at := time.Unix(1_790_000_000, 0)
	if got := store.EarnedBetween(on.Snapshots(at, false)[0], off.Snapshots(at, false)[0]); got != 0 {
		t.Errorf("switching an engine off earned %d cents, want 0", got)
	}
	if on.Total != off.Total {
		t.Errorf("total with the engine on %+v, off %+v", on.Total, off.Total)
	}
	v1 := off.Groups[1]
	if v1.Key != "legacy" || v1.ValueCents != 214_520+14_937 || v1.CashCents != 213_000+14_937 || v1.ContributedCents != 413_659+15_000 || v1.Count != 4 {
		t.Errorf("legacy group: %+v", v1)
	}
	var line *Line
	for i := range off.Buckets {
		if off.Buckets[i].Key == "KXETH15M Value v1" {
			line = &off.Buckets[i]
		}
	}
	if line == nil || line.ValueCents != 14_937 || line.CashCents != 14_937 || line.ContributedCents != 15_000 || line.AtRiskCents != 0 {
		t.Errorf("the unheld bucket's own line: %+v", line)
	}
	if len(off.Buckets) != 4 {
		t.Errorf("%d bucket lines, want the 3 held and the 1 unheld", len(off.Buckets))
	}
}

// A snapshot row is there for good, so the minute is skipped whenever the books cannot be
// trusted or cannot be priced.
func TestSnapshotRefusal(t *testing.T) {
	now := time.Unix(1_790_000_105, 0)
	closed, open := float64(1_790_000_100), float64(1_790_001_000)
	priced := func(closes float64) Position {
		return Position{Coin: "BTC", CostCents: 500, ValueCents: ptr(620), Closes: closes}
	}

	if err := SnapshotRefusal(nil, now); err != nil {
		t.Errorf("no books: %v", err)
	}
	// A losing side with no bid, in a round still open, is fairly worth nothing: written.
	fine := []Book{{Engine: "v1", Series: "KXBTC15M", Positions: []Position{priced(open)}}, {Engine: "v2", Positions: []Position{{Coin: "ETH", CostCents: 800, Closes: open}}}}
	if err := SnapshotRefusal(fine, now); err != nil {
		t.Errorf("every round still open: %v", err)
	}

	// One bet whose round closed five seconds ago refuses the whole batch, even with a bid on it.
	waiting := []Book{fine[0], {Engine: "v2", Positions: []Position{priced(open), priced(closed)}}}
	err := SnapshotRefusal(waiting, now)
	if !errors.Is(err, ErrAwaitingSettlement) || !strings.HasPrefix(err.Error(), "1 open bets") {
		t.Errorf("a bet between close and settlement: %v", err)
	}
	if err := SnapshotRefusal(waiting, time.Unix(int64(closed), 0)); !errors.Is(err, ErrAwaitingSettlement) {
		t.Errorf("at the very second of the close: %v", err)
	}
	if err := SnapshotRefusal(waiting, time.Unix(int64(closed)-1, 0)); err != nil {
		t.Errorf("a second before the close: %v", err)
	}
	// Once the poller has stopped asking for the result it will not clear by itself: a fault.
	if err := SnapshotRefusal(waiting, now.Add(16*time.Minute)); err == nil || errors.Is(err, ErrAwaitingSettlement) {
		t.Errorf("a bet nobody will ever settle: %v", err)
	}

	// A halted engine's memory and the ledger disagree, whichever engine it is.
	halted := []Book{fine[0], {Engine: "v2", Halted: "closing kalshi15m2 Scalper v2: connection refused"}}
	err = SnapshotRefusal(halted, now)
	if err == nil || errors.Is(err, ErrAwaitingSettlement) || !strings.Contains(err.Error(), "v2 is halted") {
		t.Errorf("a halted engine: %v", err)
	}
}

// world3 is world1 after the third engine's two buckets were staked: $1,000 each came from
// outside through the common pool, and one of them has since bought a bet.
func world3() ([]Book, store.Capital) {
	books, capital := world1()
	books = append(books, Book{Engine: "v3", Buckets: []BucketBook{
		{BucketID: 50, Name: "kalshi15m3 Scalper v3", Strategy: "Scalper", Engine: "v3", World: "real", Version: 3, CashCents: 97_400, AtRiskCents: 2_600, MarkedCents: 2_450, Bets: 1},
		{BucketID: 51, Name: "kalshi15m3 Value v3", Strategy: "Value", Engine: "v3", World: "real", Version: 3, CashCents: 100_000},
	}, Positions: []Position{{Coin: "BTC", Engine: "v3", CostCents: 2_600, ValueCents: ptr(2_450)}}})
	capital.Buckets = append(capital.Buckets,
		store.BucketCapital{ID: 50, Name: "kalshi15m3 Scalper v3", Status: "active", Version: 3, ContributedCents: 100_000},
		store.BucketCapital{ID: 51, Name: "kalshi15m3 Value v3", Status: "active", Version: 3, ContributedCents: 100_000})
	capital.Money.External += 200_000
	return books, capital
}

func sumLines(lines []Line) Line {
	var sum Line
	for _, g := range lines {
		sum.ValueCents, sum.CashCents, sum.AtRiskCents = sum.ValueCents+g.ValueCents, sum.CashCents+g.CashCents, sum.AtRiskCents+g.AtRiskCents
		sum.ContributedCents, sum.Unmarked, sum.Count = sum.ContributedCents+g.ContributedCents, sum.Unmarked+g.Unmarked, sum.Count+g.Count
	}
	return sum
}

// The live engine's buckets are a group of their own, the three groups add up to the total, and
// staking them changes no other group by a cent: the money group's contribution in particular,
// which is "everything from outside that no bucket group has" and must take the new group off too.
func TestThreeGroupsAddUpToTheTotal(t *testing.T) {
	coins := []string{"BTC", "ETH", "SOL", "XRP", "DOGE"}
	before0, capital0 := world1()
	before := Value(before0, capital0, coins)
	books, capital := world3()
	v := Value(books, capital, coins)

	if got := strings.Join(Groups, " "); got != "v3 legacy money" {
		t.Fatalf("groups are %q", got)
	}
	sum := sumLines(v.Groups)
	sum.Scope, sum.Key = "total", "all"
	if sum != v.Total {
		t.Errorf("groups add up to %+v, total is %+v", sum, v.Total)
	}
	if v.Total.ContributedCents != capital.Money.External {
		t.Errorf("total contributed %d, want what came from outside, %d", v.Total.ContributedCents, capital.Money.External)
	}
	want := Line{Scope: "group", Key: "v3", ValueCents: 97_400 + 2_450 + 100_000, CashCents: 197_400, AtRiskCents: 2_600, ContributedCents: 200_000, Count: 2}
	if v.Groups[0] != want {
		t.Errorf("live engine group: %+v, want %+v", v.Groups[0], want)
	}
	for i, g := range v.Groups {
		if g.Key != "v3" && g != before.Groups[i] {
			t.Errorf("staking the live engine changed %s: %+v, was %+v", g.Key, g, before.Groups[i])
		}
	}
	if got := v.Total.ValueCents - v.Total.ContributedCents - (before.Total.ValueCents - before.Total.ContributedCents); got != -150 {
		t.Errorf("the live engine's arrival reads as %d cents earned, want -150", got)
	}
}

// A live-engine bucket goes to its own group by its version NUMBER, from whichever side names
// it: the engine's book (the Version field, or the engine label when a book does not set the
// field) and the ledger's capital. Archived v1/v2 buckets (including twins) sit in "legacy".
func TestLiveEngineBucketsNeverLandInLegacy(t *testing.T) {
	books, capital := world3()
	for i := range books[2].Buckets {
		books[2].Buckets[i].Version = 0 // a book that says only "v3"
	}
	v := Value(books, capital, []string{"BTC"})
	if g := v.Groups[0]; g.Count != 2 || g.ValueCents != 199_850 || g.ContributedCents != 200_000 {
		t.Errorf("by engine label: %+v", g)
	}
	if g := v.Groups[1]; g.Count != 3 || g.ContributedCents != 413_659 {
		t.Errorf("legacy took a live-engine bucket: %+v", g)
	}

	capital.Buckets[5].CashCents, capital.Buckets[6].CashCents = 97_400, 100_000
	off := Value(books[:2], capital, []string{"BTC"})
	if g := off.Groups[0]; g.Key != "v3" || g.Count != 2 || g.ValueCents != 197_400 || g.ContributedCents != 200_000 {
		t.Errorf("unheld: %+v", g)
	}
	for _, c := range []struct {
		version int
		anti    bool
		want    string
	}{{1, false, "legacy"}, {2, false, "legacy"}, {2, true, "legacy"}, {3, false, "v3"}, {3, true, "v3"}, {4, false, "legacy"}} {
		if got := groupOf(c.version, c.anti); got != c.want {
			t.Errorf("groupOf(%d, %v) = %q, want %q", c.version, c.anti, got, c.want)
		}
	}
}

// With no live-engine bucket anywhere, the other two groups and the total are exactly what
// they were, and the batch gains exactly one all-zero row.
func TestNoLiveEngineBucketsLeavesTheOtherGroupsAlone(t *testing.T) {
	books, capital := world1()
	v := Value(books, capital, []string{"BTC", "ETH", "SOL", "XRP", "DOGE"})
	if g := v.Groups[0]; g != (Line{Scope: "group", Key: "v3"}) {
		t.Errorf("an empty live engine group is %+v, want all zeros", g)
	}
	two := sumLines([]Line{v.Groups[1], v.Groups[2]})
	two.Scope, two.Key = "total", "all"
	if two != v.Total {
		t.Errorf("the archived groups add up to %+v, total is %+v", two, v.Total)
	}
	rows := v.Snapshots(time.Unix(1_790_000_000, 0), false)
	if len(rows) != 4 || rows[1].Scope != "group" || rows[1].Key != "v3" || rows[1].ValueCents != 0 || rows[1].ContributedCents != 0 || rows[1].AtRiskCents != 0 {
		t.Errorf("snapshot rows: %+v", rows)
	}
}
