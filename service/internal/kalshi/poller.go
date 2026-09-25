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
	SaveQuotes(ctx context.Context, at time.Time, marketID int64, m MarketInfo, closes time.Time, q Quotes) error
	// SaveResult records how a round settled. It reports true only for the call that stored
	// it, so whatever acts on a settlement acts once.
	SaveResult(ctx context.Context, marketID int64, m MarketInfo, closes time.Time) (bool, error)
	// Unsettled lists the closed rounds with no result stored that can still get one, by ticker:
	// every round a bucket traded, and any other for an hour after its close
	// (store.UnsettledMarkets). The watch follows this list, at the start and every resweepEvery.
	Unsettled(ctx context.Context, before time.Time) (map[string]Pending, error)
}

// Status is what the health endpoint reports about one series.
type Status struct {
	Ticker       string    `json:"ticker"`
	Strike       float64   `json:"strike"`
	Closes       time.Time `json:"closes"`
	Opens        time.Time `json:"-"` // Kalshi's own open time for the round, zero if it sent none. Not in the status document, which does not change
	Quotes       Quotes    `json:"quotes"`
	LastQuotesAt time.Time `json:"last_quotes_at"`
	Awaiting     int       `json:"awaiting_results"`
	// NotBinary counts the rounds awaited whose result Kalshi gave as something other than yes or
	// no ("scalar", the only other value its API lists). Nothing settles them: they are logged as
	// errors and asked about until someone decides.
	NotBinary int    `json:"results_not_yes_or_no"`
	LastError string `json:"last_error,omitempty"`
}

// Pending is a closed round still waiting for its result.
type Pending struct {
	ID     int64
	Closes time.Time
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
	other   string // a result that is neither yes nor no, once seen: logged once, counted
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

// LateAfter is when a round's result is late. Kalshi normally reports it about five seconds after
// the close. Past this the watch asks once a minute, and the value snapshot treats a bet in the
// round as a fault (runner.SnapshotRefusal).
const LateAfter = 15 * time.Minute

// The watch's pace [CONVENTIONS]: every pass for a minute after the close, then every ten seconds,
// then once a minute once the result is late. And how often the watch is brought back in line with
// the record's rule (Sink.Unsettled): it used to give up on every round after LateAfter while a
// restart asked again about a traded one, so a late result waited for a restart, and until then
// the bets in the round stayed open, no value snapshot was written and nothing was skimmed.
const (
	resweepEvery = time.Minute
	askSoon      = 10 * time.Second
	askLate      = time.Minute
	untradedFor  = time.Hour // store.UnsettledMarkets' own hour for a round nobody traded
)

// resweep makes the watch the record's list: adds every round it lists, and drops a round it no
// longer lists once that round is past untradedFor (settled, or nobody traded it). A round closed
// within the hour is kept whether listed or not: the list is read as of `now`, and a round closing
// this second may not be in it yet.
func (p *Poller) resweep(ctx context.Context, now time.Time, waiting map[string]*awaiting) error {
	listed, err := p.Sink.Unsettled(ctx, now)
	if err != nil {
		return err
	}
	for ticker, pd := range listed {
		if _, ok := waiting[ticker]; !ok {
			waiting[ticker] = &awaiting{id: pd.ID, closes: pd.Closes}
		}
	}
	for ticker, w := range waiting {
		if _, ok := listed[ticker]; !ok && now.Sub(w.closes) > untradedFor {
			delete(waiting, ticker)
		}
	}
	return nil
}

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	if p.Now == nil {
		p.Now = time.Now
	}
	var current *round
	waiting := map[string]*awaiting{}
	var swept time.Time
	sweep := func() {
		now := p.Now()
		if now.Sub(swept) < resweepEvery {
			return
		}
		swept = now // a failed read is tried again at the next sweep, not every second
		if err := p.resweep(ctx, now, waiting); err != nil && ctx.Err() == nil {
			slog.Warn("kalshi poll: the unsettled rounds were not read; the watch keeps what it has", "series", p.Series, "err", err)
		}
	}
	sweep() // rounds a previous run left without a result

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		sweep()
		err := p.step(ctx, &current, waiting)
		other := 0
		for _, w := range waiting {
			if w.other != "" {
				other++
			}
		}
		p.mu.Lock()
		p.status.Awaiting, p.status.NotBinary = len(waiting), other
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
		if err := p.Sink.SaveQuotes(ctx, now, cur.id, cur.info, cur.closes, q); err != nil {
			return err
		}
		p.mu.Lock()
		p.status.Ticker, p.status.Closes, p.status.LastQuotesAt = cur.info.Ticker, cur.closes, now
		p.status.Opens = cur.info.Opens()
		p.status.Quotes, p.status.Strike = q, *cur.info.FloorStrike
		p.mu.Unlock()
	}
	return p.askResults(ctx, now, waiting)
}

// askResults asks Kalshi for the result of every awaited round that is due, stores a yes or no,
// and paces the rest.
func (p *Poller) askResults(ctx context.Context, now time.Time, waiting map[string]*awaiting) error {
	for ticker, w := range waiting {
		if now.Before(w.closes.Add(time.Second)) || now.Before(w.nextTry) {
			continue
		}
		m, err := p.Client.Market(ctx, ticker)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil && (m.Result == "yes" || m.Result == "no") {
			first, err := p.Sink.SaveResult(ctx, w.id, m, w.closes)
			if err != nil {
				return err
			}
			if first {
				slog.Info("round settled", "ticker", ticker, "result", m.Result, "value", m.ExpirationValue)
			}
			delete(waiting, ticker)
			continue
		}
		if err == nil && m.Result != "" && m.Result != w.other {
			// Kalshi's API lists one other result, "scalar", which pays the YES side its settlement
			// value. Nothing here settles on it: said loudly, once, and asked about again.
			w.other = m.Result
			slog.Error("round determined neither yes nor no; its bets stay open until someone decides how it pays",
				"series", p.Series, "ticker", ticker, "result", m.Result, "closed", w.closes.UTC().Format(time.RFC3339))
		}
		// Not given up: the watch follows the record's rule (resweep), which keeps a traded round
		// until its result is stored and lets another go an hour after its close.
		switch age := now.Sub(w.closes); {
		case age > LateAfter:
			w.nextTry = now.Add(askLate)
		case age > time.Minute:
			w.nextTry = now.Add(askSoon)
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
