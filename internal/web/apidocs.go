package web

import (
	"net/http"

	"github.com/fess932/kobibri/internal/kobo"
)

// handleAPIDocs draws the Kobo API reference.
//
// The document is parsed once, at startup, by Server.apiDoc: a specification
// that will not parse is a broken build, not a broken request, and finding that
// out when someone opens the page is finding out too late.
func (s *Server) handleAPIDocs(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "apidocs.gohtml", page{
		Title: T(langOf(r), "api.title"),
		Nav:   "api",
		Data:  s.apiDoc,
	})
}

// handleAPISpec serves the specification itself, for anything that reads
// OpenAPI: Swagger UI, Postman, a client generator. The page beside it is for
// reading; this is for machines.
func (s *Server) handleAPISpec(w http.ResponseWriter, r *http.Request) {
	raw, err := kobo.OpenAPI()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="kobibri-kobo-openapi.json"`)
	w.Write(raw)
}
