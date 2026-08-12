package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/fess932/kobibri/internal/calibre"
	"github.com/fess932/kobibri/internal/store"
)

// A library's own custom columns — "Shelf", "Read status", "Mood" — can be made
// into collections on a reader.
//
// They are deliberately not mapped onto the device's Genre field. That field
// holds a category uuid from the store's own taxonomy, not free text, so a
// library's own words would be ignored there. As shelves they do exactly what
// their owner meant them to do.

// knownColumnsKey remembers what a library offers, so the settings page can list
// the columns without opening the library on every page load.
func knownColumnsKey(sourceID int64) string {
	return fmt.Sprintf("source:%d:custom_columns", sourceID)
}

// shelfColumnsKey holds the labels chosen for shelves.
func shelfColumnsKey(sourceID int64) string {
	return fmt.Sprintf("source:%d:shelf_columns", sourceID)
}

// KnownColumns lists the custom columns the last scan found in a library, in the
// order Calibre defines them.
func KnownColumns(ctx context.Context, q store.Querier, sourceID int64) []calibre.CustomColumn {
	raw, err := store.GetKV(ctx, q, knownColumnsKey(sourceID))
	if err != nil || raw == "" {
		return nil
	}
	var out []calibre.CustomColumn
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// ShelfColumns lists the labels a source was told to build shelves from.
func ShelfColumns(ctx context.Context, q store.Querier, sourceID int64) []string {
	raw, err := store.GetKV(ctx, q, shelfColumnsKey(sourceID))
	if err != nil || raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// SetShelfColumns records which columns of a library become shelves. Labels the
// library does not have are dropped rather than stored and silently ignored.
func SetShelfColumns(ctx context.Context, x store.Execer, sourceID int64, labels []string) error {
	known := map[string]bool{}
	for _, col := range KnownColumns(ctx, x, sourceID) {
		if col.UsableForShelves() {
			known[col.Label] = true
		}
	}

	kept := make([]string, 0, len(labels))
	for _, label := range labels {
		if known[label] {
			kept = append(kept, label)
		}
	}

	encoded, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return store.SetKV(ctx, x, shelfColumnsKey(sourceID), string(encoded))
}

// AnyShelfColumns reports whether any source has columns configured, which is
// what makes a rebuild worth running even with tags and series switched off.
func AnyShelfColumns(ctx context.Context, q store.Querier) bool {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM kv WHERE k LIKE 'source:%:shelf_columns' AND v <> '[]' AND v <> ''`).
		Scan(&n)
	return err == nil && n > 0
}

// readColumnsKey remembers which columns the last scan actually read, so a
// change in the choice can be noticed.
func readColumnsKey(sourceID int64) string {
	return fmt.Sprintf("source:%d:columns_read", sourceID)
}

// rememberColumns records what a library offers and returns the ones this source
// was asked to read, along with whether that choice has changed since the last
// scan.
//
// The second return value matters: a scan normally reads only the books Calibre
// says changed, so choosing a column afterwards would leave every existing book
// without a value for it. A changed choice forces one full read.
func rememberColumns(ctx context.Context, x store.Execer, sourceID int64,
	available []calibre.CustomColumn) (columns []calibre.CustomColumn, changed bool, err error) {

	encoded, err := json.Marshal(available)
	if err != nil {
		return nil, false, err
	}
	if err := store.SetKV(ctx, x, knownColumnsKey(sourceID), string(encoded)); err != nil {
		return nil, false, err
	}

	wanted := map[string]bool{}
	for _, label := range ShelfColumns(ctx, x, sourceID) {
		wanted[label] = true
	}
	for _, col := range available {
		if wanted[col.Label] && col.UsableForShelves() {
			columns = append(columns, col)
		}
	}

	labels := make([]string, 0, len(columns))
	for _, col := range columns {
		labels = append(labels, col.Label)
	}
	sort.Strings(labels)
	current, err := json.Marshal(labels)
	if err != nil {
		return nil, false, err
	}

	previous, _ := store.GetKV(ctx, x, readColumnsKey(sourceID))
	changed = previous != string(current)
	if changed {
		if err := store.SetKV(ctx, x, readColumnsKey(sourceID), string(current)); err != nil {
			return nil, false, err
		}
	}
	return columns, changed, nil
}
