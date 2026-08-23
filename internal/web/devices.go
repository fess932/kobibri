package web

import (
	"net/http"
	"strings"

	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/store"
)

type devicesData struct {
	Devices   []deviceView
	Tokens    []store.APIToken
	NewToken  string
	SetupLine string
	BaseURL   string
	NeedsBase bool
	Users     []*store.User
}

type deviceView struct {
	store.DeviceRow
	Tombstones []store.TombstoneEntry
	// Syncs is what this reader was actually told, newest first. When someone
	// says a book did not arrive, this is the only thing that answers it.
	Syncs []store.SyncRun
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())

	all, err := store.ListAllDevices(r.Context(), s.store.Reader())
	if err != nil {
		s.fail(w, r, err)
		return
	}

	data := devicesData{BaseURL: s.publicBase(r), NeedsBase: s.baseURL == ""}
	for _, d := range all {
		// A non-admin only manages their own readers.
		if !user.IsAdmin && d.UserID != user.ID {
			continue
		}
		tombstones, err := store.DeviceTombstones(r.Context(), s.store.Reader(), d.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		syncs, err := store.RecentSyncRuns(r.Context(), s.store.Reader(), d.ID, 8)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data.Devices = append(data.Devices,
			deviceView{DeviceRow: d, Tombstones: tombstones, Syncs: syncs})
	}

	owner := user.ID
	if data.Tokens, err = store.ListAPITokens(r.Context(), s.store.Reader(), owner); err != nil {
		s.fail(w, r, err)
		return
	}

	// A freshly issued secret is shown once, passed through the redirect.
	if raw := r.URL.Query().Get("token"); raw != "" {
		data.NewToken = raw
		data.SetupLine = "api_endpoint=" + data.BaseURL + "/kobo/" + raw
	}

	s.render(w, r, "devices.gohtml", page{Title: T(langOf(r), "devices.title"), Nav: "devices", Data: data})
}

func (s *Server) handleIssueToken(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	label := strings.TrimSpace(r.FormValue("label"))

	raw, err := store.CreateAPIToken(r.Context(), s.store.Writer(), user.ID, label)
	if err != nil {
		redirect(w, r, "/devices", "", err.Error())
		return
	}
	// The secret is shown once and cannot be recovered, so it travels back in
	// the redirect rather than being stored anywhere retrievable.
	http.Redirect(w, r, "/devices?token="+urlEscape(raw), http.StatusSeeOther)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	user := userFrom(r.Context())

	tokens, err := store.ListAPITokens(r.Context(), s.store.Reader(), user.ID)
	if err != nil {
		redirect(w, r, "/devices", "", err.Error())
		return
	}
	owned := false
	for _, t := range tokens {
		if t.TokenHash == hash {
			owned = true
			break
		}
	}
	if !owned && !user.IsAdmin {
		redirect(w, r, "/devices", "", "flash.notYours")
		return
	}

	if err := store.RevokeAPIToken(r.Context(), s.store.Writer(), hash); err != nil {
		redirect(w, r, "/devices", "", err.Error())
		return
	}
	redirect(w, r, "/devices",
		"flash.tokenRevoked", "")
}

func (s *Server) handleResendLibrary(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	if !s.ownsDevice(r, id) {
		redirect(w, r, "/devices", "", "flash.notYours")
		return
	}

	if err := store.ResetDeviceSyncState(r.Context(), s.store.Writer(), id); err != nil {
		redirect(w, r, "/devices", "", err.Error())
		return
	}
	redirect(w, r, "/devices",
		"flash.syncReset", "")
}

func (s *Server) handleForgetTombstone(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	bookID := r.PathValue("book")

	if !s.ownsDevice(r, id) {
		redirect(w, r, "/devices", "", "flash.notYours")
		return
	}
	if err := store.RemoveTombstone(r.Context(), s.store.Writer(), id, bookID); err != nil {
		redirect(w, r, "/devices", "", err.Error())
		return
	}
	redirect(w, r, "/devices", "flash.tombstoneGone", "")
}

func (s *Server) ownsDevice(r *http.Request, deviceID int64) bool {
	user := userFrom(r.Context())
	if user == nil {
		return false
	}
	if user.IsAdmin {
		return true
	}
	device, err := store.GetDevice(r.Context(), s.store.Reader(), deviceID)
	return err == nil && device.UserID == user.ID
}

// Users

type usersData struct {
	Users []*store.User
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := store.ListUsers(r.Context(), s.store.Reader())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "users.gohtml", page{Title: T(langOf(r), "users.title"), Nav: "users",
		Data: usersData{Users: users}})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	password := r.FormValue("password")

	if name == "" || len(password) < 8 {
		redirect(w, r, "/users", "", "flash.needNamePassword")
		return
	}

	hash, err := HashPassword(password)
	if err != nil {
		redirect(w, r, "/users", "", err.Error())
		return
	}
	if _, err := store.CreateUser(r.Context(), s.store.Writer(), name, hash,
		r.FormValue("admin") == "1"); err != nil {
		redirect(w, r, "/users", "", Msg("flash.userAddFailed", name+": "+err.Error()))
		return
	}
	redirect(w, r, "/users", Msg("flash.userAdded", name), "")
}

func (s *Server) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	password := r.FormValue("password")

	if len(password) < 8 {
		redirect(w, r, "/users", "", "flash.shortPassword")
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		redirect(w, r, "/users", "", err.Error())
		return
	}
	if err := store.SetUserPassword(r.Context(), s.store.Writer(), id, hash); err != nil {
		redirect(w, r, "/users", "", err.Error())
		return
	}
	redirect(w, r, "/users", "flash.passwordChanged", "")
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))

	if user := userFrom(r.Context()); user != nil && user.ID == id {
		redirect(w, r, "/users", "", "flash.cannotRemoveSelf")
		return
	}

	users, err := store.ListUsers(r.Context(), s.store.Reader())
	if err != nil {
		redirect(w, r, "/users", "", err.Error())
		return
	}
	admins := 0
	for _, u := range users {
		if u.IsAdmin {
			admins++
		}
	}
	for _, u := range users {
		if u.ID == id && u.IsAdmin && admins <= 1 {
			redirect(w, r, "/users", "", "flash.lastAdmin")
			return
		}
	}

	if err := store.DeleteUser(r.Context(), s.store.Writer(), id); err != nil {
		redirect(w, r, "/users", "", err.Error())
		return
	}
	redirect(w, r, "/users", "flash.userRemoved", "")
}

// downloadName builds a filename for a browser download.
func downloadName(book *store.Book, ext string) string {
	name := book.Title
	// The first author is enough for a filename, and keeps it the same length
	// whoever else worked on the book.
	if authors := authorList(book.AuthorsJSON); len(authors) > 0 {
		name += " - " + authors[0]
	}
	name = sanitiseFilename(name)
	if name == "" {
		name = book.ID
	}
	return name + ext
}

func sanitiseFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
		case strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 120 {
		out = strings.TrimSpace(out[:120])
	}
	return out
}

func contentDisposition(filename string) string {
	return httpx.ContentDisposition(filename)
}
