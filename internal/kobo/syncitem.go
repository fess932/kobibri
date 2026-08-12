package kobo

import "encoding/json"

// A sync response is a flat JSON array of single-key objects: each element
// names its kind and carries one payload. See docs/kobo-protocol.md §3.
const (
	kindNewEntitlement      = "NewEntitlement"
	kindChangedEntitlement  = "ChangedEntitlement"
	kindChangedProductMeta  = "ChangedProductMetadata"
	kindChangedReadingState = "ChangedReadingState"
	kindNewTag              = "NewTag"
	kindChangedTag          = "ChangedTag"
	kindDeletedTag          = "DeletedTag"
)

// SyncItem is one element of the sync array.
type SyncItem struct {
	Kind    string
	Payload any
}

func (i SyncItem) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{i.Kind: i.Payload})
}

// newEntitlement announces a book the device does not have.
func newEntitlement(c BookEntitlementContainer) SyncItem {
	return SyncItem{Kind: kindNewEntitlement, Payload: c}
}

// changedEntitlement is the only way to retract a book: there is no
// DeletedEntitlement in this protocol. The device moves it to its Archive.
func changedEntitlement(c BookEntitlementContainer) SyncItem {
	return SyncItem{Kind: kindChangedEntitlement, Payload: c}
}

func changedProductMetadata(m BookMetadata) SyncItem {
	return SyncItem{Kind: kindChangedProductMeta, Payload: m}
}

func changedReadingState(rs ReadingState) SyncItem {
	return SyncItem{Kind: kindChangedReadingState, Payload: wrappedReadingState{ReadingState: rs}}
}

func newTag(t Tag) SyncItem     { return SyncItem{Kind: kindNewTag, Payload: wrappedTag{Tag: t}} }
func changedTag(t Tag) SyncItem { return SyncItem{Kind: kindChangedTag, Payload: wrappedTag{Tag: t}} }
func deletedTag(t Tag) SyncItem { return SyncItem{Kind: kindDeletedTag, Payload: wrappedTag{Tag: t}} }

// changedBookItems is what a modified book must be sent as.
//
// The device ignores a ReadingState nested inside a ChangedEntitlement — a
// quirk Komga established against real hardware. So a change is announced as a
// NewEntitlement (which the device treats idempotently, keying on Id), followed
// by the metadata and the reading state as separate items.
func changedBookItems(c BookEntitlementContainer) []SyncItem {
	items := []SyncItem{
		newEntitlement(c),
		changedProductMetadata(c.BookMetadata),
	}
	if c.ReadingState != nil {
		items = append(items, changedReadingState(*c.ReadingState))
	}
	return items
}
