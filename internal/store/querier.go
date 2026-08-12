package store

import (
	"context"
	"database/sql"
)

// Querier and Execer let queries run against either a pool or a transaction, so
// ingest can compose the same helpers inside and outside a write transaction.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Querier
}

var (
	_ Execer = (*sql.DB)(nil)
	_ Execer = (*sql.Tx)(nil)
)
