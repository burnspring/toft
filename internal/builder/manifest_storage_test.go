package builder

import (
	"context"
	"errors"
	"strings"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
	"toft/internal/runtime"
)

// The defaults, and the shape of an entry as the planner receives it.
func TestStorageEntriesTakeTheirDefaults(t *testing.T) {
	dir := writeManifest(t, `
[[storage]]
name = "sales-data"

[[storage]]
name   = "archive"
folder = "2026/"
access = "full"

[[storage]]
name = "models"
path = "/data/models"
`)
	d, warnings, err := ParseManifestAll(dir)
	if err != nil {
		t.Fatalf("ParseManifestAll: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if !d.StorageDeclared {
		t.Fatal("StorageDeclared is false for a file declaring [[storage]]")
	}
	// An omitted access stays empty: the planner picks it (validateAttach) —
	// the root's level on a shared install, read elsewhere (docs/bugs
	// 2026-11-03 #1).
	want := []StorageEntry{
		{Name: "sales-data", Access: "", Path: "/storage/sales-data"},
		{Name: "archive", Access: "full", Folder: "2026", Path: "/storage/archive/2026"},
		{Name: "models", Access: "", Path: "/data/models"},
	}
	if len(d.Storage) != len(want) {
		t.Fatalf("entries = %+v, want %+v", d.Storage, want)
	}
	for i := range want {
		if d.Storage[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, d.Storage[i], want[i])
		}
	}
}

// No table and an absent table are the same thing, and both say so: removing
// [[storage]] is how every manifest mapping goes.
func TestNoStorageTableIsNotDeclared(t *testing.T) {
	d, _, err := ParseManifestAll(writeManifest(t, "[jobs.a]\ncommand = \"x\"\n"))
	if err != nil {
		t.Fatalf("ParseManifestAll: %v", err)
	}
	if d.StorageDeclared || d.Storage != nil {
		t.Errorf("declared = %v, entries = %v, want neither", d.StorageDeclared, d.Storage)
	}
}

// Each refusal names the entry and the key, in the manifest's own vocabulary,
// and carries a code, as every other manifest refusal does (drills 2026-11-02).
func TestStorageEntriesAreRefusedByField(t *testing.T) {
	for _, tc := range []struct {
		name, body, field, code string
	}{
		{"no name", "[[storage]]\naccess = \"read\"\n", "storage[0].name", StorageEntryInvalid},
		{"a name that is not one", "[[storage]]\nname = \"Sales Data\"\n", "storage[0].name", StorageEntryInvalid},
		{"a level on no ladder", "[[storage]]\nname = \"x-data\"\naccess = \"rw\"\n", "storage[0].access", StorageEntryInvalid},
		{"a relative path", "[[storage]]\nname = \"x-data\"\npath = \"data\"\n", "storage[0].path", StorageEntryInvalid},
		{"a path the runner owns", "[[storage]]\nname = \"x-data\"\npath = \"/workspace/x\"\n", "storage[0].path", StorageEntryInvalid},
		{"a traversal through the default path", "[[storage]]\nname = \"x-data\"\nfolder = \"../etc\"\n", "storage[0].path", StorageEntryInvalid},
		{"the same name twice", "[[storage]]\nname = \"x-data\"\n[[storage]]\nname = \"x-data\"\npath = \"/b\"\n",
			"storage[1].name", StorageNameAmbiguous},
		{"two entries at one path", "[[storage]]\nname = \"x-data\"\npath = \"/a\"\n[[storage]]\nname = \"y-data\"\npath = \"/a\"\n",
			"storage[1].path", StoragePathTaken},
		// BB-048, fifth 2026-11-03 black-box list: whole and by folder is a
		// mount inside a mount, which no start can make.
		{"a resource whole and by folder", "[[storage]]\nname = \"x-data\"\n[[storage]]\nname = \"x-data\"\nfolder = \"f\"\n",
			"storage[1].path", StoragePathTaken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseManifestAll(writeManifest(t, tc.body))
			var me ManifestError
			if !errors.As(err, &me) {
				t.Fatalf("err = %v, want a ManifestError", err)
			}
			if me.Field != tc.field || me.Code != tc.code {
				t.Errorf("field %q code %q, want %q %q (%v)", me.Field, me.Code, tc.field, tc.code, err)
			}
			// And toft check carries it as the problem's field (D-22).
			ps := RunPreflight(writeManifest(t, tc.body), "shared", nil, config.Envelope{}).Problems
			found := false
			for _, p := range ps {
				found = found || (p.Field == tc.field && p.Code == tc.code)
			}
			if !found {
				t.Errorf("check problems = %+v, want one with field %q", ps, tc.field)
			}
		})
	}
}

// A key nobody reads inside an entry is a warning, like every table's: an
// `acess = "full"` is a mount that silently stayed read-only.
func TestAnUnreadKeyInAStorageEntryIsAWarning(t *testing.T) {
	_, warnings, err := ParseManifestAll(writeManifest(t, "[[storage]]\nname = \"x-data\"\nacess = \"full\"\n"))
	if err != nil {
		t.Fatalf("ParseManifestAll: %v", err)
	}
	if len(warnings) != 1 || warnings[0] != "storage.acess" {
		t.Errorf("warnings = %v, want [storage.acess]", warnings)
	}
}

