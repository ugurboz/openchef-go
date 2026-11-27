package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ugurboz/openchef/internal/quota"
)

// RecordObservation stores one quota observation.
func (db *DB) RecordObservation(ctx context.Context, o *quota.Observation) error {
	switch o.Source {
	case quota.SourceStream, quota.SourceProbe, quota.SourceEstimate:
	default:
		return fmt.Errorf("store: record observation: unknown source %q", o.Source)
	}
	if o.Pool == "" || o.At.IsZero() {
		return errors.New("store: record observation: pool and time are required")
	}
	windows := o.Windows
	if windows == nil {
		windows = []quota.Window{}
	}
	raw, err := json.Marshal(windows)
	if err != nil {
		return fmt.Errorf("store: encode quota windows: %w", err)
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO quota_observations (pool, source, at, exhausted, windows) VALUES (?, ?, ?, ?, ?)",
			o.Pool, string(o.Source), formatTime(o.At), boolInt(o.Exhausted), string(raw)); err != nil {
			return fmt.Errorf("store: record observation: %w", err)
		}
		return nil
	})
}

// Observations returns the observations made since since, oldest first.
func (db *DB) Observations(ctx context.Context, since time.Time) ([]quota.Observation, error) {
	return queryAll(ctx, db.reader, scanObservation, "quota observations",
		"SELECT pool, source, at, exhausted, windows FROM quota_observations WHERE at >= ? ORDER BY at, id", formatTime(since))
}

// PruneObservations deletes observations older than before.
func (db *DB) PruneObservations(ctx context.Context, before time.Time) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM quota_observations WHERE at < ?", formatTime(before)); err != nil {
			return fmt.Errorf("store: prune observations: %w", err)
		}
		return nil
	})
}

func scanObservation(r rowScanner) (quota.Observation, error) {
	var (
		o                quota.Observation
		source, at, wins string
		exhausted        int
	)
	if err := r.Scan(&o.Pool, &source, &at, &exhausted, &wins); err != nil {
		return quota.Observation{}, err
	}
	o.Source, o.Exhausted = quota.Source(source), exhausted == 1
	var err error
	if o.At, err = parseTime(at); err != nil {
		return quota.Observation{}, err
	}
	if err := json.Unmarshal([]byte(wins), &o.Windows); err != nil {
		return quota.Observation{}, fmt.Errorf("store: decode quota windows: %w", err)
	}
	return o, nil
}

// Flag returns a stored switch, or "" when it was never set.
func (db *DB) Flag(ctx context.Context, name string) (string, error) {
	var v string
	err := db.reader.QueryRowContext(ctx, "SELECT value FROM flags WHERE name = ?", name).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("store: read flag %s: %w", name, err)
	}
	return v, nil
}

// SetFlag stores a switch.
func (db *DB) SetFlag(ctx context.Context, name, value string) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO flags (name, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			name, value, formatTime(db.now())); err != nil {
			return fmt.Errorf("store: set flag %s: %w", name, err)
		}
		return nil
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
