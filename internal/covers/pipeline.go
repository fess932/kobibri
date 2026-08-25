// Package covers serves book covers to Kobo devices: decoded, scaled into a few
// buckets, cached, and always JPEG.
package covers

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/image/draw"

	"github.com/fess932/kobibri/internal/store"
)

// Buckets. The device asks for whatever pixel size its current view wants, and
// serving a full-resolution JPEG for a list thumbnail visibly stalls library
// browsing. Kobo screens are 3:4, so each bucket keeps that ratio.
const (
	BucketSmall  = "small"
	BucketMedium = "medium"
	BucketLarge  = "large"
)

type bucketSize struct {
	name          string
	width, height int
}

var buckets = map[string]bucketSize{
	BucketSmall:  {BucketSmall, 270, 360},
	BucketMedium: {BucketMedium, 540, 720},
	BucketLarge:  {BucketLarge, 900, 1200},
}

// BucketFor picks a bucket from the height the device asked for.
func BucketFor(height int) string {
	switch {
	case height > 1000:
		return BucketLarge
	case height > 500:
		return BucketMedium
	default:
		return BucketSmall
	}
}

// jpegQuality is a deliberate compromise: the device dithers to greyscale
// anyway, so anything higher is bytes the reader waits for and cannot see.
const jpegQuality = 85

// maxSourceBytes guards against a "cover" that is really a scanned poster.
const maxSourceBytes = 32 << 20

var ErrNoCover = errors.New("no cover available")

// Cache renders and stores scaled covers.
type Cache struct {
	dir   string
	store *store.Store
}

func NewCache(dir string, st *store.Store) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cover cache dir: %w", err)
	}
	return &Cache{dir: dir, store: st}, nil
}

// Get returns a scaled JPEG for the given image id, rendering it if needed.
//
// imageID is the cache-busting form, "<book uuid>-<cover mtime>": the device
// caches covers by image id forever, so a replaced cover has to arrive under a
// new id or it is never refetched.
func (c *Cache) Get(imageID, bucket, srcPath string) (path string, err error) {
	size, ok := buckets[bucket]
	if !ok {
		size = buckets[BucketSmall]
	}

	dst := c.pathFor(imageID, size.name)
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		c.touch(imageID, size.name)
		return dst, nil
	}
	if srcPath == "" {
		return "", ErrNoCover
	}

	if err := c.render(srcPath, dst, size); err != nil {
		return "", err
	}
	c.record(imageID, size, dst)
	return dst, nil
}

func (c *Cache) render(srcPath, dst string, size bucketSize) error {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	if fi.Size() > maxSourceBytes {
		return fmt.Errorf("cover is %d bytes, beyond the %d limit", fi.Size(), maxSourceBytes)
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	src, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("decode cover: %w", err)
	}

	scaled := fit(src, size.width, size.height)

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Render to a temporary name and rename, so an interrupted write cannot
	// leave a truncated file that later looks cached.
	tmp := dst + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(out, scaled, &jpeg.Options{Quality: jpegQuality}); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	_ = out.Close()

	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// fit scales an image to sit inside the target box, preserving aspect ratio and
// never enlarging: upscaling a small cover only wastes bytes.
func fit(src image.Image, maxW, maxH int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return src
	}
	if w <= maxW && h <= maxH {
		return src
	}

	scale := min(float64(maxW)/float64(w), float64(maxH)/float64(h))
	dstW := max(1, int(float64(w)*scale))
	dstH := max(1, int(float64(h)*scale))

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

func (c *Cache) pathFor(imageID, bucket string) string {
	shard := imageID
	if len(shard) > 2 {
		shard = shard[:2]
	}
	return filepath.Join(c.dir, bucket, shard, imageID+".jpg")
}

func (c *Cache) record(imageID string, size bucketSize, path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	now := store.Now()
	_, err = c.store.Writer().Exec(`
		INSERT INTO cover_cache (image_id, bucket, path, width, height, size, created_at, last_used_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(image_id, bucket) DO UPDATE SET
			path = excluded.path, size = excluded.size, last_used_at = excluded.last_used_at`,
		imageID, size.name, path, size.width, size.height, fi.Size(), now, now)
	if err != nil {
		slog.Debug("recording cover cache entry", "err", err)
	}
}

func (c *Cache) touch(imageID, bucket string) {
	_, _ = c.store.Writer().Exec(
		`UPDATE cover_cache SET last_used_at = ? WHERE image_id = ? AND bucket = ?`,
		store.Now(), imageID, bucket)
}

// Evict trims the cache to a byte budget, least recently used first.
func (c *Cache) Evict(budget int64) error {
	rows, err := c.store.Reader().Query(
		`SELECT image_id, bucket, path, size FROM cover_cache ORDER BY last_used_at DESC`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	type entry struct {
		imageID, bucket, path string
		size                  int64
	}
	var (
		keep  int64
		stale []entry
	)
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.imageID, &e.bucket, &e.path, &e.size); err != nil {
			return err
		}
		if keep+e.size <= budget {
			keep += e.size
			continue
		}
		stale = append(stale, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, e := range stale {
		if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
			slog.Debug("removing cached cover", "path", e.path, "err", err)
		}
		_, _ = c.store.Writer().Exec(
			`DELETE FROM cover_cache WHERE image_id = ? AND bucket = ?`, e.imageID, e.bucket)
	}
	return nil
}

// placeholder is the image served when a book has no cover or its cover cannot
// be decoded. It is deliberately not an error: the device hammers cover URLs
// that fail.
var placeholder = sync1(func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 270, 360))
	// Mid grey rather than near-black: the same JPEG is served to a light page,
	// a dark page and a greyscale e-ink screen, and only a middle value reads
	// as "no cover" on all three instead of a hole.
	shade := color.RGBA{R: 0x8c, G: 0x8e, B: 0x96, A: 0xff}
	draw.Draw(img, img.Bounds(), &image.Uniform{shade}, image.Point{}, draw.Src)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70}); err != nil {
		return nil
	}
	return buf.Bytes()
})

// Placeholder returns a neutral JPEG of the right shape.
func Placeholder() []byte { return placeholder() }

// sync1 memoises a niladic function.
func sync1[T any](fn func() T) func() T {
	var (
		once  bool
		value T
	)
	return func() T {
		if !once {
			value, once = fn(), true
		}
		return value
	}
}
