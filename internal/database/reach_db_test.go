package database_test

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The reach reader's tests. The first two lead because their failure would be
// **silent**: a source that stops being reported while the page keeps
// rendering, and a query that is correct and unusable on the estate it exists
// for.

// TestReachSourcesAreTheSpecification is the whole of D1's enforcement.
//
// It walks every table in the schema carrying an `app_id` column and fails on
// one that is neither read by a ReachSource nor named in NotReach with a
// reason. The failure it exists to prevent is the quiet one: a later feature
// gives apps a new way to touch something, this reader says nothing about it,
// the page keeps rendering, and the only symptom is an answer that was
// incomplete for a year.
//
// It walks the migrated schema rather than the migration files, because what
// the reader has to keep up with is the database that exists.
func TestReachSourcesAreTheSpecification(t *testing.T) {
	db := dbtest.New(t)

	rows, err := db.Conn().Query(`
		SELECT table_name FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name = 'app_id'
		ORDER BY table_name`)
	if err != nil {
		t.Fatalf("walking the schema: %v", err)
	}
	defer rows.Close()

	sourced := map[string]string{}
	elsewhere := map[string]string{}
	for _, s := range database.ReachSources {
		for _, tbl := range s.Tables {
			sourced[tbl] = s.Key
		}
		for _, tbl := range s.Elsewhere {
			elsewhere[tbl] = s.Key
		}
	}

	var carrying []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan: %v", err)
		}
		carrying = append(carrying, table)
		if _, ok := sourced[table]; ok {
			continue
		}
		if why, ok := database.NotReach[table]; ok {
			if why == "" {
				t.Errorf("%s is excluded from reach with no reason given — "+
					"the reason is the half that makes this a decision rather than an oversight", table)
			}
			continue
		}
		t.Errorf("%s carries an app_id and is neither a reach source nor excluded.\n"+
			"Something new can be given to an app. Either add it to database.ReachSources — "+
			"and to the per-app read, the estate query and both surfaces — or add it to "+
			"database.NotReach with the reason it is not reach.", table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(carrying) == 0 {
		t.Fatal("no table carries an app_id, so this guard checked nothing")
	}

	// And the other direction, so neither list can name something that has
	// gone. An exclusion for a table nobody has any more is a reason somebody
	// will later read as still applying to something.
	held := map[string]bool{}
	for _, tbl := range carrying {
		held[tbl] = true
	}
	for tbl := range database.NotReach {
		if !held[tbl] {
			t.Errorf("NotReach excludes %s, which no longer carries an app_id", tbl)
		}
	}
	for tbl, key := range sourced {
		if !held[tbl] {
			t.Errorf("the %s source reads %s, which no longer carries an app_id — "+
				"either it moved to a different key, in which case it belongs in Elsewhere, "+
				"or the source is reading a table that is gone", key, tbl)
		}
	}
	// Elsewhere is the other half of the same claim: a table keyed some way
	// other than app_id, which the walk above cannot see. Assert it exists and
	// that it really is keyed some other way, so a table that *gains* an
	// app_id moves into Tables and comes back under the guard.
	for tbl, key := range elsewhere {
		var n int
		if err := db.Conn().QueryRow(`
			SELECT COUNT(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = ?`, tbl).Scan(&n); err != nil {
			t.Fatalf("looking up %s: %v", tbl, err)
		}
		if n == 0 {
			t.Errorf("the %s source names %s in Elsewhere, and no such table exists", key, tbl)
		}
		if held[tbl] {
			t.Errorf("%s carries an app_id now, so it belongs in the %s source's Tables "+
				"where the schema walk can check it", tbl, key)
		}
	}
}

