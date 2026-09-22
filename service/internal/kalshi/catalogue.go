package kalshi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// NewPacedClient is a client whose every request first waits its turn at pace. It is for what the
// assets page switches on (app/assets.go): the catalogue's reads and the 15-minute Poller, which
// has no pacing of its own, share the ladder recorders' pacer, so together they add no more than
// that pacer allows. A LadderRecorder paces itself: give it a plain client.
func NewPacedClient(userAgent string, pace *Pacer) *Client {
	c := NewClient(userAgent)
	c.http.Transport = pacedTransport{pace: pace, next: http.DefaultTransport}
	return c
}

type pacedTransport struct {
	pace *Pacer
	next http.RoundTripper
}

func (t pacedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.pace.Wait(r.Context()); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(r)
}

// SeriesList is GET /series for one category, as received.
func (c *Client) SeriesList(ctx context.Context, category string) ([]byte, error) {
	var raw json.RawMessage
	err := c.get(ctx, "/series?category="+url.QueryEscape(category), &raw)
	return raw, err
}

// SampleMarket is what the catalogue reads of an open market to tell what kind of series it is.
type SampleMarket struct {
	StrikeType  string   `json:"strike_type"`
	FloorStrike *float64 `json:"floor_strike"`
	CloseTime   string   `json:"close_time"`
}

// OpenSample is up to n of a series' open markets.
func (c *Client) OpenSample(ctx context.Context, series string, n int) ([]SampleMarket, error) {
	var out struct {
		Markets []SampleMarket `json:"markets"`
	}
	err := c.get(ctx, fmt.Sprintf("/markets?series_ticker=%s&status=open&limit=%d", url.QueryEscape(series), n), &out)
	return out.Markets, err
}
