package kobo

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/store"
)

// defaultSyncBatch bounds how many books one response covers. It counts books,
// not JSON objects: a changed book costs three objects.
const defaultSyncBatch = 100

func (h *Handler) batchSize() int {
	if h.syncBatch > 0 {
		return h.syncBatch
	}
	return defaultSyncBatch
}

// deviceLocks serialises syncs per device. Two overlapping syncs would race to
// create and complete snapshots for the same device.
type deviceLocks struct {
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

func newDeviceLocks() *deviceLocks { return &deviceLocks{locks: map[int64]*sync.Mutex{}} }

func (d *deviceLocks) Lock(deviceID int64) func() {
	d.mu.Lock()
	m, ok := d.locks[deviceID]
	if !ok {
		m = &sync.Mutex{}
		d.locks[deviceID] = m
	}
	d.mu.Unlock()

	m.Lock()
	return m.Unlock
}

// handleSync serves GET /v1/library/sync.
//
// The response is always a JSON array — `[]` when there is nothing to say,
// never null — with the exact content type the device expects.
func (h *Handler) handleSync(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	if device == nil {
		writeSyncItems(w, nil, SyncToken{Version: 1}, false)
		return
	}

	unlock := h.syncLocks.Lock(device.ID)
	defer unlock()

	incoming := ParseSyncToken(r.Header.Get(hdrSyncToken))

	// The overwhelmingly common sync has nothing to say. Recognising that before
	// building a snapshot is what keeps a device checking in every few minutes
	// from rewriting the whole library each time — tens of thousands of rows, on
	// a machine that is often a NAS with an SD card in it.
	if done, err := h.nothingToSay(r.Context(), device, incoming); err != nil {
		slog.Debug("checking whether anything changed", "device", device.ID, "err", err)
	} else if done != nil {
		writeSyncItems(w, nil, *done, false)
		return
	}

	sp, err := h.resolveSyncPoint(r.Context(), device, incoming)
	if err != nil {
		// Answering with an empty array and the token the device already has
		// makes it retry later. An error status would make it abandon the sync.
		slog.Error("resolving sync point", "device", device.ID, "err", err)
		writeSyncItems(w, nil, incoming, false)
		return
	}

	items, cursor, more, counts, err := h.drain(r, device, sp)
	if err != nil {
		slog.Error("draining sync", "device", device.ID, "sync_point", sp.ID, "err", err)
		writeSyncItems(w, nil, incoming, false)
		return
	}

	if err := store.AddSyncCounts(r.Context(), h.store.Writer(), sp.ID, counts); err != nil {
		slog.Debug("recording what was sent", "sync_point", sp.ID, "err", err)
	}

	outgoing := SyncToken{Version: 1, Raw: sp.RawKoboToken}
	if more {
		if err := store.SaveSyncCursor(r.Context(), h.store.Writer(), sp.ID,
			cursor.cat, cursor.key, sp.ItemsSent+len(items)); err != nil {
			slog.Error("saving sync cursor", "sync_point", sp.ID, "err", err)
		}
		outgoing.Ongoing = sp.ID
		outgoing.Last = sp.ParentID
	} else {
		if err := store.CompleteSyncPoint(r.Context(), h.store.Writer(), sp); err != nil {
			slog.Error("completing sync point", "sync_point", sp.ID, "err", err)
		}
		if err := store.FinishSyncRun(r.Context(), h.store.Writer(), sp.ID, "ok"); err != nil {
			slog.Debug("closing the sync history entry", "sync_point", sp.ID, "err", err)
		}
		outgoing.Last = sp.ID
	}

	slog.Info("sync", "device", device.ID, "items", len(items), "more", more)
	writeSyncItems(w, items, outgoing, more)
}

// nothingToSay reports the token to answer with when the library has not moved
// since the device's last completed sync, or nil when a real sync is needed.
//
// It is deliberately conservative: anything unexpected — a token mid-sync, no
// completed snapshot, a snapshot from before this was recorded — falls through
// to the ordinary path. Being slow is a cost; being silently stale is a fault.
func (h *Handler) nothingToSay(ctx context.Context, device *store.Device, tok SyncToken) (*SyncToken, error) {
	if tok.Ongoing != "" || tok.Last == "" {
		return nil, nil // mid-sync, or a device starting fresh
	}

	sp, err := store.GetSyncPoint(ctx, h.store.Reader(), tok.Last)
	if err != nil {
		return nil, err
	}
	if sp.DeviceID != device.ID || sp.State != store.SyncStateCompleted || sp.Generation == "" {
		return nil, nil
	}

	now, err := store.LibraryGeneration(ctx, h.store.Reader(), device.UserID, device.ID)
	if err != nil {
		return nil, err
	}
	if now != sp.Generation {
		return nil, nil
	}

	// The device keeps the token it already has: the snapshot it names is still
	// exactly what it holds.
	if err := store.TouchDeviceSync(ctx, h.store.Writer(), device.ID); err != nil {
		slog.Debug("recording a sync that had nothing to say", "device", device.ID, "err", err)
	}
	slog.Debug("sync with nothing to say", "device", device.ID, "generation", now)
	return &SyncToken{Version: 1, Last: sp.ID, Raw: sp.RawKoboToken}, nil
}

// resolveSyncPoint decides whether to resume a paused sync or start a new one.
func (h *Handler) resolveSyncPoint(ctx context.Context, device *store.Device, tok SyncToken) (*store.SyncPoint, error) {
	if tok.Ongoing != "" {
		sp, err := store.GetSyncPoint(ctx, h.store.Reader(), tok.Ongoing)
		if err == nil && sp.DeviceID == device.ID && sp.State == store.SyncStateOngoing {
			return sp, nil
		}
	}

	// A snapshot left mid-drain by a device that restarted is retired rather
	// than deleted, and its parent stays put, so nothing is lost.
	if ongoing, err := store.OngoingSyncPoint(ctx, h.store.Reader(), device.ID); err == nil && ongoing != nil {
		if err := store.AbandonSyncPoint(ctx, h.store.Writer(), ongoing.ID); err != nil {
			return nil, err
		}
	}

	parent := tok.Last
	if parent != "" {
		if sp, err := store.GetSyncPoint(ctx, h.store.Reader(), parent); err != nil ||
			sp.DeviceID != device.ID || sp.State != store.SyncStateCompleted {
			parent = ""
		}
	}
	if parent == "" {
		last, err := store.LastCompletedSyncPoint(ctx, h.store.Reader(), device.ID)
		if err != nil {
			return nil, err
		}
		if last != nil {
			parent = last.ID
		}
	}

	var sp *store.SyncPoint
	err := h.store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		sp, err = store.CreateSyncPoint(ctx, tx, device.ID, device.UserID, parent, tok.Raw)
		if err != nil {
			return err
		}
		return store.StartSyncRun(ctx, tx, device.ID, sp.ID)
	})
	return sp, err
}

