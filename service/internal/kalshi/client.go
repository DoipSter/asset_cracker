// Package kalshi reads Kalshi's public market data.
//
// Read-only. Every request is an unauthenticated GET; there is no order code and no
// credential handling in this package.
package kalshi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const baseURL = "https://api.elections.kalshi.com/trade-api/v2"

// Client fetches public market data.
type Client struct {
	http      *http.Client
	userAgent string
	base      string
}

// NewClient returns a client that reuses one connection.
func NewClient(userAgent string) *Client {
	return &Client{http: &http.Client{Timeout: 8 * time.Second}, userAgent: userAgent, base: baseURL}
}

// ErrNotFound means Kalshi has no such market (yet).
var ErrNotFound = fmt.Errorf("kalshi: not found")

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // always drain, so the connection is reused
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("kalshi: HTTP %d for %s", resp.StatusCode, path)
	}
	return json.Unmarshal(body, out)
}

// MarketInfo is the part of a Kalshi market this service uses. Money and values arrive as
// decimal text and are kept that way.
type MarketInfo struct {
	Ticker          string   `json:"ticker"`
	FloorStrike     *float64 `json:"floor_strike"`
	OpenTime        string   `json:"open_time"`
	CloseTime       string   `json:"close_time"`
	Status          string   `json:"status"`
	Result          string   `json:"result"`
	ExpirationValue string   `json:"expiration_value"`
	SettlementTS    string   `json:"settlement_ts"`
}

// Closes parses the close time.
func (m MarketInfo) Closes() (time.Time, error) { return time.Parse(time.RFC3339Nano, m.CloseTime) }

// Opens parses the open time; zero if absent.
func (m MarketInfo) Opens() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, m.OpenTime)
	return t
}

// OpenMarkets lists a series' open markets. The list is cached by Kalshi for several seconds:
// use it to learn which round is open and its strike, never for prices.
func (c *Client) OpenMarkets(ctx context.Context, series string) ([]MarketInfo, error) {
	var out struct {
		Markets []MarketInfo `json:"markets"`
	}
	err := c.get(ctx, "/markets?series_ticker="+url.QueryEscape(series)+"&status=open&limit=10", &out)
	return out.Markets, err
}

// SettledMarkets lists a series' most recently settled markets, for measuring how far Kalshi's
// index has been sitting above our exchange's price before we started watching.
func (c *Client) SettledMarkets(ctx context.Context, series string, n int) ([]MarketInfo, error) {
	var out struct {
		Markets []MarketInfo `json:"markets"`
	}
	err := c.get(ctx, fmt.Sprintf("/markets?series_ticker=%s&status=settled&limit=%d", url.QueryEscape(series), n), &out)
	return out.Markets, err
}

// Market fetches one market by ticker.
func (c *Client) Market(ctx context.Context, ticker string) (MarketInfo, error) {
	var out struct {
		Market MarketInfo `json:"market"`
	}
	err := c.get(ctx, "/markets/"+url.PathEscape(ticker), &out)
	return out.Market, err
}

// OrderBook fetches a market's live book and returns its top-of-book quotes.
func (c *Client) OrderBook(ctx context.Context, ticker string) (Quotes, error) {
	var out struct {
		Book struct {
			Yes [][]string `json:"yes_dollars"`
			No  [][]string `json:"no_dollars"`
		} `json:"orderbook_fp"`
	}
	if err := c.get(ctx, "/markets/"+url.PathEscape(ticker)+"/orderbook", &out); err != nil {
		return Quotes{}, err
	}
	return BookQuotes(out.Book.Yes, out.Book.No)
}
