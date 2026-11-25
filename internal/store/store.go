// Package store owns OpenChef's durable state: one SQLite database in WAL mode with
// versioned, forward-only migrations (docs/architecture.md §11).
//
// Concurrency model: a single writer connection runs every write transaction with
// BEGIN IMMEDIATE, so writers queue instead of failing with SQLITE_BUSY on lock
// upgrade; a separate pool of query-only connections serves reads concurrently.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

const (
	driverName    = "sqlite"
	busyTimeoutMS = "5000"
	dirPerm       = 0o700
	filePerm      = 0o600
)

// DB is an open state database.
type DB struct {
	path   string
	writer *sql.DB
	reader *sql.DB
	// now stamps created_at/updated_at and similar columns; tests replace it.
	now func() time.Time
}

// Open opens (creating if necessary) the database at path and applies all pending
// migrations. The parent directory is created with owner-only permissions.
func Open(ctx context.Context, path string) (*DB, error) {
	return open(ctx, path, embeddedMigrations())
}

func open(ctx context.Context, path string, migrations fs.FS) (*DB, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %q: %w", path, err)
	}
	if err := ensureFile(abs); err != nil {
		return nil, err
	}
	migs, err := loadMigrations(migrations)
	if err != nil {
		return nil, err
	}

	writer, err := openPool(abs, writerParams(), 1)
	if err != nil {
		return nil, err
	}
	if err := checkWAL(ctx, writer); err != nil {
		return nil, errors.Join(err, closePool(writer))
	}
	if err := migrate(ctx, writer, migs); err != nil {
		return nil, errors.Join(err, closePool(writer))
	}

	reader, err := openPool(abs, readerParams(), max(4, runtime.NumCPU()))
	if err != nil {
		return nil, errors.Join(err, closePool(writer))
	}
	if err := reader.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("store: open reader: %w", err), closePool(reader), closePool(writer))
	}
	return &DB{path: abs, writer: writer, reader: reader, now: time.Now}, nil
}

// Path returns the absolute path of the database file.
func (db *DB) Path() string { return db.path }

// Reader returns the read-only connection pool. Any write through it fails.
func (db *DB) Reader() *sql.DB { return db.reader }

// Write runs fn in an immediate write transaction. The transaction commits if fn
// returns nil and rolls back otherwise; fn's error is returned unchanged (joined with a
// rollback failure, if any) so callers can match it with errors.Is/As.
func (db *DB) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	return withTx(ctx, db.writer, fn)
}

// Close closes both pools.
func (db *DB) Close() error {
	return errors.Join(closePool(db.reader), closePool(db.writer))
}

func withTx(ctx context.Context, pool *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("store: rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// ensureFile creates the directory and an empty database file with owner-only
// permissions. SQLite gives the -wal and -shm files the database file's permissions.
func ensureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("store: create directory: %w", err)
	}
	//nolint:gosec // G304: the path is the configured state database, not user-controlled input.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: create database file: %w", err)
	}
	return nil
}

func writerParams() url.Values {
	v := url.Values{}
	v.Add("_pragma", "busy_timeout("+busyTimeoutMS+")")
	v.Add("_pragma", "journal_mode(WAL)")
	v.Add("_pragma", "synchronous(NORMAL)")
	v.Add("_pragma", "foreign_keys(1)")
	v.Set("_txlock", "immediate")
	return v
}

func readerParams() url.Values {
	v := url.Values{}
	v.Add("_pragma", "busy_timeout("+busyTimeoutMS+")")
	v.Add("_pragma", "foreign_keys(1)")
	v.Add("_pragma", "query_only(1)")
	return v
}

func openPool(path string, params url.Values, maxConns int) (*sql.DB, error) {
	// The driver splits the DSN at the first '?', so the path is passed as-is.
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("store: database path must not contain '?': %q", path)
	}
	pool, err := sql.Open(driverName, path+"?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	pool.SetMaxOpenConns(maxConns)
	pool.SetMaxIdleConns(maxConns)
	return pool, nil
}

func checkWAL(ctx context.Context, pool *sql.DB) error {
	var mode string
	if err := pool.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("store: read journal mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("store: journal mode is %q, want wal", mode)
	}
	return nil
}

func closePool(pool *sql.DB) error {
	if err := pool.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}
