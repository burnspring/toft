package database_test

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/tags"
)

// mkVisibleApp is mkApp with an explicit visibility, for the scoping tests.
// mkApp (helpers_db_test.go) leaves apps private, which is the right default there
// but hides the interesting cases here.
func mkVisibleApp(t *testing.T, db *database.DB, slug, visibility, framework string) *database.App {
	t.Helper()
	owner := &database.User{ID: "owner-" + slug, Login: "owner-" + slug}
	if err := db.UpsertUser(owner); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	app := &database.App{
		Slug: slug, Name: slug, OwnerID: owner.ID,
		RepoURL:    "https://github.com/example/" + slug,
		Branch:     "main",
		Status:     database.AppStatusStopped,
		Visibility: visibility,
		Framework:  framework,
	}
	if err := db.CreateApp(app); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	return app
}

func mkTag(t *testing.T, db *database.DB, name, slug string) *database.Tag {
	t.Helper()
	tag := &database.Tag{ID: "tag-" + slug, Name: name, Slug: slug, Kind: database.TagKindCustom}
	inTx(t, db, func(tx *database.Tx) error { return db.CreateTagTx(tx, tag) })
	return tag
}

func setTag(t *testing.T, db *database.DB, appID, tagID, value, valueSlug string) {
	t.Helper()
	inTx(t, db, func(tx *database.Tx) error {
		return db.SetAppTagTx(tx, appID, tagID, value, valueSlug, database.BuiltinAdminID)
	})
}

