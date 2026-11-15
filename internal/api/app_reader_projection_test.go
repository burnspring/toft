package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"toft/internal/appint"
	"toft/internal/database"
)

// BB-106: a viewer of an app reads its record and its list row, but not which
// release serves, on what port, or when it was deployed — those are an
// editor's, as `toft status` already said by refusing them. The run-state word
// stays every reader's.
func TestAViewerReadsNoServingFieldsAndAnEditorReadsThemAll(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "vera", database.SystemRoleViewer)
	seedUser(t, srv.db, "ed", database.SystemRolePublisher)
	app := &database.App{Slug: "served", Name: "Served", OwnerID: "owner", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	// last_deployed_at is derived from deployments, and this app has none;
	// withholdServingFromReader blanks it beside these.
	if _, err := srv.db.Conn().Exec(`UPDATE apps SET current_release = 'served-abc', port = 3112,
		serving_since = ?, status_changed_at = ? WHERE id = ?`,
		time.Now(), time.Now(), app.ID); err != nil {
		t.Fatal(err)
	}
	for who, role := range map[string]string{"vera": database.ObjectRoleViewer, "ed": database.ObjectRoleEditor} {
		if err := srv.db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser,
			who, role, "owner", database.RequestMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	hidden := []string{"current_release", "serving_since", "status_changed_at"}

	check := func(who string, wantShown bool, rec map[string]any, where string) {
		t.Helper()
		for _, k := range hidden {
			v, ok := rec[k]
			shown := ok && v != nil && v != ""
			if shown != wantShown {
				t.Errorf("%s, %s: %s shown=%v (%v), want %v", who, where, k, shown, v, wantShown)
			}
		}
		if port, _ := rec["port"].(float64); (port != 0) != wantShown {
			t.Errorf("%s, %s: port = %v", who, where, rec["port"])
		}
		if rec["status"] != database.AppStatusRunning {
			t.Errorf("%s, %s: status = %v, want the run-state word", who, where, rec["status"])
		}
	}
	for who, editor := range map[string]bool{"vera": false, "ed": true} {
		for _, path := range []string{"/api/v1/apps/" + app.ID, "/api/v1/apps/" + app.ID + "?view=brief"} {
			rec := sendAs(t, srv, router, who, http.MethodGet, path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("%s GET %s: %d %s", who, path, rec.Code, rec.Body)
			}
			var body struct {
				App     map[string]any `json:"app"`
				Metrics any            `json:"metrics"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			check(who, editor, body.App, path)
			if !editor && body.Metrics != nil {
				t.Errorf("%s GET %s: metrics = %v", who, path, body.Metrics)
			}
		}
		rec := sendAs(t, srv, router, who, http.MethodGet, "/api/v1/apps", "")
		var list []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("%s: %v %s", who, err, rec.Body)
		}
		found := false
		for _, row := range list {
			if row["id"] == app.ID {
				found = true
				check(who, editor, row, "GET apps")
			}
		}
		if !found {
			t.Errorf("%s: the app is not in the list", who)
		}
	}
	// The detail handler still computes from the real row: the record in the
	// database is untouched.
	after, _ := srv.db.GetApp(app.ID)
	if after.CurrentRelease != "served-abc" {
		t.Errorf("the row itself was changed: %q", after.CurrentRelease)
	}
}

// WB-2 (security review 2026-11-14): GET /apps/{id} gave a reader below editor
// the attached credentials' inventory rows, each integration's destination and
// the summary facts the routes owning them refuse a viewer. A viewer gets
// none of them; an editor still gets all of them.
func TestAViewerReadsNoCredentialsDestinationsOrEditorSummaryFacts(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	seedUser(t, srv.db, "vera", database.SystemRoleViewer)
	seedUser(t, srv.db, "ed", database.SystemRolePublisher)
	app := &database.App{Slug: "assembled", Name: "Assembled", OwnerID: "owner", Framework: "flask",
		Status: database.AppStatusRunning, Visibility: database.VisibilityPrivate}
	if err := srv.db.CreateAppTx(app, "", "owner", "", "", ""); err != nil {
		t.Fatal(err)
	}
	for who, role := range map[string]string{"vera": database.ObjectRoleViewer, "ed": database.ObjectRoleEditor} {
		if err := srv.db.UpsertGrant(context.Background(), app.ID, database.PrincipalTypeUser,
			who, role, "owner", database.RequestMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	audit := database.AuditEntry{ActorType: database.PrincipalTypeUser, ActorID: "owner"}
	c, err := srv.db.CreateCredential(context.Background(), "warehouse", "the prod one",
		map[string]string{"WAREHOUSE_USER": "svc"}, srv.encryptor.Encrypt, audit)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.db.AttachCredential(context.Background(), app.ID, c.ID, audit); err != nil {
		t.Fatal(err)
	}
	conn := &database.IntegrationConnection{ID: "iconn-wb2", Kind: appint.IntegrationSlack,
		Name: "Workspace", ForApps: true,
		DefaultDestination: database.Destination{ID: "C0DEF", Label: "#finance-alerts"}}
	if err := srv.db.SaveConnection(conn, "", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.AttachAppIntegration(&database.AppIntegration{AppID: app.ID,
		Kind: appint.IntegrationSlack, ConnectionID: conn.ID, Enabled: true}, "", database.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.InsertAppEvent(&database.AppEvent{AppID: app.ID, Kind: "crash",
		Source: database.EventSourceHealth, Summary: "it fell over"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.CreateDeployment(&database.Deployment{AppID: app.ID, Status: database.DeployStatusSuccess}, nil); err != nil {
		t.Fatal(err)
	}

	editorOnly := []string{"last_failure_at", "quiet_since", "schedules", "schedules_enabled"}
	readerToo := []string{"grant_users", "grant_groups", "requests_30d", "viewers_30d", "quiet_days", "quiet_basis"}
	for who, editor := range map[string]bool{"vera": false, "ed": true} {
		rec := sendAs(t, srv, router, who, http.MethodGet, "/api/v1/apps/"+app.ID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", who, rec.Code, rec.Body)
		}
		var body struct {
			Credentials []map[string]any `json:"credentials"`
			Policy      struct {
				Integrations []map[string]any `json:"integrations"`
			} `json:"policy"`
			Summary map[string]any `json:"summary"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Credentials == nil {
			t.Errorf("%s: credentials is not a list: %s", who, rec.Body)
		}
		if got := len(body.Credentials) == 1; got != editor {
			t.Errorf("%s: credentials = %v", who, body.Credentials)
		}
		if len(body.Policy.Integrations) != 1 {
			t.Fatalf("%s: policy.integrations = %v, want the one attachment", who, body.Policy.Integrations)
		}
		in := body.Policy.Integrations[0]
		if in["kind"] != appint.IntegrationSlack || in["enabled"] != true {
			t.Errorf("%s: the reader's summary of the attachment is gone: %v", who, in)
		}
		if _, ok := in["destination"]; ok != editor {
			t.Errorf("%s: destination present=%v (%v)", who, ok, in["destination"])
		}
		if body.Summary == nil {
			t.Fatalf("%s: no summary", who)
		}
		for _, k := range editorOnly {
			if _, ok := body.Summary[k]; ok != editor {
				t.Errorf("%s: summary.%s present=%v", who, k, ok)
			}
		}
		for _, k := range readerToo {
			if _, ok := body.Summary[k]; !ok {
				t.Errorf("%s: summary.%s missing", who, k)
			}
		}
	}
}
