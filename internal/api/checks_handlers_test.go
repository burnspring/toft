package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// The check list is the specification, and this is what makes that true rather
// than aspirational. Every failure below is one that would otherwise be silent:
// a check that stops running looks exactly like a check that passed.

var lowerSnake = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// checksRequest is the request the report is computed for. One check reads it
// — the trusted-proxy one — so a report is always asked for by somebody.
func checksRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/system/checks", nil)
	r.RemoteAddr = "127.0.0.1:34567"
	return r
}

func TestChecksRegistry_IsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	categories := map[string]bool{}
	for _, c := range checkCategories {
		categories[c] = true
	}

	for i, def := range checks {
		switch {
		case def.id == "":
			t.Errorf("check %d has no id", i)
		case !lowerSnake.MatchString(def.id):
			t.Errorf("check %q: id is not lower_snake", def.id)
		case seen[def.id]:
			t.Errorf("check %q: duplicate id — one of the two is unreachable in a report", def.id)
		}
		seen[def.id] = true

		if def.name == "" {
			t.Errorf("check %q has no name; it is what a timed-out or panicking check is filed under", def.id)
		}
		if !categories[def.category] {
			t.Errorf("check %q: category %q is not in the closed list", def.id, def.category)
		}
		if def.run == nil {
			t.Errorf("check %q has no function", def.id)
		}
		// At most one destination. Both set is a row that has to be resolved
		// two ways, and the SPA would have to pick.
		if def.href != "" && def.setting != "" {
			t.Errorf("check %q sets both href and setting; at most one", def.id)
		}
		if def.href != "" && !strings.HasPrefix(def.href, "/admin/") {
			t.Errorf("check %q: href %q is not an admin route", def.id, def.href)
		}
	}
}

// newBareServer is an install that has configured nothing optional: no artifact
// store, no isolated backend, no mailer, no Slack, no license, no git
// connection, no SCIM token, no directory mapping. The three data directories
// exist because they are not optional — an install without them is broken
// rather than minimal.
func newBareServer(t *testing.T) *Server {
	t.Helper()
	db := dbtest.New(t)
	cfg := &config.Config{
		BaseURL:  "http://localhost:8080",
		AppsDir:  t.TempDir(),
		LogsDir:  t.TempDir(),
		CacheDir: t.TempDir(),
		// The image's and the chart's defaults, so the containment row reads
		// an install as shipped rather than the binary started bare.
		AppUserPerApp:    true,
		BlockAWSMetadata: true,
	}
	// An enabled provider, because "local sign-in is the only way in" is a real
	// warning about a real install and not one about an optional capability —
	// it would fail this test for the opposite of the reason the test exists.
	// Providers are database rows, so setting the environment's GitHub client
	// is not enough to make one.
	p := &database.AuthProvider{
		ID: "okta", Kind: database.ProviderKindOIDC, DisplayName: "Okta",
		Enabled: true, ClientID: "client-id", Issuer: "https://okta.example.com",
		DefaultRole: database.SystemRoleViewer, AutoProvision: true,
	}
	p.SetEncryptedSecret("ciphertext")
	if err := db.CreateAuthProvider(p, database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: "admin-1",
	}); err != nil {
		t.Fatalf("CreateAuthProvider: %v", err)
	}
	return newTestServerWithConfig(t, db, cfg)
}

// This is ADR §30's central rule, and it is the one that decays first — one
// well-meant check at a time, each of which looks reasonable on its own. An
// install that has not configured an optional capability is not a degraded
// install, and must not be told it is.
func TestChecks_NothingConfigured_NothingWarns(t *testing.T) {
	srv := newBareServer(t)

	// Excluded, and each for a stated reason rather than to make the test
	// pass. These read the machine the test is running on, not the install's
	// configuration: a CI box with no Node, or a disk at 90%, would fail this
	// test for a reason that has nothing to do with what it is about.
	environmental := func(id string) bool {
		return id == "disk_space" || id == "memory" || strings.HasPrefix(id, "toolchain_")
	}

	for _, row := range srv.runChecks(checksRequest()) {
		if environmental(row.ID) {
			continue
		}
		if row.Status == checkWarn || row.Status == checkFail {
			t.Errorf("%s (%s) is %s on an install that configured nothing: %q",
				row.ID, row.Category, row.Status, row.Detail)
		}
	}
}

// Every row the report produces is well-formed, whoever produced it: the ids
// are the wire contract an operator's script greps for, and a row with no
// category lands in no group on the page.
func TestChecks_EveryRowIsWellFormed(t *testing.T) {
	srv := newBareServer(t)
	categories := map[string]bool{}
	for _, c := range checkCategories {
		categories[c] = true
	}

	seen := map[string]bool{}
	for _, row := range srv.runChecks(checksRequest()) {
		if seen[row.ID] {
			t.Errorf("duplicate row id %q", row.ID)
		}
		seen[row.ID] = true
		// The ids a run emits, not only the ones the registry declares: the
		// toolchain rows are built at run time and once read `toolchain_Node.js`.
		if !lowerSnake.MatchString(row.ID) {
			t.Errorf("row id %q is not lower_snake", row.ID)
		}
		if row.Name == "" {
			t.Errorf("row %q has no name", row.ID)
		}
		if !categories[row.Category] {
			t.Errorf("row %q: category %q is not in the closed list", row.ID, row.Category)
		}
		switch row.Status {
		case checkPass, checkWarn, checkFail, checkSkip:
		default:
			t.Errorf("row %q: %q is not one of the four verdicts", row.ID, row.Status)
		}
		if row.Detail == "" {
			t.Errorf("row %q has no detail, which is the whole value of the row", row.ID)
		}
		// A fact links nowhere. This is the quiet signal that separates a
		// finding from the shape of the install, and it is enforced by the
		// report rather than by each check remembering.
		if (row.Status == checkPass || row.Status == checkSkip) && (row.Href != "" || row.Setting != "") {
			t.Errorf("row %q is a %s and carries a destination", row.ID, row.Status)
		}
	}
}

