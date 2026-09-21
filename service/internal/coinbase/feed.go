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

// Stream connects, subscribes to the trade stream for products, and calls onTrade for every
// print until ctx ends. It reconnects by itself, backing off up to 30 seconds.
//
// The "matches" channel is the raw trade stream. "ticker" carries the same prices but was
// measured to arrive up to ~150 ms later, unevenly (see the Python app's notes).
func Stream(ctx context.Context, userAgent string, products []string, latest *Latest, onTrade func(Trade)) {
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

func run(ctx context.Context, userAgent string, products []string, latest *Latest, onTrade func(Trade)) error {
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

	sub, _ := json.Marshal(map[string]any{
		"type": "subscribe", "product_ids": products, "channels": []string{"matches"},
	})
	if err := conn.Write(ctx, websocket.MessageText, sub); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	slog.Info("coinbase feed connected", "products", products)

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
