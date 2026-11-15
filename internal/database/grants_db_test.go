package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// A group resolves to the role its grant row stores, for every role.
//
// This test used to assert the opposite — every group-derived role clamped down
// to viewer — and it is kept, inverted, rather than deleted, because the change
// is the security-relevant one in ADR §51 and the file should say so at the
// place a reader will look for it. What made the clamp acceptable to remove is
// that groups here are local rows an administrator manages, never claims from a
// directory.
//
// The clamp that survives is a different rule that happened to sit beside it: a
// user whose *system* role is viewer never holds owner or editor. It is
// in auth.EffectiveObjectRole, it applies to direct grants too, and
// TestSystemViewerInOwningGroupIsStillAViewer is where it is stated.
func TestGroupGrantResolvesToItsStoredRole(t *testing.T) {
	db := dbtest.New(t)

	for _, role := range []string{
		database.ObjectRoleViewer,
		database.ObjectRoleEditor,
		database.ObjectRoleOwner,
	} {
		t.Run(role, func(t *testing.T) {
			app := mkApp(t, db, "stored-"+role)
			if _, err := db.Conn().Exec(`
				INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by)
				VALUES (?, 'group', 'g1', ?, NULL)
			`, app.ID, role); err != nil {
				t.Fatalf("inserting group grant: %v", err)
			}
			got, err := db.EffectiveRole(app.ID, database.PrincipalTypeUser, "member", []string{"g1"})
			if err != nil {
				t.Fatalf("EffectiveRole: %v", err)
			}
			if got != role {
				t.Fatalf("group-derived role = %q, want %q", got, role)
			}
		})
	}

	// A direct user grant still wins when it is higher, and loses when it is
	// not: precedence, not principal type, decides.
	app := mkApp(t, db, "precedence")
	if _, err := db.Conn().Exec(`
		INSERT INTO object_grants (object_id, principal_type, principal_id, role, granted_by)
		VALUES (?, 'group', 'g1', 'viewer', NULL), (?, 'user', 'member', 'editor', NULL)
	`, app.ID, app.ID); err != nil {
		t.Fatalf("inserting grants: %v", err)
	}
	role, err := db.EffectiveRole(app.ID, database.PrincipalTypeUser, "member", []string{"g1"})
	if err != nil {
		t.Fatalf("EffectiveRole: %v", err)
	}
	if role != database.ObjectRoleEditor {
		t.Fatalf("role = %q, want editor", role)
	}
}
