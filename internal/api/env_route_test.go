package api

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
)

func patchEnvAs(t *testing.T, srv *Server, router http.Handler, who, appID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/apps/"+appID+"/env", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.cfg.BaseURL)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func envValues(t *testing.T, srv *Server, appID string) (map[string]string, map[string]bool) {
	t.Helper()
	values, flags := map[string]string{}, map[string]bool{}
	for _, v := range mustEnvVars(t, srv, appID) {
		plain, err := srv.encryptor.Decrypt(v.Value)
		if err != nil {
			t.Fatalf("Decrypt %s: %v", v.Key, err)
		}
		values[v.Key], flags[v.Key] = plain, v.Sensitive
	}
	return values, flags
}

func seedEnv(t *testing.T, srv *Server, appID, key, value string, sensitive bool) {
	t.Helper()
	enc, err := srv.encryptor.Encrypt(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.db.SetEnvVar(appID, key, enc, sensitive); err != nil {
		t.Fatalf("SetEnvVar %s: %v", key, err)
	}
}

// The merge: a key the body does not mention is untouched, "" clears a
// secret, a new key without a flag is visible, and the answer names keys
// and never a value.
func TestEnvRoute_Merge(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	seedEnv(t, srv, app.ID, "KEEP", "untouched", true)
	seedEnv(t, srv, app.ID, "SECRET", "hunter2", true)
	seedEnv(t, srv, app.ID, "LEGACY", "x", false)

	rec := patchEnvAs(t, srv, router, "owner", app.ID, `{
		"set": {"SECRET": {"value": ""}, "NEW": {"value": "rotated-secret", "sensitive": true}, "DB_HOST": {"value": "db"}},
		"unset": ["LEGACY", "NEVER_THERE"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH env = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"hunter2", "rotated-secret", "untouched"} {
		if strings.Contains(body, secret) {
			t.Fatalf("the answer carries a value: %s", body)
		}
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	join := func(k string) string {
		var out []string
		for _, v := range got[k].([]any) {
			out = append(out, v.(string))
		}
		return strings.Join(out, ",")
	}
	if join("set") != "DB_HOST,NEW" || join("cleared") != "SECRET" || join("unset") != "LEGACY" {
		t.Errorf("answer = %s", body)
	}
	if _, ok := got["restart_needed"]; ok {
		t.Errorf("the answer speaks of a restart, which is its own command: %s", body)
	}
	values, flags := envValues(t, srv, app.ID)
	if values["KEEP"] != "untouched" {
		t.Error("an unmentioned key changed")
	}
	if values["SECRET"] != "" || !flags["SECRET"] {
		t.Errorf("SECRET = %q/%v, want cleared, still sensitive", values["SECRET"], flags["SECRET"])
	}
	if !flags["NEW"] || flags["DB_HOST"] {
		t.Errorf("flags = %v, want NEW sensitive as asked and DB_HOST visible by default", flags)
	}
	if _, ok := values["LEGACY"]; ok {
		t.Error("LEGACY survived its unset")
	}
}

// Every refusal writes nothing.
func TestEnvRoute_Refusals(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	seedEnv(t, srv, app.ID, "PW", "hunter2", true)

	for _, tc := range []struct{ name, body, code string }{
		{"both set and unset", `{"set":{"A":{"value":"1"}},"unset":["A"]}`, "set_and_unset"},
		{"a reserved name", `{"set":{"A":{"value":"1"},"PORT":{"value":"1"}}}`, "reserved_env_name"},
		{"an invalid name", `{"set":{"A":{"value":"1"},"1BAD":{"value":"1"}}}`, "invalid_env_name"},
		{"a flag that would reveal a secret", `{"set":{"A":{"value":"1"},"PW":{"sensitive":false}}}`, "sensitive_needs_value"},
		{"a new key with no value", `{"set":{"A":{"value":"1"},"NOPE":{"sensitive":true}}}`, "value_required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := patchEnvAs(t, srv, router, "owner", app.ID, tc.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("= %d %s, want 400 %s", rec.Code, rec.Body.String(), tc.code)
			}
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Fatal("an error body carries a value")
			}
			values, _ := envValues(t, srv, app.ID)
			if _, ok := values["A"]; ok || len(values) != 1 {
				t.Fatalf("a refused request wrote %v", values)
			}
		})
	}
}

func TestEnvRoute_OnlyMissing(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	seedEnv(t, srv, app.ID, "TUNED", "operator", false)
	rec := patchEnvAs(t, srv, router, "owner", app.ID,
		`{"set":{"TUNED":{"value":"file"},"FRESH":{"value":"f"}},"only_missing":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"kept":["TUNED"]`) {
		t.Fatalf("= %d %s", rec.Code, rec.Body.String())
	}
	if values, _ := envValues(t, srv, app.ID); values["TUNED"] != "operator" || values["FRESH"] != "f" {
		t.Errorf("values = %v", values)
	}
	// Every key present: nothing written, all kept, still 200.
	rec = patchEnvAs(t, srv, router, "owner", app.ID, `{"set":{"TUNED":{"value":"x"}},"only_missing":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"set":[]`) {
		t.Fatalf("= %d %s", rec.Code, rec.Body.String())
	}
}

// "not set" is a sensitive row holding nothing; a filled secret and any
// visible row carry no flag.
func TestGetApp_UnsetMarksAnEmptySecret(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	seedEnv(t, srv, app.ID, "EMPTY_SECRET", "", true)
	seedEnv(t, srv, app.ID, "FULL_SECRET", "x", true)
	seedEnv(t, srv, app.ID, "EMPTY_VISIBLE", "", false)

	rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID)
	var detail struct {
		EnvVars []envVarResponse `json:"env_vars"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range detail.EnvVars {
		got[v.Key] = v.Unset
	}
	if !got["EMPTY_SECRET"] || got["FULL_SECRET"] || got["EMPTY_VISIBLE"] {
		t.Errorf("unset = %v, want only EMPTY_SECRET", got)
	}
}

// A sensitive value reaches no log: not the access log, not an error line,
// not on a refusal. The default slog handler writes through the log
// package, so capturing log's output captures both.
func TestEnvRoute_NoLogCarriesASensitiveValue(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	var buf strings.Builder
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	const secret = "s3cret-that-must-not-be-logged"
	for _, body := range []string{
		`{"set":{"DB_PASSWORD":{"value":"` + secret + `"}}}`,
		`{"set":{"DB_PASSWORD":{"value":"` + secret + `","sensitive":true}},"only_missing":true}`,
		`{"set":{"DB_PASSWORD":{"value":"` + secret + `"},"PORT":{"value":"` + secret + `"}}}`, // refused
	} {
		patchEnvAs(t, srv, router, "owner", app.ID, body)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("a log line carries the sensitive value:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "/env") {
		t.Fatal("captured nothing; the test is not watching the logs it means to")
	}
}

// A pipeline rotates a secret with the key it deploys with; a read-scoped key
// is refused by its scope. The audit row names the key, not a person.
func TestEnvRoute_ADeployKeyRotatesASecret(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedEnv(t, srv, app.ID, "DB_PASSWORD", "old", true)
	deploy := mintDeployKey(t, srv, router, "pipeline")
	read := mintScopedDeployKey(t, srv, router, "estate", database.DeployKeyScopeRead)

	body := `{"set":{"DB_PASSWORD":{"value":"new"}}}`
	if rec := asDeployKey(t, router, http.MethodPatch, "/api/v1/apps/"+app.ID+"/env", deploy, body); rec.Code != http.StatusOK {
		t.Fatalf("deploy key = %d %s", rec.Code, rec.Body.String())
	}
	if values, flags := envValues(t, srv, app.ID); values["DB_PASSWORD"] != "new" || !flags["DB_PASSWORD"] {
		t.Errorf("DB_PASSWORD = %q/%v, want rotated and still sensitive", values["DB_PASSWORD"], flags["DB_PASSWORD"])
	}
	var actorType string
	if err := srv.db.Conn().QueryRow(`SELECT actor_type FROM audit_log WHERE action='app.env_change' AND target_id=?`, app.ID).Scan(&actorType); err != nil || actorType != auth.PrincipalTypeDeployKey {
		t.Errorf("audit actor_type = %q (%v), want the deploy key", actorType, err)
	}

	rec := asDeployKey(t, router, http.MethodPatch, "/api/v1/apps/"+app.ID+"/env", read, `{"set":{"DB_PASSWORD":{"value":"x"}}}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "deploy_key_read_scope") {
		t.Fatalf("read key = %d %s, want 403 deploy_key_read_scope", rec.Code, rec.Body.String())
	}
}
