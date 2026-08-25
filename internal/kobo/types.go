package kobo

// Wire types for the Kobo store sync API. Field names and casing are exact and
// load-bearing; see docs/NOTES.md for where each one came from.

// Fixed values every self-hosted implementation sends.
const (
	accessibilityFull = "Full"
	statusActive      = "Active"
	originImported    = "Imported"

	// Kobo's dummy category/genre id. Real store books carry real ones; a
	// self-hosted library has nothing meaningful to put here.
	dummyCategory = "00000000-0000-0000-0000-000000000001"

	// The device accepts Generic or Android. It asks for both.
	platformGeneric = "Generic"
	drmNone         = "None"

	tagTypeUser        = "UserTag"
	tagItemTypeProduct = "ProductRevisionTagItem"
)

// BookEntitlement is the device's claim on a book.
//
// Every id field carries the same canonical book uuid; the device keys on
// RevisionId and EntitlementId. IsRemoved is how a book is retracted — there is
// no DeletedEntitlement in this protocol.
type BookEntitlement struct {
	Accessibility       string       `json:"Accessibility"`
	ActivePeriod        ActivePeriod `json:"ActivePeriod"`
	Created             KoboTime     `json:"Created"`
	CrossRevisionID     string       `json:"CrossRevisionId"`
	ID                  string       `json:"Id"`
	IsRemoved           bool         `json:"IsRemoved"`
	IsHiddenFromArchive bool         `json:"IsHiddenFromArchive"`
	IsLocked            bool         `json:"IsLocked"`
	LastModified        KoboTime     `json:"LastModified"`
	OriginCategory      string       `json:"OriginCategory"`
	RevisionID          string       `json:"RevisionId"`
	Status              string       `json:"Status"`
}

type ActivePeriod struct {
	From KoboTime `json:"From"`
}

// BookMetadata is everything the device shows about a book.
type BookMetadata struct {
	Categories              []string          `json:"Categories"`
	Contributors            []string          `json:"Contributors"`
	ContributorRoles        []ContributorRole `json:"ContributorRoles"`
	CoverImageID            string            `json:"CoverImageId"`
	CrossRevisionID         string            `json:"CrossRevisionId"`
	CurrentDisplayPrice     Price             `json:"CurrentDisplayPrice"`
	CurrentLoveDisplayPrice LovePrice         `json:"CurrentLoveDisplayPrice"`
	Description             string            `json:"Description"`
	DownloadUrls            []DownloadURL     `json:"DownloadUrls"`
	EntitlementID           string            `json:"EntitlementId"`
	ExternalIds             []string          `json:"ExternalIds"`
	Genre                   string            `json:"Genre"`
	IsEligibleForKoboLove   bool              `json:"IsEligibleForKoboLove"`
	IsInternetArchive       bool              `json:"IsInternetArchive"`
	IsPreOrder              bool              `json:"IsPreOrder"`
	IsSocialEnabled         bool              `json:"IsSocialEnabled"`
	ISBN                    string            `json:"ISBN,omitempty"`
	Language                string            `json:"Language"`
	PhoneticPronunciations  map[string]string `json:"PhoneticPronunciations"`
	PublicationDate         KoboTime          `json:"PublicationDate"`
	Publisher               Publisher         `json:"Publisher"`
	RevisionID              string            `json:"RevisionId"`
	Series                  *Series           `json:"Series,omitempty"`
	Title                   string            `json:"Title"`
	WorkID                  string            `json:"WorkId"`
}

type ContributorRole struct {
	Name string `json:"Name"`
}

type Price struct {
	CurrencyCode string  `json:"CurrencyCode"`
	TotalAmount  float64 `json:"TotalAmount"`
}

type LovePrice struct {
	TotalAmount float64 `json:"TotalAmount"`
}

type Publisher struct {
	Imprint string `json:"Imprint"`
	Name    string `json:"Name"`
}

// Series.Id is uuid3 over NAMESPACE_DNS of the series name, matching every
// other implementation bit for bit so a device that has synced elsewhere does
// not end up with duplicate series.
type Series struct {
	Name        string  `json:"Name"`
	Number      float64 `json:"Number"`
	NumberFloat float64 `json:"NumberFloat"`
	ID          string  `json:"Id"`
}

// DownloadURL tells the device where and in what format to fetch a book.
//
// Exactly one is ever sent. Offering both KEPUB and EPUB lets the device choose
// EPUB, which silently costs span-level reading progress.
type DownloadURL struct {
	Format   string `json:"Format"`
	Size     int64  `json:"Size"`
	URL      string `json:"Url"`
	Platform string `json:"Platform"`
	DrmType  string `json:"DrmType"`
}

// Reading status values.
const (
	StatusReadyToRead = "ReadyToRead"
	StatusReading     = "Reading"
	StatusFinished    = "Finished"
)

type ReadingState struct {
	EntitlementID     string      `json:"EntitlementId"`
	Created           KoboTime    `json:"Created"`
	LastModified      KoboTime    `json:"LastModified"`
	PriorityTimestamp KoboTime    `json:"PriorityTimestamp"`
	StatusInfo        StatusInfo  `json:"StatusInfo"`
	Statistics        *Statistics `json:"Statistics,omitempty"`
	CurrentBookmark   *Bookmark   `json:"CurrentBookmark,omitempty"`
}

type StatusInfo struct {
	LastModified           KoboTime `json:"LastModified"`
	Status                 string   `json:"Status"`
	TimesStartedReading    int      `json:"TimesStartedReading"`
	LastTimeStartedReading KoboTime `json:"LastTimeStartedReading,omitzero"`
}

type Statistics struct {
	LastModified         KoboTime `json:"LastModified"`
	SpentReadingMinutes  int      `json:"SpentReadingMinutes"`
	RemainingTimeMinutes int      `json:"RemainingTimeMinutes"`
}

// Bookmark carries reading position. Location.Type "KoboSpan" only works for
// kepub; a plain EPUB gives chapter-level granularity at best.
type Bookmark struct {
	LastModified                 KoboTime  `json:"LastModified"`
	ProgressPercent              float64   `json:"ProgressPercent"`
	ContentSourceProgressPercent float64   `json:"ContentSourceProgressPercent"`
	Location                     *Location `json:"Location,omitempty"`
}

type Location struct {
	Value  string `json:"Value"`
	Type   string `json:"Type"`
	Source string `json:"Source"`
}

// Tag is a device collection.
type Tag struct {
	Created      KoboTime  `json:"Created"`
	ID           string    `json:"Id"`
	Items        []TagItem `json:"Items,omitempty"`
	LastModified KoboTime  `json:"LastModified"`
	Name         string    `json:"Name"`
	Type         string    `json:"Type"`
}

type TagItem struct {
	RevisionID string `json:"RevisionId"`
	Type       string `json:"Type"`
}

// BookEntitlementContainer is the payload of New/ChangedEntitlement.
type BookEntitlementContainer struct {
	BookEntitlement BookEntitlement `json:"BookEntitlement"`
	BookMetadata    BookMetadata    `json:"BookMetadata"`
	ReadingState    *ReadingState   `json:"ReadingState,omitempty"`
}

type wrappedTag struct {
	Tag Tag `json:"Tag"`
}

type wrappedReadingState struct {
	ReadingState ReadingState `json:"ReadingState"`
}
