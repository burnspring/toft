package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/password"
)

// Local password sign-in, which is the bootstrap: the account that exists so an
// install with no identity provider still has a way in.
//
// The property most of these tests are really about is that a *published*
// default password is safe to ship. It is safe for exactly one reason — it mints
// no session — and if that ever regresses, the platform ships with a
// well-documented administrator login instead.

// postJSON drives an unauthenticated POST through the real router.
func postJSON(t *testing.T, router http.Handler, path string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.10:44444"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func sessionCookieIn(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

// The whole reason a published default is acceptable: it buys the right to
// replace itself and nothing else.
func TestDefaultPasswordMintsNoSession(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	rec := postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": database.BuiltinAdminLogin,
		"password": password.Default,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("login with the default password = %d body=%s", rec.Code, rec.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["must_change_password"] != true {
		t.Errorf("must_change_password = %v, want true", out["must_change_password"])
	}
	if c := sessionCookieIn(rec); c != nil {
		t.Fatal("a session cookie was issued for the published default password")
	}
}

// The change closes the loop: it is the first request that may mint a session,
// and afterwards the new password signs in directly.
func TestPasswordChangeIssuesTheFirstSession(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	const chosen = "correct-horse-battery-staple"
	rec := postJSON(t, router, "/api/v1/auth/local/password", map[string]string{
		"username":     database.BuiltinAdminLogin,
		"password":     password.Default,
		"new_password": chosen,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("password change = %d body=%s", rec.Code, rec.Body.String())
	}
	cookie := sessionCookieIn(rec)
	if cookie == nil {
		t.Fatal("no session cookie after a successful password change")
	}
	claims, err := srv.jwt.Validate(cookie.Value)
	if err != nil {
		t.Fatalf("validating the session: %v", err)
	}
	if claims.UserID != database.BuiltinAdminID || claims.SystemRole != database.SystemRoleAdmin {
		t.Errorf("claims = %s/%s, want the built-in administrator", claims.UserID, claims.SystemRole)
	}

	// must_change is cleared, so the new password signs in without ceremony.
	rec = postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": database.BuiltinAdminLogin,
		"password": chosen,
	})
	if rec.Code != http.StatusOK || sessionCookieIn(rec) == nil {
		t.Fatalf("login with the new password = %d, cookie=%v", rec.Code, sessionCookieIn(rec) != nil)
	}

	// And the old one is gone.
	rec = postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": database.BuiltinAdminLogin,
		"password": password.Default,
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("login with the replaced default = %d, want 401", rec.Code)
	}
}

// A login form that says which half was wrong is an account-enumeration oracle,
// and these usernames are people's work identities.
func TestFailedLoginsAreIndistinguishable(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	wrongPassword := postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": database.BuiltinAdminLogin, "password": "not-the-password",
	})
	noSuchUser := postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": "nobody-here", "password": "not-the-password",
	})

	if wrongPassword.Code != http.StatusUnauthorized || noSuchUser.Code != http.StatusUnauthorized {
		t.Fatalf("codes = %d and %d, want 401 for both", wrongPassword.Code, noSuchUser.Code)
	}
	if wrongPassword.Body.String() != noSuchUser.Body.String() {
		t.Errorf("bodies differ:\n wrong password: %s\n unknown user:   %s",
			wrongPassword.Body.String(), noSuchUser.Body.String())
	}
}

// Guessing this password is the one online attack the platform is exposed to,
// and a per-IP limit does nothing against a distributed attempt.
func TestRepeatedFailuresLockTheAccount(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	var last *httptest.ResponseRecorder
	for i := 0; i < 11; i++ {
		last = postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
			"username": database.BuiltinAdminLogin, "password": "wrong",
		})
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("11th failed attempt = %d, want 429", last.Code)
	}

	// The lockout holds even for the correct password: otherwise it would only
	// throttle attackers who never succeed.
	rec := postJSON(t, router, "/api/v1/auth/local/login", map[string]string{
		"username": database.BuiltinAdminLogin, "password": password.Default,
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("correct password during lockout = %d, want 429", rec.Code)
	}
}

