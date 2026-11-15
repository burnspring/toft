package database_test

import (
	"context"
	"fmt"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The list rung's two predicates, asserted against a real database over
// generated grant states.
//
// The property is containment: **every app a principal may open appears in the
// listing they are shown.** It is stated as a property rather than as a handful
// of cases because the two predicates are indistinguishable in any test that has
// one app in it, and getting them backwards fails in two different ways that are
// both quiet. A listing narrowed by `readable` hides apps from people who hold
// the rung — reported as "the platform lost my app", and diagnosed nowhere near
// this code. An object check narrowed by `listable` makes every app readable to
// everyone who could see its name, which nothing reports at all.
func grantTo(t *testing.T, db *database.DB, appID, principalType, principalID, role string) {
	t.Helper()
	if role == "" {
		return
	}
	if err := db.UpsertGrant(context.Background(), appID, principalType, principalID, role,
		database.BuiltinAdminID, database.RequestMeta{}); err != nil {
		t.Fatalf("UpsertGrant(%s, %s, %s): %v", appID, principalID, role, err)
	}
}

func TestListableContainsReadable(t *testing.T) {
	db := dbtest.New(t)

	group := mkGroup(t, db, "listers")
	user := mkUserIn(t, db, "u-subject", database.SystemRolePublisher, group)
	groups := []string{group.ID}
	p := &auth.Principal{Type: database.PrincipalTypeUser, ID: user.ID, SystemRole: database.SystemRolePublisher, Groups: groups}

	visibilities := []string{database.VisibilityPublic, database.VisibilityAuthenticated, database.VisibilityPrivate}
	directRoles := []string{"", database.ObjectRoleList, database.ObjectRoleViewer, database.ObjectRoleEditor, database.ObjectRoleOwner}
	groupRoles := []string{"", database.ObjectRoleList, database.ObjectRoleViewer}

	type state struct {
		app              *database.App
		visibility       string
		direct, viaGroup string
	}
	var states []state

	n := 0
	for _, vis := range visibilities {
		for _, direct := range directRoles {
			for _, viaGroup := range groupRoles {
				n++
				app := mkVisibleApp(t, db, fmt.Sprintf("gen-%02d", n), vis, "flask")
				grantTo(t, db, app.ID, database.PrincipalTypeUser, user.ID, direct)
				grantTo(t, db, app.ID, database.PrincipalTypeGroup, group.ID, viaGroup)
				states = append(states, state{app: app, visibility: vis, direct: direct, viaGroup: viaGroup})
			}
		}
	}

	// §9.3: a group-owned app whose group is *also* granted the rung on another
	// app. Neither combination existed before, so it is asserted here rather
	// than reasoned about.
	owned := &database.App{
		Slug: "gen-group-owned", Name: "gen-group-owned", OwnerGroupID: group.ID,
		RepoURL: "https://github.com/example/gen-group-owned", Branch: "main",
		Status: database.AppStatusStopped, Visibility: database.VisibilityPrivate, Framework: "flask",
	}
	if err := db.CreateApp(owned); err != nil {
		t.Fatalf("CreateApp(group-owned): %v", err)
	}
	grantTo(t, db, owned.ID, database.PrincipalTypeGroup, group.ID, database.ObjectRoleOwner)
	states = append(states, state{app: owned, visibility: database.VisibilityPrivate, viaGroup: database.ObjectRoleOwner})

	listed, err := db.ListApps(user.ID, database.PrincipalTypeUser, groups)
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	inListing := slugs(listed)

	for _, st := range states {
		fresh, err := db.GetApp(st.app.ID)
		if err != nil {
			t.Fatalf("GetApp(%s): %v", st.app.Slug, err)
		}
		readable := auth.CanRead(p, fresh.AuthObject(), db)
		desc := fmt.Sprintf("%s (visibility=%s direct=%q group=%q)", st.app.Slug, st.visibility, st.direct, st.viaGroup)

		if readable && !inListing[st.app.Slug] {
			t.Errorf("%s is readable and absent from the listing — listable must contain readable", desc)
		}

		// And the rung itself: a grant of `list` and nothing stronger puts the
		// app in the listing and does not open it. The visibility clause can
		// still make it readable on its own, which is why this only fires when
		// the app is private.
		onlyListRung := (st.direct == database.ObjectRoleList || st.viaGroup == database.ObjectRoleList) &&
			st.direct != database.ObjectRoleViewer && st.direct != database.ObjectRoleEditor && st.direct != database.ObjectRoleOwner &&
			st.viaGroup != database.ObjectRoleViewer
		if onlyListRung && st.visibility == database.VisibilityPrivate {
			if readable {
				t.Errorf("%s is readable on a list grant alone — the rung must sit below every way of reading", desc)
			}
			if !inListing[st.app.Slug] {
				t.Errorf("%s is absent from the listing despite a list grant — the rung confers exactly this", desc)
			}
		}
	}
}

// The rung is a grant naming a principal, so an anonymous listing cannot be
// widened by one. An install's public listing is exactly what it was.
func TestTheRungNeverReachesAnAnonymousListing(t *testing.T) {
	db := dbtest.New(t)

	group := mkGroup(t, db, "listers")
	user := mkUserIn(t, db, "u-anon-subject", database.SystemRolePublisher, group)
	private := mkVisibleApp(t, db, "hidden", database.VisibilityPrivate, "flask")
	grantTo(t, db, private.ID, database.PrincipalTypeUser, user.ID, database.ObjectRoleList)
	grantTo(t, db, private.ID, database.PrincipalTypeGroup, group.ID, database.ObjectRoleList)
	open := mkVisibleApp(t, db, "open", database.VisibilityPublic, "flask")

	anon, err := db.ListApps("", "", nil)
	if err != nil {
		t.Fatalf("ListApps(anonymous): %v", err)
	}
	got := slugs(anon)
	if got[private.Slug] {
		t.Error("a list grant put a private app in the anonymous listing; the rung names a principal and anonymous is not one")
	}
	if !got[open.Slug] {
		t.Error("the public app left the anonymous listing")
	}
}

// EffectiveRole resolves the rung to its own word rather than to the empty
// string. It matters because the refusal the API writes is chosen from the
// effective role: a list holder who resolved as "" would be told 404 like a
// stranger, and the whole feature is the difference between those two answers.
func TestEffectiveRoleResolvesTheRung(t *testing.T) {
	db := dbtest.New(t)

	group := mkGroup(t, db, "listers")
	user := mkUserIn(t, db, "u-rung", database.SystemRolePublisher, group)
	app := mkVisibleApp(t, db, "rung", database.VisibilityPrivate, "flask")

	grantTo(t, db, app.ID, database.PrincipalTypeUser, user.ID, database.ObjectRoleList)
	role, err := db.EffectiveRole(app.ID, database.PrincipalTypeUser, user.ID, []string{group.ID})
	if err != nil {
		t.Fatalf("EffectiveRole: %v", err)
	}
	if role != database.ObjectRoleList {
		t.Fatalf("a direct list grant resolves as %q, want %q", role, database.ObjectRoleList)
	}

	// And a stronger grant elsewhere still wins: the rung is the bottom of the
	// ladder, not a veto.
	grantTo(t, db, app.ID, database.PrincipalTypeGroup, group.ID, database.ObjectRoleViewer)
	role, err = db.EffectiveRole(app.ID, database.PrincipalTypeUser, user.ID, []string{group.ID})
	if err != nil {
		t.Fatalf("EffectiveRole: %v", err)
	}
	if role != database.ObjectRoleViewer {
		t.Fatalf("a viewer grant beside a list grant resolves as %q, want %q", role, database.ObjectRoleViewer)
	}
}

// The install-wide rung: an administrator says every signed-in person may see
// that every app exists, and nobody gains access to anything.
//
// The two halves have to agree or the feature is a bug report. The listing
// clause and the authorizer's floor are one fact carried on the app row, for
// the reason the anonymity suppressors are: an app that appears in somebody's
// list and 404s when they open it is exactly the disagreement that fact exists
// to prevent, and it is invisible to a test that checks only one of them.
func TestTheInstallWideRungListsWithoutOpening(t *testing.T) {
	db := dbtest.New(t)

	stranger := mkUserIn(t, db, "u-everyone", database.SystemRolePublisher, nil)
	p := &auth.Principal{
		Type: database.PrincipalTypeUser, ID: stranger.ID,
		SystemRole: database.SystemRolePublisher,
	}
	app := mkVisibleApp(t, db, "someone-elses", database.VisibilityPrivate, "flask")

	// **On, from an absent row.** This is the upgrade path and it is the one
	// behaviour nobody will ever test by hand: every developer database has a
	// settings row in it within a minute of somebody opening the page, so the
	// only place the absence is ever observed is an install that has just been
	// upgraded and belongs to somebody else. A query asking for 'true' passes
	// every other assertion in this file and fails only here.
	listed, err := db.ListApps(stranger.ID, database.PrincipalTypeUser, nil)
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if !slugs(listed)[app.Slug] {
		t.Fatal("a private app was absent from a stranger's listing with no settings row at all — " +
			"absence must read as on, or every install that never opened the settings page keeps " +
			"the old behaviour for ever")
	}

	// Off, explicitly, which is the only way off is now reachable: back to what
	// the install did before the rung existed.
	if _, err := db.PutSetting(database.SettingAppsListableToAuthenticated, "false", ""); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	listed, err = db.ListApps(stranger.ID, database.PrincipalTypeUser, nil)
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if slugs(listed)[app.Slug] {
		t.Fatal("a private app was listed to somebody holding nothing on it, with the switch off")
	}

	// And on again, written this time rather than absent, because the two have
	// to be the same answer.
	if _, err := db.PutSetting(database.SettingAppsListableToAuthenticated, "true", ""); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}

	listed, err = db.ListApps(stranger.ID, database.PrincipalTypeUser, nil)
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if !slugs(listed)[app.Slug] {
		t.Error("the switch is on and the app is not in the listing")
	}

	fresh, err := db.GetApp(app.ID)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	obj := fresh.AuthObject()
	if auth.CanRead(p, obj, db) {
		t.Error("the switch made a private app readable; it confers the rung and nothing above it")
	}
	// **Authorize's answer, not the ladder's.** The install-wide rung is a floor
	// in Authorize; EffectiveObjectRole is what a principal's *explicit*
	// relationships confer and says "none" here, correctly — nobody gave this
	// person anything. Asking the ladder was what folded an install-wide fact
	// into the one function three callers read as "what were they given", and it
	// cost an administrator with no grant the ability to open any private app.
	if d := auth.Authorize(p, auth.ActionRead, obj, db); d.EffectiveRole != database.ObjectRoleList {
		t.Errorf("effective role under the switch is %q, want %q — a caller resolving as anything "+
			"else meets a 404 on an app their own list just showed them",
			d.EffectiveRole, database.ObjectRoleList)
	}
	if role := auth.EffectiveObjectRole(p, obj, db); role != "none" {
		t.Errorf("the ladder says %q for somebody holding nothing, want none — the install-wide "+
			"rung is not a relationship and must not appear here", role)
	}

	// Anonymous is untouched: the switch is a statement about people who can
	// sign in, and there is nothing below `none` to raise a stranger from.
	anon, err := db.ListApps("", "", nil)
	if err != nil {
		t.Fatalf("ListApps(anonymous): %v", err)
	}
	if slugs(anon)[app.Slug] {
		t.Error("the switch put a private app in the anonymous listing")
	}

	// And it is a floor, never a ceiling: a real grant still wins.
	grantTo(t, db, app.ID, database.PrincipalTypeUser, stranger.ID, database.ObjectRoleViewer)
	if d := auth.Authorize(p, auth.ActionRead, fresh.AuthObject(), db); d.EffectiveRole != database.ObjectRoleViewer {
		t.Errorf("a viewer grant under the switch resolves as %q, want %q",
			d.EffectiveRole, database.ObjectRoleViewer)
	}
}

func slugs(apps []database.App) map[string]bool {
	out := map[string]bool{}
	for _, a := range apps {
		out[a.Slug] = true
	}
	return out
}
