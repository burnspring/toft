package api

import (
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

// The door, and what it does not do (ADR §70).
//
// The switch has two halves and they fail in opposite directions. The gate here
// refuses the *transition*, so a mistake makes it possible to publish on an
// install that does not publish. The substitution — which lives in
// AuthObjectOf and is tested in internal/database — withdraws the *anonymity*,
// so a mistake there leaves an app on the internet with nothing on any screen
// saying so. This file is the first half, plus the one thing the two share: no
// column is written either way.

func publicAppsTestEnv(t *testing.T) (*Server, http.Handler, *database.App) {
	t.Helper()

	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	app := &database.App{
		Slug: "payroll", Name: "Payroll", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate,
	}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if _, err := srv.db.PutSetting(database.SettingPublicAppsEnabled, "false", ""); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	return srv, srv.Router(http.NotFoundHandler()), app
}

func TestPublicAppsOff_RefusesTheTransitionToPublic(t *testing.T) {
	srv, router, app := publicAppsTestEnv(t)

	rec := patchAppAs(t, srv, router, "owner", app.ID, `{"visibility":"public"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("publishing with public apps off = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	// The code, not the prose: the SPA switches on it, and this refusal must
	// never be mistaken for the license's — one is bought off and the other is
	// this install's own administrator saying no.
	if !strings.Contains(rec.Body.String(), `"code":"public_apps_disabled"`) {
		t.Errorf("no public_apps_disabled code: %s", rec.Body.String())
	}

	after, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Visibility != database.VisibilityPrivate {
		t.Errorf("visibility = %q — the refused change was written anyway", after.Visibility)
	}
	if n := countVisibilityAudits(t, srv, app.ID); n != 0 {
		t.Errorf("%d audit rows for a refusal — a refused change is not a change", n)
	}
}

// The narrowing directions stay open. An owner who cannot publish must still be
// able to take access away, and refusing that would strand an app nobody can
// tighten on the one setting an administrator has just decided against.
func TestPublicAppsOff_NarrowingIsStillAllowed(t *testing.T) {
	srv, router, app := publicAppsTestEnv(t)

	for _, to := range []string{database.VisibilityAuthenticated, database.VisibilityPrivate} {
		rec := patchAppAs(t, srv, router, "owner", app.ID, `{"visibility":"`+to+`"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("moving to %q = %d, want 200 (%s)", to, rec.Code, rec.Body.String())
		}
	}
}

// An app that is already public keeps its column, and the switch is what makes
// it stop serving anonymous visitors — not a sweep, and not an edit.
//
// This is the case the whole design turns on: the owner's sentence is still
// `public`, so turning the switch back on republishes it with nobody having to
// remember which apps to restore.
func TestPublicAppsOff_AnAlreadyPublicAppKeepsItsColumn(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)

	app := &database.App{
		Slug: "reports", Name: "Reports", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPublic,
	}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatalf("create app: %v", err)
	}
	if _, err := srv.db.PutSetting(database.SettingPublicAppsEnabled, "false", ""); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}

	got, err := srv.db.GetApp(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Visibility != database.VisibilityPublic {
		t.Errorf("visibility = %q, want %q — the platform wrote the owner's column",
			got.Visibility, database.VisibilityPublic)
	}
	if !got.PublicWithheld {
		t.Fatal("the app row does not carry the install's decision")
	}
	if v := objectFromApp(got).Visibility; v != database.VisibilityAuthenticated {
		t.Errorf("the authorizer sees %q, want %q", v, database.VisibilityAuthenticated)
	}

	// Re-sending `public` on an app that is already public is not a change, so
	// the gate must not fire on it: the owner is not publishing anything, and
	// a 403 here would be the install refusing a request that asks for nothing.
	if rec := patchAppAs(t, srv, routerFor(srv), "owner", app.ID, `{"visibility":"public"}`); rec.Code != http.StatusOK {
		t.Errorf("re-sending the visibility it already has = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func routerFor(srv *Server) http.Handler { return srv.Router(http.NotFoundHandler()) }
