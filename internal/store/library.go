package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// joinUnderRoot resolves a library-relative path, refusing anything that
// escapes its root. Paths come from a database we do not control.
func joinUnderRoot(root, rel string) (string, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	clean := filepath.Clean(root)
	if full != clean && !strings.HasPrefix(full, clean+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes its library root", rel)
	}
	return full, nil
}

// Stats is the summary the dashboard shows.
type Stats struct {
	Sources        int
	SourcesOK      int
	SourcesBroken  int
	Books          int
	Syncable       int
	Unavailable    int
	Hidden         int
	Devices        int
	Users          int
	Collections    int
	KepubCached    int
	KepubFailed    int
	CoversCached   int
	KepubCacheSize int64
	CoverCacheSize int64
}

func GetStats(ctx context.Context, q Querier) (*Stats, error) {
	var s Stats
	scalars := []struct {
		query string
		dst   any
	}{
		{`SELECT count(*) FROM sources`, &s.Sources},
		{`SELECT count(*) FROM sources WHERE enabled = 1 AND last_status = 'ok'`, &s.SourcesOK},
		{`SELECT count(*) FROM sources WHERE enabled = 1 AND last_status IN ('unreachable','error','suspicious')`, &s.SourcesBroken},
		{`SELECT count(*) FROM books WHERE merged_into IS NULL`, &s.Books},
		{`SELECT count(*) FROM books WHERE merged_into IS NULL AND syncable = 1`, &s.Syncable},
		{`SELECT count(*) FROM books WHERE merged_into IS NULL AND available = 0`, &s.Unavailable},
		{`SELECT count(*) FROM books WHERE merged_into IS NULL AND hidden = 1`, &s.Hidden},
		{`SELECT count(*) FROM devices`, &s.Devices},
		{`SELECT count(*) FROM users`, &s.Users},
		{`SELECT count(*) FROM tags WHERE deleted_at IS NULL`, &s.Collections},
		{`SELECT count(*) FROM kepub_cache`, &s.KepubCached},
		{`SELECT count(*) FROM kepub_failures`, &s.KepubFailed},
		{`SELECT count(*) FROM cover_cache`, &s.CoversCached},
		{`SELECT COALESCE(sum(size), 0) FROM kepub_cache`, &s.KepubCacheSize},
		{`SELECT COALESCE(sum(size), 0) FROM cover_cache`, &s.CoverCacheSize},
	}
	for _, sc := range scalars {
		if err := q.QueryRowContext(ctx, sc.query).Scan(sc.dst); err != nil {
			return nil, err
		}
	}
	return &s, nil
}

// LibraryRow is one line of the library listing.
type LibraryRow struct {
	ID           string
	Title        string
	Authors      string
	SeriesName   string
	SeriesIndex  sql.NullFloat64
	Format       string
	Available    bool
	Hidden       bool
	Syncable     bool
	CoverImageID string
	SourceCount  int
	Converted    bool
}

// LibraryQuery filters the library listing.
type LibraryQuery struct {
	Search   string
	SourceID int64
	Only     string // "" | syncable | unavailable | hidden | unconverted
	// UserID limits the listing to books this person is allowed to see, by the
	// same rule the sync snapshot uses. Zero means no restriction, which is what
	// the administrative listing wants.
	UserID int64
	// Sort is SortTitle or SortNewest.
	Sort   string
	Limit  int
	Offset int
}