// Without a planner the table is refused rather than ignored: storage this
// install cannot judge is not storage it attaches.
func TestStorageWithoutAPlannerIsRefused(t *testing.T) {
	var b Builder
	_, err := b.planStorage(nil, nil, ManifestDecl{StorageDeclared: true,
		Storage: []StorageEntry{{Name: "x-data"}}}, func(string) {})
	var refused *storageRefusal
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "does not offer storage") {
		t.Fatalf("err = %v, want a storage refusal", err)
	}
	if plan, err := b.planStorage(nil, nil, ManifestDecl{}, func(string) {}); plan != nil || err != nil {
		t.Errorf("no table and no planner = %v, %v; want nothing at all", plan, err)
	}
}

type fakePlanner struct {
	plan *StoragePlan
	got  StoragePlanRequest
}

func (f *fakePlanner) PlanManifestStorage(req StoragePlanRequest) (*StoragePlan, error) {
	f.got = req
	return f.plan, nil
}

func (f *fakePlanner) PrepareStoragePlan(context.Context, *database.App, *StoragePlan) error {
	return nil
}

func (f *fakePlanner) ReconcileManifestStorage(*database.App, *StoragePlan, Deployer) error {
	return nil
}

// The deploy hands the planner the deployment's principal, refuses with every
// problem at once, and logs a plan it may carry out.
func TestPlanStorageRefusesWithEveryProblemAndLogsThePlan(t *testing.T) {
	fp := &fakePlanner{plan: &StoragePlan{Problems: []StorageProblem{
		{Code: StorageUnknown, Message: "first"}, {Code: StorageRequiresPerson, Message: "second"}}}}
	b := Builder{storagePlanner: fp}
	deploy := &database.Deployment{DeployedByType: database.DeployerUser, DeployedByID: "u1"}
	_, err := b.planStorage(&database.App{}, deploy, ManifestDecl{StorageDeclared: true}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
		t.Fatalf("err = %v, want both problems", err)
	}
	if fp.got.Deployer != (Deployer{Type: database.DeployerUser, ID: "u1"}) {
		t.Errorf("deployer = %+v, want the deployment's", fp.got.Deployer)
	}

	fp.plan = &StoragePlan{Add: []PlannedStorage{{Name: "reports", Access: "read", Path: "/storage/reports"}}}
	var log []string
	if _, err := b.planStorage(&database.App{}, deploy, ManifestDecl{StorageDeclared: true},
		func(l string) { log = append(log, l) }); err != nil {
		t.Fatalf("planStorage: %v", err)
	}
	if got := strings.Join(log, "\n"); !strings.Contains(got, "+ reports, read at /storage/reports") {
		t.Errorf("log = %q, want the add", got)
	}
}

// The flip compares the mounts the workload was started with against the
// table as it is then: a row revoked during the build is named, an attach made
// since the start is not (code review 2026-11-11, B-1).
func TestMountsWithoutRowsNamesWhatWasRevokedDuringTheBuild(t *testing.T) {
	started := []runtime.Mount{
		{Name: "reports", Path: "/data/reports", Access: "read"},
		{Name: "archive", Folder: "2026", Path: "/archive", Access: "full"},
	}
	recorded := []runtime.Mount{
		{Name: "reports", Path: "/data/reports", Access: "read"},
		{Name: "scratch", Path: "/scratch", Access: "full"},
	}
	if got := mountsWithoutRows(started, recorded); len(got) != 1 || got[0] != "archive/2026" {
		t.Errorf("gone = %v, want [archive/2026]", got)
	}
	if got := mountsWithoutRows(started, started); len(got) != 0 {
		t.Errorf("unchanged table: gone = %v", got)
	}
	// Lowered access is a difference too: the workload holds more than the row says.
	recorded[0].Access = "none"
	if got := mountsWithoutRows(started[:1], recorded); len(got) != 1 {
		t.Errorf("lowered access: gone = %v", got)
	}
}

// Asked before the flip's reconcile, which commits: a mount the plan adds or
// changes has no row yet and is not a gap, but a console mount whose row went
// during the build still is (security review 2026-11-14, PB-4).
func TestMountsWithoutRowsUnlessPlannedIgnoresWhatThePlanWrites(t *testing.T) {
	started := []runtime.Mount{
		{Name: "reports", Path: "/data/reports", Access: "read"},        // console, row gone
		{Name: "archive", Folder: "2026", Path: "/new", Access: "full"}, // plan change
		{Name: "fresh", Path: "/fresh", Access: "read"},                 // plan add
		{Name: "kept", Path: "/kept", Access: "read"},                   // plan keep, row there
	}
	recorded := []runtime.Mount{
		{Name: "archive", Folder: "2026", Path: "/old", Access: "read"},
		{Name: "kept", Path: "/kept", Access: "read"},
	}
	plan := &StoragePlan{
		Add:    []PlannedStorage{{Name: "fresh", Path: "/fresh", Access: "read"}},
		Change: []PlannedStorage{{Name: "archive", Folder: "2026", Path: "/new", Access: "full", WasPath: "/old", WasAccess: "read"}},
		Keep:   []PlannedStorage{{Name: "kept", Path: "/kept", Access: "read"}},
	}
	if got := mountsWithoutRowsUnlessPlanned(started, recorded, plan); len(got) != 1 || got[0] != "reports" {
		t.Errorf("gone = %v, want [reports]", got)
	}
	recorded = append(recorded, runtime.Mount{Name: "reports", Path: "/data/reports", Access: "read"})
	if got := mountsWithoutRowsUnlessPlanned(started, recorded, plan); len(got) != 0 {
		t.Errorf("every unplanned mount has its row: gone = %v", got)
	}
}
