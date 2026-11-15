package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toft/cli/internal/config"
)

// isolateConfigDir points the credentials file at a temporary directory, so a
// test never reads or writes the developer's own.
func isolateConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// The Windows branch reads this one; setting both keeps the test honest on
	// either platform.
	t.Setenv("AppData", dir)
}

// stdinHolding replaces os.Stdin with a file holding what a pipeline would pipe
// in, because `--token-stdin` reads the real descriptor by design: a token that
// arrives any other way would be a token in an argument list.
func stdinHolding(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening the token file: %v", err)
	}
	saved := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = saved; f.Close() })
}

// **The credential class built for pipelines has to get through the command
// built for pipelines.**
//
// `/auth/me` is mounted for personal keys only, and the deploy key's allow-list
// contains no /auth route at all — so verifying an `toftd_` key against it reports
// a perfectly valid credential as invalid, and sends whoever hit it to rotate a
// key that did not need rotating.
//
// The property is that the prefix selects *the probe* and nothing else, which is
// why this asserts on both classes: each has to ask its own question and neither
// may ask the other's.
func TestTokenStdinProbesTheRouteItsClassMayReach(t *testing.T) {
	cases := []struct {
		name       string
		token      string
		wantPath   string
		wantUser   string
		wantSaid   string
		wantUnsaid string
	}{
		{
			name:       "a deploy key asks a question it is allowed to ask",
			token:      "toftd_ci_runner",
			wantPath:   "/api/v1/apps",
			wantUser:   "",
			wantSaid:   "deploy key",
			wantUnsaid: "Signed in",
		},
		{
			name:       "a personal key still asks who it is",
			token:      "toftu_adas_laptop",
			wantPath:   "/api/v1/auth/me",
			wantUser:   "ada@corp.com",
			wantSaid:   "Signed in to",
			wantUnsaid: "deploy key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigDir(t)
			stdinHolding(t, tc.token+"\n")

			var hit []string
			srv := httptest.NewServer(answersAsInstall(func(w http.ResponseWriter, r *http.Request) {
				hit = append(hit, r.URL.Path)
				if got := r.Header.Get("Authorization"); got != "Bearer "+tc.token {
					t.Errorf("Authorization = %q, want the token that was piped in", got)
				}
				switch r.URL.Path {
				case "/api/v1/apps":
					w.Write([]byte(`[]`))
				case "/api/v1/auth/me":
					w.Write([]byte(`{"login":"ada","email":"ada@corp.com"}`))
				case "/api/v1/me/capabilities":
					w.Write([]byte(`{"credential":{"expires_at":"2026-12-31T00:00:00Z"}}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			e, buf := testEnv()
			if err := loginWithToken(e, srv.URL); err != nil {
				t.Fatalf("loginWithToken: %v", err)
			}

			// The expiry is read from /me/capabilities, which both classes
			// reach; the identity probe is the one that must be the class's own.
			var probes []string
			for _, p := range hit {
				if p != "/api/v1/me/capabilities" {
					probes = append(probes, p)
				}
			}
			if len(probes) != 1 || probes[0] != tc.wantPath {
				t.Errorf("probed %v, want exactly [%s] — the other class's route must not be touched",
					probes, tc.wantPath)
			}

			creds, err := config.LoadCredentials()
			if err != nil {
				t.Fatalf("LoadCredentials: %v", err)
			}
			c, ok, _ := creds.Lookup(srv.URL)
			if !ok {
				t.Fatalf("nothing stored for %s", srv.URL)
			}
			if c.Token != tc.token {
				t.Errorf("stored token = %q, want %q", c.Token, tc.token)
			}
			// The expiry the install states, so `login --status` can say it
			// rather than "expires unknown" (the eks drills, 2026-10-22).
			if want := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC); !c.ExpiresAt.Equal(want) {
				t.Errorf("stored expiry = %v, want %v", c.ExpiresAt, want)
			}
			// A deploy key belongs to nobody, so there is no person to record.
			// Writing one would be a claim about a human where there is none.
			if c.User != tc.wantUser {
				t.Errorf("stored user = %q, want %q", c.User, tc.wantUser)
			}

			said := buf.String()
			if !strings.Contains(said, tc.wantSaid) {
				t.Errorf("said %q, want it to mention %q", said, tc.wantSaid)
			}
			if strings.Contains(said, tc.wantUnsaid) {
				t.Errorf("said %q, which must not mention %q", said, tc.wantUnsaid)
			}
		})
	}
}

// A revoked key of either class fails at login, before anything is written.
//
// The point is that the refusal arrives here rather than on the next deploy with
// a message about that deploy — and that a failed probe leaves no credential
// behind to be picked up by the command after it.
func TestTokenStdinWritesNothingWhenTheProbeRefuses(t *testing.T) {
	for _, token := range []string{"toftd_revoked", "toftu_revoked"} {
		t.Run(token, func(t *testing.T) {
			isolateConfigDir(t)
			stdinHolding(t, token+"\n")

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"invalid API key","code":"invalid_key"}`))
			}))
			defer srv.Close()

			e, _ := testEnv()
			if err := loginWithToken(e, srv.URL); err == nil {
				t.Fatal("stored a key the install refused")
			}

			creds, err := config.LoadCredentials()
			if err != nil {
				t.Fatalf("LoadCredentials: %v", err)
			}
			if _, ok, _ := creds.Lookup(srv.URL); ok {
				t.Error("a refused key was written to the credentials file")
			}
		})
	}
}

