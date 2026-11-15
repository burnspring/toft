package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/cli/internal/client"
	"toft/cli/internal/config"
)

// sessionInstall answers the built-in administrator's password form and
// /auth/me the way an install does: a cookie on the first, the cookie checked
// on the second.
func sessionInstall(t *testing.T, loginStatus int, loginBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(answersAsInstall(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/local/login":
			if loginStatus == http.StatusOK && !strings.Contains(loginBody, "must_change") {
				http.SetCookie(w, &http.Cookie{Name: client.SessionCookie, Value: "jwt-1", Path: "/api/v1", MaxAge: 1800})
			}
			w.WriteHeader(loginStatus)
			w.Write([]byte(loginBody))
		case "/api/v1/auth/me":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("a session was sent as a bearer key: %q", r.Header.Get("Authorization"))
			}
			if ck, err := r.Cookie(client.SessionCookie); err != nil || ck.Value != "jwt-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"login":"admin","email":"admin@toft.local"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// **A session goes beside the key, never in place of it.** The machine that
// runs the drills is the tester and the administrator at once; a password
// sign-in that replaced the key would turn every later `toft deploy` into the
// administrator's, and a key sign-in that dropped the session would send the
// next gate back to a person.
func TestPasswordLoginStoresASessionBesideTheKey(t *testing.T) {
	isolateConfigDir(t)
	srv := sessionInstall(t, http.StatusOK, `{"ok":true,"redirect":"/"}`)

	creds, _ := config.LoadCredentials()
	creds.Set(config.Credential{URL: srv.URL, Token: "toftu_tester", User: "tester@corp.com", Alias: "drills"})
	if err := creds.Save(); err != nil {
		t.Fatal(err)
	}

	stdinHolding(t, "s3cret\n")
	e, buf := testEnv()
	if err := loginWithPassword(e, srv.URL, "admin"); err != nil {
		t.Fatalf("loginWithPassword: %v", err)
	}
	if !strings.Contains(buf.String(), "a session") {
		t.Errorf("said %q, want it to say a session was stored", buf.String())
	}

	creds, _ = config.LoadCredentials()
	c, _, _ := creds.Lookup(srv.URL)
	if c.Token != "toftu_tester" || c.User != "tester@corp.com" || c.Alias != "drills" {
		t.Errorf("the key's entry moved: %+v", c)
	}
	if c.Session != "jwt-1" || c.SessionUser != "admin@toft.local" || c.SessionExpiresAt.IsZero() {
		t.Errorf("session not stored: %+v", c)
	}

	// And back the other way: re-keying keeps the session.
	stdinHolding(t, "toftu_rotated\n")
	keyed := httptest.NewServer(answersAsInstall(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"login":"tester","email":"tester@corp.com"}`))
	}))
	defer keyed.Close()
	creds.Set(config.Credential{URL: keyed.URL, Token: "toftu_old", Session: "jwt-keep", SessionUser: "admin"})
	creds.Save()
	if err := loginWithToken(e, keyed.URL); err != nil {
		t.Fatalf("loginWithToken: %v", err)
	}
	creds, _ = config.LoadCredentials()
	k, _, _ := creds.Lookup(keyed.URL)
	if k.Token != "toftu_rotated" || k.Session != "jwt-keep" {
		t.Errorf("re-keying lost the session: %+v", k)
	}
}

// Each way a password sign-in can fail is an auth refusal (exit 3) that names
// its cause, and none of them writes anything.
func TestPasswordLoginRefusalsSayWhy(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"local sign-in switched off", http.StatusNotFound, `{"error":"not found"}`, "Admin → Authentication"},
		{"wrong password", http.StatusUnauthorized, `{"error":"incorrect username or password"}`, "incorrect username or password"},
		{"the published default", http.StatusOK,
			`{"must_change_password":true,"reason":"This account still has its initial password."}`, "initial password"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigDir(t)
			srv := sessionInstall(t, tc.status, tc.body)
			stdinHolding(t, "nope\n")
			e, _ := testEnv()
			err := loginWithPassword(e, srv.URL, "admin")
			var ae *authError
			if !errors.As(err, &ae) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want an auth refusal mentioning %q", err, tc.want)
			}
			// Coded and hinted where the cause is the caller's (L5).
			if tc.status != http.StatusOK && (ae.code == "" || ae.hint == "") {
				t.Errorf("%s: code %q hint %q", tc.name, ae.code, ae.hint)
			}
			creds, _ := config.LoadCredentials()
			if len(creds.Installs) != 0 {
				t.Errorf("a refused sign-in stored %v", creds.Installs)
			}
		})
	}
}

// A redirected sign-in is a refusal, and the password is not posted on to the
// host the Location names (CLI-4).
func TestPasswordLoginDoesNotFollowARedirect(t *testing.T) {
	isolateConfigDir(t)
	var reached bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(answersAsInstall(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/v1/auth/local/login", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	stdinHolding(t, "hunter2\n")
	e, _ := testEnv()
	err := loginWithPassword(e, srv.URL, "admin")
	var ae *authError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v, want an auth refusal naming the redirect", err)
	}
	if reached {
		t.Error("the password was posted on to the redirect's host")
	}
}

// `--session` with nothing stored, or with a session past its expiry, is
// refused before any request — never quietly answered with the key, which
// would reach a governance route and be refused `session_required` for a
// reason nobody would look for.
func TestSessionFlagNeverFallsBackToTheKey(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv("TOFT_URL", "")
	t.Setenv("TOFT_TOKEN", "")
	creds, _ := config.LoadCredentials()
	creds.Set(config.Credential{URL: "https://toft.corp", Token: "toftu_tester"})
	creds.Save()

	install := config.Value{Value: "https://toft.corp", Origin: "--install"}
	var none *config.NoAdminSessionError
	if _, err := config.ResolveAdminSession(install); !errors.As(err, &none) {
		t.Fatalf("no session stored: err = %v, want NoAdminSessionError", err)
	}

	creds, _ = config.LoadCredentials()
	creds.Set(config.Credential{URL: "https://toft.corp", Token: "toftu_tester",
		Session: "jwt-old", SessionExpiresAt: time.Now().Add(-time.Minute)})
	creds.Save()
	if _, err := config.ResolveAdminSession(install); !errors.As(err, &none) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired session: err = %v, want it refused as expired", err)
	}

	creds, _ = config.LoadCredentials()
	creds.Set(config.Credential{URL: "https://toft.corp", Token: "toftu_tester",
		Session: "jwt-live", SessionExpiresAt: time.Now().Add(time.Hour)})
	creds.Save()
	sess, err := config.ResolveAdminSession(install)
	if err != nil || sess.Token != "jwt-live" || !sess.Cookie {
		t.Fatalf("live session: %+v, %v", sess, err)
	}
	if w := sess.ExpiryWarning(time.Now()); w != "" {
		t.Errorf("a session an hour from its end warned %q; a session is hours long by design", w)
	}
}

// A session slides, so a renewed token in any answer replaces the one held —
// in memory for the rest of this command and, through OnRenew, on disk for the
// next. A key never reads Set-Cookie at all.
func TestARenewedSessionIsKept(t *testing.T) {
	var presented []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ck, _ := r.Cookie(client.SessionCookie)
		if ck != nil {
			presented = append(presented, ck.Value)
		}
		if len(presented) == 1 {
			http.SetCookie(w, &http.Cookie{Name: client.SessionCookie, Value: "jwt-2", MaxAge: 1800})
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := client.New(srv.URL, "jwt-1", "toft/test")
	c.Cookie = true
	var renewed string
	c.OnRenew = func(token string, _ time.Time) { renewed = token }
	for i := 0; i < 2; i++ {
		if err := c.Do(http.MethodGet, "/api/v1/apps", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(presented, ",") != "jwt-1,jwt-2" || renewed != "jwt-2" {
		t.Errorf("presented %v, renewed %q; want jwt-1 then jwt-2", presented, renewed)
	}

	// A redirect that ends on another host: its Set-Cookie is that host's
	// choice of session and is never adopted (CLI-1).
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: client.SessionCookie, Value: "planted", MaxAge: 1800})
		w.Write([]byte(`{}`))
	}))
	defer elsewhere.Close()
	bounce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/v1/apps", http.StatusFound)
	}))
	defer bounce.Close()
	away := client.New(bounce.URL, "jwt-1", "toft/test")
	away.Cookie = true
	away.OnRenew = func(token string, _ time.Time) { t.Errorf("renewed to %q from another host", token) }
	away.Do(http.MethodGet, "/api/v1/apps", nil, nil)
	if away.Token != "jwt-1" {
		t.Errorf("token = %q after a redirect to another host, want jwt-1", away.Token)
	}

	key := client.New(srv.URL, "toftu_x", "toft/test")
	key.OnRenew = func(string, time.Time) { t.Error("a key took a cookie") }
	presented = nil
	key.Do(http.MethodGet, "/api/v1/apps", nil, nil)
	if key.Token != "toftu_x" {
		t.Errorf("a key's token changed to %q", key.Token)
	}
}

// A session the install refuses has ended there — revoked by a change to the
// person's role or groups, or expired on the install's clock — while the copy
// stored here still looks current. The install's generic hint ("send a
// credential … or run `toft login`") names neither; the run that presented the
// session names the sign-in that replaces it (drills 2026-10-28 #6), in the text
// and under --json. A key's 401 keeps the install's own hint.
func TestARefusedSessionNamesThePasswordSignIn(t *testing.T) {
	const generic = "send a credential: Authorization: Bearer <key>, or run `toft login`"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid session","code":"unauthorized","hint":"` + generic + `"}`))
	}))
	defer srv.Close()
	isolateConfigDir(t)
	t.Setenv("TOFT_URL", "")
	t.Setenv("TOFT_TOKEN", "")
	creds, _ := config.LoadCredentials()
	creds.Set(config.Credential{URL: srv.URL, Token: "toftu_tester",
		Session: "jwt-revoked", SessionExpiresAt: time.Now().Add(time.Hour)})
	creds.Save()

	for _, c := range []struct {
		name string
		args []string
	}{
		{"text", []string{"--install", srv.URL, "--session"}},
		{"json", []string{"--install", srv.URL, "--session", "--json"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, buf := testEnv()
			e.dir = t.TempDir()
			if got := report(e, cmdApps(e, c.args)); got != exitAuth {
				t.Fatalf("exit %d, want %d:\n%s", got, exitAuth, buf)
			}
			if !strings.Contains(buf.String(), "toft login --password-stdin "+srv.URL) ||
				strings.Contains(buf.String(), "Bearer <key>") {
				t.Errorf("the refusal does not name the password sign-in:\n%s", buf)
			}
		})
	}

	t.Run("a key keeps the install's hint", func(t *testing.T) {
		e, buf := testEnv()
		e.dir = t.TempDir()
		if got := report(e, cmdApps(e, []string{"--install", srv.URL})); got != exitAuth {
			t.Fatalf("exit %d:\n%s", got, buf)
		}
		if !strings.Contains(buf.String(), "Bearer <key>") || strings.Contains(buf.String(), "--password-stdin") {
			t.Errorf("a key's refusal lost the install's hint:\n%s", buf)
		}
	})
}

