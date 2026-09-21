package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/doipster/asset_cracker/service/internal/coinbase"
	"github.com/doipster/asset_cracker/service/internal/kalshi"
	k2 "github.com/doipster/asset_cracker/service/internal/kalshi15m2"
	"github.com/doipster/asset_cracker/service/internal/store"
)

// Coin2 is one coin the second engine version trades.
type Coin2 struct {
	Coin, Series, Product string
	Cal                   k2.Calibration
}

// Runner2 runs the second engine version live, in simulation: twelve accounts, one balance each
// shared across every coin, six originals and their six anti-world twins.
//
// A bucket per account holds its $1,000 in the ledger. When an original runs out the engine
// stakes it again; in the ledger that is the old bucket reaped and frozen and a NEW bucket
// ("... life 2") seeded in its place, so nothing is ever topped up and every life keeps its own
// record. A twin that runs out is reaped, frozen and not replaced.
type Runner2 struct {
	db     *store.Store
	setup  store.SimSetup
	mu     sync.Mutex
	trader *k2.Trader
	coins  map[string]Coin2 // by coin
	halted string
	last   map[string]string // account+coin -> the last journaled signal, to thin the journal
	lastAt map[string]float64
}

const engine2 = "kalshi15m2"

// NewRunner2 prepares the twelve buckets and restores the engine's saved state.
func NewRunner2(ctx context.Context, db *store.Store, coins []Coin2) (*Runner2, error) {
	var names, order []string
	cal := map[string]k2.Calibration{}
	byCoin := map[string]Coin2{}
	for _, p := range k2.AllStrategies() {
		names = append(names, p.Name)
	}
	for _, c := range coins {
		order = append(order, c.Coin)
		cal[c.Coin], byCoin[c.Coin] = c.Cal, c
	}
	setup, err := db.EnsureSimSetup(ctx, engine2, "kalshi15m", 2, names, cents(k2.StartBalance))
	if err != nil {
		return nil, fmt.Errorf("v2 sim setup: %w", err)
	}
	r := &Runner2{db: db, setup: setup, coins: byCoin, last: map[string]string{}, lastAt: map[string]float64{}}
	r.trader = k2.NewTrader(func() float64 { return unix(time.Now()) }, order, cal)
	var saved k2.SavedState
	if found, err := db.LoadEngineState(ctx, engine2, &saved); err != nil {
		return nil, fmt.Errorf("v2 load state: %w", err)
	} else if found {
		r.trader.Import(saved)
	}
	for _, a := range r.trader.Accounts { // the ledger is the record of money
		b := setup.Buckets[a.Params.Name]
		if a.Retired && b.Frozen {
			continue
		}
		if got := cents(a.Cash); got != b.CashCents {
			r.halted = fmt.Sprintf("%s: engine says %d cents, ledger says %d", a.Params.Name, got, b.CashCents)
			slog.Error("v2 halted: engine and ledger disagree", "detail", r.halted)
			break
		}
	}
	return r, nil
}

// Seed primes each coin's volatility and index offset from the exchanges.
func (r *Runner2) Seed(ctx context.Context, client *kalshi.Client, userAgent string) {
	for _, c := range r.coins {
		closes, measured := seedData(ctx, client, userAgent, c.Series, c.Product)
		r.mu.Lock()
		if len(closes) > 0 {
			r.trader.SeedVol(c.Coin, closes)
		}
		var fresh [][2]float64
		for _, m := range measured {
			if !r.trader.Coins[c.Coin].HasOffsetAt(m[0]) {
				fresh = append(fresh, m)
			}
		}
		r.trader.SeedOffsets(c.Coin, fresh)
		r.mu.Unlock()
	}
}

// Observe takes a trade print for whichever coin uses that product.
func (r *Runner2) Observe(t coinbase.Trade) {
	price, err := strconv.ParseFloat(t.Price, 64)
	if err != nil {
		return
	}
	r.mu.Lock()
	for _, c := range r.coins {
		if c.Product == t.Product {
			r.trader.Observe(c.Coin, price, unix(t.At))
		}
	}
	r.mu.Unlock()
}

