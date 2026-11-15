package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
)

// Helpers shared by the Workstream 0 authorization tests. These drive requests
// through the real router so routing, the auth middleware, and the handler are
// all exercised together — the findings they cover were all "the handler simply
// never asked", which a direct handler call can hide.

// seedUser creates a user with the given system role. UpsertUser deliberately
// does not write system_role — a login must never reset a role — so the role is
// set separately.
func seedUser(t *testing.T, db *database.DB, id, role string) *database.User {
	t.Helper()

	u := &database.User{ID: id, Login: id, Email: id + "@example.com"}
	if err := db.UpsertUser(u); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
	if err := db.UpdateUserRole(id, role); err != nil {
		t.Fatalf("set role for %s: %v", id, err)
	}
	u.SystemRole = role
	return u
}

// sessionCookieFor mints the session cookie the auth middleware expects.
func sessionCookieFor(t *testing.T, srv *Server, who string) *http.Cookie {
	t.Helper()

	u, err := srv.db.GetUser(who)
	if err != nil || u == nil {
		t.Fatalf("no such test user %q: %v", who, err)
	}
	version, err := srv.db.GetTokenVersion(u.ID)
	if err != nil {
		t.Fatalf("token version for %s: %v", who, err)
	}
	// Group claims, exactly as the sign-in path mints them. Left out for a long
	// time without consequence, because a group could only ever confer viewer
	// and the tests that needed one said so directly. Once a group can own an
	// app (ADR §51) a token without its groups is a different principal from
	// the one production would produce, and every group-derived permission
	// silently reads as 404.
	groups, err := srv.db.GetUserGroupIDs(u.ID)
	if err != nil {
		t.Fatalf("groups for %s: %v", who, err)
	}
	sess, err := srv.jwt.Generate(auth.GenerateParams{
		UserID: u.ID, Login: u.Login, SystemRole: u.SystemRole,
		Groups: groups, TokenVersion: version, Policy: srv.sessionPolicy(),
	})
	if err != nil {
		t.Fatalf("mint token for %s: %v", who, err)
	}
	return &http.Cookie{Name: auth.SessionCookie, Value: sess.Token}
}

// getAs issues a GET through the router as the named user, or anonymously when
// who is empty.
func getAs(t *testing.T, srv *Server, router http.Handler, who, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}
