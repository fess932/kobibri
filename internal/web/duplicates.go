package web

import (
	"net/http"
	"strconv"

	"github.com/fess932/kobibri/internal/ingest"
)

type duplicatesData struct {
	Suspects []ingest.Duplicate
}

// handleDuplicates lists books joined on the weakest evidence there is.
func (s *Server) handleDuplicates(w http.ResponseWriter, r *http.Request) {
	suspects, err := ingest.SuspectMerges(r.Context(), s.store.Reader())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "duplicates.gohtml", page{
		Title: T(langOf(r), "dupes.title"), Nav: "library",
		Data: duplicatesData{Suspects: suspects}})
}

func (s *Server) handleSplit(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	sourceBookID, err := strconv.ParseInt(r.PathValue("sb"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	newBookID, err := ingest.Split(r.Context(), s.store, sourceBookID)
	if err != nil {
		redirect(w, r, "/books/"+bookID, "", Msg("flash.splitFailed", err.Error()))
		return
	}

	// The new book needs converting before anyone asks for it.
	s.prewarm()
	redirect(w, r, "/books/"+newBookID, "flash.split", "")
}

func (s *Server) handleRejoin(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	sourceBookID, err := strconv.ParseInt(r.PathValue("sb"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := ingest.Rejoin(r.Context(), s.store, sourceBookID); err != nil {
		redirect(w, r, "/books/"+bookID, "", Msg("flash.rejoinFailed", err.Error()))
		return
	}
	redirect(w, r, "/books/"+bookID, "flash.rejoined", "")
}
