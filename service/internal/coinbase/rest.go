package coinbase

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"
)

const restURL = "https://api.exchange.coinbase.com/products"

// Candle is one bar. Coinbase sends [start, low, high, open, close, volume].
type Candle struct {
	Start                  time.Time
	Low, High, Open, Close float64
}

var restClient = &http.Client{Timeout: 10 * time.Second}

// Candles fetches bars of `granularity` seconds, oldest first. With zero times it returns the
// most recent ~300 bars.
func Candles(ctx context.Context, userAgent, product string, granularity int, start, end time.Time) ([]Candle, error) {
	q := url.Values{"granularity": {fmt.Sprint(granularity)}}
	if !start.IsZero() {
		q.Set("start", start.UTC().Format(time.RFC3339))
		q.Set("end", end.UTC().Format(time.RFC3339))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, restURL+"/"+url.PathEscape(product)+"/candles?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := restClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coinbase: HTTP %d for %s candles", resp.StatusCode, product)
	}
	var rows [][]float64
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	out := make([]Candle, 0, len(rows))
	for _, r := range rows {
		if len(r) >= 5 {
			out = append(out, Candle{Start: time.Unix(int64(r[0]), 0), Low: r[1], High: r[2], Open: r[3], Close: r[4]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}