type syncCursor struct {
	cat store.SyncCategory
	key string
}

// drain walks the categories in their fixed order, emitting items until the
// budget is spent or everything is delivered.
//
// Categories must not interleave: the device wants each one exhausted before
// the next begins.
func (h *Handler) drain(r *http.Request, device *store.Device, sp *store.SyncPoint) ([]SyncItem, syncCursor, bool, store.SyncCounts, error) {
	ctx := r.Context()
	cur := syncCursor{cat: sp.CursorCat, key: sp.CursorKey}

	var (
		items  []SyncItem
		counts store.SyncCounts
		budget = h.batchSize()
	)

	for cur.cat < store.CatDone {
		if budget <= 0 {
			return items, cur, true, counts, nil
		}

		ids, err := h.categoryIDs(ctx, device, sp, cur, budget+1)
		if err != nil {
			return nil, cur, false, counts, err
		}

		exhausted := len(ids) <= budget
		if !exhausted {
			ids = ids[:budget]
		}

		for _, id := range ids {
			produced, err := h.emit(r, device, sp, cur.cat, id)
			if err != nil {
				return nil, cur, false, counts, err
			}
			items = append(items, produced...)
			countOne(&counts, cur.cat)
			cur.key = id
			budget--
		}

		if !exhausted {
			return items, cur, true, counts, nil
		}
		cur.cat, cur.key = cur.cat+1, ""
	}
	return items, cur, false, counts, nil
}

// countOne tallies what a device was told, for the history a person reads when a
// book did not arrive and the only useful question is what was sent.
func countOne(c *store.SyncCounts, cat store.SyncCategory) {
	switch cat {
	case store.CatNewBooks:
		c.NewBooks++
	case store.CatChangedBooks:
		c.ChangedBooks++
	case store.CatRemovedBooks:
		c.RemovedBooks++
	case store.CatReadingStates:
		c.ReadingStates++
	case store.CatNewTags, store.CatChangedTags, store.CatDeletedTags:
		c.Tags++
	}
}

