package kobo_test

import (
	"archive/zip"
	"bytes"
	"image"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/store"
)

// The download must arrive as a valid KEPUB, and the filename must carry the
// .kepub.epub suffix: Kobo picks its renderer by filename, so getting this
// wrong silently costs mid-chapter reading progress.
func TestDownloadServesKepub(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Readable", Authors: []string{"Jane Author"}})
	id := s.bookID("Readable")

	resp := s.do("GET", s.kobo("/download/"+id+"/KEPUB"), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/epub+zip" {
		t.Errorf("Content-Type = %q", ct)
	}

	disposition := resp.Header.Get("Content-Disposition")
	if !strings.Contains(disposition, ".kepub.epub") {
		t.Errorf("Content-Disposition = %q, must name a .kepub.epub file", disposition)
	}
	if !strings.HasPrefix(disposition, "attachment;") {
		t.Errorf("Content-Disposition = %q, want an attachment", disposition)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("download was empty")
	}

	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("downloaded file is not a valid zip: %v", err)
	}

	var content strings.Builder
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".xhtml") && !strings.HasSuffix(f.Name, ".html") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		buf, _ := io.ReadAll(rc)
		_ = rc.Close()
		content.Write(buf)
	}
	if !strings.Contains(content.String(), "koboSpan") {
		t.Error("the downloaded file has no koboSpan markup, so it was not converted")
	}
}

// A fixed-layout book must be served untouched: it already has one page per
// chapter, and converting it would break full-screen rendering.
func TestFixedLayoutBookIsServedUnconverted(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{
		Title:   "Fixed Art",
		Formats: []calibretest.FormatSpec{{Format: "EPUB", Kind: "pre-paginated"}},
	})
	id := s.bookID("Fixed Art")

	book, err := store.GetBook(s.ctx, s.store.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	if book.DownloadFormat != store.FormatEPUB3FL {
		t.Fatalf("DownloadFormat = %q, want EPUB3FL", book.DownloadFormat)
	}

	resp := s.do("GET", s.kobo("/download/"+id+"/EPUB3FL"), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a valid zip: %v", err)
	}

	var content strings.Builder
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".xhtml") {
			rc, _ := f.Open()
			buf, _ := io.ReadAll(rc)
			_ = rc.Close()
			content.Write(buf)
		}
	}
	if strings.Contains(content.String(), "koboSpan") {
		t.Error("a fixed-layout book was converted to KEPUB; it must be served as it is")
	}
}

