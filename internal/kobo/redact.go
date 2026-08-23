package kobo

import (
	"net/url"
	"sort"
	"strings"
)

// secretParams are query keys whose values must never be written down. The
// store's API is undocumented, so the list is by shape rather than by knowledge
// of every parameter a device might send.
var secretParams = []string{"token", "key", "secret", "password", "signature", "auth", "code"}

// redactQuery keeps the shape of a query — which parameters were sent — while
// dropping anything that could be a credential.
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "<unparseable>"
	}

	out := make([]string, 0, len(values))
	for key := range values {
		if isSecretParam(key) {
			out = append(out, key+"=<redacted>")
			continue
		}
		out = append(out, key+"="+strings.Join(values[key], ","))
	}
	sort.Strings(out)
	return strings.Join(out, "&")
}

func isSecretParam(key string) bool {
	lower := strings.ToLower(key)
	for _, secret := range secretParams {
		if strings.Contains(lower, secret) {
			return true
		}
	}
	return false
}