// ListLibrary returns a page of the library and the total matching count.
func ListLibrary(ctx context.Context, q Querier, f LibraryQuery) ([]LibraryRow, int, error) {
	where := []string{"b.merged_into IS NULL"}
	var args []any

	if s := strings.TrimSpace(f.Search); s != "" {
		where = append(where, "(b.title LIKE ? OR b.authors_json LIKE ? OR b.series_name LIKE ?)")
		like := "%" + s + "%"
		args = append(args, like, like, like)
	}
	if f.SourceID > 0 {
		where = append(where, `EXISTS (SELECT 1 FROM source_books sb
			WHERE sb.book_id = b.id AND sb.source_id = ? AND sb.missing = 0)`)
		args = append(args, f.SourceID)
	}
	if f.UserID > 0 {
		where = append(where, `EXISTS (SELECT 1 FROM source_books sb
			JOIN sources s ON s.id = sb.source_id
			LEFT JOIN source_acl a ON a.source_id = s.id AND a.user_id = ?
			WHERE sb.book_id = b.id AND sb.missing = 0 AND s.enabled = 1
			  AND (s.share_all = 1 OR a.user_id IS NOT NULL))`)
		args = append(args, f.UserID)
	}
	switch f.Only {
	case "syncable":
		where = append(where, "b.syncable = 1")
	case "unavailable":
		where = append(where, "b.available = 0")
	case "hidden":
		where = append(where, "b.hidden = 1")
	case "unconverted":
		where = append(where, `b.download_format = 'KEPUB'
			AND NOT EXISTS (SELECT 1 FROM kepub_cache c WHERE c.book_id = b.id)`)
	}
	clause := " WHERE " + strings.Join(where, " AND ")

	var total int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM books b`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.QueryContext(ctx, `
		SELECT b.id, b.title, b.authors_json, b.series_name, b.series_index,
		       b.download_format, b.available, b.hidden, b.syncable, b.cover_image_id,
		       (SELECT count(*) FROM source_books sb WHERE sb.book_id = b.id AND sb.missing = 0),
		       EXISTS (SELECT 1 FROM kepub_cache c WHERE c.book_id = b.id)
		FROM books b`+clause+`
		ORDER BY `+libraryOrder(f.Sort)+`
		LIMIT ? OFFSET ?`, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []LibraryRow
	for rows.Next() {
		var r LibraryRow
		if err := rows.Scan(&r.ID, &r.Title, &r.Authors, &r.SeriesName, &r.SeriesIndex,
			&r.Format, &r.Available, &r.Hidden, &r.Syncable, &r.CoverImageID,
			&r.SourceCount, &r.Converted); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// Orderings for a library listing.
const (
	SortTitle  = "title"
	SortNewest = "added"
)

// libraryOrder is a fixed set of orderings rather than anything caller-supplied:
// it is interpolated into the query, so it must never carry input. An unknown
// value falls back to title rather than failing — it can only come from a URL
// someone typed.
func libraryOrder(sort string) string {
	if sort == SortNewest {
		return "b.created_at DESC, b.id DESC"
	}
	return "b.sort_title, b.title, b.id"
}

// Contributor is one source row behind a canonical book, as the book detail
// page shows it: which source it came from and whether it won.
type Contributor struct {
	SourceBookID int64
	SourceID     int64
	SourceName   string
	Priority     int
	CalibreID    int64
	CalibreUUID  string
	ISBN13       string
	Title        string
	Missing      bool
	HasCover     bool
	// Pinned marks a copy someone split off a wrong merge, which a later scan
	// must not undo.
	Pinned       bool
	IsWinner     bool
	IsCoverOwner bool
	Formats      []SourceBookFile
}

// Contributors lists every source row attached to a book, live or missing, so
// the UI can explain why the merged record looks the way it does.
func Contributors(ctx context.Context, q Querier, book *Book) ([]Contributor, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT sb.id, sb.source_id, s.name, s.priority, sb.calibre_id, sb.calibre_uuid,
		       sb.isbn13, sb.title, sb.missing, sb.cover_rel_path <> '',
		       sb.pinned_book_id IS NOT NULL
		FROM source_books sb
		JOIN sources s ON s.id = sb.source_id
		WHERE sb.book_id = ?
		ORDER BY sb.missing ASC, s.priority ASC, s.id ASC, sb.calibre_id ASC`, book.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Contributor
	for rows.Next() {
		var c Contributor
		if err := rows.Scan(&c.SourceBookID, &c.SourceID, &c.SourceName, &c.Priority,
			&c.CalibreID, &c.CalibreUUID, &c.ISBN13, &c.Title, &c.Missing, &c.HasCover,
			&c.Pinned); err != nil {
			return nil, err
		}
		c.IsWinner = book.PrimarySourceBookID.Valid && book.PrimarySourceBookID.Int64 == c.SourceBookID
		c.IsCoverOwner = book.CoverSourceBookID.Valid && book.CoverSourceBookID.Int64 == c.SourceBookID
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		files, err := sourceBookFiles(ctx, q, out[i].SourceBookID)
		if err != nil {
			return nil, err
		}
		out[i].Formats = files
	}
	return out, nil
}