func (h *Handler) categoryIDs(ctx context.Context, device *store.Device, sp *store.SyncPoint, cur syncCursor, limit int) ([]string, error) {
	q := h.store.Reader()
	from, to := sp.ParentID, sp.ID

	switch cur.cat {
	case store.CatNewBooks:
		return store.NewBookIDs(ctx, q, from, to, cur.key, limit)
	case store.CatChangedBooks:
		return store.ChangedBookIDs(ctx, q, from, to, cur.key, limit)
	case store.CatRemovedBooks:
		return store.RemovedBookIDs(ctx, q, from, to, cur.key, limit)
	case store.CatReadingStates:
		return store.ChangedReadingStateIDs(ctx, q, from, to, cur.key, device.UserID, device.ID, limit)
	case store.CatNewTags:
		return store.NewTagIDs(ctx, q, from, to, cur.key, limit)
	case store.CatChangedTags:
		return store.ChangedTagIDs(ctx, q, from, to, cur.key, limit)
	case store.CatDeletedTags:
		return store.DeletedTagIDs(ctx, q, from, to, cur.key, limit)
	default:
		return nil, nil
	}
}

// emit turns one id into the JSON items its category calls for.
func (h *Handler) emit(r *http.Request, device *store.Device, sp *store.SyncPoint, cat store.SyncCategory, id string) ([]SyncItem, error) {
	ctx := r.Context()

	switch cat {
	case store.CatNewBooks, store.CatChangedBooks, store.CatRemovedBooks:
		book, err := store.GetBook(ctx, h.store.Reader(), id)
		if err != nil {
			// A book that disappeared between snapshot and drain is skipped
			// rather than failing the sync.
			slog.Warn("skipping book missing at drain time", "book", id, "err", err)
			return nil, nil
		}

		if cat == store.CatRemovedBooks {
			return []SyncItem{changedEntitlement(h.buildRemovedContainer(r, book))}, nil
		}

		container := BookEntitlementContainer{
			BookEntitlement: h.buildEntitlement(r, book, false),
			BookMetadata:    h.buildMetadata(r, book),
		}
		rs, err := h.readingState(ctx, device.UserID, book)
		if err != nil {
			return nil, err
		}
		container.ReadingState = rs

		if cat == store.CatNewBooks {
			return []SyncItem{newEntitlement(container)}, nil
		}
		// A changed book cannot be announced as a ChangedEntitlement carrying a
		// ReadingState: the device ignores the nested state.
		return changedBookItems(container), nil

	case store.CatReadingStates:
		book, err := store.GetBook(ctx, h.store.Reader(), id)
		if err != nil {
			return nil, nil
		}
		rs, err := h.readingState(ctx, device.UserID, book)
		if err != nil || rs == nil {
			return nil, err
		}
		return []SyncItem{changedReadingState(*rs)}, nil

	case store.CatNewTags, store.CatChangedTags, store.CatDeletedTags:
		tag, err := h.buildTag(ctx, id, cat == store.CatDeletedTags)
		if err != nil || tag == nil {
			return nil, err
		}
		switch cat {
		case store.CatNewTags:
			return []SyncItem{newTag(*tag)}, nil
		case store.CatChangedTags:
			return []SyncItem{changedTag(*tag)}, nil
		default:
			return []SyncItem{deletedTag(*tag)}, nil
		}
	}
	return nil, nil
}

// writeSyncItems sends the response, always as an array.
func writeSyncItems(w http.ResponseWriter, items []SyncItem, token SyncToken, more bool) {
	if items == nil {
		items = []SyncItem{}
	}

	w.Header().Set(hdrSyncToken, token.String())
	if more {
		// "continue" tells the device to come straight back for the rest.
		w.Header().Set(hdrSync, "continue")
	}
	w.Header().Set("Content-Type", httpx.ContentTypeJSON)

	buf, err := json.Marshal(items)
	if err != nil {
		slog.Error("encoding sync response", "err", err)
		buf = []byte("[]")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// jsonUnmarshalStrings decodes a stored JSON array of strings.
func jsonUnmarshalStrings(raw string, dst *[]string) error {
	return json.Unmarshal([]byte(raw), dst)
}