// postJSONWith is postJSON carrying cookies.
func postJSONWith(t *testing.T, router http.Handler, path string, body map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.10:44444"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// Anyone who can reach the form can lock the built-in administrator — the
// account that exists to recover the install. A browser that has signed in as
// it before is not touched by that lock: it is counted on its own, and nobody
// else's failures reach its counter.
func TestARememberedBrowserIsNotLockedOutByOtherPeoplesFailures(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	right := map[string]string{"username": database.BuiltinAdminLogin, "password": password.Default}
	wrong := map[string]string{"username": database.BuiltinAdminLogin, "password": "wrong"}

	first := postJSON(t, router, "/api/v1/auth/local/login", right)
	var device *http.Cookie
	for _, c := range first.Result().Cookies() {
		if c.Name == signinDeviceCookie {
			device = c
		}
	}
	if device == nil || !device.HttpOnly || device.Path != signinDeviceCookiePath {
		t.Fatalf("a verified password left no confined device cookie: %+v", device)
	}

	for i := 0; i < 11; i++ {
		postJSON(t, router, "/api/v1/auth/local/login", wrong)
	}
	if rec := postJSON(t, router, "/api/v1/auth/local/login", right); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a fresh browser during the account's lockout = %d, want 429", rec.Code)
	}
	if rec := postJSONWith(t, router, "/api/v1/auth/local/login", right, device); rec.Code != http.StatusOK {
		t.Fatalf("the remembered browser during the account's lockout = %d, want 200: %s", rec.Code, rec.Body)
	}

	// The remembered browser is still held to the rule itself.
	for i := 0; i < 10; i++ {
		postJSONWith(t, router, "/api/v1/auth/local/login", wrong, device)
	}
	if rec := postJSONWith(t, router, "/api/v1/auth/local/login", right, device); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the remembered browser after its own ten failures = %d, want 429", rec.Code)
	}
}

func TestPasswordPolicyRefusals(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())

	for _, tc := range []struct{ name, newPassword string }{
		{"too short", "short"},
		{"the published default again", password.Default},
		{"unchanged", password.Default},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, router, "/api/v1/auth/local/password", map[string]string{
				"username":     database.BuiltinAdminLogin,
				"password":     password.Default,
				"new_password": tc.newPassword,
			})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("change to %q = %d, want 422 body=%s", tc.newPassword, rec.Code, rec.Body.String())
			}
			if rec.Body.Len() == 0 {
				t.Error("a refusal the person can act on must say why")
			}
		})
	}
}

// The asymmetry that makes disabling meaningful: off through the API, on only
// through a shell.
func TestLocalSignInCannotBeEnabledThroughTheAPI(t *testing.T) {
	srv, router := providerTestEnv(t)
	disableLocalSignIn(t, srv)

	rec := sendAs(t, srv, router, "root", http.MethodPatch,
		"/api/v1/admin/auth-providers/"+database.LocalProviderID, `{"enabled":true}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("enabling through the API = %d, want 422 body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("local-admin enable")) {
		t.Errorf("the refusal must name the command that does work, got: %s", rec.Body.String())
	}

	// Still off.
	p, err := srv.db.GetAuthProvider(database.LocalProviderID)
	if err != nil {
		t.Fatalf("reading the provider: %v", err)
	}
	if p.Enabled {
		t.Error("the API enabled local sign-in after refusing to")
	}
}

// Switching it off while it is the only working way in would lock the install out
// of its own admin surface.
func TestLocalSignInDisableRequiresAnotherWayIn(t *testing.T) {
	srv, router := providerTestEnv(t)
	path := "/api/v1/admin/auth-providers/" + database.LocalProviderID

	// An environment GitHub provider exists in this fixture, but nobody has ever
	// signed in through it — so it is not yet a way back.
	rec := sendAs(t, srv, router, "root", http.MethodPatch, path, `{"enabled":false}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("disable with no admin on another provider = %d, want 422 body=%s", rec.Code, rec.Body.String())
	}

	// Give the administrator an identity on it, which is what "can actually sign
	// in" means.
	if _, err := srv.db.Conn().Exec(`
		INSERT INTO user_identities (provider_id, subject, user_id, email, login)
		VALUES ('github', 'sub-root', 'root', 'root@example.com', 'root')`); err != nil {
		t.Fatalf("linking the admin to github: %v", err)
	}

	rec = sendAs(t, srv, router, "root", http.MethodPatch, path, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable with an administrator on github = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	p, err := srv.db.GetAuthProvider(database.LocalProviderID)
	if err != nil {
		t.Fatalf("reading the provider: %v", err)
	}
	if p.Enabled {
		t.Error("the provider is still enabled after a successful disable")
	}
}

// One local provider, built in, not duplicable and not removable.
func TestLocalProviderIsReserved(t *testing.T) {
	srv, router := providerTestEnv(t)

	t.Run("cannot be created", func(t *testing.T) {
		for _, body := range []string{
			`{"id":"local","kind":"password","display_name":"Another"}`,
			`{"id":"passwords","kind":"password","display_name":"Another","client_id":"x","client_secret":"y"}`,
		} {
			rec := sendAs(t, srv, router, "root", http.MethodPost, "/api/v1/admin/auth-providers", body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("create %s = %d, want 422", body, rec.Code)
			}
		}
	})

	t.Run("cannot be deleted", func(t *testing.T) {
		rec := sendAs(t, srv, router, "root", http.MethodDelete,
			"/api/v1/admin/auth-providers/"+database.LocalProviderID, "")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("delete = %d, want 422 body=%s", rec.Code, rec.Body.String())
		}
		if _, err := srv.db.GetAuthProvider(database.LocalProviderID); err != nil {
			t.Fatalf("the provider is gone after a refused delete: %v", err)
		}
	})
}

