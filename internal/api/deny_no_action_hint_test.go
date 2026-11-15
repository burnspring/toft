package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

// A viewer of an app who tries to deploy it, or to change its settings, is
// refused by the app's own ladder — deny_no_action, not deny_system_role, which
// is only creating an app. The refusal used to be the bare word "forbidden":
// nothing to do next, so it read as a broken command (black-box BB-1,
// 2026-11-04). It now names the act — ask the owner for editor — and the step
// before it for an account whose system role is viewer, which cannot hold
// editor until it is a publisher.
func TestAViewerOfAnAppIsToldWhatToAskFor(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "vee", database.SystemRoleViewer)
	app := &database.App{Slug: "claims", Name: "Claims", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityAuthenticated}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser, "vee",
		database.ObjectRoleViewer, "owner", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, method, path, body string }{
		{"deploy", http.MethodPost, "/api/v1/apps/" + app.ID + "/deploy", `{}`},
		{"settings", http.MethodPatch, "/api/v1/apps/" + app.ID, `{"name":"Claims 2"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := sendJSONAs(t, srv, router, "vee", c.method, c.path, c.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("a viewer's %s = %d, want 403 (%s)", c.name, rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["code"] != "deny_no_action" {
				t.Fatalf("code = %v, want deny_no_action (%s)", body["code"], rec.Body.String())
			}
			hint, _ := body["hint"].(string)
			for _, want := range []string{"editor", "access-requests", "promotion-requests"} {
				if !strings.Contains(hint, want) {
					t.Errorf("the hint never names %q, so the viewer has no next step: %q", want, hint)
				}
			}
		})
	}
}

// Deleting an app, changing who can see it and managing who has access are the
// owner's alone. An editor refused one of them used to get deny_no_action's
// hint — ask the owner to make you an editor — which it already was, so the
// owner got mail asking for a role the caller held (black-box BB-32,
// 2026-11-05). They are deny_owner_only now, for an editor and a viewer alike,
// and the hint names the owner, never the access request.
func TestAnOwnerOnlyActionNamesTheOwnerNotAnAccessRequest(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "eddie", database.SystemRolePublisher)
	seedUser(t, srv.db, "vee", database.SystemRolePublisher)
	app := &database.App{Slug: "ledger", Name: "Ledger", OwnerID: "owner",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	for who, role := range map[string]string{"eddie": database.ObjectRoleEditor, "vee": database.ObjectRoleViewer} {
		if err := srv.db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser, who,
			role, "owner", database.RequestMeta{}); err != nil {
			t.Fatal(err)
		}
	}

	for _, who := range []string{"eddie", "vee"} {
		for _, c := range []struct{ name, method, path, body string }{
			{"delete", http.MethodDelete, "/api/v1/apps/" + app.ID, ""},
			{"visibility", http.MethodPatch, "/api/v1/apps/" + app.ID, `{"visibility":"authenticated"}`},
			{"grants", http.MethodGet, "/api/v1/apps/" + app.ID + "/grants", ""},
		} {
			t.Run(who+"/"+c.name, func(t *testing.T) {
				rec := sendJSONAs(t, srv, router, who, c.method, c.path, c.body)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("%s = %d, want 403 (%s)", c.name, rec.Code, rec.Body.String())
				}
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body["code"] != "deny_owner_only" {
					t.Fatalf("code = %v, want deny_owner_only (%s)", body["code"], rec.Body.String())
				}
				hint, _ := body["hint"].(string)
				if strings.Contains(hint, "access-requests") {
					t.Errorf("the hint sends the caller to ask for a role that cannot help: %q", hint)
				}
				if !strings.Contains(hint, "owner") {
					t.Errorf("the hint never names who can do it: %q", hint)
				}
			})
		}
	}
}
