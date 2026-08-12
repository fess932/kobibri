package kobo

import (
	"strings"
	"time"
)

// koboTimeLayout is what the real store emits and what every implementation
// sends. The device is lenient about the format, but there is no reason to find
// out where its tolerance ends.
const koboTimeLayout = "2006-01-02T15:04:05Z"

// KoboTime marshals as the store's timestamp format. The zero value marshals as
// null, which is what `omitzero` on a field then elides entirely.
type KoboTime struct {
	time.Time
}

func Time(t time.Time) KoboTime { return KoboTime{Time: t.UTC()} }

// ParseStored converts a timestamp in kobibri's storage format.
func ParseStored(s string) KoboTime {
	if s == "" {
		return KoboTime{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return KoboTime{}
	}
	return KoboTime{Time: t.UTC()}
}

func (t KoboTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + t.UTC().Format(koboTimeLayout) + `"`), nil
}

func (t *KoboTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*t = KoboTime{}
		return nil
	}
	// Devices send several shapes; accept the ones seen in the wild rather than
	// failing a reading-state update over a fractional second.
	for _, layout := range []string{
		koboTimeLayout,
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999Z",
		"2006-01-02T15:04:05",
	} {
		if parsed, err := time.Parse(layout, s); err == nil {
			*t = KoboTime{Time: parsed.UTC()}
			return nil
		}
	}
	// An unparseable timestamp must not fail the request; the device would
	// abandon the sync over a field we can safely default.
	*t = KoboTime{}
	return nil
}

// IsZero reports whether the timestamp is unset.
func (t KoboTime) IsZero() bool { return t.Time.IsZero() }
