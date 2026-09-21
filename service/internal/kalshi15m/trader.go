package kalshi15m

import (
	"math"
	"sort"
	"strconv"

	"github.com/doipster/asset_cracker/service/internal/pyfloat"
)

// CSVFields is the trade log's header, as the Python writes it.
var CSVFields = []string{"time", "strategy", "event", "ticker", "side", "contracts", "price", "multiplier",
	"fee", "cost", "payout", "pnl", "result", "strike", "btc_price", "final_value", "balance_after",
	"model_prob", "edge"}

type secPrice struct {
	sec   int64
	price float64
}
type offsetObs struct{ at, offset float64 }

// Trader holds every strategy's account for one coin, plus the shared price, volatility and
// index-gap tracking. The clock is passed in: the Python reads time.time() from inside.
type Trader struct {
	Now func() float64 // unix seconds

	BaseOffset, SDPct, DefaultSigma float64
	Accounts                        []*Account // in Strategies order
	Paused                          bool
	RoundsMonitored                 int
	Sigma2                          float64
	Market                          *Market
	Trades                          [][]string // the trade log, header first

	offsets     []offsetObs // at most 24: about six hours of rounds
	roundTicker string
	ring        []secPrice // at most 150: (second, price), for the settlement average
	volRef      *secPrice
	minCloses   []float64 // at most 60: the price at the end of each finished minute
	minLast     *secPrice // (minute number, latest price in it)
}

// NewTrader starts every account fresh.
func NewTrader(now func() float64, offsetPct, sdPct, defaultSigma float64) *Trader {
	t := &Trader{Now: now, BaseOffset: offsetPct, SDPct: sdPct, DefaultSigma: defaultSigma,
		Sigma2: defaultSigma * defaultSigma}
	for _, p := range Strategies {
		t.Accounts = append(t.Accounts, NewAccount(p))
	}
	return t
}

func pushBack[T any](s []T, v T, maxLen int) []T {
	s = append(s, v)
	if len(s) > maxLen {
		s = s[1:]
	}
	return s
}

// ---- how far the index sits above our exchange price -----------------------------------

func (t *Trader) recentOffsets() []float64 {
	cutoff := t.Now() - 6*3600 // older than this and the market has moved on
	var out []float64
	for _, o := range t.offsets {
		if o.at >= cutoff {
			out = append(out, o.offset)
		}
	}
	return out
}

// OffsetPct is the median of the recent measured gaps, or the starting constant until three
// have come in.
func (t *Trader) OffsetPct() float64 {
	if recent := t.recentOffsets(); len(recent) >= 3 {
		return pyfloat.Median(recent)
	}
	return t.BaseOffset
}

func (t *Trader) addOffset(at, offset float64) bool {
	if math.Abs(offset) <= 0.002 { // anything wilder than 0.2% is bad data, not a real gap
		t.offsets = pushBack(t.offsets, offsetObs{at, pyfloat.Round(offset, 8)}, 24)
		return true
	}
	return false
}

// NoteSettlement uses a settled round as a measurement: Kalshi's settled value IS the index
// averaged over the final minute, so comparing it with our own average measures the gap.
func (t *Trader) NoteSettlement(closeAt float64, finalValue any) {
	indexAvg, ok := ParseAmount(finalValue)
	if !ok {
		return
	}
	var ours []float64
	for _, p := range t.ring {
		if s := float64(p.sec); closeAt-60 <= s && s < closeAt {
			ours = append(ours, p.price)
		}
	}
	if len(ours) < 40 || indexAvg <= 0 { // too few ticks in that minute to average fairly
		return
	}
	t.addOffset(closeAt, indexAvg/(pyfloat.Sum(ours)/float64(len(ours)))-1)
}

// SeedOffsets primes the estimate from rounds that settled before we started.
func (t *Trader) SeedOffsets(measurements [][2]float64) {
	sorted := append([][2]float64(nil), measurements...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i][0] != sorted[j][0] {
			return sorted[i][0] < sorted[j][0]
		}
		return sorted[i][1] < sorted[j][1]
	})
	for _, m := range sorted {
		t.addOffset(m[0], m[1])
	}
}

// ---- volatility ---------------------------------------------------------------------------

func clampSigma2(s float64) float64 { return math.Min(math.Max(s, 2e-5*2e-5), 3e-4*3e-4) }

