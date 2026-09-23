package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrVersionNotFound means the id is not a version-3 strategy row.
var ErrVersionNotFound = errors.New("store: that version-3 strategy is not registered")

// OrdersSetting is the buckets-page switch for new orders. set is false when nobody has
// written it, and the process then keeps the AC_V3 environment value.
func (s *Store) OrdersSetting(ctx context.Context) (on bool, set bool, err error) {
	var value string
	err = s.pool.QueryRow(ctx, `select value from operator_setting where key = 'orders'`).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return value == "on", true, nil
}

// SetOrdersSetting records the orders switch. It takes effect in this process when the
// runner applies it, and at the next start either way.
func (s *Store) SetOrdersSetting(ctx context.Context, on bool) error {
	value := "off"
	if on {
		value = "on"
	}
	tag, err := s.pool.Exec(ctx, `
		insert into operator_setting (key, value, set_at, set_by, note)
		select 'orders', $1, now(), id, 'set from the buckets page'
		  from actor where handle = 'service'
		on conflict (key) do update
		    set value = excluded.value, set_at = now(), set_by = excluded.set_by, note = excluded.note`, value)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("actor service is missing")
	}
	return nil
}

// Version3 is one version-3 strategy the buckets page can approve or retire.
type Version3 struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Version    int    `json:"version"`
	Status     string `json:"status"`
	Hypothesis string `json:"hypothesis"`
	Params     []byte `json:"-"`      // engine.Params as stored; the page's Remix reads a shape from it
	Family     string `json:"family"` // kalshi15m (the rounds) or kalshiladder (the daily and weekly ladders)
	Held       bool   `json:"held"`   // a bucket of this version is not frozen: Reap applies
	Lives      int    `json:"lives"`  // buckets this version has had, frozen ones included: Restake applies when > 0 and not held
}

// The market families a version may belong to. Each has its own runner and bucket prefix.
const (
	FamilyRounds  = "kalshi15m"
	FamilyLadders = "kalshiladder"
)

