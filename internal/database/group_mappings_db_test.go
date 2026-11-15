package database_test

import (
	"context"
	"errors"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The reconciler's tests, and the first three lead because their failure would
// be **silent** — no error, no refusal, nothing on a screen. Each of them is a
// way this feature could quietly destroy authorization on a working install.

func mapClaim(t *testing.T, db *database.DB, groupID, providerID, value string) {
	t.Helper()
	if err := db.AddGroupClaimMapping(context.Background(), groupID, providerID, value, "builtin-admin", database.RequestMeta{}); err != nil {
		t.Fatalf("AddGroupClaimMapping(%s, %s, %s): %v", groupID, providerID, value, err)
	}
}

func memberGroupIDs(t *testing.T, db *database.DB, userID string) []string {
	t.Helper()
	ids, err := db.GetUserGroupIDs(userID)
	if err != nil {
		t.Fatalf("GetUserGroupIDs(%s): %v", userID, err)
	}
	return ids
}

func reconcile(t *testing.T, db *database.DB, in database.DirectorySync) database.DirectorySyncResult {
	t.Helper()
	res, err := db.ReconcileDirectoryMemberships(context.Background(), in, database.RequestMeta{})
	if err != nil {
		t.Fatalf("ReconcileDirectoryMemberships: %v", err)
	}
	return res
}

// SILENT #1. Entra omits the groups claim entirely once somebody is in more
// groups than it will assert, and Okta can be configured to omit it per
// application. If absent read as "removed from everything", a configuration
// change nobody made in this product would strip every directory membership on
// the install, for everybody, on their next sign-in — with no error anywhere and
// an audit log full of ordinary-looking removals.
func TestAnAbsentClaimRemovesNothingAndAnEmptyOneRemovesEverything(t *testing.T) {
	db := dbtest.New(t)
	g := mkGroup(t, db, "payments")
	u := mkUserIn(t, db, "alice", database.SystemRoleViewer, nil)
	mapClaim(t, db, g.ID, "sso", "payments-team")

	reconcile(t, db, database.DirectorySync{
		UserID: u.ID, ProviderID: "sso", Claimed: []string{"payments-team"}, Asserted: true,
	})
	if got := memberGroupIDs(t, db, u.ID); len(got) != 1 {
		t.Fatalf("membership after the first sync = %v, want one group", got)
	}

	// The claim did not arrive. Nothing may move.
	res := reconcile(t, db, database.DirectorySync{UserID: u.ID, ProviderID: "sso", Asserted: false})
	if res.Changed() {
		t.Errorf("an absent claim changed membership: %+v", res)
	}
	if got := memberGroupIDs(t, db, u.ID); len(got) != 1 {
		t.Fatalf("an absent claim removed a membership: %v", got)
	}

	// The claim arrived and said nothing. That is an assertion, and it removes.
	res = reconcile(t, db, database.DirectorySync{
		UserID: u.ID, ProviderID: "sso", Claimed: nil, Asserted: true,
	})
	if len(res.Removed) != 1 {
		t.Errorf("an empty claim removed %v, want the one membership", res.Removed)
	}
	if got := memberGroupIDs(t, db, u.ID); len(got) != 0 {
		t.Errorf("membership after an empty claim = %v, want none", got)
	}
}

// SILENT #2. With two providers configured, a reconciler narrowing only on
// `source = 'directory'` would delete the other provider's memberships every
// time somebody signed in — slowly, one sign-in at a time, with an audit trail
// that looks exactly like ordinary sync.
func TestASyncNeverTouchesAnotherProvidersMemberships(t *testing.T) {
	db := dbtest.New(t)
	fromA := mkGroup(t, db, "from-okta")
	fromB := mkGroup(t, db, "from-entra")
	u := mkUserIn(t, db, "alice", database.SystemRoleViewer, nil)
	mapClaim(t, db, fromA.ID, "okta", "team")
	mapClaim(t, db, fromB.ID, "entra", "team")

	reconcile(t, db, database.DirectorySync{UserID: u.ID, ProviderID: "okta", Claimed: []string{"team"}, Asserted: true})
	reconcile(t, db, database.DirectorySync{UserID: u.ID, ProviderID: "entra", Claimed: []string{"team"}, Asserted: true})
	if got := memberGroupIDs(t, db, u.ID); len(got) != 2 {
		t.Fatalf("membership after both providers = %v, want two groups", got)
	}

	// Okta now says nothing about this person. Entra's row is not Okta's to
	// remove.
	res := reconcile(t, db, database.DirectorySync{UserID: u.ID, ProviderID: "okta", Claimed: nil, Asserted: true})
	if len(res.Removed) != 1 || res.Removed[0] != fromA.ID {
		t.Errorf("removed %v, want only okta's group %s", res.Removed, fromA.ID)
	}
	got := memberGroupIDs(t, db, u.ID)
	if len(got) != 1 || got[0] != fromB.ID {
		t.Errorf("membership = %v, want entra's group %s to survive", got, fromB.ID)
	}
}

// SILENT #3. A membership write bumps token_version, which ends every other
// session the person holds. If a no-change sign-in wrote one anyway, signing in
// on a laptop would silently sign you out on your phone, every time, forever —
// which reads as a session bug and would be chased as one.
func TestANoChangeSyncWritesNothing(t *testing.T) {
	db := dbtest.New(t)
	g := mkGroup(t, db, "payments")
	u := mkUserIn(t, db, "alice", database.SystemRoleViewer, nil)
	mapClaim(t, db, g.ID, "sso", "payments-team")

	in := database.DirectorySync{UserID: u.ID, ProviderID: "sso", Claimed: []string{"payments-team"}, Asserted: true}
	reconcile(t, db, in)

	before, err := db.GetTokenVersion(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	res := reconcile(t, db, in)
	if res.Changed() {
		t.Errorf("a repeat of the same claim changed something: %+v", res)
	}
	after, err := db.GetTokenVersion(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("token version moved from %d to %d on a no-change sync — every sign-in "+
			"would end the person's other sessions", before, after)
	}
}

// A membership an administrator wrote by hand is an assertion the directory does
// not know about. The sync may not take ownership of it, and may not remove it
// when the directory stops mentioning that person.
func TestAManualMembershipSurvivesEverySync(t *testing.T) {
	db := dbtest.New(t)
	g := mkGroup(t, db, "payments")
	u := mkUserIn(t, db, "contractor", database.SystemRoleViewer, nil)
	if err := db.AddGroupMember(context.Background(), g.ID, u.ID, "builtin-admin", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	mapClaim(t, db, g.ID, "sso", "payments-team")

	// The directory asserts the same membership: the row stays the
	// administrator's, and nothing is written.
	res := reconcile(t, db, database.DirectorySync{
		UserID: u.ID, ProviderID: "sso", Claimed: []string{"payments-team"}, Asserted: true,
	})
	if res.Changed() {
		t.Errorf("a claim matching a manual membership wrote something: %+v", res)
	}

	// The directory stops asserting it. The administrator's row stands.
	res = reconcile(t, db, database.DirectorySync{UserID: u.ID, ProviderID: "sso", Claimed: nil, Asserted: true})
	if len(res.Removed) != 0 {
		t.Errorf("a sync removed a manual membership: %v", res.Removed)
	}
	if got := memberGroupIDs(t, db, u.ID); len(got) != 1 {
		t.Errorf("membership = %v, want the manual one to survive", got)
	}
}

// The cap is a trigger that RAISEs, and a RAISE aborts its transaction. The
// reconciler counts first so the eleventh group is a recorded refusal rather
// than a failed sign-in.
func TestTheEleventhGroupIsRefusedAndRecorded(t *testing.T) {
	db := dbtest.New(t)
	u := mkUserIn(t, db, "alice", database.SystemRoleViewer, nil)

	claims := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		g := mkGroup(t, db, "team-"+string(rune('a'+i)))
		value := "claim-" + string(rune('a'+i))
		mapClaim(t, db, g.ID, "sso", value)
		claims = append(claims, value)
	}

	res := reconcile(t, db, database.DirectorySync{
		UserID: u.ID, ProviderID: "sso", Claimed: claims, Asserted: true,
	})
	if len(res.Added) != database.GroupMembershipCap {
		t.Errorf("added %d groups, want the cap of %d", len(res.Added), database.GroupMembershipCap)
	}
	if len(res.Capped) != 12-database.GroupMembershipCap {
		t.Errorf("capped %v, want the %d that did not fit", res.Capped, 12-database.GroupMembershipCap)
	}
	if got := memberGroupIDs(t, db, u.ID); len(got) != database.GroupMembershipCap {
		t.Errorf("membership = %d groups, want %d", len(got), database.GroupMembershipCap)
	}
}

// Removals free slots, so the order inside one reconcile decides whether a
// person at the cap can move between groups at all. Removing first is what makes
// a lateral move work; adding first would refuse it and record a cap that was
// never really reached.
func TestAMoveAtTheCapSucceedsBecauseRemovalsComeFirst(t *testing.T) {
	db := dbtest.New(t)
	u := mkUserIn(t, db, "alice", database.SystemRoleViewer, nil)

	held := make([]string, 0, database.GroupMembershipCap)
	for i := 0; i < database.GroupMembershipCap; i++ {
		g := mkGroup(t, db, "team-"+string(rune('a'+i)))
		value := "claim-" + string(rune('a'+i))
		mapClaim(t, db, g.ID, "sso", value)
		held = append(held, value)
	}
	reconcile(t, db, database.DirectorySync{UserID: u.ID, ProviderID: "sso", Claimed: held, Asserted: true})

	moved := mkGroup(t, db, "team-new")
	mapClaim(t, db, moved.ID, "sso", "claim-new")
	next := append([]string{"claim-new"}, held[1:]...) // drop the first, add one

	res := reconcile(t, db, database.DirectorySync{UserID: u.ID, ProviderID: "sso", Claimed: next, Asserted: true})
	if len(res.Capped) != 0 {
		t.Errorf("a lateral move at the cap was refused: %v", res.Capped)
	}
	if len(res.Added) != 1 || len(res.Removed) != 1 {
		t.Errorf("added %v removed %v, want one of each", res.Added, res.Removed)
	}
}

// A claim value nobody mapped is ignored in silence — no group, no row, no
// error. This is what keeps an install with 400 AD groups from acquiring 400
// rows nobody curated.
func TestAnUnmappedClaimValueIsIgnored(t *testing.T) {
	db := dbtest.New(t)
	u := mkUserIn(t, db, "alice", database.SystemRoleViewer, nil)

	res := reconcile(t, db, database.DirectorySync{
		UserID: u.ID, ProviderID: "sso",
		Claimed: []string{"some-ad-group", "another-one"}, Asserted: true,
	})
	if res.Changed() || len(res.Capped) != 0 {
		t.Errorf("unmapped claim values did something: %+v", res)
	}
	if got := memberGroupIDs(t, db, u.ID); len(got) != 0 {
		t.Errorf("membership = %v, want none", got)
	}
}

// Removing a mapping takes the memberships it wrote with it, now, in one
// transaction. The alternative is those rows being deleted one person at a time
// over the following weeks as each of them happens to sign in, and a revocation
// spread over arbitrary sign-ins is not a revocation.
func TestUnmappingRemovesWhatItWroteAndNothingElse(t *testing.T) {
	db := dbtest.New(t)
	g := mkGroup(t, db, "payments")
	synced := mkUserIn(t, db, "alice", database.SystemRoleViewer, nil)
	manual := mkUserIn(t, db, "contractor", database.SystemRoleViewer, nil)
	mapClaim(t, db, g.ID, "sso", "payments-team")

	reconcile(t, db, database.DirectorySync{
		UserID: synced.ID, ProviderID: "sso", Claimed: []string{"payments-team"}, Asserted: true,
	})
	if err := db.AddGroupMember(context.Background(), g.ID, manual.ID, "builtin-admin", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}

	if err := db.RemoveGroupClaimMapping(context.Background(), g.ID, "sso", "payments-team", "builtin-admin", database.RequestMeta{}); err != nil {
		t.Fatalf("RemoveGroupClaimMapping: %v", err)
	}

	if got := memberGroupIDs(t, db, synced.ID); len(got) != 0 {
		t.Errorf("the directory membership survived the unmap: %v", got)
	}
	if got := memberGroupIDs(t, db, manual.ID); len(got) != 1 {
		t.Errorf("the unmap took a manual membership with it: %v", got)
	}
}

// DB1-8: deleting a provider left its claim mappings and the memberships its
// directory wrote, with no foreign key to take them — live authorization
// nothing reconciled any more. The delete takes both, and only those.
func TestDeletingAProviderRemovesWhatItsDirectoryWrote(t *testing.T) {
	db := dbtest.New(t)
	mkProvider(t, db, "sso", nil)
	g := mkGroup(t, db, "payments")
	synced := mkUserIn(t, db, "alice", database.SystemRoleViewer, nil)
	manual := mkUserIn(t, db, "contractor", database.SystemRoleViewer, nil)
	mapClaim(t, db, g.ID, "sso", "payments-team")
	reconcile(t, db, database.DirectorySync{
		UserID: synced.ID, ProviderID: "sso", Claimed: []string{"payments-team"}, Asserted: true,
	})
	if err := db.AddGroupMember(context.Background(), g.ID, manual.ID, "builtin-admin", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	var tokenBefore int
	db.Conn().QueryRow(`SELECT token_version FROM users WHERE id=?`, synced.ID).Scan(&tokenBefore)

	if err := db.DeleteAuthProvider("sso", database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}); err != nil {
		t.Fatalf("DeleteAuthProvider: %v", err)
	}

	if got := memberGroupIDs(t, db, synced.ID); len(got) != 0 {
		t.Errorf("the directory membership survived the provider: %v", got)
	}
	if got := memberGroupIDs(t, db, manual.ID); len(got) != 1 {
		t.Errorf("the delete took a manual membership with it: %v", got)
	}
	if mapped, err := db.GroupIsMapped(g.ID); err != nil || mapped {
		t.Errorf("the claim mapping survived the provider (mapped=%v, err=%v)", mapped, err)
	}
	var tokenAfter int
	db.Conn().QueryRow(`SELECT token_version FROM users WHERE id=?`, synced.ID).Scan(&tokenAfter)
	if tokenAfter <= tokenBefore {
		t.Error("the removed member's sessions were not invalidated")
	}
}

// ADR §51 permitted this feature on one condition: a directory administrator
// cannot become an owner of an install's apps. The condition is enforced from
// both ends, and this is the end that refuses the mapping.
func TestAGroupThatOwnsAppsCannotBeMapped(t *testing.T) {
	db := dbtest.New(t)
	g := mkGroup(t, db, "platform")
	mkUserIn(t, db, "alice", database.SystemRolePublisher, g)

	app := &database.App{
		Slug: "owned", Name: "Owned", OwnerGroupID: g.ID,
		Status: database.AppStatusStopped, Visibility: database.VisibilityPrivate,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	err := db.AddGroupClaimMapping(context.Background(), g.ID, "sso", "platform-team", "builtin-admin", database.RequestMeta{})
	if !errors.Is(err, database.ErrGroupOwnsApps) {
		t.Fatalf("AddGroupClaimMapping on an owning group = %v, want ErrGroupOwnsApps", err)
	}

	// And the refusal can name what it is protecting.
	owned, err := db.AppsOwnedByGroup(g.ID)
	if err != nil || len(owned) != 1 || owned[0] != "Owned" {
		t.Errorf("AppsOwnedByGroup = %v, %v", owned, err)
	}

	// Nothing was written on the way to the refusal.
	rows, err := db.ListGroupClaimMappings(g.ID)
	if err != nil || len(rows) != 0 {
		t.Errorf("a refused mapping left %v behind", rows)
	}
}

// A group with no mapping is an ordinary group, and GroupIsMapped is what the
// ownership paths read. It answers false rather than erroring for a group that
// has never been mapped, which is every group on every install today.
func TestGroupIsMappedIsFalseUntilSomethingIsAttached(t *testing.T) {
	db := dbtest.New(t)
	g := mkGroup(t, db, "payments")

	if mapped, err := db.GroupIsMapped(g.ID); err != nil || mapped {
		t.Fatalf("GroupIsMapped on a fresh group = %v, %v", mapped, err)
	}
	mapClaim(t, db, g.ID, "sso", "payments-team")
	if mapped, err := db.GroupIsMapped(g.ID); err != nil || !mapped {
		t.Errorf("GroupIsMapped after a mapping = %v, %v", mapped, err)
	}
	if err := db.RemoveGroupClaimMapping(context.Background(), g.ID, "sso", "payments-team", "builtin-admin", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if mapped, err := db.GroupIsMapped(g.ID); err != nil || mapped {
		t.Errorf("GroupIsMapped after the unmap = %v, %v", mapped, err)
	}
}
