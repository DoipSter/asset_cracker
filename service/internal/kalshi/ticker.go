package kalshi

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A 15-minute market's ticker carries its closing time, e.g. KXBTC15M-26SEP202200-00:
// series, then yy MON dd HH MM, then the minute again.
var tickerRE = regexp.MustCompile(`^(.+)-(\d\d)([A-Z]{3})(\d\d)(\d\d)(\d\d)-(\d\d)$`)

// English month names, fixed: the ticker does not follow anyone's locale.
var months = []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}

// NextTicker returns the ticker of the round that follows, `round` later. After a round closes
// Kalshi's market list takes about 30 seconds to show the next one, but the next ticker can be
// worked out and asked for directly (measured by the Python app: ~5 seconds instead of ~34).
//
// The arithmetic is on the wall-clock digits in the ticker, with no time zone, so across a
// daylight-saving change it can name a round that does not exist. The caller falls back to the
// market list when the predicted ticker is not found.
func NextTicker(ticker string, round time.Duration) (string, bool) {
	m := tickerRE.FindStringSubmatch(ticker)
	if m == nil {
		return "", false
	}
	month := -1
	for i, name := range months {
		if name == m[3] {
			month = i + 1
		}
	}
	if month < 0 {
		return "", false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	at := time.Date(2000+n(m[2]), time.Month(month), n(m[4]), n(m[5]), n(m[6]), 0, 0, time.UTC)
	if at.Day() != n(m[4]) || at.Hour() != n(m[5]) || at.Minute() != n(m[6]) {
		return "", false // digits that are not a real date or time
	}
	at = at.Add(round)
	return fmt.Sprintf("%s-%02d%s%02d%02d%02d-%02d", m[1], at.Year()%100,
		strings.ToUpper(months[at.Month()-1]), at.Day(), at.Hour(), at.Minute(), at.Minute()), true
}
