package kalshi

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Sink is where the poller puts what it learns. The store implements it; tests use a fake.
type Sink interface {
	// SaveMarket records a round and returns its id.
	SaveMarket(ctx context.Context, m MarketInfo, closes time.Time) (int64, error)
	// SaveQuotes records the live quotes for a round at one moment.
	SaveQuotes(ctx context.Context, at time.Time, marketID int64, q Quotes) error
	// SaveResult records how a round settled. It reports true only for the call that stored
	// it, so whatever acts on a settlement acts once.
	SaveResult(ctx context.Context, marketID int64, m MarketInfo) (bool, error)
	// Unsettled lists rounds that closed recently with no result stored: ticker -> id.
	Unsettled(ctx context.Context, before time.Time) (map[string]int64, error)
}

// Status is what the health endpoint reports about one series.
type Status struct {
	Ticker       string    `json:"ticker"`
	Closes       time.Time `json:"closes"`
	LastQuotesAt time.Time `json:"last_quotes_at"`
	Awaiting     int       `json:"awaiting_results"`
	LastError    string    `json:"last_error,omitempty"`
}

type round struct {
	info   MarketInfo
	id     int64
	closes time.Time
	queued bool // already handed to the results watch
}

type awaiting struct {
	id      int64
	closes  time.Time
	nextTry time.Time
}

// Poller follows one series: which round is open, its live quotes once a second, and how each
// round settles.
type Poller struct {
	Client *Client
	Sink   Sink
	Series string
	Round  time.Duration
	Now    func() time.Time

	mu     sync.RWMutex
	status Status
}

// Status returns a snapshot for the health endpoint.
func (p *Poller) Status() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status
}

// giveUpAfter is how long to keep asking for a round's result. Kalshi normally reports it
// about five seconds after the close.
const giveUpAfter = 15 * time.Minute

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	if p.Now == nil {
		p.Now = time.Now
	}
	var current *round
	waiting := map[string]*awaiting{}
	if old, err := p.Sink.Unsettled(ctx, p.Now()); err == nil {
		for ticker, id := range old { // rounds a previous run left without a result
			waiting[ticker] = &awaiting{id: id, closes: p.Now().Add(-time.Minute)}
		}
	}

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		err := p.step(ctx, &current, waiting)
		p.mu.Lock()
		p.status.Awaiting = len(waiting)
		p.status.LastError = ""
		if err != nil {
			p.status.LastError = err.Error()
		}
		p.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			slog.Warn("kalshi poll", "series", p.Series, "err", err)
			select { // back off before retrying
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (p *Poller) step(ctx context.Context, current **round, waiting map[string]*awaiting) error {
	now := p.Now()
	cur := *current

	switch {
	case cur == nil:
		found, err := p.findOpen(ctx, now)
		if err != nil {
			return err
		}
		cur = found
	case !now.Before(cur.closes):
		// The round has ended. Hand it to the results watch exactly once. (The Python poller
		// re-armed this on every pass until the next round appeared, so one settlement was
		// delivered up to seven times; see docs/flutter-port-scope.md.)
		if !cur.queued {
			cur.queued = true
			waiting[cur.info.Ticker] = &awaiting{id: cur.id, closes: cur.closes}
		}
		next, err := p.findNext(ctx, cur, now)
		if err != nil {
			return err
		}
		if next != nil {
			cur = next
		}
	}
	*current = cur

	if cur != nil && now.Before(cur.closes) {
		q, err := p.Client.OrderBook(ctx, cur.info.Ticker)
		if err != nil {
			return err
		}
		if err := p.Sink.SaveQuotes(ctx, now, cur.id, q); err != nil {
			return err
		}
		p.mu.Lock()
		p.status.Ticker, p.status.Closes, p.status.LastQuotesAt = cur.info.Ticker, cur.closes, now
		p.mu.Unlock()
	}

	for ticker, w := range waiting {
		if now.Before(w.closes.Add(time.Second)) || now.Before(w.nextTry) {
			continue
		}
		m, err := p.Client.Market(ctx, ticker)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil && (m.Result == "yes" || m.Result == "no") {
			first, err := p.Sink.SaveResult(ctx, w.id, m)
			if err != nil {
				return err
			}
			if first {
				slog.Info("round settled", "ticker", ticker, "result", m.Result, "value", m.ExpirationValue)
			}
			delete(waiting, ticker)
			continue
		}
		switch age := now.Sub(w.closes); {
		case age > giveUpAfter:
			slog.Warn("no result for round; giving up", "ticker", ticker)
			delete(waiting, ticker)
		case age > time.Minute:
			w.nextTry = now.Add(10 * time.Second)
		}
	}
	return nil
}

// findOpen asks the (slowly refreshed) list for the open round that closes soonest.
func (p *Poller) findOpen(ctx context.Context, now time.Time) (*round, error) {
	markets, err := p.Client.OpenMarkets(ctx, p.Series)
	if err != nil {
		return nil, err
	}
	var best *MarketInfo
	var bestCloses time.Time
	for i := range markets {
		m := &markets[i]
		closes, err := m.Closes()
		if err != nil || m.FloorStrike == nil || !closes.After(now) {
			continue
		}
		if best == nil || closes.Before(bestCloses) {
			best, bestCloses = m, closes
		}
	}
	if best == nil {
		return nil, nil
	}
	return p.adopt(ctx, *best, bestCloses)
}

// findNext looks for the round after cur: first by predicting its ticker, then, once that has
// plainly not worked, through the list.
func (p *Poller) findNext(ctx context.Context, cur *round, now time.Time) (*round, error) {
	if ticker, ok := NextTicker(cur.info.Ticker, p.Round); ok {
		m, err := p.Client.Market(ctx, ticker)
		if err == nil {
			if closes, cerr := m.Closes(); cerr == nil && m.FloorStrike != nil && closes.After(now) {
				return p.adopt(ctx, m, closes)
			}
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if now.After(cur.closes.Add(time.Minute)) {
		return p.findOpen(ctx, now)
	}
	return nil, nil // not published yet; try again next second
}

func (p *Poller) adopt(ctx context.Context, m MarketInfo, closes time.Time) (*round, error) {
	id, err := p.Sink.SaveMarket(ctx, m, closes)
	if err != nil {
		return nil, err
	}
	slog.Info("following round", "ticker", m.Ticker, "strike", *m.FloorStrike, "closes", closes.UTC().Format(time.RFC3339))
	return &round{info: m, id: id, closes: closes}, nil
}
