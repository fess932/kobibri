package store

import (
	"context"
	"database/sql"
	"errors"
)

// GetKV reads a small server-side setting or cached value. A missing key
// returns "" rather than an error, since every caller treats it as a default.
func GetKV(ctx context.Context, q Querier, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT v FROM kv WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func SetKV(ctx context.Context, x Execer, key, value string) error {
	_, err := x.ExecContext(ctx,
		`INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		key, value)
	return err
}
