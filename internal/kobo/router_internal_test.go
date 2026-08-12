package kobo

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The protocol is undocumented, so an endpoint we do not answer is worth a log
// line — but one line for the shape, not one per book.
func TestUnknownEndpointsCollapseToAShape(t *testing.T) {
	tests := []struct{ method, path, want string }{
		{"POST", "/kobo/tok/v1/products/8f2a1b3c-4d5e-6f70-8192-a3b4c5d6e7f8/rating/4",
			"POST /v1/products/{id}/rating/4"},
		{"GET", "/kobo/tok/v1/products/8f2a1b3c-4d5e-6f70-8192-a3b4c5d6e7f8/reviews",
			"GET /v1/products/{id}/reviews"},
		{"GET", "/kobo/tok/v1/user/profile", "GET /v1/user/profile"},
		{"GET", "/kobo/tok/v1/deals/1234567", "GET /v1/deals/{id}"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(tt.method, tt.path, nil)
		if got := endpointShape(r); got != tt.want {
			t.Errorf("endpointShape(%s %s)\n got %q\nwant %q", tt.method, tt.path, got, tt.want)
		}
	}
}

// Two requests for different books are the same endpoint, and must be logged
// once. The token never appears: it is a secret.
func TestTheShapeHidesTheTokenAndTheBook(t *testing.T) {
	a := endpointShape(httptest.NewRequest("GET",
		"/kobo/secrettoken/v1/products/8f2a1b3c-4d5e-6f70-8192-a3b4c5d6e7f8/reviews", nil))
	b := endpointShape(httptest.NewRequest("GET",
		"/kobo/othertoken/v1/products/11111111-2222-3333-4444-555555555555/reviews", nil))

	if a != b {
		t.Errorf("two requests for the same endpoint gave %q and %q", a, b)
	}
	for _, secret := range []string{"secrettoken", "othertoken"} {
		if strings.Contains(a, secret) || strings.Contains(b, secret) {
			t.Error("the token leaked into a log line")
		}
	}
}

// Everything that leaves this server is logged, so the query has to be written
// down — but never a credential in it.
func TestQueryIsLoggedWithoutItsSecrets(t *testing.T) {
	tests := []struct{ raw, want string }{
		{"", ""},
		{"product_id=42&page=2", "page=2&product_id=42"},
		{"access_token=sekrit&page=1", "access_token=<redacted>&page=1"},
		{"ApiKey=abc", "ApiKey=<redacted>"},
		{"signature=zzz&refresh_token=yyy", "refresh_token=<redacted>&signature=<redacted>"},
	}
	for _, tt := range tests {
		if got := redactQuery(tt.raw); got != tt.want {
			t.Errorf("redactQuery(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

// The values themselves must not survive anywhere in the line.
func TestASecretValueNeverReachesTheLog(t *testing.T) {
	const secret = "eyJhbGciOiJIUzI1NiJ9.sekrit"
	got := redactQuery("access_token=" + secret + "&user_key=" + secret + "&title=Dune")
	if strings.Contains(got, secret) {
		t.Errorf("a credential survived redaction: %q", got)
	}
	if !strings.Contains(got, "title=Dune") {
		t.Errorf("an ordinary parameter was lost: %q", got)
	}
}
