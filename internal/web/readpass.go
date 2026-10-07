package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fess932/kobibri/internal/store"
)

const readPassWindow = 12 * time.Hour

var (
	readSecretOnce sync.Once
	readSecret     []byte
)

func readPassKey() []byte {
	readSecretOnce.Do(func() {
		readSecret = make([]byte, 32)
		_, _ = rand.Read(readSecret)
	})
	return readSecret
}

func signReadPass(bookID string, userID, expires int64) []byte {
	mac := hmac.New(sha256.New, readPassKey())
	mac.Write([]byte(bookID))
	_ = binary.Write(mac, binary.BigEndian, [2]int64{userID, expires})
	return mac.Sum(nil)[:16]
}

func newReadPass(bookID string, userID int64, now time.Time) string {
	window := int64(readPassWindow / time.Second)
	expires := (now.Unix()/window + 2) * window

	raw := make([]byte, 16, 32)
	binary.BigEndian.PutUint64(raw[:8], uint64(userID))
	binary.BigEndian.PutUint64(raw[8:], uint64(expires))
	raw = append(raw, signReadPass(bookID, userID, expires)...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func readPassUser(pass, bookID string, now time.Time) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(pass)
	if err != nil || len(raw) != 32 {
		return 0, false
	}
	userID := int64(binary.BigEndian.Uint64(raw[:8]))
	expires := int64(binary.BigEndian.Uint64(raw[8:16]))
	if now.Unix() > expires || !hmac.Equal(raw[16:], signReadPass(bookID, userID, expires)) {
		return 0, false
	}
	return userID, true
}

func (l readLook) segment() string {
	return l.Theme + "." + l.Font + "." + strconv.Itoa(l.Size)
}

func readLookFromSegment(segment string) readLook {
	look := defaultReadLook()
	parts := strings.Split(segment, ".")
	if len(parts) != 3 {
		return look
	}
	values := map[string][]string{"theme": {parts[0]}, "font": {parts[1]}, "size": {parts[2]}}
	return look.with(values)
}

func (s *Server) handleReadPage(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lookupBook(w, r)
	if !ok {
		return
	}
	userID, ok := readPassUser(r.PathValue("pass"), book.ID, time.Now())
	if !ok {
		http.NotFound(w, r)
		return
	}
	user, err := store.GetUser(r.Context(), s.store.Reader(), userID)
	if err != nil || user.Disabled {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=3600")
	s.serveBookFile(w, r, book, r.PathValue("path"), readLookFromSegment(r.PathValue("look")))
}