// **A cleartext sign-in carries the device code out and a ninety-day key back.**
//
// The refusal is before the first request, because every point after it is a
// point where the secret has already been on the wire. Loopback is the one
// legitimate case and is exempt by host rather than by a flag — an --insecure
// flag is added to a CI config once and outlives the reason for it.
func TestLoginRefusesPlaintextExceptLoopback(t *testing.T) {
	cases := []struct {
		url    string
		refuse bool
	}{
		{"https://toft.corp", false},
		{"http://toft.corp", true},
		{"http://toft.corp:8080/base", true},
		{"http://192.168.1.10:8080", true},
		{"http://localhost:8080", false},
		{"http://localhost", false},
		{"http://127.0.0.1", false},
		{"http://127.0.0.1:8080", false},
		// The whole 127/8 range is loopback, not just the one address.
		{"http://127.0.0.2:9000", false},
		{"http://[::1]:8080", false},
		{"http://[::1]", false},
	}
	for _, tc := range cases {
		err := refusePlaintext(tc.url)
		if tc.refuse && err == nil {
			t.Errorf("refusePlaintext(%q) allowed it — the key would be readable in transit", tc.url)
		}
		if !tc.refuse && err != nil {
			t.Errorf("refusePlaintext(%q) refused: %v", tc.url, err)
		}
		// A refusal about how the command was invoked is exit 2, and it has to
		// name the spelling that would have worked.
		if tc.refuse {
			e, _ := testEnv()
			if got := report(e, err); got != exitUsage {
				t.Errorf("refusePlaintext(%q) exits %d, want %d", tc.url, got, exitUsage)
			}
			if !strings.Contains(err.Error(), "https://") {
				t.Errorf("refusePlaintext(%q) = %q, which does not name the https:// spelling", tc.url, err)
			}
		}
	}
}

// Only an http(s) page of the install itself goes to the system's opener; a
// file:, UNC or foreign address the install answered with is printed (CLI-3).
func TestOnlyTheInstallsOwnPageIsOpened(t *testing.T) {
	for target, want := range map[string]bool{
		"https://toft.example/cli?code=ABCD": true,
		"https://TOFT.example:8443/cli":      true,
		"http://toft.example/cli":            true,
		"https://attacker.example/cli":       false,
		"file:///tmp/payload.desktop":        false,
		`\\attacker\share\setup.exe`:         false,
		"javascript:alert(1)":                false,
		"ms-settings:":                       false,
		"":                                   false,
	} {
		if got := browsable(target, "https://toft.example"); got != want {
			t.Errorf("browsable(%q) = %v, want %v", target, got, want)
		}
	}
}

