package kalshi15m2

import (
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/doipster/asset_cracker/service/internal/pyfloat"
)

// CSVFields and ExitFields are the headers of the trade log and the early-sales log.
var CSVFields = []string{"session", "time", "strategy", "coin", "event", "ticker", "side", "contracts", "price",
	"multiplier", "fee", "cost", "payout", "pnl", "result", "strike", "btc_price", "final_value", "balance_after",
	"model_prob", "edge", "why", "tau", "yes_bid", "yes_ask", "mkt_prob"}
var ExitFields = []string{"session", "time", "strategy", "coin", "ticker", "side", "contracts", "why", "entry", "exit",
	"cost", "sold_for", "booked", "held_would_pay", "gave_up", "tau_at_exit", "result"}

type secPrice struct {
	sec   int64
	price float64
}
type offsetObs struct{ at, offset float64 }

// Calibration is a coin's starting figures: how far Kalshi's index runs above the exchange
// price, how uncertain that gap is, and the coin's typical volatility per sqrt(second).
type Calibration struct {
	OffsetPct    float64 `json:"index_offset_pct"`
	SDPct        float64 `json:"index_sd_pct"`
	DefaultSigma float64 `json:"default_sigma"`
	Decimals     int     `json:"decimals"`
}

// CoinState is everything specific to one coin. No money lives here.
type CoinState struct {
	Coin            string
	Cal             Calibration
	Sigma2          float64
	Market          *Market
	Price           float64
	RoundsMonitored int

	now         func() float64
	offsets     []offsetObs // at most 24: about six hours of rounds
	roundTicker string
	ring        []secPrice // at most 150
	volRef      *secPrice
	minCloses   []float64 // at most 60
	minLast     *secPrice
}

func pushBack[T any](s []T, v T, maxLen int) []T {
	s = append(s, v)
	if len(s) > maxLen {
		s = s[1:]
	}
	return s
}

func (c *CoinState) recentOffsets() []float64 {
	cutoff := c.now() - 6*3600
	var out []float64
	for _, o := range c.offsets {
		if o.at >= cutoff {
			out = append(out, o.offset)
		}
	}
	return out
}

// OffsetPct is the median of the recent measured gaps, or the starting figure until three are in.
func (c *CoinState) OffsetPct() float64 {
	if recent := c.recentOffsets(); len(recent) >= 3 {
		return pyfloat.Median(recent)
	}
	return c.Cal.OffsetPct
}

// OffsetStatus is the gap in use and how many recent measurements it rests on.
func (c *CoinState) OffsetStatus() (float64, int) { return c.OffsetPct(), len(c.recentOffsets()) }

// HasOffsetAt reports whether a round's measurement is already held.
func (c *CoinState) HasOffsetAt(at float64) bool {
	for _, o := range c.offsets {
		if o.at == at {
			return true
		}
	}
	return false
}

func (c *CoinState) addOffset(at, offset float64) bool {
	if math.Abs(offset) <= 0.002 {
		c.offsets = pushBack(c.offsets, offsetObs{at, pyfloat.Round(offset, 8)}, 24)
		return true
	}
	return false
}

func (c *CoinState) noteSettlement(closeAt float64, finalValue any) {
	indexAvg, ok := ParseAmount(finalValue)
	if !ok {
		return
	}
	var ours []float64
	for _, p := range c.ring {
		if s := float64(p.sec); closeAt-60 <= s && s < closeAt {
			ours = append(ours, p.price)
		}
	}
	if len(ours) < 40 || indexAvg <= 0 {
		return
	}
	c.addOffset(closeAt, indexAvg/(pyfloat.Sum(ours)/float64(len(ours)))-1)
}

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

func (c *CoinState) seedVol(closes []float64) {
	if sq := squaredLogReturns(closes); len(sq) >= 5 {
		c.Sigma2 = clampSigma2(pyfloat.Sum(sq) / float64(len(sq)) / 60)
	}
	for i := len(closes) - 1; i >= 0; i-- { // as deque(maxlen=60).extendleft: the newest end falls off
		c.minCloses = append([]float64{closes[i]}, c.minCloses...)
		if len(c.minCloses) > 60 {
			c.minCloses = c.minCloses[:60]
		}
	}
}

