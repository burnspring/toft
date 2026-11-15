package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"toft/internal/database"
	"toft/internal/safefetch"
)

// Test 14. **The picker filters; it is not an enforcement point.**
//
// A repository the source policy refuses is shown *disabled with the refusal
// text* rather than hidden — a developer who can see why does not open a
// ticket — and typing that same repository into the URL field beside it meets
// exactly the check it always did. That is the whole argument for the picker
// not being an eighth entry on the enforcement list: a repository it never
// lists can still be typed.

// pickerHost is a fake GitHub for the API layer: a mint that answers, and an
// installation listing that answers whatever bearer it is shown.
func pickerHost(t *testing.T, repos []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_minted"})
	})
	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		out := make([]map[string]any, 0, len(repos))
		for _, name := range repos {
			out = append(out, map[string]any{
				"full_name":      name,
				"clone_url":      "https://github.com/" + name + ".git",
				"private":        true,
				"default_branch": "main",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(out), "repositories": out})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// seedAppConnection creates an App-backed connection pointed at a fake host,
// and gives `who` an identity on it.
func seedAppConnection(t *testing.T, e *sourceEnv, host *httptest.Server, who string) *database.GitConnection {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	ciphertext, err := e.srv.encryptor.Encrypt(string(pem.EncodeToMemory(block)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	conn := &database.GitConnection{
		ID: "conn-app", Kind: database.GitConnGitHub, DisplayName: "Acme GitHub",
		BaseURL: host.URL, AuthMethod: database.GitAuthApp, Enabled: true,
		GitHubAppID: "1", InstallationID: "2", ClientID: "Iv1.abc",
	}
	conn.SetEncryptedCloneSecret(ciphertext)
	if err := e.db.CreateGitConnection(conn, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID,
	}); err != nil {
		t.Fatalf("CreateGitConnection: %v", err)
	}
	e.srv.gitConn.SetClientForTesting(conn.ID, safefetch.NewForTesting(
		strings.TrimPrefix(host.URL, "https://"),
		host.Client().Transport.(*http.Transport).TLSClientConfig, false, 20e9))

	token, err := e.srv.encryptor.Encrypt("the-persons-token")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := e.db.PutGitConnectionIdentity(conn.ID, who, token, nil); err != nil {
		t.Fatalf("PutGitConnectionIdentity: %v", err)
	}
	return conn
}

func TestThePickerMarksARefusedRepositoryAndRefusesNothing(t *testing.T) {
	e := newSourceEnv(t)
	host := pickerHost(t, []string{"acme/reports", "elsewhere/tool"})
	conn := seedAppConnection(t, e, host, "publisher")
	// The install deploys only from github.com/acme/*.
	e.restrict(t, "github.com/acme/*")

	rec := e.do(t, "publisher", http.MethodGet,
		"/api/v1/git/connections/"+conn.ID+"/repos", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("repos = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Repositories []struct {
			FullName string `json:"full_name"`
			Allowed  bool   `json:"allowed"`
			Refusal  string `json:"refusal"`
			Covered  bool   `json:"covered"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	// **Both are listed.** Hiding the refused one would leave a developer
	// wondering where their repository went, which is a ticket.
	if len(body.Repositories) != 2 {
		t.Fatalf("the picker listed %d repositories, want both", len(body.Repositories))
	}
	byName := map[string]bool{}
	refusals := map[string]string{}
	for _, repo := range body.Repositories {
		byName[repo.FullName] = repo.Allowed
		refusals[repo.FullName] = repo.Refusal
	}
	if !byName["acme/reports"] {
		t.Error("an allowed repository was marked refused")
	}
	if byName["elsewhere/tool"] {
		t.Error("a repository outside the policy was marked allowed")
	}
	if !strings.Contains(refusals["elsewhere/tool"], "github.com/acme/*") {
		t.Errorf("the marking does not say what would work: %q", refusals["elsewhere/tool"])
	}

	// And the picker wrote nothing: typing the refused repository into the URL
	// field beside it meets exactly the check it always did.
	rec = e.do(t, "publisher", http.MethodPost, "/api/v1/apps", `{
		"name":"Tool","slug":"tool","repo_url":"https://github.com/elsewhere/tool",
		"branch":"main","git_connection_id":"`+conn.ID+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("typing the refused repository = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if app, _ := e.db.GetAppBySlug("tool"); app != nil {
		t.Error("the refused create left an app row behind")
	}
}

// A person who has not signed in to the host yet gets an ordinary answer with
// somewhere to go, not an error: it is the first state, not a failure.
func TestThePickerAsksForASignInRatherThanFailing(t *testing.T) {
	e := newSourceEnv(t)
	host := pickerHost(t, []string{"acme/reports"})
	conn := seedAppConnection(t, e, host, "publisher")

	// admin-user has no identity on this connection.
	rec := e.do(t, "admin-user", http.MethodGet,
		"/api/v1/git/connections/"+conn.ID+"/repos", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("repos = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		NeedsAuthorize bool   `json:"needs_authorize"`
		AuthorizeURL   string `json:"authorize_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !body.NeedsAuthorize || body.AuthorizeURL == "" {
		t.Errorf("a person with no host identity got no way in: %s", rec.Body.String())
	}
}

// A token-backed connection has no picker, and the developer is never told
// which of the two methods backs it — only that they should enter a URL.
func TestAConnectionWithNoPickerAnswersTheRepoRouteWithoutSayingWhy(t *testing.T) {
	e := newSourceEnv(t)
	conn := createConnection(t, e, tokenConnectionBody)

	for _, path := range []string{"/repos", "/authorize"} {
		rec := e.do(t, "publisher", http.MethodGet,
			"/api/v1/git/connections/"+conn.ID+path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404 (%s)", path, rec.Code, rec.Body.String())
		}
		for _, forbidden := range []string{"auth_method", "token-backed", "GitHub App", "personal access"} {
			if strings.Contains(rec.Body.String(), forbidden) {
				t.Errorf("%s tells a developer which credential backs the connection: %s", path, rec.Body.String())
			}
		}
	}
}

// The picker's state cookie sits at its own path, so the browser is what keeps
// a repository-scoped callback out of the sign-in handler.
func TestThePickerStateCookieIsSeparatedByPath(t *testing.T) {
	e := newSourceEnv(t)
	host := pickerHost(t, nil)
	conn := seedAppConnection(t, e, host, "publisher")

	rec := e.do(t, "publisher", http.MethodGet,
		"/api/v1/git/connections/"+conn.ID+"/authorize", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize = %d, want 302 (%s)", rec.Code, rec.Body.String())
	}
	var state *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == gitStateCookie {
			state = c
		}
		if c.Name == oauthStateCookie {
			t.Error("the picker set the sign-in flow's state cookie")
		}
	}
	if state == nil {
		t.Fatal("no picker state cookie was set")
	}
	if state.Path != gitCookiePath {
		t.Errorf("state cookie path = %q, want %q", state.Path, gitCookiePath)
	}
	if state.Path == oauthCookiePath {
		t.Error("the picker's state is reachable by the sign-in callback")
	}
	if !state.HttpOnly {
		t.Error("the picker's state cookie is readable by script")
	}
}

// DG-4 (security review 2026-11-14): the picker reads the installation's
// coverage once per request, not once per listed repository.
func TestThePickerReadsCoverageOncePerRequest(t *testing.T) {
	e := newSourceEnv(t)
	var repos []string
	for i := 0; i < 50; i++ {
		repos = append(repos, "acme/repo-"+strconv.Itoa(i))
	}
	var mu sync.Mutex
	installationReads := 0
	var hostURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_minted"})
	})
	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer ghs_minted" {
			mu.Lock()
			installationReads++
			mu.Unlock()
		}
		out := make([]map[string]any, 0, len(repos))
		for _, name := range repos {
			// On the connection's own host, or HostRefusal answers first and
			// the installation is never read at all.
			out = append(out, map[string]any{"full_name": name,
				"clone_url": hostURL + "/" + name + ".git", "default_branch": "main"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(out), "repositories": out})
	})
	host := httptest.NewTLSServer(mux)
	t.Cleanup(host.Close)
	hostURL = host.URL
	conn := seedAppConnection(t, e, host, "publisher")

	rec := e.do(t, "publisher", http.MethodGet, "/api/v1/git/connections/"+conn.ID+"/repos", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("repos = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Repositories []struct {
			Covered bool `json:"covered"`
		} `json:"repositories"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Repositories) != 50 || body.HasMore {
		t.Fatalf("listed %d (has_more %v), want 50 and no more", len(body.Repositories), body.HasMore)
	}
	for _, r := range body.Repositories {
		if !r.Covered {
			t.Fatal("a repository the installation covers was marked not covered")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if installationReads != 1 {
		t.Errorf("the installation was read %d times for one page, want 1", installationReads)
	}
}
