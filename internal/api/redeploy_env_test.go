package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// **A deploy never touches an app's environment.** Values are configuration
// the app's owner — or, on a promotion target, that install's administrator —
// set by hand, and the only writers are a create and an explicit edit. A new
// version uploaded and a redeploy of the branch each leave every name, value
// and sensitive flag exactly as they were (a rollback needs a release on disk
// this server does not have, and writes no env either): the Migration Console
// ships every later release to an app whose values were typed in over there,
// and a deploy that reset them would undo that on each push.
func TestARedeployKeepsTheEnvironment(t *testing.T) {
	e := newCreateModeEnv(t)

	type envRow struct {
		value     string
		sensitive bool
	}
	read := func(appID string) map[string]envRow {
		t.Helper()
		vars, err := e.srv.db.GetEnvVars(appID)
		if err != nil {
			t.Fatalf("env: %v", err)
		}
		out := map[string]envRow{}
		for _, v := range vars {
			plain, err := e.srv.encryptor.Decrypt(v.Value)
			if err != nil {
				t.Fatalf("decrypt %s: %v", v.Key, err)
			}
			out[v.Key] = envRow{plain, v.Sensitive}
		}
		return out
	}
	settle := func() {
		t.Helper()
		if _, err := e.srv.db.Conn().Exec(
			`UPDATE deployments SET status = 'success', finished_at = now() WHERE status <> 'success'`); err != nil {
			t.Fatalf("settling deployments: %v", err)
		}
	}
	post := func(path, contentType, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Origin", e.srv.cfg.BaseURL)
		req.AddCookie(sessionCookieFor(t, e.srv, "root"))
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		return rec
	}
	want := map[string]envRow{"GREETING": {"hello", false}, "API_TOKEN": {"s3cret", true}}
	same := func(step string, got map[string]envRow) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: env = %v, want %v", step, got, want)
		}
		for k, w := range want {
			if got[k] != w {
				t.Errorf("%s: %s = %+v, want %+v", step, k, got[k], w)
			}
		}
	}

	// A git app: redeploy the branch.
	rec := e.create(t, "root", `{"repo_url":"https://github.com/acme/tool","slug":"envkeep","name":"envkeep",
		"env_vars":{"GREETING":"hello","API_TOKEN":"s3cret"},"env_sensitive":{"API_TOKEN":true}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == "" {
		app := e.appBySlug(t, "envkeep")
		created.ID = app.ID
	}
	same("after create", read(created.ID))
	settle()
	if rec := post("/api/v1/apps/"+created.ID+"/deploy", "application/json", `{}`); rec.Code >= 300 {
		t.Fatalf("redeploy: %d %s", rec.Code, rec.Body.String())
	}
	same("after a redeploy", read(created.ID))

	// An uploaded app: a new version, with a slug and visibility riding along
	// as the Migration Console sends them.
	upRec := e.upload(t, "root", "envkeep-up", "")
	if upRec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", upRec.Code, upRec.Body.String())
	}
	upApp := e.appBySlug(t, "envkeep-up")
	settle()
	edit := `{"set":{"GREETING":{"value":"hello","sensitive":false},"API_TOKEN":{"value":"s3cret","sensitive":true}}}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+upApp.ID+"/env", strings.NewReader(edit))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.srv.cfg.BaseURL)
	req.AddCookie(sessionCookieFor(t, e.srv, "root"))
	patched := httptest.NewRecorder()
	e.router.ServeHTTP(patched, req)
	if patched.Code != http.StatusOK {
		t.Fatalf("setting env: %d %s", patched.Code, patched.Body.String())
	}
	same("after the edit", read(upApp.ID))

	var buf strings.Builder
	const boundary = "----tofttest"
	for k, v := range map[string]string{"slug": "envkeep-up", "visibility": "authenticated"} {
		buf.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"" + k + "\"\r\n\r\n" + v + "\r\n")
	}
	buf.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"bundle.zip\"\r\n")
	buf.WriteString("Content-Type: application/zip\r\n\r\nPK\x05\x06" + strings.Repeat("\x00", 18) + "\r\n--" + boundary + "--\r\n")
	if rec := post("/api/v1/apps/"+upApp.ID+"/upload", "multipart/form-data; boundary="+boundary, buf.String()); rec.Code >= 300 {
		t.Fatalf("new version: %d %s", rec.Code, rec.Body.String())
	}
	same("after a new version", read(upApp.ID))
}
