package kepubconv

import (
	"archive/zip"
	"context"
	"fmt"
	"os"

	"github.com/pgaskin/kepubify/v4/kepub"
)

// libConverter converts in-process using kepubify as a library.
//
// This is the only file that imports kepubify. The indirection is deliberate:
// the project has not seen a release since 2022, so the day it has to be
// replaced — by a newer upstream, or by our own converter — the blast radius is
// this file plus a constructor. See docs/PROGRESS.md.
type libConverter struct {
	c *kepub.Converter
}

func newLibConverter() *libConverter {
	return &libConverter{c: kepub.NewConverter()}
}

func (l *libConverter) Name() string { return "kepubify-library" }

func (l *libConverter) Convert(ctx context.Context, srcPath, dstPath string) error {
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open epub: %w", err)
	}
	defer zr.Close()

	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if err := l.c.Convert(ctx, out, zr); err != nil {
		return fmt.Errorf("kepubify: %w", err)
	}
	return out.Sync()
}