// BookFilePath returns the on-disk path of one format of a book, along with the
// name it should be downloaded as.
func BookFilePath(ctx context.Context, q Querier, book *Book, format string) (path string, err error) {
	var libraryPath, relPath string
	err = q.QueryRowContext(ctx, `
		SELECT s.library_path, f.rel_path
		FROM source_book_files f
		JOIN source_books sb ON sb.id = f.source_book_id
		JOIN sources s ON s.id = sb.source_id
		WHERE f.source_book_id = ? AND f.format = ? AND f.present = 1
		LIMIT 1`, book.PrimarySourceBookID.Int64, strings.ToUpper(format)).
		Scan(&libraryPath, &relPath)
	if err != nil {
		return "", err
	}
	return joinUnderRoot(libraryPath, relPath)
}

// BookCoverPath returns the on-disk path of a book's cover.
func BookCoverPath(ctx context.Context, q Querier, bookID string) (string, error) {
	var libraryPath, relPath string
	err := q.QueryRowContext(ctx, `
		SELECT s.library_path, sb.cover_rel_path
		FROM books b
		JOIN source_books sb ON sb.id = b.cover_source_book_id
		JOIN sources s ON s.id = sb.source_id
		WHERE b.id = ? AND sb.cover_rel_path <> ''`, bookID).Scan(&libraryPath, &relPath)
	if err != nil {
		return "", err
	}
	return joinUnderRoot(libraryPath, relPath)
}

// DeviceBookState says how one device stands with one book, for the detail page.
type DeviceBookState struct {
	DeviceID     int64
	DeviceName   string
	InSnapshot   bool
	Tombstoned   bool
	LastSyncAt   string
	ReadingState string
}