func (c *CoinState) observe(price, ts float64) {
	c.Price = price
	sec := int64(ts)
	minute := sec / 60
	if c.minLast != nil && minute != c.minLast.sec {
		c.minCloses = pushBack(c.minCloses, c.minLast.price, 60)
	}
	c.minLast = &secPrice{minute, price}
	if n := len(c.ring); n > 0 && c.ring[n-1].sec == sec {
		c.ring[n-1].price = price
	} else {
		c.ring = pushBack(c.ring, secPrice{sec, price}, 150)
	}
	switch {
	case c.volRef == nil:
		c.volRef = &secPrice{sec, price}
	case sec-c.volRef.sec >= 5:
		dt := float64(sec - c.volRef.sec)
		lr := math.Log(price / c.volRef.price)
		inst := lr * lr / dt
		weight := 1 - math.Pow(0.5, dt/300)
		c.Sigma2 = clampSigma2(c.Sigma2 + float64(weight*(inst-c.Sigma2)))
		c.volRef = &secPrice{sec, price}
	}
}

func (c *CoinState) knownAvg(closeAt, tau float64) *float64 {
	if tau >= 60 {
		return nil
	}
	var vals []float64
	for _, p := range c.ring {
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

func tail(s []float64, n int) []float64 {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func variance(closes []float64) (float64, bool) {
	sq := squaredLogReturns(closes)
	if len(sq) == 0 {
		return 0, false
	}
	return pyfloat.Sum(sq) / float64(len(sq)) / 60, true
}

func (c *CoinState) tailContext(m *Market, price, tau float64) TailContext {
	c45, c6 := tail(c.minCloses, 46), tail(c.minCloses, 7)
	if len(c45) < 15 || len(c6) < 4 {
		return TailContext{}
	}
	s45, ok45 := variance(c45)
	s5, ok5 := variance(c6)
	if !ok45 || s45 == 0 || !ok5 {
		return TailContext{}
	}
	s45 = clampSigma2(s45)
	s2 := math.Max(s45, s5) * (LotteryFatten * LotteryFatten)
	pTail := ProbYes(price, m.Strike, tau, s2, c.knownAvg(m.Close, tau), c.OffsetPct(), c.Cal.SDPct)
	spike := math.Sqrt(s5 / s45)
	return TailContext{PTail: &pTail, Spike: &spike}
}

// Trader holds the twelve accounts, one balance each shared across every coin, plus a CoinState
// per coin. The clock is passed in: the Python reads time.time() from inside.
type Trader struct {
	Now             func() float64
	Coins           map[string]*CoinState
	CoinOrder       []string
	Accounts        []*Account // six originals then their six twins
	Paused          bool
	RoundsMonitored int // a round is one 15-minute window across every coin, counted once
	Session         string
	Trades, Exits   [][]string // the two logs, header first

	roundClose *float64
}

// NewTrader starts every account fresh. coins is in the order the app lists them.
func NewTrader(now func() float64, order []string, cal map[string]Calibration) *Trader {
	t := &Trader{Now: now, Coins: map[string]*CoinState{}, CoinOrder: order,
		Session: time.UnixMicro(int64(math.RoundToEven(now() * 1e6))).Local().Format("20060102_150405")}
	for _, name := range order {
		k := cal[name]
		t.Coins[name] = &CoinState{Coin: name, Cal: k, Sigma2: k.DefaultSigma * k.DefaultSigma, now: now}
	}
	for _, p := range AllStrategies() {
		t.Accounts = append(t.Accounts, NewAccount(p))
	}
	return t
}

// Account finds an account by name.
func (t *Trader) Account(name string) *Account {
	for _, a := range t.Accounts {
		if a.Params.Name == name {
			return a
		}
	}
	return nil
}

// Markets is every coin's live quotes, for valuing open bets.
func (t *Trader) Markets() map[string]*Market {
	out := map[string]*Market{}
	for name, c := range t.Coins {
		if c.Market != nil {
			out[name] = c.Market
		}
	}
	return out
}

func (t *Trader) SeedVol(coin string, closes []float64)  { t.Coins[coin].seedVol(closes) }
func (t *Trader) Observe(coin string, price, ts float64) { t.Coins[coin].observe(price, ts) }
func (t *Trader) NoteSettlement(coin string, closeAt float64, finalValue any) {
	t.Coins[coin].noteSettlement(closeAt, finalValue)
}

// SeedOffsets primes a coin's index-gap estimate from rounds that settled before the start.
func (t *Trader) SeedOffsets(coin string, measurements [][2]float64) {
	sorted := append([][2]float64(nil), measurements...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i][0] != sorted[j][0] {
			return sorted[i][0] < sorted[j][0]
		}
		return sorted[i][1] < sorted[j][1]
	})
	for _, m := range sorted {
		t.Coins[coin].addOffset(m[0], m[1])
	}
}

// Step gives every original a look at this coin's open round, and its twin the other side of
// whatever it did. The accounts are shared, so a bet here spends the same balance as a bet on
// any other coin.
func (t *Trader) Step(coin string, market Market, price, now float64) []Event {
	c := t.Coins[coin]
	market.Coin = coin
	c.Market = &market
	if price == 0 {
		return nil
	}
	if market.Ticker != c.roundTicker {
		c.roundTicker = market.Ticker
		c.RoundsMonitored++
	}
	if t.roundClose == nil || market.Close != *t.roundClose { // a new window, for all coins
		cl := market.Close
		t.roundClose = &cl
		t.RoundsMonitored++
	}
	tau := market.Close - now
	pModel := ProbYes(price, market.Strike, tau, c.Sigma2, c.knownAvg(market.Close, tau), c.OffsetPct(), c.Cal.SDPct)
	ctx := c.tailContext(&market, price, tau)
	var events []Event
	for _, a := range t.Accounts {
		if a.Params.Anti {
			continue // twins shadow; they are driven by their original, just below
		}
		moves := a.Step(c.Market, price, now, pModel, t.Paused, ctx)
		events = append(events, moves...)
		if twin := t.Account("Anti " + a.Params.Name); twin != nil && len(moves) > 0 {
			events = append(events, twin.Mirror(moves, c.Market, now, price)...)
		}
	}
	return t.record(events, now)
}

// OnSettled pays out or writes off when Kalshi reports a round's real result, then grades the
// round's early sales against holding them.
func (t *Trader) OnSettled(ticker, result string, finalValue any, now float64, price *float64) []Event {
	if result != "yes" && result != "no" {
		return nil
	}
	var events []Event
	for _, a := range t.Accounts {
		events = append(events, a.OnSettled(ticker, result, finalValue, now, price)...)
	}
	t.gradeExits(ticker, result, now)
	return t.record(events, now)
}

// gradeExits compares every early sale in a round with holding it to the end. gave_up is the
// point: positive means selling cost us, negative means it saved us.
func (t *Trader) gradeExits(ticker, result string, now float64) {
	for _, a := range t.Accounts {
		for _, lot := range a.Log {
			if lot.Ticker != ticker || lot.Status != "sold" || lot.Graded {
				continue
			}
			lot.Graded = true
			held := 0.0
			if (result == "yes") == (lot.Side == "UP") {
				held = float64(lot.Contracts)
			}
			gave := round2(held - *lot.Payout)
			lot.GaveUp = &gave
			t.Exits = appendRow(t.Exits, ExitFields, []any{t.Session, ISOSeconds(now), a.Params.Name, lot.Coin, ticker, lot.Side,
				lot.Contracts, lot.Why, lot.Price, lot.ExitPrice, lot.Cost, lot.Payout, lot.PnL, round2(held), gave, lot.ExitTau, result})
		}
	}
}

func field(v any) string {
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
	case *int:
		if x == nil {
			return ""
		}
		return strconv.Itoa(*x)
	case string:
		return x
	}
	return ""
}

