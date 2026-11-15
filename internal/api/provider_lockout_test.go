package api

import (
	"bytes"
	"net/http"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/password"
)

// The other half of TestLocalSignInDisableRequiresAnotherWayIn.
//
// That guard has existed for as long as local sign-in has been switchable, and
// it only ever watched one of the two doors: an install running on a single
// identity provider with local sign-in already off could be switched off — or
// deleted — from the admin page, leaving nobody able to sign in and nobody able
// to undo it without `toft local-admin enable` on the host.

// lockoutEnv is an install with no environment-configured provider, so the only
// ways in are the ones a test puts there. `providerTestEnv` carries an
// environment GitHub provider, which is exactly the second way in these cases
// are about not having.
func lockoutEnv(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	db := dbtest.New(t, database.WithBootstrapAdminPassword(password.Default))
	srv := newTestServerWithConfig(t, db, &config.Config{BaseURL: "http://localhost:8080"})
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	return srv, srv.Router(http.NotFoundHandler())
}

// linkIdentity is what "an administrator can actually sign in through this"
// means: a row in user_identities, not a provider that merely exists.
func linkIdentity(t *testing.T, srv *Server, providerID, userID string) {
	t.Helper()
	if _, err := srv.db.Conn().Exec(`
		INSERT INTO user_identities (provider_id, subject, user_id, email, login)
		VALUES (?, ?, ?, 'root@example.com', 'root')`,
		providerID, "sub-"+providerID, userID); err != nil {
		t.Fatalf("linking %s to %s: %v", userID, providerID, err)
	}
}

func createOkta(t *testing.T, srv *Server, router http.Handler) {
	t.Helper()
	rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/auth-providers", oktaBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating okta = %d, want 201 body=%s", rec.Code, rec.Body.String())
	}
}

func TestDisablingTheLastWayInIsRefused(t *testing.T) {
	srv, router := lockoutEnv(t)
	createOkta(t, srv, router)
	// Honestly reached: local sign-in may only go off once an administrator can
	// get in through the provider this test then tries to take down.
	linkIdentity(t, srv, "okta", "root")
	disableLocalSignIn(t, srv)

	rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/okta", `{"enabled":false}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("disabling the only provider = %d, want 422 body=%s", rec.Code, rec.Body.String())
	}
	// A refusal somebody can act on names the way out, and the only way out
	// here is on the machine.
	if !bytes.Contains(rec.Body.Bytes(), []byte("local-admin enable")) {
		t.Errorf("the refusal must name the command that recovers it, got: %s", rec.Body.String())
	}

	p, err := srv.db.GetAuthProvider("okta")
	if err != nil {
		t.Fatalf("reading the provider: %v", err)
	}
	if !p.Enabled {
		t.Error("the provider was disabled after the API refused to disable it")
	}
}

// A delete closes the door as surely as the switch does, and more permanently.
// A guard on only one of them is a guard the other walks past.
func TestDeletingTheLastWayInIsRefused(t *testing.T) {
	srv, router := lockoutEnv(t)
	createOkta(t, srv, router)
	linkIdentity(t, srv, "okta", "root")
	disableLocalSignIn(t, srv)

	rec := sendAs(t, srv, router, "root", http.MethodDelete,
		"/api/v1/admin/auth-providers/okta", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("deleting the only provider = %d, want 422 body=%s", rec.Code, rec.Body.String())
	}
	if _, err := srv.db.GetAuthProvider("okta"); err != nil {
		t.Fatalf("the provider is gone after a refused delete: %v", err)
	}
}

// Local password sign-in is the guarantee — the built-in administrator is
// seeded by the schema and re-created if deleted — so while it is on the login
// page this refusal has nothing to say.
func TestDisablingAProviderIsAllowedWhileLocalSignInIsOn(t *testing.T) {
	srv, router := lockoutEnv(t)
	createOkta(t, srv, router)

	rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/okta", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabling with local sign-in on = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
}

// The test is not "does another provider exist" but "can an administrator
// actually get in through one" — the same distinction the local half makes.
func TestDisablingAProviderNeedsAnAdministratorOnAnother(t *testing.T) {
	// This fixture carries an environment-configured GitHub provider, which is
	// the second door.
	srv, router := providerTestEnv(t)
	createOkta(t, srv, router)
	linkIdentity(t, srv, "okta", "root")
	disableLocalSignIn(t, srv)

	// GitHub is on the login page and nobody has ever signed in through it.
	rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/okta", `{"enabled":false}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("disable with no administrator on github = %d, want 422 body=%s", rec.Code, rec.Body.String())
	}

	linkIdentity(t, srv, "github", "root")
	rec = sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/okta", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable with an administrator on github = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	p, err := srv.db.GetAuthProvider("okta")
	if err != nil {
		t.Fatalf("reading the provider: %v", err)
	}
	if p.Enabled {
		t.Error("the provider is still enabled after a successful disable")
	}
}

// An edit that is not a disable is not this guard's business: the last provider
// on the login page may still be renamed, re-issued and re-credentialed.
func TestTheGuardOnlyStopsTheDisable(t *testing.T) {
	srv, router := lockoutEnv(t)
	createOkta(t, srv, router)
	linkIdentity(t, srv, "okta", "root")
	disableLocalSignIn(t, srv)

	rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/okta", `{"display_name":"Acme SSO"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("renaming the only provider = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
}
