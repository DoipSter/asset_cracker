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
	rows, err := candleRows(ctx, userAgent, product, granularity, start, end)
	if err != nil {
		return nil, err
	}
	out := make([]Candle, 0, len(rows))
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		var f [5]float64
		for i := range f {
			if f[i], err = r[i].Float64(); err != nil {
				return nil, fmt.Errorf("coinbase: %s candle field %d: %w", product, i, err)
			}
		}
		out = append(out, Candle{Start: time.Unix(int64(f[0]), 0), Low: f[1], High: f[2], Open: f[3], Close: f[4]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// Bar is one candle with its numbers as the decimal text Coinbase sent, for storing as numeric
// without a trip through float64. Start is the candle's START time.
type Bar struct {
	Start                          time.Time
	Low, High, Open, Close, Volume string
}

// Bars fetches the candles whose start is in [start, end] (Coinbase includes both ends: checked
// with one daily request on 2026-09-21, which also showed six fields per row and newest first),
// oldest first. At most 300 candles per request; a longer range is refused by Coinbase.
func Bars(ctx context.Context, userAgent, product string, granularity int, start, end time.Time) ([]Bar, error) {
	rows, err := candleRows(ctx, userAgent, product, granularity, start, end)
	if err != nil {
		return nil, err
	}
	out := make([]Bar, 0, len(rows))
	for _, r := range rows {
		if len(r) < 6 {
			return nil, fmt.Errorf("coinbase: %s candle row has %d fields, want 6", product, len(r))
		}
		t, err := r[0].Int64()
		if err != nil {
			return nil, fmt.Errorf("coinbase: %s candle time: %w", product, err)
		}
		out = append(out, Bar{Start: time.Unix(t, 0).UTC(), Low: r[1].String(), High: r[2].String(),
			Open: r[3].String(), Close: r[4].String(), Volume: r[5].String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// candleRows is one candles request, its numbers kept as the text Coinbase sent.
func candleRows(ctx context.Context, userAgent, product string, granularity int, start, end time.Time) ([][]json.Number, error) {
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
	var rows [][]json.Number
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}
