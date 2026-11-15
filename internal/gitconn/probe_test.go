package gitconn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/safefetch"
)

// The connection test: one authenticated call to the host, on demand.
//
// What these assert is the property the feature exists for — that an
// administrator is told *which field to go and fix*. A test that answered
// "couldn't reach the host" to a revoked token, a rotated ENCRYPTION_KEY and a
// mistyped base URL alike would be a button that proves the network works.

// tokenHost is a host that answers "who am I" for exactly one bearer.
//
// `paths` is what it serves, so one fake can be a GitHub Enterprise Server
// (/api/v3) or a GitLab (/api/v4) — and, for the group-token case, a GitLab
// that refuses /user and answers /version.
type tokenHost struct {
	server *httptest.Server
}

func newTokenHost(t *testing.T, wantBearer string, routes map[string]any) *tokenHost {
	t.Helper()
	mux := http.NewServeMux()
	for path, body := range routes {
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != wantBearer {
				w.WriteHeader(http.StatusUnauthorized)
				// A body, so a test would catch it if one were ever echoed
				// into an error: the rule is the host's status and never its
				// body.
				_, _ = w.Write([]byte(`{"message":"Bad credentials","token":"` + wantBearer + `"}`))
				return
			}
			writeJSONBody(w, body)
		})
	}
	h := &tokenHost{server: httptest.NewTLSServer(mux)}
	t.Cleanup(h.server.Close)
	return h
}

func writeJSONBody(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	switch v := body.(type) {
	case string:
		_, _ = w.Write([]byte(v))
	default:
		_, _ = w.Write([]byte("{}"))
	}
}

func (h *tokenHost) wire(t *testing.T, db *database.DB, r *Resolver, conn *database.GitConnection) {
	t.Helper()
	conn.BaseURL = h.server.URL
	if err := db.UpdateGitConnection(conn, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID,
	}); err != nil {
		t.Fatalf("UpdateGitConnection: %v", err)
	}
	client := safefetch.NewForTesting(strings.TrimPrefix(h.server.URL, "https://"),
		h.server.Client().Transport.(*http.Transport).TLSClientConfig, false, hostTimeout)
	r.SetClientForTesting(conn.ID, client)
}

// tokenConnection makes a token-backed row holding `secret`, of either kind.
func tokenConnection(t *testing.T, db *database.DB, enc *crypto.Encryptor, id, kind, secret string) *database.GitConnection {
	t.Helper()
	ciphertext, err := enc.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	conn := &database.GitConnection{
		ID: id, Kind: kind, DisplayName: "Acme " + kind,
		BaseURL: "https://git.acme.internal", AuthMethod: database.GitAuthToken,
		Enabled: true, Coverage: "acme/*",
	}
	conn.SetEncryptedCloneSecret(ciphertext)
	if err := db.CreateGitConnection(conn, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: database.BuiltinAdminID,
	}); err != nil {
		t.Fatalf("CreateGitConnection: %v", err)
	}
	return conn
}

// The App method's test is the mint, because the mint is what a clone does one
// step before git is spawned. A pass reports what the App is installed on.
func TestTestingAnAppConnectionMintsAndCountsWhatItReaches(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := appConnection(t, db, enc)
	host := newFakeHost(t, map[string][]string{
		"ghs_for_": {"acme/payroll", "acme/reports"},
	})
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)

	res := r.Check(context.Background(), conn)
	if !res.OK {
		t.Fatalf("a working App connection failed: %s", res.Summary)
	}
	if res.Repositories == nil || *res.Repositories != 2 {
		t.Fatalf("repositories = %v, want 2", res.Repositories)
	}
	if !strings.Contains(res.Summary, "2 repositories") {
		t.Fatalf("summary does not say what the App reaches: %q", res.Summary)
	}
}

// A token connection names the account the host says the credential is, which
// is the fact an administrator uses to decide whether it is the one they meant.
func TestTestingATokenConnectionNamesTheAccountTheHostReports(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := tokenConnection(t, db, enc, "conn-gh", database.GitConnGitHub, "ghp_good")
	host := newTokenHost(t, "ghp_good", map[string]any{
		"/api/v3/user": `{"login":"acme-deploy"}`,
	})
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)

	res := r.Check(context.Background(), conn)
	if !res.OK {
		t.Fatalf("a working token connection failed: %s", res.Summary)
	}
	if res.Identity != "acme-deploy" {
		t.Fatalf("identity = %q, want acme-deploy", res.Identity)
	}
}