// An administrator-only route hides itself with a 404 from a session it no
// longer admits (docs/invariants.md: 404 to everyone else), so a revoked
// session read as "not found" and nobody signed in again (drills 2026-11-02
// O4). The client asks /auth/me: a dead session is exit 3 naming the sign-in,
// and a live one keeps the 404 as the install meant it.
func TestARevokedSessionsHiddenRouteSaysTheSessionEnded(t *testing.T) {
	alive := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			if alive {
				w.Write([]byte(`{"id":"u1"}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid session","code":"unauthorized"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found","code":"not_found","retryable":false}`))
	}))
	defer srv.Close()
	isolateConfigDir(t)
	t.Setenv("TOFT_URL", "")
	t.Setenv("TOFT_TOKEN", "")
	creds, _ := config.LoadCredentials()
	creds.Set(config.Credential{URL: srv.URL, Token: "toftu_tester",
		Session: "jwt-revoked", SessionExpiresAt: time.Now().Add(time.Hour)})
	creds.Save()

	e, buf := testEnv()
	e.dir = t.TempDir()
	if got := report(e, cmdAPI(e, []string{"GET", "system/settings", "--install", srv.URL, "--session"})); got != exitAuth {
		t.Fatalf("revoked: exit %d, want %d:\n%s", got, exitAuth, buf)
	}
	if !strings.Contains(buf.String(), "toft login --password-stdin "+srv.URL) {
		t.Errorf("revoked: the refusal does not name the sign-in:\n%s", buf)
	}

	alive = true
	e, buf = testEnv()
	e.dir = t.TempDir()
	if got := report(e, cmdAPI(e, []string{"GET", "system/settings", "--install", srv.URL, "--session"})); got != exitUsage {
		t.Fatalf("live: exit %d, want %d (a real 404):\n%s", got, exitUsage, buf)
	}
}
