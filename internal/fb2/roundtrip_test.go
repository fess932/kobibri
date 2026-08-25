package fb2_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fess932/kobibri/internal/fb2"
	"github.com/fess932/kobibri/internal/reader"
)

// The point of converting is that everything downstream can then treat the book
// as an ordinary EPUB: our own reader opens it, its cover comes out, and the
// KEPUB conversion has something to work with.
func TestAConvertedFB2IsAnOrdinaryEPUB(t *testing.T) {
	src := os.Getenv("FB2")
	if src == "" {
		t.Skip("set FB2 to a real .fb2 file")
	}

	dst := filepath.Join(t.TempDir(), "out.epub")
	if err := fb2.Convert(context.Background(), src, dst); err != nil {
		t.Fatalf("convert: %v", err)
	}

	b, err := reader.Open(dst)
	if err != nil {
		t.Fatalf("our own reader will not open it: %v", err)
	}
	defer func() { _ = b.Close() }()

	if b.Title == "" {
		t.Error("no title")
	}
	if len(b.Spine) == 0 {
		t.Error("no chapters")
	}
	t.Logf("title=%q authors=%v chapters=%d", b.Title, b.Meta.Authors, len(b.Spine))

	data, ext, err := b.Cover()
	if err != nil {
		t.Errorf("no cover came out: %v", err)
	} else {
		t.Logf("cover: %d bytes, %s", len(data), ext)
	}

	// Chapter titles have to reach the table of contents, or a reader shows a
	// book of numbered fragments.
	var named int
	for _, c := range b.Spine {
		if c.Title != "" && c.Title != "1" {
			named++
		}
	}
	if named == 0 {
		t.Error("no chapter kept its title")
	}
}
