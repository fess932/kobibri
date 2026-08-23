package kobo_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/kobo"
	"github.com/fess932/kobibri/internal/store"
)

// createCollection posts a new collection and returns the id the server issued.
func createCollection(t *testing.T, s *syncEnv, name string, bookIDs ...string) string {
	t.Helper()

	items := make([]string, len(bookIDs))
	for i, id := range bookIDs {
		items[i] = `{"RevisionId":"` + id + `","Type":"ProductRevisionTagItem"}`
	}
	body := `{"Name":"` + name + `","Items":[` + strings.Join(items, ",") + `]}`

	resp := s.do("POST", s.kobo("/v1/library/tags"), body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create collection status = %d, want 201", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	// The device expects a bare JSON string, not an object wrapping the id.
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatalf("create collection returned %s, want a bare JSON string: %v", raw, err)
	}
	if id == "" {
		t.Fatal("create collection returned an empty id")
	}
	return id
}

// tagsIn pulls the collections out of a set of sync items.
func tagsIn(t *testing.T, items []map[string]json.RawMessage, kind string) []kobo.Tag {
	t.Helper()
	var out []kobo.Tag
	for _, item := range items {
		payload, ok := item[kind]
		if !ok {
			continue
		}
		var wrapper struct {
			Tag kobo.Tag `json:"Tag"`
		}
		if err := json.Unmarshal(payload, &wrapper); err != nil {
			t.Fatalf("decoding %s: %v", kind, err)
		}
		out = append(out, wrapper.Tag)
	}
	return out
}

func TestCreateCollectionReturnsABareID(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "One"})
	id := createCollection(t, s, "Favourites", s.bookID("One"))

	tag, err := store.GetTag(s.ctx, s.store.Reader(), id)
	if err != nil {
		t.Fatalf("the created collection is not in the store: %v", err)
	}
	if tag.Name != "Favourites" {
		t.Errorf("Name = %q", tag.Name)
	}
	if tag.UserID != s.userID {
		t.Errorf("UserID = %d, want %d", tag.UserID, s.userID)
	}

	members, err := store.TagBookIDs(s.ctx, s.store.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0] != s.bookID("One") {
		t.Errorf("members = %v, want the book it was created with", members)
	}
}

// A collection created on one device must reach the others, with its members.
func TestCollectionReachesAnotherDevice(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "One"},
		calibretest.BookSpec{Title: "Two"},
	)

	other := newFakeKoboAs(t, s.env, "device-two")
	other.sync() // establish a baseline so the collection shows up as new

	id := createCollection(t, s, "Favourites", s.bookID("One"), s.bookID("Two"))

	items := other.sync()
	tags := tagsIn(t, items, "NewTag")
	if len(tags) != 1 {
		t.Fatalf("the other device received %d new collections, want 1: %v", len(tags), kinds(items))
	}

	tag := tags[0]
	if tag.ID != id {
		t.Errorf("Tag.Id = %q, want %q", tag.ID, id)
	}
	if tag.Name != "Favourites" {
		t.Errorf("Tag.Name = %q", tag.Name)
	}
	if tag.Type != "UserTag" {
		t.Errorf("Tag.Type = %q, want UserTag", tag.Type)
	}
	if len(tag.Items) != 2 {
		t.Fatalf("Tag.Items has %d entries, want 2", len(tag.Items))
	}
	for _, item := range tag.Items {
		if item.Type != "ProductRevisionTagItem" {
			t.Errorf("Tag item Type = %q, want ProductRevisionTagItem", item.Type)
		}
		if item.RevisionID == "" {
			t.Error("a tag item has no RevisionId")
		}
	}
}

