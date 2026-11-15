package database_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/runtime"
)

// The resolution order, as a pure function and with no database.
//
// This is the rule that decides what a *later* revocation removes: a mapping is
// stamped with the delegation it resolved through, and a revocation collects by
// that stamp. So it has to be the same answer every time it is asked, whichever
// order the rows arrive in — which is exactly the property a table test can
// state and a query cannot.
func TestResolveAttachAuthority(t *testing.T) {
	user := func(id, level string) database.StorageDelegation {
		return database.StorageDelegation{SubjectType: database.StoreSubjectUser, SubjectID: id, MaxAccess: level}
	}
	group := func(id, level string) database.StorageDelegation {
		return database.StorageDelegation{SubjectType: database.StoreSubjectGroup, SubjectID: id, MaxAccess: level}
	}

	for _, tc := range []struct {
		name  string
		in    []database.StorageDelegation
		want  string
		level string
		none  bool
	}{
		{name: "nothing at all", in: nil, none: true},
		{name: "one direct row", in: []database.StorageDelegation{user("u1", runtime.AccessWrite)},
			want: "user:u1", level: runtime.AccessWrite},
		{name: "one group row", in: []database.StorageDelegation{group("g1", runtime.AccessRead)},
			want: "group:g1", level: runtime.AccessRead},

		// Union-of-max, not direct-wins: adding a narrow direct delegation to
		// somebody who already reaches a store widely through a group must not
		// quietly take the wide one away.
		{name: "the higher ceiling wins over a direct row",
			in:   []database.StorageDelegation{user("u1", runtime.AccessRead), group("g1", runtime.AccessFull)},
			want: "group:g1", level: runtime.AccessFull},
		{name: "the higher ceiling wins over a group row",
			in:   []database.StorageDelegation{user("u1", runtime.AccessFull), group("g1", runtime.AccessRead)},
			want: "user:u1", level: runtime.AccessFull},

		// A tie is broken by the more specific statement about this person.
		{name: "at equal level, direct beats group",
			in:   []database.StorageDelegation{group("g1", runtime.AccessWrite), user("u1", runtime.AccessWrite)},
			want: "user:u1", level: runtime.AccessWrite},
		{name: "two groups at equal level resolve by id",
			in:   []database.StorageDelegation{group("g9", runtime.AccessWrite), group("g2", runtime.AccessWrite)},
			want: "group:g2", level: runtime.AccessWrite},

		// A level on neither ladder is a row that means nothing, and must not
		// win by being the only candidate — a stamp pointing at it would be a
		// mapping no revocation could reason about.
		{name: "an unrecognised level is not a candidate",
			in: []database.StorageDelegation{group("g1", "sudo")}, none: true},
		{name: "an unrecognised level never outranks a real one",
			in:   []database.StorageDelegation{group("g1", "sudo"), user("u1", runtime.AccessRead)},
			want: "user:u1", level: runtime.AccessRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := database.ResolveAttachAuthority(tc.in)
			if tc.none {
				if ok {
					t.Fatalf("ResolveAttachAuthority(%v) = %q, want no authority", tc.in, got.AuthorityKey())
				}
				return
			}
			if !ok {
				t.Fatalf("ResolveAttachAuthority(%v) found nothing, want %q", tc.in, tc.want)
			}
			if got.AuthorityKey() != tc.want {
				t.Errorf("authority = %q, want %q", got.AuthorityKey(), tc.want)
			}
			if got.MaxAccess != tc.level {
				t.Errorf("ceiling = %q, want %q", got.MaxAccess, tc.level)
			}
		})
	}
}

