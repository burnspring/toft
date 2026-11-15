package database_test

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/sources"
)

// A fresh install deploys from anywhere and accepts uploads. The whole feature
// has to be invisible until somebody turns it on: an install that never touches
// the setting never notices it exists.
func TestFreshInstallDeploysFromAnywhere(t *testing.T) {
	db := dbtest.New(t)

	p, err := db.ResolveSourcePolicy()
	if err != nil {
		t.Fatalf("ResolveSourcePolicy: %v", err)
	}
	if p.Mode != sources.ModeAny {
		t.Errorf("mode = %q, want %q", p.Mode, sources.ModeAny)
	}
	if !p.UploadsEnabled {
		t.Error("uploads are off on a fresh install")
	}
	if p.Restricted() {
		t.Error("a fresh install reports itself restricted")
	}
	if err := p.Allows("https://github.com/anyone/anything"); err != nil {
		t.Errorf("a fresh install refused a repository: %v", err)
	}
}

func TestSetAndResolveSourcePolicy(t *testing.T) {
	db := dbtest.New(t)

	err := db.SetSourcePolicy(sources.ModeAllowlist, false, []database.AllowedGitSource{
		{Pattern: "github.com/acme/*", Note: "our org"},
		{Pattern: "gitlab.internal/*"},
	}, database.BuiltinAdminID, "req", "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}

	p, err := db.ResolveSourcePolicy()
	if err != nil {
		t.Fatalf("ResolveSourcePolicy: %v", err)
	}
	if p.Mode != sources.ModeAllowlist || p.UploadsEnabled {
		t.Fatalf("resolved %+v", p.Policy)
	}
	if err := p.Allows("https://github.com/acme/dashboard"); err != nil {
		t.Errorf("an allowed source was refused: %v", err)
	}
	if err := p.Allows("https://github.com/randomguy/tool"); err == nil {
		t.Error("a source on no pattern was allowed")
	}

	// The two switches are readable through the settings document as well, so
	// one settings read answers every question about the install.
	s, err := db.GetPlatformSettings()
	if err != nil {
		t.Fatalf("GetPlatformSettings: %v", err)
	}
	if s.GitSourcePolicy != sources.ModeAllowlist || s.SourceUploadsEnabled {
		t.Errorf("settings disagree with the resolver: %+v", s)
	}
}

// The diff is the compensating control for keeping the list in the database:
// an administrator can widen it and then deploy, and "settings were changed"
// would not be a record of that.
func TestPatternChangesAreAuditedAsADiff(t *testing.T) {
	db := dbtest.New(t)

	if err := db.SetSourcePolicy(sources.ModeAllowlist, true, []database.AllowedGitSource{
		{Pattern: "github.com/acme/*", Note: "our org"},
	}, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}
	// Same list again: nothing changed, so nothing new is recorded.
	if err := db.SetSourcePolicy(sources.ModeAllowlist, true, []database.AllowedGitSource{
		{Pattern: "github.com/acme/*", Note: "our org"},
	}, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy (idempotent): %v", err)
	}
	// And now one swapped for another.
	if err := db.SetSourcePolicy(sources.ModeAllowlist, true, []database.AllowedGitSource{
		{Pattern: "gitlab.internal/*"},
	}, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy (swap): %v", err)
	}

	rows, err := db.Conn().Query(
		`SELECT action, COALESCE(before::text,''), COALESCE(after::text,'')
		   FROM audit_log WHERE action LIKE 'source_%'`)
	if err != nil {
		t.Fatalf("reading audit_log: %v", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	added, removed := "", ""
	for rows.Next() {
		var action, before, after string
		if err := rows.Scan(&action, &before, &after); err != nil {
			t.Fatalf("scanning audit_log: %v", err)
		}
		counts[action]++
		if action == "source_pattern_added" && strings.Contains(after, "gitlab.internal") {
			added = after
		}
		if action == "source_pattern_removed" && strings.Contains(before, "acme") {
			removed = before
		}
	}
	if counts["source_policy_changed"] != 1 {
		t.Errorf("source_policy_changed written %d times, want 1 (the mode moved once)", counts["source_policy_changed"])
	}
	if counts["source_pattern_added"] != 2 {
		t.Errorf("source_pattern_added written %d times, want 2", counts["source_pattern_added"])
	}
	if counts["source_pattern_removed"] != 1 {
		t.Errorf("source_pattern_removed written %d times, want 1", counts["source_pattern_removed"])
	}
	if added == "" || removed == "" {
		t.Errorf("the diff does not name the patterns: added=%q removed=%q", added, removed)
	}
}

func TestUsageCountsAndNonCompliance(t *testing.T) {
	db := dbtest.New(t)
	mkApp(t, db, "one") // https://github.com/example/one
	mkApp(t, db, "two") // https://github.com/example/two
	other := mkApp(t, db, "three")
	if err := db.SetAppRepoURL(other.ID, "https://gitlab.internal/dept/three"); err != nil {
		t.Fatalf("SetAppRepoURL: %v", err)
	}

	if err := db.SetSourcePolicy(sources.ModeAllowlist, true, []database.AllowedGitSource{
		{Pattern: "github.com/example/*"},
		{Pattern: "github.com/unused/*"},
	}, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatalf("SetSourcePolicy: %v", err)
	}

	entries, err := db.ListAllowedGitSourcesWithUsage()
	if err != nil {
		t.Fatalf("ListAllowedGitSourcesWithUsage: %v", err)
	}
	usage := map[string]int{}
	for _, e := range entries {
		usage[e.Pattern] = e.Apps
	}
	if usage["github.com/example/*"] != 2 {
		t.Errorf("github.com/example/* covers %d apps, want 2", usage["github.com/example/*"])
	}
	// An entry added for one migration and long since irrelevant is exactly
	// what the count exists to make findable.
	if usage["github.com/unused/*"] != 0 {
		t.Errorf("an unused pattern reports %d apps, want 0", usage["github.com/unused/*"])
	}

	p, err := db.ResolveSourcePolicy()
	if err != nil {
		t.Fatalf("ResolveSourcePolicy: %v", err)
	}
	bad, err := db.NonCompliantApps(p.Policy)
	if err != nil {
		t.Fatalf("NonCompliantApps: %v", err)
	}
	if len(bad) != 1 || bad[0].Slug != "three" {
		t.Fatalf("non-compliant = %+v, want just three", bad)
	}
	if bad[0].OwnerName == "" {
		t.Error("the preview does not name the owner whose deploys are about to break")
	}
}

// Seeding: the install offers what it is already deploying from, at three
// widths, so a starting list is a few clicks rather than transcription.
func TestObservedSources(t *testing.T) {
	db := dbtest.New(t)
	mkApp(t, db, "alpha")
	mkApp(t, db, "beta")

	observed, err := db.ObservedSources()
	if err != nil {
		t.Fatalf("ObservedSources: %v", err)
	}
	got := map[string]database.ObservedSource{}
	for _, o := range observed {
		got[o.Pattern] = o
	}
	if o := got["github.com/*"]; o.Apps != 2 || o.Kind != "host" {
		t.Errorf("host suggestion = %+v, want 2 apps", o)
	}
	if o := got["github.com/example/*"]; o.Apps != 2 || o.Kind != "org" {
		t.Errorf("org suggestion = %+v, want 2 apps", o)
	}
	if o := got["github.com/example/alpha"]; o.Apps != 1 || o.Kind != "repo" {
		t.Errorf("repo suggestion = %+v, want 1 app", o)
	}
	// Widest first: the entry that covers the most apps is usually the one an
	// administrator wants to click.
	if observed[0].Kind != "host" {
		t.Errorf("the list leads with %q, want the host", observed[0].Kind)
	}
}

// An uploaded app has no remote and can never be non-compliant on the git
// policy. Counting it would make every preview wrong in the same direction.
func TestUploadedAppsAreNotAGitSource(t *testing.T) {
	db := dbtest.New(t)
	app := mkApp(t, db, "zipped")
	if err := db.SetAppRepoURL(app.ID, "upload://thing.zip"); err != nil {
		t.Fatalf("SetAppRepoURL: %v", err)
	}

	apps, err := db.ListAppSources()
	if err != nil {
		t.Fatalf("ListAppSources: %v", err)
	}
	for _, a := range apps {
		if a.AppID == app.ID {
			t.Fatal("an uploaded app is listed as a git source")
		}
	}

	bad, err := db.NonCompliantApps(sources.Policy{Mode: sources.ModeNone})
	if err != nil {
		t.Fatalf("NonCompliantApps: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("an uploaded app is non-compliant under mode none: %+v", bad)
	}
}

// A mode this binary does not understand reads as the most restrictive one.
// Failing open would silently remove a control somebody set; failing closed is
// loud, visible on the admin page, and one click to fix.
func TestUnknownModeFailsClosed(t *testing.T) {
	db := dbtest.New(t)
	if _, err := db.PutSetting(database.SettingGitSourcePolicy, "gallery_only", database.BuiltinAdminID); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	p, err := db.ResolveSourcePolicy()
	if err != nil {
		t.Fatalf("ResolveSourcePolicy: %v", err)
	}
	if p.Mode != sources.ModeNone {
		t.Errorf("mode = %q, want %q", p.Mode, sources.ModeNone)
	}
}
