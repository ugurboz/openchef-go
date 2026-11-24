package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func testMigrations(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

var baseMigrations = map[string]string{
	"0001_items.sql": "CREATE TABLE items (id INTEGER PRIMARY KEY, n INTEGER NOT NULL) STRICT;",
	"0002_tags.sql": `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL) STRICT;
CREATE INDEX tags_name ON tags(name);`,
}

func openTest(t *testing.T, path string, files map[string]string) *DB {
	t.Helper()
	db, err := open(context.Background(), path, testMigrations(files))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return db
}

func dbPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state", "state.db")
}

func TestOpenEmbeddedMigrations(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	v, err := db.SchemaVersion(context.Background())
	if err != nil || v < 1 {
		t.Fatalf("SchemaVersion = %d, %v; want >= 1", v, err)
	}
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM meta").Scan(&n); err != nil {
		t.Fatalf("meta table missing: %v", err)
	}
}

func TestOpenCreatesPrivateFileInWALMode(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	db := openTest(t, path, baseMigrations)

	if db.Path() != path {
		t.Errorf("Path() = %q, want %q", db.Path(), path)
	}
	var mode string
	if err := db.Reader().QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	if runtime.GOOS == "windows" {
		return // POSIX permission bits are not meaningful on Windows
	}
	for p, want := range map[string]os.FileMode{path: filePerm, filepath.Dir(path): dirPerm} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %v, want %v", p, got, want)
		}
	}
}

func TestMigrationsAreAppliedOnce(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	db := openTest(t, path, baseMigrations)
	if err := db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "INSERT INTO items (n) VALUES (7)")
		return err
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen with an extra migration: only the new one runs; data survives.
	more := map[string]string{"0003_more.sql": "ALTER TABLE items ADD COLUMN note TEXT;"}
	for k, v := range baseMigrations {
		more[k] = v
	}
	db2 := openTest(t, path, more)
	v, err := db2.SchemaVersion(context.Background())
	if err != nil || v != 3 {
		t.Fatalf("SchemaVersion = %d, %v; want 3", v, err)
	}
	var n int
	if err := db2.Reader().QueryRowContext(t.Context(), "SELECT n FROM items").Scan(&n); err != nil || n != 7 {
		t.Fatalf("data lost across reopen: n=%d err=%v", n, err)
	}
}

func TestSchemaTooNewIsRejected(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	db := openTest(t, path, baseMigrations)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	older := map[string]string{"0001_items.sql": baseMigrations["0001_items.sql"]}
	_, err := open(context.Background(), path, testMigrations(older))
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("got %v, want ErrSchemaTooNew", err)
	}
}

func TestModifiedMigrationIsRejected(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	db := openTest(t, path, baseMigrations)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	edited := map[string]string{
		"0001_items.sql": baseMigrations["0001_items.sql"] + "\n-- edited",
		"0002_tags.sql":  baseMigrations["0002_tags.sql"],
	}
	_, err := open(context.Background(), path, testMigrations(edited))
	if !errors.Is(err, ErrMigrationModified) {
		t.Fatalf("got %v, want ErrMigrationModified", err)
	}
}

func TestFailedMigrationLeavesNoTrace(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	broken := map[string]string{
		"0001_items.sql": baseMigrations["0001_items.sql"],
		"0002_broken.sql": `CREATE TABLE half (id INTEGER PRIMARY KEY) STRICT;
THIS IS NOT SQL;`,
	}
	if _, err := open(context.Background(), path, testMigrations(broken)); err == nil {
		t.Fatal("expected migration failure")
	}

	db := openTest(t, path, map[string]string{"0001_items.sql": baseMigrations["0001_items.sql"]})
	v, err := db.SchemaVersion(context.Background())
	if err != nil || v != 1 {
		t.Fatalf("SchemaVersion = %d, %v; want 1", v, err)
	}
	var name string
	err = db.Reader().QueryRowContext(t.Context(), "SELECT name FROM sqlite_master WHERE name = 'half'").Scan(&name)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("partially applied migration left table behind (err=%v)", err)
	}
}

func TestInvalidMigrationSets(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]string{
		"gap":       {"0001_a.sql": "SELECT 1;", "0003_c.sql": "SELECT 1;"},
		"bad name":  {"1_a.sql": "SELECT 1;"},
		"uppercase": {"0001_A.sql": "SELECT 1;"},
		"not first": {"0002_b.sql": "SELECT 1;"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := open(context.Background(), dbPath(t), testMigrations(files))
			if !errors.Is(err, ErrInvalidMigration) {
				t.Fatalf("got %v, want ErrInvalidMigration", err)
			}
		})
	}
}

func TestWriteRollsBackAndPreservesError(t *testing.T) {
	t.Parallel()
	db := openTest(t, dbPath(t), baseMigrations)
	sentinel := errors.New("boom")

	err := db.Write(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO items (n) VALUES (1)"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want sentinel error", err)
	}
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM items").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rollback failed: count=%d err=%v", n, err)
	}
}

func TestConcurrentWritesAreSerialized(t *testing.T) {
	t.Parallel()
	db := openTest(t, dbPath(t), baseMigrations)
	if err := db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "INSERT INTO items (id, n) VALUES (1, 0)")
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				// Read-modify-write: lost updates would show up as a wrong total.
				errs <- db.Write(context.Background(), func(tx *sql.Tx) error {
					var n int
					if err := tx.QueryRowContext(t.Context(), "SELECT n FROM items WHERE id = 1").Scan(&n); err != nil {
						return err
					}
					_, err := tx.ExecContext(t.Context(), "UPDATE items SET n = ? WHERE id = 1", n+1)
					return err
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), "SELECT n FROM items WHERE id = 1").Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != workers*perWorker {
		t.Fatalf("n = %d, want %d (lost updates)", n, workers*perWorker)
	}
}

func TestReaderIsReadOnly(t *testing.T) {
	t.Parallel()
	db := openTest(t, dbPath(t), baseMigrations)
	_, err := db.Reader().ExecContext(t.Context(), "INSERT INTO items (n) VALUES (1)")
	if err == nil {
		t.Fatal("write through reader pool succeeded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") &&
		!strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Fatalf("unexpected error kind: %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	t.Parallel()
	db := openTest(t, dbPath(t), baseMigrations)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := db.Write(ctx, func(*sql.Tx) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if _, err := Open(ctx, dbPath(t)); err == nil {
		t.Fatal("Open with cancelled context succeeded")
	}
}

func TestOpenRejectsBadPaths(t *testing.T) {
	t.Parallel()
	if _, err := Open(context.Background(), ""); err == nil {
		t.Error("empty path: expected error")
	}
	bad := filepath.Join(t.TempDir(), "what?.db")
	if _, err := Open(context.Background(), bad); err == nil {
		t.Error("path with '?': expected error")
	}
}