// The order the rows arrive in is not part of the answer. Reversing the input
// has to give the same stamp, or a revocation would collect a different set
// depending on how the database happened to sort a join.
func TestResolveAttachAuthorityIsOrderIndependent(t *testing.T) {
	in := []database.StorageDelegation{
		{SubjectType: database.StoreSubjectGroup, SubjectID: "g2", MaxAccess: runtime.AccessWrite},
		{SubjectType: database.StoreSubjectUser, SubjectID: "u1", MaxAccess: runtime.AccessWrite},
		{SubjectType: database.StoreSubjectGroup, SubjectID: "g1", MaxAccess: runtime.AccessWrite},
	}
	forward, _ := database.ResolveAttachAuthority(in)

	reversed := make([]database.StorageDelegation, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		reversed = append(reversed, in[i])
	}
	backward, _ := database.ResolveAttachAuthority(reversed)

	if forward.AuthorityKey() != backward.AuthorityKey() {
		t.Fatalf("order changed the answer: %q then %q", forward.AuthorityKey(), backward.AuthorityKey())
	}
	if forward.AuthorityKey() != "user:u1" {
		t.Errorf("authority = %q, want user:u1", forward.AuthorityKey())
	}
}

func putManager(t *testing.T, db *database.DB, resourceID, subjectType, subjectID, level string) *database.StorageManager {
	t.Helper()
	m := &database.StorageManager{
		ResourceID: resourceID, SubjectType: subjectType, SubjectID: subjectID, MaxAccess: level,
	}
	if err := db.PutStorageManager(m, database.BuiltinAdminID, "req-1", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageManager(%s=%s, %s): %v", subjectType, subjectID, level, err)
	}
	return m
}

func putMapping(t *testing.T, db *database.DB, resourceID, appID, path, access, authority string) {
	t.Helper()
	m := &database.StorageMapping{
		ResourceID: resourceID, AppID: appID, Path: path, Access: access, Authority: authority,
	}
	if err := db.PutStorageMapping(m, database.BuiltinAdminID, "req-1", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping(%s as %s): %v", path, authority, err)
	}
}

func mappingPaths(t *testing.T, db *database.DB, appID string) []string {
	t.Helper()
	ms, err := db.ListStorageMappingsForApp(appID)
	if err != nil {
		t.Fatalf("ListStorageMappingsForApp: %v", err)
	}
	out := []string{}
	for _, m := range ms {
		out = append(out, m.Path)
	}
	return out
}

// A revocation collects its own mappings and nothing else.
//
// This is the property the stamp exists for. Without it the only way to answer
// "which mappings did this delegation write" would be to re-derive who *could*
// have written them — a query whose answer changes as group membership changes,
// quietly collecting mappings nobody's revocation was meant to reach.
func TestRevokingCollectsItsOwnMappingsOnly(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)
	group := mkGroup(t, db, "analysts")
	mkUserIn(t, db, "sm-delegate", database.SystemRolePublisher, group)
	app := mkApp(t, db, "reports")
	other := mkApp(t, db, "dashboards")

	putManager(t, db, res.ID, database.StoreSubjectGroup, group.ID, runtime.AccessWrite)
	putManager(t, db, res.ID, database.StoreSubjectUser, "sm-delegate", runtime.AccessRead)

	// Three mappings on the same resource, from three authorities.
	putMapping(t, db, res.ID, app.ID, "/data/sets", runtime.AccessWrite,
		database.AuthorityKey(database.StoreSubjectGroup, group.ID))
	putMapping(t, db, res.ID, other.ID, "/data/sets", runtime.AccessRead,
		database.AuthorityKey(database.StoreSubjectUser, "sm-delegate"))

	adminApp := mkApp(t, db, "finance")
	putMapping(t, db, res.ID, adminApp.ID, "/data/sets", runtime.AccessFull, database.AuthorityAdmin)

	affected, err := db.DeleteStorageManagerAndMappings(res.ID, database.StoreSubjectGroup, group.ID,
		database.BuiltinAdminID, "req-2", "10.0.0.1", "test")
	if err != nil {
		t.Fatalf("DeleteStorageManagerAndMappings: %v", err)
	}
	if len(affected) != 1 || affected[0].ID != app.ID {
		t.Fatalf("affected = %+v, want just %s", affected, app.Slug)
	}
	if got := mappingPaths(t, db, app.ID); len(got) != 0 {
		t.Errorf("the revoked delegation's mapping survived: %v", got)
	}
	if got := mappingPaths(t, db, other.ID); len(got) != 1 {
		t.Errorf("another delegate's mapping was collected: %v", got)
	}
	if got := mappingPaths(t, db, adminApp.ID); len(got) != 1 {
		t.Errorf("an administrator's mapping was collected: %v", got)
	}

	// And the delegation itself is gone, so the same revoke twice is a 404
	// rather than a second silent success.
	if _, err := db.DeleteStorageManagerAndMappings(res.ID, database.StoreSubjectGroup, group.ID,
		database.BuiltinAdminID, "req-3", "10.0.0.1", "test"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("second revoke: err = %v, want sql.ErrNoRows", err)
	}
}