func BookDeviceStates(ctx context.Context, q Querier, bookID string) ([]DeviceBookState, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT d.id,
		       CASE WHEN d.model <> '' THEN d.model ELSE 'device ' || d.id END,
		       COALESCE(d.last_sync_at, ''),
		       EXISTS (SELECT 1 FROM device_tombstones t WHERE t.device_id = d.id AND t.book_id = ?),
		       EXISTS (SELECT 1 FROM sync_points sp
		               JOIN sync_point_books spb ON spb.sync_point_id = sp.id
		               WHERE sp.device_id = d.id AND sp.state = 'completed' AND spb.book_id = ?),
		       COALESCE((SELECT status FROM reading_states rs
		                 WHERE rs.user_id = d.user_id AND rs.book_id = ?), '')
		FROM devices d ORDER BY d.last_seen_at DESC`, bookID, bookID, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeviceBookState
	for rows.Next() {
		var s DeviceBookState
		if err := rows.Scan(&s.DeviceID, &s.DeviceName, &s.LastSyncAt,
			&s.Tombstoned, &s.InSnapshot, &s.ReadingState); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ScanRun is one entry of a source's history.
type ScanRun struct {
	ID         int64
	StartedAt  string
	FinishedAt string
	Status     string
	Error      string
	Seen       int
	Added      int
	Updated    int
	Vanished   int
}

func RecentScanRuns(ctx context.Context, q Querier, sourceID int64, limit int) ([]ScanRun, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, started_at, COALESCE(finished_at, ''), status, error, seen, added, updated, vanished
		FROM scan_runs WHERE source_id = ? ORDER BY started_at DESC LIMIT ?`, sourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScanRun
	for rows.Next() {
		var r ScanRun
		if err := rows.Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.Status, &r.Error,
			&r.Seen, &r.Added, &r.Updated, &r.Vanished); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TombstoneEntry pairs a tombstoned book id with its title, so the devices page
// can show what was deleted rather than a list of uuids.
type TombstoneEntry struct {
	BookID    string
	Title     string
	CreatedAt string
}

func DeviceTombstones(ctx context.Context, q Querier, deviceID int64) ([]TombstoneEntry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT t.book_id, COALESCE(b.title, ''), t.created_at
		FROM device_tombstones t
		LEFT JOIN books b ON b.id = t.book_id
		WHERE t.device_id = ? ORDER BY t.created_at DESC`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TombstoneEntry
	for rows.Next() {
		var e TombstoneEntry
		if err := rows.Scan(&e.BookID, &e.Title, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListAllDevices returns every device with the name of its owner.
type DeviceRow struct {
	Device
	UserName     string
	TombstoneN   int
	SnapshotN    int
	TokenHint    string
	TokenLabel   string
	TokenRevoked bool
}

func ListAllDevices(ctx context.Context, q Querier) ([]DeviceRow, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT d.id, d.user_id, d.token_hash, d.kobo_device_id, d.model, d.serial,
		       d.firmware, d.user_agent, d.first_seen_at, d.last_seen_at,
		       COALESCE(d.last_sync_at, ''), d.last_sync_status,
		       u.name, t.token_hint, t.label, t.revoked_at IS NOT NULL,
		       (SELECT count(*) FROM device_tombstones ts WHERE ts.device_id = d.id),
		       (SELECT count(*) FROM sync_points sp WHERE sp.device_id = d.id)
		FROM devices d
		JOIN users u ON u.id = d.user_id
		LEFT JOIN api_tokens t ON t.token_hash = d.token_hash
		ORDER BY d.last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeviceRow
	for rows.Next() {
		var r DeviceRow
		var hint, label sql.NullString
		var revoked sql.NullBool
		if err := rows.Scan(&r.ID, &r.UserID, &r.TokenHash, &r.KoboDeviceID, &r.Model,
			&r.Serial, &r.Firmware, &r.UserAgent, &r.FirstSeenAt, &r.LastSeenAt,
			&r.LastSyncAt, &r.LastSyncStatus, &r.UserName, &hint, &label, &revoked,
			&r.TombstoneN, &r.SnapshotN); err != nil {
			return nil, err
		}
		r.TokenHint, r.TokenLabel, r.TokenRevoked = hint.String, label.String, revoked.Bool
		out = append(out, r)
	}
	return out, rows.Err()
}

func ListUsers(ctx context.Context, q Querier) ([]*User, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, name, password_hash, is_admin, disabled, created_at FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.PasswordHash, &u.IsAdmin, &u.Disabled, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

func SetUserPassword(ctx context.Context, x Execer, userID int64, hash string) error {
	_, err := x.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	return err
}

func DeleteUser(ctx context.Context, x Execer, userID int64) error {
	_, err := x.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	return err
}

func DeleteSource(ctx context.Context, x Execer, sourceID int64) error {
	// The canonical books survive: their ids are what devices hold, and
	// removing a source must never make a book unresolvable.
	_, err := x.ExecContext(ctx, `DELETE FROM sources WHERE id = ?`, sourceID)
	return err
}

func UpdateSource(ctx context.Context, x Execer, s *Source) error {
	_, err := x.ExecContext(ctx, `
		UPDATE sources SET name = ?, library_path = ?, priority = ?, share_all = ?,
			scan_interval_sec = ? WHERE id = ?`,
		s.Name, s.LibraryPath, s.Priority, s.ShareAll, s.ScanIntervalSec, s.ID)
	return err
}
