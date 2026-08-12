package kepubconv

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Fixtures are written by someone who already knows what they are testing, which
// is exactly their weakness. Real books carry things nobody thinks to invent:
// nested tables of contents, inline SVG, footnote machinery, markup left behind
// by six different converters.
//
// Point this at a directory of EPUBs and it converts every one with both
// converters and compares them span for span. It is skipped unless asked, since
// it needs books this repository does not have:
//
//	KOBIBRI_TEST_EPUBS=~/Calibre\ Library go test ./internal/kepubconv/ -run RealBooks -v
//
// It needs a real kepubify on PATH, since that is the whole point of the
// comparison. This is how ours became the default: fifty-seven books out of
// someone's actual library, identical span for span. The first run over them
// matched two, which is what a hand-written fixture set is worth.
func TestRealBooksConvertIdentically(t *testing.T) {
	dir := os.Getenv("KOBIBRI_TEST_EPUBS")
	if dir == "" {
		t.Skip("set KOBIBRI_TEST_EPUBS to a directory of EPUBs to compare the converters on real books")
	}

	bin, err := exec.LookPath("kepubify")
	if err != nil {
		t.Skipf("kepubify is not on PATH, and the comparison must be against it: %v", err)
	}
	kepubify := &execConverter{bin: bin}

	books := findEPUBs(t, dir)
	if len(books) == 0 {
		t.Fatalf("no .epub files under %s", dir)
	}
	t.Logf("comparing %d books", len(books))

	var agreed, differed, failed int
	for _, book := range books {
		name := filepath.Base(book)

		theirs, err := convertOne(t, kepubify, book)
		if err != nil {
			t.Logf("SKIP %s: kepubify could not convert it: %v", name, err)
			continue
		}
		ours, err := convertOne(t, newNativeConverter(), book)
		if err != nil {
			failed++
			t.Errorf("FAIL %s: ours could not convert a book kepubify could: %v", name, err)
			continue
		}

		if diff := compareChapters(t, ours, theirs); diff != "" {
			differed++
			t.Errorf("DIFF %s\n%s", name, diff)
			continue
		}
		agreed++
	}

	t.Logf("identical: %d, different: %d, failed: %d", agreed, differed, failed)
}

// compareChapters reports the first real difference, or "" when the two agree.
func compareChapters(t *testing.T, ours, theirs map[string]string) string {
	t.Helper()

	names := make([]string, 0, len(theirs))
	for name := range theirs {
		names = append(names, name)
	}
	sort.Strings(names)

	var report strings.Builder
	for _, name := range names {
		mine, ok := ours[name]
		if !ok {
			fmt.Fprintf(&report, "  %s: missing from ours\n", name)
			continue
		}

		want := spansOf(t, theirs[name])
		got := spansOf(t, mine)
		if len(got) != len(want) {
			fmt.Fprintf(&report, "  %s: %d spans, want %d\n", name, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				fmt.Fprintf(&report, "  %s: span %d is %+v, want %+v\n", name, i, got[i], want[i])
				break
			}
		}
		if report.Len() > 2000 {
			report.WriteString("  …\n")
			break
		}
	}
	return report.String()
}

func convertOne(t *testing.T, conv Converter, src string) (map[string]string, error) {
	t.Helper()

	dst := filepath.Join(t.TempDir(), "out.kepub.epub")
	if err := conv.Convert(context.Background(), src, dst); err != nil {
		return nil, err
	}
	return readChapters(t, dst), nil
}

func findEPUBs(t *testing.T, dir string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of a library is not this test's business
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(path), ".epub") &&
			!strings.HasSuffix(strings.ToLower(path), ".kepub.epub") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}
