package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/database"
)

// Every door that brings code writes one app.deploy, naming who started it,
// the deployment, its trigger and how the code came in. "Who deployed this"
// is the first question of a change review, and before this the only build
// that named a person was the rollback, which changes no code.
func TestEveryDeployDoorWritesOneAppDeploy(t *testing.T) {
	e := newCreateModeEnv(t)
	settle := func() {
		t.Helper()
		if _, err := e.srv.db.Conn().Exec(
			`UPDATE deployments SET status = 'success', finished_at = now() WHERE status <> 'success'`); err != nil {
			t.Fatal(err)
		}
	}
	post := func(path, contentType, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Origin", e.srv.cfg.BaseURL)
		req.AddCookie(sessionCookieFor(t, e.srv, "root"))
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		return rec
	}
	type row struct {
		actorType, actorID string
		after              map[string]string
	}
	deploys := func(appID string) []row {
		t.Helper()
		rows, err := e.srv.db.Conn().Query(`SELECT actor_type, COALESCE(actor_id, ''), after::text
			FROM audit_log WHERE action = ? AND app_id = ? ORDER BY occurred_at`, database.ActionAppDeploy, appID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			var after string
			if err := rows.Scan(&r.actorType, &r.actorID, &after); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal([]byte(after), &r.after)
			out = append(out, r)
		}
		return out
	}
	check := func(step string, got []row, n int, via string) {
		t.Helper()
		if len(got) != n {
			t.Fatalf("%s: %d app.deploy rows, want %d", step, len(got), n)
		}
		last := got[n-1]
		if last.actorType != "user" || last.actorID == "" {
			t.Errorf("%s: actor = (%s, %q), want the person", step, last.actorType, last.actorID)
		}
		if last.after["via"] != via || last.after["trigger"] != database.DeployTriggerManual || last.after["deployment_id"] == "" {
			t.Errorf("%s: after = %v, want via %s, manual, a deployment", step, last.after, via)
		}
	}

	// From git: the create's first build, then a redeploy.
	if rec := e.create(t, "root", `{"repo_url":"https://github.com/acme/tool","slug":"who","name":"who"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	gitApp := e.appBySlug(t, "who")
	check("create from git", deploys(gitApp.ID), 1, "git")
	settle()
	if rec := post("/api/v1/apps/"+gitApp.ID+"/deploy", "application/json", `{}`); rec.Code >= 300 {
		t.Fatalf("redeploy: %d %s", rec.Code, rec.Body.String())
	}
	check("redeploy", deploys(gitApp.ID), 2, "git")

	// From an archive: the create, then a new version.
	if rec := e.upload(t, "root", "who-up", ""); rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	upApp := e.appBySlug(t, "who-up")
	check("upload create", deploys(upApp.ID), 1, "upload")
	settle()
	var buf strings.Builder
	const boundary = "----tofttest"
	buf.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"bundle.zip\"\r\n")
	buf.WriteString("Content-Type: application/zip\r\n\r\nPK\x05\x06" + strings.Repeat("\x00", 18) + "\r\n--" + boundary + "--\r\n")
	if rec := post("/api/v1/apps/"+upApp.ID+"/upload", "multipart/form-data; boundary="+boundary, buf.String()); rec.Code >= 300 {
		t.Fatalf("new version: %d %s", rec.Code, rec.Body.String())
	}
	check("new version", deploys(upApp.ID), 2, "upload")
}
