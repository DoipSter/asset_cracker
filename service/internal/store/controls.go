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
}

// ListVersion3 is the version-3 rows of the kalshi15m family. An empty list is the
// ordinary state: those versions are registered only after their measured numbers exist.
func (s *Store) ListVersion3(ctx context.Context) ([]Version3, error) {
	rows, err := s.pool.Query(ctx, `
		select v.id, st.name, v.version, v.status, v.hypothesis
		  from strategy_version v
		  join strategy st on st.id = v.strategy_id
		 where st.family = 'kalshi15m' and v.version = 3
		 order by st.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version3{}
	for rows.Next() {
		var v Version3
		if err := rows.Scan(&v.ID, &v.Name, &v.Version, &v.Status, &v.Hypothesis); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
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

// ResetCounts is what reset_sim removed.
type ResetCounts struct {
	Fills       int64 `json:"fills"`
	Settlements int64 `json:"settlements"`
	Orders      int64 `json:"orders"`
	Decisions   int64 `json:"decisions"`
	Buckets     int64 `json:"buckets"`
	Transfers   int64 `json:"transfers"`
	Snapshots   int64 `json:"snapshots"`
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