func appendRow(log [][]string, header []string, row []any) [][]string {
	if len(log) == 0 {
		log = append(log, header)
	}
	out := make([]string, len(row))
	for i, v := range row {
		out[i] = field(v)
	}
	return append(log, out)
}

func (t *Trader) record(events []Event, now float64) []Event {
	for _, e := range events {
		lot := e.Lot
		price, btc := any(lot.Price), any(lot.BTCPrice)
		var payout, pnl, result, final any = "", "", "", ""
		why := lot.Why
		switch e.Kind {
		case "bet":
			why = "entry"
		case "sold":
			price = lot.ExitPrice
		}
		if e.Kind != "bet" {
			payout, pnl, btc = lot.Payout, lot.PnL, lot.ExitBTC
		}
		if e.Kind == "settled" {
			result, final = lot.Result, lot.FinalValue
		}
		// the book as it stood when this happened, so a row can be read on its own later
		var yb, ya, mkt any = "", "", ""
		if c := t.Coins[lot.Coin]; c != nil && c.Market != nil {
			yb, ya, mkt = c.Market.YesBid, c.Market.YesAsk, pyfloat.Round((c.Market.YesBid+c.Market.YesAsk)/2, 3)
		}
		t.Trades = appendRow(t.Trades, CSVFields, []any{t.Session, ISOSeconds(now), e.Strategy, lot.Coin,
			map[string]string{"bet": "BET", "sold": "SOLD", "settled": "SETTLED"}[e.Kind], lot.Ticker, lot.Side, lot.Contracts,
			price, lot.Multiplier, lot.Fee, lot.Cost, payout, pnl, result, lot.Strike, btc, final,
			round2(t.Account(e.Strategy).Equity(t.Markets())), lot.ModelProb, lot.Edge, why, pyRoundInt(lot.Close - now), yb, ya, mkt})
	}
	// Every path that moves money ends here, so this is the one place to notice that a strategy
	// has nothing left. A strategy is staked again; a twin is not: it is a measurement of its
	// original, and handing it a fresh stake every time would say nothing except that it failed.
	for _, a := range t.Accounts {
		if !a.Broke() {
			continue
		}
		e := Event{Kind: "bankrupt", Strategy: a.Params.Name, DiedWith: a.Cash, Rounds: a.Participated()}
		if a.Params.Anti {
			a.Retired = true
		} else {
			a.Revive()
		}
		e.Life, e.Retired = a.Bankruptcies, a.Retired
		events = append(events, e)
	}
	return events
}

