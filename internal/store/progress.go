package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
)

// Reading statuses, as the device words them.
const (
	ReadReady    = "ReadyToRead"
	ReadReading  = "Reading"
	ReadFinished = "Finished"
)

// Progress is how far through a book someone is.
//
// It comes back from the reader on every sync and is stored as the device sent
// it. Pulling the percentage out for the browser is the only reason to look
// inside the bookmark at all — everything else in there is the device's own
// business, and is handed back untouched.
type Progress struct {
	Status  string
	Percent float64
	// LastRead is when the device last reported a change.
	LastRead string
}

// Started reports whether there is anything worth showing.
func (p Progress) Started() bool {
	return p.Status == ReadReading || p.Status == ReadFinished || p.Percent > 0
}

// Rounded is the percentage as a whole number, never rounded up to 100 before
// the book is actually finished: telling someone they are done when they are not
// is worse than being a percent shy.
func (p Progress) Rounded() int {
	if p.Status == ReadFinished {
		return 100
	}
	n := int(math.Floor(p.Percent))
	if n > 99 {
		n = 99
	}
	if n < 0 {
		n = 0
	}
	return n
}

// percentOf digs the percentage out of a stored bookmark.
//
// A bookmark that will not parse is not an error worth surfacing: the device
// owns that field, it is handed back to the device unchanged, and the worst case
// here is a book shown as unstarted.
func percentOf(bookmarkJSON string) float64 {
	if bookmarkJSON == "" || bookmarkJSON == "null" {
		return 0
	}
	// Pointers because zero and absent are different answers here: a reader one
	// page into a book sends ProgressPercent 0, and treating that as "not sent"
	// falls through to the wrong field.
	var bm struct {
		ProgressPercent              *float64 `json:"ProgressPercent"`
		ContentSourceProgressPercent *float64 `json:"ContentSourceProgressPercent"`
	}
	if err := json.Unmarshal([]byte(bookmarkJSON), &bm); err != nil {
		return 0
	}
	// ProgressPercent is the whole book. ContentSourceProgressPercent is only
	// how far into the current spine file the reader is, which is why it is the
	// fallback and never the preference. See docs/NOTES.md.
	if bm.ProgressPercent != nil {
		return *bm.ProgressPercent
	}
	if bm.ContentSourceProgressPercent != nil {
		return *bm.ContentSourceProgressPercent
	}
	return 0
}

// BookProgress is one person's place in one book, for the book's own page. The
// per-device table beside it says the same thing from each reader's angle; this
// is the answer to "where am I", which is what someone opening the page wants.
func BookProgress(ctx context.Context, q Querier, userID int64, bookID string) (Progress, error) {
	var p Progress
	var bookmark string
	err := q.QueryRowContext(ctx, `
		SELECT status, COALESCE(bookmark_json, ''), COALESCE(last_modified, '')
		FROM reading_states WHERE user_id = ? AND book_id = ?`,
		userID, bookID).Scan(&p.Status, &bookmark, &p.LastRead)
	if errors.Is(err, sql.ErrNoRows) {
		return Progress{}, nil
	}
	if err != nil {
		return Progress{}, err
	}
	p.Percent = percentOf(bookmark)
	return p, nil
}
