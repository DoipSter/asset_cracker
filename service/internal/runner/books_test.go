package runner

import (
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

	if len(v.Groups) != 4 {
		t.Fatalf("%d groups, want 4", len(v.Groups))
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
		"strategies": {ValueCents: 98_900, CashCents: 98_000, AtRiskCents: 2_000, ContributedCents: 198_659, Unmarked: 1, Count: 1},
		"anti":       {ValueCents: 101_000, CashCents: 101_000, ContributedCents: 200_000, Count: 1}, // the retired twin's cash is not counted; its seed is
		"v1":         {ValueCents: 14_620, CashCents: 14_000, AtRiskCents: 500, ContributedCents: 15_000, Count: 1},
		"money":      {ValueCents: 1_341, CashCents: 1_341, ContributedCents: 1_341, Count: 4},
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
	if n := len(after.Snapshots(at, true)); n != 1+4+len(after.Buckets)+len(after.Coins) {
		t.Errorf("a detailed batch has %d rows", n)
	}
}
