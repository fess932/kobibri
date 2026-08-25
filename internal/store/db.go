// Package store owns the server's own SQLite database: schema migrations, the
// connection pools, and the queries every other package goes through.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"runtime"
	"time"

	_ "modernc.org/sqlite"
)

// Store holds two pools. Writes go through a single connection, which removes
// SQLITE_BUSY as a class of failure; reads scale across the machine.
type Store struct {
	w    *sql.DB
	r    *sql.DB
	path string
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	w, err := open(path, writerDSN)
	if err != nil {
		return nil, fmt.Errorf("open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxIdleTime(time.Hour)

	if err := w.PingContext(ctx); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}

	s := &Store{w: w, path: path}
	if err := s.migrate(ctx); err != nil {
		_ = w.Close()
		return nil, err
	}

	r, err := open(path, readerDSN)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open reader: %w", err)
	}
	r.SetMaxOpenConns(max(4, runtime.NumCPU()))
	r.SetConnMaxIdleTime(time.Hour)
	s.r = r
	return s, nil
}

func open(path string, dsn func(string) string) (*sql.DB, error) {
	return sql.Open("sqlite", dsn(path))
}

// The pragma syntax below is modernc.org/sqlite's, verified against v1.56.0
// (SQLite 3.53.3); it differs from mattn/go-sqlite3's `_busy_timeout=` form.
func writerDSN(path string) string {
	return "file:" + url.PathEscape(path) + "?" +
		"_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
}

func readerDSN(path string) string {
	return "file:" + url.PathEscape(path) + "?" +
		"_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
}

// Path is the database file this store was opened from.
func (s *Store) Path() string { return s.path }

// Reader returns the pool for read-only queries.
func (s *Store) Reader() *sql.DB { return s.r }

// Writer returns the single-connection pool for writes. Prefer Tx.
func (s *Store) Writer() *sql.DB { return s.w }

func (s *Store) Close() error {
	var first error
	if s.r != nil {
		first = s.r.Close()
	}
	if err := s.w.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// Tx runs fn inside a write transaction, rolling back on error or panic.
func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
