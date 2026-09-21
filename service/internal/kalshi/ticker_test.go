package kalshi

import (
	"testing"
	"time"
)

func TestNextTicker(t *testing.T) {
	cases := []struct{ in, want string }{
		{"KXBTC15M-26SEP200145-45", "KXBTC15M-26SEP200200-00"}, // the example in the Python app
		{"KXBTC15M-26SEP202145-45", "KXBTC15M-26SEP202200-00"}, // seen live 2026-09-20
		{"KXETH15M-26SEP302345-45", "KXETH15M-26OCT010000-00"}, // across a month end
		{"KXBTC15M-26DEC312345-45", "KXBTC15M-27JAN010000-00"}, // across a year end
		{"KXBTC15M-28FEB282345-45", "KXBTC15M-28FEB290000-00"}, // leap day
	}
	for _, c := range cases {
		got, ok := NextTicker(c.in, 15*time.Minute)
		if !ok || got != c.want {
			t.Errorf("NextTicker(%q) = %q, %v; want %q", c.in, got, ok, c.want)
		}
	}
	for _, bad := range []string{"", "KXBTC15M", "KXBTC15M-26XXX200145-45", "KXBTC15M-26SEP319999-45"} {
		if got, ok := NextTicker(bad, 15*time.Minute); ok {
			t.Errorf("NextTicker(%q) = %q, want not ok", bad, got)
		}
	}
}
