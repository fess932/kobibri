package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "kobibri.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateCreatesSchema(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations embedded")
	}

	got, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if got != len(ms) {
		t.Fatalf("schema version = %d, want %d", got, len(ms))
	}

	// Every table the plan calls for must exist, or a later milestone fails in
	// a much more confusing way.
	want := []string{
		"api_tokens", "book_identities", "books", "cover_cache", "device_tombstones",
		"devices", "kepub_cache", "kepub_failures", "kv", "reading_states", "scan_runs",
		"sessions", "source_acl", "source_book_files", "source_books", "sources",
		"sync_point_books", "sync_point_tags", "sync_points", "sync_runs", "tag_books",
		"tags", "users",
	}
	rows, err := s.Reader().QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var have []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		have = append(have, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)

	missing := diff(want, have)
	if len(missing) > 0 {
		t.Errorf("missing tables: %v", missing)
	}
	if extra := diff(have, want); len(extra) > 0 {
		t.Errorf("unexpected tables: %v (update the test if this is intentional)", extra)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kobibri.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	v1, _ := first.SchemaVersion(ctx)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	v2, _ := second.SchemaVersion(ctx)

	if v1 != v2 {
		t.Fatalf("schema version changed on reopen: %d -> %d", v1, v2)
	}
}

// TestForeignKeysEnforced guards the pragma: without it, ON DELETE CASCADE is
// silently inert and orphaned sync points would pile up forever.
func TestForeignKeysEnforced(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	_, err := s.Writer().ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, csrf, created_at, expires_at)
		 VALUES ('s1', 4242, 'c', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')`)
	if err == nil {
		t.Fatal("insert with a dangling user_id succeeded; foreign_keys pragma is not in effect")
	}
}

// TestWALMode guards the writer DSN: WAL is what lets a paginated sync read
// while a scan is writing.
func TestWALMode(t *testing.T) {
	s := openTemp(t)
	var mode string
	if err := s.Writer().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestTxRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	sentinel := errTest{}
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kv(k, v) VALUES ('a', 'b')`); err != nil {
			return err
		}
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("Tx error = %v, want sentinel", err)
	}

	var n int
	if err := s.Reader().QueryRowContext(ctx, `SELECT count(*) FROM kv`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("kv has %d rows after rollback, want 0", n)
	}
}

type errTest struct{}

func (errTest) Error() string { return "sentinel" }

// diff returns the elements of a that are absent from b.
func diff(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[s] = true
	}
	var out []string
	for _, s := range a {
		if !set[s] {
			out = append(out, s)
		}
	}
	return out
}
