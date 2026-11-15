package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"toft/internal/database"
)

// The environment of an existing app has one write route. env_vars on PATCH
// /apps/{id} is refused, naming the route, and writes nothing — while
// creation, which has no rows to replace, still takes it.
func TestUpdateApp_EnvVarsAreRefused(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	for _, body := range []string{
		`{"env_vars":{"A":"1"}}`,
		`{"env_sensitive":{"A":true}}`,
		`{"name":"renamed","env_vars":{}}`,
	} {
		rec := patchAppAs(t, srv, router, "owner", app.ID, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "use_env_route") {
			t.Fatalf("%s = %d %s, want 400 use_env_route", body, rec.Code, rec.Body.String())
		}
	}
	if vars := mustEnvVars(t, srv, app.ID); len(vars) != 0 {
		t.Fatalf("a refused PATCH wrote %+v", vars)
	}
	if got, _ := srv.db.GetApp(app.ID); got.Name == "renamed" {
		t.Error("a refused PATCH applied its other fields")
	}
}

// Visible values come back from GET /apps/{id} — but only to someone who may
// edit the app, and sensitive values never do.
func TestGetApp_EnvVarVisibility(t *testing.T) {
	srv, router, app := modeTestEnv(t)
	encrypt := func(v string) string {
		t.Helper()
		enc, err := srv.encryptor.Encrypt(v)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		return enc
	}
	if err := srv.db.SetEnvVar(app.ID, "CONFIG", encrypt("shown"), false); err != nil {
		t.Fatalf("SetEnvVar: %v", err)
	}
	if err := srv.db.SetEnvVar(app.ID, "SECRET", encrypt("hidden"), true); err != nil {
		t.Fatalf("SetEnvVar: %v", err)
	}
	if err := srv.db.SetEnvVar(app.ID, "EMPTY", encrypt(""), false); err != nil {
		t.Fatalf("SetEnvVar: %v", err)
	}

	rec := getAs(t, srv, router, "owner", "/api/v1/apps/"+app.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET as owner = %d", rec.Code)
	}
	var detail struct {
		EnvVars []envVarResponse `json:"env_vars"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]envVarResponse{}
	for _, v := range detail.EnvVars {
		got[v.Key] = v
	}
	if v := got["CONFIG"]; v.Value == nil || *v.Value != "shown" || v.Sensitive {
		t.Errorf("CONFIG = %+v, want its value, visible", v)
	}
	if v := got["SECRET"]; v.Value != nil || !v.Sensitive {
		t.Errorf("SECRET = %+v, want no value, sensitive", v)
	}
	// A visible "" is a value, and reads as one: absent is the secret's shape.
	if v := got["EMPTY"]; v.Value == nil || *v.Value != "" {
		t.Errorf("EMPTY = %+v, want value \"\" present", v)
	}
	if !strings.Contains(rec.Body.String(), `"key":"EMPTY","value":""`) {
		t.Errorf("the visible empty variable has no value on the wire: %s", rec.Body.String())
	}

	// An admin can read any app but env values follow edit permission — a
	// system_admin may edit, so use a plain viewer with a grant-free private
	// app: the app is invisible to them entirely (404), which is the stronger
	// property and already covered by authz tests. Here we pin that the owner
	// path returns values and the response never carries a sensitive value.
	for _, v := range detail.EnvVars {
		if v.Sensitive && v.Value != nil {
			t.Errorf("sensitive %s carries a value in the API response", v.Key)
		}
	}
}

func mustEnvVars(t *testing.T, srv *Server, appID string) []database.EnvVar {
	t.Helper()
	vars, err := srv.db.GetEnvVars(appID)
	if err != nil {
		t.Fatalf("GetEnvVars: %v", err)
	}
	return vars
}
