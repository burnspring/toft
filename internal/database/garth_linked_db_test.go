package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// A deploy from Garth links the app once — later ones keep the first time —
// and unlinking clears it and is audited (ADR §158).
func TestGarthLinkIsMarkedOnceAndUnlinked(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "garth-linked")
	if got, _ := db.GetApp(app.ID); got.GarthLinkedAt != nil {
		t.Fatalf("a new app is linked to Garth: %v", got.GarthLinkedAt)
	}
	if err := db.MarkAppGarthLinked(app.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := db.GetApp(app.ID)
	if first.GarthLinkedAt == nil {
		t.Fatal("a deploy from Garth did not link the app")
	}
	if err := db.MarkAppGarthLinked(app.ID); err != nil {
		t.Fatal(err)
	}
	if again, _ := db.GetApp(app.ID); !again.GarthLinkedAt.Equal(*first.GarthLinkedAt) {
		t.Fatalf("a second deploy moved the link's time: %v -> %v", first.GarthLinkedAt, again.GarthLinkedAt)
	}
	if err := db.UnlinkAppFromGarth(app.ID, database.PrincipalTypeUser, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetApp(app.ID); got.GarthLinkedAt != nil {
		t.Fatalf("unlinked app still linked: %v", got.GarthLinkedAt)
	}
	var n int
	if err := db.Conn().QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'app.garth.unlink' AND target_id = ?`, app.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unlink audited %d times (%v)", n, err)
	}
	if err := db.UnlinkAppFromGarth("00000000-0000-0000-0000-000000000000", database.PrincipalTypeUser, database.BuiltinAdminID, "", "", ""); err == nil {
		t.Fatal("unlinking a missing app succeeded")
	}
}
