package store

import (
	"context"
	"database/sql"
	"time"
)

// ReadingEvent is one progress report from a device, as it arrived.
type ReadingEvent struct {
	UserID     int64
	BookID     string
	DeviceID   int64
	At         time.Time
	DeviceAt   time.Time
	Status     string
	Percent    *float64
	Source     string
	Block      *int
	Span       string
	Spent      *int
	Remaining  *int
	SpentDelta int
}

// AppendReadingEvent records a progress report and returns whether it was kept.
//
// A report saying nothing new is dropped: a device that is woken, opens a book
// and closes it again sends the position it already sent, and a history full of
// those turns every average into a lie about how long the book took.
//
// The delta is computed against the last event from the same device, because
// SpentReadingMinutes is that device's own counter. A reader who moves to
// another Kobo starts a second counter, and subtracting one from the other
// would invent hours of reading or wipe them out.
func AppendReadingEvent(ctx context.Context, tx *sql.Tx, ev ReadingEvent) (bool, error) {
	var (
		lastSpent            sql.NullInt64
		lastBlock            sql.NullInt64
		lastSource, lastSpan sql.NullString
		lastStatus           sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT spent_minutes, block, source, span, status FROM reading_events
		WHERE user_id = ? AND book_id = ? AND device_id = ?
		ORDER BY id DESC LIMIT 1`,
		ev.UserID, ev.BookID, ev.DeviceID).
		Scan(&lastSpent, &lastBlock, &lastSource, &lastSpan, &lastStatus)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	seen := err == nil

	if seen && ev.Span == lastSpan.String && ev.Source == lastSource.String &&
		ev.Status == lastStatus.String &&
		(ev.Spent == nil || (lastSpent.Valid && int64(*ev.Spent) == lastSpent.Int64)) {
		return false, nil
	}

	ev.SpentDelta = 0
	if ev.Spent != nil && seen && lastSpent.Valid {
		if d := int64(*ev.Spent) - lastSpent.Int64; d > 0 {
			ev.SpentDelta = int(d)
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO reading_events
			(user_id, book_id, device_id, at, device_at, status, percent,
			 source, block, span, spent_minutes, spent_delta, remaining_minutes)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.UserID, ev.BookID, ev.DeviceID, FormatTime(ev.At), formatOrEmpty(ev.DeviceAt),
		ev.Status, nullFloat(ev.Percent), ev.Source, nullInt(ev.Block), ev.Span,
		nullInt(ev.Spent), ev.SpentDelta, nullInt(ev.Remaining))
	return err == nil, err
}

func formatOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return FormatTime(t)
}

func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
