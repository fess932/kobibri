package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/fess932/kobibri/internal/ebookconv"
	"github.com/fess932/kobibri/internal/store"
)

// Attach resolves a source row to a canonical book, creating one if this is the
// first time we have seen it and merging existing ones if the row bridges two
// that were previously thought distinct.
//
// The returned id is stable: it survives the source being removed and re-added,
// because every identity key is derived from content rather than from a
// database rowid.
func Attach(ctx context.Context, x store.Execer, sb *store.SourceBook) (string, error) {
	keys := identityRows(sb)
	if len(keys) == 0 {
		return "", fmt.Errorf("source book %d/%d yields no identity keys", sb.SourceID, sb.CalibreID)
	}

	matches, err := store.LookupIdentities(ctx, x, keys)
	if err != nil {
		return "", err
	}

	var bookID string
	switch len(matches) {
	case 0:
		if bookID, err = store.CreateBook(ctx, x); err != nil {
			return "", err
		}
	case 1:
		bookID = matches[0]
	default:
		// This row bridges books we previously thought distinct — typically a
		// source that carries ISBNs meeting one that only had title+author.
		if bookID, err = store.PickSurvivor(ctx, x, matches); err != nil {
			return "", err
		}
		for _, loser := range matches {
			if loser == bookID {
				continue
			}
			if err := store.MergeBooks(ctx, x, bookID, loser); err != nil {
				return "", err
			}
		}
	}

	// Claim any keys this source contributes that were not known yet, so a
	// later source matching on only one of them still lands here.
	if err := store.AddIdentities(ctx, x, bookID, keys); err != nil {
		return "", err
	}
	return bookID, nil
}

func identityRows(sb *store.SourceBook) []store.IdentityRow {
	var out []store.IdentityRow

	// A link is the strongest key there is for an imported book: it is exactly
	// what the reader asked for, and re-importing it must land on the same book.
	if u := strings.TrimSpace(sb.WebURL); u != "" {
		out = append(out, store.IdentityRow{Kind: KindWebURL, Key: strings.ToLower(u)})
	}

	for _, k := range Keys(sb.CalibreUUID, sb.ISBN13, sb.Title, sb.AuthorSort) {
		out = append(out, store.IdentityRow{Kind: k.Kind, Key: k.Key})
	}
	return out
}

// Resolve recomputes a canonical book from its contributing source rows and
// writes the result. It runs whenever any contributor changes.
//
// The winner is taken whole rather than field-by-field, to avoid Frankenstein
// metadata; only fields the winner leaves empty fall back to the next-ranked
// source.
func Resolve(ctx context.Context, x store.Execer, bookID string) error {
	book, err := store.GetBook(ctx, x, bookID)
	if err != nil {
		return err
	}
	if book.MergedInto != "" {
		return nil // an alias; the survivor carries the state
	}

	candidates, err := store.Candidates(ctx, x, bookID)
	if err != nil {
		return err
	}

	before := *book
	apply(book, candidates)

	// metadata_rev is what makes a device see an update, so it must move only
	// when something the device can actually observe changed. Bumping it on
	// every scan would push the whole library to every device, every time.
	//
	// An empty previous hash means this is the book's first resolve, so there
	// is no earlier revision for a device to have seen: the book stays at rev 1.
	if book.ServingHash != before.ServingHash && before.ServingHash != "" {
		book.MetadataRev = before.MetadataRev + 1
	}
	return store.UpdateBookDerived(ctx, x, book)
}

func apply(book *store.Book, candidates []store.Candidate) {
	if len(candidates) == 0 {
		// Every contributing source row is gone or disabled.
		//
		// The serving metadata is deliberately frozen rather than recomputed:
		// clearing the title, cover or download format would change
		// serving_hash, bump metadata_rev, and make the next sync announce a
		// change to every device — for a book that merely stopped being
		// available on the server. A device holding it must be told nothing at
		// all, and if the source comes back unchanged the hash still matches so
		// the silence continues.
		//
		// Only the server-side facts move. Snapshot membership is gated on
		// syncable, so a device that never received the book never will.
		book.Available = false
		book.Syncable = false
		book.PrimarySourceBookID = sql.NullInt64{}
		return
	}

	winner := candidates[0].SourceBook
	book.Available = true
	book.LastAvailableAt = store.Now()
	book.PrimarySourceBookID = sql.NullInt64{Int64: winner.ID, Valid: true}

	book.Title = winner.Title
	book.SortTitle = winner.SortTitle
	book.AuthorsJSON = winner.AuthorsJSON
	book.AuthorSort = winner.AuthorSort
	book.SeriesName = winner.SeriesName
	book.SeriesIndex = winner.SeriesIndex
	book.DescriptionHTML = winner.DescriptionHTML
	book.Publisher = winner.Publisher
	book.PublishedAt = winner.PublishedAt
	book.Language = winner.Language
	book.ISBN13 = winner.ISBN13

	// Fill only what the winner left empty, from the next-ranked sources.
	for _, c := range candidates[1:] {
		sb := c.SourceBook
		fillEmpty(&book.DescriptionHTML, sb.DescriptionHTML)
		fillEmpty(&book.Publisher, sb.Publisher)
		fillEmpty(&book.PublishedAt, sb.PublishedAt)
		fillEmpty(&book.Language, sb.Language)
		fillEmpty(&book.ISBN13, sb.ISBN13)
		if book.SeriesName == "" && sb.SeriesName != "" {
			book.SeriesName = sb.SeriesName
			book.SeriesIndex = sb.SeriesIndex
		}
	}

	if book.Language == "" {
		book.Language = "en"
	}
	book.SeriesUUID = SeriesUUID(book.SeriesName)

	applyCover(book, candidates)
	applyDownload(book, candidates)

	book.Syncable = book.Available && !book.Hidden && book.DownloadFormat != ""
	book.ServingHash = servingHash(book)
}

