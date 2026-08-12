package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

var ErrSessionNotFound = errors.New("session not found")

// SessionTTL is how long a web login lasts.
const SessionTTL = 30 * 24 * time.Hour

// Session is a logged-in browser.
type Session struct {
	ID        string
	UserID    int64
	CSRF      string
	CreatedAt string
	ExpiresAt string
}

// CreateSession issues a session and its CSRF token.
func CreateSession(ctx context.Context, x Execer, userID int64) (*Session, error) {
	s := &Session{
		ID:        randomID(32),
		UserID:    userID,
		CSRF:      randomID(32),
		CreatedAt: Now(),
		ExpiresAt: FormatTime(time.Now().Add(SessionTTL)),
	}
	_, err := x.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, csrf, created_at, expires_at) VALUES (?,?,?,?,?)`,
		s.ID, s.UserID, s.CSRF, s.CreatedAt, s.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// GetSession resolves a session cookie, treating an expired one as absent.
func GetSession(ctx context.Context, q Querier, id string) (*Session, error) {
	var s Session
	err := q.QueryRowContext(ctx,
		`SELECT id, user_id, csrf, created_at, expires_at FROM sessions WHERE id = ?`, id).
		Scan(&s.ID, &s.UserID, &s.CSRF, &s.CreatedAt, &s.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	if ParseTime(s.ExpiresAt).Before(time.Now()) {
		return nil, ErrSessionNotFound
	}
	return &s, nil
}

func DeleteSession(ctx context.Context, x Execer, id string) error {
	_, err := x.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteExpiredSessions is called by the janitor.
func DeleteExpiredSessions(ctx context.Context, x Execer) error {
	_, err := x.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, Now())
	return err
}

func randomID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// A session id that cannot be random is a security problem, not a
		// degraded feature; fail loudly rather than issue a guessable one.
		panic("kobibri: no source of randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
