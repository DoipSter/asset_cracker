package kalshi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	mu      sync.Mutex
	markets []string
	quotes  int
	results map[string]int // ticker -> how many times SaveResult was called
	ids     map[int64]string
}

func (f *fakeSink) SaveMarket(_ context.Context, m MarketInfo, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markets = append(f.markets, m.Ticker)
	id := int64(len(f.markets))
	f.ids[id] = m.Ticker
	return id, nil
}
func (f *fakeSink) SaveQuotes(context.Context, time.Time, int64, Quotes) error {
	f.mu.Lock()
	f.quotes++
	f.mu.Unlock()
	return nil
}
func (f *fakeSink) SaveResult(_ context.Context, id int64, _ MarketInfo) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[f.ids[id]]++
	return f.results[f.ids[id]] == 1, nil
}
func (f *fakeSink) Unsettled(context.Context, time.Time) (map[string]int64, error) { return nil, nil }

// The scenario that went wrong in the Python poller: a round closes, its result is published
// five seconds later, and the NEXT round does not appear for twenty seconds. The Python
// re-armed its watch on every pass in that gap and delivered the settlement again each second.
func TestSettlementIsRecordedOnceWhileTheNextRoundIsLate(t *testing.T) {
	closeAt := time.Date(2026, 9, 20, 22, 0, 0, 0, time.UTC)
	first, second := "KXBTC15M-26SEP202200-00", "KXBTC15M-26SEP202215-15"
	now := closeAt.Add(-3 * time.Second)
	strike := 81444.32

	market := func(ticker string, closes time.Time, result string) map[string]any {
		return map[string]any{"market": map[string]any{"ticker": ticker, "floor_strike": strike,
			"close_time": closes.Format(time.RFC3339), "result": result, "expiration_value": "81500.00",
			"settlement_ts": closes.Add(5 * time.Second).Format(time.RFC3339)}}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/orderbook"):
			_ = json.NewEncoder(w).Encode(map[string]any{"orderbook_fp": map[string]any{
				"yes_dollars": [][]string{{"0.4000", "10"}}, "no_dollars": [][]string{{"0.5900", "10"}}}})
		case strings.HasSuffix(path, "/markets"):
			_ = json.NewEncoder(w).Encode(map[string]any{"markets": []any{market(first, closeAt, "")["market"]}})
		case strings.HasSuffix(path, first):
			result := ""
			if !now.Before(closeAt.Add(5 * time.Second)) {
				result = "yes"
			}
			_ = json.NewEncoder(w).Encode(market(first, closeAt, result))
		case strings.HasSuffix(path, second):
			if now.Before(closeAt.Add(20 * time.Second)) {
				http.NotFound(w, r) // not published yet
				return
			}
			_ = json.NewEncoder(w).Encode(market(second, closeAt.Add(15*time.Minute), ""))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sink := &fakeSink{results: map[string]int{}, ids: map[int64]string{}}
	client := NewClient("test")
	client.base = srv.URL
	p := &Poller{Client: client, Sink: sink, Series: "KXBTC15M", Round: 15 * time.Minute, Now: func() time.Time { return now }}

	var current *round
	waiting := map[string]*awaiting{}
	for i := 0; i < 40; i++ { // forty one-second passes: 3 s before the close to 37 s after
		if err := p.step(context.Background(), &current, waiting); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		now = now.Add(time.Second)
	}

	if got := sink.results[first]; got != 1 {
		t.Errorf("settlement of %s recorded %d times, want exactly 1", first, got)
	}
	if len(waiting) != 0 {
		t.Errorf("still waiting on %d rounds, want 0", len(waiting))
	}
	if len(sink.markets) != 2 || sink.markets[1] != second {
		t.Errorf("followed rounds %v, want the first and then %s", sink.markets, second)
	}
	if current == nil || current.info.Ticker != second {
		t.Errorf("not following the second round at the end")
	}
	if sink.quotes == 0 {
		t.Error("no quotes were recorded")
	}
}
