// Package runner runs a strategy family live, in simulation, for one series.
//
// It feeds the kalshi15m engine exactly what the Python app fed its own (every trade print,
// the open round's quotes once a second, each settlement once), journals every decision, and
// books every simulated fill and payout in the platform's ledger. The engine keeps the
// Python's float-dollar accounts; every amount it moves is already a whole number of cents,
// so the ledger mirrors it exactly, and the two are compared on every start.
//
// If anything cannot be recorded, the series HALTS: it stops deciding and reports why. It
// never keeps trading with an unrecorded state.
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	k "github.com/doipster/asset_cracker/service/internal/kalshi15m"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// Runner is one series: its engine, its buckets, and its record-keeping.
type Runner struct {
	Series, Coin, Product string

	db     *store.Store
	setup  store.SimSetup
	mu     sync.Mutex
	trader *k.Trader
	halted string
	ptb    float64
	closes float64
}

func cents(dollars float64) int64 { return int64(math.Round(dollars * 100)) }

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// New prepares the simulated world for a series and restores the engine's saved state.
func New(ctx context.Context, db *store.Store, series, coin, product string, offsetPct, sdPct, sigma float64) (*Runner, error) {
	names := make([]string, len(k.Strategies))
	for i, p := range k.Strategies {
		names[i] = p.Name
	}
	setup, err := db.EnsureSimSetup(ctx, series, "kalshi15m", 1, names, cents(k.StartBalance))
	if err != nil {
		return nil, fmt.Errorf("%s: sim setup: %w", series, err)
	}
	r := &Runner{Series: series, Coin: coin, Product: product, db: db, setup: setup}
	r.trader = k.NewTrader(func() float64 { return unix(time.Now()) }, offsetPct, sdPct, sigma)

	var saved k.SavedState
	found, err := db.LoadEngineState(ctx, series, &saved)
	if err != nil {
		return nil, fmt.Errorf("%s: load state: %w", series, err)
	}
	if found {
		r.trader.Import(saved)
	}
	// The ledger is the record of money. The engine's idea of each bucket's cash must match it.
	for _, a := range r.trader.Accounts {
		if got, want := cents(a.Cash), setup.Buckets[a.Params.Name].CashCents; got != want {
			r.halted = fmt.Sprintf("%s: engine says %d cents, ledger says %d", a.Params.Name, got, want)
			slog.Error("series halted: engine and ledger disagree", "series", series, "detail", r.halted)
			break
		}
	}
	return r, nil
}

// Seed primes volatility and the index offset from the exchanges, as the Python does at launch.
// Failing to seed is not fatal: the engine starts from its defaults and learns.
func (r *Runner) Seed(ctx context.Context, client *kalshi.Client, userAgent string) {
	closes, measured := seedData(ctx, client, userAgent, r.Series, r.Product)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(closes) > 0 {
		r.trader.SeedVol(closes)
	}
	var fresh [][2]float64
	for _, m := range measured {
		if !r.trader.HasOffsetAt(m[0]) {
			fresh = append(fresh, m)
		}
	}
	r.trader.SeedOffsets(fresh)
}

// seedData fetches what an engine is primed with at start: the last fifty finished one-minute
// closes, and for each of the last ten settled rounds how far Kalshi's settled value sat from
// our exchange's average over that round's final minute.
func seedData(ctx context.Context, client *kalshi.Client, userAgent, series, product string) (closes []float64, measured [][2]float64) {
	now := time.Now()
	if candles, err := coinbase.Candles(ctx, userAgent, product, 60, time.Time{}, time.Time{}); err == nil {
		if len(candles) > 50 {
			candles = candles[len(candles)-50:]
		}
		for _, c := range candles {
			if !c.Start.Add(time.Minute).After(now) { // drop the minute still in progress
				closes = append(closes, c.Close)
			}
		}
	} else {
		slog.Warn("could not seed volatility", "series", series, "err", err)
	}
	settled, err := client.SettledMarkets(ctx, series, 10)
	if err != nil || len(settled) == 0 {
		return closes, nil
	}
	type done struct {
		closes time.Time
		value  float64
	}
	var rounds []done
	lo, hi := now, time.Time{}
	for _, m := range settled {
		v, ok := k.ParseAmount(m.ExpirationValue)
		c, cerr := m.Closes()
		if !ok || v <= 0 || cerr != nil {
			continue
		}
		rounds = append(rounds, done{c, v})
		if c.Before(lo) {
			lo = c
		}
		if c.After(hi) {
			hi = c
		}
	}
	if len(rounds) == 0 {
		return closes, nil
	}
	candles, err := coinbase.Candles(ctx, userAgent, product, 60, lo.Add(-3*time.Minute), hi.Add(2*time.Minute))
	if err != nil {
		return closes, nil
	}
	byStart := map[int64]coinbase.Candle{}
	for _, c := range candles {
		byStart[c.Start.Unix()] = c
	}
	for _, d := range rounds {
		// The candle covering the round's final minute; open and close average out to about its mean.
		if c, ok := byStart[d.closes.Unix()-60]; ok {
			if ours := (c.Open + c.Close) / 2; ours > 0 {
				measured = append(measured, [2]float64{unix(d.closes), d.value/ours - 1})
			}
		}
	}
	return closes, measured
}