// **A GitLab group access token cannot always answer "who am I", and that is
// not a bad credential.** The fallback is an authenticated route with no notion
// of a user; without it this button would tell an administrator to replace a
// token that works.
func TestAGitLabConnectionFallsBackToVersionWhenTheTokenIsNotAUser(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := tokenConnection(t, db, enc, "conn-gl", database.GitConnGitLab, "glpat_good")
	// No /user route at all, so it 404s — the shape a group token meets.
	host := newTokenHost(t, "glpat_good", map[string]any{
		"/api/v4/version": `{"version":"17.2.1"}`,
	})
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)

	res := r.Check(context.Background(), conn)
	if !res.OK {
		t.Fatalf("a working group token was reported as broken: %s", res.Summary)
	}
	if !strings.Contains(res.Summary, "17.2.1") {
		t.Fatalf("summary does not name the host it reached: %q", res.Summary)
	}
}

// A refused credential is a *result*, and the sentence has to name the
// credential rather than the network — and must not carry the host's body,
// which is where a token would end up in a log.
func TestARefusedTokenIsReportedAsTheCredentialAndCarriesNoBody(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := tokenConnection(t, db, enc, "conn-bad", database.GitConnGitHub, "ghp_revoked")
	host := newTokenHost(t, "ghp_expected", map[string]any{
		"/api/v3/user": `{"login":"nobody"}`,
	})
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)

	res := r.Check(context.Background(), conn)
	if res.OK {
		t.Fatal("a revoked token passed the test")
	}
	if !strings.Contains(res.Summary, "access token") {
		t.Fatalf("summary does not name the credential: %q", res.Summary)
	}
	if strings.Contains(res.Summary, "ghp_") || strings.Contains(res.Summary, "Bad credentials") {
		t.Fatalf("the host's body reached the summary: %q", res.Summary)
	}
}

// **A rotated ENCRYPTION_KEY is not a revoked token**, and the whole reason this
// package exists is that those two used to be indistinguishable to the person
// reading the message. The test must not send anything to the host either:
// there is nothing to ask.
func TestAnUnreadableCredentialSaysSoRatherThanBlamingTheHost(t *testing.T) {
	db := dbtest.New(t)
	conn := tokenConnection(t, db, testEncryptor(t, testKey), "conn-lost", database.GitConnGitHub, "ghp_good")

	// A resolver holding a different key, which is what a rotation looks like.
	other := NewResolver(db, testEncryptor(t, strings.Repeat("ab", 32)))
	res := other.Check(context.Background(), conn)

	if res.OK {
		t.Fatal("a credential this install cannot open passed the test")
	}
	if !strings.Contains(res.Summary, "re-enter") {
		t.Fatalf("summary does not say what to do: %q", res.Summary)
	}
	if strings.Contains(res.Summary, "refused") || strings.Contains(res.Summary, "Couldn't reach") {
		t.Fatalf("an unreadable credential was reported as a host failure: %q", res.Summary)
	}
}

// A working credential on a disabled connection is two facts, and either alone
// misleads: "it works" hides that every deploy through it fails.
func TestADisabledConnectionThatPassesSaysNothingDeploysThroughIt(t *testing.T) {
	db := dbtest.New(t)
	enc := testEncryptor(t, testKey)
	conn := tokenConnection(t, db, enc, "conn-off", database.GitConnGitHub, "ghp_good")
	conn.Enabled = false
	host := newTokenHost(t, "ghp_good", map[string]any{
		"/api/v3/user": `{"login":"acme-deploy"}`,
	})
	r := NewResolver(db, enc)
	host.wire(t, db, r, conn)

	res := r.Check(context.Background(), conn)
	if !res.OK {
		t.Fatalf("a disabled connection's credential was reported broken: %s", res.Summary)
	}
	if !res.Disabled || !strings.Contains(res.Summary, "disabled") {
		t.Fatalf("summary does not say the connection is disabled: %q", res.Summary)
	}
}