// An administrator editing a delegate's mapping makes it theirs.
//
// The stamp is the authority of the *last* write, which is what makes "a
// mapping an administrator has touched never disappears with somebody else's
// delegation" true by construction rather than by a rule somebody has to
// remember.
func TestAnAdministratorsWriteAdoptsTheMapping(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)
	mkUserIn(t, db, "sm-owner", database.SystemRolePublisher, nil)
	app := mkApp(t, db, "reports")
	authority := database.AuthorityKey(database.StoreSubjectUser, "sm-owner")

	putManager(t, db, res.ID, database.StoreSubjectUser, "sm-owner", runtime.AccessWrite)
	putMapping(t, db, res.ID, app.ID, "/data/sets", runtime.AccessRead, authority)

	// The administrator moves it. No authority on the write means admin.
	putMapping(t, db, res.ID, app.ID, "/data/moved", runtime.AccessFull, "")

	ms, err := db.ListStorageMappingsForResource(res.ID)
	if err != nil {
		t.Fatalf("ListStorageMappingsForResource: %v", err)
	}
	if len(ms) != 1 || ms[0].Authority != database.AuthorityAdmin {
		t.Fatalf("authority = %+v, want a single admin-stamped row", ms)
	}

	affected, err := db.DeleteStorageManagerAndMappings(res.ID, database.StoreSubjectUser, "sm-owner",
		database.BuiltinAdminID, "req-2", "10.0.0.1", "test")
	if err != nil {
		t.Fatalf("DeleteStorageManagerAndMappings: %v", err)
	}
	if len(affected) != 0 {
		t.Errorf("the revocation reached an adopted mapping: %+v", affected)
	}
	if got := mappingPaths(t, db, app.ID); len(got) != 1 {
		t.Errorf("the adopted mapping was collected: %v", got)
	}
}

// The ceiling holds with the handler bypassed.
//
// The handler checks it first, so a delegate reads a message naming the ladder.
// This is the copy underneath, and the reason it exists is that a constraint
// only the handler enforces lasts until somebody writes a second caller — so
// the test that matters is the one that does not go through the handler.
func TestTheCeilingHoldsBeneathTheHandler(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)
	mkUserIn(t, db, "sm-capped", database.SystemRolePublisher, nil)
	app := mkApp(t, db, "reports")
	authority := database.AuthorityKey(database.StoreSubjectUser, "sm-capped")

	putManager(t, db, res.ID, database.StoreSubjectUser, "sm-capped", runtime.AccessWrite)

	// Through the database layer, which is one caller below the handler.
	m := &database.StorageMapping{
		ResourceID: res.ID, AppID: app.ID, Path: "/data/sets",
		Access: runtime.AccessFull, Authority: authority,
	}
	err := db.PutStorageMapping(m, database.BuiltinAdminID, "req-1", "10.0.0.1", "test")
	if !errors.Is(err, database.ErrStorageCeiling) {
		t.Fatalf("err = %v, want ErrStorageCeiling", err)
	}

	// And one caller below *that*: raw SQL, past the Go check entirely, is where
	// the trigger is the only thing left.
	_, err = db.Conn().Exec(`
		INSERT INTO storage_mappings (id, resource_id, app_id, path, access, authority, created_at, updated_at)
		VALUES ('sm-raw', ?, ?, '/data/raw', 'full', ?, NOW(), NOW())`, res.ID, app.ID, authority)
	if err == nil {
		t.Fatal("raw insert above the ceiling succeeded — the trigger is not there")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("the trigger should say what the ceiling was, got: %v", err)
	}

	// At the ceiling it goes in, by both routes.
	putMapping(t, db, res.ID, app.ID, "/data/sets", runtime.AccessWrite, authority)
}

