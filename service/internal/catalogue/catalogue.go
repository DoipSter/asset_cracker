// Package catalogue is what the venues list that the service could record, and the rules for which
// of it the existing recorders can follow. The service fills it at start and once a day
// (app/assets.go), the store keeps it (migration 0018), and the assets page (GET /assets) searches
// it and switches recording on and off.
//
// Everything here is pure except Build, which calls the fetchers it is given. It imports the store
// for its row types only.
package catalogue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/doipster/asset_cracker/service/internal/store"
)

// Sources, as catalogue_item.source and source.code.
const (
	Kalshi   = "kalshi"
	Coinbase = "coinbase"
)

// The recorders an item can be followed by. "" is none yet.
const (
	RecorderRound   = "round"   // kalshi.Poller with a record-only sink: one market a 15-minute round
	RecorderLadder  = "ladder"  // kalshi.LadderRecorder: every open "greater" market of a series
	RecorderCandles = "candles" // Coinbase daily, hourly and minute candles; not the trade stream
)

// Settings [CONVENTION].
const (
	DefaultMaxSelected = 10             // instruments selected at once
	SearchLimit        = 50             // results a search returns
	SampleSize         = 100            // open markets read per Kalshi series to tell what kind it is
	RefreshEvery       = 24 * time.Hour // the catalogue is read at start and then this often
	RoundSeconds       = 900            // what the round poller and store.FifteenMinuteSeries read
)

// DefaultCategories are the Kalshi categories listed when AC_CATALOGUE_CATEGORIES is unset.
var DefaultCategories = []string{"Crypto"}

// Item is one row of catalogue_item.
type Item struct {
	Source, Code, Title, Category, Frequency string

	What     string // what it is, in words
	Recorder string // RecorderRound, RecorderLadder, RecorderCandles, or "" for none yet
	WhyNot   string // why not, when Recorder is ""
	Checked  bool   // false: its kind could not be read this time; the stored row keeps what it had
	Raw      json.RawMessage
}

// Series is one entry of Kalshi's series list (fields read from the public API on 2026-09-21).
type Series struct {
	Ticker    string          `json:"ticker"`
	Title     string          `json:"title"`
	Category  string          `json:"category"`
	Frequency string          `json:"frequency"`
	Tags      []string        `json:"tags"`
	Raw       json.RawMessage `json:"-"`
}