// withChecks swaps the registry for one test. The list is a package var so that
// it can be walked; that also makes it injectable, which is the only way to
// exercise a check that hangs or panics without shipping one that does.
func withChecks(t *testing.T, defs ...checkDef) {
	t.Helper()
	original := checks
	checks = defs
	t.Cleanup(func() { checks = original })
}

func TestChecks_OneThatHangsIsAbandoned(t *testing.T) {
	srv := newBareServer(t)
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })

	withChecks(t,
		checkDef{id: "hangs", name: "Hangs", category: catPlatform, href: "/admin/settings",
			run: func(_ *Server, _ context.Context, rp *checkReport) {
				<-released // never, within this test
				rp.add("hangs", "Hangs", checkPass, "unreachable")
			}},
		checkDef{id: "after", name: "After", category: catPlatform,
			run: func(_ *Server, _ context.Context, rp *checkReport) {
				rp.add("after", "After", checkPass, "still ran")
			}},
	)

	start := time.Now()
	rows := srv.runChecks(checksRequest())
	elapsed := time.Since(start)

	if elapsed > checkBudget+2*time.Second {
		t.Fatalf("a hanging check held the report for %s; the bound is %s", elapsed, checkBudget)
	}
	if len(rows) != 2 {
		t.Fatalf("expected the hanging check's row and the one after it, got %d", len(rows))
	}
	if rows[0].Status != checkWarn {
		t.Errorf("a check the platform could not ask is warn, not %s", rows[0].Status)
	}
	if !strings.Contains(rows[0].Detail, "Did not answer") {
		t.Errorf("the row does not say it timed out: %q", rows[0].Detail)
	}
	// It is a finding, so it carries its check's destination.
	if rows[0].Href != "/admin/settings" {
		t.Errorf("timed-out row lost its destination: %q", rows[0].Href)
	}
	if rows[1].ID != "after" || rows[1].Status != checkPass {
		t.Errorf("the check after the hanging one did not run: %+v", rows[1])
	}
}

func TestChecks_OneThatPanicsIsContained(t *testing.T) {
	srv := newBareServer(t)
	withChecks(t,
		checkDef{id: "panics", name: "Panics", category: catPlatform,
			run: func(_ *Server, _ context.Context, _ *checkReport) {
				panic("the secret is hunter2")
			}},
		checkDef{id: "after", name: "After", category: catPlatform,
			run: func(_ *Server, _ context.Context, rp *checkReport) {
				rp.add("after", "After", checkPass, "still ran")
			}},
	)

	rows := srv.runChecks(checksRequest())
	if len(rows) != 2 {
		t.Fatalf("expected two rows, got %d", len(rows))
	}
	if rows[0].Status != checkFail {
		t.Errorf("a panicking check is a fail, not %s", rows[0].Status)
	}
	// The panic value is logged, never rendered: it is not a string this page
	// has vetted, and D5 is about every byte of a detail.
	if strings.Contains(rows[0].Detail, "hunter2") {
		t.Errorf("the panic value reached the detail: %q", rows[0].Detail)
	}
	if rows[1].ID != "after" {
		t.Error("a panic took down the rest of the report")
	}
}

// The budget, not the per-check bound: a report that has run out of time still
// accounts for the checks it never reached, because a row that is absent and a
// row that passed look identical on the page.
func TestChecks_BudgetExhausted_RemainingChecksAreAccountedFor(t *testing.T) {
	srv := newBareServer(t)
	var defs []checkDef
	for _, id := range []string{"first", "second", "third"} {
		defs = append(defs, checkDef{id: id, name: id, category: catPlatform,
			run: func(_ *Server, _ context.Context, rp *checkReport) {
				t.Errorf("%s ran with no budget left", id)
			}})
	}
	withChecks(t, defs...)

	original := reportBudget
	reportBudget = 0
	t.Cleanup(func() { reportBudget = original })

	rows := srv.runChecks(checksRequest())
	if len(rows) != 3 {
		t.Fatalf("every check is accounted for even when unreached; got %d rows", len(rows))
	}
	for _, row := range rows {
		if row.Status != checkWarn {
			t.Errorf("%s: an unreached check is warn, not %s", row.ID, row.Status)
		}
		if !strings.Contains(row.Detail, "ran out of time") {
			t.Errorf("%s: the row does not say why it did not run: %q", row.ID, row.Detail)
		}
	}
}

func TestThousands(t *testing.T) {
	// `261380 advisories` on the operator's most-read page is a figure nobody
	// parses. The SPA has separated its numbers since it had any.
	for in, want := range map[int]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000", 12345: "12,345",
		261380: "261,380", 1234567: "1,234,567", -4321: "-4,321",
	} {
		if got := thousands(in); got != want {
			t.Errorf("thousands(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCountOfPluralizesProperly(t *testing.T) {
	// Eleven strings on this page said "3 connection(s)" and "1 provider(s)"
	// in a product that pluralizes properly everywhere else.
	for _, c := range []struct {
		n    int
		noun string
		want string
	}{
		{0, "app", "0 apps"},
		{1, "app", "1 app"},
		{2, "app", "2 apps"},
		{1500, "advisory record", "1,500 advisory records"},
	} {
		if got := countOf(c.n, c.noun); got != c.want {
			t.Errorf("countOf(%d, %q) = %q, want %q", c.n, c.noun, got, c.want)
		}
	}
}
