package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/runtime"
)

func listDrives(t *testing.T, srv *Server, router http.Handler) []storageDrive {
	t.Helper()
	rec := storageRequest(t, srv, router, http.MethodGet, "/api/v1/admin/storage", "root", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Drives []storageDrive `json:"drives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Drives
}

func driveByKey(drives []storageDrive, key string) *storageDrive {
	for i := range drives {
		if drives[i].Key == key {
			return &drives[i]
		}
	}
	return nil
}

// A declared root is a drive; a root name resources still record that nothing
// declares is listed too, undeclared, with how many resources are stranded on it.
func TestDrivesListDeclaredAndStrandedRoots(t *testing.T) {
	srv, router, _ := storageTestEnv(t, runtime.NewSharedStorage(
		[]runtime.StorageRoot{{Name: "oms", Path: t.TempDir()}, {Name: "ord", Path: t.TempDir(), ReadOnly: true}},
		t.TempDir(), ""))
	for slug, root := range map[string]string{"sales-data": "oms", "old-share": "efs", "old-cache": "efs"} {
		res := &database.StorageResource{Slug: slug, Kind: database.StorageKindVolume,
			Backing: database.StorageBackingHost, Root: root}
		if err := srv.db.CreateStorageResource(res, "root", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	drives := listDrives(t, srv, router)
	oms, ord, efs := driveByKey(drives, "root:oms"), driveByKey(drives, "root:ord"), driveByKey(drives, "root:efs")
	switch {
	case oms == nil || !oms.Declared || oms.ResourceCount != 1 || oms.Label != "oms":
		t.Errorf("oms = %+v, want declared, one resource, labelled by its name", oms)
	case ord == nil || !ord.ReadOnly || ord.ResourceCount != 0:
		t.Errorf("ord = %+v, want declared read-only and empty", ord)
	case efs == nil || efs.Declared || efs.ResourceCount != 2:
		t.Errorf("efs = %+v, want undeclared with two stranded resources", efs)
	}
}

func TestADriveLabelIsSetClearedAndKeptUnique(t *testing.T) {
	srv, router, _ := storageTestEnv(t, runtime.NewSharedStorage(
		[]runtime.StorageRoot{{Name: "oms", Path: t.TempDir()}, {Name: "ord", Path: t.TempDir()}},
		t.TempDir(), ""))
	patch := func(key, body string) *http.Response {
		rec := storageRequest(t, srv, router, http.MethodPatch, "/api/v1/admin/storage/drives/"+key, "root", body)
		return rec.Result()
	}

	if got := patch("root:oms", `{"label":"Order management","description":"Backed up nightly"}`); got.StatusCode != http.StatusOK {
		t.Fatalf("label = %d", got.StatusCode)
	}
	d := driveByKey(listDrives(t, srv, router), "root:oms")
	if d.Label != "Order management" || d.Description != "Backed up nightly" {
		t.Errorf("after labelling: %+v", d)
	}

	// Unique against other labels and against other drives' names, because one
	// select tells them apart by what it prints.
	for _, body := range []string{`{"label":"order MANAGEMENT"}`, `{"label":"oms"}`} {
		if got := patch("root:ord", body); got.StatusCode != http.StatusConflict {
			t.Errorf("ord %s = %d, want 409", body, got.StatusCode)
		}
	}
	for _, body := range []string{`{"label":"two\nlines"}`, `{"label":"` + strings.Repeat("x", 61) + `"}`} {
		if got := patch("root:ord", body); got.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("ord %q = %d, want 422", body, got.StatusCode)
		}
	}
	if got := patch("root:nowhere", `{"label":"x"}`); got.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown drive = %d, want 404", got.StatusCode)
	}

	// The SPA encodes the key; `root%3Aoms` is the same drive.
	if got := patch("root%3Aoms", `{"label":"Order management","description":"Backed up nightly"}`); got.StatusCode != http.StatusOK {
		t.Errorf("an encoded key = %d, want 200", got.StatusCode)
	}

	// An empty label clears it back to the drive's name.
	if got := patch("root:oms", `{"label":""}`); got.StatusCode != http.StatusOK {
		t.Fatalf("clear = %d", got.StatusCode)
	}
	if d := driveByKey(listDrives(t, srv, router), "root:oms"); d.Label != "oms" || d.Description != "" {
		t.Errorf("after clearing: %+v", d)
	}
}

// A cluster's declared classes are drives, and a volume naming no class counts
// against its backing's default.
func TestClassDrivesCountVolumesOnTheirDefault(t *testing.T) {
	prov := &classProvisioner{classes: []runtime.DeclaredClass{
		{Name: "efs-toft", Backing: runtime.BackingEFS, Default: true},
	}}
	srv, router, _ := storageTestEnv(t, prov)
	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"sales-data","kind":"volume","backing":"efs"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	d := driveByKey(listDrives(t, srv, router), "class:efs-toft")
	if d == nil || !d.Declared || d.ResourceCount != 1 || d.Backing != runtime.BackingEFS {
		t.Errorf("efs-toft = %+v, want declared with the one volume", d)
	}
}

// A create naming no class records the class it resolves to, so a later change
// to the defaults moves no existing resource; several non-default classes for
// the backing is a choice the create asks for.
func TestACreateRecordsTheClassItResolves(t *testing.T) {
	prov := &classProvisioner{classes: []runtime.DeclaredClass{
		{Name: "efs-toft", Backing: runtime.BackingEFS, Default: true},
		{Name: "a", Backing: runtime.BackingAzureFiles},
		{Name: "b", Backing: runtime.BackingAzureFiles},
	}}
	srv, router, _ := storageTestEnv(t, prov)

	rec := storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"sales-data","kind":"volume","backing":"efs"}`)
	var res database.StorageResource
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &res) != nil || res.StorageClass != "efs-toft" {
		t.Errorf("create = %d %s, want the default class recorded", rec.Code, rec.Body.String())
	}
	rec = storageRequest(t, srv, router, http.MethodPost, "/api/v1/admin/storage", "root",
		`{"name":"azure-data","kind":"volume","backing":"azurefiles"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "choose one") {
		t.Errorf("an ambiguous backing = %d %s, want 422 asking for a drive", rec.Code, rec.Body.String())
	}
}

// Volumes made before the class was recorded get it once, at startup.
func TestRecordStorageClassesWritesOnceAndAudits(t *testing.T) {
	srv, _, _ := storageTestEnv(t, &fakeProvisioner{})
	old := &database.StorageResource{Slug: "old-share", Kind: database.StorageKindVolume, Backing: database.StorageBackingEFS}
	if err := srv.db.CreateStorageResource(old, "root", "", "", ""); err != nil {
		t.Fatal(err)
	}
	resolve := func(string) string { return "efs-toft" }
	if n, err := srv.db.RecordStorageClasses(resolve); err != nil || n != 1 {
		t.Fatalf("first run wrote %d (%v), want 1", n, err)
	}
	if n, _ := srv.db.RecordStorageClasses(resolve); n != 0 {
		t.Errorf("second run wrote %d, want 0", n)
	}
	got, _ := srv.db.GetStorageResource(old.ID)
	if got.StorageClass != "efs-toft" {
		t.Errorf("class = %q", got.StorageClass)
	}
}

// repointEnv is a host install that renamed its root: resources recorded on
// `efs`, which is gone, and `oms` declared at the directory that holds them.
func repointEnv(t *testing.T, made ...string) (*Server, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	srv, router, _ := storageTestEnv(t, runtime.NewSharedStorage(
		[]runtime.StorageRoot{{Name: "oms", Path: dir}, {Name: "ro", Path: t.TempDir(), ReadOnly: true}}, t.TempDir(), ""))
	for _, slug := range []string{"sales-data", "old-cache", "team-share"} {
		res := &database.StorageResource{Slug: slug, Kind: database.StorageKindVolume,
			Backing: database.StorageBackingHost, Root: "efs"}
		if err := srv.db.CreateStorageResource(res, "root", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, slug := range made {
		if err := os.MkdirAll(filepath.Join(dir, slug), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return srv, router, dir
}

func rootsOf(t *testing.T, srv *Server) map[string]string {
	t.Helper()
	all, err := srv.db.ListStorageResources()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range all {
		out[r.Slug] = r.Root
	}
	return out
}

// The silent failure this guards: a re-point that moved the resources whose
// directories it found and left the rest, splitting one drive's resources
// across two names with nothing failing.
func TestARepointWithOneMissingDirectoryMovesNothing(t *testing.T) {
	srv, router, _ := repointEnv(t, "sales-data", "old-cache")
	rec := storageRequest(t, srv, router, http.MethodPost,
		"/api/v1/admin/storage/drives/root:efs/repoint", "root", `{"to":"oms"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "team-share") {
		t.Fatalf("repoint = %d %s, want 409 naming the missing directory", rec.Code, rec.Body.String())
	}
	for slug, root := range rootsOf(t, srv) {
		if root != "efs" {
			t.Errorf("%s moved to %q on a refused re-point", slug, root)
		}
	}
}

