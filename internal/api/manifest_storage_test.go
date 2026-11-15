package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
)

// planEnv is an app owned by "owner", a store "reports" (from delegateEnv) and
// a second store "archive", with nothing delegated.
func planEnv(t *testing.T) (*Server, *database.App, *database.StorageResource) {
	t.Helper()
	srv, _, app, res := delegateEnv(t)
	app.Framework = "flask"
	archive := &database.StorageResource{Name: "archive", Slug: "archive", Kind: database.StorageKindStore,
		Bucket: "acme-data", Prefix: "team/archive"}
	if err := srv.db.CreateStorageResource(archive, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("CreateStorageResource: %v", err)
	}
	return srv, app, res
}

func manifestMapping(t *testing.T, srv *Server, res *database.StorageResource, app *database.App, path, access, authority string) {
	t.Helper()
	m := &database.StorageMapping{ResourceID: res.ID, AppID: app.ID, Path: path, Access: access, Authority: authority}
	if err := srv.db.PutStorageMapping(m, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
	if _, err := srv.db.Conn().Exec(`UPDATE storage_mappings SET source='manifest' WHERE id=?`, m.ID); err != nil {
		t.Fatalf("marking manifest: %v", err)
	}
}

func deployKey(t *testing.T, srv *Server, id string, mayAttach bool) {
	t.Helper()
	if _, err := srv.db.Conn().Exec(`INSERT INTO deploy_keys (id, name, key_hash, scope, may_attach)
		VALUES (?, ?, ?, 'deploy', ?)`, id, id, "hash-"+id, mayAttach); err != nil {
		t.Fatalf("deploy key: %v", err)
	}
}

func plan(t *testing.T, srv *Server, app *database.App, dep builder.Deployer, entries ...builder.StorageEntry) *builder.StoragePlan {
	t.Helper()
	p, err := srv.PlanManifestStorage(builder.StoragePlanRequest{App: app, Entries: entries, Deployer: dep})
	if err != nil {
		t.Fatalf("PlanManifestStorage: %v", err)
	}
	return p
}

func codes(p *builder.StoragePlan) string {
	var out []string
	for _, pr := range p.Problems {
		out = append(out, pr.Code)
	}
	return strings.Join(out, ",")
}

func entry(name, access string) builder.StorageEntry {
	return builder.StorageEntry{Name: name, Access: access, Path: builder.DefaultStoragePath(name, "")}
}

var (
	asOwner    = builder.Deployer{Type: database.DeployerUser, ID: "owner"}
	asStranger = builder.Deployer{Type: database.DeployerUser, ID: "stranger"}
	asAdmin    = builder.Deployer{Type: database.DeployerUser, ID: "root"}
	asNobody   = builder.Deployer{}
)

// The two keys: the owner holds every entry, and the deployer is asked only
// about a change (PRD D2, D3). Each case is (deployer, owner's delegation,
// the deployer's, the table, the entry) → the list it lands in or the code.
func TestThePlanJudgesTheOwnerAlwaysAndTheDeployerOnChanges(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dep         builder.Deployer
		ownerLevel  string // "" = no delegation
		callerLevel string // the stranger's
		table       string // the manifest row's level, "" = none
		access      string
		want        string // keep, add, change, or a code
	}{
		{"the owner adds what they manage", asOwner, "full", "", "", "read", "add"},
		{"the owner adds above their ceiling", asOwner, "read", "", "", "full", builder.StorageLevelAboveCeiling},
		{"an editor with no right adds", asStranger, "full", "", "", "read", builder.StorageNotAttachableByYou},
		{"an editor with no right keeps", asStranger, "full", "", "read", "read", "keep"},
		{"an editor with no right narrows", asStranger, "full", "", "full", "read", "change"},
		{"an editor with no right widens", asStranger, "full", "", "read", "full", builder.StorageNotAttachableByYou},
		{"an editor who manages it adds for an owner who does", asStranger, "full", "read", "", "read", "add"},
		{"an editor adds above their own ceiling", asStranger, "full", "read", "", "full", builder.StorageLevelAboveCeiling},
		{"an editor who manages it, an owner who does not", asStranger, "", "full", "", "read", builder.StorageNotAttachableByOwner},
		{"neither manages it", asStranger, "", "", "", "read", builder.StorageUnknown},
		{"the owner lost the right to a kept entry", asOwner, "", "", "read", "read", builder.StorageUnknown},
		{"an administrator is not a pass", asAdmin, "", "", "", "read", builder.StorageNotAttachableByYou}, // refused, and told the ways through,
		{"no person keeps", asNobody, "full", "", "read", "read", "keep"},
		{"no person narrows", asNobody, "full", "", "full", "read", "change"},
		{"no person adds", asNobody, "full", "", "", "read", builder.StorageRequiresPerson},
		{"no person keeps what the owner lost", asNobody, "", "", "read", "read", builder.StorageNotAttachableByOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, app, res := planEnv(t)
			if tc.ownerLevel != "" {
				delegate(t, srv, res.ID, "owner", tc.ownerLevel)
			}
			if tc.callerLevel != "" {
				delegate(t, srv, res.ID, "stranger", tc.callerLevel)
			}
			if tc.table != "" {
				// Written through an administrator's stamp, as a raw prior
				// state; the plan's Keep compares the stamp too, so a kept
				// entry is one the owner's delegation already stamps.
				authority := database.AuthorityAdmin
				if tc.ownerLevel != "" {
					authority = database.AuthorityKey(database.StoreSubjectUser, "owner")
				}
				manifestMapping(t, srv, res, app, "/storage/reports", tc.table, authority)
			}
			p := plan(t, srv, app, tc.dep, entry("reports", tc.access))
			got := codes(p)
			switch {
			case len(p.Add) == 1:
				got = "add"
			case len(p.Keep) == 1:
				got = "keep"
			case len(p.Change) == 1:
				got = "change"
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q (%+v)", got, tc.want, p.Problems)
			}
			for _, list := range [][]builder.PlannedStorage{p.Add, p.Keep, p.Change} {
				for _, m := range list {
					if m.Authority != database.AuthorityKey(database.StoreSubjectUser, "owner") {
						t.Errorf("stamped %q, want the owner's delegation — never the deployer's, never admin", m.Authority)
					}
				}
			}
		})
	}
}

