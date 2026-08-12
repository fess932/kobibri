package kobo

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// tokenPrefix marks a token as ours, so the real Kobo store's token is never
// mistaken for one of ours and vice versa.
const tokenPrefix = "KOBIBRI."

// SyncToken is the opaque blob the device carries between sync requests.
//
// It holds only references. Everything that matters lives in the sync point
// rows, which is what makes an interrupted sync resumable — a token cannot go
// stale in a way that loses books.
type SyncToken struct {
	Version int    `json:"v"`
	Ongoing string `json:"o,omitempty"`
	Last    string `json:"l,omitempty"`
	// Raw is the real store's token, kept verbatim so proxied syncs keep
	// working alongside ours.
	Raw string `json:"r,omitempty"`
}

func (t SyncToken) String() string {
	buf, err := json.Marshal(t)
	if err != nil {
		return ""
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
}

// ParseSyncToken reads a token from the x-kobo-synctoken header.
//
// Anything we do not recognise is treated as the real store's token: a device
// that has previously synced with Kobo arrives carrying one, and it must be
// preserved rather than discarded. An unrecognised token simply means we start
// from our own last completed snapshot, which is safe — re-announcing a book
// the device already has is idempotent, since it keys on the id.
func ParseSyncToken(header string) SyncToken {
	header = strings.TrimSpace(header)
	if header == "" {
		return SyncToken{Version: 1}
	}
	if !strings.HasPrefix(header, tokenPrefix) {
		return SyncToken{Version: 1, Raw: header}
	}

	buf, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(header, tokenPrefix))
	if err != nil {
		return SyncToken{Version: 1}
	}
	var t SyncToken
	if err := json.Unmarshal(buf, &t); err != nil {
		return SyncToken{Version: 1}
	}
	t.Version = 1
	return t
}
