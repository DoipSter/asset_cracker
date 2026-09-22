package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The catalogue (migration 0018): what the venues list, the assets page's search of it, and the
// switch that records an item or stops recording it. The rules (which recorder, which spec, the
// limit) are the catalogue package's; this file reads and writes. WRITTEN WITHOUT A DATABASE
// (2026-09-21).

// CatalogueRow is one row of catalogue_item as a refresh writes it.
type CatalogueRow struct {
	Source, Code, Title, Category, Frequency string
	What, Recorder, WhyNot                   string
	Checked                                  bool // false: keep the stored recorder, what and why_not
	Raw                                      []byte
}

const sqlUpsertCatalogue = `
	insert into catalogue_item (source, code, title, category, frequency, what, recorder, why_not, checked_at, first_seen, last_seen, raw)
	values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10, $11)
	on conflict (source, code) do update set
	    title = excluded.title, category = excluded.category, frequency = excluded.frequency,
	    last_seen = excluded.last_seen, raw = excluded.raw,
	    what       = case when excluded.checked_at is null then catalogue_item.what     else excluded.what end,
	    recorder   = case when excluded.checked_at is null then catalogue_item.recorder else excluded.recorder end,
	    why_not    = case when excluded.checked_at is null then catalogue_item.why_not  else excluded.why_not end,
	    checked_at = coalesce(excluded.checked_at, catalogue_item.checked_at)`