// The name of a resource the caller cannot attach gets the same sentence
// whether it exists or not: the toml cannot enumerate an install's storage.
func TestAnUnknownNameAndAnUnreachableOneReadTheSame(t *testing.T) {
	srv, app, _ := planEnv(t)
	missing := plan(t, srv, app, asStranger, entry("no-such-thing", "read"))
	hidden := plan(t, srv, app, asStranger, entry("reports", "read"))
	if codes(missing) != builder.StorageUnknown || codes(hidden) != builder.StorageUnknown {
		t.Fatalf("codes %q / %q, want storage_unknown twice", codes(missing), codes(hidden))
	}
	a := strings.ReplaceAll(missing.Problems[0].Message, "no-such-thing", "X")
	b := strings.ReplaceAll(hidden.Problems[0].Message, "reports", "X")
	if a != b {
		t.Errorf("the two refusals differ:\n%s\n%s", a, b)
	}
}

// A deploy key adds only when an administrator let it, and even then never in
// place of the owner (D19; PRD §7 test 5 — silent if wrong, because the
// attach succeeds).
func TestADeployKeyAddsOnlyWithMayAttachAndNeverForTheOwner(t *testing.T) {
	srv, app, res := planEnv(t)
	deployKey(t, srv, "dk-plain", false)
	deployKey(t, srv, "dk-attach", true)
	plain := builder.Deployer{Type: database.DeployerDeployKey, ID: "dk-plain"}
	attach := builder.Deployer{Type: database.DeployerDeployKey, ID: "dk-attach"}

	if p := plan(t, srv, app, attach, entry("reports", "read")); codes(p) != builder.StorageNotAttachableByOwner {
		t.Fatalf("may_attach with an owner who cannot = %q, want storage_not_attachable_by_owner", codes(p))
	}
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	p := plan(t, srv, app, plain, entry("reports", "read"))
	if codes(p) != builder.StorageRequiresPerson || !strings.Contains(p.Problems[0].Message, "deploy key") {
		t.Fatalf("a plain key adds = %q %+v, want storage_requires_person naming the key", codes(p), p.Problems)
	}
	if p := plan(t, srv, app, attach, entry("reports", "read")); len(p.Add) != 1 {
		t.Fatalf("may_attach with an owner who can = %+v, want one add", p)
	}
}