func inTx(t *testing.T, db *database.DB, fn func(*database.Tx) error) {
	t.Helper()
	tx, err := db.Conn().Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		t.Fatalf("tx body: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// adminScope sees everything, like a system_admin.
var adminScope = database.AppScope{All: true}

func userScope(id string) database.AppScope {
	return database.AppScope{PrincipalID: id, PrincipalType: database.PrincipalTypeUser}
}

func TestFrameworkTagIsSeeded(t *testing.T) {
	db := dbtest.New(t)

	tag, err := db.GetTag(tags.FrameworkTagID)
	if err != nil {
		t.Fatalf("GetTag: %v", err)
	}
	if tag == nil {
		t.Fatal("the schema did not seed the Framework tag")
	}
	if tag.Kind != database.TagKindSystem {
		t.Errorf("Framework tag kind = %q, want %q", tag.Kind, database.TagKindSystem)
	}
	if tag.Slug != tags.FrameworkTagSlug {
		t.Errorf("Framework tag slug = %q, want %q", tag.Slug, tags.FrameworkTagSlug)
	}
}

func TestTagCRUD(t *testing.T) {
	db := dbtest.New(t)

	tag := mkTag(t, db, "Department", "department")

	got, err := db.GetTagBySlug("department")
	if err != nil || got == nil {
		t.Fatalf("GetTagBySlug: %v (tag=%v)", err, got)
	}
	if got.Name != "Department" || got.Kind != database.TagKindCustom {
		t.Errorf("got %+v, want name Department / kind custom", got)
	}

	// Rename leaves the slug alone: /tags/department stays a working permalink.
	tag.Name = "Team"
	inTx(t, db, func(tx *database.Tx) error { return db.UpdateTagTx(tx, tag) })

	got, _ = db.GetTag(tag.ID)
	if got.Name != "Team" {
		t.Errorf("after rename name = %q, want Team", got.Name)
	}
	if got.Slug != "department" {
		t.Errorf("rename changed the slug to %q; permalinks would break", got.Slug)
	}

	inTx(t, db, func(tx *database.Tx) error { return db.DeleteTagTx(tx, tag.ID) })
	got, err = db.GetTag(tag.ID)
	if err != nil {
		t.Fatalf("GetTag after delete: %v", err)
	}
	if got != nil {
		t.Error("tag still present after delete")
	}
}

func TestTagNameIsCaseInsensitivelyUnique(t *testing.T) {
	db := dbtest.New(t)
	mkTag(t, db, "Department", "department")

	tx, err := db.Conn().Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	err = db.CreateTagTx(tx, &database.Tag{
		ID: "tag-dupe", Name: "department", Slug: "department-2", Kind: database.TagKindCustom,
	})
	if err == nil {
		t.Fatal("created a second tag named \"department\"; the vocabulary would fork on casing")
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("unexpected error: %v", err)
	}
}

// The core invariant: an app holds one value per tag, so setting a new value
// replaces the old rather than accumulating.
func TestOneValuePerTag(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")
	tag := mkTag(t, db, "Department", "department")

	setTag(t, db, app.ID, tag.ID, "Engineering", "engineering")
	setTag(t, db, app.ID, tag.ID, "Product", "product")

	got, err := db.TagsForApp(app.ID)
	if err != nil {
		t.Fatalf("TagsForApp: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("app carries %d values for one tag, want 1: %+v", len(got), got)
	}
	if got[0].Value != "Product" || got[0].ValueSlug != "product" {
		t.Errorf("got %q/%q, want Product/product", got[0].Value, got[0].ValueSlug)
	}
}

func TestSetAppTagIsIdempotent(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")
	tag := mkTag(t, db, "Department", "department")

	setTag(t, db, app.ID, tag.ID, "Engineering", "engineering")
	setTag(t, db, app.ID, tag.ID, "Engineering", "engineering")

	got, _ := db.TagsForApp(app.ID)
	if len(got) != 1 || got[0].Value != "Engineering" {
		t.Errorf("replaying the same value changed the result: %+v", got)
	}
}

func TestClearAppTag(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")
	dept := mkTag(t, db, "Department", "department")
	life := mkTag(t, db, "Lifecycle", "lifecycle")

	setTag(t, db, app.ID, dept.ID, "Engineering", "engineering")
	setTag(t, db, app.ID, life.ID, "Production", "production")

	inTx(t, db, func(tx *database.Tx) error { return db.ClearAppTagTx(tx, app.ID, dept.ID) })

	got, _ := db.TagsForApp(app.ID)
	if len(got) != 1 || got[0].TagID != life.ID {
		t.Errorf("clearing one tag disturbed the others: %+v", got)
	}
}

// Free-text values fork the vocabulary unless a new spelling snaps to the one
// already stored. CanonicalValue is what the write path uses to do that.
func TestCanonicalValueSnapsToExistingSpelling(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")
	tag := mkTag(t, db, "Department", "department")

	setTag(t, db, app.ID, tag.ID, "Engineering", "engineering")

	canonical, err := db.CanonicalValue(tag.ID, "engineering")
	if err != nil {
		t.Fatalf("CanonicalValue: %v", err)
	}
	if canonical != "Engineering" {
		t.Errorf("CanonicalValue = %q, want Engineering", canonical)
	}

	fresh, err := db.CanonicalValue(tag.ID, "product")
	if err != nil {
		t.Fatalf("CanonicalValue (new): %v", err)
	}
	if fresh != "" {
		t.Errorf("an unused value slug returned %q, want empty", fresh)
	}
}

func TestDeleteTagCascadesToApps(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")
	tag := mkTag(t, db, "Department", "department")
	setTag(t, db, app.ID, tag.ID, "Engineering", "engineering")

	n, err := db.CountAppsWithTag(tag.ID)
	if err != nil || n != 1 {
		t.Fatalf("CountAppsWithTag = %d, %v; want 1", n, err)
	}

	inTx(t, db, func(tx *database.Tx) error { return db.DeleteTagTx(tx, tag.ID) })

	got, _ := db.TagsForApp(app.ID)
	if len(got) != 0 {
		t.Errorf("app still carries %d tags after the tag was deleted", len(got))
	}
	// The app itself survives.
	if a, _ := db.GetApp(app.ID); a == nil {
		t.Error("deleting a tag deleted the app")
	}
}

func TestDeletingAppRemovesItsTags(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "demo")
	tag := mkTag(t, db, "Department", "department")
	setTag(t, db, app.ID, tag.ID, "Engineering", "engineering")

	if err := db.DeleteApp(app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	n, err := db.CountAppsWithTag(tag.ID)
	if err != nil {
		t.Fatalf("CountAppsWithTag: %v", err)
	}
	if n != 0 {
		t.Errorf("%d app_tags rows survived the app, want 0", n)
	}
}

func TestTagsForAppsBatches(t *testing.T) {
	db := dbtest.New(t)
	a := mkApp(t, db, "alpha")
	b := mkApp(t, db, "beta")
	mkApp(t, db, "gamma") // untagged
	dept := mkTag(t, db, "Department", "department")
	life := mkTag(t, db, "Lifecycle", "lifecycle")

	setTag(t, db, a.ID, dept.ID, "Engineering", "engineering")
	setTag(t, db, a.ID, life.ID, "Production", "production")
	setTag(t, db, b.ID, dept.ID, "Product", "product")

	got, err := db.TagsForApps([]string{a.ID, b.ID})
	if err != nil {
		t.Fatalf("TagsForApps: %v", err)
	}
	if len(got[a.ID]) != 2 {
		t.Errorf("alpha has %d tags, want 2", len(got[a.ID]))
	}
	if len(got[b.ID]) != 1 || got[b.ID][0].Value != "Product" {
		t.Errorf("beta = %+v, want one Product value", got[b.ID])
	}

	// An empty input must not reach the database — "IN ()" is a syntax error.
	empty, err := db.TagsForApps(nil)
	if err != nil {
		t.Fatalf("TagsForApps(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("TagsForApps(nil) = %v, want empty", empty)
	}
}

// **These tests are about visibility, so they say what the rung is doing.**
//
// The tag surfaces are *listings* — `AppScope.listingPredicate`, asserted by
// TestAppsPredicateCallers — so the install-wide `list` rung reaches every one
// of them, and since 2026-08-31 it is on by default. A stranger on a default
// install therefore sees every app's tags, which is correct and is what
// TestTagSurfacesFollowTheInstallWideRung below asserts.
//
// What each of the tests using this helper is actually about is the *visibility*
// scoping underneath that — public versus private, owner versus stranger — and
// the rung sits on top of it and hides it. So they turn it off and say so, which
// makes the precondition a line in the test rather than a default that can move
// under them again.
func rungOff(t *testing.T, db *database.DB) {
	t.Helper()
	if _, err := db.PutSetting(database.SettingAppsListableToAuthenticated, "false", ""); err != nil {
		t.Fatalf("turning the install-wide list rung off: %v", err)
	}
}

func TestListAppsByTagRespectsVisibility(t *testing.T) {
	db := dbtest.New(t)
	rungOff(t, db)
	pub := mkVisibleApp(t, db, "public-app", database.VisibilityPublic, "nextjs")
	priv := mkVisibleApp(t, db, "private-app", database.VisibilityPrivate, "flask")
	tag := mkTag(t, db, "Department", "department")

	setTag(t, db, pub.ID, tag.ID, "Engineering", "engineering")
	setTag(t, db, priv.ID, tag.ID, "Engineering", "engineering")

	cases := []struct {
		name  string
		scope database.AppScope
		want  int
	}{
		{"anonymous sees only the public app", database.AppScope{}, 1},
		{"a stranger sees only the public app", userScope("stranger"), 1},
		{"the private app's owner sees both", userScope(priv.OwnerID), 2},
		{"an admin sees both", adminScope, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apps, err := db.ListAppsByTag("department", "engineering", tc.scope)
			if err != nil {
				t.Fatalf("ListAppsByTag: %v", err)
			}
			if len(apps) != tc.want {
				t.Errorf("got %d apps, want %d", len(apps), tc.want)
			}
		})
	}

	// An empty value slug means "any value for this tag".
	any, err := db.ListAppsByTag("department", "", adminScope)
	if err != nil {
		t.Fatalf("ListAppsByTag (any value): %v", err)
	}
	if len(any) != 2 {
		t.Errorf("any-value query returned %d apps, want 2", len(any))
	}
}

func TestListAppsByFrameworkRespectsVisibility(t *testing.T) {
	db := dbtest.New(t)
	rungOff(t, db)
	mkVisibleApp(t, db, "public-next", database.VisibilityPublic, "nextjs")
	priv := mkVisibleApp(t, db, "private-next", database.VisibilityPrivate, "nextjs")

	apps, err := db.ListAppsByFramework("nextjs", userScope("stranger"))
	if err != nil {
		t.Fatalf("ListAppsByFramework: %v", err)
	}
	if len(apps) != 1 {
		t.Errorf("a stranger saw %d nextjs apps, want just the public one", len(apps))
	}

	apps, _ = db.ListAppsByFramework("nextjs", userScope(priv.OwnerID))
	if len(apps) != 2 {
		t.Errorf("the owner saw %d nextjs apps, want 2", len(apps))
	}
}

// The picker's autocomplete would otherwise reveal that some app the caller
// can't see is tagged "Project Titan".
func TestListTagValuesIsViewerScoped(t *testing.T) {
	db := dbtest.New(t)
	rungOff(t, db)
	pub := mkVisibleApp(t, db, "public-app", database.VisibilityPublic, "nextjs")
	priv := mkVisibleApp(t, db, "private-app", database.VisibilityPrivate, "nextjs")
	tag := mkTag(t, db, "Department", "department")

	setTag(t, db, pub.ID, tag.ID, "Engineering", "engineering")
	setTag(t, db, priv.ID, tag.ID, "Project Titan", "project-titan")

	values, err := db.ListTagValues(tag.ID, "", userScope("stranger"))
	if err != nil {
		t.Fatalf("ListTagValues: %v", err)
	}
	if len(values) != 1 || values[0].Value != "Engineering" {
		t.Fatalf("a stranger saw %+v; the private app's value leaked", values)
	}

	all, _ := db.ListTagValues(tag.ID, "", adminScope)
	if len(all) != 2 {
		t.Errorf("admin saw %d values, want 2", len(all))
	}

	// The query filter is a substring match, case-insensitive.
	filtered, err := db.ListTagValues(tag.ID, "titan", adminScope)
	if err != nil {
		t.Fatalf("ListTagValues (filtered): %v", err)
	}
	if len(filtered) != 1 || filtered[0].Value != "Project Titan" {
		t.Errorf("filtered = %+v, want just Project Titan", filtered)
	}
}

func TestListTagsCountsAreViewerScoped(t *testing.T) {
	db := dbtest.New(t)
	rungOff(t, db)
	pub := mkVisibleApp(t, db, "public-app", database.VisibilityPublic, "nextjs")
	priv := mkVisibleApp(t, db, "private-app", database.VisibilityPrivate, "flask")
	tag := mkTag(t, db, "Department", "department")
	mkTag(t, db, "Unused", "unused")

	setTag(t, db, pub.ID, tag.ID, "Engineering", "engineering")
	setTag(t, db, priv.ID, tag.ID, "Product", "product")

	byName := func(list []database.Tag) map[string]database.Tag {
		m := map[string]database.Tag{}
		for _, tg := range list {
			m[tg.Slug] = tg
		}
		return m
	}

	stranger, err := db.ListTags(userScope("stranger"))
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	sm := byName(stranger)
	if sm["department"].AppCount != 1 || sm["department"].ValueCount != 1 {
		t.Errorf("stranger saw department apps=%d values=%d, want 1/1 — the private app leaked",
			sm["department"].AppCount, sm["department"].ValueCount)
	}

	// A tag nobody can see any usage of must still be listed, with zero counts,
	// rather than vanishing from the vocabulary.
	if _, ok := sm["unused"]; !ok {
		t.Error("a tag with no readable apps disappeared from the list")
	}
	if sm["unused"].AppCount != 0 {
		t.Errorf("unused tag app count = %d, want 0", sm["unused"].AppCount)
	}

	admin := byName(mustListTags(t, db, adminScope))
	if admin["department"].AppCount != 2 || admin["department"].ValueCount != 2 {
		t.Errorf("admin saw department apps=%d values=%d, want 2/2",
			admin["department"].AppCount, admin["department"].ValueCount)
	}
}

// The Framework tag's counts come from apps.framework, not app_tags: nothing is
// ever written to app_tags for it.
func TestFrameworkTagCountsComeFromTheAppColumn(t *testing.T) {
	db := dbtest.New(t)
	rungOff(t, db)
	mkVisibleApp(t, db, "a", database.VisibilityPublic, "nextjs")
	mkVisibleApp(t, db, "b", database.VisibilityPublic, "nextjs")
	priv := mkVisibleApp(t, db, "c", database.VisibilityPrivate, "flask")

	find := func(scope database.AppScope) database.Tag {
		for _, tg := range mustListTags(t, db, scope) {
			if tg.ID == tags.FrameworkTagID {
				return tg
			}
		}
		t.Fatal("Framework tag missing from ListTags")
		return database.Tag{}
	}

	stranger := find(userScope("stranger"))
	if stranger.AppCount != 2 || stranger.ValueCount != 1 {
		t.Errorf("stranger saw Framework apps=%d values=%d, want 2/1",
			stranger.AppCount, stranger.ValueCount)
	}

	owner := find(userScope(priv.OwnerID))
	if owner.AppCount != 3 || owner.ValueCount != 2 {
		t.Errorf("owner saw Framework apps=%d values=%d, want 3/2", owner.AppCount, owner.ValueCount)
	}

	counts, err := db.FrameworkValueCounts(adminScope)
	if err != nil {
		t.Fatalf("FrameworkValueCounts: %v", err)
	}
	if counts["nextjs"] != 2 || counts["flask"] != 1 {
		t.Errorf("FrameworkValueCounts = %v, want nextjs:2 flask:1", counts)
	}

	// Nothing was written to app_tags for any of this.
	if n, _ := db.CountAppsWithTag(tags.FrameworkTagID); n != 0 {
		t.Errorf("%d app_tags rows exist for the Framework tag; its values must stay derived", n)
	}
}

func mustListTags(t *testing.T, db *database.DB, scope database.AppScope) []database.Tag {
	t.Helper()
	list, err := db.ListTags(scope)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	return list
}

func TestAppIDsWithTag(t *testing.T) {
	db := dbtest.New(t)
	a := mkApp(t, db, "alpha")
	b := mkApp(t, db, "beta")
	tag := mkTag(t, db, "Department", "department")
	setTag(t, db, a.ID, tag.ID, "Engineering", "engineering")
	setTag(t, db, b.ID, tag.ID, "Product", "product")

	ids, err := db.AppIDsWithTag(tag.ID, 100)
	if err != nil {
		t.Fatalf("AppIDsWithTag: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("got %d app ids, want 2", len(ids))
	}

	bounded, err := db.AppIDsWithTag(tag.ID, 1)
	if err != nil {
		t.Fatalf("AppIDsWithTag (bounded): %v", err)
	}
	if len(bounded) != 1 {
		t.Errorf("limit ignored: got %d ids, want 1", len(bounded))
	}
}

// The other half of the sentence above: with the rung on — the default — a
// stranger sees every app's tags, because a tag surface is a listing and the
// rung's whole subject is what a listing contains.
//
// It is asserted rather than assumed because the two answers are one flag
// apart, and a change that made the tag surfaces read `readable` instead would
// pass every test above this one and quietly hide apps from the people the
// install had just decided to show them to.
func TestTagSurfacesFollowTheInstallWideRung(t *testing.T) {
	db := dbtest.New(t)

	pub := mkVisibleApp(t, db, "rung-public", database.VisibilityPublic, "nextjs")
	priv := mkVisibleApp(t, db, "rung-private", database.VisibilityPrivate, "flask")
	tag := mkTag(t, db, "Department", "department")
	setTag(t, db, pub.ID, tag.ID, "Engineering", "engineering")
	setTag(t, db, priv.ID, tag.ID, "Engineering", "engineering")

	// On, from an absent settings row — nothing is written to turn it on.
	apps, err := db.ListAppsByTag("department", "engineering", userScope("rung-stranger"))
	if err != nil {
		t.Fatalf("ListAppsByTag: %v", err)
	}
	if len(apps) != 2 {
		t.Errorf("a stranger saw %d apps under the tag, want both — a tag surface is a listing, "+
			"and the install-wide rung lists every app to everybody signed in", len(apps))
	}

	// And the owner's withdrawal reaches here too, because it narrows the same
	// disjunct: one column, every listing.
	if err := db.SetAppListable(priv.ID, true, false,
		database.PrincipalTypeUser, database.BuiltinAdminID, "", "", "", ""); err != nil {
		t.Fatalf("SetAppListable: %v", err)
	}
	apps, err = db.ListAppsByTag("department", "engineering", userScope("rung-stranger"))
	if err != nil {
		t.Fatalf("ListAppsByTag: %v", err)
	}
	if len(apps) != 1 {
		t.Errorf("a stranger saw %d apps after the private one was withdrawn, want just the public "+
			"one — the column narrows every listing, not only the apps page", len(apps))
	}
}
