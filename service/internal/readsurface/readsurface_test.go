package readsurface

import (
	"testing"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var now = time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)

func TestParseTime(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"now", now},
		{"-7d", now.Add(-7 * 24 * time.Hour)},
		{"now-36h", now.Add(-36 * time.Hour)},
		{"-2w", now.Add(-14 * 24 * time.Hour)},
		{"-90m", now.Add(-90 * time.Minute)},
		{"2026-09-01", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-09-01T12:30:00Z", time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)},
		{"2026-09-01T12:30:00-07:00", time.Date(2026, 9, 1, 19, 30, 0, 0, time.UTC)},
		{"2026-09-01T12:30", time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := parseTime(c.in, now)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%q: got %s, want %s", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "yesterday", "-7", "7d", "-x", "2026-13-01"} {
		if _, err := parseTime(bad, now); err == nil {
			t.Errorf("%q parsed; it should not", bad)
		}
	}
}

func TestBounds(t *testing.T) {
	r := &Surface{now: func() time.Time { return now }}
	from, to, err := r.bounds(Window{From: "-1d"})
	if err != nil || !to.Equal(now) || !from.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("default to: %s %s %v", from, to, err)
	}
	if _, _, err := r.bounds(Window{}); err == nil {
		t.Fatal("a window without from must be refused")
	}
	if _, _, err := r.bounds(Window{From: "2026-09-02", To: "2026-09-01"}); err == nil {
		t.Fatal("a window ending before it starts must be refused")
	}
}

func TestLimitsAndCaps(t *testing.T) {
	if limit(0) != DefaultLimit || limit(-3) != DefaultLimit || limit(10) != 10 || limit(MaxLimit+1) != MaxLimit {
		t.Fatal("limit caps are wrong")
	}
	from := now.Add(-10 * time.Hour)
	// 600 one-minute buckets in ten hours, cap 500: the window is cut to from + 500 min.
	start, to, cut := capWindow(from, now, time.Minute, 500)
	if !cut || !start.Equal(from) || !to.Equal(from.Add(500*time.Minute)) {
		t.Fatalf("capWindow: %s %s %v", start, to, cut)
	}
	// 10 hourly buckets in ten hours: nothing to cut.
	if _, to, cut := capWindow(from, now, time.Hour, 500); cut || !to.Equal(now) {
		t.Fatalf("capWindow should not cut: %s %v", to, cut)
	}
	// A start off the grid is moved down onto it, so the capped window holds whole bins: 5
	// five-minute bins from 23:47:52 are 23:45 .. 00:10, and the end is 00:10, not 00:12:52.
	off := time.Date(2026, 9, 21, 23, 47, 52, 0, time.UTC)
	start, to, cut = capWindow(off, now, 5*time.Minute, 5)
	if !cut || !start.Equal(time.Date(2026, 9, 21, 23, 45, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 9, 22, 0, 10, 0, 0, time.UTC)) {
		t.Fatalf("capWindow alignment: %s %s %v", start, to, cut)
	}
	if got := alignDown(time.Date(1969, 12, 31, 23, 59, 0, 0, time.UTC), time.Hour); !got.Equal(time.Date(1969, 12, 31, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("alignDown before the epoch: %s", got)
	}
	if _, err := granularity(900); err == nil {
		t.Fatal("15-minute candles are not stored and must be refused")
	}
	if g, _ := granularity(0); g != 3600 {
		t.Fatal("the default granularity is hourly")
	}
	if err := checkBars("lookbacks", []int{1, 2, 3, 4, 5, 6, 7, 8, 9}, 8); err == nil {
		t.Fatal("nine lookbacks must be refused")
	}
	if err := checkBars("lookbacks", []int{1, 1}, 8); err == nil {
		t.Fatal("a repeated lookback must be refused")
	}
	if err := checkBars("lookbacks", []int{0}, 8); err == nil {
		t.Fatal("a zero lookback must be refused")
	}
}

func TestPage(t *testing.T) {
	a, b, c := now.Add(-3*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour)
	full := store.Table{Columns: []string{"at", "x"}, Rows: [][]any{{a, 1.0}, {b, 2.0}, {c, 3.0}}}
	got := page(full, 2)
	if !got.Truncated || len(got.Rows) != 2 || got.Next != b.Format(time.RFC3339Nano) {
		t.Fatalf("page: %+v", got)
	}
	got = page(full, 3)
	if got.Truncated || got.Next != "" || len(got.Rows) != 3 {
		t.Fatalf("a page that fits must not be truncated: %+v", got)
	}
	if _, ok, err := cursor(""); ok || err != nil {
		t.Fatal("an empty cursor is no cursor")
	}
	if _, _, err := cursor("last week"); err == nil {
		t.Fatal("a cursor this surface did not issue must be refused")
	}
}

// TestRegister builds the server with every tool: the SDK infers each tool's input and output
// schema from its Go types at registration and panics on a type it cannot describe, so this is
// where such a mistake would show, not on the Pi.
func TestRegister(t *testing.T) {
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	(&Surface{now: time.Now}).register(s)
}
