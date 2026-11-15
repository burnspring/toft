package database_test

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The tests here are the ones ADR §51 exists to make true. They are not
// coverage of the group CRUD — that is elsewhere — but statements of the four
// things that would silently break authorization if they stopped holding.

func mkGroup(t *testing.T, db *database.DB, name string) *database.Group {
	t.Helper()
	g, err := db.CreateGroup(context.Background(), name, "", database.RequestMeta{})
	if err != nil {
		t.Fatalf("CreateGroup(%s): %v", name, err)
	}
	return g
}

func mkUserIn(t *testing.T, db *database.DB, id, role string, g *database.Group) *database.User {
	t.Helper()
	u := &database.User{ID: id, Login: id, SystemRole: role}
	if err := db.UpsertUser(u); err != nil {
		t.Fatalf("UpsertUser(%s): %v", id, err)
	}
	// UpsertUser deliberately does not write system_role — who you are comes
	// from the provider, what you may do is assigned here — so the role has to
	// be set separately or every fixture is a viewer, which is exactly the
	// thing these tests are trying to tell apart.
	if err := db.UpdateUserRole(id, role); err != nil {
		t.Fatalf("UpdateUserRole(%s, %s): %v", id, role, err)
	}
	if g != nil {
		if err := db.AddGroupMember(context.Background(), g.ID, id, "", database.RequestMeta{}); err != nil {
			t.Fatalf("AddGroupMember(%s): %v", id, err)
		}
	}
	return u
}