func TestARepointMovesEveryResourceAndCarriesTheLabel(t *testing.T) {
	srv, router, dir := repointEnv(t, "sales-data", "old-cache", "team-share")
	if rec := storageRequest(t, srv, router, http.MethodPatch, "/api/v1/admin/storage/drives/root:efs",
		"root", `{"label":"Order management"}`); rec.Code != http.StatusOK {
		t.Fatalf("label = %d %s", rec.Code, rec.Body.String())
	}

	// A dry run proves and writes nothing.
	rec := storageRequest(t, srv, router, http.MethodPost,
		"/api/v1/admin/storage/drives/root:efs/repoint", "root", `{"to":"root:oms","dry_run":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), filepath.Join(dir, "sales-data")) ||
		strings.Contains(rec.Body.String(), `"moved":true`) {
		t.Fatalf("dry run = %d %s", rec.Code, rec.Body.String())
	}
	if rootsOf(t, srv)["sales-data"] != "efs" {
		t.Fatal("a dry run moved a resource")
	}

	rec = storageRequest(t, srv, router, http.MethodPost,
		"/api/v1/admin/storage/drives/root:efs/repoint", "root", `{"to":"oms"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"moved":true`) {
		t.Fatalf("repoint = %d %s", rec.Code, rec.Body.String())
	}
	for slug, root := range rootsOf(t, srv) {
		if root != "oms" {
			t.Errorf("%s is on %q, want oms", slug, root)
		}
	}
	if d := driveByKey(listDrives(t, srv, router), "root:oms"); d == nil || d.Label != "Order management" || d.ResourceCount != 3 {
		t.Errorf("oms after the re-point = %+v, want the carried label and three resources", d)
	}
	if driveByKey(listDrives(t, srv, router), "root:efs") != nil {
		t.Error("efs is still listed with nothing on it")
	}
}

func TestARepointRefusesWhatIsNotStranded(t *testing.T) {
	srv, router, _ := repointEnv(t, "sales-data", "old-cache", "team-share")
	for _, tc := range []struct {
		path, body string
		code       int
	}{
		{"root:oms", `{"to":"efs"}`, http.StatusConflict},                // oms is declared
		{"root:efs", `{"to":"nowhere"}`, http.StatusUnprocessableEntity}, // not a declared drive
		{"root:missing", `{"to":"oms"}`, http.StatusNotFound},
	} {
		rec := storageRequest(t, srv, router, http.MethodPost,
			"/api/v1/admin/storage/drives/"+tc.path+"/repoint", "root", tc.body)
		if rec.Code != tc.code {
			t.Errorf("%s %s = %d, want %d: %s", tc.path, tc.body, rec.Code, tc.code, rec.Body.String())
		}
	}
}

// In shared mode a level is which mount a resource is under, so a resource an
// app writes to cannot be re-pointed onto a `:ro` root, even where its
// directory is there.
func TestARepointRefusesAWritableAttachmentOntoAReadOnlyDrive(t *testing.T) {
	roDir := t.TempDir()
	srv, router, app := storageTestEnv(t, runtime.NewSharedStorage(
		[]runtime.StorageRoot{{Name: "ro", Path: roDir, ReadOnly: true}}, t.TempDir(), ""))
	res := &database.StorageResource{Slug: "sales-data", Kind: database.StorageKindVolume,
		Backing: database.StorageBackingHost, Root: "efs"}
	if err := srv.db.CreateStorageResource(res, "root", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := srv.db.PutStorageMapping(&database.StorageMapping{
		ResourceID: res.ID, AppID: app.ID, Path: "/data/sales", Access: runtime.AccessWrite,
	}, database.BuiltinAdminID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(roDir, "sales-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := storageRequest(t, srv, router, http.MethodPost,
		"/api/v1/admin/storage/drives/root:efs/repoint", "root", `{"to":"ro"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "read-only") {
		t.Errorf("repoint = %d %s, want 409 naming the read-only drive", rec.Code, rec.Body.String())
	}
}

// proverProvisioner is a cluster whose classes and bound volumes the test
// states: bound maps a resource slug to the file systems its claims are on.
type proverProvisioner struct {
	classProvisioner
	classFS map[string]string
	bound   map[string][]string
}

func (p *proverProvisioner) ClassFileSystem(_ context.Context, class string) (string, string, error) {
	fs, ok := p.classFS[class]
	if !ok {
		return "", "", errors.New("not found")
	}
	return "efs.csi.aws.com", fs, nil
}

func (p *proverProvisioner) BoundFileSystems(_ context.Context, res runtime.StorageResource, _ []string) ([]string, error) {
	return p.bound[res.Slug], nil
}

// A cluster renamed its class: volumes recorded on `efs-old`, which nobody
// declares any more, and `ord-efs` declared on the same file system.
func classRepointEnv(t *testing.T, bound map[string][]string, classFS map[string]string) (*Server, http.Handler) {
	t.Helper()
	prov := &proverProvisioner{
		classProvisioner: classProvisioner{classes: []runtime.DeclaredClass{
			{Name: "ord-efs", Backing: runtime.BackingEFS, Default: true},
			{Name: "ord-azure", Backing: runtime.BackingAzureFiles},
		}},
		classFS: classFS, bound: bound,
	}
	srv, router, _ := storageTestEnv(t, prov)
	for _, slug := range []string{"sales-data", "team-share"} {
		res := &database.StorageResource{Slug: slug, Kind: database.StorageKindVolume,
			Backing: database.StorageBackingEFS, StorageClass: "efs-old"}
		if err := srv.db.CreateStorageResource(res, "root", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	return srv, router
}

func classesOf(t *testing.T, srv *Server) map[string]string {
	t.Helper()
	all, _ := srv.db.ListStorageResources()
	out := map[string]string{}
	for _, r := range all {
		out[r.Slug] = r.StorageClass
	}
	return out
}

// The silent failure: a volume bound to another file system re-pointed anyway,
// so its next folder binding is made on a file system its data is not on.
func TestAClassRepointRefusesAVolumeOnAnotherFileSystem(t *testing.T) {
	srv, router := classRepointEnv(t,
		map[string][]string{"sales-data": {"fs-ord"}, "team-share": {"fs-elsewhere"}},
		map[string]string{"ord-efs": "fs-ord"})
	rec := storageRequest(t, srv, router, http.MethodPost,
		"/api/v1/admin/storage/drives/class:efs-old/repoint", "root", `{"to":"ord-efs"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "fs-elsewhere") {
		t.Fatalf("repoint = %d %s, want 409 naming the other file system", rec.Code, rec.Body.String())
	}
	for slug, class := range classesOf(t, srv) {
		if class != "efs-old" {
			t.Errorf("%s moved to %q on a refused re-point", slug, class)
		}
	}
}

func TestAClassRepointMovesWhenEveryVolumeIsOnTheTargetFileSystem(t *testing.T) {
	srv, router := classRepointEnv(t,
		map[string][]string{"sales-data": {"fs-ord"}, "team-share": {"fs-ord"}},
		map[string]string{"ord-efs": "fs-ord"})
	rec := storageRequest(t, srv, router, http.MethodPost,
		"/api/v1/admin/storage/drives/class:efs-old/repoint", "root", `{"to":"ord-efs"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bound to fs-ord") {
		t.Fatalf("repoint = %d %s", rec.Code, rec.Body.String())
	}
	for slug, class := range classesOf(t, srv) {
		if class != "ord-efs" {
			t.Errorf("%s is on %q, want ord-efs", slug, class)
		}
	}
}

func TestAClassRepointFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		bound   map[string][]string
		classFS map[string]string
		to      string
		code    int
		says    string
	}{
		"never bound": {map[string][]string{"sales-data": {"fs-ord"}}, map[string]string{"ord-efs": "fs-ord"},
			"ord-efs", http.StatusConflict, "never been bound"},
		"class unreadable": {map[string][]string{"sales-data": {"fs-ord"}, "team-share": {"fs-ord"}}, map[string]string{},
			"ord-efs", http.StatusUnprocessableEntity, "could not be read"},
		"azure": {nil, nil, "ord-azure", http.StatusUnprocessableEntity, "Azure Files"},
	} {
		srv, router := classRepointEnv(t, tc.bound, tc.classFS)
		rec := storageRequest(t, srv, router, http.MethodPost,
			"/api/v1/admin/storage/drives/class:efs-old/repoint", "root", `{"to":"`+tc.to+`"}`)
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.says) {
			t.Errorf("%s: %d %s, want %d saying %q", name, rec.Code, rec.Body.String(), tc.code, tc.says)
		}
	}
}
