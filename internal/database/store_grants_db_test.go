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

// A grant is the join between two mechanisms that already existed: the store's
// access ladder, and presigning. These tests are about the properties the join
// has to have — not coverage of the CRUD.

func putGrant(t *testing.T, db *database.DB, resourceID, subjectType, subjectID, access string) *database.StoreGrant {
	t.Helper()
	g := &database.StoreGrant{
		ResourceID: resourceID, SubjectType: subjectType, SubjectID: subjectID, Access: access,
	}
	if err := db.PutStoreGrant(g, database.BuiltinAdminID, "req-1", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStoreGrant(%s=%s, %s): %v", subjectType, subjectID, access, err)
	}
	return g
}

// A person's level is the highest of their direct grant and the grants of every
// group they belong to.
//
// Union-of-max rather than "the direct grant wins": a direct grant is not a
// ceiling anybody set, it is just the row that happens to name them, and a
// person who is in a group with `full` does not lose it by also being granted
// `read` individually. The opposite rule would make adding a narrow grant a
// silent revocation.
func TestStoreGrantResolutionIsUnionOfMax(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	group := mkGroup(t, db, "analysts")
	mkUserIn(t, db, "sg-user", database.SystemRolePublisher, group)

	// Direct only.
	putGrant(t, db, res.ID, database.StoreSubjectUser, "sg-user", runtime.AccessRead)
	if got := reachLevel(t, db, "sg-user", "reports"); got != runtime.AccessRead {
		t.Fatalf("direct grant alone: got %q, want %q", got, runtime.AccessRead)
	}

	// The group's is higher, so the group's is the answer.
	putGrant(t, db, res.ID, database.StoreSubjectGroup, group.ID, runtime.AccessFull)
	if got := reachLevel(t, db, "sg-user", "reports"); got != runtime.AccessFull {
		t.Fatalf("group grant is higher: got %q, want %q", got, runtime.AccessFull)
	}

	// The direct one is higher, so the direct one is. Same rule, other side.
	putGrant(t, db, res.ID, database.StoreSubjectGroup, group.ID, runtime.AccessRead)
	putGrant(t, db, res.ID, database.StoreSubjectUser, "sg-user", runtime.AccessWrite)
	if got := reachLevel(t, db, "sg-user", "reports"); got != runtime.AccessWrite {
		t.Fatalf("direct grant is higher: got %q, want %q", got, runtime.AccessWrite)
	}

	// One store, listed once, at the resolved level — not one row per grant.
	stores, err := db.ListReachableStores("sg-user")
	if err != nil {
		t.Fatalf("ListReachableStores: %v", err)
	}
	if len(stores) != 1 {
		t.Fatalf("two grants on one store should list it once, got %d rows", len(stores))
	}
	if stores[0].Top() != runtime.AccessWrite {
		t.Errorf("listing level = %q, want %q", stores[0].Top(), runtime.AccessWrite)
	}
}

// A person with no grant reaches nothing, and an administrator is a person.
//
// There is no admin bypass on purpose: granting yourself is one audited call,
// which is strictly better than an implicit reach no audit_log row explains.
func TestNoGrantReachesNoStore(t *testing.T) {
	db := dbtest.New(t)
	mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	mkUserIn(t, db, "sg-admin", database.SystemRoleAdmin, nil)

	stores, err := db.ListReachableStores("sg-admin")
	if err != nil {
		t.Fatalf("ListReachableStores: %v", err)
	}
	if len(stores) != 0 {
		t.Fatalf("an administrator with no grant reaches %d stores, want 0", len(stores))
	}
	reach, err := db.StoreScopesBySlug("sg-admin", "reports")
	if err != nil {
		t.Fatalf("StoreScopesBySlug: %v", err)
	}
	if reach != nil {
		t.Errorf("StoreScopesBySlug = %+v, want nil so the API can 404", reach)
	}
}

