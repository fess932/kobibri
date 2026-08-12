package store

import (
	"context"
	"fmt"
)

// A device checks in every few minutes and almost always has nothing to be told.
// Answering that used to cost a whole new snapshot — twenty thousand rows written
// and twenty thousand deleted, for a library of twenty thousand books — every
// time, for every device. On a NAS with an SD card that is not merely slow, it
// wears the card out.
//
// So: a cheap fingerprint of everything a snapshot is built from. If it matches
// the one recorded when the last snapshot was made, nothing can have changed and
// the answer is an empty list without writing anything.
//
// It is computed from the same data rather than maintained as a counter on the
// side. A counter has to be bumped from every place that writes, and the day one
// of those is forgotten a device silently stops receiving updates — the worst
// failure this system has. An aggregate cannot be forgotten.

// Generation identifies the state of everything a device's snapshot depends on.
type Generation string

// LibraryGeneration fingerprints what one device would be told about.
//
// Counts and revision sums together: an edited book moves the sum, an added or
// removed one moves the count, and a book that becomes hidden moves the count
// too. Both would have to move in opposite directions by exactly the same amount
// to hide a change, which takes a coincidence nobody will arrange by accident.
func LibraryGeneration(ctx context.Context, q Querier, userID, deviceID int64) (Generation, error) {
	var (
		books, bookRevs           int64
		states, stateRevs         int64
		tags, tagRevs             int64
		tombstones, enabledSource int64
	)

	err := q.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM books WHERE merged_into IS NULL AND syncable = 1),
			(SELECT COALESCE(sum(metadata_rev), 0) FROM books
			  WHERE merged_into IS NULL AND syncable = 1),
			(SELECT count(*) FROM reading_states WHERE user_id = ?),
			(SELECT COALESCE(sum(rev), 0) FROM reading_states WHERE user_id = ?),
			(SELECT count(*) FROM tags WHERE user_id = ?),
			(SELECT COALESCE(sum(rev), 0) FROM tags WHERE user_id = ?),
			(SELECT count(*) FROM device_tombstones WHERE device_id = ?),
			(SELECT count(*) FROM sources WHERE enabled = 1)`,
		userID, userID, userID, userID, deviceID).
		Scan(&books, &bookRevs, &states, &stateRevs, &tags, &tagRevs,
			&tombstones, &enabledSource)
	if err != nil {
		return "", fmt.Errorf("library generation: %w", err)
	}

	return Generation(fmt.Sprintf("1:%d.%d:%d.%d:%d.%d:%d:%d",
		books, bookRevs, states, stateRevs, tags, tagRevs,
		tombstones, enabledSource)), nil
}
