package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/fess932/kobibri/internal/upload"
)

type uploadsData struct {
	Items    []upload.Item
	Accepted string
	// HasCalibre says whether the formats this server cannot convert by itself
	// are possible at all here.
	HasCalibre bool
	Disabled   bool
}

func (s *Server) handleUploads(w http.ResponseWriter, r *http.Request) {
	// What this machine can actually do, not what the format list says in
	// principle: promising AZW3 with no Calibre installed is how someone uploads
	// twelve files and gets twelve rows that never convert.
	data := uploadsData{
		Accepted:   strings.Join(s.ebook.Formats(), ", "),
		HasCalibre: s.ebook.HasCalibre(),
		Disabled:   s.uploads == nil,
	}
	if s.uploads != nil {
		items, err := s.uploads.List(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data.Items = items
	}
	s.render(w, r, "uploads.gohtml", page{
		Title: T(langOf(r), "uploads.title"), Nav: "uploads", Data: data})
}

// handleUpload takes files straight from the browser.
//
// Each file is handled on its own: one book that will not do must not throw away
// the twelve that would have.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		redirect(w, r, "/uploads", "", "flash.uploadsOff")
		return
	}

	// Streamed rather than buffered: a book is far larger than a form, and
	// ParseMultipartForm would put the whole lot in memory or a temporary file
	// only for it to be copied again.
	r.Body = http.MaxBytesReader(w, r.Body, upload.MaxSize+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		redirect(w, r, "/uploads", "", "flash.uploadFailed")
		return
	}

	var added int
	var failed []string
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() != "file" || part.FileName() == "" {
			_ = part.Close()
			continue
		}

		name := part.FileName()
		_, err = s.uploads.Add(r.Context(), name, part)
		_ = part.Close()
		if err != nil {
			slog.Warn("upload rejected", "file", name, "err", err)
			failed = append(failed, name+" — "+reasonFor(langOf(r), err))
			continue
		}
		added++
	}

	if added > 0 {
		// Newly uploaded books need converting before a device asks for them.
		s.prewarm()
	}

	switch {
	case len(failed) > 0 && added == 0:
		redirect(w, r, "/uploads", "", Msg("flash.uploadFailedWith", strings.Join(failed, "; ")))
	case len(failed) > 0:
		redirect(w, r, "/uploads", Msg("flash.uploadedSome", strconv.Itoa(added)),
			Msg("flash.uploadFailedWith", strings.Join(failed, "; ")))
	case added == 0:
		redirect(w, r, "/uploads", "", "flash.uploadNothing")
	default:
		redirect(w, r, "/uploads", Msg("flash.uploaded", strconv.Itoa(added)), "")
	}
}

func (s *Server) handleUploadRemove(w http.ResponseWriter, r *http.Request) {
	if s.uploads == nil {
		redirect(w, r, "/uploads", "", "flash.uploadsOff")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.uploads.Remove(r.Context(), id); err != nil {
		redirect(w, r, "/uploads", "", err.Error())
		return
	}
	redirect(w, r, "/uploads", "flash.uploadRemoved", "")
}

// reasonFor turns a rejection into something worth reading, in the language the
// list around it is written in.
func reasonFor(lang Lang, err error) string {
	switch {
	case errors.Is(err, upload.ErrUnsupported):
		return T(lang, "upload.badFormat")
	case errors.Is(err, upload.ErrTooLarge):
		return T(lang, "upload.tooLarge")
	case errors.Is(err, upload.ErrEmpty):
		return T(lang, "upload.empty")
	}
	return err.Error()
}
