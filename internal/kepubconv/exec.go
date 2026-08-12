package kepubconv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// execConverter shells out to the kepubify binary.
//
// The escape hatch for the day the library and the command-line tool disagree,
// or a newer kepubify ships that we have not vendored. Set KOBIBRI_KEPUBIFY_BIN
// to switch at runtime, with no rebuild.
type execConverter struct {
	bin string
}

func (e *execConverter) Name() string { return "kepubify-exec:" + e.bin }

func (e *execConverter) Convert(ctx context.Context, srcPath, dstPath string) error {
	// kepubify only converts when the destination name ends in .kepub.epub, and
	// it insists on choosing the filename itself when given a directory. So we
	// hand it a scratch directory and move the result into place.
	tmpDir, err := os.MkdirTemp(filepath.Dir(dstPath), "kepubify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	base := strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath))
	out := filepath.Join(tmpDir, base+KepubSuffix)

	cmd := exec.CommandContext(ctx, e.bin, "-o", out, srcPath)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kepubify %s: %w: %s", srcPath, err, strings.TrimSpace(string(combined)))
	}

	produced, err := soleFile(tmpDir)
	if err != nil {
		return err
	}
	return os.Rename(produced, dstPath)
}

// soleFile finds what kepubify actually wrote, since it may pick its own name.
func soleFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("kepubify produced no output in %s", dir)
}