func TestRenameCollectionPropagates(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "One"})

	other := newFakeKoboAs(t, s.env, "device-two")
	id := createCollection(t, s, "Old Name", s.bookID("One"))
	other.sync() // it now knows the collection

	resp := s.do("PUT", s.kobo("/v1/library/tags/"+id), `{"Name":"New Name"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("rename status = %d, want 200", resp.StatusCode)
	}

	items := other.sync()
	tags := tagsIn(t, items, "ChangedTag")
	if len(tags) != 1 {
		t.Fatalf("got %d changed collections, want 1: %v", len(tags), kinds(items))
	}
	if tags[0].Name != "New Name" {
		t.Errorf("Tag.Name = %q, want the new name", tags[0].Name)
	}
}

func TestDeleteCollectionPropagates(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "One"})

	other := newFakeKoboAs(t, s.env, "device-two")
	id := createCollection(t, s, "Temporary", s.bookID("One"))
	other.sync()

	req, _ := http.NewRequest("DELETE", s.server.URL+s.kobo("/v1/library/tags/"+id), nil)
	req.Header.Set("x-kobo-deviceid", "device-abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("delete status = %d, want 200", resp.StatusCode)
	}

	items := other.sync()
	tags := tagsIn(t, items, "DeletedTag")
	if len(tags) != 1 {
		t.Fatalf("got %d deleted collections, want 1: %v", len(tags), kinds(items))
	}
	if tags[0].ID != id {
		t.Errorf("DeletedTag id = %q, want %q", tags[0].ID, id)
	}
}

// Kobo's own map spells the add-items path with a capital I --
// /v1/library/tags/{TagId}/Items -- and a device that derives the path from
// api_endpoint rather than from our map sends it that way. ServeMux is
// case-sensitive, so without the alias the book silently never joins the shelf:
// the request falls through to the unknown-endpoint handler and gets 200 {}.
func TestCapitalisedItemsPathAlsoAddsToACollection(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "One"},
		calibretest.BookSpec{Title: "Two"},
	)
	one, two := s.bookID("One"), s.bookID("Two")
	id := createCollection(t, s, "Shelf", one)

	other := newFakeKoboAs(t, s.env, "device-two")
	other.sync()

	resp := s.do("POST", s.kobo("/v1/library/tags/"+id+"/Items"),
		`{"Items":[{"RevisionId":"`+two+`","Type":"ProductRevisionTagItem"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST .../Items status = %d, want 201", resp.StatusCode)
	}

	tags := tagsIn(t, other.sync(), "ChangedTag")
	if len(tags) != 1 || len(tags[0].Items) != 2 {
		t.Fatalf("after the capitalised add the other device saw %+v, want one collection with 2 items", tags)
	}
}

// Membership changes must reach other devices too, which is why a change bumps
// the collection's revision rather than relying on comparing contents.
func TestCollectionMembershipChangesPropagate(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "One"},
		calibretest.BookSpec{Title: "Two"},
	)
	one, two := s.bookID("One"), s.bookID("Two")

	other := newFakeKoboAs(t, s.env, "device-two")
	id := createCollection(t, s, "Shelf", one)
	other.sync()

	// Add a book.
	resp := s.do("POST", s.kobo("/v1/library/tags/"+id+"/items"),
		`{"Items":[{"RevisionId":"`+two+`","Type":"ProductRevisionTagItem"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add items status = %d, want 201", resp.StatusCode)
	}

	tags := tagsIn(t, other.sync(), "ChangedTag")
	if len(tags) != 1 || len(tags[0].Items) != 2 {
		t.Fatalf("after adding a book the other device saw %+v, want one collection with 2 items", tags)
	}

	// Remove one.
	resp = s.do("POST", s.kobo("/v1/library/tags/"+id+"/items/delete"),
		`{"Items":[{"RevisionId":"`+one+`","Type":"ProductRevisionTagItem"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("remove items status = %d, want 200", resp.StatusCode)
	}

	tags = tagsIn(t, other.sync(), "ChangedTag")
	if len(tags) != 1 || len(tags[0].Items) != 1 {
		t.Fatalf("after removing a book the other device saw %+v, want one collection with 1 item", tags)
	}
	if tags[0].Items[0].RevisionID != two {
		t.Errorf("remaining member = %q, want %q", tags[0].Items[0].RevisionID, two)
	}
}

// Recreating a deleted collection by name must work: the device has no idea we
// soft-delete, and refusing would leave it unable to make the collection.
func TestRecreatingADeletedCollectionWorks(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "One"})

	first := createCollection(t, s, "Recycled", s.bookID("One"))

	req, _ := http.NewRequest("DELETE", s.server.URL+s.kobo("/v1/library/tags/"+first), nil)
	req.Header.Set("x-kobo-deviceid", "device-abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	second := createCollection(t, s, "Recycled")
	if second == "" {
		t.Fatal("recreating a deleted collection produced no id")
	}

	tag, err := store.GetTag(s.ctx, s.store.Reader(), second)
	if err != nil {
		t.Fatal(err)
	}
	if tag.DeletedAt != "" {
		t.Error("the revived collection is still marked deleted")
	}
}

// A collection must not be touchable by a device belonging to another user.
func TestCollectionsAreScopedToTheirOwner(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "One"})
	id := createCollection(t, s, "Private", s.bookID("One"))

	// A second user with their own token, on the same server.
	otherUser, err := store.CreateUser(s.ctx, s.store.Writer(), "intruder", "x", false)
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := store.CreateAPIToken(s.ctx, s.store.Writer(), otherUser, "их kobo")
	if err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("PUT",
		s.server.URL+"/kobo/"+otherToken+"/v1/library/tags/"+id,
		strings.NewReader(`{"Name":"Hijacked"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-kobo-deviceid", "intruder-device")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	tag, err := store.GetTag(s.ctx, s.store.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	if tag.Name != "Private" {
		t.Errorf("another user's device renamed the collection to %q", tag.Name)
	}
}

// A malformed request must not produce an error status; the device would give
// up on the whole sync.
func TestMalformedCollectionRequestsStayQuiet(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "One"})

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"no name", "POST", "/v1/library/tags", `{"Items":[]}`},
		{"not json", "POST", "/v1/library/tags", `<xml/>`},
		{"unknown collection", "PUT", "/v1/library/tags/nope", `{"Name":"x"}`},
		{"items for unknown collection", "POST", "/v1/library/tags/nope/items", `{"Items":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.do(tc.method, s.kobo(tc.path), tc.body)
			if resp.StatusCode >= 400 {
				t.Errorf("status = %d, want a quiet success so the device keeps syncing",
					resp.StatusCode)
			}
		})
	}
}

// Collections created before a device's first sync must arrive in that sync.
func TestNewDeviceReceivesExistingCollections(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "One"})
	createCollection(t, s, "Established", s.bookID("One"))

	fresh := newFakeKoboAs(t, s.env, "device-fresh")
	items := fresh.sync()

	tags := tagsIn(t, items, "NewTag")
	if len(tags) != 1 {
		t.Fatalf("a fresh device received %d collections, want 1: %v", len(tags), kinds(items))
	}
	if tags[0].Name != "Established" {
		t.Errorf("Tag.Name = %q", tags[0].Name)
	}
}