// A book the device deleted must not have a live download URL on that device.
func TestDownloadOfATombstonedBookIs404(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Deleted"})
	id := s.bookID("Deleted")

	// Register the device, then record the deletion.
	s.do("GET", s.kobo("/v1/initialization"), "")
	devices, err := store.ListDevices(s.ctx, s.store.Reader(), s.userID)
	if err != nil || len(devices) == 0 {
		t.Fatalf("devices = %v, err = %v", devices, err)
	}
	if err := store.AddTombstone(s.ctx, s.store.Writer(), devices[0].ID, id); err != nil {
		t.Fatal(err)
	}

	if resp := s.do("GET", s.kobo("/download/"+id+"/KEPUB"), ""); resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDownloadOfUnknownBookIs404(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Known"})

	resp := s.do("GET", s.kobo("/download/00000000-0000-4000-8000-000000000000/KEPUB"), "")
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// Kobo devices resume interrupted downloads, so range requests must work.
func TestDownloadSupportsRangeRequests(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Resumable"})
	id := s.bookID("Resumable")

	req, err := http.NewRequest("GET", s.server.URL+s.kobo("/download/"+id+"/KEPUB"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-99")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 Partial Content", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 100 {
		t.Errorf("got %d bytes for a 100-byte range", len(body))
	}
	if resp.Header.Get("Content-Range") == "" {
		t.Error("no Content-Range header on a partial response")
	}
}

// A non-ASCII title must not produce a header a client will reject.
func TestDownloadFilenameHandlesNonASCII(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{
		Title:   "Война и мир",
		Authors: []string{"Лев Толстой"},
	})
	id := s.bookID("Война и мир")

	resp := s.do("GET", s.kobo("/download/"+id+"/KEPUB"), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	disposition := resp.Header.Get("Content-Disposition")
	if !strings.Contains(disposition, "filename*=UTF-8''") {
		t.Errorf("Content-Disposition = %q, want a UTF-8 filename* parameter", disposition)
	}
	if !strings.Contains(disposition, ".kepub.epub") {
		t.Errorf("Content-Disposition = %q lost the kepub suffix", disposition)
	}
	// The plain filename parameter must stay ASCII, or strict clients choke.
	for _, r := range disposition {
		if r > 127 {
			t.Errorf("Content-Disposition contains a non-ASCII rune %q: %s", r, disposition)
			break
		}
	}
}

// Covers must arrive as JPEG, scaled down: serving full-resolution images
// visibly stalls the device's library browsing.
func TestCoverIsServedAsScaledJPEG(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Illustrated", Cover: true})
	id := s.bookID("Illustrated")

	book, err := store.GetBook(s.ctx, s.store.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	if book.CoverImageID == "" {
		t.Fatal("book has no CoverImageId despite a cover on disk")
	}

	resp := s.do("GET", s.kobo("/covers/"+book.CoverImageID+"/270/360/false/image.jpg"), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response is not a decodable image: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("image format = %q, want jpeg", format)
	}
	if cfg.Width > 270 || cfg.Height > 360 {
		t.Errorf("cover is %dx%d, larger than the requested bucket", cfg.Width, cfg.Height)
	}
}

// The six-segment template carries a Quality parameter; both shapes must work.
func TestCoverQualityTemplateWorks(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Illustrated", Cover: true})
	book, err := store.GetBook(s.ctx, s.store.Reader(), s.bookID("Illustrated"))
	if err != nil {
		t.Fatal(err)
	}

	resp := s.do("GET", s.kobo("/covers/"+book.CoverImageID+"/540/720/85/false/image.jpg"), "")
	if resp.StatusCode != 200 {
		t.Errorf("six-segment cover URL status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// A book with no cover must still produce an image: the device retries failing
// cover URLs relentlessly.
func TestMissingCoverServesAPlaceholderNot404(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Plain"})
	id := s.bookID("Plain")

	resp := s.do("GET", s.kobo("/covers/"+id+"/270/360/false/image.jpg"), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 with a placeholder", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if _, _, err := image.DecodeConfig(bytes.NewReader(body)); err != nil {
		t.Errorf("placeholder is not a decodable image: %v", err)
	}
}

// The device caches covers by image id forever, so a replaced cover has to
// arrive under a new id. The handler must still resolve the plain uuid.
func TestCoverIDWithCacheBusterResolves(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Illustrated", Cover: true})
	id := s.bookID("Illustrated")

	book, err := store.GetBook(s.ctx, s.store.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(book.CoverImageID, id+"-") {
		t.Fatalf("CoverImageId = %q, want %q with a cache-busting suffix", book.CoverImageID, id)
	}

	// Both the suffixed id and the bare uuid must serve an image.
	for _, imageID := range []string{book.CoverImageID, id} {
		resp := s.do("GET", s.kobo("/covers/"+imageID+"/270/360/false/image.jpg"), "")
		if resp.StatusCode != 200 {
			t.Errorf("cover %q: status = %d", imageID, resp.StatusCode)
		}
	}
}

// Larger requests must get a larger rendering, not the small bucket upscaled.
func TestCoverBucketsScaleWithTheRequest(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Illustrated", Cover: true})
	book, err := store.GetBook(s.ctx, s.store.Reader(), s.bookID("Illustrated"))
	if err != nil {
		t.Fatal(err)
	}

	sizes := map[string]int{}
	for _, req := range []struct{ w, h string }{{"270", "360"}, {"900", "1200"}} {
		resp := s.do("GET", s.kobo("/covers/"+book.CoverImageID+"/"+req.w+"/"+req.h+"/false/image.jpg"), "")
		body, _ := io.ReadAll(resp.Body)
		cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("%sx%s: %v", req.w, req.h, err)
		}
		sizes[req.h] = cfg.Height
	}

	// The fixture cover is small, so neither bucket should upscale it; what
	// matters is that a large request is never served something smaller.
	if sizes["1200"] < sizes["360"] {
		t.Errorf("the large bucket (%d px) is smaller than the small one (%d px)",
			sizes["1200"], sizes["360"])
	}
}
