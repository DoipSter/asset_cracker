// Package coinbase streams public trade prints from Coinbase Exchange.
//
// Market data only. There is no authentication and no order code here.
package coinbase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const feedURL = "wss://ws-feed.exchange.coinbase.com"

// silence is how long the feed may go quiet before the link is treated as dead. BTC and ETH
// trade many times a second, so 15 seconds of nothing means the connection has gone.
const silence = 15 * time.Second

// Trade is one print from the "matches" channel.
type Trade struct {
	Product    string
	At         time.Time // the exchange's timestamp
	ReceivedAt time.Time
	Price      string // decimal text as sent
	Size       string
}

// Latest remembers the most recent trade per product, for anything that needs "the price now".
type Latest struct {
	mu sync.RWMutex
	m  map[string]Trade
}

// NewLatest returns an empty Latest.
func NewLatest() *Latest { return &Latest{m: map[string]Trade{}} }

func (l *Latest) set(t Trade) {
	l.mu.Lock()
	l.m[t.Product] = t
	l.mu.Unlock()
}

// Get returns the last trade seen for a product.
func (l *Latest) Get(product string) (Trade, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t, ok := l.m[product]
	return t, ok
}

type message struct {
	Type      string `json:"type"`
	ProductID string `json:"product_id"`
	Price     string `json:"price"`
	Size      string `json:"size"`
	Time      string `json:"time"`
	Message   string `json:"message"`
}

// resubscribeEvery is how often a live connection asks products() again and subscribes to what
// was added since (and unsubscribes from what was dropped). An asset switched on from the page
// gets a live price within about this long.
const resubscribeEvery = 20 * time.Second

// Stream connects, subscribes to the trade stream for products(), and calls onTrade for every
// print until ctx ends. It reconnects by itself, backing off up to 30 seconds, and while
// connected it follows products(): a product added to the answer is subscribed to, one removed
// is unsubscribed from, without a reconnection.
//
// The "matches" channel is the raw trade stream. "ticker" carries the same prices but was
// measured to arrive up to ~150 ms later, unevenly (see the Python app's notes).
func Stream(ctx context.Context, userAgent string, products func() []string, latest *Latest, onTrade func(Trade)) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := run(ctx, userAgent, products, latest, onTrade)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second // it had been working: start the backoff over
		}
		slog.Warn("coinbase feed dropped", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func run(ctx context.Context, userAgent string, products func() []string, latest *Latest, onTrade func(Trade)) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, feedURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"User-Agent": []string{userAgent}},
	})
	cancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	want := products()
	if err := subscribe(ctx, conn, "subscribe", want); err != nil {
		return err
	}
	slog.Info("coinbase feed connected", "products", want)

	// Follow products() for as long as this connection lives. Write is safe to call from here
	// while the loop below reads; a failed write ends the connection through the read loop.
	followCtx, stopFollowing := context.WithCancel(ctx)
	defer stopFollowing()
	go follow(followCtx, conn, products, want)

	for {
		readCtx, cancel := context.WithTimeout(ctx, silence)
		_, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		received := time.Now()
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch m.Type {
		case "match":
			at, err := time.Parse(time.RFC3339Nano, m.Time)
			if err != nil || m.Price == "" {
				continue
			}
			t := Trade{Product: m.ProductID, At: at, ReceivedAt: received, Price: m.Price, Size: m.Size}
			latest.set(t)
			onTrade(t)
		case "last_match":
			// Sent once on subscribing: the most recent trade before we connected. Useful as
			// the current price, but it is a replay, so it is not recorded as a new print.
			if at, err := time.Parse(time.RFC3339Nano, m.Time); err == nil && m.Price != "" {
				latest.set(Trade{Product: m.ProductID, At: at, ReceivedAt: received, Price: m.Price, Size: m.Size})
			}
		case "error":
			return fmt.Errorf("feed error: %s", m.Message)
		}
	}
}

// subscriptionDiff is what to subscribe to and what to drop to make current equal want. Both
// answers are sorted, so the messages and the log read the same every time.
func subscriptionDiff(current map[string]bool, want []string) (add, drop []string) {
	wanted := map[string]bool{}
	for _, p := range want {
		wanted[p] = true
		if !current[p] {
			add = append(add, p)
		}
	}
	for p := range current {
		if !wanted[p] {
			drop = append(drop, p)
		}
	}
	sort.Strings(add)
	sort.Strings(drop)
	return add, drop
}

func subscribe(ctx context.Context, conn *websocket.Conn, kind string, products []string) error {
	if len(products) == 0 {
		return nil
	}
	msg, _ := json.Marshal(map[string]any{"type": kind, "product_ids": products, "channels": []string{"matches"}})
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}
	return nil
}

// follow diffs products() against what is subscribed every resubscribeEvery and sends the
// difference, until ctx ends (the connection is gone or the service is stopping).
func follow(ctx context.Context, conn *websocket.Conn, products func() []string, have []string) {
	t := time.NewTicker(resubscribeEvery)
	defer t.Stop()
	current := map[string]bool{}
	for _, p := range have {
		current[p] = true
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		add, drop := subscriptionDiff(current, products())
		if len(add) == 0 && len(drop) == 0 {
			continue
		}
		if err := subscribe(ctx, conn, "subscribe", add); err != nil {
			slog.Warn("coinbase feed could not add products; the reconnection will", "products", add, "err", err)
			return
		}
		if err := subscribe(ctx, conn, "unsubscribe", drop); err != nil {
			slog.Warn("coinbase feed could not drop products; the reconnection will", "products", drop, "err", err)
			return
		}
		for _, p := range add {
			current[p] = true
		}
		for _, p := range drop {
			delete(current, p)
		}
		slog.Info("coinbase feed follows the tracked assets", "added", add, "dropped", drop)
	}
}