// A mapping stamped to a delegation that does not exist is refused, which is
// the other half of the same trigger: a stamp is a claim about an authority and
// an unbacked one is a mapping no revocation would ever collect.
func TestAMappingCannotClaimADelegationThatIsNotThere(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)
	app := mkApp(t, db, "reports")

	m := &database.StorageMapping{
		ResourceID: res.ID, AppID: app.ID, Path: "/data/sets",
		Access: runtime.AccessRead, Authority: "user:nobody",
	}
	if err := db.PutStorageMapping(m, database.BuiltinAdminID, "req-1", "10.0.0.1", "test"); !errors.Is(err, database.ErrStorageNotManaged) {
		t.Fatalf("err = %v, want ErrStorageNotManaged", err)
	}

	_, err := db.Conn().Exec(`
		INSERT INTO storage_mappings (id, resource_id, app_id, path, access, authority, created_at, updated_at)
		VALUES ('sm-ghost', ?, ?, '/data/ghost', 'read', 'user:nobody', NOW(), NOW())`, res.ID, app.ID)
	if err == nil {
		t.Fatal("raw insert with an unbacked stamp succeeded — the trigger is not there")
	}
	if !strings.Contains(err.Error(), "does not manage") {
		t.Errorf("the trigger should name the missing delegation, got: %v", err)
	}
}

// A delegation on a volume may only name a level a volume has.
//
// A volume has two rungs and a store has three, and `write` on a volume is not
// a stricter version of anything: a create-only mount of a filesystem would be
// advisory, so it is a level that means nothing there.
func TestAVolumeDelegationIsCappedByTheVolumeLadder(t *testing.T) {
	db := dbtest.New(t)
	vol := mkResource(t, db, "Shared cache", "shared-cache", database.StorageKindVolume)
	mkUserIn(t, db, "sm-vol", database.SystemRolePublisher, nil)

	m := &database.StorageManager{
		ResourceID: vol.ID, SubjectType: database.StoreSubjectUser,
		SubjectID: "sm-vol", MaxAccess: runtime.AccessWrite,
	}
	if err := db.PutStorageManager(m, database.BuiltinAdminID, "req-1", "10.0.0.1", "test"); !errors.Is(err, database.ErrStorageCeiling) {
		t.Fatalf("err = %v, want ErrStorageCeiling", err)
	}

	_, err := db.Conn().Exec(`
		INSERT INTO storage_managers (id, resource_id, subject_type, subject_id, max_access, created_at, updated_at)
		VALUES ('sm-vol-raw', ?, 'user', 'sm-vol', 'write', NOW(), NOW())`, vol.ID)
	if err == nil {
		t.Fatal("raw insert of write on a volume succeeded — the trigger is not there")
	}
	if !strings.Contains(err.Error(), "is a volume") {
		t.Errorf("the trigger should name the kind, got: %v", err)
	}

	putManager(t, db, vol.ID, database.StoreSubjectUser, "sm-vol", runtime.AccessFull)
}

