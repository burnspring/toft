package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/password"
)

// getSettingsAs / putSettings drive the platform settings endpoint, which is
// where the two refusal messages are written.
func putSettings(t *testing.T, srv *Server, router http.Handler, who, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/system/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func loginOptions(t *testing.T, router http.Handler) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/providers", nil)
	req.RemoteAddr = "127.0.0.1:34567"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/providers = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding login options: %v", err)
	}
	return out
}

// The messages an administrator writes are what the login page shows, so they
// have to reach an anonymous caller — the person reading them is by definition
// not signed in.
func TestRefusalMessagesAreAdminWrittenAndPubliclyReadable(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "pub", database.SystemRolePublisher)

	// Before anybody edits them, the defaults are served rather than nothing.
	opts := loginOptions(t, router)
	if opts["locked_message"] != database.DefaultLockedMessage ||
		opts["no_account_message"] != database.DefaultNoAccountMessage {
		t.Fatalf("defaults not served: %+v", opts)
	}

	// Only an administrator may write them; the settings surface answers 404 to
	// everyone else, as it does for every other capability switch.
	if rec := putSettings(t, srv, router, "pub",
		`{"locked_account_message":"mine now"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("non-admin write = %d, want 404", rec.Code)
	}

	rec := putSettings(t, srv, router, "root",
		`{"locked_account_message":"Ask #it-help to restore your account.",
		  "no_account_message":"Request access in the IT portal first."}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin write = %d body=%s", rec.Code, rec.Body.String())
	}

	opts = loginOptions(t, router)
	if opts["locked_message"] != "Ask #it-help to restore your account." {
		t.Errorf("locked_message = %v", opts["locked_message"])
	}
	if opts["no_account_message"] != "Request access in the IT portal first." {
		t.Errorf("no_account_message = %v", opts["no_account_message"])
	}

	// Cleared means "say the standard thing", never "say nothing".
	if rec := putSettings(t, srv, router, "root",
		`{"locked_account_message":"   "}`); rec.Code != http.StatusOK {
		t.Fatalf("clearing = %d body=%s", rec.Code, rec.Body.String())
	}
	opts = loginOptions(t, router)
	if opts["locked_message"] != database.DefaultLockedMessage {
		t.Errorf("cleared message did not fall back to the default: %v", opts["locked_message"])
	}

	// The settings row is not a document store.
	if rec := putSettings(t, srv, router, "root",
		`{"no_account_message":"`+strings.Repeat("x", messageLimit+1)+`"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("over-long message = %d, want 400", rec.Code)
	}
}

// /auth/me reads the session cookie itself rather than going through the
// middleware, so it has to apply the same liveness rule. A locked user whose
// browser still holds a session must be told they are locked — otherwise the
// SPA renders as signed in and then fails every call it makes.
func TestMeRefusesALockedUserWithTheAdminMessage(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)
	seedUser(t, srv.db, "bob", database.SystemRolePublisher)

	if rec := putSettings(t, srv, router, "root",
		`{"locked_account_message":"Your access was revoked — email security@acme.com."}`); rec.Code != http.StatusOK {
		t.Fatalf("setting the message: %d body=%s", rec.Code, rec.Body.String())
	}

	session := sessionCookieFor(t, srv, "bob")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-lock /auth/me = %d, want 200", rec.Code)
	}

	if rec := postAs(t, srv, router, "root", "/api/v1/users/bob/lock"); rec.Code != http.StatusOK {
		t.Fatalf("lock: %d body=%s", rec.Code, rec.Body.String())
	}

	// The same cookie, now on a locked account.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("locked /auth/me = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	var body refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body["reason"] != deniedLocked {
		t.Errorf("reason = %q, want %q", body["reason"], deniedLocked)
	}
	if body["error"] != "Your access was revoked — email security@acme.com." {
		t.Errorf("error = %q, want the administrator's message", body["error"])
	}
	// The dead session is cleared on the way out, so the browser stops
	// presenting a credential that can never work again.
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the session cookie was not cleared for a locked user")
	}
}

// Local password sign-in is a sign-in path like any other, so a locked account
// gets the same wording there.
func TestLocalLoginRefusalCarriesTheAdminMessage(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	if _, err := srv.db.Conn().Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, NOW())`,
		database.SettingLockedMessage, "Locked by policy. See go/access."); err != nil {
		t.Fatalf("seeding the message: %v", err)
	}
	if _, err := srv.db.Conn().Exec(`UPDATE users SET locked_at = NOW() WHERE id = $1`,
		database.BuiltinAdminID); err != nil {
		t.Fatalf("locking the built-in administrator: %v", err)
	}

	// A correct password and a locked account: authentication succeeded and the
	// refusal is the platform's, which is exactly the case the message is for.
	body, _ := json.Marshal(map[string]string{
		"username": database.BuiltinAdminLogin, "password": password.Default,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/local/login", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("locked local login = %d, want 403", rec.Code)
	}
	var out refusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out["error"] != "Locked by policy. See go/access." {
		t.Errorf("error = %q, want the administrator's message", out["error"])
	}
}