// UpsertCatalogue stores one refresh. An item whose kind could not be read (Checked false) updates
// its listing and keeps the recorder it had, or is stored as not recordable if it is new.
func (s *Store) UpsertCatalogue(ctx context.Context, items []CatalogueRow, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b := &pgx.Batch{}
	for _, it := range items {
		var checked *time.Time
		if it.Checked {
			checked = &at
		}
		raw := it.Raw
		if len(raw) == 0 || !json.Valid(raw) {
			raw = []byte(`{}`)
		}
		b.Queue(sqlUpsertCatalogue, it.Source, it.Code, it.Title, it.Category, it.Frequency, it.What, it.Recorder, it.WhyNot, checked, at, raw)
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CatalogueHit is one search result: an item, and whether the service records it.
type CatalogueHit struct {
	Source     string    `json:"source"`
	Code       string    `json:"code"`
	Title      string    `json:"title"`
	Category   string    `json:"category"`
	Frequency  string    `json:"frequency"`
	What       string    `json:"what"`
	Recordable bool      `json:"recordable"`
	WhyNot     string    `json:"why_not,omitempty"`
	LastSeen   time.Time `json:"last_seen"`
	Recorded   bool      `json:"recorded"` // an active instrument records it
	Selected   bool      `json:"selected"` // that instrument was switched on here, so it can be switched off
	Seeded     bool      `json:"seeded"`   // that instrument came from a migration: not switchable here
}

const sqlSearchCatalogue = `
	select c.source, c.code, c.title, c.category, c.frequency, c.what, c.recorder <> '', c.why_not, c.last_seen,
	       coalesce(i.active, false), coalesce(i.spec @> '{"selected": true}'::jsonb, false), i.id is not null
	  from catalogue_item c
	  join source s on s.code = c.source
	  left join instrument i on i.source_id = s.id and i.symbol = c.code
	 where ($1 = '' or strpos(lower(c.code), lower($1)) > 0 or strpos(lower(c.title), lower($1)) > 0)
	   and ($2 = '' or c.source = $2)
	   and ($3 = '' or c.frequency = $3)
	 order by coalesce(i.active, false) desc, lower(c.code) = lower($1) desc, strpos(lower(c.code), lower($1)) = 1 desc,
	          c.recorder <> '' desc, c.source, c.code
	 limit $4`

// SearchCatalogue matches q as a substring of the code or the title, recorded items first.
func (s *Store) SearchCatalogue(ctx context.Context, q, source, frequency string, limit int) ([]CatalogueHit, error) {
	rows, err := s.pool.Query(ctx, sqlSearchCatalogue, q, source, frequency, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogueHit{}
	for rows.Next() {
		var h CatalogueHit
		var selected, exists bool
		if err := rows.Scan(&h.Source, &h.Code, &h.Title, &h.Category, &h.Frequency, &h.What, &h.Recordable, &h.WhyNot, &h.LastSeen,
			&h.Recorded, &selected, &exists); err != nil {
			return nil, err
		}
		h.Selected, h.Seeded = exists && selected, exists && !selected
		out = append(out, h)
	}
	return out, rows.Err()
}

// CatalogueSummary is how big the catalogue is and when it was last refreshed.
type CatalogueSummary struct {
	Items      int        `json:"items"`
	Recordable int        `json:"recordable"`
	Refreshed  *time.Time `json:"refreshed"` // the newest last_seen; null before the first refresh
}

// CatalogueSummary counts the catalogue.
func (s *Store) CatalogueSummary(ctx context.Context) (CatalogueSummary, error) {
	var out CatalogueSummary
	err := s.pool.QueryRow(ctx, `select count(*), count(*) filter (where recorder <> ''), max(last_seen) from catalogue_item`).
		Scan(&out.Items, &out.Recordable, &out.Refreshed)
	return out, err
}

// Recorded is one active instrument, for the assets page's list of what is being recorded.
type Recorded struct {
	ID        int64  `json:"id"`
	Source    string `json:"source"`
	Symbol    string `json:"symbol"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Frequency string `json:"frequency"`
	Selected  bool   `json:"selected"` // switched on from the assets page
}

// RecordedInstruments lists the active instruments: the seeded ones, then the selected ones.
func (s *Store) RecordedInstruments(ctx context.Context) ([]Recorded, error) {
	rows, err := s.pool.Query(ctx, `
		select i.id, s.code, i.symbol, i.kind, coalesce(c.title, ''), coalesce(c.frequency, ''), i.spec @> '{"selected": true}'::jsonb
		  from instrument i
		  join source s on s.id = i.source_id
		  left join catalogue_item c on c.source = s.code and c.code = i.symbol
		 where i.active
		 order by 7, s.code, i.symbol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Recorded{}
	for rows.Next() {
		var r Recorded
		if err := rows.Scan(&r.ID, &r.Source, &r.Symbol, &r.Kind, &r.Title, &r.Frequency, &r.Selected); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SelectedInstruments are the active instruments switched on from the assets page: the ones the
// supervisor (app/assets.go) runs, and that app.go's start-up list leaves out.
func (s *Store) SelectedInstruments(ctx context.Context) ([]Instrument, error) {
	rows, err := s.pool.Query(ctx, `
		select i.id, s.code, i.kind, i.symbol, i.underlying, i.spec
		  from instrument i join source s on s.id = i.source_id
		 where i.active and i.spec @> '{"selected": true}'::jsonb order by i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instrument
	for rows.Next() {
		var in Instrument
		var spec []byte
		if err := rows.Scan(&in.ID, &in.Source, &in.Kind, &in.Symbol, &in.Underlying, &spec); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(spec, &in.Spec); err != nil {
			return nil, fmt.Errorf("instrument %s spec: %w", in.Symbol, err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// AssetChange is one press of the assets page's Record switch.
type AssetChange struct {
	Source, Code string
	Record       bool
	Via          string // what asked, for the journal
}

// ErrNotCatalogued is a switch of something the catalogue does not list.
var ErrNotCatalogued = errors.New("not in the catalogue")

// AssetState is what the database holds for an item when it is switched.
type AssetState struct {
	Item     CatalogueRow
	Record   bool     // what the switch asks for
	Exists   bool     // an instrument row exists for it
	Seeded   bool     // that row came from a migration: its spec has no "selected"
	Active   bool     // that row is active
	Kind     string   // that row's kind
	Selected int      // instruments selected and active now
	Coinbase []string // the catalogue's Coinbase product codes, for telling a Kalshi series' coin
}

// AssetPlan is what a switch does: Action "" (nothing), "create" (insert Kind, Underlying and
// Spec), "activate" or "deactivate".
type AssetPlan struct {
	Action     string
	Kind       string
	Underlying string
	Spec       map[string]any
}

// AssetResult is what the switch did.
type AssetResult struct {
	InstrumentID int64 `json:"instrument_id"`
	Record       bool  `json:"record"`
	Changed      bool  `json:"changed"`
	Selected     int   `json:"selected"` // instruments selected and active after it
}

// SetAssetSelection switches recording of a catalogue item on or off as decide says, and journals
// a change in asset_selection. One at a time (an advisory lock), so that two presses cannot both
// pass the limit. decide's error is returned as it is; an item not listed is ErrNotCatalogued.
func (s *Store) SetAssetSelection(ctx context.Context, c AssetChange, decide func(AssetState) (AssetPlan, error)) (AssetResult, error) {
	out := AssetResult{Record: c.Record}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('asset_selection'))`); err != nil {
		return out, err
	}

	st := AssetState{Item: CatalogueRow{Source: c.Source, Code: c.Code}, Record: c.Record}
	it := &st.Item
	err = tx.QueryRow(ctx, `
		select title, category, frequency, what, recorder, why_not, raw, checked_at is not null
		  from catalogue_item where source = $1 and code = $2`,
		c.Source, c.Code).Scan(&it.Title, &it.Category, &it.Frequency, &it.What, &it.Recorder, &it.WhyNot, &it.Raw, &it.Checked)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotCatalogued
	}
	if err != nil {
		return out, err
	}
	var (
		sourceID int64
		id       *int64
		active   *bool
		selected *bool
		kind     *string
	)
	if err := tx.QueryRow(ctx, `
		select s.id, i.id, i.active, i.spec @> '{"selected": true}'::jsonb, i.kind
		  from source s left join instrument i on i.source_id = s.id and i.symbol = $2
		 where s.code = $1`, c.Source, c.Code).Scan(&sourceID, &id, &active, &selected, &kind); err != nil {
		return out, err
	}
	if id != nil {
		st.Exists, st.Active, st.Seeded, st.Kind = true, *active, !*selected, *kind
		out.InstrumentID = *id
	}
	if err := tx.QueryRow(ctx, `select count(*) from instrument where active and spec @> '{"selected": true}'::jsonb`).Scan(&st.Selected); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `select code from catalogue_item where source = 'coinbase'`)
	if err != nil {
		return out, err
	}
	if st.Coinbase, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return out, err
	}
	out.Selected = st.Selected
	plan, err := decide(st)
	if err != nil || plan.Action == "" {
		return out, err
	}

	switch plan.Action {
	case "create":
		if st.Exists {
			return out, fmt.Errorf("%s: an instrument exists already", c.Code)
		}
		spec, err := json.Marshal(plan.Spec)
		if err != nil {
			return out, err
		}
		if err := tx.QueryRow(ctx, `insert into instrument (source_id, kind, symbol, underlying, spec) values ($1, $2, $3, $4, $5) returning id`,
			sourceID, plan.Kind, c.Code, plan.Underlying, spec).Scan(&out.InstrumentID); err != nil {
			return out, err
		}
		out.Selected++
	case "activate", "deactivate":
		if !st.Exists || st.Seeded {
			return out, fmt.Errorf("%s: no selected instrument to %s", c.Code, plan.Action)
		}
		on := plan.Action == "activate"
		if _, err := tx.Exec(ctx, `update instrument set active = $2 where id = $1`, out.InstrumentID, on); err != nil {
			return out, err
		}
		if on {
			out.Selected++
		} else {
			out.Selected--
		}
	default:
		return out, fmt.Errorf("unknown action %q", plan.Action)
	}
	tag, err := tx.Exec(ctx, `
		insert into asset_selection (actor_id, via, source, code, instrument_id, record)
		select id, $1, $2, $3, $4, $5 from actor where handle = 'service'`, c.Via, c.Source, c.Code, out.InstrumentID, c.Record)
	if err != nil {
		return out, err
	}
	if tag.RowsAffected() != 1 {
		return out, fmt.Errorf("actor service is missing")
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	out.Changed = true
	return out, nil
}