// A subject whose delegations stamp live mappings is not deleted (ADR § Storage
// somebody else can attach).
//
// Before, the rows went and nothing restarted, so the mounts disappeared at
// each app's next *start* — a crash, an idle wake — at a random later time.
// Now the delete is refused with the apps named, and once the delegation is
// revoked the same delete goes through and collects the delegation rows.
func TestDeletingASubjectWithStampedMappingsIsRefused(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)
	other := mkResource(t, db, "Archive", "archive", database.StorageKindStore)
	group := mkGroup(t, db, "analysts")
	app := mkApp(t, db, "reports")

	putManager(t, db, res.ID, database.StoreSubjectGroup, group.ID, runtime.AccessWrite)
	putManager(t, db, other.ID, database.StoreSubjectGroup, group.ID, runtime.AccessRead)
	putMapping(t, db, res.ID, app.ID, "/data/sets", runtime.AccessWrite,
		database.AuthorityKey(database.StoreSubjectGroup, group.ID))

	err := db.DeleteGroup(context.Background(), group.ID, database.BuiltinAdminID, database.RequestMeta{})
	var held *database.SubjectHoldsMappingsError
	if !errors.As(err, &held) || !errors.Is(err, database.ErrSubjectHoldsMappings) {
		t.Fatalf("DeleteGroup = %v, want *SubjectHoldsMappingsError", err)
	}
	if len(held.Mappings) != 1 || held.Mappings[0].AppSlug != "reports" || held.Mappings[0].ResourceName != "datasets" {
		t.Fatalf("refusal names %+v, want reports on datasets", held.Mappings)
	}
	if got := mappingPaths(t, db, app.ID); len(got) != 1 {
		t.Errorf("a refused delete touched the mappings: %v", got)
	}
	if g, _ := db.GetGroup(group.ID); g == nil {
		t.Fatal("a refused delete removed the group")
	}

	if _, err := db.DeleteStorageManagerAndMappings(res.ID, database.StoreSubjectGroup, group.ID,
		database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := db.DeleteGroup(context.Background(), group.ID, database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("DeleteGroup after the revoke: %v", err)
	}
	for _, id := range []string{res.ID, other.ID} {
		managers, err := db.ListStorageManagers(id)
		if err != nil {
			t.Fatalf("ListStorageManagers: %v", err)
		}
		if len(managers) != 0 {
			t.Errorf("delegations outlived the deleted group: %+v", managers)
		}
	}
}

// The admin listing carries names, not a count.
//
// An administrator about to revoke is about to restart these apps, and a number
// does not let them warn anybody. `mapping_count` on the resource stays a count
// because nothing there is about to restart.
func TestTheManagerListingNamesTheAppsEachDelegationMounts(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)
	mkUserIn(t, db, "sm-named", database.SystemRolePublisher, nil)
	app := mkApp(t, db, "reports")
	adminApp := mkApp(t, db, "finance")
	authority := database.AuthorityKey(database.StoreSubjectUser, "sm-named")

	putManager(t, db, res.ID, database.StoreSubjectUser, "sm-named", runtime.AccessWrite)
	putMapping(t, db, res.ID, app.ID, "/data/sets", runtime.AccessWrite, authority)
	putMapping(t, db, res.ID, adminApp.ID, "/data/sets", runtime.AccessFull, database.AuthorityAdmin)

	managers, err := db.ListStorageManagers(res.ID)
	if err != nil {
		t.Fatalf("ListStorageManagers: %v", err)
	}
	if len(managers) != 1 {
		t.Fatalf("managers = %+v, want one", managers)
	}
	if len(managers[0].Apps) != 1 || managers[0].Apps[0].Slug != "reports" {
		t.Fatalf("apps = %+v, want just reports — an administrator's mapping is not this delegation's", managers[0].Apps)
	}
	if managers[0].SubjectName != "sm-named" {
		t.Errorf("subject name = %q, want the delegate's name", managers[0].SubjectName)
	}
}

// Managing a store confers its data plane, at the delegation's own ceiling —
// through the resolver, not through a second row.
//
// A shadow `store_grants` row would be two facts for one decision, and the
// revoke path would then have to know which grant rows are shadows that die
// with their delegation. This asserts the arm instead: no grant anywhere, and
// the store is still reachable.
func TestManagingAStoreConfersItsDataPlane(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	mkUserIn(t, db, "sm-reach", database.SystemRolePublisher, nil)

	if got := reachLevel(t, db, "sm-reach", "reports"); got != "" {
		t.Fatalf("reach before any delegation = %q, want none", got)
	}

	putManager(t, db, res.ID, database.StoreSubjectUser, "sm-reach", runtime.AccessWrite)
	if got := reachLevel(t, db, "sm-reach", "reports"); got != runtime.AccessWrite {
		t.Fatalf("reach = %q, want %q", got, runtime.AccessWrite)
	}

	stores, err := db.ListReachableStores("sm-reach")
	if err != nil {
		t.Fatalf("ListReachableStores: %v", err)
	}
	if len(stores) != 1 || stores[0].Resource.Slug != "reports" {
		t.Fatalf("stores = %+v, want reports", stores)
	}

	// And it stops on the next call, uncached, exactly as a revoked grant does.
	if _, err := db.DeleteStorageManagerAndMappings(res.ID, database.StoreSubjectUser, "sm-reach",
		database.BuiltinAdminID, "req-2", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageManagerAndMappings: %v", err)
	}
	if got := reachLevel(t, db, "sm-reach", "reports"); got != "" {
		t.Errorf("reach after revoking = %q, want none", got)
	}
}

