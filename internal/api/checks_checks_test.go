package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"toft/internal/config"

	"toft/internal/database"
)

// Per-check verdict tables. Each threshold is exercised from both sides,
// because a boundary is where a threshold check is wrong or nowhere.

// rowFor runs one check against a server and returns the single row it
// produced. Direct rather than through runChecks, so a table stays about the
// check's own judgement.
func rowFor(t *testing.T, srv *Server, run func(*Server, context.Context, *checkReport)) systemCheck {
	t.Helper()
	rp := &checkReport{}
	run(srv, context.Background(), rp)
	if len(rp.checks) != 1 {
		t.Fatalf("expected exactly one row, got %d", len(rp.checks))
	}
	return rp.checks[0]
}

func mkConnection(t *testing.T, srv *Server, id string, shape func(*database.GitConnection)) {
	t.Helper()
	c := &database.GitConnection{
		ID: id, Kind: "github", DisplayName: strings.ToUpper(id),
		BaseURL: "https://github.com", AuthMethod: "token", Enabled: true,
	}
	shape(c)
	if err := srv.db.CreateGitConnection(c, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin-1",
	}); err != nil {
		t.Fatalf("CreateGitConnection: %v", err)
	}
}

// record is how a connection's last outcome is written in production: the
// recorder, not the insert.
func record(t *testing.T, srv *Server, id, failure string) {
	t.Helper()
	if _, err := srv.db.RecordGitConnectionResult(id, failure); err != nil {
		t.Fatalf("RecordGitConnectionResult: %v", err)
	}
}

func ago(d time.Duration) *time.Time   { v := time.Now().Add(-d); return &v }
func ahead(d time.Duration) *time.Time { v := time.Now().Add(d); return &v }