// Observe takes a trade print.
func (r *Runner) Observe(t coinbase.Trade) {
	price, err := strconv.ParseFloat(t.Price, 64)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.trader.Observe(price, unix(t.At))
	r.mu.Unlock()
}

func f(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

// Step is one look at the open round: let every strategy decide, then record all of it.
func (r *Runner) Step(ctx context.Context, evalID int64, at time.Time, marketID int64, info kalshi.MarketInfo, closes time.Time, q kalshi.Quotes, price string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := store.StepRecord{EvaluationID: evalID, At: at, MarketID: marketID, UnderlyingPrice: price, Quotes: q}
	r.ptb, r.closes = *info.FloorStrike, unix(closes)
	p := f(price)
	if r.halted != "" || p == 0 {
		rec.Model = map[string]any{"halted": r.halted}
		return r.db.RecordStep(ctx, r.setup, rec) // still record the quotes
	}
	m := k.Market{Ticker: info.Ticker, Strike: *info.FloorStrike, Close: unix(closes),
		YesBid: f(q.YesBid), YesAsk: f(q.YesAsk), NoBid: f(q.NoBid), NoAsk: f(q.NoAsk),
		YesAskSize: f(q.YesAskSize), NoAskSize: f(q.NoAskSize)}
	events := r.trader.Step(m, p, unix(at))

	offset, samples := r.trader.OffsetStatus()
	rec.Model = map[string]any{"sigma2": r.trader.Sigma2, "index_offset": offset, "offset_samples": samples}
	index := map[string]int{}
	for _, a := range r.trader.Accounts {
		v := a.View
		if v == nil {
			continue
		}
		b := r.setup.Buckets[a.Params.Name]
		d := store.DecisionRow{BucketID: b.ID, VersionID: b.VersionID, ModelProb: v.PModel, MarketProb: v.Mid,
			Edge: v.Best.Edge, Side: side(v.Best.Side), Action: "none", Why: v.Signal.Why}
		if !v.Signal.Bet {
			d.BlockedBy = v.Signal.Why
		}
		index[a.Params.Name] = len(rec.Decisions)
		rec.Decisions = append(rec.Decisions, d)
	}
	for _, e := range events {
		b := r.setup.Buckets[e.Strategy]
		i, journaled := index[e.Strategy]
		if !journaled {
			i = -1
		}
		lot := e.Lot
		switch e.Kind {
		case "bet":
			if journaled {
				n := lot.Contracts
				rec.Decisions[i].Action, rec.Decisions[i].SizeAlone = "enter", &n
			}
			rec.Trades = append(rec.Trades, store.TradeRow{DecisionIndex: i, BucketID: b.ID, BucketLedgerID: b.LedgerAccountID,
				Action: "buy", Side: side(lot.Side), Qty: lot.Contracts, Price: lot.Price, FeeCents: cents(lot.Fee),
				CashCents: cents(lot.Cost), Detail: lot})
		case "sold":
			if journaled && rec.Decisions[i].Action == "none" {
				rec.Decisions[i].Action = "exit"
			}
			rec.Trades = append(rec.Trades, store.TradeRow{DecisionIndex: i, BucketID: b.ID, BucketLedgerID: b.LedgerAccountID,
				Action: "sell", Side: side(lot.Side), Qty: lot.Contracts, Price: *lot.ExitPrice, FeeCents: cents(e.SellFee),
				CashCents: cents(*lot.Payout), Detail: lot})
		}
	}
	if err := r.db.RecordStep(ctx, r.setup, rec); err != nil {
		return r.halt(fmt.Errorf("recording a step: %w", err), len(events) > 0)
	}
	if len(events) > 0 {
		if err := r.db.SaveEngineState(ctx, r.Series, r.trader.Export()); err != nil {
			return r.halt(fmt.Errorf("saving state: %w", err), true)
		}
		for _, e := range events {
			slog.Info("sim trade", "series", r.Series, "strategy", e.Strategy, "kind", e.Kind, "side", e.Lot.Side,
				"contracts", e.Lot.Contracts, "price", e.Lot.Price)
		}
	}
	return nil
}

// halt stops the series when money moved in the engine but could not be recorded. A failure
// that moved nothing is only reported: the next second gets another try.
func (r *Runner) halt(err error, moneyMoved bool) error {
	if moneyMoved {
		r.halted = err.Error()
		slog.Error("series halted: simulated money moved but was not recorded", "series", r.Series, "err", err)
	}
	return err
}

func side(s string) string {
	if s == "UP" {
		return "yes"
	}
	return "no"
}

// Settled handles a round's result. The poller calls it once per round.
func (r *Runner) Settled(ctx context.Context, marketID int64, info kalshi.MarketInfo, closes time.Time, price string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.halted != "" {
		return nil
	}
	now := time.Now()
	var pp *float64
	if p := f(price); p != 0 {
		pp = &p
	}
	r.trader.NoteSettlement(unix(closes), info.ExpirationValue)
	events := r.trader.OnSettled(info.Ticker, info.Result, info.ExpirationValue, unix(now), pp)

	type key struct{ strategy, side string }
	sums := map[key]*store.SettlementRow{}
	var order []key
	for _, e := range events {
		b := r.setup.Buckets[e.Strategy]
		kk := key{e.Strategy, side(e.Lot.Side)}
		if sums[kk] == nil {
			sums[kk] = &store.SettlementRow{BucketID: b.ID, BucketLedgerID: b.LedgerAccountID, Side: kk.side}
			order = append(order, kk)
		}
		sums[kk].Qty += e.Lot.Contracts
		sums[kk].PayoutCents += cents(*e.Lot.Payout)
	}
	rows := make([]store.SettlementRow, 0, len(order))
	for _, kk := range order {
		rows = append(rows, *sums[kk])
	}
	if err := r.db.RecordSettlements(ctx, r.setup, marketID, now, rows); err != nil {
		return r.halt(fmt.Errorf("recording settlements: %w", err), len(rows) > 0)
	}
	if err := r.db.SaveEngineState(ctx, r.Series, r.trader.Export()); err != nil {
		return r.halt(fmt.Errorf("saving state: %w", err), len(rows) > 0)
	}
	return nil
}

// Snapshot is what the display needs: the leaderboard and every account's view and bet log.
func (r *Runner) Snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	offset, samples := r.trader.OffsetStatus()
	accounts := []map[string]any{}
	for _, a := range r.trader.Accounts {
		eq := a.Equity(r.trader.Market)
		log := a.Log
		if log == nil {
			log = []*k.Lot{} // an empty list, not null, for whoever reads the JSON
		}
		if len(log) > 80 {
			log = log[len(log)-80:]
		}
		open := []*k.Lot{}
		for _, lot := range a.Log {
			if lot.Status == "open" {
				open = append(open, lot)
			}
		}
		acct := map[string]any{"name": a.Params.Name, "blurb": a.Params.Blurb, "equity": eq, "cash": a.Cash,
			"pnl": eq - k.StartBalance, "pnl_pct": (eq/k.StartBalance - 1) * 100, "bets": a.Bets, "wins": a.Wins,
			"losses": a.Losses, "joined": a.Participated(), "log": log, "open": open, "log_total": len(a.Log)}
		if v := a.View; v != nil {
			acct["view"] = map[string]any{"p_model": v.PModel, "p_up": v.PUp, "mid": v.Mid, "side": v.Best.Side,
				"conf": v.Signal.Conf, "bet": v.Signal.Bet, "why": v.Signal.Why}
		}
		accounts = append(accounts, acct)
	}
	return map[string]any{"series": r.Series, "coin": r.Coin, "product": r.Product, "halted": r.halted,
		"start_balance": k.StartBalance, "rounds_monitored": r.trader.RoundsMonitored,
		"index_offset": offset, "offset_samples": samples, "accounts": accounts}
}