func squaredLogReturns(closes []float64) []float64 {
	var out []float64
	for i := 0; i+1 < len(closes); i++ {
		if a, b := closes[i], closes[i+1]; a > 0 && b > 0 {
			r := math.Log(b / a)
			out = append(out, float64(r*r))
		}
	}
	return out
}

// SeedVol starts from real recent volatility using 1-minute closes, oldest first.
func (t *Trader) SeedVol(closes []float64) {
	if sq := squaredLogReturns(closes); len(sq) >= 5 {
		t.Sigma2 = clampSigma2(pyfloat.Sum(sq) / float64(len(sq)) / 60)
	}
	// Older minutes go in front of any collected live. As Python's deque(maxlen=60).extendleft:
	// one at a time, and when full the NEWEST end is what falls off.
	for i := len(closes) - 1; i >= 0; i-- {
		t.minCloses = append([]float64{closes[i]}, t.minCloses...)
		if len(t.minCloses) > 60 {
			t.minCloses = t.minCloses[:60]
		}
	}
}

func variance(closes []float64) (float64, bool) {
	sq := squaredLogReturns(closes)
	if len(sq) == 0 {
		return 0, false
	}
	return pyfloat.Sum(sq) / float64(len(sq)) / 60, true
}

func tail(s []float64, n int) []float64 {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func (t *Trader) tailContext(m Market, price, tau float64) TailContext {
	c45, c6 := tail(t.minCloses, 46), tail(t.minCloses, 7)
	if len(c45) < 15 || len(c6) < 4 {
		return TailContext{}
	}
	s45, ok45 := variance(c45)
	s5, ok5 := variance(c6)
	if !ok45 || s45 == 0 || !ok5 {
		return TailContext{}
	}
	s45 = clampSigma2(s45)
	s2 := math.Max(s45, s5) * (LotteryFatten * LotteryFatten) // widen the tails a little
	pTail := ProbYes(price, m.Strike, tau, s2, t.knownAvg(m.Close, tau), t.OffsetPct(), t.SDPct)
	spike := math.Sqrt(s5 / s45)
	return TailContext{PTail: &pTail, Spike: &spike}
}

// Observe takes every live price: keeps a per-second record and updates volatility.
func (t *Trader) Observe(price, ts float64) {
	sec := int64(ts)
	minute := sec / 60
	if t.minLast != nil && minute != t.minLast.sec {
		t.minCloses = pushBack(t.minCloses, t.minLast.price, 60)
	}
	t.minLast = &secPrice{minute, price}
	if n := len(t.ring); n > 0 && t.ring[n-1].sec == sec {
		t.ring[n-1].price = price
	} else {
		t.ring = pushBack(t.ring, secPrice{sec, price}, 150)
	}
	switch {
	case t.volRef == nil:
		t.volRef = &secPrice{sec, price}
	case sec-t.volRef.sec >= 5: // sample every 5 s to dodge bid/ask bounce
		dt := float64(sec - t.volRef.sec)
		lr := math.Log(price / t.volRef.price)
		inst := lr * lr / dt
		weight := 1 - math.Pow(0.5, dt/300) // ~5 minute half-life
		t.Sigma2 = clampSigma2(t.Sigma2 + float64(weight*(inst-t.Sigma2)))
		t.volRef = &secPrice{sec, price}
	}
}

func (t *Trader) knownAvg(closeAt, tau float64) *float64 {
	if tau >= 60 {
		return nil
	}
	var vals []float64
	for _, p := range t.ring {
		if float64(p.sec) >= closeAt-60 {
			vals = append(vals, p.price)
		}
	}
	if len(vals) == 0 {
		return nil
	}
	avg := pyfloat.Sum(vals) / float64(len(vals))
	return &avg
}

// ---- trading ------------------------------------------------------------------------------

// Step gives every strategy a look at the open round.
func (t *Trader) Step(m Market, price, now float64) []Event {
	t.Market = &m
	if price == 0 {
		return nil
	}
	if m.Ticker != t.roundTicker { // a new round started: count it once
		t.roundTicker = m.Ticker
		t.RoundsMonitored++
	}
	tau := m.Close - now
	pModel := ProbYes(price, m.Strike, tau, t.Sigma2, t.knownAvg(m.Close, tau), t.OffsetPct(), t.SDPct)
	ctx := t.tailContext(m, price, tau)
	var events []Event
	for _, a := range t.Accounts {
		events = append(events, a.Step(m, price, now, pModel, t.Paused, ctx)...)
	}
	t.record(events, now)
	return events
}

// OnSettled pays out or writes off when Kalshi reports a round's real result.
func (t *Trader) OnSettled(ticker, result string, finalValue any, now float64, price *float64) []Event {
	if result != "yes" && result != "no" {
		return nil
	}
	var events []Event
	for _, a := range t.Accounts {
		events = append(events, a.OnSettled(ticker, result, finalValue, now, price)...)
	}
	t.record(events, now)
	return events
}

func (t *Trader) account(name string) *Account {
	for _, a := range t.Accounts {
		if a.Params.Name == name {
			return a
		}
	}
	return nil
}

func pyField(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case float64:
		return pyfloat.Repr(x)
	case *float64:
		if x == nil {
			return ""
		}
		return pyfloat.Repr(*x)
	case int:
		return strconv.Itoa(x)
	case string:
		return x
	}
	return ""
}

