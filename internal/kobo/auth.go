package kobo

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/fess932/kobibri/internal/httpx"
)

// authRequest is what a device posts to /v1/auth/device and /v1/auth/refresh.
// Only UserKey is read; the rest is recorded for diagnostics.
type authRequest struct {
	AffiliateName string `json:"AffiliateName"`
	AppVersion    string `json:"AppVersion"`
	ClientKey     string `json:"ClientKey"`
	DeviceID      string `json:"DeviceId"`
	PlatformID    string `json:"PlatformId"`
	SerialNumber  string `json:"SerialNumber"`
	UserKey       string `json:"UserKey"`
	RefreshToken  string `json:"RefreshToken"`
}

type authResponse struct {
	AccessToken  string `json:"AccessToken"`
	RefreshToken string `json:"RefreshToken"`
	TokenType    string `json:"TokenType"`
	TrackingId   string `json:"TrackingId"`
	UserKey      string `json:"UserKey"`
}

// handleAuth answers /v1/auth/device and /v1/auth/refresh.
//
// The tokens are random and never checked again. Authorisation here is the
// opaque secret in the request path, not anything the device sends in a header
// or body — a Kobo's DeviceId and UserKey are effectively irrevocable
// credentials, so they are unsuitable as our access control. Every other
// self-hosted implementation does the same. See docs/NOTES.md.
func (h *Handler) handleAuth(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := httpx.DecodeJSON(r, 64<<10, &req); err != nil {
		// A device that sends us nothing usable still needs a well-formed
		// answer, or it will not proceed to sync.
		slog.Debug("auth request body was not usable", "err", err)
	}

	if dev := deviceFrom(r.Context()); dev != nil {
		slog.Info("device authenticated",
			"device", dev.ID, "model", dev.Model, "firmware", req.AppVersion)
	}

	httpx.WriteJSON(w, http.StatusOK, authResponse{
		AccessToken:  randomToken(),
		RefreshToken: randomToken(),
		TokenType:    "Bearer",
		TrackingId:   uuid.NewString(),
		UserKey:      req.UserKey,
	})
}

func randomToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		// Falling back to a uuid keeps the device moving; these tokens carry no
		// security meaning for us.
		return base64.StdEncoding.EncodeToString([]byte(uuid.NewString()))
	}
	return base64.StdEncoding.EncodeToString(buf)
}