// ParseSeries reads GET /series. An entry that does not parse, has no ticker or repeats one is
// skipped and counted.
func ParseSeries(body []byte) (out []Series, skipped int, err error) {
	var page struct {
		Series *[]json.RawMessage `json:"series"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, 0, fmt.Errorf("kalshi series list: %w", err)
	}
	if page.Series == nil {
		return nil, 0, errors.New("kalshi series list: no series field")
	}
	seen := map[string]bool{}
	for _, raw := range *page.Series {
		var s Series
		if json.Unmarshal(raw, &s) != nil {
			skipped++
			continue
		}
		s.Ticker = strings.TrimSpace(s.Ticker)
		if s.Ticker == "" || seen[s.Ticker] {
			skipped++
			continue
		}
		seen[s.Ticker] = true
		s.Raw = raw
		out = append(out, s)
	}
	return out, skipped, nil
}

// Product is one Coinbase product (fields read from the public API on 2026-09-21).
type Product struct {
	ID          string          `json:"id"`
	Base        string          `json:"base_currency"`
	Quote       string          `json:"quote_currency"`
	Status      string          `json:"status"`
	DisplayName string          `json:"display_name"`
	Raw         json.RawMessage `json:"-"`
}

// ParseProducts reads GET /products and keeps the USD-quoted products that are online, by id.
func ParseProducts(body []byte) ([]Product, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(body, &raws); err != nil {
		return nil, fmt.Errorf("coinbase products: %w", err)
	}
	var out []Product
	seen := map[string]bool{}
	for _, raw := range raws {
		var p Product
		if json.Unmarshal(raw, &p) != nil || p.ID == "" || seen[p.ID] || p.Quote != "USD" || p.Status != "online" {
			continue
		}
		seen[p.ID] = true
		p.Raw = raw
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Market is what the catalogue reads of one open Kalshi market.
type Market struct {
	StrikeType string
	HasFloor   bool   // floor_strike is set
	Closes     string // close_time as sent
}

var frequencyWords = map[string]string{
	"fifteen_min": "15-minute", "hourly": "hourly", "daily": "daily", "weekly": "weekly",
	"monthly": "monthly", "annual": "annual", "one_off": "one-off", "custom": "custom",
}

func seriesWord(frequency string) string {
	if w, ok := frequencyWords[frequency]; ok {
		return w + " series"
	}
	if frequency == "" {
		return "series"
	}
	return frequency + " series"
}

// ClassifyKalshi says what a series is and which recorder, if any, can follow it, from a sample of
// its open markets. readErr is the error reading them, if any.
func ClassifyKalshi(s Series, open []Market, readErr error) Item {
	it := Item{Source: Kalshi, Code: s.Ticker, Title: s.Title, Category: s.Category, Frequency: s.Frequency, Raw: s.Raw}
	if readErr != nil {
		it.What, it.WhyNot = seriesWord(s.Frequency), "its open markets could not be read at the last refresh"
		return it
	}
	it.Checked = true
	it.Recorder, it.What, it.WhyNot = kalshiKind(s.Frequency, open)
	return it
}

func kalshiKind(frequency string, open []Market) (recorder, what, whyNot string) {
	base := seriesWord(frequency)
	if len(open) == 0 {
		return "", base, "no open markets when it was last checked"
	}
	if frequency == "fifteen_min" && oneMarketARound(open) {
		return RecorderRound, "15-minute up/down series: one market a round, with a floor strike", ""
	}
	if allGreater(open) {
		return RecorderLadder, base + ": above/below ladder, every open market strike type greater", ""
	}
	types := strings.Join(strikeTypes(open), ", ")
	if frequency == "fifteen_min" {
		return "", base, "its rounds are not one above/below market with a floor strike (strike types: " + types + ")"
	}
	return "", base, "open markets are strike type " + types + `; only 15-minute up/down series and all-"greater" ladders can be recorded yet`
}

// oneMarketARound is the shape kalshi.Poller follows: each open market has a floor strike and is
// strike type greater_or_equal (as the five recorded 15-minute series were on 2026-09-21) or
// greater, and no two close together.
func oneMarketARound(open []Market) bool {
	closes := map[string]bool{}
	for _, m := range open {
		if !m.HasFloor || (m.StrikeType != "greater_or_equal" && m.StrikeType != "greater") || closes[m.Closes] {
			return false
		}
		closes[m.Closes] = true
	}
	return true
}

// allGreater is the shape kalshi.LadderRecorder records: it leaves out any other strike type.
func allGreater(open []Market) bool {
	for _, m := range open {
		if m.StrikeType != "greater" {
			return false
		}
	}
	return true
}

func strikeTypes(open []Market) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range open {
		t := m.StrikeType
		if t == "" {
			t = "none"
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// CoinbaseItem is a Coinbase product's row: every USD product that is online can have candles.
func CoinbaseItem(p Product) Item {
	title := p.DisplayName
	if title == "" {
		title = p.ID
	}
	return Item{Source: Coinbase, Code: p.ID, Title: title, Category: "Spot", Frequency: "continuous",
		What:     "Coinbase spot product: daily, hourly and minute candles recorded; listed on the home page with its live price, whose prints are not recorded",
		Recorder: RecorderCandles, Checked: true, Raw: p.Raw}
}

// Fetchers are the venue calls a refresh makes. Kalshi's are paced by whoever supplies them.
type Fetchers struct {
	SeriesList func(ctx context.Context, category string) ([]byte, error)
	OpenSample func(ctx context.Context, series string) ([]Market, error)
	Products   func(ctx context.Context) ([]byte, error)
}

// Build reads both venues and classifies everything they list. A category or venue that cannot be
// read is left out, so its stored rows keep their last state, and is reported in problems.
func Build(ctx context.Context, f Fetchers, categories []string) (items []Item, problems []error) {
	seen := map[string]bool{}
	for _, category := range categories {
		body, err := f.SeriesList(ctx, category)
		if err != nil {
			problems = append(problems, fmt.Errorf("kalshi category %q: %w", category, err))
			continue
		}
		list, skipped, err := ParseSeries(body)
		if err != nil {
			problems = append(problems, fmt.Errorf("kalshi category %q: %w", category, err))
			continue
		}
		if skipped > 0 {
			problems = append(problems, fmt.Errorf("kalshi category %q: %d entries skipped", category, skipped))
		}
		for _, s := range list {
			if seen[s.Ticker] {
				continue
			}
			seen[s.Ticker] = true
			open, err := f.OpenSample(ctx, s.Ticker)
			if ctx.Err() != nil {
				return items, append(problems, ctx.Err())
			}
			items = append(items, ClassifyKalshi(s, open, err))
		}
	}
	body, err := f.Products(ctx)
	if err == nil {
		var products []Product
		if products, err = ParseProducts(body); err == nil {
			for _, p := range products {
				items = append(items, CoinbaseItem(p))
			}
		}
	}
	if err != nil {
		problems = append(problems, fmt.Errorf("coinbase: %w", err))
	}
	return items, problems
}

// Bases are the Coinbase USD base currencies among items, for telling a Kalshi series' coin.
func Bases(codes []string) []string {
	var out []string
	for _, c := range codes {
		if base, ok := strings.CutSuffix(c, "-USD"); ok && base != "" {
			out = append(out, base)
		}
	}
	return out
}

// Underlying is a Kalshi series' coin, when it can be told: a tag that is a Coinbase USD base
// (KXBTC15M is tagged BTC), else the ticker without its KX prefix and its 15M or D suffix when that
// is exactly a base (KXBCH15M). Otherwise "" and false: the rows are recorded without a price.
// An inference from names, not a reading of the contract terms.
func Underlying(code string, tags, bases []string) (string, bool) {
	known := map[string]bool{}
	for _, b := range bases {
		known[strings.ToUpper(b)] = true
	}
	for _, t := range tags {
		if t = strings.ToUpper(strings.TrimSpace(t)); known[t] {
			return t, true
		}
	}
	stem := strings.TrimPrefix(strings.ToUpper(code), "KX")
	for _, suffix := range []string{"15M", "D"} {
		if s, ok := strings.CutSuffix(stem, suffix); ok && known[s] {
			return s, true
		}
	}
	return "", false
}

// Instrument is the instrument row a selection creates.
type Instrument struct {
	Kind       string
	Underlying string
	Spec       map[string]any
}

// InstrumentFor is the instrument row that records it, with the spec the existing recorder reads:
// round_seconds 900 for a 15-minute series, kind binary_ladder for a ladder, kind spot for a
// product. Every one is "selected": true, which is what keeps it from app.go's start-up list, and
// "trade": false. A 15-minute one is still outside the analysis: store.FifteenMinuteSeries names
// the five series.
func InstrumentFor(it Item, bases []string) (Instrument, error) {
	switch it.Recorder {
	case RecorderRound, RecorderLadder:
		var raw struct {
			Tags []string `json:"tags"`
		}
		_ = json.Unmarshal(it.Raw, &raw)
		u, known := Underlying(it.Code, raw.Tags, bases)
		spec := map[string]any{"selected": true, "trade": false}
		if known {
			spec["price_from"] = "coinbase:" + u + "-USD"
		}
		if it.Recorder == RecorderRound {
			spec["round_seconds"] = RoundSeconds
			return Instrument{Kind: "binary_contract", Underlying: u, Spec: spec}, nil
		}
		spec["ladder"], spec["strike_type"] = true, "greater"
		return Instrument{Kind: "binary_ladder", Underlying: u, Spec: spec}, nil
	case RecorderCandles:
		var raw struct {
			Base string `json:"base_currency"`
		}
		_ = json.Unmarshal(it.Raw, &raw)
		if raw.Base == "" {
			raw.Base, _, _ = strings.Cut(it.Code, "-")
		}
		return Instrument{Kind: "spot", Underlying: raw.Base, Spec: map[string]any{"selected": true, "trade": false, "candles_only": true}}, nil
	}
	return Instrument{}, Refused{it.Code + " is not recordable yet: " + it.WhyNot}
}

// RecorderFor is the recorder an instrument of this kind runs under, "" for none.
func RecorderFor(kind string) string {
	switch kind {
	case "binary_contract":
		return RecorderRound
	case "binary_ladder":
		return RecorderLadder
	case "spot":
		return RecorderCandles
	}
	return ""
}

// Refused is a selection the rules do not allow. Its text is shown on the page.
type Refused struct{ Why string }

func (r Refused) Error() string { return r.Why }

// State is what the database holds for an item when it is switched.
type State struct {
	Exists   bool   // an instrument row exists for it
	Seeded   bool   // that row came from a migration (no "selected" in its spec)
	Active   bool   // that row is active
	Kind     string // that row's kind
	Selected int    // instruments selected and active now, this one included if it is
}

// Action is what a switch does to the instrument table.
type Action int

// The actions.
const (
	Nothing    Action = iota
	Create            // insert the instrument row
	Activate          // set an old selected row active again
	Deactivate        // set it inactive; everything recorded stays
)

// Decide is what switching an item on (record) or off does. max is the most selected at once.
func Decide(it Item, st State, record bool, max int) (Action, error) {
	if st.Seeded {
		return Nothing, Refused{it.Code + " is recorded by the service's own configuration (a migration) and cannot be switched here"}
	}
	if !record {
		if st.Exists && st.Active {
			return Deactivate, nil
		}
		return Nothing, nil
	}
	if st.Exists && st.Active {
		return Nothing, nil
	}
	want, err := InstrumentFor(it, nil)
	if err != nil {
		return Nothing, err
	}
	if st.Exists && st.Kind != want.Kind {
		return Nothing, Refused{fmt.Sprintf("%s was recorded as %s and the catalogue now says %s; it cannot be switched back on here", it.Code, st.Kind, want.Kind)}
	}
	if st.Selected >= max {
		return Nothing, Refused{fmt.Sprintf("%d instruments are selected already, the most at once (AC_MAX_SELECTED=%d); switch one off first", st.Selected, max)}
	}
	if st.Exists {
		return Activate, nil
	}
	return Create, nil
}

// Row is the item as store.UpsertCatalogue writes it.
func (it Item) Row() store.CatalogueRow {
	return store.CatalogueRow{Source: it.Source, Code: it.Code, Title: it.Title, Category: it.Category, Frequency: it.Frequency,
		What: it.What, Recorder: it.Recorder, WhyNot: it.WhyNot, Checked: it.Checked, Raw: it.Raw}
}

// Rules is the switch's rule as store.SetAssetSelection asks for it: Decide, and for a new row,
// InstrumentFor. max is the most selected at once.
func Rules(max int) func(store.AssetState) (store.AssetPlan, error) {
	return func(st store.AssetState) (store.AssetPlan, error) {
		r := st.Item
		it := Item{Source: r.Source, Code: r.Code, Title: r.Title, Category: r.Category, Frequency: r.Frequency,
			What: r.What, Recorder: r.Recorder, WhyNot: r.WhyNot, Checked: r.Checked, Raw: r.Raw}
		action, err := Decide(it, State{Exists: st.Exists, Seeded: st.Seeded, Active: st.Active, Kind: st.Kind, Selected: st.Selected}, st.Record, max)
		if err != nil {
			return store.AssetPlan{}, err
		}
		switch action {
		case Create:
			in, err := InstrumentFor(it, Bases(st.Coinbase))
			if err != nil {
				return store.AssetPlan{}, err
			}
			return store.AssetPlan{Action: "create", Kind: in.Kind, Underlying: in.Underlying, Spec: in.Spec}, nil
		case Activate:
			return store.AssetPlan{Action: "activate"}, nil
		case Deactivate:
			return store.AssetPlan{Action: "deactivate"}, nil
		}
		return store.AssetPlan{}, nil
	}
}

// Selected is an instrument switched on from the assets page, as the supervisor reads it.
type Selected struct {
	ID     int64
	Source string
	Symbol string
	Kind   string
}

// Plan is what the supervisor does to record exactly the wanted instruments: which to start and
// which to stop. running maps an instrument to the recorder it runs under. At most max run: the
// oldest selections (lowest id) first; the rest, and any whose kind no recorder follows, are
// returned in left so they can be logged.
func Plan(running map[int64]string, wanted []Selected, max int) (start []Selected, stop []int64, left []Selected) {
	sorted := append([]Selected(nil), wanted...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	keep := map[int64]string{}
	for _, w := range sorted {
		r := RecorderFor(w.Kind)
		if r == "" || len(keep) >= max || keep[w.ID] != "" {
			if keep[w.ID] == "" {
				left = append(left, w)
			}
			continue
		}
		keep[w.ID] = r
		if running[w.ID] != r {
			start = append(start, w)
		}
	}
	for id, r := range running {
		if keep[id] != r {
			stop = append(stop, id)
		}
	}
	sort.Slice(stop, func(i, j int) bool { return stop[i] < stop[j] })
	return start, stop, left
}