// Union-of-max across both tables. A grant and a delegation at different levels
// resolve to the higher one, whichever table it is in — because "the direct one
// wins" or "a grant beats a delegation" would make adding one a silent
// revocation of the other.
func TestReachIsUnionOfMaxAcrossGrantsAndDelegations(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	group := mkGroup(t, db, "analysts")
	mkUserIn(t, db, "sm-both", database.SystemRolePublisher, group)

	putGrant(t, db, res.ID, database.StoreSubjectUser, "sm-both", runtime.AccessRead)
	putManager(t, db, res.ID, database.StoreSubjectGroup, group.ID, runtime.AccessFull)
	if got := reachLevel(t, db, "sm-both", "reports"); got != runtime.AccessFull {
		t.Fatalf("the delegation is higher: got %q, want %q", got, runtime.AccessFull)
	}

	// The other way round, on one resource.
	other := mkResource(t, db, "Archive", "archive", database.StorageKindStore)
	putGrant(t, db, other.ID, database.StoreSubjectGroup, group.ID, runtime.AccessFull)
	putManager(t, db, other.ID, database.StoreSubjectUser, "sm-both", runtime.AccessRead)
	if got := reachLevel(t, db, "sm-both", "archive"); got != runtime.AccessFull {
		t.Fatalf("the grant is higher: got %q, want %q", got, runtime.AccessFull)
	}
}

// A delegation on a volume confers no data plane, and the volume never appears
// in the caller's stores.
//
// There is nothing to confer: no presigned URL, no browser page, no CLI. A
// volume delegation is attach-only, and the copy has to say so rather than let
// the word "manage" promise a page that does not exist for that kind.
func TestManagingAVolumeConfersNoDataPlane(t *testing.T) {
	db := dbtest.New(t)
	vol := mkResource(t, db, "Shared cache", "shared-cache", database.StorageKindVolume)
	mkUserIn(t, db, "sm-volonly", database.SystemRolePublisher, nil)
	putManager(t, db, vol.ID, database.StoreSubjectUser, "sm-volonly", runtime.AccessFull)

	if got := reachLevel(t, db, "sm-volonly", "shared-cache"); got != "" {
		t.Errorf("reach on a volume = %q, want none", got)
	}
	stores, err := db.ListReachableStores("sm-volonly")
	if err != nil {
		t.Fatalf("ListReachableStores: %v", err)
	}
	if len(stores) != 0 {
		t.Errorf("stores = %+v, want none — a volume has no data plane", stores)
	}

	// It is still attachable, which is the whole point of it being a fourth
	// table rather than a flag on the store-only one.
	attachable, err := db.ListAttachableStorage("sm-volonly")
	if err != nil {
		t.Fatalf("ListAttachableStorage: %v", err)
	}
	if len(attachable) != 1 || attachable[0].Slug != "shared-cache" {
		t.Fatalf("attachable = %+v, want the volume", attachable)
	}
	if attachable[0].MaxAccess != runtime.AccessFull {
		t.Errorf("ceiling = %q, want %q", attachable[0].MaxAccess, runtime.AccessFull)
	}
	// The containment boundary is an administrator's, and never on this wire.
	if attachable[0].Kind != database.StorageKindVolume {
		t.Errorf("kind = %q, want volume", attachable[0].Kind)
	}
}