// Revoking takes effect on the next mint. Grants are read per request and
// nothing is cached — a cache with a TTL would make "revoked" mean "revoked in
// a while" for a credential whose whole job is minting URLs.
func TestRevokedGrantTakesEffectOnTheNextMint(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	mkUserIn(t, db, "sg-revoked", database.SystemRolePublisher, nil)
	putGrant(t, db, res.ID, database.StoreSubjectUser, "sg-revoked", runtime.AccessWrite)

	if got := reachLevel(t, db, "sg-revoked", "reports"); got != runtime.AccessWrite {
		t.Fatalf("before revocation: got %q", got)
	}
	if err := db.DeleteStoreGrant(res.ID, database.StoreSubjectUser, "sg-revoked", "",
		database.BuiltinAdminID, "req-2", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStoreGrant: %v", err)
	}
	reach, err := db.StoreScopesBySlug("sg-revoked", "reports")
	if err != nil {
		t.Fatalf("StoreScopesBySlug: %v", err)
	}
	if reach != nil {
		t.Errorf("after revocation the reach is %+v, want nil", reach)
	}
}

// A grant needs objects behind it. An EFS-backed file store has none — the only
// way to write to one is to mount it — so it is refused, twice: by the
// handler's path for a readable message, and by the trigger so a later caller
// cannot route around the handler.
//
// This is the rule migration 00018 left standing. It used to be spelled "only a
// store takes grants" and now reads "only a resource that names a bucket", and
// the set it refuses on this install is the same set: a file store an operator
// pointed at EFS names no bucket and never will.
func TestEFSFileStoreTakesNoGrant(t *testing.T) {
	db := dbtest.New(t)
	vol := mkResource(t, db, "Shared cache", "shared-cache", database.StorageKindVolume)
	mkUserIn(t, db, "sg-vol", database.SystemRolePublisher, nil)

	g := &database.StoreGrant{
		ResourceID: vol.ID, SubjectType: database.StoreSubjectUser,
		SubjectID: "sg-vol", Access: runtime.AccessWrite,
	}
	err := db.PutStoreGrant(g, database.BuiltinAdminID, "req-1", "10.0.0.1", "test")
	if !errors.Is(err, database.ErrStoreGrantNoBucket) {
		t.Fatalf("err = %v, want ErrStoreGrantNoBucket", err)
	}
	// The kind is named, so the message can say what a store is for.
	if !strings.Contains(err.Error(), "volume") {
		t.Errorf("the refusal should name the kind, got: %v", err)
	}

	// And the trigger holds when the handler is bypassed entirely.
	_, err = db.Conn().Exec(`
		INSERT INTO store_grants (id, resource_id, subject_type, subject_id, access, created_at, updated_at)
		VALUES ('direct-1', ?, 'user', 'sg-vol', 'write', NOW(), NOW())`, vol.ID)
	if err == nil {
		t.Fatal("a direct INSERT of a grant on a volume succeeded; the trigger is not holding")
	}
	if !strings.Contains(err.Error(), "names no bucket") {
		t.Errorf("trigger error = %v, want the store_grants message", err)
	}
}

