package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TextIndex says how many words stand before every position a device can
// report, so two reported positions subtract into a number of words read.
type TextIndex struct {
	Fingerprint string
	Words       int
	Spanned     bool
	Docs        []TextDoc
	Blocks      []TextBlock
}

// TextDoc is one content document of the book, in reading order.
type TextDoc struct {
	Seq    int
	Source string
	Title  string
	Words  int
	Before int
}

// TextBlock is one koboSpan block: a paragraph, near enough.
type TextBlock struct {
	Source string
	Block  int
	Before int
}

var ErrNoTextIndex = errors.New("book has no word index")

// TextIndexFingerprint is what the stored index was built from, or empty.
func TextIndexFingerprint(ctx context.Context, q Querier, bookID string) (string, error) {
	var fp string
	err := q.QueryRowContext(ctx,
		`SELECT fingerprint FROM book_text_index WHERE book_id = ?`, bookID).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return fp, err
}

// SaveTextIndex replaces a book's index wholesale. A book that gained chapters
// has different offsets, and half of each is worse than either.
func (s *Store) SaveTextIndex(ctx context.Context, bookID string, ix *TextIndex) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM book_text_blocks WHERE book_id = ?`,
			`DELETE FROM book_text_docs WHERE book_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, bookID); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO book_text_index (book_id, fingerprint, words, documents, spanned, built_at)
			VALUES (?,?,?,?,?,?)
			ON CONFLICT(book_id) DO UPDATE SET
				fingerprint = excluded.fingerprint, words = excluded.words,
				documents = excluded.documents, spanned = excluded.spanned,
				built_at = excluded.built_at`,
			bookID, ix.Fingerprint, ix.Words, len(ix.Docs), ix.Spanned,
			FormatTime(time.Now())); err != nil {
			return err
		}

		docs, err := tx.PrepareContext(ctx, `
			INSERT INTO book_text_docs (book_id, source, seq, title, words, before)
			VALUES (?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer docs.Close()
		for _, d := range ix.Docs {
			if _, err := docs.ExecContext(ctx, bookID, d.Source, d.Seq, d.Title, d.Words, d.Before); err != nil {
				return err
			}
		}

		blocks, err := tx.PrepareContext(ctx, `
			INSERT INTO book_text_blocks (book_id, source, block, before) VALUES (?,?,?,?)`)
		if err != nil {
			return err
		}
		defer blocks.Close()
		for _, b := range ix.Blocks {
			if _, err := blocks.ExecContext(ctx, bookID, b.Source, b.Block, b.Before); err != nil {
				return err
			}
		}
		return nil
	})
}

// BookWords is the length of a book in words, or zero when it has no index.
func BookWords(ctx context.Context, q Querier, bookID string) (int, error) {
	var words int
	err := q.QueryRowContext(ctx,
		`SELECT words FROM book_text_index WHERE book_id = ?`, bookID).Scan(&words)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return words, err
}

// BooksNeedingTextIndex lists books someone has read that have no index yet,
// newest activity first. A book nobody has opened is not worth opening.
func BooksNeedingTextIndex(ctx context.Context, q Querier, limit int) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT DISTINCT rs.book_id FROM reading_states rs
		JOIN books b ON b.id = rs.book_id AND b.merged_into IS NULL
		LEFT JOIN book_text_index ti ON ti.book_id = rs.book_id
		WHERE ti.book_id IS NULL
		ORDER BY rs.last_modified DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