// State is the saved state in the Python's shape, for the parts that depend only on the inputs.
func (t *Trader) State() map[string]any {
	mk := t.Markets()
	type standing struct {
		a  *Account
		eq float64
	}
	var table []standing
	accounts := map[string]any{}
	for _, a := range t.Accounts {
		eq := a.Equity(mk)
		if !a.Params.Anti {
			table = append(table, standing{a, eq})
		}
		log := a.Log
		if len(log) > LogKept {
			log = log[len(log)-LogKept:]
		}
		if log == nil {
			log = []*Lot{}
		}
		accounts[a.Params.Name] = map[string]any{"strategy": a.Params.Blurb, "balance": round2(eq), "profit": round2(eq - StartBalance),
			"return_pct": pyfloat.Round((eq/StartBalance-1)*100, 3), "cash": round2(a.Cash), "realized_pnl": round2(a.RealizedPnL),
			"bets": a.Bets, "wins": a.Wins, "losses": a.Losses, "bankruptcies": a.Bankruptcies, "retired": a.Retired, "log": log}
	}
	sort.SliceStable(table, func(i, j int) bool { return table[i].eq > table[j].eq })
	board := make([]any, len(table))
	for i, s := range table {
		board[i] = map[string]any{"strategy": s.a.Params.Name, "balance": round2(s.eq),
			"return_pct": round2((s.eq/StartBalance - 1) * 100), "bets": s.a.Bets, "rounds_joined": s.a.Participated()}
	}
	coins := map[string]any{}
	for name, c := range t.Coins {
		offsets := make([]any, len(c.offsets))
		for i, o := range c.offsets {
			offsets[i] = []any{pyfloat.Round(o.at, 3), o.offset}
		}
		open := 0
		for _, a := range t.Accounts {
			for _, lot := range a.openLots("") {
				if lot.Coin == name {
					open++
				}
			}
		}
		var last any
		if c.roundTicker != "" {
			last = c.roundTicker
		}
		coins[name] = map[string]any{"rounds_monitored": c.RoundsMonitored, "last_round_ticker": last, "index_offsets": offsets, "open_bets": open}
	}
	var lastClose any
	if t.roundClose != nil {
		lastClose = *t.roundClose
	}
	return map[string]any{"rounds_monitored": t.RoundsMonitored, "last_round_close": lastClose, "leaderboard": board,
		"accounts": accounts, "coins": coins}
}