// TestEstateReachIsOneQuery is the other silent failure, and the sharper of the
// two.
//
// N+1 is *correct*: it produces the right answer and degrades with the size of
// the estate, so it is fastest on the installs that do not need this page and
// unusable on the one that does — and nobody finds out until an incident, on
// the page opened during it.
//
// What it asserts is the property rather than a number: the statement count
// does not grow with the number of apps. A constant offset is fine and a slope
// is not, which is exactly the difference between one aggregate query and a
// query per app.
func TestEstateReachIsOneQuery(t *testing.T) {
	db, queries := dbtest.NewCounting(t)

	measure := func(apps int) int64 {
		for i := 0; i < apps; i++ {
			mkApp(t, db, fmt.Sprintf("reach-%d-%d", apps, i))
		}
		before := queries()
		if _, err := db.EstateReach(database.AppScope{All: true}); err != nil {
			t.Fatalf("EstateReach: %v", err)
		}
		return queries() - before
	}

	small := measure(4)
	large := measure(24)

	if small != 1 || large != 1 {
		t.Fatalf("EstateReach took %d round trips over 4 apps and %d over 28, want 1 and 1 — "+
			"a query per app is correct and unusable on the estate this page exists for", small, large)
	}
}

// TestEstateReachNeverOmitsAnApp — an app with no deployment, no grants and no
// attachments is still a row.
//
// A governance reader that answers less than it knows without saying so is the
// defect § Optional capabilities exists to prevent, and on this page the empty
// app is often the most interesting row: on an estate where everything else has
// ten entries, the one holding nothing is the one somebody should look at.
func TestEstateReachNeverOmitsAnApp(t *testing.T) {
	db := dbtest.New(t)
	bare := mkApp(t, db, "holds-nothing")

	set, err := db.EstateReach(database.AppScope{All: true})
	if err != nil {
		t.Fatalf("EstateReach: %v", err)
	}
	if set.AppsTotal != 1 || len(set.Rows) != 1 {
		t.Fatalf("EstateReach = %d rows / total %d, want one of each", len(set.Rows), set.AppsTotal)
	}
	row := set.Rows[0]
	if row.AppID != bare.ID {
		t.Fatalf("row is %s, want %s", row.AppID, bare.ID)
	}
	if set.AppsWithReach != 0 {
		t.Errorf("an app holding nothing counted toward apps_with_reach")
	}
	// Empty lists, never nil: the surfaces render `.length` and a null would be
	// a crash rather than an empty section.
	if row.Credentials == nil || row.Mounts == nil || row.Integrations == nil {
		t.Errorf("row carries a nil list: %+v", row)
	}
}

// The coverage is summed from the rows the table renders, so the sentence above
// the table and the table cannot disagree — the failure EstateCensus and
// EstateTargetSet each avoid the same way.
func TestEstateReachCoverageComesFromTheRows(t *testing.T) {
	db := dbtest.New(t)

	mkApp(t, db, "empty-one")
	withCred := mkApp(t, db, "holds-a-credential")
	withMount := mkApp(t, db, "mounts-a-share")

	cred := mkCredential(t, db, "prod-db", map[string]string{"PASSWORD": "s3cret"})
	if err := db.AttachCredential(context.Background(), withCred.ID, cred.ID,
		database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "builtin-admin"}); err != nil {
		t.Fatalf("AttachCredential: %v", err)
	}
	mountWritable(t, db, withMount.ID, "reports", "/data/reports", "write")

	set, err := db.EstateReach(database.AppScope{All: true})
	if err != nil {
		t.Fatalf("EstateReach: %v", err)
	}
	if set.AppsTotal != 3 {
		t.Fatalf("apps_total = %d, want 3", set.AppsTotal)
	}
	if set.AppsWithReach != 2 {
		t.Errorf("apps_with_reach = %d, want 2 — the empty app is a row and not a holder", set.AppsWithReach)
	}
	if set.AppsWritableStorage != 1 {
		t.Errorf("apps_writable_storage = %d, want 1", set.AppsWritableStorage)
	}

	byID := map[string]database.EstateReachRow{}
	for _, r := range set.Rows {
		byID[r.AppID] = r
	}
	if got := byID[withCred.ID].Credentials; len(got) != 1 || got[0].Name != "prod-db" {
		t.Errorf("credentials on the holder = %+v, want one named prod-db", got)
	}
	// The name is there and no value is, which is D3 at the row level.
	if m := byID[withMount.ID].Mounts; len(m) != 1 || m[0].Path != "/data/reports" || !m[0].Writable() {
		t.Errorf("mounts = %+v, want one writable /data/reports", m)
	}
}

