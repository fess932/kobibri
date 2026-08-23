package web_test

import (
	"strings"
	"testing"
)

// The overview, the books and the series share one spine entry and separate
// along the top; every other page must show no strip at all.
func TestLibrarySectionSharesOneSpineEntry(t *testing.T) {
	e := newEnv(t)
	e.login()

	for _, tc := range []struct{ path, current string }{
		{"/", "/\""},
		{"/library", "/library\""},
		{"/series", "/series\""},
	} {
		_, body := e.get(tc.path)
		if !strings.Contains(body, `class="subnav"`) {
			t.Errorf("%s has no sub-navigation strip", tc.path)
			continue
		}
		// The strip must mark the page you are on, not just exist.
		i := strings.Index(body, `class="subnav"`)
		strip := body[i:]
		if end := strings.Index(strip, "</nav>"); end > 0 {
			strip = strip[:end]
		}
		if !strings.Contains(strip, `href="`+tc.current+` aria-current="page"`) &&
			!strings.Contains(strip, tc.current+` aria-current="page"`) {
			t.Errorf("%s: the strip does not mark the current page; got %q", tc.path, strip)
		}
	}

	// A page outside the section gets no strip.
	if _, body := e.get("/devices"); strings.Contains(body, `class="subnav"`) {
		t.Error("/devices shows the library sub-navigation")
	}
}