// Step is one look at one coin's open round.
func (r *Runner2) Step(ctx context.Context, coin string, evalID int64, at time.Time, marketID int64, info kalshi.MarketInfo, closes time.Time, q kalshi.Quotes, price string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := f(price)
	if r.halted != "" || p == 0 {
		return nil
	}
	now := unix(at)
	m := k2.Market{Ticker: info.Ticker, Strike: *info.FloorStrike, Close: unix(closes), YesBid: f(q.YesBid), YesAsk: f(q.YesAsk),
		NoBid: f(q.NoBid), NoAsk: f(q.NoAsk), YesAskSize: f(q.YesAskSize), NoAskSize: f(q.NoAskSize)}
	events := r.trader.Step(coin, m, p, now)

	rec := store.StepRecord{EvaluationID: evalID, At: at, MarketID: marketID}
	index := map[string]int{}
	acted := map[string]bool{}
	for _, e := range events {
		acted[e.Strategy] = true
	}
	for _, a := range r.trader.Accounts {
		v := a.Views[coin]
		if a.Params.Anti || v == nil {
			continue
		}
		// Thirty rows a second would be a gigabyte every few days. A decision is journaled when
		// the strategy acted, when its answer changed, and every fifteen seconds regardless. The
		// market snapshot it looked at is stored every second, so any second can be recomputed.
		key, sig := a.Params.Name+coin, v.Best.Side+"|"+v.Signal.Why
		if !acted[a.Params.Name] && r.last[key] == sig && now-r.lastAt[key] < 15 {
			continue
		}
		r.last[key], r.lastAt[key] = sig, now
		b := r.setup.Buckets[a.Params.Name]
		d := store.DecisionRow{BucketID: b.ID, VersionID: b.VersionID, ModelProb: v.PModel, MarketProb: v.Mid, Edge: v.Best.Edge,
			Side: side(v.Best.Side), Action: "none", Why: v.Signal.Why}
		if !v.Signal.Bet {
			d.BlockedBy = v.Signal.Why
		}
		index[a.Params.Name] = len(rec.Decisions)
		rec.Decisions = append(rec.Decisions, d)
	}
	var closed []k2.Event
	for _, e := range events {
		if e.Kind == "bankrupt" {
			closed = append(closed, e)
			continue
		}
		b := r.setup.Buckets[e.Strategy]
		i, journaled := index[e.Strategy]
		if !journaled {
			i = -1 // a twin has no decision of its own: it mirrors
		}
		lot := e.Lot
		row := store.TradeRow{DecisionIndex: i, BucketID: b.ID, BucketLedgerID: b.LedgerAccountID, Side: side(lot.Side), Qty: lot.Contracts, Detail: lot}
		switch e.Kind {
		case "bet":
			if journaled {
				n := lot.Contracts
				rec.Decisions[i].Action, rec.Decisions[i].SizeAlone = "enter", &n
			}
			row.Action, row.Price, row.FeeCents, row.CashCents = "buy", lot.Price, cents(lot.Fee), cents(lot.Cost)
		case "sold":
			if journaled && rec.Decisions[i].Action == "none" {
				rec.Decisions[i].Action = "exit"
			}
			row.Action, row.Price, row.FeeCents, row.CashCents = "sell", *lot.ExitPrice, cents(e.SellFee), cents(*lot.Payout)
		}
		rec.Trades = append(rec.Trades, row)
	}
	moved := len(rec.Trades) > 0
	if len(rec.Decisions) > 0 || moved {
		if err := r.db.RecordStep(ctx, r.setup, rec); err != nil {
			return r.halt(fmt.Errorf("recording a v2 step: %w", err), moved)
		}
	}
	return r.afterEvents(ctx, events, closed, moved)
}

