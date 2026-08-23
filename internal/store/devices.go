package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrDeviceNotFound = errors.New("device not found")

// Device is one e-reader talking to us. It is identified by the pair of the
// token in its api_endpoint and the device id it reports, so two Kobos sharing
// one token still get separate sync state and separate tombstones.
type Device struct {
	ID             int64
	UserID         int64
	TokenHash      string
	KoboDeviceID   string
	Model          string
	Serial         string
	Firmware       string
	UserAgent      string
	FirstSeenAt    string
	LastSeenAt     string
	LastSyncAt     string
	LastSyncStatus string
}

// DeviceIdentity is what a request tells us about the device that sent it.
type DeviceIdentity struct {
	TokenHash    string
	UserID       int64
	KoboDeviceID string
	Model        string
	Serial       string
	Firmware     string
	UserAgent    string
}

// UpsertDevice records a device and returns it, creating the row on first
// contact.
//
// A device is identified by (token_hash, kobo_device_id), and not every request
// carries the device id: /v1/auth/device, /v1/affiliate and /v1/initialization
// arrive before the reader starts sending the header. Taking those at face
// value files them under an id of "", which is a different key — so one reader
// showed up twice, once as itself and once as a nameless row that had never
// synced. The two branches below are what keep it to one row.
func UpsertDevice(ctx context.Context, x Execer, id DeviceIdentity) (*Device, error) {
	now := Now()

	if id.KoboDeviceID == "" {
		// Headerless request. Attach it to the row this token already has
		// rather than opening a nameless one beside it. Most recently seen,
		// because a token reused on a second reader should land on the one
		// actually in use.
		existing, err := scanDevice(x.QueryRowContext(ctx,
			deviceColumns+` FROM devices WHERE token_hash = ?
			 ORDER BY last_seen_at DESC, id DESC LIMIT 1`, id.TokenHash))
		if err == nil {
			id.KoboDeviceID = existing.KoboDeviceID
		} else if err != sql.ErrNoRows {
			return nil, fmt.Errorf("upsert device: %w", err)
		}
		// Still empty means this token has never been seen at all; the row
		// created below is the reader's first, and the branch above will adopt
		// it as soon as a request carrying the id arrives.
	} else {
		// The id has arrived. If this token already opened a nameless row —
		// which is what every first contact does — that row *is* this reader,
		// so name it rather than leave it behind as a duplicate.
		//
		// OR IGNORE because a properly named row may already exist, in which
		// case the rename would collide with UNIQUE(token_hash, kobo_device_id)
		// and there is nothing to heal.
		if _, err := x.ExecContext(ctx,
			`UPDATE OR IGNORE devices SET kobo_device_id = ?
			  WHERE token_hash = ? AND kobo_device_id = ''`,
			id.KoboDeviceID, id.TokenHash); err != nil {
			return nil, fmt.Errorf("upsert device: %w", err)
		}
	}

	_, err := x.ExecContext(ctx, `
		INSERT INTO devices (user_id, token_hash, kobo_device_id, model, serial, firmware,
		                     user_agent, first_seen_at, last_seen_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(token_hash, kobo_device_id) DO UPDATE SET
			model      = CASE WHEN excluded.model      <> '' THEN excluded.model      ELSE devices.model END,
			serial     = CASE WHEN excluded.serial     <> '' THEN excluded.serial     ELSE devices.serial END,
			firmware   = CASE WHEN excluded.firmware   <> '' THEN excluded.firmware   ELSE devices.firmware END,
			user_agent = CASE WHEN excluded.user_agent <> '' THEN excluded.user_agent ELSE devices.user_agent END,
			last_seen_at = excluded.last_seen_at`,
		id.UserID, id.TokenHash, id.KoboDeviceID, id.Model, id.Serial, id.Firmware,
		id.UserAgent, now, now)
	if err != nil {
		return nil, fmt.Errorf("upsert device: %w", err)
	}
	return GetDeviceByToken(ctx, x, id.TokenHash, id.KoboDeviceID)
}

const deviceColumns = `SELECT id, user_id, token_hash, kobo_device_id, model, serial,
	firmware, user_agent, first_seen_at, last_seen_at,
	COALESCE(last_sync_at, ''), last_sync_status`

func scanDevice(row rowScanner) (*Device, error) {
	var d Device
	err := row.Scan(&d.ID, &d.UserID, &d.TokenHash, &d.KoboDeviceID, &d.Model, &d.Serial,
		&d.Firmware, &d.UserAgent, &d.FirstSeenAt, &d.LastSeenAt, &d.LastSyncAt,
		&d.LastSyncStatus)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func GetDeviceByToken(ctx context.Context, q Querier, tokenHash, koboDeviceID string) (*Device, error) {
	d, err := scanDevice(q.QueryRowContext(ctx,
		deviceColumns+` FROM devices WHERE token_hash = ? AND kobo_device_id = ?`,
		tokenHash, koboDeviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	return d, err
}

func GetDevice(ctx context.Context, q Querier, id int64) (*Device, error) {
	d, err := scanDevice(q.QueryRowContext(ctx, deviceColumns+` FROM devices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", ErrDeviceNotFound, id)
	}
	return d, err
}

func ListDevices(ctx context.Context, q Querier, userID int64) ([]*Device, error) {
	rows, err := q.QueryContext(ctx,
		deviceColumns+` FROM devices WHERE user_id = ? ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// AddTombstone records that a device deleted a book on itself. Tombstones are
// permanent and scoped to one device: the book stays in the library, stays on
// every other device, and is never offered to this one again.
func AddTombstone(ctx context.Context, x Execer, deviceID int64, bookID string) error {
	_, err := x.ExecContext(ctx,
		`INSERT OR IGNORE INTO device_tombstones (device_id, book_id, created_at) VALUES (?,?,?)`,
		deviceID, bookID, Now())
	return err
}

// RemoveTombstone undoes an accidental on-device deletion, so the next sync
// offers the book again.
func RemoveTombstone(ctx context.Context, x Execer, deviceID int64, bookID string) error {
	_, err := x.ExecContext(ctx,
		`DELETE FROM device_tombstones WHERE device_id = ? AND book_id = ?`, deviceID, bookID)
	return err
}

func HasTombstone(ctx context.Context, q Querier, deviceID int64, bookID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM device_tombstones WHERE device_id = ? AND book_id = ?`,
		deviceID, bookID).Scan(&n)
	return n > 0, err
}

func ListTombstones(ctx context.Context, q Querier, deviceID int64) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT book_id FROM device_tombstones WHERE device_id = ? ORDER BY created_at DESC`,
		deviceID)
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
