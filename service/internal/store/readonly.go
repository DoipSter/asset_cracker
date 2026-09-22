package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Table is a query's answer in the shape the read surface hands to agents (docs/mcp-read-
// surface.md): the column names once, then every row as an array in that order. Compact on the
// wire, and a caller that wants records zips the two. Times are RFC 3339 in UTC, numbers are
// numbers, jsonb columns are the JSON itself.
type Table struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	// Next is the cursor that continues this read where it stopped: pass it back as `after`.
	// Empty when the window is complete.
	Next string `json:"next,omitempty"`
	// Truncated says the window held more than the cap; Next is set when it is.
	Truncated bool `json:"truncated,omitempty"`
	// Note explains units and conventions a reader of the rows must know.
	Note string `json:"note,omitempty"`
}

// Querier is the part of a connection or transaction a read needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ReadOnly runs fn inside a transaction that cannot write: Postgres itself refuses every insert,
// update, delete and DDL in a READ ONLY transaction, whatever the role, so nothing fn does can
// change the record. Every statement in it is cut off after timeout, so a careless window
// cannot occupy a connection for long. The transaction is REPEATABLE READ: every statement in
// fn sees the same snapshot, so a page and the count that frames it agree.
//
// Where the connected role is a member of assetcracker_ro AND that role is set up in this
// database (it can use the public schema: deploy/pi/setup-prod-db.sh grants that in the real
// database only), the transaction also takes that role, and can then read only what the
// read-only role may. Roles are cluster-wide, so the membership alone is not enough: in the dev
// database the deploy user is a member, but the role has no grants there, and taking it would
// make every table vanish ("relation does not exist" is what Postgres says for a schema the
// role may not use). There the transaction runs as the connected user, still read-only.
func (s *Store) ReadOnly(ctx context.Context, timeout time.Duration, fn func(Querier) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return fmt.Errorf("begin read-only: %w", err)
	}
	defer func() {
		if rerr := tx.Rollback(ctx); rerr != nil && err == nil && rerr != pgx.ErrTxClosed {
			err = rerr
		}
	}()
	// SET takes no bind parameters; the value is an integer this code made, never a caller's.
	if _, err := tx.Exec(ctx, fmt.Sprintf("set local statement_timeout = %d", timeout.Milliseconds())); err != nil {
		return fmt.Errorf("statement timeout: %w", err)
	}
	var member bool
	if err := tx.QueryRow(ctx, `
		select case when exists (select 1 from pg_roles where rolname = 'assetcracker_ro')
		            then pg_has_role(current_user, 'assetcracker_ro', 'member')
		                 and has_schema_privilege('assetcracker_ro', 'public', 'usage')
		            else false end`).Scan(&member); err != nil {
		return fmt.Errorf("role check: %w", err)
	}
	if member {
		if _, err := tx.Exec(ctx, "set local role assetcracker_ro"); err != nil {
			return fmt.Errorf("set role: %w", err)
		}
	}
	return fn(tx)
}

// QueryTable runs one statement and returns its whole answer as a Table. Column names are the
// statement's own, so a query names its columns for the reader. Postgres's types are turned into
// JSON-friendly values: numerics become float64, jsonb stays JSON, times stay time.Time (which
// marshal as RFC 3339). Callers cap their own rows with LIMIT; this reads whatever comes back.
func QueryTable(ctx context.Context, q Querier, sql string, args ...any) (Table, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return Table{}, err
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	t := Table{Columns: make([]string, len(fds)), Rows: [][]any{}}
	for i, f := range fds {
		t.Columns[i] = f.Name
	}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return Table{}, err
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = jsonValue(v, fds[i].DataTypeOID)
		}
		t.Rows = append(t.Rows, row)
	}
	return t, rows.Err()
}

// jsonValue makes one Postgres value something encoding/json renders plainly.
func jsonValue(v any, oid uint32) any {
	switch x := v.(type) {
	case nil:
		return nil
	case pgtype.Numeric:
		if !x.Valid {
			return nil
		}
		if f, err := x.Float64Value(); err == nil && f.Valid {
			return f.Float64
		}
		if s, err := x.Value(); err == nil {
			return s
		}
		return nil
	case time.Time:
		return x.UTC()
	case []byte:
		if oid == pgtype.JSONBOID || oid == pgtype.JSONOID {
			return json.RawMessage(x)
		}
		return string(x)
	case pgtype.Interval:
		if !x.Valid {
			return nil
		}
		return (time.Duration(x.Microseconds) * time.Microsecond).String()
	default:
		return v
	}
}