// Withdraw takes money out of an account from outside the engine: the platform's skim. The
// Python has no such thing. It is not used in a parity replay, and it means a live account's
// balance stops being comparable with the same strategy run in the Python app, by exactly what
// was skimmed.
func (t *Trader) Withdraw(name string, dollars float64) {
	if a := t.Account(name); a != nil {
		a.Cash -= dollars
	}
}

// ---- saving and restoring -------------------------------------------------------------------

// SavedAccount and SavedState are what survives a restart. Prices seen and volatility are not
// saved: they are re-seeded from the exchange at start, as the Python does.
type SavedAccount struct {
	Cash         float64 `json:"cash"`
	Log          []*Lot  `json:"log"`
	Bets         int     `json:"bets"`
	Wins         int     `json:"wins"`
	Losses       int     `json:"losses"`
	RealizedPnL  float64 `json:"realized_pnl"`
	NextID       int     `json:"next_id"`
	Bankruptcies int     `json:"bankruptcies"`
	Retired      bool    `json:"retired"`
}
type SavedCoin struct {
	Offsets         [][2]float64 `json:"index_offsets"`
	RoundsMonitored int          `json:"rounds_monitored"`
	RoundTicker     string       `json:"last_round_ticker"`
}
type SavedState struct {
	Accounts        map[string]SavedAccount `json:"accounts"`
	Coins           map[string]SavedCoin    `json:"coins"`
	RoundsMonitored int                     `json:"rounds_monitored"`
	RoundClose      *float64                `json:"last_round_close"`
	Paused          bool                    `json:"paused"`
}

// Export captures the state to save.
func (t *Trader) Export() SavedState {
	s := SavedState{Accounts: map[string]SavedAccount{}, Coins: map[string]SavedCoin{}, RoundsMonitored: t.RoundsMonitored,
		RoundClose: t.roundClose, Paused: t.Paused}
	for _, a := range t.Accounts {
		s.Accounts[a.Params.Name] = SavedAccount{a.Cash, a.Log, a.Bets, a.Wins, a.Losses, a.RealizedPnL, a.NextID, a.Bankruptcies, a.Retired}
	}
	for name, c := range t.Coins {
		sc := SavedCoin{RoundsMonitored: c.RoundsMonitored, RoundTicker: c.roundTicker}
		for _, o := range c.offsets {
			sc.Offsets = append(sc.Offsets, [2]float64{o.at, o.offset})
		}
		s.Coins[name] = sc
	}
	return s
}

// Import restores a saved state into a fresh trader.
func (t *Trader) Import(s SavedState) {
	for _, a := range t.Accounts {
		if sa, ok := s.Accounts[a.Params.Name]; ok {
			a.Cash, a.Log, a.Bets, a.Wins, a.Losses, a.RealizedPnL = sa.Cash, sa.Log, sa.Bets, sa.Wins, sa.Losses, sa.RealizedPnL
			a.NextID, a.Bankruptcies, a.Retired = max(sa.NextID, 1), sa.Bankruptcies, sa.Retired
		}
	}
	for name, sc := range s.Coins {
		if c := t.Coins[name]; c != nil {
			c.RoundsMonitored, c.roundTicker, c.offsets = sc.RoundsMonitored, sc.RoundTicker, nil
			for _, o := range sc.Offsets {
				c.offsets = append(c.offsets, offsetObs{o[0], o[1]})
			}
		}
	}
	t.RoundsMonitored, t.roundClose, t.Paused = s.RoundsMonitored, s.RoundClose, s.Paused
}