// ListVersion3 is the version-3 rows of both families. An empty list is the ordinary state
// before anything is registered.
func (s *Store) ListVersion3(ctx context.Context) ([]Version3, error) {
	rows, err := s.pool.Query(ctx, `
		select v.id, st.name, st.family, v.version, v.status, v.hypothesis, v.params,
		       exists (select 1 from bucket b where b.strategy_version_id = v.id and b.mode = 'sim' and b.status <> 'frozen'),
		       (select count(*) from bucket b where b.strategy_version_id = v.id and b.mode = 'sim')
		  from strategy_version v
		  join strategy st on st.id = v.strategy_id
		 where st.family in ($1, $2) and v.version = 3
		 order by st.name`, FamilyRounds, FamilyLadders)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version3{}
	for rows.Next() {
		var v Version3
		if err := rows.Scan(&v.ID, &v.Name, &v.Family, &v.Version, &v.Status, &v.Hypothesis, &v.Params, &v.Held, &v.Lives); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ErrVersionExists is a builder submission whose strategy already has a version 3.
var ErrVersionExists = errors.New("that strategy already has a version 3")

// NewVersion3 is what the builder registers: a strategy row (made if absent) and its version-3
// row, as draft, with the params the engine built and labelled.
type NewVersion3 struct {
	Name       string // the strategy's name; the params' name, with its label
	Blurb      string // the strategy's description
	Hypothesis string // what the version is meant to test; the registry's text
	Params     []byte // engine.Params as JSON, already validated by the engine
	Parent     string // the parent strategy's name whose version 2 this descends from ("Scalper" or "Value")
	CodeRef    string // the release
	Family     string // FamilyRounds (the default) or FamilyLadders: which runner holds its bucket
}

// CreateVersion3 registers a builder's version as draft and returns its id. One more row in the
// trials registry, which every significance figure is corrected for. The version is created by
// the service actor: the page has no login, and the buckets page's operator key is the gate.
func (s *Store) CreateVersion3(ctx context.Context, v NewVersion3) (int64, error) {
	v.Name, v.Blurb, v.Hypothesis = strings.TrimSpace(v.Name), strings.TrimSpace(v.Blurb), strings.TrimSpace(v.Hypothesis)
	switch {
	case v.Name == "" || len(v.Name) > 80:
		return 0, fmt.Errorf("the name must be 1 to 80 characters")
	case len(v.Blurb) > 300:
		return 0, fmt.Errorf("the blurb is too long")
	case v.Hypothesis == "" || len(v.Hypothesis) > 2000:
		return 0, fmt.Errorf("the hypothesis must be 1 to 2000 characters")
	case len(v.Params) == 0:
		return 0, fmt.Errorf("no params")
	}
	switch v.Family {
	case "":
		v.Family = FamilyRounds
	case FamilyRounds, FamilyLadders:
	default:
		return 0, fmt.Errorf("family must be %s or %s", FamilyRounds, FamilyLadders)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The strategy row: found, or made. Not an upsert: `on conflict do update` needs UPDATE on
	// the table, and the service role may add rows to strategy, never change them (db/grants.sql).
	var strategyID int64
	err = tx.QueryRow(ctx, `select id from strategy where family = $1 and name = $2`, v.Family, v.Name).Scan(&strategyID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `insert into strategy (family, name, description) values ($1, $2, $3) returning id`, v.Family, v.Name, v.Blurb).Scan(&strategyID)
	}
	if err != nil {
		return 0, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from strategy_version where strategy_id = $1 and version = 3)`, strategyID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists {
		return 0, ErrVersionExists
	}
	var id int64
	if err := tx.QueryRow(ctx, `
		insert into strategy_version (strategy_id, version, params, code_ref, hypothesis, parent_version_id, created_by, status)
		select $1, 3, $2::jsonb, $3, $4,
		       (select v.id from strategy_version v join strategy p on p.id = v.strategy_id where p.family = 'kalshi15m' and p.name = $5 and v.version = 2),
		       (select id from actor where handle = 'service'), 'draft'
		returning id`, strategyID, v.Params, v.CodeRef, v.Hypothesis, v.Parent).Scan(&id); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, nil
}

// SetVersionStatus approves (probation or active) or retires one version-3 row.
// A version that is not version 3 is left untouched.
func (s *Store) SetVersionStatus(ctx context.Context, id int64, status, reason string) error {
	switch status {
	case "probation", "active", "retired":
	default:
		return fmt.Errorf("status must be probation, active, or retired")
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		return fmt.Errorf("the note is too long")
	}
	if status == "retired" && reason == "" {
		reason = "retired from the buckets page"
	}
	tag, err := s.pool.Exec(ctx, `
		update strategy_version set
		    status = $2,
		    retired_at = case when $2 = 'retired' then now() else null end,
		    retired_reason = case when $2 = 'retired' then $3 else '' end
		 where id = $1 and version = 3`, id, status, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrVersionNotFound
	}
	return nil
}

// RatesOK reports whether four basis-point rates are a legal allocation.
// Each is between 0 and 10000, and together they stay within 100%.
func RatesOK(winnings, replenish, tax, fees int) error {
	for _, n := range []int{winnings, replenish, tax, fees} {
		if n < 0 || n > 10000 {
			return fmt.Errorf("each rate is a percentage from 0 to 100")
		}
	}
	if winnings+replenish+tax+fees > 10000 {
		return fmt.Errorf("the four rates add up to more than 100%%")
	}
	return nil
}

// SetSimPolicy appends the allocation rule for simulated money. Past rows stay,
// and the newest is the one in force.
func (s *Store) SetSimPolicy(ctx context.Context, winnings, replenish, tax, fees int, note string) error {
	if err := RatesOK(winnings, replenish, tax, fees); err != nil {
		return err
	}
	note = strings.TrimSpace(note)
	if len(note) > 500 {
		return fmt.Errorf("the note is too long")
	}
	tag, err := s.pool.Exec(ctx, `
		insert into skim_policy (mode, winnings_bps, replenish_bps, tax_bps, fees_bps, note, set_by)
		select 'sim', $1, $2, $3, $4, $5, id from actor where handle = 'service'`,
		winnings, replenish, tax, fees, note)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("actor service is missing")
	}
	return nil
}

// ResetCounts is what reset_sim removed. Decisions is the row count only from the first
// version of the function (0017); since 0019 the journal is truncated whole and
// DecisionsTruncated says so.
type ResetCounts struct {
	Fills              int64 `json:"fills"`
	Settlements        int64 `json:"settlements"`
	Orders             int64 `json:"orders"`
	Decisions          int64 `json:"decisions"`
	DecisionsTruncated bool  `json:"decisions_truncated"`
	Buckets            int64 `json:"buckets"`
	Transfers          int64 `json:"transfers"`
	Snapshots          int64 `json:"snapshots"`
	TookMS             int64 `json:"took_ms"`
}

// ResetSim empties the simulated books. The confirmation the function requires is
// passed here as a constant: the page has already demanded the same words.
func (s *Store) ResetSim(ctx context.Context) (ResetCounts, error) {
	var raw []byte
	var out ResetCounts
	if err := s.pool.QueryRow(ctx, `select reset_sim('reset sim')`).Scan(&raw); err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, nil
}
