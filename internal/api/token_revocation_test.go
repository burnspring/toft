package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"toft/internal/database"
)

// A demoted admin must lose admin the moment the demotion commits — not when
// their 8-hour JWT expires. Without revocation the last-admin invariant is
// bypassable in effect. Regression test for hardening task 1.5.
func TestDemotionRevokesLiveSessions(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "second-admin", database.SystemRoleAdmin)

	// The session is minted while second-admin is still an admin.
	oldSession := sessionCookieFor(t, srv, "second-admin")

	// Sanity: it works before the demotion.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.AddCookie(oldSession)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-demotion admin request: %d, want 200", rec.Code)
	}

	// root demotes second-admin.
	body, _ := json.Marshal(map[string]string{"system_role": database.SystemRoleViewer})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/users/second-admin/role", bytes.NewReader(body))
	req.AddCookie(sessionCookieFor(t, srv, "root"))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("demotion: %d body=%s", rec.Code, rec.Body.String())
	}

	// The old session must be dead, not admin-for-8-more-hours.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.AddCookie(oldSession)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("demoted admin's old session still has admin access")
	}
}

// Logout must revoke the token server-side; clearing cookies alone leaves a
// captured token valid until exp.
func TestLogoutRevokesTheToken(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	session := sessionCookieFor(t, srv, "root")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d, want 204", rec.Code)
	}

	// Replay the captured token.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("token replayed after logout still works")
	}
}

// A revoked token presented to logout again ends nothing: the sessions minted
// after it stay live and no second sign-out is audited (security review
// 2026-11-14, SI-5).
func TestLogoutWithARevokedTokenEndsNothing(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	stale := sessionCookieFor(t, srv, "root")

	logout := func(c *http.Cookie) {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("logout: %d, want 204", rec.Code)
		}
		if len(rec.Result().Cookies()) == 0 {
			t.Error("logout did not clear the cookies")
		}
	}
	first, err := srv.db.GetTokenVersion("root")
	if err != nil {
		t.Fatal(err)
	}
	logout(stale)
	before, err := srv.db.GetTokenVersion("root")
	if err != nil {
		t.Fatal(err)
	}
	if before == first {
		t.Fatal("a live session's logout did not bump token_version")
	}
	rowsBefore := len(signinRows(t, srv))

	logout(stale)
	after, err := srv.db.GetTokenVersion("root")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("a revoked token's logout moved token_version %d -> %d", before, after)
	}
	if n := len(signinRows(t, srv)); n != rowsBefore {
		t.Errorf("a revoked token's logout wrote %d audit row(s)", n-rowsBefore)
	}
}

// Removing a user from a group must revoke their sessions, since group
// membership is baked into the JWT claims.
func TestGroupRemovalRevokesSessions(t *testing.T) {
	srv := newTestServer(t)

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)

	g, err := srv.db.CreateGroup(t.Context(), "team", "root", database.RequestMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}

	afterAdd, err := srv.db.GetTokenVersion("member")
	if err != nil {
		t.Fatal(err)
	}

	if err := srv.db.RemoveGroupMember(t.Context(), g.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	afterRemove, err := srv.db.GetTokenVersion("member")
	if err != nil {
		t.Fatal(err)
	}
	if afterRemove <= afterAdd {
		t.Fatalf("token_version not bumped on removal: %d -> %d", afterAdd, afterRemove)
	}
}

// Deleting a group revokes every member's sessions.
func TestGroupDeleteRevokesMemberSessions(t *testing.T) {
	srv := newTestServer(t)

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "member", database.SystemRolePublisher)

	g, err := srv.db.CreateGroup(t.Context(), "team", "root", database.RequestMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "member", "root", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	before, err := srv.db.GetTokenVersion("member")
	if err != nil {
		t.Fatal(err)
	}

	if err := srv.db.DeleteGroup(t.Context(), g.ID, "root", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	after, err := srv.db.GetTokenVersion("member")
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("token_version not bumped on group delete: %d -> %d", before, after)
	}
}

// An administrator adding themselves to a group was signed out on the spot: the
// membership bump revoked every session they held, including the one that made
// the change. The bump still has to kill their *other* sessions — those carry
// the old groups — but the session making the request comes back re-issued,
// carrying the new group, and keeps working.
func TestChangingYourOwnMembershipKeepsTheSessionThatDidIt(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	g, err := srv.db.CreateGroup(t.Context(), "team", "root", database.RequestMeta{})
	if err != nil {
		t.Fatal(err)
	}

	elsewhere := sessionCookieFor(t, srv, "root") // another browser
	session := sessionCookieFor(t, srv, "root")

	send := func(method, path string, c *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(c)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	reissued := func(rec *httptest.ResponseRecorder) *http.Cookie {
		t.Helper()
		for _, c := range rec.Result().Cookies() {
			if c.Name == session.Name && c.Value != "" {
				return &http.Cookie{Name: c.Name, Value: c.Value}
			}
		}
		t.Fatalf("no re-issued session cookie on the response")
		return nil
	}

	rec := send(http.MethodPut, "/api/v1/groups/"+g.ID+"/members/root", session)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("add self = %d (%s)", rec.Code, rec.Body.String())
	}
	session = reissued(rec)
	if rec := send(http.MethodGet, "/api/v1/users", session); rec.Code != http.StatusOK {
		t.Fatalf("the session that added itself = %d after the add, want 200", rec.Code)
	}
	claims, err := srv.jwt.Validate(session.Value)
	if err != nil || len(claims.Groups) != 1 || claims.Groups[0] != g.ID {
		t.Fatalf("re-issued session groups = %v (%v), want [%s]", claims.Groups, err, g.ID)
	}
	if rec := send(http.MethodGet, "/api/v1/users", elsewhere); rec.Code == http.StatusOK {
		t.Fatal("the person's other session survived a change to their groups")
	}

	rec = send(http.MethodDelete, "/api/v1/groups/"+g.ID+"/members/root", session)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remove self = %d (%s)", rec.Code, rec.Body.String())
	}
	session = reissued(rec)
	if rec := send(http.MethodGet, "/api/v1/users", session); rec.Code != http.StatusOK {
		t.Fatalf("the session that removed itself = %d after the removal, want 200", rec.Code)
	}
}