// Deleting it would not remove it — the next boot re-creates it with the
// published default password — so what looks like a removal is really a reset.
// Refused, by another administrator, which is the only actor who could get this
// far (deleting your own account is refused before this check).
func TestBuiltinAdminCannotBeDeleted(t *testing.T) {
	srv, router := providerTestEnv(t)

	rec := sendAs(t, srv, router, "root", http.MethodDelete,
		"/api/v1/users/"+database.BuiltinAdminID, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete = %d, want 409 body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("cannot be deleted")) {
		t.Errorf("the refusal must say why, got: %s", rec.Body.String())
	}

	user, err := srv.db.GetUser(database.BuiltinAdminID)
	if err != nil || user == nil {
		t.Fatalf("the account is gone after a refused delete: %v, %v", user, err)
	}
}

// A fresh install has exactly one administrator, and it is the built-in one —
// not whoever reached the login page first.
func TestFreshInstallHasTheBuiltinAdministrator(t *testing.T) {
	srv := newTestServer(t)

	user, err := srv.db.GetUser(database.BuiltinAdminID)
	if err != nil || user == nil {
		t.Fatalf("GetUser(%s) = %v, %v", database.BuiltinAdminID, user, err)
	}
	if user.SystemRole != database.SystemRoleAdmin {
		t.Errorf("role = %s, want system_admin", user.SystemRole)
	}
	if user.Locked() {
		t.Error("the built-in administrator starts locked")
	}

	isDefault, err := srv.db.BuiltinAdminHasDefaultPassword()
	if err != nil {
		t.Fatalf("BuiltinAdminHasDefaultPassword: %v", err)
	}
	if !isDefault {
		t.Error("a fresh install does not have the published default password")
	}
}

// A remembered browser is remembered for one password: once the password is
// set again, its old device cookie is just a browser, and its wrong guesses
// count against the account (security review 2026-11-14, SI-9). A password
// change through the form leaves a cookie for the new password.
func TestASigninDeviceDiesWithItsPassword(t *testing.T) {
	srv := newTestServer(t)
	router := srv.Router(http.NotFoundHandler())
	deviceIn := func(rec *httptest.ResponseRecorder) *http.Cookie {
		var d *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == signinDeviceCookie {
				d = c
			}
		}
		return d
	}
	old := deviceIn(postJSON(t, router, "/api/v1/auth/local/login",
		map[string]string{"username": database.BuiltinAdminLogin, "password": password.Default}))
	if old == nil {
		t.Fatal("a verified password left no device cookie")
	}

	const chosen = "correct-horse-battery-staple"
	changed := postJSONWith(t, router, "/api/v1/auth/local/password", map[string]string{
		"username": database.BuiltinAdminLogin, "password": password.Default, "new_password": chosen,
	}, old)
	if changed.Code != http.StatusOK {
		t.Fatalf("password change = %d %s", changed.Code, changed.Body)
	}
	fresh := deviceIn(changed)
	if fresh == nil || fresh.Value == old.Value {
		t.Fatalf("the password change left device cookie %+v, want a new one", fresh)
	}
	if cred, _ := srv.db.GetPasswordCredential(database.BuiltinAdminID); cred == nil ||
		srv.signinDevice(httptestWithCookie(fresh), database.BuiltinAdminID, cred.UpdatedAt) == "" {
		t.Error("the re-issued device cookie does not verify for the new password")
	}

	wrong := map[string]string{"username": database.BuiltinAdminLogin, "password": "wrong"}
	for i := 0; i < 10; i++ {
		postJSONWith(t, router, "/api/v1/auth/local/login", wrong, old)
	}
	cred, err := srv.db.GetPasswordCredential(database.BuiltinAdminID)
	if err != nil || cred == nil {
		t.Fatal(err)
	}
	if !cred.Throttled(time.Now().UTC()) {
		t.Errorf("ten wrong passwords with a device cookie from the old password left the account unlocked (%d failures)", cred.FailedAttempts)
	}
}

func httptestWithCookie(c *http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/local/login", nil)
	req.AddCookie(c)
	return req
}