// D6 — the credential is a countdown, and a failure is the last one.
func TestCheckGitConnections(t *testing.T) {
	t.Run("no connection is a skip, not a healthy pass", func(t *testing.T) {
		srv := newBareServer(t)
		row := rowFor(t, srv, (*Server).checkGitConnections)
		if row.Status != checkSkip {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})

	t.Run("a disabled connection is excluded, not reported healthy", func(t *testing.T) {
		srv := newBareServer(t)
		// Expired *and* disabled: if it were counted at all this would fail.
		mkConnection(t, srv, "old", func(c *database.GitConnection) {
			c.Enabled = false
			c.CredentialExpiresAt = ago(30 * 24 * time.Hour)
		})
		row := rowFor(t, srv, (*Server).checkGitConnections)
		if row.Status != checkSkip {
			t.Fatalf("a disabled connection produced %s: %s", row.Status, row.Detail)
		}
	})

	t.Run("expired is a fail and names the app count", func(t *testing.T) {
		srv := newBareServer(t)
		mkConnection(t, srv, "acme", func(c *database.GitConnection) {
			c.CredentialExpiresAt = ago(time.Hour)
		})
		row := rowFor(t, srv, (*Server).checkGitConnections)
		if row.Status != checkFail {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
		if !strings.Contains(row.Detail, "ACME") {
			t.Errorf("the failing connection is not named: %q", row.Detail)
		}
	})

	// Both sides of the fourteen days.
	t.Run("thirteen days out warns", func(t *testing.T) {
		srv := newBareServer(t)
		mkConnection(t, srv, "acme", func(c *database.GitConnection) {
			c.CredentialExpiresAt = ahead(13 * 24 * time.Hour)
		})
		if row := rowFor(t, srv, (*Server).checkGitConnections); row.Status != checkWarn {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})
	t.Run("fifteen days out passes", func(t *testing.T) {
		srv := newBareServer(t)
		mkConnection(t, srv, "acme", func(c *database.GitConnection) {
			c.CredentialExpiresAt = ahead(15 * 24 * time.Hour)
		})
		if row := rowFor(t, srv, (*Server).checkGitConnections); row.Status != checkPass {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})

	t.Run("a failure after the last success warns", func(t *testing.T) {
		srv := newBareServer(t)
		mkConnection(t, srv, "acme", func(*database.GitConnection) {})
		// Written by the recorder rather than by the insert, which is where
		// they come from in production too.
		record(t, srv, "acme", "")
		record(t, srv, "acme", "remote: Repository not found")
		row := rowFor(t, srv, (*Server).checkGitConnections)
		if row.Status != checkWarn {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
		if !strings.Contains(row.Detail, "Repository not found") {
			t.Errorf("the host's own words are missing: %q", row.Detail)
		}
	})

	t.Run("a success after the last failure passes", func(t *testing.T) {
		srv := newBareServer(t)
		mkConnection(t, srv, "acme", func(*database.GitConnection) {})
		record(t, srv, "acme", "remote: Repository not found")
		record(t, srv, "acme", "")
		if row := rowFor(t, srv, (*Server).checkGitConnections); row.Status != checkPass {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})
}

// D5 — no detail carries a credential. The report is readable by a personal key
// whose owner is a system administrator, so this is not academic.
//
// Not a regex over the output, which is a test that passes by accident: a
// connection is built with every secret it can hold populated with a distinct
// marker, and the whole report is asserted not to contain any of them.
func TestChecks_NoDetailCarriesASecret(t *testing.T) {
	srv := newBareServer(t)

	secrets := map[string]string{
		"clone secret":   "SECRET-clone-aaaaaaaa",
		"client secret":  "SECRET-client-bbbbbbbb",
		"webhook secret": "SECRET-webhook-cccccc",
	}
	mkConnection(t, srv, "acme", func(c *database.GitConnection) {
		c.ClientID = "client-id-not-a-secret"
		c.GitHubAppID = "12345"
		c.InstallationID = "67890"
		c.CredentialExpiresAt = ago(time.Hour) // force the fail branch, the wordiest one
		c.SetEncryptedCloneSecret(secrets["clone secret"])
		c.SetEncryptedClientSecret(secrets["client secret"])
		c.SetEncryptedWebhookSecret(secrets["webhook secret"])
	})

	record(t, srv, "acme", "remote: Invalid username or password")

	for _, row := range srv.runChecks(checksRequest()) {
		for name, secret := range secrets {
			if strings.Contains(row.Detail, secret) {
				t.Errorf("row %q carries the %s: %q", row.ID, name, row.Detail)
			}
		}
	}
}

// D7 — the expiry the save path could only check once.
func TestCheckSecurityTxt(t *testing.T) {
	publish := func(t *testing.T, srv *Server, expires string) {
		t.Helper()
		seedUser(t, srv.db, "admin-1", database.SystemRoleAdmin)
		if _, err := srv.db.PutSetting(database.SettingSecurityTxtContact, "mailto:security@example.com", "admin-1"); err != nil {
			t.Fatalf("contact: %v", err)
		}
		if _, err := srv.db.PutSetting(database.SettingSecurityTxtExpires, expires, "admin-1"); err != nil {
			t.Fatalf("expires: %v", err)
		}
	}
	day := func(d time.Duration) string { return time.Now().UTC().Add(d).Format("2006-01-02") }

	t.Run("not published is a skip", func(t *testing.T) {
		srv := newBareServer(t)
		if row := rowFor(t, srv, (*Server).checkSecurityTxt); row.Status != checkSkip {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})

	t.Run("expired warns and never fails", func(t *testing.T) {
		srv := newBareServer(t)
		publish(t, srv, day(-24*time.Hour))
		row := rowFor(t, srv, (*Server).checkSecurityTxt)
		if row.Status != checkWarn {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
		if !strings.Contains(row.Detail, "abandoned") {
			t.Errorf("the detail does not say what an expired file reads as: %q", row.Detail)
		}
	})

	// Both sides of the thirty days.
	t.Run("twenty-nine days out warns", func(t *testing.T) {
		srv := newBareServer(t)
		publish(t, srv, day(29*24*time.Hour))
		if row := rowFor(t, srv, (*Server).checkSecurityTxt); row.Status != checkWarn {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})
	t.Run("thirty-one days out passes", func(t *testing.T) {
		srv := newBareServer(t)
		publish(t, srv, day(31*24*time.Hour))
		if row := rowFor(t, srv, (*Server).checkSecurityTxt); row.Status != checkPass {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})

	t.Run("an unreadable date is absent, not a finding", func(t *testing.T) {
		srv := newBareServer(t)
		publish(t, srv, "next tuesday")
		if row := rowFor(t, srv, (*Server).checkSecurityTxt); row.Status != checkSkip {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})
}

// mkPublicApp gives the install a public surface, which is what makes it a
// thing the vendor's prober would ever look at.
func mkPublicApp(t *testing.T, srv *Server) {
	t.Helper()
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	if err := srv.db.CreateApp(&database.App{
		ID: "app-public", Slug: "public-app", Name: "Public App", OwnerID: "owner",
		Framework: "go", Status: database.AppStatusRunning,
		Visibility: database.VisibilityPublic,
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
}

// D10 — a database ahead is as broken as one behind, and the check says which.
func TestClockVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		skew time.Duration
		want string
	}{
		{"in step", 100 * time.Millisecond, checkPass},
		{"one second behind", -time.Second, checkPass},
		{"two seconds ahead warns", 2 * time.Second, checkWarn},
		{"two seconds behind warns", -2 * time.Second, checkWarn},
		{"thirty seconds ahead fails", 30 * time.Second, checkFail},
		{"thirty seconds behind fails", -30 * time.Second, checkFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, detail := clockVerdict(tc.skew)
			if status != tc.want {
				t.Fatalf("got %s, want %s: %s", status, tc.want, detail)
			}
			if tc.want == checkPass {
				return
			}
			// The direction is words, and getting it backwards would send an
			// operator to the wrong machine.
			wantWord := "ahead of"
			if tc.skew < 0 {
				wantWord = "behind"
			}
			if !strings.Contains(detail, wantWord) {
				t.Errorf("detail does not say %q: %q", wantWord, detail)
			}
		})
	}
}

// rowsFor is rowFor for the checks that legitimately emit more than one row.
func rowsFor(t *testing.T, srv *Server, run func(*Server, context.Context, *checkReport)) map[string]systemCheck {
	t.Helper()
	rp := &checkReport{req: checksRequest()}
	run(srv, context.Background(), rp)
	out := map[string]systemCheck{}
	for _, row := range rp.checks {
		out[row.ID] = row
	}
	return out
}

// D17 — a tag that moves is not a pin, and a registry port is not a tag.
func TestImageTag(t *testing.T) {
	for _, tc := range []struct{ ref, want string }{
		{"ghcr.io/acme/runner-python:1.4.2", "1.4.2"},
		{"registry.internal:5000/runner-node:3", "3"},
		{"registry.internal:5000/runner-node", ""},
		{"runner-go", ""},
		{"acme/runner-r:latest", "latest"},
		{"acme/runner@sha256:abc123", "sha256:abc123"},
	} {
		if got := imageTag(tc.ref); got != tc.want {
			t.Errorf("imageTag(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

// D19 — the one check that reads the request, and the limit it states.
func TestCheckTrustedProxy(t *testing.T) {
	srv := newBareServer(t)

	run := func(r *http.Request) systemCheck {
		rp := &checkReport{req: r}
		srv.checkTrustedProxy(context.Background(), rp)
		if len(rp.checks) != 1 {
			t.Fatalf("expected one row, got %d", len(rp.checks))
		}
		return rp.checks[0]
	}

	t.Run("direct request with no forwarded header says nothing", func(t *testing.T) {
		if row := run(checksRequest()); row.Status != checkSkip {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})

	t.Run("a forwarded header from an untrusted peer is the finding", func(t *testing.T) {
		r := checksRequest()
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		row := run(r)
		if row.Status != checkWarn {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
		// The limit belongs in the detail, not only in a design document.
		if !strings.Contains(row.Detail, "the request that") {
			t.Errorf("the detail does not state that it speaks only for this request: %q", row.Detail)
		}
	})

	t.Run("through a trusted proxy passes", func(t *testing.T) {
		r := checksRequest()
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyViaProxy{}, true))
		if row := run(r); row.Status != checkPass {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})
}

// D20 — one administrator is not a configuration, and local sign-in is the
// answer to that risk rather than a second finding about it.
func TestCheckSignIn_LockoutRisk(t *testing.T) {
	disableLocal := func(t *testing.T, srv *Server) {
		t.Helper()
		if _, err := srv.db.Conn().Exec(
			`UPDATE auth_providers SET enabled = false WHERE kind = 'password'`); err != nil {
			t.Fatalf("disable local: %v", err)
		}
	}

	t.Run("local sign-in on is not a lockout finding", func(t *testing.T) {
		srv := newBareServer(t)
		if _, found := rowsFor(t, srv, (*Server).checkSignIn)["lockout_risk"]; found {
			t.Error("warned about lockout while local sign-in is the way in")
		}
	})

	t.Run("one administrator and no local sign-in warns", func(t *testing.T) {
		srv := newBareServer(t)
		disableLocal(t, srv)
		row, found := rowsFor(t, srv, (*Server).checkSignIn)["lockout_risk"]
		if !found {
			t.Fatal("no lockout row on an install with one administrator and no local sign-in")
		}
		if row.Status != checkWarn {
			t.Fatalf("got %s: %s", row.Status, row.Detail)
		}
	})

	t.Run("a second administrator clears it", func(t *testing.T) {
		srv := newBareServer(t)
		disableLocal(t, srv)
		seedUser(t, srv.db, "admin-2", database.SystemRoleAdmin)
		if _, found := rowsFor(t, srv, (*Server).checkSignIn)["lockout_risk"]; found {
			t.Error("still warned with two administrators")
		}
	})
}

// D22 — the estate learns the words, and none of them raises the verdict.
func TestCheckEstate_LocksAreCountedAndNeverAmber(t *testing.T) {
	srv := newBareServer(t)
	seedUser(t, srv.db, "owner", database.SystemRolePublisher)
	for i, shape := range []func(*database.App){
		func(a *database.App) { a.Status = database.AppStatusRunning },
		func(a *database.App) { a.Status = database.AppStatusRunning },
	} {
		app := &database.App{
			ID: fmt.Sprintf("app-%d", i), Slug: fmt.Sprintf("app-%d", i),
			Name: "App", OwnerID: "owner", Framework: "go",
			Visibility: database.VisibilityPrivate,
		}
		shape(app)
		if err := srv.db.CreateApp(app); err != nil {
			t.Fatalf("create app: %v", err)
		}
	}
	// By slug: CreateApp mints the id, so the one in the fixture is not the one
	// in the row.
	if _, err := srv.db.Conn().Exec(`UPDATE apps SET locked_at = NOW() WHERE slug = 'app-0'`); err != nil {
		t.Fatalf("lock: %v", err)
	}

	row := rowFor(t, srv, (*Server).checkEstate)
	// An administrator's deliberate act is not a fault, and a page that
	// amber-lit it would be teaching them to ignore amber.
	if row.Status != checkPass {
		t.Fatalf("a locked app raised the verdict to %s: %s", row.Status, row.Detail)
	}
	// And it still has to be visible, because "no crashed or failed apps out of
	// 2" reads as everything is serving.
	if !strings.Contains(row.Detail, "1 locked") {
		t.Errorf("the lock is invisible: %q", row.Detail)
	}
}

// The hsts row reads the same decision the routers make, so its four answers
// are the four shapes of configured address.
func TestCheckHSTS(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"single origin, https", config.Config{BaseURL: "https://toft.example"}, checkPass},
		{"two hosts, both https", config.Config{BaseURL: "https://toft.example",
			AppsBaseURL: "https://apps.example", Port: 8080, AppsPort: 8081}, checkPass},
		{"an http install offers no https", config.Config{BaseURL: "http://localhost:8080",
			AppsBaseURL: "http://localhost:8081", Port: 8080, AppsPort: 8081}, checkSkip},
		{"one host, mixed schemes, withheld", config.Config{BaseURL: "https://toft.example",
			AppsBaseURL: "http://toft.example:8081", Port: 8080, AppsPort: 8081}, checkWarn},
		{"two hosts, apps over http", config.Config{BaseURL: "https://toft.example",
			AppsBaseURL: "http://apps.example", Port: 8080, AppsPort: 8081}, checkWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			rp := &checkReport{}
			(&Server{cfg: &cfg}).checkHSTS(context.Background(), rp)
			if len(rp.checks) != 1 || rp.checks[0].Status != tc.want {
				t.Fatalf("got %+v, want one %s row", rp.checks, tc.want)
			}
		})
	}
}

// A failed database read is reported in a fixed sentence and never with the
// driver's error, which on a connect failure carries the DSN's user, database
// and host:port. The closed pool stands in for the unreachable server.
func TestADatabaseFailureDetailCarriesNoDriverError(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.db.Close(); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func(*Server, context.Context, *checkReport){
		"database": (*Server).checkDatabase,
		"clock":    (*Server).checkClock,
		"sign_in":  (*Server).checkSignIn,
	} {
		row := rowFor(t, srv, run)
		if strings.Contains(row.Detail, "closed") || !strings.Contains(row.Detail, "platform log") {
			t.Errorf("%s detail = %q, want the fixed sentence and not the driver's error", name, row.Detail)
		}
	}
}

// System info draws these two in colour, so this report must not pass them:
// the platform user fails, reachable metadata warns, and the shipped defaults
// are quiet.
func TestCheckAppContainment(t *testing.T) {
	rows := func(cfg *config.Config) map[string]string {
		rp := &checkReport{}
		(&Server{cfg: cfg}).checkAppContainment(context.Background(), rp)
		got := map[string]string{}
		for _, r := range rp.checks {
			got[r.ID] = r.Status
		}
		return got
	}

	if got := rows(&config.Config{AppUserPerApp: true, BlockAWSMetadata: true}); got["app_accounts"] != checkPass || got["cloud_metadata"] != checkPass {
		t.Errorf("shipped defaults: %v", got)
	}
	if got := rows(&config.Config{AppRunAsUser: "toft-apps"}); got["app_accounts"] != checkPass || got["cloud_metadata"] != checkWarn {
		t.Errorf("shared account, metadata open: %v", got)
	}
	if got := rows(&config.Config{}); got["app_accounts"] != checkFail {
		t.Errorf("the platform user must fail: %v", got)
	}
}
