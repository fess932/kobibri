package ingest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fess932/kobibri/internal/calibre"
	"github.com/fess932/kobibri/internal/store"
)

func (s *Scanner) KeepCopiesIn(dir string) { s.libraryDir = dir }

func (s *Scanner) copyDir(sourceID int64) string {
	if s.libraryDir == "" {
		return ""
	}
	return filepath.Join(s.libraryDir, strconv.FormatInt(sourceID, 10))
}

func (s *Scanner) readsItsOwnCopy(src *store.Source) bool {
	dir := s.copyDir(src.ID)
	return dir == "" || src.FilesPath == dir
}

func (s *Scanner) copyChangedBooks(ctx context.Context, src *store.Source, books []*calibre.Book) error {
	dir := s.copyDir(src.ID)
	for _, b := range books {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, f := range b.Formats {
			if f.RelPath == "" || !f.Present {
				continue
			}
			if _, err := copyUnderRoot(src.LibraryPath, dir, f.RelPath); err != nil {
				return fmt.Errorf("copy %s: %w", f.RelPath, err)
			}
		}
		if b.CoverRelPath != "" {
			if _, err := copyUnderRoot(src.LibraryPath, dir, b.CoverRelPath); err != nil {
				return fmt.Errorf("copy %s: %w", b.CoverRelPath, err)
			}
		}
	}
	return nil
}

func (s *Scanner) copyWholeSource(ctx context.Context, src *store.Source) error {
	dir := s.copyDir(src.ID)
	files, err := store.SourceFiles(ctx, s.store.Reader(), src.ID)
	if err != nil {
		return err
	}
	covers, err := store.SourceCovers(ctx, s.store.Reader(), src.ID)
	if err != nil {
		return err
	}

	rels := make([]string, 0, len(files)+len(covers))
	for _, f := range files {
		rels = append(rels, f.RelPath)
	}
	rels = append(rels, covers...)

	var copied int
	for i, rel := range rels {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := copyUnderRoot(src.LibraryPath, dir, rel)
		if err != nil {
			return fmt.Errorf("copy %s: %w", rel, err)
		}
		if done {
			copied++
		}
		if (i+1)%200 == 0 {
			slog.Info("copying a library here", "source", src.Name, "files", i+1, "of", len(rels))
		}
	}

	if err := store.SetSourceFilesPath(ctx, s.store.Writer(), src.ID, dir); err != nil {
		return err
	}
	src.FilesPath = dir
	slog.Info("library copied here", "source", src.Name, "files", len(rels), "copied", copied, "dir", dir)
	return nil
}

func copyUnderRoot(fromRoot, toRoot, rel string) (bool, error) {
	from, err := underRoot(fromRoot, rel)
	if err != nil {
		return false, err
	}
	to, err := underRoot(toRoot, rel)
	if err != nil {
		return false, err
	}

	want, err := os.Stat(from)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !want.Mode().IsRegular() {
		return false, nil
	}
	if have, err := os.Stat(to); err == nil && have.Size() == want.Size() &&
		have.ModTime().Truncate(time.Second).Equal(want.ModTime().Truncate(time.Second)) {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return false, err
	}
	in, err := os.Open(from)
	if err != nil {
		return false, err
	}
	defer func() { _ = in.Close() }()

	part := to + ".part"
	out, err := os.Create(part)
	if err != nil {
		return false, err
	}
	_, err = io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chtimes(part, want.ModTime(), want.ModTime())
	}
	if err == nil {
		err = os.Rename(part, to)
	}
	if err != nil {
		_ = os.Remove(part)
		return false, err
	}
	return true, nil
}

func underRoot(root, rel string) (string, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	clean := filepath.Clean(root)
	if !strings.HasPrefix(full, clean+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes its library root", rel)
	}
	return full, nil
}

func (s *Scanner) KeepBooksOf(ctx context.Context, sourceID int64) (bool, error) {
	src, err := store.GetSource(ctx, s.store.Reader(), sourceID)
	if err != nil {
		return false, err
	}
	dir := s.copyDir(sourceID)
	if src.Kind != store.SourceKindCalibre || dir == "" {
		return false, nil
	}

	if !s.readsItsOwnCopy(src) {
		if err := s.copyWholeSource(ctx, src); err != nil {
			slog.Warn("copying a library before it is removed", "source", src.Name, "err", err)
		}
	}

	files, err := store.SourceFiles(ctx, s.store.Reader(), sourceID)
	if err != nil {
		return false, err
	}
	var absent []store.SourceFile
	for _, f := range files {
		path, err := underRoot(dir, f.RelPath)
		if err != nil || !isFile(path) {
			absent = append(absent, f)
		}
	}
	if len(absent) == len(files) {
		return false, nil
	}

	err = s.store.Tx(ctx, func(tx *sql.Tx) error {
		for _, f := range absent {
			if err := store.MarkFileAbsent(ctx, tx, f); err != nil {
				return err
			}
		}
		return store.KeepSource(ctx, tx, sourceID, dir)
	})
	if err != nil {
		return false, err
	}
	slog.Info("library removed, its books kept", "source", src.Name,
		"files", len(files)-len(absent), "not_copied", len(absent), "dir", dir)
	return true, s.ResolveSource(ctx, sourceID)
}

func (s *Scanner) DropCopies(sourceID int64) {
	dir := s.copyDir(sourceID)
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("removing a library's copied books", "dir", dir, "err", err)
	}
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