// A group-owned app attaches through the group's own row — a member's personal
// delegation is not the group's (D16).
func TestAGroupOwnedAppAttachesThroughTheGroupsOwnRow(t *testing.T) {
	srv, _, res := planEnv(t)
	seedUser(t, srv.db, "founder", database.SystemRolePublisher)
	app, g := seedGroupOwnedApp(t, srv, "team-app", "finance-apps", "stranger")
	app, _ = srv.db.GetApp(app.ID)

	delegate(t, srv, res.ID, "stranger", runtime.AccessFull)
	p := plan(t, srv, app, asStranger, entry("reports", "read"))
	if codes(p) != builder.StorageNotAttachableByOwner || !strings.Contains(p.Problems[0].Message, "group finance-apps") {
		t.Fatalf("a member's own right = %q %+v, want the owner refusal naming the group", codes(p), p.Problems)
	}

	if err := srv.db.PutStorageManager(&database.StorageManager{ResourceID: res.ID,
		SubjectType: database.StoreSubjectGroup, SubjectID: g.ID, MaxAccess: runtime.AccessFull},
		"root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageManager: %v", err)
	}
	p = plan(t, srv, app, asStranger, entry("reports", "read"))
	if len(p.Add) != 1 || p.Add[0].Authority != database.AuthorityKey(database.StoreSubjectGroup, g.ID) {
		t.Fatalf("with the group delegated = %+v, want an add stamped to the group", p)
	}
}

// The console's mappings are never adopted, never moved, and never removed by
// a deploy (D12).
func TestTheConsolesMappingsAreTheConsoles(t *testing.T) {
	srv, app, res := planEnv(t)
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	attached := &database.StorageMapping{ResourceID: res.ID, AppID: app.ID, Path: "/storage/reports", Access: "read"}
	if err := srv.db.PutStorageMapping(attached, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}

	same := plan(t, srv, app, asOwner, entry("reports", "read"))
	if len(same.Console) != 1 || same.Writes() {
		t.Fatalf("an identical console mapping = %+v, want it listed as the console's and nothing written", same)
	}
	if lines := strings.Join(same.Lines(), "\n"); !strings.Contains(lines, "kept: attached in the console") {
		t.Errorf("lines = %q, want the console mapping said to be kept", lines)
	}
	differs := plan(t, srv, app, asOwner, entry("reports", "full"))
	if codes(differs) != builder.StorageConflictsConsole {
		t.Fatalf("a differing console mapping = %q, want storage_conflicts_console_mapping", codes(differs))
	}
	if gone := plan(t, srv, app, asNobody); len(gone.Remove) != 0 {
		t.Errorf("no [[storage]] removed a console mapping: %+v", gone.Remove)
	}
}

// Removal needs nobody: a deploy with no person may take away what the toml no
// longer declares (D4).
func TestAnEntryNoLongerDeclaredIsRemoved(t *testing.T) {
	srv, app, res := planEnv(t)
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	manifestMapping(t, srv, res, app, "/storage/reports", "read", database.AuthorityKey(database.StoreSubjectUser, "owner"))
	p := plan(t, srv, app, asNobody)
	if len(p.Remove) != 1 || len(p.Problems) != 0 {
		t.Fatalf("plan = %+v, want one removal and no refusal", p)
	}
}