// A membership change removes reach and leaves mappings standing.
//
// Deriving live mounts from live membership would make an ordinary group edit a
// silent unmount of production apps, discovered by whoever gets paged. The way
// to remove those mounts is to revoke the group's delegation, which is loud,
// named and confirmed.
func TestLeavingAGroupRemovesReachAndLeavesMounts(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	group := mkGroup(t, db, "analysts")
	mkUserIn(t, db, "sm-leaver", database.SystemRolePublisher, group)
	app := mkApp(t, db, "dash")

	putManager(t, db, res.ID, database.StoreSubjectGroup, group.ID, runtime.AccessWrite)
	putMapping(t, db, res.ID, app.ID, "/data/reports", runtime.AccessWrite,
		database.AuthorityKey(database.StoreSubjectGroup, group.ID))

	if err := db.RemoveGroupMember(context.Background(), group.ID, "sm-leaver", database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("RemoveGroupMember: %v", err)
	}

	if got := reachLevel(t, db, "sm-leaver", "reports"); got != "" {
		t.Errorf("reach after leaving the group = %q, want none", got)
	}
	attachable, err := db.ListAttachableStorage("sm-leaver")
	if err != nil {
		t.Fatalf("ListAttachableStorage: %v", err)
	}
	if len(attachable) != 0 {
		t.Errorf("attachable after leaving = %+v, want none", attachable)
	}
	if got := mappingPaths(t, db, app.ID); len(got) != 1 {
		t.Errorf("mappings = %v, want the mount to stand — a membership change is not a revocation", got)
	}
}

// A group delete that races a member's attach under its delegation waits for
// the attach and then refuses, rather than deleting the delegation out from
// under a mapping that names it (security review 2026-11-14, DB-8).
func TestDeletingASubjectWaitsForAnAttachInFlight(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)
	group := mkGroup(t, db, "racers")
	app := mkApp(t, db, "racing-reports")
	putManager(t, db, res.ID, database.StoreSubjectGroup, group.ID, runtime.AccessWrite)

	attach, err := db.Conn().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer attach.Rollback()
	var mid string
	if err := attach.QueryRow(`SELECT id FROM storage_managers WHERE subject_type='group' AND subject_id=? FOR UPDATE`, group.ID).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	if _, err := attach.Exec(`
		INSERT INTO storage_mappings (id, resource_id, app_id, path, access, authority, created_at, updated_at)
		VALUES ('sm-race', ?, ?, '/data/race', 'write', ?, NOW(), NOW())`,
		res.ID, app.ID, database.AuthorityKey(database.StoreSubjectGroup, group.ID)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- db.DeleteGroup(context.Background(), group.ID, database.BuiltinAdminID, database.RequestMeta{})
	}()
	time.Sleep(200 * time.Millisecond)
	if err := attach.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, database.ErrSubjectHoldsMappings) {
		t.Fatalf("DeleteGroup during an attach = %v, want ErrSubjectHoldsMappings", err)
	}
}

// AP-5: PutStorageMapping takes the app row before the delegation row — the
// order the flip's ReconcileManifestStorage takes them in — so a delegate's
// attach at the instant of a flip waits rather than deadlocking. While the app
// row is held elsewhere, the put has not yet touched the delegation.
func TestPutStorageMappingTakesTheAppRowBeforeTheDelegation(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Datasets", "datasets", database.StorageKindStore)
	mkUserIn(t, db, "sm-order", database.SystemRolePublisher, nil)
	app := mkApp(t, db, "reports")
	putManager(t, db, res.ID, database.StoreSubjectUser, "sm-order", runtime.AccessFull)

	holder, err := db.Conn().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err := holder.Exec(`SELECT 1 FROM apps WHERE id = ? FOR NO KEY UPDATE`, app.ID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- db.PutStorageMapping(&database.StorageMapping{
			ResourceID: res.ID, AppID: app.ID, Path: "/data/sets", Access: runtime.AccessRead,
			Authority: database.AuthorityKey(database.StoreSubjectUser, "sm-order"),
		}, database.BuiltinAdminID, "req-1", "10.0.0.1", "test")
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the put finished (%v) while the app row was held", err)
	default:
	}

	probe, err := db.Conn().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Exec(`SELECT 1 FROM storage_managers WHERE resource_id = ? AND subject_type = ? AND subject_id = ? FOR UPDATE NOWAIT`,
		res.ID, database.StoreSubjectUser, "sm-order"); err != nil {
		t.Errorf("the delegation row was locked by a put still waiting on the app row: %v", err)
	}
	probe.Rollback()

	holder.Rollback()
	if err := <-done; err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
}
