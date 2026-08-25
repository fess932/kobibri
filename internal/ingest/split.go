package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fess932/kobibri/internal/store"
)

var (
	// ErrNothingToSplit is returned when a source row is the only thing holding
	// up its book, so moving it away would leave an empty one behind.
	ErrNothingToSplit = errors.New("this is the only copy of the book; there is nothing to split it from")
	// ErrAlreadySplit is returned when a row was already pinned somewhere.
	ErrAlreadySplit = errors.New("this copy was already split off")
)

// Duplicate is a book whose copies were joined on the weakest evidence there is.
type Duplicate struct {
	Book         *store.Book
	Contributors []store.Contributor
}

// SuspectMerges lists books that were merged on title and author alone.
//
// That key is the only one every book has, and it is the only one that can be
// wrong: two different books really can share a title and an author — different
// translations, two anthologies called "Selected Poems", a reissue with new
// content. A merge backed by a Calibre uuid or an ISBN is evidence; this one is
// a guess, so it is the only kind worth putting in front of a person.
//
// Nothing here is a fault by itself. Most of these are correct — the same book
// in two libraries, neither carrying an identifier. The report exists so that
// the rare wrong one can be found at all.
func SuspectMerges(ctx context.Context, q store.Querier) ([]Duplicate, error) {
	ids, err := booksWithSeveralCopies(ctx, q)
	if err != nil {
		return nil, err
	}

	var out []Duplicate
	for _, id := range ids {
		book, err := store.GetBook(ctx, q, id)
		if err != nil {
			return nil, err
		}
		contributors, err := store.Contributors(ctx, q, book)
		if err != nil {
			return nil, err
		}
		if joinedByAnIdentifier(contributors) {
			continue
		}
		out = append(out, Duplicate{Book: book, Contributors: contributors})
	}
	return out, nil
}

func booksWithSeveralCopies(ctx context.Context, q store.Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT b.id
		FROM books b
		JOIN source_books sb ON sb.book_id = b.id AND sb.missing = 0
		JOIN sources s ON s.id = sb.source_id AND s.enabled = 1
		WHERE b.merged_into IS NULL
		GROUP BY b.id
		HAVING count(*) > 1
		ORDER BY b.title`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// joinedByAnIdentifier reports whether any two copies agree on something
// stronger than their title: a Calibre uuid or an ISBN. One shared identifier
// anywhere in the group is enough — that is what made the merge more than a
// guess.
func joinedByAnIdentifier(contributors []store.Contributor) bool {
	seen := map[string]bool{}
	for _, c := range contributors {
		if c.Missing {
			continue
		}
		for _, key := range []string{c.CalibreUUID, c.ISBN13} {
			if key == "" {
				continue
			}
			if seen[key] {
				return true
			}
			seen[key] = true
		}
	}
	return false
}

// Split moves one copy out of the book it was merged into, onto a book of its
// own.
//
// The book that stays keeps its id, because that is the id every reader already
// holds. The copy that leaves becomes a new book, and arrives on a device as a
// new book — there is no way around that, and it is the point: they were two
// books all along.
//
// The row is pinned so the next scan cannot undo this. Identity keys are exactly
// what joined the two in the first place, and they will still match.
func Split(ctx context.Context, st *store.Store, sourceBookID int64) (string, error) {
	var newBookID string

	err := st.Tx(ctx, func(tx *sql.Tx) error {
		var oldBookID sql.NullString
		var pinned sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT book_id, pinned_book_id FROM source_books WHERE id = ?`,
			sourceBookID).Scan(&oldBookID, &pinned)
		if err != nil {
			return err
		}
		if pinned.Valid && pinned.String != "" {
			return ErrAlreadySplit
		}
		if !oldBookID.Valid || oldBookID.String == "" {
			return ErrNothingToSplit
		}

		resolvedOld, err := store.ResolveBookID(ctx, tx, oldBookID.String)
		if err != nil {
			return err
		}

		var siblings int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM source_books WHERE book_id = ? AND id <> ?`,
			oldBookID.String, sourceBookID).Scan(&siblings); err != nil {
			return err
		}
		if siblings == 0 {
			return ErrNothingToSplit
		}

		if newBookID, err = store.CreateBook(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE source_books SET book_id = ?, pinned_book_id = ? WHERE id = ?`,
			newBookID, newBookID, sourceBookID); err != nil {
			return err
		}

		// Both sides changed: one lost a contributor, the other has one for the
		// first time.
		if err := Resolve(ctx, tx, resolvedOld); err != nil {
			return err
		}
		if err := Resolve(ctx, tx, newBookID); err != nil {
			return err
		}

		slog.Info("split a copy onto its own book",
			"source_book", sourceBookID, "from", resolvedOld, "to", newBookID)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("split source book %d: %w", sourceBookID, err)
	}
	return newBookID, nil
}

// Rejoin undoes a split, putting a pinned copy back with the book its identity
// keys point at.
func Rejoin(ctx context.Context, st *store.Store, sourceBookID int64) error {
	return st.Tx(ctx, func(tx *sql.Tx) error {
		sb, err := store.GetSourceBook(ctx, tx, sourceBookID)
		if err != nil {
			return err
		}
		if sb.PinnedBookID == "" {
			return nil // never split; nothing to undo
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE source_books SET pinned_book_id = NULL WHERE id = ?`, sourceBookID); err != nil {
			return err
		}
		sb.PinnedBookID = ""

		was := sb.BookID
		bookID, err := Attach(ctx, tx, sb)
		if err != nil {
			return err
		}
		if err := store.SetSourceBookBookID(ctx, tx, sourceBookID, bookID); err != nil {
			return err
		}

		if was != "" && was != bookID {
			if resolved, err := store.ResolveBookID(ctx, tx, was); err == nil {
				if err := Resolve(ctx, tx, resolved); err != nil {
					return err
				}
			}
		}
		return Resolve(ctx, tx, bookID)
	})
}