// And the other half of the same rule: a file store backed by S3 Files *is* a
// bucket with a prefix, so it takes a grant and appears in the caller's reach.
//
// This is the test the whole of migration 00018 exists to make pass, and it is
// deliberately the mirror image of the one above — same kind, same ladder, same
// subject, and the only difference is what the operator put underneath it.
func TestS3FilesFileStoreTakesAGrant(t *testing.T) {
	db := dbtest.New(t)
	files := mkFileStore(t, db, "Nightly reports", "nightly-reports")
	mkUserIn(t, db, "sg-files", database.SystemRolePublisher, nil)

	g := &database.StoreGrant{
		ResourceID: files.ID, SubjectType: database.StoreSubjectUser,
		SubjectID: "sg-files", Access: runtime.AccessWrite,
	}
	if err := db.PutStoreGrant(g, database.BuiltinAdminID, "req-1", "10.0.0.1", "test"); err != nil {
		t.Fatalf("granting on an S3 Files file store: %v", err)
	}

	// The reach query is the second enforcement point and it has to agree: a
	// grant that writes but never resolves is a row nobody can use.
	reach, err := db.StoreScopesBySlug("sg-files", "nightly-reports")
	if err != nil {
		t.Fatalf("StoreScopesBySlug: %v", err)
	}
	if reach == nil {
		t.Fatal("a granted S3 Files file store is not reachable; the reach query still tests the kind")
	}
	if reach.Resource.Kind != database.StorageKindVolume {
		t.Errorf("kind = %q, want a volume — a file store is a file system to the app that mounts it", reach.Resource.Kind)
	}
	if len(reach.Scopes) != 1 || reach.Scopes[0].Access != runtime.AccessWrite {
		t.Errorf("scopes = %+v, want one write scope", reach.Scopes)
	}

	// And an EFS-backed file store beside it stays absent from the same
	// caller's reach, so the admission is the bucket rather than the kind.
	efs := mkResource(t, db, "Shared cache", "shared-cache", database.StorageKindVolume)
	_, err = db.Conn().Exec(`
		INSERT INTO store_grants (id, resource_id, subject_type, subject_id, access, created_at, updated_at)
		VALUES ('direct-efs', ?, 'user', 'sg-files', 'write', NOW(), NOW())`, efs.ID)
	if err == nil {
		t.Fatal("a direct INSERT of a grant on an EFS file store succeeded; the trigger is not holding")
	}
	stores, err := db.ListReachableStores("sg-files")
	if err != nil {
		t.Fatalf("ListReachableStores: %v", err)
	}
	if len(stores) != 1 || stores[0].Resource.Slug != "nightly-reports" {
		t.Errorf("reachable = %+v, want only the S3 Files file store", stores)
	}
}

// Every grant mutation writes audit_log in the same transaction, the rule every
// storage mutation already follows. The target is the resource, because that is
// the thing whose reach changed.
func TestStoreGrantMutationsAreAudited(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	mkUserIn(t, db, "sg-audited", database.SystemRolePublisher, nil)

	putGrant(t, db, res.ID, database.StoreSubjectUser, "sg-audited", runtime.AccessWrite)
	// A second put at another level is an edit of the first, and the audit
	// carries what it was before.
	putGrant(t, db, res.ID, database.StoreSubjectUser, "sg-audited", runtime.AccessFull)
	if err := db.DeleteStoreGrant(res.ID, database.StoreSubjectUser, "sg-audited", "",
		database.BuiltinAdminID, "req-3", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStoreGrant: %v", err)
	}

	rows, err := db.Conn().Query(`
		SELECT action, COALESCE(before::text,''), COALESCE(after::text,'')
		FROM audit_log WHERE target_id = ? AND action LIKE 'store_grant.%'
		ORDER BY occurred_at, action`, res.ID)
	if err != nil {
		t.Fatalf("reading audit_log: %v", err)
	}
	defer rows.Close()

	var actions []string
	var sawBefore, sawAfter bool
	for rows.Next() {
		var action, before, after string
		if err := rows.Scan(&action, &before, &after); err != nil {
			t.Fatalf("scan: %v", err)
		}
		actions = append(actions, action)
		if strings.Contains(before, runtime.AccessWrite) {
			sawBefore = true
		}
		if strings.Contains(after, runtime.AccessFull) {
			sawAfter = true
		}
	}
	if len(actions) != 3 {
		t.Fatalf("three mutations wrote %d audit rows: %v", len(actions), actions)
	}
	if !sawBefore {
		t.Error("an edit should record the level it replaced")
	}
	if !sawAfter {
		t.Error("a put should record the level it set")
	}
}

