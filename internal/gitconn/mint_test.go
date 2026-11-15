package gitconn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/safefetch"
)

// A fake GitHub, standing in for the one thing these tests cannot have.
//
// It is deliberately opinionated about the credential it is shown, because that
// is the property under test: the picker's list must come back through a
// *person's* token and never through the install's.
type fakeHost struct {
	mu sync.Mutex
	// mintedFor records the repositories each mint asked for, in order. Test 12
	// reads it: a token narrowed to nothing is a token that reaches the whole
	// installation.
	mintedFor [][]string
	// listedWith records the bearer each repository listing arrived with.
	listedWith []string
	// visibleTo maps a bearer token to the repositories it may see.
	visibleTo map[string][]string
	server    *httptest.Server
}

func newFakeHost(t *testing.T, visibleTo map[string][]string) *fakeHost {
	t.Helper()
	h := &fakeHost{visibleTo: visibleTo}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Repositories []string `json:"repositories"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.mintedFor = append(h.mintedFor, body.Repositories)
		h.mu.Unlock()
		// The token names what it was narrowed to, so a test can tell one
		// repository's token from another's.
		token := "ghs_for_" + strings.Join(body.Repositories, "_")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      token,
			"expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		h.mu.Lock()
		h.listedWith = append(h.listedWith, bearer)
		all := h.visibleTo[bearer]
		h.mu.Unlock()

		// Paged as the host pages: per_page and page, 1-based.
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if per < 1 {
			per = 30
		}
		if pg < 1 {
			pg = 1
		}
		var names []string
		if lo := (pg - 1) * per; lo < len(all) {
			names = all[lo:min(lo+per, len(all))]
		}
		repos := make([]map[string]any, 0, len(names))
		for _, name := range names {
			repos = append(repos, map[string]any{
				"full_name":      name,
				"clone_url":      "https://github.com/" + name + ".git",
				"private":        true,
				"default_branch": "main",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count":  len(all),
			"repositories": repos,
		})
	})

	h.server = httptest.NewTLSServer(mux)
	t.Cleanup(h.server.Close)
	return h
}

// wire points a resolver's connection at this fake host, with the address guard
// off — an httptest server is on loopback, which is the one rule a test server
// cannot survive.
func (h *fakeHost) wire(t *testing.T, db *database.DB, r *Resolver, conn *database.GitConnection) {
	t.Helper()
	conn.BaseURL = h.server.URL
	// Written back, not just set on the struct: the resolver reads the row live
	// on every call — which is the property that makes a disable immediate, and
	// which a test that only mutated its copy would sail past.
	if db != nil {
		if err := db.UpdateGitConnection(conn, database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID,
		}); err != nil {
			t.Fatalf("UpdateGitConnection: %v", err)
		}
	}
	client := safefetch.NewForTesting(strings.TrimPrefix(h.server.URL, "https://"),
		h.server.Client().Transport.(*http.Transport).TLSClientConfig, false, hostTimeout)
	r.SetClientForTesting(conn.ID, client)
}

func appConnection(t *testing.T, db *database.DB, enc *crypto.Encryptor) *database.GitConnection {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	ciphertext, err := enc.Encrypt(string(pem.EncodeToMemory(block)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	conn := &database.GitConnection{
		ID: "conn-app", Kind: database.GitConnGitHub, DisplayName: "Acme GitHub",
		BaseURL: "https://git.acme.internal", AuthMethod: database.GitAuthApp,
		Enabled: true, GitHubAppID: "12345", InstallationID: "67890", ClientID: "Iv1.abc",
	}
	conn.SetEncryptedCloneSecret(ciphertext)
	if err := db.CreateGitConnection(conn, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID,
	}); err != nil {
		t.Fatalf("CreateGitConnection: %v", err)
	}
	return conn
}

// seedUser makes a user row for an identity to hang off. The identities table
// keys on `users.id`, which is the point: a picker credential belongs to a
// person on this platform, not to an opaque subject on the host.
func seedUser(t *testing.T, db *database.DB, id string) {
	t.Helper()
	if err := db.UpsertUser(&database.User{ID: id, Login: id, Email: id + "@example.com"}); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// Test 12. **A minted token is scoped to the repository being cloned**, and the
// cache never hands one repository's token to another.
//
// The mint call accepts a repository narrowing, so asking for one is the
// difference between a credential that reaches this app's code for an hour and
// one that reaches the whole installation. Without the cache key carrying the
// repository, the second app to build would silently clone with the first's
// token — which would work, which is exactly why nothing would notice.
func TestAMintedTokenIsNarrowedToTheRepositoryAndCachedPerRepository(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := appConnection(t, db, enc)
	host := newFakeHost(t, nil)
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)

	reports := &database.App{Slug: "reports", RepoURL: host.server.URL + "/acme/reports", GitConnectionID: conn.ID}
	payroll := &database.App{Slug: "payroll", RepoURL: host.server.URL + "/acme/payroll", GitConnectionID: conn.ID}

	first, err := r.CloneCredential(context.Background(), reports)
	if err != nil {
		t.Fatalf("CloneCredential: %v", err)
	}
	second, err := r.CloneCredential(context.Background(), payroll)
	if err != nil {
		t.Fatalf("CloneCredential: %v", err)
	}
	if first.Token == second.Token {
		t.Fatal("two repositories were handed the same minted token")
	}
	if first.Token != "ghs_for_reports" {
		t.Errorf("reports got %q", first.Token)
	}
	if second.Token != "ghs_for_payroll" {
		t.Errorf("payroll got %q", second.Token)
	}
	if first.Source != SourceConnection || first.Connection != "Acme GitHub" {
		t.Errorf("the minted credential lost its provenance: %+v", first)
	}

	host.mu.Lock()
	mints := len(host.mintedFor)
	narrowings := append([][]string(nil), host.mintedFor...)
	host.mu.Unlock()
	if mints != 2 {
		t.Fatalf("%d mints for two repositories, want 2", mints)
	}
	for i, asked := range narrowings {
		if len(asked) != 1 {
			t.Errorf("mint %d asked for %v — a mint that names no repository reaches the whole installation", i, asked)
		}
	}

	// The cache is per repository, so a second build of the same app mints
	// nothing new.
	if _, err := r.CloneCredential(context.Background(), reports); err != nil {
		t.Fatalf("CloneCredential: %v", err)
	}
	host.mu.Lock()
	mints = len(host.mintedFor)
	host.mu.Unlock()
	if mints != 2 {
		t.Errorf("%d mints after a repeat build, want 2 — the cache did not hold", mints)
	}

	// **The write is what drops it.** A disable takes effect on the next mint
	// with nothing restarted, which is what Forget is called for on every write
	// to the row.
	r.Forget(conn.ID)
	host.wire(t, nil, r, conn) // Forget drops the test client; the row is already right
	if _, err := r.CloneCredential(context.Background(), reports); err != nil {
		t.Fatalf("CloneCredential: %v", err)
	}
	host.mu.Lock()
	mints = len(host.mintedFor)
	host.mu.Unlock()
	if mints != 3 {
		t.Errorf("%d mints after Forget, want 3 — the cache survived a write to the row", mints)
	}
}

// Test 1. **The picker never lists through the clone credential.**
//
// Without this the bug is a working feature that leaks the estate, and nothing
// in the product looks wrong: an install-wide credential sees everything the
// organization exposed, so every signed-in developer would get a directory of
// the company's private repositories.
func TestThePickerNeverListsThroughTheCloneCredential(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := appConnection(t, db, enc)

	// The install's credential would list ten; this person can see two.
	host := newFakeHost(t, map[string][]string{
		"ghs_for_": {
			"acme/one", "acme/two", "acme/three", "acme/four", "acme/five",
			"acme/six", "acme/seven", "acme/eight", "acme/nine", "acme/ten",
		},
		"the-persons-token": {"acme/one", "acme/two"},
	})
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)

	seedUser(t, db, "dev")
	stored, err := enc.Encrypt("the-persons-token")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if err := db.PutGitConnectionIdentity(conn.ID, "dev", stored, nil); err != nil {
		t.Fatalf("PutGitConnectionIdentity: %v", err)
	}

	repos, _, err := r.ListRepositoriesFor(context.Background(), conn, "dev", "", 1)
	if err != nil {
		t.Fatalf("ListRepositoriesFor: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("the picker listed %d repositories, want the 2 this person can see — "+
			"a longer list means it listed through the install's credential", len(repos))
	}

	host.mu.Lock()
	bearers := append([]string(nil), host.listedWith...)
	host.mu.Unlock()
	for _, bearer := range bearers {
		if bearer != "the-persons-token" {
			t.Errorf("the picker listed with %q, which is not this person's token", bearer)
		}
	}

	// And the install's own credential still reads the *installation*, which is
	// a fact about the connection rather than about a viewer — the two calls
	// exist side by side and must not converge.
	granted, err := r.InstallationCoverage(context.Background(), conn)
	if err != nil {
		t.Fatalf("InstallationCoverage: %v", err)
	}
	if len(granted) != 10 {
		t.Errorf("the installation's coverage is %d repositories, want 10", len(granted))
	}
}

// A person who has not signed in to the host yet is the ordinary first state,
// not a failure — and it must never fall back to the install's credential.
func TestAPersonWithNoHostIdentityGetsNoList(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := appConnection(t, db, enc)
	host := newFakeHost(t, map[string][]string{"ghs_for_": {"acme/one", "acme/two"}})
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)
	seedUser(t, db, "dev")

	_, _, err := r.ListRepositoriesFor(context.Background(), conn, "dev", "", 1)
	if !errors.Is(err, ErrNoPickerIdentity) {
		t.Fatalf("err = %v, want ErrNoPickerIdentity", err)
	}
	host.mu.Lock()
	listed := len(host.listedWith)
	host.mu.Unlock()
	if listed != 0 {
		t.Error("a person with no identity still caused a listing")
	}
}

// Test 15. A connection's client reaches **its own** base URL and nothing else
// on that network. The exemption is not a flag: a connection *is* a configured
// URL, so it gets a client the way the gallery and the advisory feed do.
func TestAConnectionsClientIsExemptForItsOwnHostOnly(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := &database.GitConnection{
		ID: "conn-ghes", Kind: database.GitConnGitHub, DisplayName: "Acme GHES",
		BaseURL: "https://git.acme.internal", AuthMethod: database.GitAuthToken,
	}
	r := NewResolver(db, enc)

	client := r.clientFor(conn)
	if client.MirrorHost() != "git.acme.internal" {
		t.Fatalf("mirror host = %q, want the connection's own host", client.MirrorHost())
	}
	// Its own host is exempt; a URL that arrived *inside* a response from it is
	// not, and neither is anything else on that network.
	if client.For("https://git.acme.internal/api/v3") == client.For("https://10.0.0.5/") {
		t.Error("a second host on the same network got the permissive client")
	}
	if err := client.CheckConfigured("https://git.acme.internal/api/v3"); err != nil {
		t.Errorf("the connection's own host was refused: %v", err)
	}
	// http is refused before any of this is reached.
	if err := client.CheckConfigured("http://git.acme.internal/api/v3"); err == nil {
		t.Error("a plain-http host was accepted")
	}

	// A second connection gets a second client, and neither inherits the
	// other's exemption.
	other := &database.GitConnection{ID: "conn-two", BaseURL: "https://git.other.internal"}
	if r.clientFor(other).MirrorHost() == client.MirrorHost() {
		t.Error("two connections share one exemption")
	}
}

// DG-11 (security review 2026-11-14): a search reaches past the first page of
// 100, and a plain listing of a full page says there is more.
func TestPickerSearchFindsARepositoryPastTheFirstPage(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := appConnection(t, db, enc)
	var names []string
	for i := 0; i < 150; i++ {
		names = append(names, fmt.Sprintf("acme/repo-%03d", i))
	}
	names[140] = "acme/the-needle"
	host := newFakeHost(t, map[string][]string{"the-persons-token": names})
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)
	seedUser(t, db, "dev")
	stored, err := enc.Encrypt("the-persons-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutGitConnectionIdentity(conn.ID, "dev", stored, nil); err != nil {
		t.Fatal(err)
	}

	got, more, err := r.ListRepositoriesFor(context.Background(), conn, "dev", "needle", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FullName != "acme/the-needle" || more {
		t.Errorf("search = %+v (more %v), want the one repository on page 2", got, more)
	}

	got, more, err = r.ListRepositoriesFor(context.Background(), conn, "dev", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 100 || !more {
		t.Errorf("listing = %d repositories (more %v), want 100 and more", len(got), more)
	}
}

// DG-12 step 1 (security review 2026-11-14): a repository past the walk's
// bound is "could not be confirmed", never "not installed" — and a short,
// complete list still says "not installed".
func TestCoveragePastTheBoundIsUnconfirmedNotRefusedAsUninstalled(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := appConnection(t, db, enc)
	var names []string
	for i := 0; i < maxCoveragePages*reposPerPage+50; i++ {
		names = append(names, fmt.Sprintf("acme/repo-%04d", i))
	}
	host := newFakeHost(t, map[string][]string{"ghs_for_": names})
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)

	past := host.server.URL + "/" + names[len(names)-1]
	if why := r.CoverageRefusal(context.Background(), conn, past); !strings.Contains(why, "could not be confirmed") {
		t.Errorf("past the bound = %q, want could not be confirmed", why)
	}
	if why := r.CoverageRefusal(context.Background(), conn, host.server.URL+"/"+names[0]); why != "" {
		t.Errorf("a listed repository = %q, want covered", why)
	}
}

func TestCoverageOfACompleteListStillSaysNotInstalled(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := appConnection(t, db, enc)
	host := newFakeHost(t, map[string][]string{"ghs_for_": {"acme/one"}})
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)
	if why := r.CoverageRefusal(context.Background(), conn, host.server.URL+"/acme/other"); !strings.Contains(why, "is not installed on") {
		t.Errorf("a complete list without it = %q, want not installed", why)
	}
}
