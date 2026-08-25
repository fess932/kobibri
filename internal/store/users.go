package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

var (
	ErrUserNotFound  = errors.New("user not found")
	ErrTokenNotFound = errors.New("token not found")
)

// User is an account that owns devices and reading progress.
type User struct {
	ID           int64
	Name         string
	PasswordHash string
	IsAdmin      bool
	Disabled     bool
	CreatedAt    string
}

func CreateUser(ctx context.Context, x Execer, name, passwordHash string, isAdmin bool) (int64, error) {
	res, err := x.ExecContext(ctx,
		`INSERT INTO users (name, password_hash, is_admin, created_at) VALUES (?,?,?,?)`,
		name, passwordHash, isAdmin, Now())
	if err != nil {
		return 0, fmt.Errorf("create user %q: %w", name, err)
	}
	return res.LastInsertId()
}

func GetUser(ctx context.Context, q Querier, id int64) (*User, error) {
	var u User
	err := q.QueryRowContext(ctx,
		`SELECT id, name, password_hash, is_admin, disabled, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Name, &u.PasswordHash, &u.IsAdmin, &u.Disabled, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", ErrUserNotFound, id)
	}
	return &u, err
}

func GetUserByName(ctx context.Context, q Querier, name string) (*User, error) {
	var u User
	err := q.QueryRowContext(ctx,
		`SELECT id, name, password_hash, is_admin, disabled, created_at FROM users WHERE name = ?`, name).
		Scan(&u.ID, &u.Name, &u.PasswordHash, &u.IsAdmin, &u.Disabled, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrUserNotFound, name)
	}
	return &u, err
}

func CountUsers(ctx context.Context, q Querier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// APIToken is the opaque secret embedded in a device's api_endpoint URL.
//
// Only its hash is stored. The token lives in the device's config file in clear
// text forever and cannot be rotated without re-pairing, so it is issued per
// device: one can be revoked without disturbing the others.
type APIToken struct {
	TokenHash  string
	TokenHint  string
	UserID     int64
	Label      string
	CreatedAt  string
	LastUsedAt string
	RevokedAt  string
}

// HashToken is the one-way mapping from the URL secret to what we store.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CreateAPIToken issues a token and returns the raw secret, which is the only
// time it can ever be read.
func CreateAPIToken(ctx context.Context, x Execer, userID int64, label string) (raw string, err error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	raw = hex.EncodeToString(buf)

	_, err = x.ExecContext(ctx,
		`INSERT INTO api_tokens (token_hash, token_hint, user_id, label, created_at)
		 VALUES (?,?,?,?,?)`,
		HashToken(raw), raw[:6], userID, label, Now())
	if err != nil {
		return "", fmt.Errorf("create api token: %w", err)
	}
	return raw, nil
}

// LookupAPIToken resolves a raw token from a request path. Revoked tokens do
// not resolve.
func LookupAPIToken(ctx context.Context, q Querier, raw string) (*APIToken, error) {
	var (
		t        APIToken
		lastUsed sql.NullString
		revoked  sql.NullString
	)
	err := q.QueryRowContext(ctx,
		`SELECT token_hash, token_hint, user_id, label, created_at, last_used_at, revoked_at
		 FROM api_tokens WHERE token_hash = ?`, HashToken(raw)).
		Scan(&t.TokenHash, &t.TokenHint, &t.UserID, &t.Label, &t.CreatedAt, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, err
	}
	if revoked.Valid && revoked.String != "" {
		return nil, ErrTokenNotFound
	}
	t.LastUsedAt = lastUsed.String
	return &t, nil
}

func TouchAPIToken(ctx context.Context, x Execer, tokenHash string) error {
	_, err := x.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ? WHERE token_hash = ?`, Now(), tokenHash)
	return err
}

func RevokeAPIToken(ctx context.Context, x Execer, tokenHash string) error {
	_, err := x.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = ? WHERE token_hash = ?`, Now(), tokenHash)
	return err
}

func ListAPITokens(ctx context.Context, q Querier, userID int64) ([]APIToken, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT token_hash, token_hint, user_id, label, created_at,
		        COALESCE(last_used_at, ''), COALESCE(revoked_at, '')
		 FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.TokenHash, &t.TokenHint, &t.UserID, &t.Label,
			&t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
