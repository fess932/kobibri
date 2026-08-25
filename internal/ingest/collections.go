package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"

	"github.com/fess932/kobibri/internal/store"
)

// How the library's own organisation is turned into shelves on a reader.
//
// Off by default on purpose: a library with two hundred tags would put two
// hundred shelves on someone's Kobo without being asked, which is rude.
const (
	CollectionsOff    = "off"
	CollectionsTags   = "tags"
	CollectionsSeries = "series"
	CollectionsBoth   = "both"
)

// CollectionsModeKey is where the setting lives.
const CollectionsModeKey = "collections:mode"

// LooseShelfKey names the shelf that gathers books in no series at all.
//
// Without it a reader browsing by collection sees its series and then nothing
// for everything else, so half the library is reachable only through the
// full list. Empty is off, which is the default: an unasked-for shelf on
// someone's device is the same rudeness as two hundred of them.
const LooseShelfKey = "collections:loose"

// CollectionsMode reads the setting, defaulting to off.
func CollectionsMode(ctx context.Context, q store.Querier) string {
	mode, err := store.GetKV(ctx, q, CollectionsModeKey)
	if err != nil || mode == "" {
		return CollectionsOff
	}
	return mode
}

// LooseShelf reads the name for books in no series, or empty for off.
func LooseShelf(ctx context.Context, q store.Querier) string {
	name, err := store.GetKV(ctx, q, LooseShelfKey)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

func SetLooseShelf(ctx context.Context, x store.Execer, name string) error {
	return store.SetKV(ctx, x, LooseShelfKey, strings.TrimSpace(name))
}

func SetCollectionsMode(ctx context.Context, x store.Execer, mode string) error {
	switch mode {
	case CollectionsOff, CollectionsTags, CollectionsSeries, CollectionsBoth:
	default:
		mode = CollectionsOff
	}
	return store.SetKV(ctx, x, CollectionsModeKey, mode)
}

// RebuildCollections turns the library's own organisation into collections, per
// user: its tags, its series, and any custom column that was chosen.
//
// Collections are per-user because reading them is: two people sharing a server
// see the books they are allowed to see, and their shelves follow. The work is
// idempotent — running it after every scan is the intended use — and a
// collection is only touched when its membership actually differs, so a device
// is not told about a shelf that has not changed.
//
// Note that mode `off` is not a shortcut out: a chosen custom column still
// becomes a shelf, because choosing it was already the decision. Whether there
// is anything to do at all is settled by the caller.
func RebuildCollections(ctx context.Context, x store.Execer, mode string) error {
	users, err := userIDs(ctx, x)
	if err != nil {
		return err
	}

	loose := LooseShelf(ctx, x)

	for _, userID := range users {
		wanted, err := derive(ctx, x, userID, mode, loose)
		if err != nil {
			return err
		}
		if err := applyCollections(ctx, x, userID, wanted); err != nil {
			return err
		}
	}
	return nil
}

// derive works out which books belong on which shelf, for one user.
func derive(ctx context.Context, x store.Execer, userID int64, mode, loose string) (map[string][]string, error) {
	rows, err := x.QueryContext(ctx, `
		SELECT b.id, b.series_name, COALESCE(sb.tags_json, '[]'), b.primary_source_book_id
		FROM books b
		LEFT JOIN source_books sb ON sb.id = b.primary_source_book_id
		WHERE b.merged_into IS NULL AND b.syncable = 1
		  AND EXISTS (SELECT 1 FROM source_books s2
		              JOIN sources src ON src.id = s2.source_id
		              LEFT JOIN source_acl a ON a.source_id = src.id AND a.user_id = ?
		              WHERE s2.book_id = b.id AND s2.missing = 0 AND src.enabled = 1
		                AND (src.share_all = 1 OR a.user_id IS NOT NULL))`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type row struct {
		bookID, series, tagsJSON string
		primary                  sql.NullInt64
	}
	var books []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.bookID, &r.series, &r.tagsJSON, &r.primary); err != nil {
			return nil, err
		}
		books = append(books, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	wanted := map[string][]string{}
	for _, r := range books {
		bookID, series, tagsJSON := r.bookID, r.series, r.tagsJSON

		if mode == CollectionsSeries || mode == CollectionsBoth {
			switch {
			case series != "":
				wanted[series] = append(wanted[series], bookID)
			case loose != "":
				// The catch-all is a shelf and nothing more. Writing a series
				// name into the book instead would put "Standalones #1" under
				// its title on the device, move its serving hash, and re-announce
				// most of the library over one setting.
				wanted[loose] = append(wanted[loose], bookID)
			}
		}
		if mode == CollectionsTags || mode == CollectionsBoth {
			var tags []string
			if err := json.Unmarshal([]byte(tagsJSON), &tags); err == nil {
				for _, tag := range tags {
					if tag = strings.TrimSpace(tag); tag != "" {
						wanted[tag] = append(wanted[tag], bookID)
					}
				}
			}
		}

		// A library's own columns become shelves whenever they were chosen,
		// whatever the tags-and-series setting says: picking a column is already
		// the decision.
		if r.primary.Valid {
			columns, err := store.SourceBookColumns(ctx, x, r.primary.Int64)
			if err != nil {
				return nil, err
			}
			for _, values := range columns {
				for _, value := range values {
					if value = strings.TrimSpace(value); value != "" {
						wanted[value] = append(wanted[value], bookID)
					}
				}
			}
		}
	}
	return wanted, nil
}

// applyCollections makes the stored collections match what was derived.
func applyCollections(ctx context.Context, x store.Execer, userID int64, wanted map[string][]string) error {
	existing, err := calibreCollections(ctx, x, userID)
	if err != nil {
		return err
	}

	for name, books := range wanted {
		sort.Strings(books)

		id, ok := existing[name]
		if !ok {
			// A shelf the reader deleted stays deleted. Recreating it on the
			// next scan would be an argument nobody can win.
			deleted, err := deletedByDevice(ctx, x, userID, name)
			if err != nil {
				return err
			}
			if deleted {
				continue
			}

			if id, err = store.CreateTag(ctx, x, userID, name, store.TagOriginCalibre); err != nil {
				return err
			}
			// CreateTag revives a name it has seen before without touching its
			// origin, so claim it back explicitly.
			if err := setOrigin(ctx, x, id, store.TagOriginCalibre); err != nil {
				return err
			}
		}

		current, err := store.TagBookIDs(ctx, x, id)
		if err != nil {
			return err
		}
		if sameMembers(current, books) {
			continue
		}

		if _, err := x.ExecContext(ctx, `DELETE FROM tag_books WHERE tag_id = ?`, id); err != nil {
			return err
		}
		if err := store.AddTagItems(ctx, x, id, books); err != nil {
			return err
		}
		slog.Debug("collection membership changed", "user", userID, "name", name,
			"books", len(books))
	}

	// A tag that no longer exists in the library takes its shelf with it. The
	// soft delete is what lets the diff announce it; dropping the row would
	// leave the shelf on every device forever.
	for name, id := range existing {
		if _, still := wanted[name]; still {
			continue
		}
		if _, err := x.ExecContext(ctx, `DELETE FROM tag_books WHERE tag_id = ?`, id); err != nil {
			return err
		}
		if err := store.DeleteTag(ctx, x, id); err != nil {
			return err
		}
		// Mark it as ours rather than the library's, so a tag that comes back
		// to Calibre is rebuilt while one a reader threw away stays gone.
		if err := setOrigin(ctx, x, id, store.TagOriginServer); err != nil {
			return err
		}
		slog.Debug("collection gone from the library", "user", userID, "name", name)
	}
	return nil
}

// calibreCollections lists the live shelves this made, so it does not touch the
// ones a reader created for themselves.
func calibreCollections(ctx context.Context, q store.Querier, userID int64) (map[string]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, name FROM tags WHERE user_id = ? AND origin = ? AND deleted_at IS NULL`,
		userID, store.TagOriginCalibre)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[name] = id
	}
	return out, rows.Err()
}

// deletedByDevice reports whether a reader threw this shelf away while the
// library still had the tag. Deletions this package made itself are marked with
// a different origin, so they do not count.
func deletedByDevice(ctx context.Context, q store.Querier, userID int64, name string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM tags
		 WHERE user_id = ? AND name = ? AND origin = ? AND deleted_at IS NOT NULL`,
		userID, name, store.TagOriginCalibre).Scan(&n)
	return n > 0, err
}

func setOrigin(ctx context.Context, x store.Execer, tagID, origin string) error {
	_, err := x.ExecContext(ctx, `UPDATE tags SET origin = ? WHERE id = ?`, origin, tagID)
	return err
}

func sameMembers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sorted := append([]string(nil), a...)
	sort.Strings(sorted)
	for i := range sorted {
		if sorted[i] != b[i] {
			return false
		}
	}
	return true
}

func userIDs(ctx context.Context, q store.Querier) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM users WHERE disabled = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
