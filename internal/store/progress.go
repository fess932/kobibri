package store

import (
	"context"
	"encoding/json"
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

// ReadingProgress reads one person's progress through one book.
func ReadingProgress(ctx context.Context, q Querier, userID int64, bookID string) (Progress, error) {
	var status, bookmark, lastModified string
	err := q.QueryRowContext(ctx,
		`SELECT status, bookmark_json, last_modified FROM reading_states
		 WHERE user_id = ? AND book_id = ?`, userID, bookID).
		Scan(&status, &bookmark, &lastModified)
	if err != nil {
		return Progress{}, err
	}
	return Progress{
		Status:   status,
		Percent:  percentOf(bookmark),
		LastRead: lastModified,
	}, nil
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
	var bm struct {
		ProgressPercent              float64 `json:"ProgressPercent"`
		ContentSourceProgressPercent float64 `json:"ContentSourceProgressPercent"`
	}
	if err := json.Unmarshal([]byte(bookmarkJSON), &bm); err != nil {
		return 0
	}
	// ProgressPercent is progress through the current chapter; the one across the
	// whole book is the other. Devices do not always send both.
	if bm.ContentSourceProgressPercent > 0 {
		return bm.ContentSourceProgressPercent
	}
	return bm.ProgressPercent
}
