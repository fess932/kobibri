package kobo

import (
	"net/http"

	"github.com/fess932/kobibri/internal/store"
)

// buildEntitlement renders a canonical book as the device sees it.
//
// Every id field carries the same canonical book uuid, which is what every
// implementation does and what the device expects; it keys on RevisionId and
// EntitlementId.
func (h *Handler) buildEntitlement(r *http.Request, b *store.Book, removed bool) BookEntitlement {
	created := ParseStored(b.CreatedAt)
	return BookEntitlement{
		Accessibility:       accessibilityFull,
		ActivePeriod:        ActivePeriod{From: created},
		Created:             created,
		CrossRevisionID:     b.ID,
		ID:                  b.ID,
		IsRemoved:           removed,
		IsHiddenFromArchive: false,
		IsLocked:            false,
		LastModified:        ParseStored(b.UpdatedAt),
		OriginCategory:      originImported,
		RevisionID:          b.ID,
		Status:              statusActive,
	}
}

// buildMetadata renders a book's metadata for the device.
func (h *Handler) buildMetadata(r *http.Request, b *store.Book) BookMetadata {
	authors := decodeAuthors(b.AuthorsJSON)
	roles := make([]ContributorRole, len(authors))
	for i, a := range authors {
		roles[i] = ContributorRole{Name: a}
	}

	m := BookMetadata{
		Categories:              []string{dummyCategory},
		Contributors:            authors,
		ContributorRoles:        roles,
		CoverImageID:            b.CoverImageID,
		CrossRevisionID:         b.ID,
		CurrentDisplayPrice:     Price{CurrencyCode: "USD", TotalAmount: 0},
		CurrentLoveDisplayPrice: LovePrice{TotalAmount: 0},
		Description:             b.DescriptionHTML,
		DownloadUrls:            h.buildDownloadURLs(r, b),
		EntitlementID:           b.ID,
		ExternalIds:             []string{},
		Genre:                   dummyCategory,
		IsEligibleForKoboLove:   false,
		IsInternetArchive:       false,
		IsPreOrder:              false,
		IsSocialEnabled:         true,
		ISBN:                    b.ISBN13,
		Language:                b.Language,
		PhoneticPronunciations:  map[string]string{},
		PublicationDate:         ParseStored(b.PublishedAt),
		Publisher:               Publisher{Name: b.Publisher},
		RevisionID:              b.ID,
		Title:                   b.Title,
		WorkID:                  b.ID,
	}

	if b.CoverImageID == "" {
		// The device uses this to build a cover URL; pointing it at the book id
		// still resolves, and the handler serves a placeholder.
		m.CoverImageID = b.ID
	}
	if b.SeriesName != "" {
		index := 1.0
		if b.SeriesIndex.Valid {
			index = b.SeriesIndex.Float64
		}
		m.Series = &Series{
			Name:        b.SeriesName,
			Number:      index,
			NumberFloat: index,
			ID:          b.SeriesUUID,
		}
	}
	return m
}

// buildDownloadURLs advertises exactly one format.
//
// Offering both KEPUB and EPUB lets the device pick EPUB, which silently costs
// span-level reading progress. A book with no servable file gets no URLs at
// all; it should not have been in the snapshot in the first place.
func (h *Handler) buildDownloadURLs(r *http.Request, b *store.Book) []DownloadURL {
	if b.DownloadFormat == "" {
		return []DownloadURL{}
	}
	return []DownloadURL{{
		Format:   b.DownloadFormat,
		Size:     b.DownloadSize,
		URL:      h.bookURL(r, "download", b.ID, b.DownloadFormat),
		Platform: platformGeneric,
		DrmType:  drmNone,
	}}
}

// bookURL builds a link back to us, carrying the device's token.
func (h *Handler) bookURL(r *http.Request, elem ...string) string {
	parts := append([]string{"kobo", rawTokenFrom(r.Context())}, elem...)
	return h.urls.Abs(r, parts...)
}

// buildRemovedContainer retracts a book.
//
// Full metadata is unnecessary to withdraw an entitlement — Komga sends a stub
// — and often unavailable, since the book may have been hidden or deleted. What
// matters is that the ids match what the device holds.
func (h *Handler) buildRemovedContainer(r *http.Request, b *store.Book) BookEntitlementContainer {
	meta := h.buildMetadata(r, b)
	meta.DownloadUrls = []DownloadURL{}
	return BookEntitlementContainer{
		BookEntitlement: h.buildEntitlement(r, b, true),
		BookMetadata:    meta,
	}
}

func decodeAuthors(authorsJSON string) []string {
	var authors []string
	if authorsJSON != "" {
		if err := jsonUnmarshalStrings(authorsJSON, &authors); err != nil {
			authors = nil
		}
	}
	if authors == nil {
		authors = []string{}
	}
	return authors
}
