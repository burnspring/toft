package database_test

import (
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The install-wide half of withholding anonymity (ADR §70).
//
// This is the reachability suspension's test with one word changed, and that is
// the point: the two suppressors are different decisions with the same
// consequence, and the failure they share is the silent one — an app hidden
// from the gateway and still enumerated, by name and by slug, to the anonymous
// internet.
//
// The default is the first thing asserted. Every other switch in this product
// defaults off; this one defaults **on**, and a regression that inverted it
// would take every published app on an upgraded install dark on a morning
// nobody chose — with the settings page still reading "public apps: on",
// because there is no row.

// The generic single-key door rather than SetPlatformSettings, so the SQL
// fragment is read against exactly the row an administrator's save produces and
// nothing else in the blob moves. No actor: `updated_by` is a real foreign key.
func setPublicApps(t *testing.T, db *database.DB, on bool) {
	t.Helper()
	value := "false"
	if on {
		value = "true"
	}
	if _, err := db.PutSetting(database.SettingPublicAppsEnabled, value, ""); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
}

func TestPublicAppsAreOnUntilSomebodySaysOtherwise(t *testing.T) {
	db := dbtest.New(t)
	app := mkVisibleApp(t, db, "live", database.VisibilityPublic, "flask")

	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if !s.PublicAppsEnabled {
		t.Error("public apps are off on an install where nobody has set anything")
	}

	// The SQL half has to read the absent row the same way the Go half does.
	// They are two spellings of one default, and this is the only place they
	// are compared.
	got, err := db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PublicWithheld {
		t.Error("PublicWithheld = true with no setting row — the SQL default disagrees with the Go one")
	}
	if anon, err := db.ListApps("", "", nil); err != nil || !slugs(anon)["live"] {
		t.Errorf("a public app is not listed anonymously on a default install (err %v)", err)
	}
}

func TestPublicAppsOffLeavesEveryAnonymousListing(t *testing.T) {
	db := dbtest.New(t)

	pub := mkVisibleApp(t, db, "live", database.VisibilityPublic, "nextjs")
	tag := mkTag(t, db, "Department", "department")
	setTag(t, db, pub.ID, tag.ID, "Engineering", "engineering")

	setPublicApps(t, db, false)

	// Entry point one: ListApps.
	anon, err := db.ListApps("", "", nil)
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if slugs(anon)["live"] {
		t.Error("a public app is still listed to anonymous callers on an install that does not publish")
	}

	// Entry point two: AppScope, which the tag list, tag values and tag counts
	// share. Covering one of the two is how the other comes to differ.
	byTag, err := db.ListAppsByTag("department", "engineering", database.AppScope{})
	if err != nil {
		t.Fatalf("ListAppsByTag: %v", err)
	}
	if slugs(byTag)["live"] {
		t.Error("anonymous tag browse still returns a public app")
	}

	values, err := db.ListTagValues(tag.ID, "", database.AppScope{})
	if err != nil {
		t.Fatalf("ListTagValues: %v", err)
	}
	for _, v := range values {
		if v.AppCount != 0 {
			t.Errorf("anonymous tag value %q counts %d apps, want 0", v.Value, v.AppCount)
		}
	}
}

// The other half of the same rule, and the one the choice of `authenticated`
// over `private` exists for: what is withdrawn is anonymity and nothing else.
func TestPublicAppsOffIsUnchangedForSignedInCallers(t *testing.T) {
	db := dbtest.New(t)

	app := mkVisibleApp(t, db, "reports", database.VisibilityPublic, "flask")
	if err := db.UpsertUser(&database.User{ID: "stranger", Login: "stranger"}); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	setPublicApps(t, db, false)

	for _, tc := range []struct {
		name  string
		scope database.AppScope
	}{
		{"a signed-in stranger", userScope("stranger")},
		{"the owner", userScope(app.OwnerID)},
		{"an administrator", adminScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apps, err := db.ListApps(tc.scope.PrincipalID, tc.scope.PrincipalType, nil)
			if tc.scope.All {
				apps, err = db.ListAllApps()
			}
			if err != nil {
				t.Fatalf("ListApps: %v", err)
			}
			if !slugs(apps)["reports"] {
				t.Error("an app vanished for a caller who was never anonymous")
			}
		})
	}
}

// The Go seam, and the column the platform must not write.
func TestPublicAppsOffReachesTheAuthorizerAndNotTheColumn(t *testing.T) {
	db := dbtest.New(t)
	app := mkVisibleApp(t, db, "reports", database.VisibilityPublic, "flask")
	private := mkVisibleApp(t, db, "drafts", database.VisibilityPrivate, "flask")

	setPublicApps(t, db, false)

	got, err := db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PublicWithheld {
		t.Fatal("the switch did not reach the app row")
	}
	if got.Visibility != database.VisibilityPublic {
		t.Errorf("visibility = %q, want %q — the platform wrote the owner's column",
			got.Visibility, database.VisibilityPublic)
	}
	if v := got.AuthObject().Visibility; v != database.VisibilityAuthenticated {
		t.Errorf("the authorizer sees %q, want %q", v, database.VisibilityAuthenticated)
	}

	// It is a fact about the install, so it is true on a private app too — the
	// page offering an owner the Public option needs it whatever that app says
	// today. What it must never do is change an app that is not public.
	gotPriv, err := db.GetApp(private.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !gotPriv.PublicWithheld {
		t.Error("PublicWithheld = false on a private app — the fact is about the install, not the app")
	}
	if v := gotPriv.AuthObject().Visibility; v != database.VisibilityPrivate {
		t.Errorf("a private app authorizes as %q, want %q", v, database.VisibilityPrivate)
	}

	// Nothing was written down, so turning it back on republishes exactly the
	// apps that were public — there is no list to keep and nothing to restore.
	setPublicApps(t, db, true)
	back, err := db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.PublicWithheld {
		t.Error("the app is still withheld after the switch went back on")
	}
	if anon, err := db.ListApps("", "", nil); err != nil || !slugs(anon)["reports"] {
		t.Errorf("the app was not republished by turning the switch back on (err %v)", err)
	}
}

// What the access review reports. "Who chose what" and "how exposed are we" are
// different questions, and this switch is what can make their
// answers differ.
func TestPublicAppsOffSeparatesChosenFromInEffect(t *testing.T) {
	db := dbtest.New(t)
	mkVisibleApp(t, db, "one", database.VisibilityPublic, "flask")
	mkVisibleApp(t, db, "two", database.VisibilityPublic, "flask")
	mkVisibleApp(t, db, "three", database.VisibilityAuthenticated, "flask")

	setPublicApps(t, db, false)

	public, inEffect, authed, err := db.CountAppsByVisibility()
	if err != nil {
		t.Fatalf("CountAppsByVisibility: %v", err)
	}
	if public != 2 {
		t.Errorf("public = %d, want 2 — the owners' choices are unchanged", public)
	}
	if inEffect != 0 {
		t.Errorf("publicInEffect = %d, want 0 — nothing is reachable anonymously here", inEffect)
	}
	if authed != 1 {
		t.Errorf("authenticated = %d, want 1", authed)
	}
}
