package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

var (
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict means a compare-and-set lost: the row is not in the state the caller
	// expected (another writer moved it first, or the caller's view is stale).
	ErrConflict = errors.New("store: state conflict")
)

// ConflictError reports the state a row was actually in. It matches ErrConflict.
type ConflictError struct {
	Entity string
	Key    string
	Want   string
	Got    string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("store: %s %s is %q, expected %q", e.Entity, e.Key, e.Got, e.Want)
}

// Is reports whether target is ErrConflict.
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// timeLayout is fixed-width so text order equals time order.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// nullTime stores the zero time as NULL.
func nullTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(t), Valid: true}
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: parse time %q: %w", s, err)
	}
	return t, nil
}

func parseNullTime(s sql.NullString) (time.Time, error) {
	if !s.Valid {
		return time.Time{}, nil
	}
	return parseTime(s.String)
}

// nullString stores "" as NULL.
func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func formatToken(v uint64) string { return strconv.FormatUint(v, 10) }

func parseToken(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: parse start token %q: %w", s, err)
	}
	return v, nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// casFailed explains why a guarded UPDATE touched no row: the row is missing
// (ErrNotFound) or in another state (*ConflictError). stateQuery selects the current
// state of the row identified by args.
func casFailed(ctx context.Context, q queryer, entity, key, want, stateQuery string, args ...any) error {
	var got string
	err := q.QueryRowContext(ctx, stateQuery, args...).Scan(&got)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s %s", ErrNotFound, entity, key)
	case err != nil:
		return fmt.Errorf("store: read %s %s: %w", entity, key, err)
	}
	return &ConflictError{Entity: entity, Key: key, Want: want, Got: got}
}

// execOne runs a guarded UPDATE and reports whether exactly one row changed.
func execOne(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("store: exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: rows affected: %w", err)
	}
	return n == 1, nil
}
