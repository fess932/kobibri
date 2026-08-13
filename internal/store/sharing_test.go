package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fess932/kobibri/internal/store"
)

// A server shared with family is what this is for, and until now the table
// existed and nothing ever wrote to it: every library was visible to everyone
// whatever the interface implied.
func TestSharingALibraryWithSomePeople(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "kobibri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	alice, err := store.CreateUser(ctx, st.Writer(), "alice", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.CreateUser(ctx, st.Writer(), "bob", "x", false)
	if err != nil {
		t.Fatal(err)
	}

	id, err := store.CreateSource(ctx, st.Writer(), &store.Source{
		Name: "hers", LibraryPath: t.TempDir(), Enabled: true, ShareAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SetSourceSharing(ctx, st.Writer(), id, false, []int64{alice}); err != nil {
		t.Fatal(err)
	}
	acl, err := store.SourceACL(ctx, st.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(acl) != 1 || acl[0] != alice {
		t.Fatalf("shared with %v, want just alice (%d)", acl, alice)
	}

	src, err := store.GetSource(ctx, st.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	if src.ShareAll {
		t.Error("the library still says it is shared with everyone")
	}

	// Sharing with everyone again clears the list rather than leaving it to
	// contradict the flag.
	if err := store.SetSourceSharing(ctx, st.Writer(), id, true, []int64{alice, bob}); err != nil {
		t.Fatal(err)
	}
	if acl, _ := store.SourceACL(ctx, st.Reader(), id); len(acl) != 0 {
		t.Errorf("a library shared with everyone still lists %v", acl)
	}
}

// Restricting a library to nobody would hide it from its owner too, which is
// never what anyone means by the button they just pressed.
func TestALibraryCannotBeSharedWithNobody(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "kobibri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, err := store.CreateSource(ctx, st.Writer(), &store.Source{
		Name: "orphan", LibraryPath: t.TempDir(), Enabled: true, ShareAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SetSourceSharing(ctx, st.Writer(), id, false, nil); err != nil {
		t.Fatal(err)
	}
	src, err := store.GetSource(ctx, st.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !src.ShareAll {
		t.Error("a library restricted to nobody is invisible to everyone, including whoever did it")
	}
}