func (t *Trader) record(events []Event, now float64) {
	for _, e := range events {
		if len(t.Trades) == 0 {
			t.Trades = append(t.Trades, CSVFields)
		}
		lot := e.Lot
		price, btc := any(lot.Price), any(lot.BTCPrice)
		var payout, pnl, result, final any = "", "", "", ""
		if e.Kind == "sold" {
			price = lot.ExitPrice
		}
		if e.Kind != "bet" {
			payout, pnl, btc = lot.Payout, lot.PnL, lot.ExitBTC
		}
		if e.Kind == "settled" {
			result, final = lot.Result, lot.FinalValue
		}
		row := []any{ISOSeconds(now), e.Strategy, map[string]string{"bet": "BET", "sold": "SOLD", "settled": "SETTLED"}[e.Kind],
			lot.Ticker, lot.Side, lot.Contracts, price, lot.Multiplier, lot.Fee, lot.Cost, payout, pnl, result,
			lot.Strike, btc, final, round2(t.account(e.Strategy).Equity(t.Market)), lot.ModelProb, lot.Edge}
		fields := make([]string, len(row))
		for i, v := range row {
			fields[i] = pyField(v)
		}
		t.Trades = append(t.Trades, fields)
	}
}

// State is the saved state in the Python's shape, for the parts that depend only on the
// inputs. Used to compare a replay with what the Python saved.
func (t *Trader) State() map[string]any {
	type standing struct {
		a  *Account
		eq float64
	}
	table := make([]standing, len(t.Accounts))
	for i, a := range t.Accounts {
		table[i] = standing{a, a.Equity(t.Market)}
	}
	sort.SliceStable(table, func(i, j int) bool { return table[i].eq > table[j].eq })
	board := make([]any, len(table))
	for i, s := range table {
		board[i] = map[string]any{"strategy": s.a.Params.Name, "balance": round2(s.eq),
			"return_pct": round2((s.eq/StartBalance - 1) * 100), "bets": s.a.Bets, "rounds_joined": s.a.Participated()}
	}
	accounts := map[string]any{}
	for _, a := range t.Accounts {
		eq := a.Equity(t.Market)
		log := a.Log
		if len(log) > LogKept {
			log = log[len(log)-LogKept:]
		}
		accounts[a.Params.Name] = map[string]any{"strategy": a.Params.Blurb, "balance": round2(eq),
			"profit": round2(eq - StartBalance), "return_pct": pyfloat.Round((eq/StartBalance-1)*100, 3),
			"cash": round2(a.Cash), "realized_pnl": round2(a.RealizedPnL), "bets": a.Bets, "wins": a.Wins,
			"losses": a.Losses, "log": log}
	}
	offsets := make([]any, len(t.offsets))
	for i, o := range t.offsets {
		offsets[i] = []any{pyfloat.Round(o.at, 3), o.offset}
	}
	var last any
	if t.roundTicker != "" {
		last = t.roundTicker
	}
	return map[string]any{"rounds_monitored": t.RoundsMonitored, "last_round_ticker": last,
		"index_offsets": offsets, "leaderboard": board, "accounts": accounts}
}