// An app is owned by a user or a group, never both and never neither. It is a
// database constraint rather than a convention, which is the only version of
// that sentence worth writing down.
func TestAppHasExactlyOneOwner(t *testing.T) {
	db := dbtest.New(t)
	g := mkGroup(t, db, "one-owner")
	mkUserIn(t, db, "oo-user", database.SystemRolePublisher, g)

	for _, c := range []struct{ name, user, group string }{
		{"neither", "NULL", "NULL"},
		{"both", "'oo-user'", "'" + g.ID + "'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := db.Conn().Exec(`
				INSERT INTO apps (id, slug, name, owner_user_id, owner_group_id, repo_url)
				VALUES ('x-` + c.name + `', 'x-` + c.name + `', 'x', ` + c.user + `, ` + c.group + `, 'https://example.test/r')`)
			if err == nil {
				t.Fatal("insert accepted an app with " + c.name + " owner set; apps_one_owner is not enforcing")
			}
			if !strings.Contains(err.Error(), "apps_one_owner") {
				t.Fatalf("refused, but not by apps_one_owner: %v", err)
			}
		})
	}
}

// Transferring to a group moves the column and the grant row together. They are
// two spellings of one fact and nothing may leave them disagreeing — a
// group-owned app is readable by its members only through that grant row.
func TestTransferOwnershipToGroup(t *testing.T) {
	db := dbtest.New(t)
	actor := mkActor(t, db, "group-owner-actor")
	app := mkApp(t, db, "to-group")
	g := mkGroup(t, db, "platform-team")
	mkUserIn(t, db, "tg-member", database.SystemRolePublisher, g)

	if err := db.TransferOwnership(context.Background(), app.ID,
		database.PrincipalTypeGroup, g.ID, actor, database.RequestMeta{}); err != nil {
		t.Fatalf("TransferOwnership: %v", err)
	}

	after, err := db.GetApp(app.ID)
	if err != nil || after == nil {
		t.Fatalf("GetApp: %v", err)
	}
	if after.OwnerID != "" {
		t.Errorf("owner_user_id = %q, want empty once a group owns it", after.OwnerID)
	}
	if after.OwnerGroupID != g.ID {
		t.Errorf("owner_group_id = %q, want %q", after.OwnerGroupID, g.ID)
	}
	if !after.GroupOwned() {
		t.Error("GroupOwned() = false")
	}
	if ptype, pid := after.OwnerPrincipal(); ptype != database.PrincipalTypeGroup || pid != g.ID {
		t.Errorf("OwnerPrincipal() = (%s, %s), want (group, %s)", ptype, pid, g.ID)
	}

	// The grant row agrees, and the member resolves to owner through it.
	role, err := db.EffectiveRole(app.ID, database.PrincipalTypeUser, "tg-member", []string{g.ID})
	if err != nil {
		t.Fatalf("EffectiveRole: %v", err)
	}
	if role != database.ObjectRoleOwner {
		t.Fatalf("member's role = %q, want owner", role)
	}

	// The previous owner is demoted rather than cut off.
	prev, err := db.EffectiveRole(app.ID, database.PrincipalTypeUser, app.OwnerID, nil)
	if err != nil {
		t.Fatalf("EffectiveRole(previous owner): %v", err)
	}
	if prev != database.ObjectRoleEditor {
		t.Fatalf("previous owner's role = %q, want editor", prev)
	}

	// And back again, so the move is not one-way.
	if err := db.TransferOwnership(context.Background(), app.ID,
		database.PrincipalTypeUser, app.OwnerID, actor, database.RequestMeta{}); err != nil {
		t.Fatalf("TransferOwnership back to a user: %v", err)
	}
	back, _ := db.GetApp(app.ID)
	if back.OwnerID != app.OwnerID || back.OwnerGroupID != "" {
		t.Fatalf("after transfer back: owner_user_id=%q owner_group_id=%q", back.OwnerID, back.OwnerGroupID)
	}
}

// The payoff, stated as the acceptance test it is (§9.4): deleting someone who
// owns nothing personally succeeds, even while apps they can reach through an
// owning group still exist.
func TestDeletingAUserWhoOwnsOnlyThroughAGroupSucceeds(t *testing.T) {
	db := dbtest.New(t)
	actor := mkActor(t, db, "group-owner-actor")
	app := mkApp(t, db, "leaver")
	g := mkGroup(t, db, "leaver-team")
	leaver := mkUserIn(t, db, "leaver-admin", database.SystemRolePublisher, g)
	mkUserIn(t, db, "leaver-stays", database.SystemRolePublisher, g)

	if err := db.TransferOwnership(context.Background(), app.ID,
		database.PrincipalTypeGroup, g.ID, actor, database.RequestMeta{}); err != nil {
		t.Fatalf("TransferOwnership: %v", err)
	}

	// They hold owner on it, live, through the group.
	role, _ := db.EffectiveRole(app.ID, database.PrincipalTypeUser, leaver.ID, []string{g.ID})
	if role != database.ObjectRoleOwner {
		t.Fatalf("before deletion the leaver's role = %q, want owner", role)
	}
	// And personally own nothing.
	owned, err := db.ListAppsOwnedBy(leaver.ID)
	if err != nil {
		t.Fatalf("ListAppsOwnedBy: %v", err)
	}
	if len(owned) != 0 {
		t.Fatalf("personally owns %d apps, want 0", len(owned))
	}

	if err := db.DeleteUser(leaver.ID, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: actor,
		Action: "user.delete", TargetType: "user", TargetID: leaver.ID,
	}); err != nil {
		t.Fatalf("DeleteUser: %v — this is the whole point of the feature", err)
	}

	// The app is untouched and still owned.
	after, _ := db.GetApp(app.ID)
	if after.OwnerGroupID != g.ID {
		t.Fatalf("after the deletion owner_group_id = %q, want %q", after.OwnerGroupID, g.ID)
	}
}

// Personal ownership still blocks deletion. The unblock in §9.4 is narrow and
// this is the half of it that must not have moved.
func TestDeletingAUserWhoOwnsAppsPersonallyIsStillRefused(t *testing.T) {
	db := dbtest.New(t)
	actor := mkActor(t, db, "group-owner-actor")
	app := mkApp(t, db, "still-blocked")

	err := db.DeleteUser(app.OwnerID, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: actor,
		Action: "user.delete", TargetType: "user", TargetID: app.OwnerID,
	})
	if !errors.Is(err, database.ErrUserOwnsApps) {
		t.Fatalf("DeleteUser = %v, want ErrUserOwnsApps", err)
	}
}

// §9.1 and §9.2 — the two ways an owning group could be left unable to
// administer what it owns.
func TestAnOwningGroupCannotBeDeletedOrEmptied(t *testing.T) {
	db := dbtest.New(t)
	actor := mkActor(t, db, "group-owner-actor")
	app := mkApp(t, db, "owned-by-group")
	g := mkGroup(t, db, "owning-team")
	mkUserIn(t, db, "og-one", database.SystemRolePublisher, g)
	mkUserIn(t, db, "og-two", database.SystemRolePublisher, g)

	if err := db.TransferOwnership(context.Background(), app.ID,
		database.PrincipalTypeGroup, g.ID, actor, database.RequestMeta{}); err != nil {
		t.Fatalf("TransferOwnership: %v", err)
	}

	if err := db.DeleteGroup(context.Background(), g.ID, actor, database.RequestMeta{}); !errors.Is(err, database.ErrGroupOwnsApps) {
		t.Fatalf("DeleteGroup = %v, want ErrGroupOwnsApps", err)
	}

	// Two eligible members: removing one is fine.
	if err := db.RemoveGroupMember(context.Background(), g.ID, "og-two", actor, database.RequestMeta{}); err != nil {
		t.Fatalf("removing the second-to-last member: %v", err)
	}
	// One left: removing them is refused.
	if err := db.RemoveGroupMember(context.Background(), g.ID, "og-one", actor, database.RequestMeta{}); !errors.Is(err, database.ErrGroupWouldBeEmpty) {
		t.Fatalf("removing the last eligible member = %v, want ErrGroupWouldBeEmpty", err)
	}

	// A locked member and a system viewer are members who confer nothing, so
	// neither rescues the group from being empty in the sense that matters.
	mkUserIn(t, db, "og-viewer", database.SystemRoleViewer, g)
	if err := db.RemoveGroupMember(context.Background(), g.ID, "og-one", actor, database.RequestMeta{}); !errors.Is(err, database.ErrGroupWouldBeEmpty) {
		t.Fatalf("with only a system viewer left = %v, want ErrGroupWouldBeEmpty", err)
	}
	n, err := db.CountEligibleGroupMembers(g.ID, "")
	if err != nil {
		t.Fatalf("CountEligibleGroupMembers: %v", err)
	}
	if n != 1 {
		t.Fatalf("eligible members = %d, want 1 (the system viewer does not count)", n)
	}

	// Once it owns nothing, both refusals lift.
	if err := db.TransferOwnership(context.Background(), app.ID,
		database.PrincipalTypeUser, app.OwnerID, actor, database.RequestMeta{}); err != nil {
		t.Fatalf("transferring away: %v", err)
	}
	if err := db.RemoveGroupMember(context.Background(), g.ID, "og-one", actor, database.RequestMeta{}); err != nil {
		t.Fatalf("removing the last member of a group that owns nothing: %v", err)
	}
	if err := db.DeleteGroup(context.Background(), g.ID, actor, database.RequestMeta{}); err != nil {
		t.Fatalf("deleting a group that owns nothing: %v", err)
	}
}

// Removing a member revokes owner on every app the group owns, and does it on
// the member's *next request* rather than at token expiry.
//
// Two halves, and both matter. The resolver stops returning owner immediately,
// which is this package's half. The other half is token_version: the middleware
// re-checks it on every authenticated request, so the session carrying the old
// group claims dies at once. That mechanism predates group ownership and was
// written when a group could only confer viewer — it is now what makes
// offboarding real, which is why it is asserted by name here.
func TestRemovingAMemberRevokesOwnerOnTheGroupsApps(t *testing.T) {
	db := dbtest.New(t)
	actor := mkActor(t, db, "group-owner-actor")
	app := mkApp(t, db, "revoke")
	g := mkGroup(t, db, "revoke-team")
	mkUserIn(t, db, "rv-leaver", database.SystemRolePublisher, g)
	mkUserIn(t, db, "rv-stays", database.SystemRolePublisher, g)

	if err := db.TransferOwnership(context.Background(), app.ID,
		database.PrincipalTypeGroup, g.ID, actor, database.RequestMeta{}); err != nil {
		t.Fatalf("TransferOwnership: %v", err)
	}

	role, _ := db.EffectiveRole(app.ID, database.PrincipalTypeUser, "rv-leaver", []string{g.ID})
	if role != database.ObjectRoleOwner {
		t.Fatalf("role before removal = %q, want owner", role)
	}

	before, err := db.GetTokenVersion("rv-leaver")
	if err != nil {
		t.Fatalf("GetTokenVersion: %v", err)
	}
	if err := db.RemoveGroupMember(context.Background(), g.ID, "rv-leaver", actor, database.RequestMeta{}); err != nil {
		t.Fatalf("RemoveGroupMember: %v", err)
	}
	after, err := db.GetTokenVersion("rv-leaver")
	if err != nil {
		t.Fatalf("GetTokenVersion: %v", err)
	}
	if after <= before {
		t.Fatalf("token_version %d -> %d: outstanding sessions would keep owner until they expire",
			before, after)
	}

	// The membership is gone, so the next request carries no group claim.
	groups, err := db.GetUserGroupIDs("rv-leaver")
	if err != nil {
		t.Fatalf("GetUserGroupIDs: %v", err)
	}
	role, _ = db.EffectiveRole(app.ID, database.PrincipalTypeUser, "rv-leaver", groups)
	if role == database.ObjectRoleOwner {
		t.Fatal("still owner after removal")
	}

	// And the member who stayed is unaffected.
	role, _ = db.EffectiveRole(app.ID, database.PrincipalTypeUser, "rv-stays", []string{g.ID})
	if role != database.ObjectRoleOwner {
		t.Fatalf("remaining member's role = %q, want owner", role)
	}
}

// A group is a set of humans, never an actor. Every audit row names the person
// who acted; a group appears as a target or in a diff and never in actor_id.
func TestNoAuditRowNamesAGroupAsActor(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "audit-actor")
	g := mkGroup(t, db, "audit-team")
	mkUserIn(t, db, "aa-member", database.SystemRolePublisher, g)
	mkUserIn(t, db, "aa-second", database.SystemRolePublisher, nil)

	// Exercise every write this feature adds that touches a group. The actor on
	// each is a person, which is the whole assertion — there is no call shape
	// that would let a group be passed here instead.
	if err := db.TransferOwnership(context.Background(), app.ID,
		database.PrincipalTypeGroup, g.ID, "aa-member", database.RequestMeta{}); err != nil {
		t.Fatalf("TransferOwnership: %v", err)
	}
	if err := db.SetGroupEmail(context.Background(), g.ID, "team@example.test", "aa-member", database.RequestMeta{}); err != nil {
		t.Fatalf("SetGroupEmail: %v", err)
	}
	if err := db.AddGroupMember(context.Background(), g.ID, "aa-second", "aa-member", database.RequestMeta{}); err != nil {
		t.Fatalf("AddGroupMember: %v", err)
	}
	if err := db.RemoveGroupMember(context.Background(), g.ID, "aa-second", "aa-member", database.RequestMeta{}); err != nil {
		t.Fatalf("RemoveGroupMember: %v", err)
	}

	rows, err := db.Conn().Query(`SELECT actor_type, COALESCE(actor_id,'') FROM audit_log`)
	if err != nil {
		t.Fatalf("reading audit_log: %v", err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var actorType, actorID string
		if err := rows.Scan(&actorType, &actorID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if actorType == database.PrincipalTypeGroup {
			t.Errorf("audit row has actor_type=group — a group is not an actor")
		}
		if actorID == g.ID {
			t.Errorf("audit row has actor_id = the group's id (%s) — a group is not an actor", g.ID)
		}
	}
	if n == 0 {
		t.Fatal("no audit rows were written at all; the assertion above proved nothing")
	}
}

// Where a group-owned app's failure mail goes: the group's address if it has
// one, its members if it does not, and never both.
//
// The fallback is the safe direction — a group that has not thought about an
// address still gets told — and the exclusivity is the one that keeps a
// thirty-person team from getting thirty-one copies of every failure.
func TestRecipientsForAGroupOwnedApp(t *testing.T) {
	db := dbtest.New(t)
	actor := mkActor(t, db, "group-owner-actor")
	app := mkApp(t, db, "notified")
	g := mkGroup(t, db, "on-call")

	withEmail := func(id, email, role string) {
		t.Helper()
		u := &database.User{ID: id, Login: id, Email: email}
		if err := db.UpsertUser(u); err != nil {
			t.Fatalf("UpsertUser(%s): %v", id, err)
		}
		if err := db.UpdateUserRole(id, role); err != nil {
			t.Fatalf("UpdateUserRole(%s): %v", id, err)
		}
		if err := db.AddGroupMember(context.Background(), g.ID, id, actor, database.RequestMeta{}); err != nil {
			t.Fatalf("AddGroupMember(%s): %v", id, err)
		}
	}
	withEmail("rec-one", "one@example.test", database.SystemRolePublisher)
	withEmail("rec-two", "two@example.test", database.SystemRolePublisher)
	// Neither of these can act on a failure, so neither is told.
	withEmail("rec-locked", "locked@example.test", database.SystemRolePublisher)
	withEmail("rec-viewer", "viewer@example.test", database.SystemRoleViewer)
	if _, err := db.Conn().Exec(`UPDATE users SET locked_at=NOW() WHERE id='rec-locked'`); err != nil {
		t.Fatalf("locking: %v", err)
	}

	if err := db.TransferOwnership(context.Background(), app.ID,
		database.PrincipalTypeGroup, g.ID, actor, database.RequestMeta{}); err != nil {
		t.Fatalf("TransferOwnership: %v", err)
	}

	addresses := func() []string {
		t.Helper()
		got, err := db.ListAppRecipients(app.ID)
		if err != nil {
			t.Fatalf("ListAppRecipients: %v", err)
		}
		var out []string
		for _, r := range got {
			if r.Address == "" {
				t.Errorf("a recipient with no address: %+v", r)
			}
			out = append(out, r.Address)
		}
		sort.Strings(out)
		return out
	}

	// No address on the group: the eligible members, and only those.
	got := addresses()
	want := []string{"one@example.test", "two@example.test"}
	if !slices.Equal(got, want) {
		t.Fatalf("with no group address, recipients = %v, want %v", got, want)
	}

	// An address on the group: the address *instead of* the members.
	if err := db.SetGroupEmail(context.Background(), g.ID, "on-call@example.test", actor, database.RequestMeta{}); err != nil {
		t.Fatalf("SetGroupEmail: %v", err)
	}
	got = addresses()
	if !slices.Equal(got, []string{"on-call@example.test"}) {
		t.Fatalf("with a group address, recipients = %v, want just the group's", got)
	}
	// And it is not a person.
	all, _ := db.ListAppRecipients(app.ID)
	if all[0].UserID != nil {
		t.Errorf("the group's address came back attributed to a user: %+v", all[0])
	}
	if all[0].DisplayName != "on-call" {
		t.Errorf("display name = %q, want the group's name", all[0].DisplayName)
	}

	// Clearing it puts the members back rather than silencing the app.
	if err := db.SetGroupEmail(context.Background(), g.ID, "", actor, database.RequestMeta{}); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if got := addresses(); !slices.Equal(got, want) {
		t.Fatalf("after clearing, recipients = %v, want %v", got, want)
	}
}

// Two transfers of one app at once leave one owner: the second reads the first
// one's target as the owner to demote, not the original owner both saw before
// (security review 2026-11-14, DB-9).
func TestTransferOwnershipTwiceAtOnceLeavesOneOwner(t *testing.T) {
	db := dbtest.New(t)
	actor := mkActor(t, db, "double-transfer-actor")
	app := mkApp(t, db, "double-transfer")
	mkUserIn(t, db, "dt-x", database.SystemRolePublisher, nil)
	mkUserIn(t, db, "dt-y", database.SystemRolePublisher, nil)

	hold, err := db.Conn().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(`SELECT 1 FROM apps WHERE id=? FOR UPDATE`, app.ID); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, to := range []string{"dt-x", "dt-y"} {
		go func(to string) {
			errs <- db.TransferOwnership(context.Background(), app.ID, database.PrincipalTypeUser, to, actor, database.RequestMeta{})
		}(to)
	}
	time.Sleep(300 * time.Millisecond)
	hold.Rollback()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("transfer: %v", err)
		}
	}

	after, _ := db.GetApp(app.ID)
	rows, err := db.Conn().Query(`SELECT principal_type, principal_id FROM object_grants WHERE object_id=? AND role=?`,
		app.ID, database.ObjectRoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var pt, pid string
		rows.Scan(&pt, &pid)
		owners = append(owners, pt+":"+pid)
	}
	if len(owners) != 1 || owners[0] != database.PrincipalTypeUser+":"+after.OwnerID {
		t.Errorf("owner grants = %v, apps.owner_user_id = %q; want exactly that one", owners, after.OwnerID)
	}
}
