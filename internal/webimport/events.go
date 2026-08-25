package webimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fess932/novelkit/job"

	"github.com/fess932/kobibri/internal/store"
)

// What a check turned out to be.
const (
	EventImported = "imported"
	EventChapters = "chapters"
	EventMetadata = "metadata"
	EventError    = "error"
)

// Event is one thing that happened to an imported book.
type Event struct {
	At     string
	Kind   string
	BookID string
	Title  string
	Before int
	After  int
	Detail string
}

// Added is how many chapters arrived, for a chapters event.
func (e Event) Added() int { return e.After - e.Before }

// eventHistory is how many entries one book keeps. A serial checked every few
// hours for years would otherwise grow a row per new chapter forever, and
// nobody reads past the last screenful.
const eventHistory = 200

func (im *Importer) addEvent(ctx context.Context, sourceBookID int64, e Event) {
	w := im.store.Writer()
	if _, err := w.ExecContext(ctx, `
		INSERT INTO web_import_events (source_book_id, at, kind, chapters_before,
		                               chapters_after, detail)
		VALUES (?,?,?,?,?,?)`,
		sourceBookID, store.Now(), e.Kind, e.Before, e.After, e.Detail); err != nil {
		slog.Warn("recording an import event", "err", err)
		return
	}
	if _, err := w.ExecContext(ctx, `
		DELETE FROM web_import_events
		WHERE source_book_id = ? AND id <= (
			SELECT id FROM web_import_events WHERE source_book_id = ?
			ORDER BY id DESC LIMIT 1 OFFSET ?)`,
		sourceBookID, sourceBookID, eventHistory); err != nil {
		slog.Warn("trimming import events", "err", err)
	}
}

// Events lists what has happened to imported books, newest first.
func (im *Importer) Events(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := im.store.Reader().QueryContext(ctx, `
		SELECT e.at, e.kind, COALESCE(sb.book_id, ''), sb.title,
		       e.chapters_before, e.chapters_after, e.detail
		FROM web_import_events e
		JOIN source_books sb ON sb.id = e.source_book_id
		ORDER BY e.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.At, &e.Kind, &e.BookID, &e.Title,
			&e.Before, &e.After, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// buildSignature is everything that decides the bytes of the assembled file.
//
// The chapter list carries each chapter's identity, its heading and whether it
// has been downloaded, so a renamed chapter and a newly arrived one both move
// the signature; the metadata and the cover asset move it because they end up
// inside the file as well.
func buildSignature(st job.State) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, part := range []any{st.Book, st.Cover, st.Assets, st.Chapters} {
		if err := enc.Encode(part); err != nil {
			return ""
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// newlyDone lists the chapters that this run downloaded.
func newlyDone(before, after []job.ChapterState) []job.ChapterState {
	had := make(map[int]bool, len(before))
	for _, ch := range before {
		had[ch.Index] = ch.Done
	}

	var out []job.ChapterState
	for _, ch := range after {
		if ch.Done && !had[ch.Index] {
			out = append(out, ch)
		}
	}
	return out
}

// namesOf is the readable half of a chapters event. Long runs are counted
// rather than listed: a first import can be a thousand chapters.
func namesOf(chapters []job.ChapterState) string {
	const shown = 8

	names := make([]string, 0, shown)
	for _, ch := range chapters[:min(len(chapters), shown)] {
		names = append(names, ch.Title())
	}
	if rest := len(chapters) - len(names); rest > 0 {
		return strings.Join(names, ", ") + fmt.Sprintf(" … +%d", rest)
	}
	return strings.Join(names, ", ")
}
