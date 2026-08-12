package web

import (
	"net/http"
	"strings"

	"github.com/fess932/kobibri/internal/webimport"
)

type importsData struct {
	// Link is what the person typed, kept so the form does not empty itself.
	Link string
	// Editions is the list to choose from, once a link has been looked up.
	Editions []webimport.Edition
	Running  []webimport.Status
	Imported []webimport.Imported
	Busy     bool
	Enabled  bool
}

func (s *Server) handleImports(w http.ResponseWriter, r *http.Request) {
	s.renderImports(w, r, r.URL.Query().Get("link"), nil, "", "")
}

// handleImportLookup answers the first half of the flow: given a link, ask the
// site which translations it carries.
//
// They are genuinely different texts — different wording, often different
// chapter numbering — so downloading one at random would be a coin toss.
func (s *Server) handleImportLookup(w http.ResponseWriter, r *http.Request) {
	link := strings.TrimSpace(r.FormValue("link"))
	if s.imports == nil || link == "" {
		redirect(w, r, "/imports", "", "flash.needLink")
		return
	}

	editions, err := s.imports.Editions(r.Context(), link)
	if err != nil {
		s.renderImports(w, r, link, nil, "", err.Error())
		return
	}

	// With exactly one translation there is nothing to choose; start at once.
	if len(editions) == 1 {
		s.imports.Start(s.background, link, webimport.ImportOptions{EditionID: editions[0].ID})
		redirect(w, r, "/imports", "flash.importStarted", "")
		return
	}

	s.renderImports(w, r, link, editions, "", "")
}

// handleImportStart is the second half: a translation has been chosen.
func (s *Server) handleImportStart(w http.ResponseWriter, r *http.Request) {
	link := strings.TrimSpace(r.FormValue("link"))
	edition := strings.TrimSpace(r.FormValue("edition"))

	if s.imports == nil || link == "" {
		redirect(w, r, "/imports", "", "flash.needLink")
		return
	}

	if !s.imports.Start(s.background, link, webimport.ImportOptions{EditionID: edition}) {
		redirect(w, r, "/imports", "flash.importAlreadyRunning", "")
		return
	}
	redirect(w, r, "/imports", "flash.importStarted", "")
}

// handleImportRefresh checks one imported book for newly published chapters.
func (s *Server) handleImportRefresh(w http.ResponseWriter, r *http.Request) {
	if s.imports == nil {
		redirect(w, r, "/imports", "", "flash.importsOff")
		return
	}

	bookID := r.PathValue("id")
	go func() {
		if _, err := s.imports.Refresh(s.background, bookID); err != nil {
			// Reported on the page through the import's own error field.
			return
		}
	}()
	redirect(w, r, "/imports", "flash.checkingForChapters", "")
}

func (s *Server) renderImports(w http.ResponseWriter, r *http.Request,
	link string, editions []webimport.Edition, flash, errMsg string) {

	data := importsData{Link: link, Editions: editions, Enabled: s.imports != nil}

	if s.imports != nil {
		data.Running = s.imports.Running()
		data.Busy = s.imports.Busy()
		if imported, err := s.imports.List(r.Context()); err == nil {
			data.Imported = imported
		}
	}

	s.render(w, r, "imports.gohtml", page{
		Title: T(langOf(r), "imports.title"), Nav: "imports",
		Flash: flash, Error: errMsg, Data: data,
	})
}
