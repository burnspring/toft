package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"toft/internal/database"
)

// The account record is cross-tenant introspection: it names a user's groups
// and the external logins that reach the account. Only an admin may read it,
// and a non-admin must not learn whether the id exists at all.
func TestUserDetail_AdminOnly_404sForOthers(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "bob", database.SystemRolePublisher)

	rec := getAs(t, srv, router, "bob", "/api/v1/users/root")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("publisher reading an account record: %d, want 404", rec.Code)
	}

	// Not even their own record — this route is the admin view, and /me is
	// where a user reads themselves.
	rec = getAs(t, srv, router, "bob", "/api/v1/users/bob")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("publisher reading their own record here: %d, want 404", rec.Code)
	}

	rec = getAs(t, srv, router, "", "/api/v1/users/bob")
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
		t.Fatalf("anonymous: %d, want 401 or 404", rec.Code)
	}

	rec = getAs(t, srv, router, "root", "/api/v1/users/nobody")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin reading a missing user: %d, want 404", rec.Code)
	}
}

type userDetailBody struct {
	User struct {
		ID          string  `json:"id"`
		Login       string  `json:"login"`
		SystemRole  string  `json:"system_role"`
		CreatedAt   string  `json:"created_at"`
		LastLoginAt *string `json:"last_login_at"`
		LockedAt    *string `json:"locked_at"`
	} `json:"user"`
	Groups []struct {
		ID      string  `json:"id"`
		Name    string  `json:"name"`
		AddedAt *string `json:"added_at"`
	} `json:"groups"`
	Identities []struct {
		ProviderID string `json:"provider_id"`
	} `json:"identities"`
}

func TestUserDetail_ReportsGroupsAndLastLogin(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "bob", database.SystemRolePublisher)

	g, err := srv.db.CreateGroup(t.Context(), "platform-team", "root", database.RequestMeta{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "bob", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// A seeded user has never signed in, and that has to read as "never"
	// rather than as the zero time.
	rec := getAs(t, srv, router, "root", "/api/v1/users/bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin read: %d body=%s", rec.Code, rec.Body.String())
	}
	var body userDetailBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.User.ID != "bob" || body.User.SystemRole != database.SystemRolePublisher {
		t.Fatalf("wrong user in record: %+v", body.User)
	}
	if body.User.LastLoginAt != nil {
		t.Fatalf("never-signed-in user reports last_login_at = %v", *body.User.LastLoginAt)
	}
	if len(body.Groups) != 1 || body.Groups[0].Name != "platform-team" {
		t.Fatalf("groups = %+v, want the one membership", body.Groups)
	}
	if body.Groups[0].AddedAt == nil {
		t.Fatal("membership made through AddGroupMember has no added_at")
	}

	// Minting a session is what counts as a sign-in.
	if _, err := srv.mintSession(&database.User{ID: "bob", Login: "bob", SystemRole: database.SystemRolePublisher}); err != nil {
		t.Fatalf("mint session: %v", err)
	}
	rec = getAs(t, srv, router, "root", "/api/v1/users/bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin read after sign-in: %d", rec.Code)
	}
	body = userDetailBody{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.User.LastLoginAt == nil {
		t.Fatal("last_login_at still absent after a session was minted")
	}
}
