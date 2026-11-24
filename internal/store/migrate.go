package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func embeddedMigrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		// The embed pattern guarantees the directory exists; this is a build defect.
		panic(fmt.Sprintf("store: embedded migrations: %v", err))
	}
	return sub
}

// Migration errors.
var (
	// ErrInvalidMigration means the embedded migration set is malformed (bad name,
	// duplicate or missing version). It indicates a build defect.
	ErrInvalidMigration = errors.New("store: invalid migration set")
	// ErrSchemaTooNew means the database was migrated by a newer OpenChef; this binary
	// refuses to touch it rather than risk corrupting it.
	ErrSchemaTooNew = errors.New("store: database schema is newer than this binary")
	// ErrMigrationModified means an already-applied migration's content changed.
	// Applied migrations are immutable; schema changes need a new migration.
	ErrMigrationModified = errors.New("store: applied migration was modified")
)

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

// loadMigrations reads NNNN_name.sql files and requires versions 1..n with no gaps.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("%w: read directory: %w", ErrInvalidMigration, err)
	}
	migs := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("%w: bad file name %q", ErrInvalidMigration, e.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("%w: bad version in %q: %w", ErrInvalidMigration, e.Name(), err)
		}
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("%w: read %q: %w", ErrInvalidMigration, e.Name(), err)
		}
		sum := sha256.Sum256(body)
		migs = append(migs, migration{
			version:  version,
			name:     m[2],
			sql:      string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	for i, m := range migs {
		if m.version != i+1 {
			return nil, fmt.Errorf("%w: expected version %d, found %04d_%s", ErrInvalidMigration, i+1, m.version, m.name)
		}
	}
	return migs, nil
}

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER NOT NULL PRIMARY KEY,
    name       TEXT    NOT NULL,
    checksum   TEXT    NOT NULL,
    applied_at TEXT    NOT NULL
) STRICT`

// migrate verifies applied migrations and applies pending ones, each in its own
// immediate transaction. Re-checking inside the transaction makes concurrent openers
// safe even without the daemon's single-instance lock.
func migrate(ctx context.Context, writer *sql.DB, migs []migration) error {
	if _, err := writer.ExecContext(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	applied, err := appliedMigrations(ctx, writer)
	if err != nil {
		return err
	}
	if err := verifyApplied(applied, migs); err != nil {
		return err
	}
	for _, m := range migs {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := withTx(ctx, writer, func(tx *sql.Tx) error { return applyOne(ctx, tx, m) }); err != nil {
			return err
		}
	}
	return nil
}

func appliedMigrations(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (map[int]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only cursor; Err below reports failures
	applied := map[int]string{}
	for rows.Next() {
		var (
			version  int
			checksum string
		)
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	return applied, nil
}

func verifyApplied(applied map[int]string, migs []migration) error {
	known := make(map[int]migration, len(migs))
	for _, m := range migs {
		known[m.version] = m
	}
	for version, checksum := range applied {
		m, ok := known[version]
		if !ok {
			return fmt.Errorf("%w: database has version %d, binary knows up to %d", ErrSchemaTooNew, version, len(migs))
		}
		if m.checksum != checksum {
			return fmt.Errorf("%w: %04d_%s", ErrMigrationModified, m.version, m.name)
		}
	}
	return nil
}

func applyOne(ctx context.Context, tx *sql.Tx, m migration) error {
	var exists int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM schema_migrations WHERE version = ?", m.version).Scan(&exists)
	switch {
	case err == nil:
		return nil // applied concurrently by another opener
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("store: check migration %04d: %w", m.version, err)
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: apply migration %04d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
		m.version, m.name, m.checksum, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("store: record migration %04d: %w", m.version, err)
	}
	return nil
}

// SchemaVersion returns the highest applied migration version.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := db.reader.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	return int(v.Int64), nil
}
