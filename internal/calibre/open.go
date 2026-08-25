// Package calibre reads a Calibre library: its metadata.db and the book files
// beside it. It never writes to the user's library.
package calibre

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

var (
	// ErrUnreachable means the library could not be read at all: the path is
	// gone, unmounted, or unreadable. Callers must treat this as "do nothing"
	// rather than "the library is empty" — an unmounted NAS must never mark
	// thousands of books missing.
	ErrUnreachable = errors.New("calibre library unreachable")

	// ErrCorrupt means metadata.db was reachable but failed an integrity check.
	ErrCorrupt = errors.New("calibre metadata.db is corrupt")
)

// DB is a read-only view of a Calibre library, backed by a private snapshot of
// metadata.db.
type DB struct {
	db          *sql.DB
	libraryPath string
	tmpDir      string
}

// LibraryPath is the root directory of the library this DB was opened from.
func (d *DB) LibraryPath() string { return d.libraryPath }

// Signature identifies the state of a library's metadata.db cheaply. When it is
// unchanged since the last scan there is nothing to do and the snapshot copy
// can be skipped entirely.
type Signature struct {
	Size  int64
	Mtime int64
}

// Stat reads the signature of a library's metadata.db without opening it.
func Stat(libraryPath string) (Signature, error) {
	fi, err := os.Stat(filepath.Join(libraryPath, "metadata.db"))
	if err != nil {
		return Signature{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return Signature{Size: fi.Size(), Mtime: fi.ModTime().UnixNano()}, nil
}

// Open takes a private snapshot of the library's metadata.db under workDir and
// opens that copy. The caller must Close the result, which removes the snapshot.
//
// Copying rather than opening in place is deliberate. Calibre keeps metadata.db
// in WAL mode and may be running; opening it read-only in place requires SQLite
// to map or create the -shm file, which fails on read-only mounts and misbehaves
// over SMB/NFS. Working on a copy also means a scan sees one consistent state
// even if the user edits the library halfway through.
func Open(libraryPath, workDir string) (*DB, error) {
	sig, err := Stat(libraryPath)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	tmpDir, err := os.MkdirTemp(workDir, "scan-")
	if err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}

	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	if err := snapshot(libraryPath, tmpDir, sig); err != nil {
		cleanup()
		return nil, err
	}

	// The copy is opened read-write on purpose: SQLite needs to replay the WAL
	// we copied alongside it. Nothing here can reach the user's real database.
	dsn := "file:" + filepath.Join(tmpDir, "metadata.db") +
		"?_txlock=deferred&_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	db.SetMaxOpenConns(1)

	var check string
	if err := db.QueryRow("PRAGMA quick_check(1)").Scan(&check); err != nil {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if check != "ok" {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("%w: quick_check said %q", ErrCorrupt, check)
	}

	return &DB{db: db, libraryPath: libraryPath, tmpDir: tmpDir}, nil
}

func (d *DB) Close() error {
	err := d.db.Close()
	if rmErr := os.RemoveAll(d.tmpDir); err == nil {
		err = rmErr
	}
	return err
}

// snapshot copies metadata.db and its sidecars into dst. The main database is
// copied first and its signature re-checked afterwards: if Calibre wrote to it
// mid-copy we would otherwise pair a stale database with a newer WAL.
func snapshot(libraryPath, dst string, want Signature) error {
	const attempts = 3
	var lastErr error

	for i := range attempts {
		if err := copyFile(filepath.Join(libraryPath, "metadata.db"), filepath.Join(dst, "metadata.db")); err != nil {
			return fmt.Errorf("%w: copy metadata.db: %v", ErrUnreachable, err)
		}
		for _, sidecar := range []string{"metadata.db-wal", "metadata.db-shm"} {
			err := copyFile(filepath.Join(libraryPath, sidecar), filepath.Join(dst, sidecar))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: copy %s: %v", ErrUnreachable, sidecar, err)
			}
		}

		after, err := Stat(libraryPath)
		if err != nil {
			return err
		}
		if after == want {
			return nil
		}

		slog.Debug("calibre database changed during snapshot, retrying",
			"library", libraryPath, "attempt", i+1)
		want = after
		lastErr = fmt.Errorf("metadata.db kept changing during the copy")
	}
	return fmt.Errorf("%w: %v", ErrUnreachable, lastErr)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// safeJoin resolves a slash-separated path from the database against root,
// refusing anything that escapes it. A corrupt or hostile metadata.db must not
// be able to make the server read arbitrary files.
func safeJoin(root string, relSlash ...string) (string, error) {
	rel := filepath.FromSlash(strings.Join(relSlash, "/"))
	joined := filepath.Join(root, rel)

	cleanRoot := filepath.Clean(root)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the library root", rel)
	}
	return joined, nil
}
