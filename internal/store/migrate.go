package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads the embedded migrations, ordered by their numeric prefix.
// Files are named `<version>_<name>.sql`, version starting at 1 and dense.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}

	var ms []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, rest, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q: expected <version>_<name>.sql", e.Name())
		}
		v, err := strconv.Atoi(prefix)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("migration %q: bad version prefix %q", e.Name(), prefix)
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: rest, sql: string(b)})
	}

	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration versions must be dense from 1: got %d at position %d", m.version, i+1)
		}
	}
	return ms, nil
}

// migrate steps user_version up to the highest embedded migration. Each
// migration runs in its own transaction together with its version bump, so an
// interrupted upgrade never leaves a half-applied schema.
func (s *Store) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}

	var current int
	if err := s.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if current > len(ms) {
		return fmt.Errorf("database schema is version %d but this binary only knows %d: "+
			"downgrade is not supported", current, len(ms))
	}
	if current == len(ms) {
		return nil
	}

	for _, m := range ms {
		if m.version <= current {
			continue
		}
		slog.Info("applying migration", "version", m.version, "name", m.name)

		// PRAGMA user_version does not accept a bound parameter.
		bump := fmt.Sprintf("PRAGMA user_version = %d", m.version)
		err := s.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %d_%s: %w", m.version, m.name, err)
			}
			_, err := tx.ExecContext(ctx, bump)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion reports the applied schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.r.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}