// applyCover picks the cover independently of the metadata winner, since the
// best metadata and the only cover often live in different sources.
//
// CoverImageId embeds the cover's mtime because the device caches covers by
// ImageId forever; a changed cover with an unchanged id would never be
// refetched. See docs/kobo-protocol.md §8.
func applyCover(book *store.Book, candidates []store.Candidate) {
	for _, c := range candidates {
		if c.SourceBook.CoverRelPath == "" {
			continue
		}
		book.CoverSourceBookID = sql.NullInt64{Int64: c.SourceBook.ID, Valid: true}
		book.CoverImageID = book.ID + "-" + strconv.FormatInt(c.SourceBook.CoverMtime, 10)
		return
	}
	book.CoverSourceBookID = sql.NullInt64{}
	book.CoverImageID = ""
}

// conversionAvailable says whether Calibre's converter is on this machine.
//
// Deliberately process-wide: it is a fact about the machine, fixed for the life
// of the process, and threading it through every call to Resolve would add a
// parameter to a dozen signatures to carry a constant.
var conversionAvailable atomic.Bool

// SetConversionAvailable records whether books in other formats can be turned
// into EPUB. It must be called before the first scan.
//
// It matters because a book is only advertised to a device when we can actually
// serve it: offering one and then failing the download is worse than never
// offering it at all.
func SetConversionAvailable(ok bool) { conversionAvailable.Store(ok) }

// applyDownload decides what single format is advertised to the device.
//
// A pre-paginated EPUB is offered as EPUB3FL and never converted: it already
// has one chapter per page, which is enough for progress tracking, and the
// device renders it full screen. Everything else reflowable is offered as
// KEPUB, converted lazily at download time. Books with no EPUB at all (PDF,
// CBZ, MOBI, AZW3) are not syncable — Kobo does not sync those.
func applyDownload(book *store.Book, candidates []store.Candidate) {
	for _, c := range candidates {
		for _, f := range c.Files {
			if f.Format != "EPUB" || !f.Present {
				continue
			}
			if f.Layout == store.LayoutPrePaginated {
				book.DownloadFormat = store.FormatEPUB3FL
			} else {
				book.DownloadFormat = store.FormatKEPUB
			}
			book.DownloadSize = f.Size
			book.ConvertFrom = ""
			return
		}
	}

	// No EPUB anywhere. The book may still be servable if Calibre's converter is
	// here to make one — but only then, because a book we cannot actually
	// deliver must never be offered.
	if conversionAvailable.Load() {
		for _, c := range candidates {
			var have []string
			for _, f := range c.Files {
				if f.Present {
					have = append(have, f.Format)
				}
			}
			if from := ebookconv.BestConvertible(have); from != "" {
				book.DownloadFormat = store.FormatKEPUB
				book.ConvertFrom = from
				// The converted size is unknown until it is converted; the
				// device treats it as advisory and Content-Length is right.
				book.DownloadSize = sizeOf(c.Files, from)
				return
			}
		}
	}

	book.DownloadFormat = ""
	book.DownloadSize = 0
	book.ConvertFrom = ""
}

func sizeOf(files []store.SourceBookFile, format string) int64 {
	for _, f := range files {
		if f.Format == format {
			return f.Size
		}
	}
	return 0
}

func fillEmpty(dst *string, src string) {
	if *dst == "" {
		*dst = src
	}
}

// seriesNamespace is uuid.NameSpaceDNS, matching what every other Kobo server
// implementation uses to derive Series.Id. It must agree bit for bit, or a
// device that has synced elsewhere sees duplicate series.
func SeriesUUID(name string) string {
	if name == "" {
		return ""
	}
	return uuid.NewMD5(uuid.NameSpaceDNS, []byte(name)).String()
}

// servingFields is exactly what a device can observe about a book. Anything not
// listed here can change freely without disturbing a single sync.
//
// Availability is deliberately absent: whether a source currently holds the
// file is a server-side fact, not something the device sees. Including it would
// make a book vanishing and reappearing look like two metadata changes.
type servingFields struct {
	Title       string   `json:"title"`
	SortTitle   string   `json:"sort_title"`
	Authors     string   `json:"authors"`
	AuthorSort  string   `json:"author_sort"`
	Series      string   `json:"series"`
	SeriesIndex *float64 `json:"series_index"`
	SeriesUUID  string   `json:"series_uuid"`
	Description string   `json:"description"`
	Publisher   string   `json:"publisher"`
	PublishedAt string   `json:"published_at"`
	Language    string   `json:"language"`
	ISBN13      string   `json:"isbn13"`
	CoverImage  string   `json:"cover_image_id"`
	Format      string   `json:"download_format"`
	ConvertFrom string   `json:"convert_from"`
	Size        int64    `json:"download_size"`
}

func servingHash(b *store.Book) string {
	f := servingFields{
		Title: b.Title, SortTitle: b.SortTitle, Authors: b.AuthorsJSON,
		AuthorSort: b.AuthorSort, Series: b.SeriesName, SeriesUUID: b.SeriesUUID,
		Description: b.DescriptionHTML, Publisher: b.Publisher, PublishedAt: b.PublishedAt,
		Language: b.Language, ISBN13: b.ISBN13, CoverImage: b.CoverImageID,
		Format: b.DownloadFormat, ConvertFrom: b.ConvertFrom, Size: b.DownloadSize,
	}
	if b.SeriesIndex.Valid {
		v := b.SeriesIndex.Float64
		f.SeriesIndex = &v
	}

	// json.Marshal on a struct is deterministic: field order is declaration
	// order, so the hash is stable across runs and builds.
	buf, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}
