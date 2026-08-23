package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fess932/kobibri/internal/store"
)

// One reader must be one row, however many of its requests arrive without a
// device id.
//
// A Kobo does not send x-kobo-deviceid on the requests that open a session —
// /v1/auth/device, /v1/affiliate, /v1/initialization — and only starts once the
// sync proper begins. Filing those under an id of "" put every reader in the
// devices list twice: once as itself, and once as a nameless row that had never
// synced and could never be addressed.
func TestOneReaderStaysOneDeviceRow(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "kobibri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	userID, err := store.CreateUser(ctx, st.Writer(), "reader", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.CreateAPIToken(ctx, st.Writer(), userID, "Kobo Libra Colour")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.LookupAPIToken(ctx, st.Reader(), raw)
	if err != nil {
		t.Fatal(err)
	}

	// The session opens with no device id, exactly as a real one does.
	first, err := store.UpsertDevice(ctx, st.Writer(), store.DeviceIdentity{
		TokenHash: tok.TokenHash, UserID: userID, Firmware: "4.45.23697",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Then the reader starts sending it.
	second, err := store.UpsertDevice(ctx, st.Writer(), store.DeviceIdentity{
		TokenHash: tok.TokenHash, UserID: userID,
		KoboDeviceID: "6eebe0eb445a2a46b47e8b98e435d99f0b702c11cc05773c8cb778e7b4c57a39",
		Model:        "Kobo Libra Colour", Firmware: "4.45.23697",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Errorf("the reader got two rows, %d and %d: the nameless one was left behind "+
			"instead of being named", first.ID, second.ID)
	}

	// And a later headerless request must not open a third.
	third, err := store.UpsertDevice(ctx, st.Writer(), store.DeviceIdentity{
		TokenHash: tok.TokenHash, UserID: userID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != second.ID {
		t.Errorf("a request without a device id opened row %d beside %d", third.ID, second.ID)
	}

	devices, err := store.ListDevices(ctx, st.Reader(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("%d devices, want 1", len(devices))
	}
	if devices[0].Model != "Kobo Libra Colour" {
		t.Errorf("model = %q, want the one the reader reported", devices[0].Model)
	}
	if devices[0].KoboDeviceID == "" {
		t.Error("the surviving row has no device id, so it can never be addressed")
	}
}

// Two readers sharing one token are still two readers: the fix for the nameless
// duplicate must not collapse them.
func TestTwoReadersOnOneTokenStayApart(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "kobibri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	userID, err := store.CreateUser(ctx, st.Writer(), "reader", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.CreateAPIToken(ctx, st.Writer(), userID, "shared")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.LookupAPIToken(ctx, st.Reader(), raw)
	if err != nil {
		t.Fatal(err)
	}

	one, err := store.UpsertDevice(ctx, st.Writer(), store.DeviceIdentity{
		TokenHash: tok.TokenHash, UserID: userID, KoboDeviceID: "aaaa", Model: "Libra"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := store.UpsertDevice(ctx, st.Writer(), store.DeviceIdentity{
		TokenHash: tok.TokenHash, UserID: userID, KoboDeviceID: "bbbb", Model: "Clara"})
	if err != nil {
		t.Fatal(err)
	}
	if one.ID == two.ID {
		t.Fatal("two readers sharing a token were merged into one row")
	}

	devices, err := store.ListDevices(ctx, st.Reader(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("%d devices, want 2", len(devices))
	}
}