// BB-26: a sign-in to an address that is not an install stores nothing.
//
// The address people are sent is an app's link, and an app that answers 200 {}
// on every path once took a key, printed "Signed in … as " and became the
// default. The install probe is unauthenticated, so the refusal arrives before
// the key has gone anywhere.
func TestLoginRefusesWhatIsNotAnInstall(t *testing.T) {
	isolateConfigDir(t)
	stdinHolding(t, "toftu_adas_laptop\n")

	var withKey []string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			withKey = append(withKey, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer app.Close()

	e, _ := testEnv()
	err := cmdLogin(e, []string{"--token-stdin", app.URL})
	if got := report(e, err); got == 0 {
		t.Fatal("a catch-all app was signed in to as an install")
	}
	if refusalCode(err) != "not_an_install" {
		t.Errorf("code = %q (%v), want not_an_install", refusalCode(err), err)
	}
	if len(withKey) > 0 {
		t.Errorf("the key was sent to %v", withKey)
	}
	if _, statErr := os.Stat(config.CredentialsPath()); !os.IsNotExist(statErr) {
		t.Errorf("a credentials file was written (%v)", statErr)
	}
}

// BB-26: an app's address is refused by its spelling, before any request carries
// the key — and where the origin answers as an install, the hint names it.
func TestLoginRefusesAnAppsAddressBeforeSendingTheKey(t *testing.T) {
	isolateConfigDir(t)
	stdinHolding(t, "toftu_adas_laptop\n")

	var withKey []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			withKey = append(withKey, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/v1/config":
			w.Write([]byte(`{"version":"0.9.9","apps_base_url":"","apps_path_prefix":"/apps"}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	e, buf := testEnv()
	e.json = true
	err := cmdLogin(e, []string{"--token-stdin", "--json", srv.URL + "/apps/demo"})
	if got := report(e, err); got != exitUsage {
		t.Errorf("exit %d, want %d", got, exitUsage)
	}
	doc := lastDoc(t, buf.String())
	if doc["code"] != "not_an_install" || !strings.Contains(fmt.Sprint(doc["hint"]), "toft login "+srv.URL) {
		t.Errorf("doc = %v, want not_an_install with a hint naming %s", doc, srv.URL)
	}
	if len(withKey) > 0 {
		t.Errorf("the key was sent to %v", withKey)
	}
	if _, statErr := os.Stat(config.CredentialsPath()); !os.IsNotExist(statErr) {
		t.Errorf("a credentials file was written (%v)", statErr)
	}
}

// BB-26: the origin an install names as where its apps are served is refused,
// and an install's own address still signs in through the whole command.
func TestLoginTellsTheInstallFromItsAppsOrigin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		appsBase func(self string) string
		me       string
		code     string
	}{
		{"the install itself", func(string) string { return "https://apps.example" }, `{"login":"ada","email":"ada@corp.com"}`, ""},
		{"its apps origin", func(self string) string { return self }, `{"login":"ada","email":"ada@corp.com"}`, "not_an_install"},
		{"an identity naming nobody", func(string) string { return "" }, `{}`, "not_an_install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigDir(t)
			stdinHolding(t, "toftu_adas_laptop\n")
			var self string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/config":
					fmt.Fprintf(w, `{"version":"0.9.9","apps_base_url":%q,"apps_path_prefix":"/apps"}`, tc.appsBase(self))
				case "/api/v1/auth/me":
					w.Write([]byte(tc.me))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			self = srv.URL

			e, buf := testEnv()
			err := cmdLogin(e, []string{"--token-stdin", srv.URL})
			if refusalCode(err) != tc.code {
				t.Fatalf("code = %q (%v), want %q", refusalCode(err), err, tc.code)
			}
			_, statErr := os.Stat(config.CredentialsPath())
			if tc.code != "" {
				if !os.IsNotExist(statErr) {
					t.Errorf("a credentials file was written (%v)", statErr)
				}
				if strings.Contains(buf.String(), "Signed in") {
					t.Errorf("said %q", buf.String())
				}
				return
			}
			if statErr != nil || !strings.Contains(buf.String(), "Signed in to") {
				t.Errorf("no sign-in: %v, said %q", statErr, buf.String())
			}
		})
	}
}

// answersAsInstall is a stub install: GET /api/v1/config answers as the
// platform does, unauthenticated, so the sign-in's install probe (BB-26)
// passes; every other path goes to the stub.
func answersAsInstall(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/config" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"version":"0.9.test"}`))
			return
		}
		h(w, r)
	})
}
