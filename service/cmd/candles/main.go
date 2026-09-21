// Command candles backfills Coinbase's daily and hourly candles for the spot products into the
// candle table (migration 0015), paging backwards from the newest complete candle to -since.
//
//	AC_DATABASE_URL=... go run ./cmd/candles -since 2023-09-21
//
// It stores only COMPLETE candles and never changes a stored one, so it is safe to run again: a
// rerun fills gaps and skips the rest. It prints counts only, never a price. It exits 1 if any
// request or insert failed after its retries; the counts say how many.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/doipster/asset_cracker/service/internal/candles"
	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/config"
	"github.com/doipster/asset_cracker/service/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "candles:", err)
		os.Exit(1)
	}
}

func run() error {
	now := time.Now().UTC()
	since := flag.String("since", now.AddDate(-3, 0, 0).Format(time.DateOnly), "first candle date to fetch, UTC (YYYY-MM-DD)")
	pace := flag.Duration("pace", time.Second, "wait between requests (politeness; Coinbase's own limit is not probed)")
	only := flag.String("products", "", "comma-separated products (default: every active Coinbase spot instrument)")
	flag.Parse()
	from, err := time.Parse(time.DateOnly, *since)
	if err != nil {
		return fmt.Errorf("-since: %w", err)
	}

	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()
	all, err := db.ActiveInstruments(ctx)
	if err != nil {
		return fmt.Errorf("instruments: %w", err)
	}
	ids := map[string]int64{}
	for _, in := range all {
		if in.Source == "coinbase" && in.Kind == "spot" {
			ids[in.Symbol] = in.ID
		}
	}
	names := make([]string, 0, len(ids))
	if *only != "" {
		for _, p := range strings.Split(*only, ",") {
			if _, ok := ids[p]; !ok {
				return fmt.Errorf("%s is not an active Coinbase spot instrument", p)
			}
			names = append(names, p)
		}
	} else {
		for p := range ids {
			names = append(names, p)
		}
		sort.Strings(names)
	}

	fetch := func(ctx context.Context, product string, g int, first, last time.Time) ([]coinbase.Bar, error) {
		return coinbase.Bars(ctx, cfg.UserAgent, product, g, first, last)
	}
	wait := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	fmt.Printf("since %s, products %s\n", from.Format(time.DateOnly), strings.Join(names, " "))
	var total candles.Counts
	for _, p := range names {
		for _, g := range candles.Granularities {
			ws := candles.Windows(from, time.Now(), g, candles.MaxPerRequest)
			var sum candles.Counts
			for _, w := range ws {
				var c candles.Counts
				var err error
				for try := 0; try < 3; try++ { // a retry waits 2, then 4 paces
					if try > 0 && !wait(*pace<<try) {
						break
					}
					if c, err = candles.Step(ctx, fetch, db, time.Now, ids[p], p, g, w); err == nil {
						break
					}
					fmt.Fprintln(os.Stderr, "  retry:", err)
				}
				if err != nil {
					c = candles.Counts{Failed: 1}
					fmt.Fprintln(os.Stderr, "  gave up:", err)
				}
				sum.Add(c)
				if ctx.Err() != nil || !wait(*pace) {
					break
				}
			}
			fmt.Printf("%-9s %6ds windows=%d %s\n", p, int(g/time.Second), len(ws), sum)
			total.Add(sum)
			if ctx.Err() != nil {
				return fmt.Errorf("interrupted: %s", total)
			}
		}
	}
	fmt.Printf("total     %s\n", total)
	if total.Failed > 0 {
		return fmt.Errorf("%d windows failed; run again to fill them", total.Failed)
	}
	return nil
}
