package kepubconv

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Re-records kepubify's output for every fixture.
//
// kepubify is no longer a dependency, so this runs the real binary — which is
// the point: the recording under testdata/golden is the only evidence left that
// our converter numbers spans the way every book already on a device does, and
// it has to come from kepubify itself rather than from us.
//
//	go install github.com/pgaskin/kepubify/v4/cmd/kepubify@latest
//	KOBIBRI_WRITE_GOLDEN=1 go test ./internal/kepubconv/ -run GenerateGolden
func TestGenerateGolden(t *testing.T) {
	if os.Getenv("KOBIBRI_WRITE_GOLDEN") == "" {
		t.Skip("set KOBIBRI_WRITE_GOLDEN=1 to re-record kepubify's output")
	}

	bin, err := exec.LookPath("kepubify")
	if err != nil {
		t.Fatalf("kepubify is not on PATH, and the recording must come from it: %v", err)
	}

	dir := filepath.Join("testdata", "golden")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range convertFixtures(t, &execConverter{bin: bin}) {
		if err := os.WriteFile(filepath.Join(dir, name+".xhtml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
