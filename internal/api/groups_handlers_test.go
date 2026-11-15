package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

type groupResponse struct {
	Group   *database.Group `json:"group"`
	Members []database.User `json:"members"`
}

// newGroupTestEnv builds a group containing "insider" but not "outsider".
func newGroupTestEnv(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()

	srv := newTestServer(t)

	seedUser(t, srv.db, "insider", database.SystemRolePublisher)
	seedUser(t, srv.db, "outsider", database.SystemRolePublisher)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	g, err := srv.db.CreateGroup(t.Context(), "platform-team", "root", database.RequestMeta{})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := srv.db.AddGroupMember(t.Context(), g.ID, "insider", "root", database.RequestMeta{}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	return srv, srv.Router(http.NotFoundHandler()), g.ID
}

func decodeGroup(t *testing.T, body []byte) (groupResponse, map[string]any) {
	t.Helper()

	var g groupResponse
	if err := json.Unmarshal(body, &g); err != nil {
		t.Fatalf("decode group: %v (body %q)", err, body)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	return g, raw
}

// handleGetGroup had no principal check, so an anonymous caller with a group id
// got the roster including email addresses and system roles. Finding 0.4.
func TestGetGroupRejectsAnonymousCallers(t *testing.T) {
	srv, router, groupID := newGroupTestEnv(t)

	rec := getAs(t, srv, router, "", "/api/v1/groups/"+groupID)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "insider@example.com") {
		t.Errorf("member email leaked to an anonymous caller: %q", body)
	}
}

// Any authenticated user can already enumerate group ids via GET /groups, so the
// roster itself is what has to be withheld from non-members.
func TestGetGroupWithholdsRosterFromNonMembers(t *testing.T) {
	srv, router, groupID := newGroupTestEnv(t)

	rec := getAs(t, srv, router, "outsider", "/api/v1/groups/"+groupID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	g, raw := decodeGroup(t, rec.Body.Bytes())
	if g.Group == nil || g.Group.Name != "platform-team" {
		t.Error("the group itself should still be visible")
	}
	if _, present := raw["members"]; present {
		t.Errorf("roster returned to a non-member: %v", raw["members"])
	}
	if strings.Contains(rec.Body.String(), "insider") {
		t.Error("member identity leaked to a non-member")
	}
}

// A member sees who else is in the group, but email is directory data and is
// admin-only.
func TestGetGroupGivesMembersARosterWithoutEmails(t *testing.T) {
	srv, router, groupID := newGroupTestEnv(t)

	rec := getAs(t, srv, router, "insider", "/api/v1/groups/"+groupID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	g, _ := decodeGroup(t, rec.Body.Bytes())
	if len(g.Members) != 1 || g.Members[0].ID != "insider" {
		t.Fatalf("roster = %+v, want just the insider", g.Members)
	}
	if g.Members[0].Email != "" {
		t.Errorf("email returned to a non-admin member: %q", g.Members[0].Email)
	}
}

func TestGetGroupGivesAdminsTheFullRoster(t *testing.T) {
	srv, router, groupID := newGroupTestEnv(t)

	rec := getAs(t, srv, router, "root", "/api/v1/groups/"+groupID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	g, _ := decodeGroup(t, rec.Body.Bytes())
	if len(g.Members) != 1 {
		t.Fatalf("roster = %+v, want one member", g.Members)
	}
	if g.Members[0].Email != "insider@example.com" {
		t.Errorf("admin should see emails, got %q", g.Members[0].Email)
	}
}

func TestGetGroupIsStill404ForAnUnknownGroup(t *testing.T) {
	srv, router, _ := newGroupTestEnv(t)

	rec := getAs(t, srv, router, "root", fmt.Sprintf("/api/v1/groups/%s", "no-such-group"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// VN-2 (2026-11-15): a group address is one address. A list typed into it
// fails to parse when a notice is sent, so it is refused when saved.
func TestGroupEmailMustBeOneAddress(t *testing.T) {
	srv, router, groupID := newGroupTestEnv(t)

	rec := sendJSONAs(t, srv, router, "root", http.MethodPatch, "/api/v1/groups/"+groupID, `{"email":"a@x.org, b@x.org"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a list of addresses: status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	rec = sendJSONAs(t, srv, router, "root", http.MethodPatch, "/api/v1/groups/"+groupID, `{"email":"finance@example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("one address: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}
