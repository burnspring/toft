package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func putAs(t *testing.T, srv *Server, router http.Handler, who, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.cfg.BaseURL)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func readMyTimeZone(t *testing.T, srv *Server, router http.Handler, who string) myTimeZone {
	t.Helper()
	rec := getAs(t, srv, router, who, "/api/v1/auth/me")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/me as %s = %d: %s", who, rec.Code, rec.Body.String())
	}
	var body struct {
		TimeZone myTimeZone `json:"time_zone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.TimeZone
}

// The two layers (ADR §154): an administrator's default reaches everybody who
// has not chosen, a person's own choice is theirs alone, and neither is
// resolved on the server.
func TestTimeZone_InstallDefaultAndPersonalChoice(t *testing.T) {
	srv, router := meEnv(t)

	if tz := readMyTimeZone(t, srv, router, "alice"); tz.User != "" || tz.Install != "" {
		t.Fatalf("fresh install = %+v, want both empty (the browser's zone)", tz)
	}

	rec := putAs(t, srv, router, "root", "/api/v1/system/settings", `{"display_time_zone":"America/Chicago"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH display_time_zone = %d: %s", rec.Code, rec.Body.String())
	}
	if tz := readMyTimeZone(t, srv, router, "bob"); tz.Install != "America/Chicago" || tz.User != "" {
		t.Fatalf("bob after the default = %+v", tz)
	}

	rec = putAs(t, srv, router, "alice", "/api/v1/me/time-zone", `{"time_zone":"browser"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /me/time-zone = %d: %s", rec.Code, rec.Body.String())
	}
	if tz := readMyTimeZone(t, srv, router, "alice"); tz.User != "browser" || tz.Install != "America/Chicago" {
		t.Fatalf("alice after choosing = %+v", tz)
	}
	if tz := readMyTimeZone(t, srv, router, "bob"); tz.User != "" {
		t.Fatalf("alice's choice reached bob: %+v", tz)
	}

	// Back to following the install.
	if rec := putAs(t, srv, router, "alice", "/api/v1/me/time-zone", `{"time_zone":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clear = %d", rec.Code)
	}
	if tz := readMyTimeZone(t, srv, router, "alice"); tz.User != "" {
		t.Fatalf("after clearing = %+v", tz)
	}
}

func TestTimeZone_RefusesWhatIsNotAZone(t *testing.T) {
	srv, router := meEnv(t)

	for _, body := range []string{`{"time_zone":"Local"}`, `{"time_zone":"Mars/Olympus"}`, `{}`} {
		if rec := putAs(t, srv, router, "alice", "/api/v1/me/time-zone", body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", body, rec.Code)
		}
	}
	// "browser" is a person's word, not an install default.
	for _, v := range []string{"browser", "Local", "Nowhere/Town"} {
		rec := putAs(t, srv, router, "root", "/api/v1/system/settings", `{"display_time_zone":"`+v+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH display_time_zone=%q = %d, want 400", v, rec.Code)
		}
	}
	if rec := putAs(t, srv, router, "alice", "/api/v1/system/settings", `{"display_time_zone":"UTC"}`); rec.Code == http.StatusOK {
		t.Errorf("a publisher set the install default")
	}
}