// A path is the app's, whichever half wrote it.
func TestAnEntryCannotTakeAConsoleMappingsPath(t *testing.T) {
	srv, app, res := planEnv(t)
	archive, _, _ := srv.db.StorageResourceByName("archive")
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	attached := &database.StorageMapping{ResourceID: archive.ID, AppID: app.ID, Path: "/data", Access: "read"}
	if err := srv.db.PutStorageMapping(attached, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
	p := plan(t, srv, app, asOwner, builder.StorageEntry{Name: "reports", Access: "read", Path: "/data"})
	if codes(p) != builder.StoragePathTaken {
		t.Fatalf("codes = %q, want storage_path_taken", codes(p))
	}
}

func mappingsOf(t *testing.T, srv *Server, app *database.App) map[string]database.StorageMapping {
	t.Helper()
	ms, err := srv.db.ListStorageMappingsForApp(app.ID)
	if err != nil {
		t.Fatalf("ListStorageMappingsForApp: %v", err)
	}
	out := map[string]database.StorageMapping{}
	for _, m := range ms {
		out[m.ResourceSlug] = m
	}
	return out
}

// Planning and preparing write nothing a restart reads: the table is the
// serving release's until the flip (PRD §7 test 1). Without this, a build that
// then fails would leave the old release gaining mounts at its next start.
func TestPlanningAndPreparingLeaveTheTableAlone(t *testing.T) {
	srv, app, res := planEnv(t)
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	before, _ := srv.db.AppMounts(app.ID)

	p := plan(t, srv, app, asOwner, entry("reports", "read"))
	if err := srv.PrepareStoragePlan(t.Context(), app, p); err != nil {
		t.Fatalf("PrepareStoragePlan: %v", err)
	}
	if len(p.Mounts) != 1 || p.Mounts[0].Name != "reports" || p.Mounts[0].Path != "/storage/reports" {
		t.Fatalf("planned mounts = %+v, want reports at /storage/reports", p.Mounts)
	}
	after, _ := srv.db.AppMounts(app.ID)
	if len(before) != 0 || len(after) != 0 {
		t.Fatalf("the table moved before the flip: %v → %v", before, after)
	}
}

// The flip writes the plan, stamped to the owner, as source manifest; the
// next deploy without the entry removes it, and the console's row is never
// touched (D10, D12).
func TestTheFlipWritesThePlanAndTheNextRemovesIt(t *testing.T) {
	srv, app, res := planEnv(t)
	archive, _, _ := srv.db.StorageResourceByName("archive")
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	console := &database.StorageMapping{ResourceID: archive.ID, AppID: app.ID, Path: "/data/archive", Access: "read"}
	if err := srv.db.PutStorageMapping(console, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}

	p := plan(t, srv, app, asOwner, entry("reports", "read"),
		builder.StorageEntry{Name: "archive", Access: "read", Path: "/data/archive"})
	if err := srv.ReconcileManifestStorage(app, p, asOwner); err != nil {
		t.Fatalf("ReconcileManifestStorage: %v", err)
	}
	got := mappingsOf(t, srv, app)
	if m := got["reports"]; m.Source != database.MappingSourceManifest ||
		m.Authority != database.AuthorityKey(database.StoreSubjectUser, "owner") {
		t.Fatalf("reports = %+v, want a manifest row stamped to the owner", m)
	}
	if m := got["archive"]; m.Source != database.MappingSourceConsole || m.Authority != database.AuthorityAdmin {
		t.Fatalf("archive = %+v, want the console's row untouched — never adopted", m)
	}

	// Widened by the next deploy, then removed by the one after.
	p = plan(t, srv, app, asOwner, builder.StorageEntry{Name: "reports", Access: "full", Path: "/data/reports"})
	if len(p.Change) != 1 {
		t.Fatalf("plan = %+v, want one change", p)
	}
	if err := srv.ReconcileManifestStorage(app, p, asOwner); err != nil {
		t.Fatalf("ReconcileManifestStorage: %v", err)
	}
	if m := mappingsOf(t, srv, app)["reports"]; m.Access != "full" || m.Path != "/data/reports" {
		t.Fatalf("reports after the change = %+v", m)
	}
	p = plan(t, srv, app, asNobody)
	if err := srv.ReconcileManifestStorage(app, p, asNobody); err != nil {
		t.Fatalf("ReconcileManifestStorage: %v", err)
	}
	got = mappingsOf(t, srv, app)
	if _, still := got["reports"]; still {
		t.Error("a manifest mapping outlived the entry")
	}
	if _, still := got["archive"]; !still {
		t.Error("removing [[storage]] removed a console mapping")
	}
}

// A revocation between the plan and the flip wins: the flip refuses and leaves
// no row stamped to the revoked delegation (PRD §7 test 3). Ordered by the
// sequence of calls — the plan, then the revoke, then the flip — not by a
// sleep.
func TestARevocationBetweenPlanAndFlipWins(t *testing.T) {
	srv, app, res := planEnv(t)
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	p := plan(t, srv, app, asOwner, entry("reports", "read"))
	if len(p.Add) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	if _, err := srv.db.DeleteStorageManagerAndMappings(res.ID, database.StoreSubjectUser, "owner",
		"root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	err := srv.ReconcileManifestStorage(app, p, asOwner)
	if !errors.Is(err, database.ErrStorageRevokedDuringDeploy) {
		t.Fatalf("flip = %v, want ErrStorageRevokedDuringDeploy", err)
	}
	if got := mappingsOf(t, srv, app); len(got) != 0 {
		t.Fatalf("the refused flip left %v", got)
	}

	// Lowered rather than revoked is the same race.
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	p = plan(t, srv, app, asOwner, entry("reports", "full"))
	delegate(t, srv, res.ID, "owner", runtime.AccessRead)
	if err := srv.ReconcileManifestStorage(app, p, asOwner); !errors.Is(err, database.ErrStorageRevokedDuringDeploy) {
		t.Fatalf("flip after a lowered ceiling = %v, want ErrStorageRevokedDuringDeploy", err)
	}
}

// A console mapping attached at a planned key since the plan, and differing,
// refuses the flip rather than being overwritten.
func TestAConsoleAttachSinceThePlanRefusesTheFlip(t *testing.T) {
	srv, app, res := planEnv(t)
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	p := plan(t, srv, app, asOwner, entry("reports", "read"))
	console := &database.StorageMapping{ResourceID: res.ID, AppID: app.ID, Path: "/elsewhere", Access: "full"}
	if err := srv.db.PutStorageMapping(console, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStorageMapping: %v", err)
	}
	if err := srv.ReconcileManifestStorage(app, p, asOwner); !errors.Is(err, database.ErrManifestConflictsConsole) {
		t.Fatalf("flip = %v, want ErrManifestConflictsConsole", err)
	}
	if m := mappingsOf(t, srv, app)["reports"]; m.Path != "/elsewhere" || m.Source != database.MappingSourceConsole {
		t.Fatalf("the console's row moved: %+v", m)
	}
}

// flipped writes a plan for the owner and returns the app reloaded.
func flipped(t *testing.T, srv *Server, app *database.App, entries ...builder.StorageEntry) {
	t.Helper()
	p := plan(t, srv, app, asOwner, entries...)
	if len(p.Problems) > 0 {
		t.Fatalf("plan refused: %+v", p.Problems)
	}
	if err := srv.ReconcileManifestStorage(app, p, asOwner); err != nil {
		t.Fatalf("ReconcileManifestStorage: %v", err)
	}
}

// A transfer moves a manifest mapping's stamp to the new owner, or refuses
// naming each resource; the bulk path may keep it as an administrator's
// instead (D11).
func TestATransferRestampsOrRefuses(t *testing.T) {
	srv, app, res := planEnv(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "heir", database.SystemRolePublisher)
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	flipped(t, srv, app, entry("reports", "full"))

	rec := storageRequest(t, srv, router, "POST", "/api/v1/apps/"+app.ID+"/transfer", "owner",
		`{"new_owner_id":"heir"}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "storage_not_attachable_by_new_owner") ||
		!strings.Contains(rec.Body.String(), "reports (full)") {
		t.Fatalf("transfer to an heir without the right = %d %s", rec.Code, rec.Body.String())
	}
	if a, _ := srv.db.GetApp(app.ID); a.OwnerID != "owner" {
		t.Fatalf("a refused transfer moved the app to %q", a.OwnerID)
	}

	delegate(t, srv, res.ID, "heir", runtime.AccessRead)
	if rec := storageRequest(t, srv, router, "POST", "/api/v1/apps/"+app.ID+"/transfer", "owner",
		`{"new_owner_id":"heir"}`); rec.Code != 409 {
		t.Fatalf("an heir below the mapped level = %d, want 409", rec.Code)
	}
	delegate(t, srv, res.ID, "heir", runtime.AccessFull)
	if rec := storageRequest(t, srv, router, "POST", "/api/v1/apps/"+app.ID+"/transfer", "owner",
		`{"new_owner_id":"heir"}`); rec.Code != 204 {
		t.Fatalf("transfer to an heir with the right = %d %s", rec.Code, rec.Body.String())
	}
	if m := mappingsOf(t, srv, app)["reports"]; m.Authority != database.AuthorityKey(database.StoreSubjectUser, "heir") {
		t.Fatalf("stamp after the transfer = %q, want the heir's", m.Authority)
	}
}

func TestTheBulkTransferMayKeepStorageAsAnAdministrators(t *testing.T) {
	srv, app, res := planEnv(t)
	router := srv.Router(http.NotFoundHandler())
	seedUser(t, srv.db, "heir", database.SystemRolePublisher)
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	flipped(t, srv, app, entry("reports", "read"))

	path := "/api/v1/users/owner/transfer-apps"
	if rec := storageRequest(t, srv, router, "POST", path, "root", `{"to_id":"heir"}`); rec.Code != 409 ||
		!strings.Contains(rec.Body.String(), "keep_as_admin") {
		t.Fatalf("bulk transfer without the option = %d %s", rec.Code, rec.Body.String())
	}
	if rec := storageRequest(t, srv, router, "POST", path, "root", `{"to_id":"heir","storage":"keep_as_admin"}`); rec.Code != 200 {
		t.Fatalf("bulk transfer keeping storage = %d %s", rec.Code, rec.Body.String())
	}
	if m := mappingsOf(t, srv, app)["reports"]; m.Authority != database.AuthorityAdmin {
		t.Fatalf("stamp = %q, want admin", m.Authority)
	}
}

// A mapping toft.toml declares is the deploy's: the console's PUT on it is a
// 409, a delegate cannot detach it, and an administrator can — told the next
// deploy brings it back (D13).
func TestAManifestMappingIsReadOnlyInTheConsole(t *testing.T) {
	srv, app, res := planEnv(t)
	router := srv.Router(http.NotFoundHandler())
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	flipped(t, srv, app, entry("reports", "read"))

	put := `{"path":"/storage/reports","access":"full"}`
	if rec := storageRequest(t, srv, router, "PUT", "/api/v1/apps/"+app.ID+"/storage/"+res.ID, "owner", put); rec.Code != 409 ||
		!strings.Contains(rec.Body.String(), "declared_in_manifest") {
		t.Fatalf("delegate PUT = %d %s", rec.Code, rec.Body.String())
	}
	if rec := storageRequest(t, srv, router, "PUT", "/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root", put); rec.Code != 409 {
		t.Fatalf("admin PUT = %d %s", rec.Code, rec.Body.String())
	}
	if rec := storageRequest(t, srv, router, "DELETE", "/api/v1/apps/"+app.ID+"/storage/"+res.ID, "owner", ""); rec.Code != 409 {
		t.Fatalf("delegate DELETE = %d %s", rec.Code, rec.Body.String())
	}
	rec := storageRequest(t, srv, router, "DELETE", "/api/v1/admin/storage/"+res.ID+"/mappings/"+app.ID, "root", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "next deploy") {
		t.Fatalf("admin DELETE = %d %s", rec.Code, rec.Body.String())
	}
}

// A store grant reaches a resource from outside and is not an attach right: a
// person holding one is told so, under storage_not_attachable_by_you, rather
// than being answered as if the name did not exist (BB-004). Somebody with no
// grant at all still gets storage_unknown, above.
func TestAStoreGrantIsToldItIsNotAnAttachRight(t *testing.T) {
	srv, app, res := planEnv(t)
	if err := srv.db.PutStoreGrant(&database.StoreGrant{ResourceID: res.ID, SubjectType: database.StoreSubjectUser,
		SubjectID: "stranger", Access: "read"}, "root", "req", "10.0.0.1", "test"); err != nil {
		t.Fatalf("PutStoreGrant: %v", err)
	}
	p := plan(t, srv, app, asStranger, entry("reports", "read"))
	if codes(p) != builder.StorageNotAttachableByYou {
		t.Fatalf("code %q, want storage_not_attachable_by_you (%+v)", codes(p), p.Problems)
	}
	if msg := p.Problems[0].Message; !strings.Contains(msg, "store grant") || !strings.Contains(msg, "administrator") {
		t.Errorf("the refusal must say a grant is not an attach right: %s", msg)
	}
	if other := plan(t, srv, app, asStranger, entry("archive", "read")); codes(other) != builder.StorageUnknown {
		t.Errorf("a resource with no grant must stay storage_unknown, got %q", codes(other))
	}
}

// An administrator with no delegation is refused — being one is not an attach
// right — but told the resource exists and the two ways through, rather than
// storage_unknown (porting feedback 2026-11-03).
func TestAnAdministratorIsToldTheWaysToAttach(t *testing.T) {
	srv, app, _ := planEnv(t)
	p := plan(t, srv, app, asAdmin, entry("archive", "read"))
	if codes(p) != builder.StorageNotAttachableByYou {
		t.Fatalf("code %q, want storage_not_attachable_by_you (%+v)", codes(p), p.Problems)
	}
	if msg := p.Problems[0].Message; !strings.Contains(msg, "exists") || !strings.Contains(msg, "/managers") ||
		!strings.Contains(msg, "/mappings/") {
		t.Errorf("the refusal must name both ways through: %s", msg)
	}
	// The hint is an administrator's routes, not "ask the administrator"
	// (BB-032, fourth 2026-11-03 black-box list).
	if h := p.Problems[0].Hint; strings.Contains(h, "ask the resource's administrator") || !strings.Contains(h, "/mappings/") {
		t.Errorf("an administrator's hint = %q", h)
	}
	// The app is known, so its id is in the command, not <app-id> (BB-008).
	if h := p.Problems[0].Hint; strings.Contains(h, "<app-id>") || !strings.Contains(h, "/mappings/"+app.ID+" ") {
		t.Errorf("the hint must name this app's id: %q", h)
	}
}

// Two entries may swap paths, and one may take the path another is leaving, in
// one deploy: the flip parks the moving rows before it writes, so UNIQUE
// (app_id, path) is never met mid-transaction (code review 2026-11-11, B-2).
func TestEntriesMaySwapPathsInOneDeploy(t *testing.T) {
	srv, app, res := planEnv(t)
	archive, _, _ := srv.db.StorageResourceByName("archive")
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	delegate(t, srv, archive.ID, "owner", runtime.AccessFull)

	p := plan(t, srv, app, asOwner,
		builder.StorageEntry{Name: "reports", Access: "read", Path: "/data"},
		builder.StorageEntry{Name: "archive", Access: "read", Path: "/archive"})
	if err := srv.ReconcileManifestStorage(app, p, asOwner); err != nil {
		t.Fatalf("first flip: %v", err)
	}
	// Swap.
	p = plan(t, srv, app, asOwner,
		builder.StorageEntry{Name: "reports", Access: "read", Path: "/archive"},
		builder.StorageEntry{Name: "archive", Access: "read", Path: "/data"})
	if len(p.Problems) != 0 {
		t.Fatalf("plan refused a swap: %+v", p.Problems)
	}
	if err := srv.ReconcileManifestStorage(app, p, asOwner); err != nil {
		t.Fatalf("swap flip: %v", err)
	}
	got := mappingsOf(t, srv, app)
	if got["reports"].Path != "/archive" || got["archive"].Path != "/data" {
		t.Fatalf("after the swap: reports %s, archive %s", got["reports"].Path, got["archive"].Path)
	}
	// One moves away, a new one takes its path.
	p = plan(t, srv, app, asOwner,
		builder.StorageEntry{Name: "reports", Access: "read", Path: "/elsewhere"},
		builder.StorageEntry{Name: "archive", Access: "read", Path: "/archive"})
	if err := srv.ReconcileManifestStorage(app, p, asOwner); err != nil {
		t.Fatalf("take-the-leaving-path flip: %v", err)
	}
	got = mappingsOf(t, srv, app)
	if got["reports"].Path != "/elsewhere" || got["archive"].Path != "/archive" {
		t.Fatalf("after the move: reports %s, archive %s", got["reports"].Path, got["archive"].Path)
	}
}

// An Add planned under the owner's delegation is written only if the owner
// holds it at the flip: with the app transferred to someone without one, the
// plan's stamped row still existed and the mapping was written under a
// delegation the new owner did not hold (code review 2026-11-11, B-4).
func TestAnAddIsRefusedWhenTheOwnerNoLongerHoldsTheDelegation(t *testing.T) {
	srv, app, res := planEnv(t)
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	p := plan(t, srv, app, asOwner, entry("reports", "read"))
	if len(p.Add) != 1 || len(p.Problems) != 0 {
		t.Fatalf("plan = %+v, want one add", p)
	}
	// Transferred during the build to an owner with no delegation; the
	// delegation row itself stays.
	if _, err := srv.db.Conn().Exec(`UPDATE apps SET owner_user_id = 'stranger' WHERE id = ?`, app.ID); err != nil {
		t.Fatal(err)
	}
	err := srv.ReconcileManifestStorage(app, p, asOwner)
	if !errors.Is(err, database.ErrStorageRevokedDuringDeploy) {
		t.Fatalf("flip after the transfer = %v, want the revoked refusal", err)
	}
	if _, has := mappingsOf(t, srv, app)["reports"]; has {
		t.Error("the mapping was written under a delegation the owner does not hold")
	}
}

// AP-3: a sweep never releases a binding while a deploy is in flight, because
// a building release binds at plan time and records its mapping only at the
// flip — a count of committed rows cannot see it.
func TestASweepDuringADeployReleasesNothing(t *testing.T) {
	srv, app, res := planEnv(t)
	fake := srv.storage.(*fakeProvisioner)
	removed := []database.StorageMapping{{ResourceID: res.ID, AppID: app.ID, Access: "read", Folder: "f"}}

	d := &database.Deployment{AppID: app.ID, Status: database.DeployStatusBuilding}
	if err := srv.db.CreateDeployment(d, nil); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	srv.sweepBindings(t.Context(), res.ID, removed)
	if len(fake.removed) != 0 {
		t.Fatalf("released %+v while a deploy was building", fake.removed)
	}

	if _, err := srv.db.Conn().Exec(`UPDATE deployments SET status = ? WHERE id = ?`, database.DeployStatusSuccess, d.ID); err != nil {
		t.Fatal(err)
	}
	srv.sweepBindings(t.Context(), res.ID, removed)
	if len(fake.removed) != 1 || fake.removed[0].Folder != "f" {
		t.Fatalf("with nothing in flight the sweep released %+v, want folder f", fake.removed)
	}
}

// AP-3: a plan whose entries are all kept still ensures their bindings, so a
// redeploy repairs one released from under a mapping.
func TestAPlanOfOnlyKeptEntriesStillEnsuresTheirBindings(t *testing.T) {
	srv, app, res := planEnv(t)
	fake := srv.storage.(*fakeProvisioner)
	delegate(t, srv, res.ID, "owner", runtime.AccessFull)
	flipped(t, srv, app, entry("reports", "read"))

	p := plan(t, srv, app, asOwner, entry("reports", "read"))
	if len(p.Keep) != 1 || len(p.Add)+len(p.Change) != 0 {
		t.Fatalf("plan = %+v, want one kept entry", p)
	}
	fake.ensured = nil
	if err := srv.PrepareStoragePlan(t.Context(), app, p); err != nil {
		t.Fatalf("PrepareStoragePlan: %v", err)
	}
	if len(fake.ensured) != 1 || fake.ensured[0].Slug != "reports" {
		t.Fatalf("ensured = %+v, want the kept entry's binding", fake.ensured)
	}
}