// Settled handles one round's result. The poller calls it once per round.
func (r *Runner2) Settled(ctx context.Context, coin string, marketID int64, info kalshi.MarketInfo, closes time.Time, price string) error {
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
	r.trader.NoteSettlement(coin, unix(closes), info.ExpirationValue)
	events := r.trader.OnSettled(info.Ticker, info.Result, info.ExpirationValue, unix(now), pp)

	type key struct{ strategy, side string }
	sums := map[key]*store.SettlementRow{}
	var order []key
	var closed []k2.Event
	for _, e := range events {
		if e.Kind == "bankrupt" {
			closed = append(closed, e)
			continue
		}
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
		return r.halt(fmt.Errorf("recording v2 settlements: %w", err), len(rows) > 0)
	}
	return r.afterEvents(ctx, events, closed, true)
}

// afterEvents closes and replaces the buckets of accounts that ran out, then saves the state.
func (r *Runner2) afterEvents(ctx context.Context, events, closed []k2.Event, save bool) error {
	for _, e := range closed {
		b := r.setup.Buckets[e.Strategy]
		reason := fmt.Sprintf("ran out: $%.2f left after %d rounds", e.DiedWith, e.Rounds)
		next, err := r.db.CloseBucket(ctx, r.setup, b, reason, !e.Retired, e.Life+1, cents(k2.StartBalance))
		if err != nil {
			return r.halt(fmt.Errorf("closing %s: %w", b.Name, err), true)
		}
		r.setup.Buckets[e.Strategy] = next
		slog.Warn("v2 account ran out", "strategy", e.Strategy, "left", e.DiedWith, "retired", e.Retired, "now", next.Name)
		save = true
	}
	if save && len(events) > 0 {
		if err := r.db.SaveEngineState(ctx, engine2, r.trader.Export()); err != nil {
			return r.halt(fmt.Errorf("saving v2 state: %w", err), true)
		}
	}
	for _, e := range events {
		if e.Kind == "bet" || e.Kind == "sold" {
			slog.Info("sim trade", "engine", "v2", "strategy", e.Strategy, "coin", e.Lot.Coin, "kind", e.Kind, "side", e.Lot.Side,
				"contracts", e.Lot.Contracts, "price", e.Lot.Price, "why", e.Lot.Why)
		}
	}
	return nil
}

func (r *Runner2) halt(err error, moneyMoved bool) error {
	if moneyMoved {
		r.halted = err.Error()
		slog.Error("v2 halted: simulated money moved but was not recorded", "err", err)
	}
	return err
}

// Snapshot is what the display needs: both worlds' leaderboards, every account's bet log and
// its view of each coin, and each coin's learned index offset.
func (r *Runner2) Snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	mk := r.trader.Markets()
	accounts := []map[string]any{}
	for _, a := range r.trader.Accounts {
		eq := a.Equity(mk)
		log := a.Log
		if len(log) > 120 {
			log = log[len(log)-120:]
		}
		if log == nil {
			log = []*k2.Lot{}
		}
		views := map[string]any{}
		for coin, v := range a.Views {
			if v != nil {
				views[coin] = map[string]any{"p_model": v.PModel, "p_up": v.PUp, "mid": v.Mid, "side": v.Best.Side,
					"conf": v.Signal.Conf, "bet": v.Signal.Bet, "why": v.Signal.Why}
			}
		}
		accounts = append(accounts, map[string]any{"name": a.Params.Name, "blurb": a.Params.Blurb, "anti": a.Params.Anti,
			"equity": eq, "cash": a.Cash, "pnl": eq - k2.StartBalance, "pnl_pct": (eq/k2.StartBalance - 1) * 100,
			"bets": a.Bets, "wins": a.Wins, "losses": a.Losses, "joined": a.Participated(), "log": log, "log_total": len(a.Log),
			"views": views, "bankruptcies": a.Bankruptcies, "retired": a.Retired, "at_risk": a.Committed()})
	}
	coins := map[string]any{}
	for name, c := range r.trader.Coins {
		offset, samples := c.OffsetStatus()
		coins[name] = map[string]any{"index_offset": offset, "offset_samples": samples, "rounds_monitored": c.RoundsMonitored}
	}
	return map[string]any{"engine": "v2", "halted": r.halted, "start_balance": k2.StartBalance, "total_cap": k2.TotalCap * k2.StartBalance,
		"rounds_monitored": r.trader.RoundsMonitored, "accounts": accounts, "coins": coins}
}
