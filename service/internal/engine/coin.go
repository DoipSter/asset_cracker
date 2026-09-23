package engine

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/doipster/asset_cracker/service/internal/pyfloat"
)

// This file is a FORK of the archived v2 per-coin state (legacy kalshi15m2 CoinState). That
// package's methods are unexported and the package is frozen, so the code is copied, not called.
// ProbYes is copied too (prob_model.go). The arithmetic is kept byte for byte, including every
// float64() wrapper around a product that feeds an addition. TestForkMatchesV2 holds the fork
// to the archive on the recorded fixtures. A nil ref does not close the drift gate.
//
// Left out on purpose: the minute closes and the tail context (only v2's Lottery reads them),
// the round counters, and the Market pointer. None of them feeds sigma2, the offset or p_model.
//
// One difference, stated: v2's CoinState reads a wall clock of its own to decide which offsets
// are younger than six hours. Here the clock is always passed in. The two can disagree only in
// the instant an offset turns exactly six hours old, and the drift gate sees that second.

type secPrice struct {
	sec   int64
	price float64
}
type offsetObs struct{ at, offset float64 }

// Calibration is a coin's starting figures: how far Kalshi's index runs above the exchange
// price, how uncertain that gap is, and the coin's typical volatility per sqrt(second). The same
// shape and JSON keys as the second engine's, so one config file feeds both.
type Calibration struct {
	OffsetPct    float64 `json:"index_offset_pct"`
	SDPct        float64 `json:"index_sd_pct"`
	DefaultSigma float64 `json:"default_sigma"`
	Decimals     int     `json:"decimals"`
}

// CoinState is everything specific to one coin. No money lives here, and Engine never touches it.
type CoinState struct {
	Coin   string
	Cal    Calibration
	Sigma2 float64
	Price  float64

	offsets []offsetObs // at most 24: about six hours of rounds
	ring    []secPrice  // at most 150
	volRef  *secPrice
}

func pushBack[T any](s []T, v T, maxLen int) []T {
	s = append(s, v)
	if len(s) > maxLen {
		s = s[1:]
	}
	return s
}

func (c *CoinState) recentOffsets(now float64) []float64 {
	cutoff := now - 6*3600
	var out []float64
	for _, o := range c.offsets {
		if o.at >= cutoff {
			out = append(out, o.offset)
		}
	}
	return out
}

// offsetStatus is the gap in use and how many recent measurements it rests on: the median of the
// recent measured gaps, or the starting figure until three are in.
func (c *CoinState) offsetStatus(now float64) (float64, int) {
	recent := c.recentOffsets(now)
	if len(recent) >= 3 {
		return pyfloat.Median(recent), len(recent)
	}
	return c.Cal.OffsetPct, len(recent)
}

func (c *CoinState) hasOffsetAt(at float64) bool {
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
}