// The PRD wrote this case as "a storage resource is deleted while a mapping
// survives", and the schema does not allow it: storage_mappings cascades on the
// resource, and DeleteStorageResource refuses by name while any app mounts one.
// So what is asserted is what actually happens — the mount goes with the
// resource, and the reader says so rather than reporting a mount to something
// that is not there.
//
// `ReachMount.ResourceMissing` and the LEFT JOIN behind it stay, defensively,
// and their comment says they are unreachable today. This test is what would
// start failing if either guarantee were removed, which is when that flag stops
// being decorative.
func TestReachDropsAMountWhenItsResourceIsDeleted(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "cascaded-mount")
	res := mountWritable(t, db, app.ID, "going", "/data/going", "read")

	if r, err := db.ReachOf(app.ID); err != nil || len(r.Mounts) != 1 {
		t.Fatalf("ReachOf before the delete = %v, %v; want one mount", r, err)
	}

	// Straight at the table: DeleteStorageResource refuses while the app
	// mounts it, which is the guarantee above and not what is under test here.
	if _, err := db.Conn().Exec(`DELETE FROM storage_resources WHERE id = ?`, res.ID); err != nil {
		t.Fatalf("deleting the resource: %v", err)
	}

	r, err := db.ReachOf(app.ID)
	if err != nil {
		t.Fatalf("ReachOf: %v", err)
	}
	if len(r.Mounts) != 0 {
		t.Fatalf("mounts = %+v after the resource was deleted, want none — "+
			"the cascade took the mapping, and a reader showing one would be describing a row that is gone", r.Mounts)
	}
}

// ReachOf answers nil for an app that does not exist, so the layer above turns
// that into the same 404 every other by-id read answers with rather than into
// an empty assembly that reads like an app holding nothing.
func TestReachOfAnAppThatIsNotThere(t *testing.T) {
	db := dbtest.New(t)
	r, err := db.ReachOf("no-such-app")
	if err != nil {
		t.Fatalf("ReachOf: %v", err)
	}
	if r != nil {
		t.Fatalf("ReachOf on a missing app = %+v, want nil", r)
	}
}

// The scope is the reading predicate, beside the census and the targets: what
// an app holds is a fact about the app, and the `list` rung discloses the row
// that names it and not one field more.
func TestEstateReachIsScoped(t *testing.T) {
	db := dbtest.New(t)
	mine := mkApp(t, db, "mine")
	mkApp(t, db, "somebody-elses")

	set, err := db.EstateReach(database.AppScope{
		PrincipalID: mine.OwnerID, PrincipalType: database.PrincipalTypeUser,
	})
	if err != nil {
		t.Fatalf("EstateReach: %v", err)
	}
	var slugs []string
	for _, r := range set.Rows {
		slugs = append(slugs, r.Slug)
	}
	sort.Strings(slugs)
	if len(slugs) != 1 || slugs[0] != "mine" {
		t.Fatalf("scoped read returned %v, want just the caller's own app", slugs)
	}
	if set.AppsTotal != 1 {
		t.Errorf("apps_total = %d, want 1 — the coverage is scoped with the rows", set.AppsTotal)
	}
}

// mountWritable gives an app a storage resource at one path and level.
func mountWritable(t *testing.T, db *database.DB, appID, slug, path, access string) *database.StorageResource {
	t.Helper()
	res := &database.StorageResource{
		ID: "res-" + slug, Slug: slug, Name: slug,
		Kind: "volume", Backing: "efs", ClaimName: "pvc-" + slug,
	}
	if err := db.CreateStorageResource(res, "builtin-admin", "", "", ""); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	if _, err := db.Conn().Exec(`
		INSERT INTO storage_mappings (id, resource_id, app_id, path, access, folder, authority, created_by)
		VALUES (?, ?, ?, ?, ?, '', 'admin', 'builtin-admin')`,
		"map-"+slug+"-"+appID, res.ID, appID, path, access); err != nil {
		t.Fatalf("inserting the mapping: %v", err)
	}
	// CreateStorageResource mints the id itself, so the caller gets the row
	// back rather than the one it asked for.
	return res
}