// The (resource, subject) pair is the grant's identity, which is what makes the
// PUT an upsert: a second grant at another level is an edit, never a row
// beside it. Two rows would make "what may this person do here" a question with
// two answers.
func TestStoreGrantIsIdentifiedByResourceAndSubject(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	mkUserIn(t, db, "sg-upsert", database.SystemRolePublisher, nil)

	first := putGrant(t, db, res.ID, database.StoreSubjectUser, "sg-upsert", runtime.AccessRead)
	second := putGrant(t, db, res.ID, database.StoreSubjectUser, "sg-upsert", runtime.AccessFull)
	if first.ID != second.ID {
		t.Errorf("a re-grant made a new row (%s → %s); it should have moved the first", first.ID, second.ID)
	}
	// And it is answered as the row it is: created when the first grant was,
	// by whoever made that one -- not 0001-01-01 and nobody (the eks drills).
	if second.CreatedAt.IsZero() || !second.CreatedAt.Equal(first.CreatedAt.Truncate(time.Microsecond)) || second.CreatedBy != first.CreatedBy {
		t.Errorf("the raised grant answered created %v by %q, want %v by %q",
			second.CreatedAt, second.CreatedBy, first.CreatedAt, first.CreatedBy)
	}

	grants, err := db.ListStoreGrants(res.ID)
	if err != nil {
		t.Fatalf("ListStoreGrants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("got %d grants, want 1", len(grants))
	}
	if grants[0].Access != runtime.AccessFull {
		t.Errorf("access = %q, want %q", grants[0].Access, runtime.AccessFull)
	}
}

// Revoking something that was never granted is a miss, not a silent success —
// the API answers 404 rather than telling an administrator it removed
// something that was not there.
func TestRevokingAGrantThatDoesNotExistIsAMiss(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)

	err := db.DeleteStoreGrant(res.ID, database.StoreSubjectUser, "nobody", "",
		database.BuiltinAdminID, "req-1", "10.0.0.1", "test")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

// A grant carries no foreign key on its subject, because the subject is
// polymorphic — so deleting the subject has to sweep the grants explicitly, or
// a live authorization row is left pointing at nobody.
func TestDeletingASubjectSweepsItsGrants(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	group := mkGroup(t, db, "leavers")
	mkUserIn(t, db, "sg-leaver", database.SystemRolePublisher, group)

	putGrant(t, db, res.ID, database.StoreSubjectUser, "sg-leaver", runtime.AccessWrite)
	putGrant(t, db, res.ID, database.StoreSubjectGroup, group.ID, runtime.AccessRead)

	if err := db.DeleteGroup(context.Background(), group.ID, database.BuiltinAdminID,
		database.RequestMeta{}); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	if err := db.DeleteUser("sg-leaver", database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID,
		Action: "user.delete", TargetType: "user", TargetID: "sg-leaver",
	}); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	grants, err := db.ListStoreGrants(res.ID)
	if err != nil {
		t.Fatalf("ListStoreGrants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("%d grants survived their subject: %+v", len(grants), grants)
	}
}

// Deleting the store takes its grants with it. CASCADE rather than a refusal:
// a grant on a deleted resource is not a historical record, it is a live
// authorization row pointing at nothing.
func TestDeletingAStoreTakesItsGrants(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	mkUserIn(t, db, "sg-cascade", database.SystemRolePublisher, nil)
	putGrant(t, db, res.ID, database.StoreSubjectUser, "sg-cascade", runtime.AccessWrite)

	if err := db.DeleteStorageResource(res.ID, database.BuiltinAdminID, "req-2", "10.0.0.1", "test"); err != nil {
		t.Fatalf("DeleteStorageResource: %v", err)
	}

	var n int
	if err := db.Conn().QueryRow(`SELECT COUNT(*) FROM store_grants WHERE resource_id = ?`, res.ID).
		Scan(&n); err != nil {
		t.Fatalf("counting grants: %v", err)
	}
	if n != 0 {
		t.Errorf("%d grants survived the store", n)
	}
}

// StoreAccessAtLeast is the one definition of "is this rung high enough", so no
// route spells the comparison out for itself.
func TestStoreAccessAtLeastIsTheLadder(t *testing.T) {
	ladder := []string{runtime.AccessRead, runtime.AccessWrite, runtime.AccessFull}
	for i, have := range ladder {
		for j, want := range ladder {
			got := database.StoreAccessAtLeast(have, want)
			if got != (i >= j) {
				t.Errorf("StoreAccessAtLeast(%q, %q) = %v, want %v", have, want, got, i >= j)
			}
		}
	}
	// An unknown rung is not a rung. "" >= "" must not be true, or a resource
	// with no resolved level would satisfy every check.
	if database.StoreAccessAtLeast("", "") {
		t.Error(`StoreAccessAtLeast("", "") must be false`)
	}
	if database.StoreAccessAtLeast(runtime.AccessFull, "root") {
		t.Error("a level that is not on the ladder must never be satisfied")
	}
}

func reachLevel(t *testing.T, db *database.DB, userID, slug string) string {
	t.Helper()
	scopes, err := db.StoreScopesBySlug(userID, slug)
	if err != nil {
		t.Fatalf("StoreScopesBySlug(%s, %s): %v", userID, slug, err)
	}
	if scopes == nil {
		return ""
	}
	return scopes.Top()
}

// The listing counts a store's audience, and a mapping is not part of it.
//
// This is the "neither implies the other" invariant seen from the admin list:
// a store with no app mounting it can still be reachable by six people, and a
// store mounted by four apps can be reachable by nobody. Counting the two
// facts from one column would make the emptier of them invisible, which is
// exactly the case an administrator most needs to see before deleting.
//
// Counted by subject type rather than resolved through group membership: the
// number has to reconcile with the list the grants dialog draws, and that list
// is rows. A group of forty is one row there and one group here.
func TestListingCountsGrantsAndMappingsSeparately(t *testing.T) {
	db := dbtest.New(t)
	res := mkResource(t, db, "Reports", "reports", database.StorageKindStore)
	group := mkGroup(t, db, "analysts")
	mkUserIn(t, db, "gc-user", database.SystemRolePublisher, group)

	// Two subjects, one of each kind, and no app mounting any of it.
	putGrant(t, db, res.ID, database.StoreSubjectUser, "gc-user", runtime.AccessRead)
	putGrant(t, db, res.ID, database.StoreSubjectGroup, group.ID, runtime.AccessFull)

	got := findResource(t, db, res.ID)
	if got.GrantUsers != 1 || got.GrantGroups != 1 {
		t.Errorf("audience = %d users, %d groups; want 1 and 1", got.GrantUsers, got.GrantGroups)
	}
	if got.MappingCount != 0 {
		t.Errorf("mapping count = %d, want 0 — granting a person mounts nothing", got.MappingCount)
	}

	// Mapping an app moves the other number and leaves the audience alone.
	app := mkApp(t, db, "reporter")
	m := &database.StorageMapping{
		ResourceID: res.ID, AppID: app.ID, Path: "/data/reports", Access: runtime.AccessRead,
	}
	if err := db.PutStorageMapping(m, database.BuiltinAdminID, "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}

	got = findResource(t, db, res.ID)
	if got.MappingCount != 1 {
		t.Errorf("mapping count = %d, want 1", got.MappingCount)
	}
	if got.GrantUsers != 1 || got.GrantGroups != 1 {
		t.Errorf("mapping an app changed the audience: %d users, %d groups", got.GrantUsers, got.GrantGroups)
	}
}

// A volume takes no grants, so its counts are zero for a reason the kind
// explains — the list prints nothing there rather than a zero, and this is the
// fact that entitles it to.
func TestAVolumeHasNoAudience(t *testing.T) {
	db := dbtest.New(t)
	vol := mkResource(t, db, "Shared cache", "shared-cache", database.StorageKindVolume)

	g := &database.StoreGrant{
		ResourceID:  vol.ID,
		SubjectType: database.StoreSubjectUser,
		SubjectID:   database.BuiltinAdminID,
		Access:      runtime.AccessRead,
	}
	if err := db.PutStoreGrant(g, database.BuiltinAdminID, "req", "10.0.0.1", "test"); !errors.Is(err, database.ErrStoreGrantNoBucket) {
		t.Fatalf("granting on a volume: got %v, want ErrStoreGrantNoBucket", err)
	}

	got := findResource(t, db, vol.ID)
	if got.GrantUsers != 0 || got.GrantGroups != 0 {
		t.Errorf("a volume reported an audience: %d users, %d groups", got.GrantUsers, got.GrantGroups)
	}
}

// Read through the listing rather than GetStorageResource, because the listing
// is what the admin page renders and therefore what the counts have to be
// correct in.
func findResource(t *testing.T, db *database.DB, id string) database.StorageResource {
	t.Helper()
	all, err := db.ListStorageResources()
	if err != nil {
		t.Fatalf("ListStorageResources: %v", err)
	}
	for _, r := range all {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("resource %s is not in the listing", id)
	return database.StorageResource{}
}