func (c *CoinState) observe(price, ts float64) {
	c.Price = price
	sec := int64(ts)
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

// ---- Model: the fork behind its own small lock ------------------------------------------------

// V2Inputs is what the second engine journals about one coin each second (Runner2.Inputs): the
// reference the drift gate compares this package's model with.
type V2Inputs struct {
	Sigma2       float64
	IndexOffset  float64
	OffsetSource string // "measured" or "constant"
}

// V2InputsFrom reads the map Runner2.Inputs returns. nil if it is missing or not in that shape,
// which the drift gate treats as "v2 is absent": no entries.
func V2InputsFrom(m map[string]any) *V2Inputs {
	sigma2, ok1 := m["sigma2"].(float64)
	offset, ok2 := m["index_offset"].(float64)
	source, ok3 := m["offset_source"].(string)
	if !ok1 || !ok2 || !ok3 {
		return nil
	}
	return &V2Inputs{Sigma2: sigma2, IndexOffset: offset, OffsetSource: source}
}

// View is what one second's decision needs from the model, computed once and journaled every
// second. Engine.Decide reads a View and never a CoinState.
type View struct {
	Coin string
	// OK is false when there is nothing to decide on: an unknown coin, no price yet, or a market
	// with no strike. Decide then sends nothing.
	OK            bool
	Price, Tau    float64
	Sigma2        float64
	Offset        float64
	OffsetSamples int
	OffsetSource  string  // "measured" once three recent settlements are in, else "constant"
	PModel        float64 // the RAW model probability of Yes; what decision.model_prob stores
	// VolRatio is the model's volatility over the coin's calibrated default, sqrt(Sigma2) /
	// DefaultSigma: 1 is ordinary, 2 is twice as fast a market. Params.MinVolRatio reads it.
	VolRatio float64

	// Drift is the gate of plan 4.3: true when a live reference is supplied and the fork cannot
	// be shown, this second, to be that model. While it is true no ENTRY is sent; exits go on.
	// A nil reference (no live v2) leaves the gate open.
	Drift     bool
	DriftWhy  string  // "" | "offset source differs" | "p_model differs"
	DriftDiff float64 // |p_model - v2's|, when v2's inputs were there; what S5 reports
	RefPModel float64 // v2's model probability, recomputed from v2's sigma2 and offset
	HasRef    bool
}

// Journal is the view as stored with the market snapshot.
func (v View) Journal() map[string]any {
	if !v.OK {
		return map[string]any{"v": 3, "ok": false}
	}
	out := map[string]any{"v": 3, "ok": true, "sigma2": v.Sigma2, "index_offset": v.Offset, "offset_samples": v.OffsetSamples,
		"offset_source": v.OffsetSource, "p_model": v.PModel, "drift": v.Drift, "vol_ratio": v.VolRatio}
	if v.Drift {
		out["drift_why"] = v.DriftWhy
	}
	if v.HasRef {
		out["drift_diff"], out["p_model_v2"] = v.DriftDiff, v.RefPModel
	}
	return out
}

// Model is the per-coin model state of every coin. It is a separate value from Engine so that the
// price stream and the journal never wait on the engine's lock, which its runner holds across a
// database write (plan 5.1). Its mutex is held only across arithmetic: nothing under it does
// input or output, takes another lock, or calls out of this package except into pure functions.
type Model struct {
	mu       sync.Mutex
	coins    map[string]*CoinState
	driftTol float64
}

// NewModel starts every coin at its calibration. driftTol is Params.DriftTol, the same for every
// version; it has no default here, for the reason lambda has none.
func NewModel(order []string, cal map[string]Calibration, driftTol float64) (*Model, error) {
	if !(driftTol >= 0 && driftTol <= 1) {
		return nil, fmt.Errorf("engine: drift tolerance %v is outside 0..1", driftTol)
	}
	m := &Model{coins: map[string]*CoinState{}, driftTol: driftTol}
	for _, name := range order {
		k := cal[name]
		m.coins[name] = &CoinState{Coin: name, Cal: k, Sigma2: k.DefaultSigma * k.DefaultSigma}
	}
	return m, nil
}

// SeedVol primes a coin's volatility from recent minute closes, as the second engine does at start.
func (m *Model) SeedVol(coin string, closes []float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.coins[coin]; c != nil {
		c.seedVol(closes)
	}
}

// SeedOffsets primes a coin's index-gap estimate from rounds that settled before the start, or
// restores the saved ones. Each measurement is {close, offset}.
func (m *Model) SeedOffsets(coin string, measurements [][2]float64) {
	sorted := append([][2]float64(nil), measurements...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i][0] != sorted[j][0] {
			return sorted[i][0] < sorted[j][0]
		}
		return sorted[i][1] < sorted[j][1]
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.coins[coin]; c != nil {
		for _, o := range sorted {
			c.addOffset(o[0], o[1])
		}
	}
}

// Offsets is a coin's held measurements, oldest first: the part of the model a restart cannot
// get back from the exchanges, so the runner saves it.
func (m *Model) Offsets(coin string) [][2]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.coins[coin]
	if c == nil {
		return nil
	}
	out := make([][2]float64, len(c.offsets))
	for i, o := range c.offsets {
		out[i] = [2]float64{o.at, o.offset}
	}
	return out
}

// HasOffsetAt reports whether a round's measurement is already held.
func (m *Model) HasOffsetAt(coin string, at float64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.coins[coin]
	return c != nil && c.hasOffsetAt(at)
}

// Observe takes one trade print.
func (m *Model) Observe(coin string, price, ts float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.coins[coin]; c != nil {
		c.observe(price, ts)
	}
}

// NoteSettlement learns the index gap from a settled round's final value.
func (m *Model) NoteSettlement(coin string, closeAt float64, finalValue any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.coins[coin]; c != nil {
		c.noteSettlement(closeAt, finalValue)
	}
}

// View computes what one second's decision needs. ref is an optional characterisation against
// the archived v2 model (fork tests); nil in the live service.
func (m *Model) View(coin string, mk Market, price, now float64, ref *V2Inputs) View {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.coins[coin]
	if c == nil || !(price > 0) || !(mk.Strike > 0) {
		return View{Coin: coin}
	}
	tau := mk.Close - now
	known := c.knownAvg(mk.Close, tau)
	offset, samples := c.offsetStatus(now)
	v := View{Coin: coin, OK: true, Price: price, Tau: tau, Sigma2: c.Sigma2, Offset: offset, OffsetSamples: samples,
		OffsetSource: "measured"}
	if samples < 3 {
		v.OffsetSource = "constant"
	}
	v.PModel = ProbYes(price, mk.Strike, tau, c.Sigma2, known, offset, c.Cal.SDPct)
	if c.Cal.DefaultSigma > 0 {
		v.VolRatio = math.Sqrt(c.Sigma2) / c.Cal.DefaultSigma
	}

	switch {
	case ref == nil:
		// v2 is not live. The fork test is the characterisation; do not block every entry.
	default:
		// the same price, strike, time and price ring; the archive's volatility and offset
		v.HasRef = true
		v.RefPModel = ProbYes(price, mk.Strike, tau, ref.Sigma2, known, ref.IndexOffset, c.Cal.SDPct)
		v.DriftDiff = math.Abs(v.PModel - v.RefPModel)
		switch {
		case ref.OffsetSource != v.OffsetSource:
			v.Drift, v.DriftWhy = true, "offset source differs"
		case !(v.DriftDiff <= m.driftTol): // written so that a NaN is a drift
			v.Drift, v.DriftWhy = true, "p_model differs"
		}
	}
	return v
}
