// Package web serves kobibri's browser interface: sources, library, devices and
// users. It is rendered server-side and ships with no external assets, so the
// whole thing stays one binary.
package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/fess932/kobibri/internal/store"
)

const sessionCookie = "kobibri_session"

type ctxKey int

const (
	userKey ctxKey = iota
	sessionKey
)

func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(userKey).(*store.User)
	return u
}

func sessionFrom(ctx context.Context) *store.Session {
	s, _ := ctx.Value(sessionKey).(*store.Session)
	return s
}

// HashPassword prepares a password for storage.
func HashPassword(password string) (string, error) {
	buf, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(buf), err
}

// checkPassword reports whether a password matches. A user with no password set
// cannot log in at all, rather than matching everything.
func checkPassword(hash, password string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// requireLogin gates a handler on a valid session.
func (s *Server) requireLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			s.redirectToLogin(w, r)
			return
		}

		session, err := store.GetSession(r.Context(), s.store.Reader(), cookie.Value)
		if err != nil {
			s.clearSession(w)
			s.redirectToLogin(w, r)
			return
		}
		user, err := store.GetUser(r.Context(), s.store.Reader(), session.UserID)
		if err != nil || user.Disabled {
			s.clearSession(w)
			s.redirectToLogin(w, r)
			return
		}

		// Every state-changing request carries the session's CSRF token, so a
		// form on another site cannot act on the user's behalf.
		if isMutating(r.Method) && !validCSRF(r, session.CSRF) {
			slog.Warn("rejected a request with a bad CSRF token",
				"user", user.Name, "path", r.URL.Path)
			http.Error(w, T(langOf(r), "err.formExpired"), http.StatusForbidden)
			return
		}

		ctx := context.WithValue(r.Context(), userKey, user)
		ctx = context.WithValue(ctx, sessionKey, session)
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin additionally gates on the admin flag.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireLogin(func(w http.ResponseWriter, r *http.Request) {
		if u := userFrom(r.Context()); u == nil || !u.IsAdmin {
			http.Error(w, T(langOf(r), "err.adminsOnly"), http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func validCSRF(r *http.Request, want string) bool {
	got := r.Header.Get("X-CSRF-Token")
	if got == "" {
		got = r.FormValue("csrf")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.RequestURI()
	if r.Method != http.MethodGet {
		next = "/"
	}
	http.Redirect(w, r, "/login?next="+urlEscape(next), http.StatusSeeOther)
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, session *store.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    session.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   int(store.SessionTTL.Seconds()),
	})
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
}

// bootstrapAdmin creates the first account so a fresh install can be logged
// into. It runs only when there are no users at all.
func (s *Server) bootstrapAdmin(ctx context.Context, password string) error {
	n, err := store.CountUsers(ctx, s.store.Reader())
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if password == "" {
		slog.Warn("no users yet: set KOBIBRI_ADMIN_PASSWORD and restart to create the first account")
		return nil
	}

	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := store.CreateUser(ctx, s.store.Writer(), "admin", hash, true); err != nil {
		return err
	}
	slog.Info("created the first administrator account", "name", "admin")
	return nil
}

var errBadLogin = errors.New("that username and password do not match")

// authenticate checks a login attempt.
func (s *Server) authenticate(ctx context.Context, name, password string) (*store.User, error) {
	user, err := store.GetUserByName(ctx, s.store.Reader(), strings.TrimSpace(name))
	if err != nil {
		// Run a hash comparison anyway so a missing user and a wrong password
		// take about the same time.
		bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalidin"), []byte(password))
		return nil, errBadLogin
	}
	if user.Disabled || !checkPassword(user.PasswordHash, password) {
		return nil, errBadLogin
	}
	return user, nil
}
