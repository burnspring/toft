package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// A test harness with the privileged calls recorded rather than made. What is
// asserted throughout this file is that the right call was made about the right
// path — which is the half a bug lives in, and the half a box that cannot run
// groupadd can still check.
type fakeShared struct {
	*SharedStorage
	groupsAdded []string
	// sets is every setGates call, as "user:gate,gate".
	sets   []string
	chowns map[string][2]int
	// regroups is every regroup call, as "path:gid".
	regroups []string
}

// gateFor is the gate the database would have recorded for a resource in these
// tests: its own name, at one fixed number.
func gateFor(slug string) Gate { return Gate{Group: StorageGroupName(slug), GID: 4242} }

func newFakeShared(t *testing.T, roots []StorageRoot, appsDir string) *fakeShared {
	t.Helper()
	f := &fakeShared{chowns: map[string][2]int{}}
	s := NewSharedStorage(roots, appsDir, "")
	if s == nil {
		t.Fatal("NewSharedStorage returned nil for a non-empty root list")
	}
	s.ensureGroup = func(g Gate) error { f.groupsAdded = append(f.groupsAdded, g.Group); return nil }
	s.setGates = func(a *AppAccount, gates []string) error {
		f.sets = append(f.sets, a.Username+":"+strings.Join(gates, ","))
		return nil
	}
	s.chown = func(_ *os.Root, _, path string, uid, gid int) error {
		f.chowns[path] = [2]int{uid, gid}
		return nil
	}
	s.own = func(*os.Root, string, *AppAccount) error { return nil }
	s.regroup = func(_ *os.Root, _, path string, gid int) error {
		f.regroups = append(f.regroups, fmt.Sprintf("%s:%d", path, gid))
		return nil
	}
	f.SharedStorage = s
	return f
}

// The operator's declaration is read strictly. A root that silently did not
// parse is a resource that cannot be provisioned, discovered at the first deploy
// that needed it.
func TestStorageRootsAreParsedStrictly(t *testing.T) {
	roots, err := ParseStorageRoots(" reports=/mnt/reports , archive=/mnt/archive:ro ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("got %d roots, want 2: %+v", len(roots), roots)
	}
	// Sorted by name, so two installs with the same roots in a different order
	// report them the same way.
	if roots[0].Name != "archive" || !roots[0].ReadOnly || roots[0].Path != "/mnt/archive" {
		t.Errorf("archive = %+v", roots[0])
	}
	if roots[1].Name != "reports" || roots[1].ReadOnly || roots[1].Path != "/mnt/reports" {
		t.Errorf("reports = %+v", roots[1])
	}

	for _, bad := range []string{"reports", "reports=", "=/mnt/x", "reports=relative/path",
		"a=/mnt/one,a=/mnt/two"} {
		if _, err := ParseStorageRoots(bad); err == nil {
			t.Errorf("%q parsed, want a refusal", bad)
		}
	}

	// No roots is not an error; it is the whole of "not offered".
	roots, err = ParseStorageRoots("")
	if err != nil || len(roots) != 0 {
		t.Errorf("empty = %v, %v; want no roots and no error", roots, err)
	}
	if NewSharedStorage(nil, "/apps", "") != nil {
		t.Error("NewSharedStorage with no roots and no signer should be nil — absent, not dormant")
	}
}

// The directory is root-owned and gated by the resource's own system group.
//
// The mode bit is load-bearing here where it would not be on a directory the
// app owned: the app does not own this one, so it cannot chmod it. That is the
// same property the release directory's gate already relies on.
func TestAResourceDirectoryIsRootOwnedAndGroupGated(t *testing.T) {
	// The umask a platform under sudo or systemd runs with: Mkdir alone left
	// the directory 0750 there, which a developer's 0002 never showed.
	defer syscall.Umask(syscall.Umask(0o022))
	dir := t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: dir}}, t.TempDir())

	res := StorageResource{Slug: "reports", Kind: MountVolume, Backing: BackingHost, Root: "data"}
	if err := f.EnsureStorage(context.Background(), res); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	made := filepath.Join(dir, "reports")
	info, err := os.Stat(made)
	if err != nil {
		t.Fatalf("the resource directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("the resource path is not a directory")
	}
	if got := info.Mode().Perm(); got != 0o770 {
		t.Errorf("mode = %o, want 0770", got)
	}
	// Closed, and nothing more: the group and the gate are made at the start,
	// from the number the database recorded, because a group made here would
	// die with whichever container served the request.
	if len(f.groupsAdded) != 0 || len(f.chowns) != 0 {
		t.Errorf("ensure made groups %v and chowns %v, want neither", f.groupsAdded, f.chowns)
	}

	// Idempotent, and a directory that exists is left exactly as it is: the
	// attach routes ensure on every mapping write, and re-owning a live
	// directory would shut out every app running on it.
	if err := os.Chmod(made, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := f.EnsureStorage(context.Background(), res); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if info, _ := os.Stat(made); info.Mode().Perm() != 0o750 {
		t.Errorf("a second ensure changed an existing directory's mode to %o", info.Mode().Perm())
	}

	// And a root nobody declared is refused by name rather than by silence.
	err = f.EnsureStorage(context.Background(), StorageResource{
		Slug: "elsewhere", Kind: MountVolume, Backing: BackingHost, Root: "nope"})
	if err == nil || !strings.Contains(err.Error(), "SHARED_STORAGE_ROOTS") {
		t.Errorf("undeclared root: got %v, want a refusal naming the setting", err)
	}
}

// Deleting a resource under a host root deletes its directory and every file
// in it — but never what a link inside it points at, and never anything on a
// read-only root, which the files were never Toft's to write.
func TestRemovingAResourceDeletesItsDirectoryAndNothingElse(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: dir}, {Name: "frozen", Path: dir, ReadOnly: true}}, t.TempDir())
	res := StorageResource{Slug: "reports", Kind: MountVolume, Backing: BackingHost, Root: "data"}

	if err := f.EnsureStorage(context.Background(), res); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	made := filepath.Join(dir, "reports")
	if err := os.MkdirAll(filepath.Join(made, "q1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(made, "q1", "a.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(outside, "precious")
	if err := os.WriteFile(precious, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(made, "escape")); err != nil {
		t.Fatal(err)
	}

	// Releasing is not destroying: the sweep and every other caller of
	// RemoveStorage must never reach the files.
	if err := f.RemoveStorage(context.Background(), res); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(made, "q1", "a.csv")); err != nil {
		t.Fatalf("RemoveStorage deleted files — only DestroyStorage may: %v", err)
	}

	report, err := f.DestroyStorage(context.Background(), res, nil)
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !report.RemovesData || len(report.Removed) != 1 || report.Removed[0] != made {
		t.Errorf("report = %+v, want the directory named as removed", report)
	}
	if _, err := os.Stat(made); !os.IsNotExist(err) {
		t.Errorf("the resource's directory survived its delete: %v", err)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Errorf("the delete followed a link out of the resource: %v", err)
	}

	kept := filepath.Join(dir, "archive")
	if err := os.MkdirAll(kept, 0o700); err != nil {
		t.Fatal(err)
	}
	ro := StorageResource{Slug: "archive", Kind: MountVolume, Backing: BackingHost, Root: "frozen"}
	report, err = f.DestroyStorage(context.Background(), ro, nil)
	if err != nil {
		t.Fatalf("destroy on a read-only root: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("a read-only root's directory was deleted: %v", err)
	}
	if report.RemovesData || len(report.LeftBehind) != 1 {
		t.Errorf("a read-only root's report = %+v, want it left behind and said", report)
	}

	gone := StorageResource{Slug: "reports", Kind: MountVolume, Backing: BackingHost, Root: "nowhere"}
	report, err = f.DestroyStorage(context.Background(), gone, nil)
	if err != nil || len(report.LeftBehind) != 1 || report.RemovesData {
		t.Errorf("an undeclared root = %+v, %v; want left behind, not an error", report, err)
	}
}

// Releasing one folder's binding — what the mapping sweep does when the last
// app on a folder is detached — removes nothing. It used to remove the whole
// resource's directory, every other folder of a partitioned resource included.
func TestReleasingAFolderKeepsTheResource(t *testing.T) {
	dir := t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: dir}}, t.TempDir())
	res := StorageResource{Slug: "share", Kind: MountVolume, Backing: BackingHost, Root: "data"}
	if err := f.EnsureStorage(context.Background(), res); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for _, folder := range []string{"teams", "finance"} {
		if err := os.MkdirAll(filepath.Join(dir, "share", folder), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "share", folder, "a.csv"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	folder := res
	folder.Folder = "teams"
	if err := f.RemoveStorage(context.Background(), folder); err != nil {
		t.Fatalf("release folder: %v", err)
	}
	for _, kept := range []string{"teams", "finance"} {
		if _, err := os.Stat(filepath.Join(dir, "share", kept, "a.csv")); err != nil {
			t.Errorf("releasing the teams folder deleted %s: %v", kept, err)
		}
	}
}

// Attaching resolves the mount under the app's own directory and puts the app's
// account in the resource's group — and the group is joined as a supplementary
// one, which is what the launcher's `setpriv --init-groups` makes work.
func TestAttachResolvesThePathAndJoinsTheGroup(t *testing.T) {
	rootDir, appsDir := t.TempDir(), t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, appsDir)

	account := &AppAccount{Username: "toft-reports", UID: 5000, GID: 6000, GroupGID: 7000, GroupName: "toft-reports"}
	mounts := []Mount{{Gate: gateFor("nightly"), Name: "nightly", Path: "/data/reports", Kind: MountVolume,
		Access: AccessWrite, Root: "data"}}

	out, err := f.Attach("app-1", account, mounts)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d mounts back, want 1", len(out))
	}

	// The mapping's path is absolute inside a workload, and a shared workload's
	// filesystem is the host's — so it is resolved under the app's own
	// directory rather than honoured literally, which would collide between
	// apps and need root at a place the platform has no business writing.
	want := filepath.Join(appsDir, "app-1", "storage", "data", "reports")
	if out[0].Path != want {
		t.Errorf("resolved path = %q, want %q", out[0].Path, want)
	}
	target, err := os.Readlink(want)
	if err != nil {
		t.Fatalf("the mount point is not a symlink: %v", err)
	}
	if target != filepath.Join(rootDir, "nightly") {
		t.Errorf("link target = %q, want the resource's directory under its root", target)
	}
	if len(f.sets) != 1 || f.sets[0] != "toft-reports:toft-store-nightly" {
		t.Errorf("gates set = %v, want the app's account in exactly the resource's group", f.sets)
	}
	if got := f.chowns[filepath.Join(rootDir, "nightly")]; got != [2]int{0, 4242} {
		t.Errorf("resource dir chown = %v, want root and the recorded gid", got)
	}

	// TOFT_STORAGE is built from what came back, so the description and
	// the directories cannot disagree.
	if desc := DescribeMounts(out); !strings.Contains(desc, want) {
		t.Errorf("the description does not carry the resolved path: %s", desc)
	}

	// Re-attaching replaces the link rather than failing on it: an
	// administrator who re-pointed a mapping must not leave the app following
	// the old one.
	mounts[0].Folder = "2026"
	if _, err := f.Attach("app-1", account, mounts); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	folder := filepath.Join(rootDir, "nightly", "2026")
	if target, _ := os.Readlink(want); target != folder {
		t.Errorf("link target after re-pointing = %q, want the folder", target)
	}
	// And the folder itself exists behind the same gate. Nothing else creates
	// it — a cluster gets one from the CSI driver's subPath and there is no
	// driver here — so without this the app opens a dangling symlink.
	info, err := os.Stat(folder)
	if err != nil {
		t.Fatalf("the folder was not created: %v", err)
	}
	if got := info.Mode() & (os.ModePerm | os.ModeSetgid); got != gatedMode {
		t.Errorf("folder mode = %v, want %v — setgid, so what an app writes takes the gate's group", got, gatedMode)
	}
	if got := f.chowns[folder]; got != [2]int{0, 4242} {
		t.Errorf("folder chown = %v, want root and the resource's group", got)
	}
}

// A service start and several job runs of one app attach at the same moment,
// and every one of them writes the same links. Before the attaches were
// serialized and the link swapped rather than removed, the loser of that race
// failed with "symlinkat …: file exists" and its run was an
// infrastructure_error — and an attach that changed nothing still took the
// path away from the running service for a moment.
func TestConcurrentAttachesOfOneAppAllSucceedAndLeaveAStableLink(t *testing.T) {
	rootDir, appsDir := t.TempDir(), t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, appsDir)
	account := &AppAccount{Username: "toft-reports", UID: 5000, GID: 6000, GroupGID: 7000, GroupName: "toft-reports"}
	mounts := func() []Mount {
		return []Mount{{Gate: gateFor("nightly"), Name: "nightly", Path: "/data/reports", Kind: MountVolume,
			Access: AccessWrite, Root: "data"}}
	}
	if _, err := f.Attach("app-1", account, mounts()); err != nil {
		t.Fatalf("first attach: %v", err)
	}
	link := filepath.Join(appsDir, "app-1", "storage", "data", "reports")
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.Attach("app-1", account, mounts()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent attach failed: %v", err)
	}
	after, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the link is gone after the attaches: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("an attach that changed nothing replaced the link — the running service loses its path for a moment each time")
	}
}

// ResolvedMounts is what `GET /api/v1/app/storage` answers from, and Attach is
// what TOFT_STORAGE is built from. The two computing the path separately is how
// the route came to report the administrator's path while the workload was
// handed a link under apps/<id>.
func TestResolvedMountsAgreeWithAttach(t *testing.T) {
	rootDir, appsDir := t.TempDir(), t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, appsDir)
	mounts := []Mount{
		{Gate: gateFor("nightly"), Name: "nightly", Path: "/data/reports", Kind: MountVolume, Access: AccessFull, Root: "data"},
		{Gate: gateFor("nightly"), Name: "nightly", Path: "/data/drop", Kind: MountVolume, Access: AccessFull, Root: "data", Folder: "in"},
	}
	resolved := f.ResolvedMounts("app-1", mounts)
	attached, err := f.Attach("app-1", &AppAccount{Username: "u", UID: 1, GID: 1, GroupGID: 7000, GroupName: "u"}, mounts)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got, want := DescribeMounts(resolved), DescribeMounts(attached); got != want {
		t.Errorf("resolved %s\nattached %s", got, want)
	}
	if !strings.Contains(DescribeMounts(resolved), `"folder":"in"`) {
		t.Error("the folder did not survive resolution")
	}
	// Nothing on the host was touched by resolving alone.
	if mounts[0].Path != "/data/reports" {
		t.Error("ResolvedMounts rewrote its caller's slice")
	}
}

// A store is reached over signed URLs, so a shared host attaches nothing for
// it: no group to join, no link to make, and an entry that says so by having no
// path. Resolving it through a root — the only path that existed — failed the
// start on a store, since a store never names one.
func TestAStoreIsDescribedNotAttached(t *testing.T) {
	rootDir, appsDir := t.TempDir(), t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, appsDir)
	mounts := []Mount{
		{Name: "uploads", Path: "/data/uploads", Kind: MountStore, Access: AccessWrite},
		{Gate: gateFor("nightly"), Name: "nightly", Path: "/data/reports", Kind: MountVolume, Access: AccessFull, Root: "data"},
	}
	out, err := f.Attach("app-1", &AppAccount{Username: "toft-r", UID: 1, GID: 1, GroupGID: 7000, GroupName: "toft-r"}, mounts)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if out[0].Path != "" {
		t.Errorf("store path = %q, want none", out[0].Path)
	}
	if _, err := os.Lstat(filepath.Join(appsDir, "app-1", "storage", "data", "uploads")); !os.IsNotExist(err) {
		t.Errorf("a link was made for a store: %v", err)
	}
	if len(f.sets) != 1 || f.sets[0] != "toft-r:toft-store-nightly" {
		t.Errorf("gates set = %v, want the volume's group only", f.sets)
	}
	if got := f.ResolvedMounts("app-1", mounts); got[0].Path != "" || got[1].Path != out[1].Path {
		t.Errorf("resolved %+v disagrees with attached %+v", got, out)
	}
	// With no provisioner at all a store is still described the same way.
	var none *SharedStorage
	if got := none.ResolvedMounts("app-1", mounts[:1]); got[0].Path != "" {
		t.Errorf("no provisioner: store path = %q, want none", got[0].Path)
	}
}

// What is refused is storage with nowhere to put it. A store needs no root, so
// an install with none refuses the volume beside it and names only the volume.
func TestOnlyAMountThatNeedsARootIsRefused(t *testing.T) {
	store := Mount{Name: "uploads", Kind: MountStore}
	vol := Mount{Name: "nightly", Kind: MountVolume}
	if err := RejectMounts(WorkloadSpec{Slug: "a", Mounts: []Mount{store}}); err != nil {
		t.Errorf("a store alone was refused: %v", err)
	}
	err := RejectMounts(WorkloadSpec{Slug: "a", Mounts: []Mount{store, vol}})
	if err == nil || !strings.Contains(err.Error(), "nightly") || strings.Contains(err.Error(), "uploads") {
		t.Errorf("refusal = %v, want the volume named and the store not", err)
	}
	rts := NewRuntimes(NewMockRuntime(nil), nil)
	if got := rts.UnplaceableSharedMounts([]Mount{store, vol}); len(got) != 1 || got[0].Name != "nightly" {
		t.Errorf("unplaceable = %+v, want the volume", got)
	}
}

// **The test whose failure would be silent.** A shared provisioner can now
// exist for stores alone, with no root — and the refusal of a volume used to
// ask whether a provisioner existed. If it asked that again, a volume mapping
// would deploy into a release directory the next deploy abandons: data loss
// that looks exactly like a successful deploy. Both refusals are asked here.
func TestASignerAloneStillRefusesAVolume(t *testing.T) {
	storesOnly := NewSharedStorage(nil, "/apps", BackingMountpoint)
	if storesOnly == nil {
		t.Fatal("a signer alone should make a provisioner")
	}
	pm2 := NewAgentRuntime(Isolation{}, LocalResolver{}, &stubAllocator{}, "", "").WithSharedStorage(storesOnly)
	if pm2.RootsDeclared() {
		t.Fatal("a provisioner with no roots reported roots")
	}
	volume := Mount{Name: "nightly", Path: "/data/reports", Kind: MountVolume, Access: AccessFull}
	store := Mount{Name: "uploads", Path: "/data/uploads", Kind: MountStore, Access: AccessWrite}

	rts := NewRuntimes(pm2, nil)
	if got := rts.UnplaceableSharedMounts([]Mount{store, volume}); len(got) != 1 || got[0].Name != "nightly" {
		t.Errorf("builder's question: unplaceable = %+v, want the volume", got)
	}
	_, err := pm2.Deploy(WorkloadSpec{Slug: "demo", Name: "demo-abc123", Framework: "nextjs",
		Release: LocalRelease("/srv/apps/demo/releases/dep1"), Mounts: []Mount{store, volume}})
	if !errors.Is(err, ErrStorageNotSupported) {
		t.Errorf("backend's question: err = %v, want ErrStorageNotSupported", err)
	}
}

// A root offers volumes, a signer offers stores, and neither implies the other.
func TestKindsFollowRootsAndSignerSeparately(t *testing.T) {
	roots := []StorageRoot{{Name: "data", Path: "/mnt/data"}}
	for _, tc := range []struct {
		name    string
		s       *SharedStorage
		kinds   string
		backing string
	}{
		{"neither", NewSharedStorage(nil, "/apps", ""), "", ""},
		{"root", NewSharedStorage(roots, "/apps", ""), "volume", ""},
		{"signer", NewSharedStorage(nil, "/apps", BackingMountpoint), "store", BackingMountpoint},
		{"both", NewSharedStorage(roots, "/apps", BackingMountpoint), "volume,store", BackingMountpoint},
		{"azblob", NewSharedStorage(nil, "/apps", BackingBlob), "store", BackingBlob},
	} {
		if got := strings.Join(tc.s.StorageKinds(), ","); got != tc.kinds {
			t.Errorf("%s: kinds = %q, want %q", tc.name, got, tc.kinds)
		}
		if got := strings.Join(tc.s.StorageBackings(MountStore), ","); got != tc.backing {
			t.Errorf("%s: store backings = %q, want %q", tc.name, got, tc.backing)
		}
	}
	if got := NewSharedStorage(nil, "/apps", BackingMountpoint).StorageBackings(MountVolume); len(got) != 0 {
		t.Errorf("a signer alone offered volume backings %v", got)
	}
}

// Ensuring a store on a shared host creates nothing — the bucket pre-exists and
// there is nothing to bind — and must not go looking for a root it never has.
func TestEnsuringAStoreCreatesNothing(t *testing.T) {
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: t.TempDir()}}, t.TempDir())
	res := StorageResource{Slug: "uploads", Kind: MountStore, Backing: BackingMountpoint}
	if err := f.EnsureStorage(context.Background(), res); err != nil {
		t.Errorf("EnsureStorage(store) = %v", err)
	}
	if err := f.RemoveStorage(context.Background(), res); err != nil {
		t.Errorf("RemoveStorage(store) = %v", err)
	}
	if len(f.groupsAdded) != 0 {
		t.Errorf("a store made groups %v", f.groupsAdded)
	}
}

// A level in shared mode is which mount the resource is under, never a
// permission the platform applies. So there are exactly two honest answers, and
// anything else is refused rather than recorded and not enforced.
func TestALevelTheRootCannotEnforceIsRefused(t *testing.T) {
	rootDir, appsDir := t.TempDir(), t.TempDir()
	// The roots exist, as an operator's mounts do: Attach makes the resource's
	// directory under a root and never the root (Q2-1).
	os.Mkdir(filepath.Join(rootDir, "rw"), 0o755)
	os.Mkdir(filepath.Join(rootDir, "ro"), 0o755)
	f := newFakeShared(t, []StorageRoot{
		{Name: "rw", Path: filepath.Join(rootDir, "rw")},
		{Name: "ro", Path: filepath.Join(rootDir, "ro"), ReadOnly: true},
	}, appsDir)

	for _, c := range []struct {
		name  string
		mount Mount
		want  string
	}{
		{"full on a read-only root", Mount{Gate: gateFor("a"), Name: "a", Path: "/data/a", Root: "ro",
			Access: AccessFull}, "mounted read-only"},
		// A volume offers read and full, so the refusal names full — the
		// level that works — and never `write`, which a volume no longer has.
		{"read on a writable root", Mount{Gate: gateFor("b"), Name: "b", Path: "/data/b", Root: "rw",
			Access: AccessRead}, "Map it at full"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.Attach("app-1", nil, []Mount{c.mount})
			if err == nil {
				t.Fatal("the level was accepted, want a refusal")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal = %v, want it to name %q", err, c.want)
			}
		})
	}

	// And the two that are honest go through.
	if _, err := f.Attach("app-1", nil, []Mount{
		{Gate: gateFor("a"), Name: "a", Path: "/data/a", Root: "ro", Access: AccessRead},
		{Gate: gateFor("b"), Name: "b", Path: "/data/b", Root: "rw", Access: AccessFull},
	}); err != nil {
		t.Fatalf("the enforceable levels were refused: %v", err)
	}
}

// An install with no root offers no shared-mode storage at all, and the refusal
// names what is missing rather than the mode.
func TestNoRootIsARefusalThatNamesTheSetting(t *testing.T) {
	spec := WorkloadSpec{Slug: "reports", Mounts: []Mount{{Name: "nightly"}}}
	err := RejectMounts(spec)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "SHARED_STORAGE_ROOTS") {
		t.Errorf("the refusal should name the setting, got: %v", err)
	}
	if !strings.Contains(err.Error(), "nightly") {
		t.Errorf("the refusal should name the resources, got: %v", err)
	}
	// No mounts, no refusal: this is not a rule about shared mode.
	if err := RejectMounts(WorkloadSpec{Slug: "plain"}); err != nil {
		t.Errorf("an app with no storage was refused: %v", err)
	}
}

// A store needs a CSI driver and therefore a cluster; there is no host form of
// one, and offering it would be a form that fails on submit.
func TestOnlyAFileStoreIsOfferedOnAHostRoot(t *testing.T) {
	s := NewSharedStorage([]StorageRoot{{Name: "data", Path: "/mnt/data"}}, "/apps", "")

	if got := s.StorageKinds(); len(got) != 1 || got[0] != MountVolume {
		t.Errorf("kinds = %v, want just a file store", got)
	}
	if got := s.StorageBackings(MountStore); len(got) != 0 {
		t.Errorf("store backings = %v, want none", got)
	}
	backings := s.StorageBackings(MountVolume)
	if len(backings) != 1 || backings[0] != BackingHost {
		t.Errorf("backings = %v, want host alone", backings)
	}
}

// S3 Files under a root is offered for one reason — people's transfers signed
// straight to its bucket — so it is offered only where an S3 signer exists.
// Never efs, whatever is configured: there is no cluster to bind one in.
func TestS3FilesIsOfferedOnlyBesideAnS3Signer(t *testing.T) {
	roots := []StorageRoot{{Name: "data", Path: "/mnt/data"}}
	for _, tc := range []struct {
		name, signer, want string
	}{
		{"no signer", "", BackingHost},
		{"s3 signer", BackingMountpoint, BackingHost + "," + BackingS3Files},
		{"azure signer", BackingBlob, BackingHost},
	} {
		got := NewSharedStorage(roots, "/apps", tc.signer).StorageBackings(MountVolume)
		if strings.Join(got, ",") != tc.want {
			t.Errorf("%s: volume backings = %v, want %s", tc.name, got, tc.want)
		}
		for _, b := range got {
			if b == BackingEFS {
				t.Errorf("%s: efs was offered with no cluster to bind an access point in", tc.name)
			}
		}
	}
}

// An app owns apps/<id>/storage and can replace any component of it with a
// link. Root must not delete or re-create an entry at the far end of one.
func TestAttachRefusesALinkPlantedUnderTheAppsStorage(t *testing.T) {
	rootDir, appsDir := t.TempDir(), t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, appsDir)
	account := &AppAccount{Username: "toft-reports", UID: 5000, GID: 6000, GroupGID: 7000, GroupName: "toft-reports"}
	mounts := []Mount{{Gate: gateFor("nightly"), Name: "nightly", Path: "/data/reports", Kind: MountVolume,
		Access: AccessWrite, Root: "data"}}
	if _, err := f.Attach("app-1", account, mounts); err != nil {
		t.Fatalf("first attach: %v", err)
	}

	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "reports")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(appsDir, "app-1", "storage", "data")
	if err := os.RemoveAll(data); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimDir, data); err != nil {
		t.Fatal(err)
	}
	_, _ = f.Attach("app-1", account, mounts) // may fail; must not touch the victim
	if fi, err := os.Lstat(victim); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("an entry outside apps/<id> was replaced through a planted link: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim content = %q", b)
	}
}

// A real file or directory where the mount's link belongs fails the start and
// is left exactly as it was: the rename used to replace a file silently and
// fail on a directory with a bare EISDIR.
func TestAttachRefusesARealEntryWhereTheLinkBelongs(t *testing.T) {
	account := &AppAccount{Username: "toft-reports", UID: 5000, GID: 6000, GroupGID: 7000, GroupName: "toft-reports"}
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			rootDir, appsDir := t.TempDir(), t.TempDir()
			f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, appsDir)
			mounts := []Mount{{Gate: gateFor("nightly"), Name: "nightly", Path: "/data/reports", Kind: MountVolume,
				Access: AccessWrite, Root: "data"}}
			if _, err := f.Attach("app-1", account, mounts); err != nil {
				t.Fatalf("first attach: %v", err)
			}
			link := filepath.Join(appsDir, "app-1", "storage", "data", "reports")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			keep := link
			if kind == "directory" {
				if err := os.Mkdir(link, 0o750); err != nil {
					t.Fatal(err)
				}
				keep = filepath.Join(link, "theirs.txt")
			}
			if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := f.Attach("app-1", account, mounts)
			if err == nil || !strings.Contains(err.Error(), "is a "+kind) || !strings.Contains(err.Error(), "move it aside") {
				t.Fatalf("attach over a real %s = %v, want a refusal naming it", kind, err)
			}
			if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink != 0 {
				t.Errorf("the %s was replaced: %v %v", kind, fi, err)
			}
			if b, _ := os.ReadFile(keep); string(b) != "keep" {
				t.Errorf("content = %q, want it untouched", b)
			}

			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Attach("app-1", account, mounts); err != nil {
				t.Fatalf("moved aside, the attach still fails: %v", err)
			}
		})
	}
}

// A folder that is a link planted inside a resource is never gated through.
func TestAttachDoesNotGateThroughAPlantedFolderLink(t *testing.T) {
	rootDir, appsDir := t.TempDir(), t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, appsDir)
	victim := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "nightly"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(rootDir, "nightly", "team")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(victim)
	mounts := []Mount{{Gate: gateFor("nightly"), Name: "nightly", Folder: "team", Path: "/data/reports", Kind: MountVolume,
		Access: AccessWrite, Root: "data"}}
	if _, err := f.Attach("app-1", nil, mounts); err == nil {
		t.Error("attach through a planted folder link succeeded")
	}
	if len(f.chowns[victim]) != 0 && f.chowns[victim] != [2]int{} {
		t.Errorf("the link's target was chowned: %v", f.chowns[victim])
	}
	after, _ := os.Stat(victim)
	if after.Mode() != before.Mode() {
		t.Errorf("the link's target was chmod'ed: %v -> %v", before.Mode(), after.Mode())
	}
}

// Setting an account's gates takes it out of every storage group it no longer
// holds and into every one it now does — and touches nothing else. The account's
// own group, the shared primary and an operator's group on a pre-provisioned
// account are not Toft's to remove.
func TestMembershipChangesTouchOnlyTheGates(t *testing.T) {
	have := []string{"toft-apps", "toft-app-reports", "toft-store-old", "toft-store-kept",
		"toft-sf-0123456789ab", "finance-readers"}
	want := []string{"toft-store-kept", "toft-store-new"}
	leave, join := membershipChanges(have, "toft-app-reports", want)
	if got := strings.Join(leave, ","); got != "toft-sf-0123456789ab,toft-store-old" {
		t.Errorf("leave = %s, want the two gates no longer held", got)
	}
	if got := strings.Join(join, ","); got != "toft-store-new" {
		t.Errorf("join = %s, want the one gate not yet held", got)
	}
	if leave, join := membershipChanges(have, "toft-app-reports", nil); len(join) != 0 || len(leave) != 3 {
		t.Errorf("no mounts: leave %v join %v, want every gate left and nothing joined", leave, join)
	}
}

// An app whose last mapping was removed leaves the gate at its next start.
// Attach used to return before doing anything when it had no mounts, so the
// account kept the group for as long as the account existed.
func TestAStartWithNoMountsLeavesEveryGate(t *testing.T) {
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: t.TempDir()}}, t.TempDir())
	if _, err := f.Attach("app-1", &AppAccount{Username: "toft-r", UID: 1, GID: 1, GroupGID: 7000, GroupName: "toft-r"}, nil); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if len(f.sets) != 1 || f.sets[0] != "toft-r:" {
		t.Errorf("gates set = %v, want the account set to no gate at all", f.sets)
	}
}

// Every start re-makes the gate — the group at its recorded number and the
// directory behind it — because a container restart empties /etc/group and the
// directory still carries the number. Doing it at admin time only was how the
// first start after a restart failed on a group that no longer existed.
func TestEveryStartRemakesTheGateAtItsRecordedNumber(t *testing.T) {
	rootDir := t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, t.TempDir())
	account := &AppAccount{Username: "toft-r", UID: 1, GID: 1, GroupGID: 7000, GroupName: "toft-r"}
	mounts := []Mount{{Name: "nightly", Path: "/data/r", Kind: MountVolume, Access: AccessFull,
		Root: "data", Gate: Gate{Group: "toft-store-nightly", GID: 62007}}}
	for i := 0; i < 2; i++ {
		if _, err := f.Attach("app-1", account, mounts); err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		dir := filepath.Join(rootDir, "nightly")
		if got := f.chowns[dir]; got != [2]int{0, 62007} {
			t.Errorf("attach %d: resource dir chown = %v, want root and the recorded gid", i, got)
		}
		delete(f.chowns, dir)
	}
	if len(f.groupsAdded) != 2 {
		t.Errorf("groups ensured = %v, want the gate ensured at every start", f.groupsAdded)
	}

	// A mount that carries no recorded gate is refused rather than gated to a
	// number nobody recorded.
	mounts[0].Gate = Gate{}
	if _, err := f.Attach("app-1", account, mounts); err == nil || !strings.Contains(err.Error(), "no recorded gate") {
		t.Errorf("attach without a gate = %v, want a refusal", err)
	}
}

// A gate whose number moved is a gate on someone else's files, so the host
// disagreeing with the database refuses — both ways round — rather than being
// repaired. Run against real groups that every box has; neither case reaches
// groupadd.
func TestAGateTheHostDisagreesWithIsRefused(t *testing.T) {
	root, err := user.LookupGroupId("0")
	if err != nil {
		t.Skip("no gid 0 on this machine")
	}
	err = systemGroupEnsure(Gate{Group: root.Name, GID: 62999})
	if err == nil || !strings.Contains(err.Error(), "in the database") {
		t.Errorf("name at another gid: %v, want a refusal naming both numbers", err)
	}
	err = systemGroupEnsure(Gate{Group: "toft-store-no-such-gate-here", GID: 0})
	if err == nil || !strings.Contains(err.Error(), "belongs to "+root.Name) {
		t.Errorf("gid under another name: %v, want a refusal naming its owner", err)
	}
}

// The gate range is read as strictly as the roots: a range that silently did
// not parse is a gate that cannot be numbered, found at the first create.
func TestTheGateRangeIsParsedStrictly(t *testing.T) {
	if lo, hi, err := ParseGIDRange(""); err != nil || lo != 0 || hi != 0 {
		t.Errorf("empty = %d-%d, %v; want the database's default (0, 0)", lo, hi, err)
	}
	if lo, hi, err := ParseGIDRange(" 61000 - 61499 "); err != nil || lo != 61000 || hi != 61499 {
		t.Errorf("valid range = %d-%d, %v", lo, hi, err)
	}
	for _, bad := range []string{"62000", "a-b", "62000-x", "999-2000", "62000-65534", "63000-62000"} {
		if _, _, err := ParseGIDRange(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// An account with no group of its own cannot hold a gate. PM2 would start it
// through its own uid/gid drop, which never gains the account's supplementary
// groups, so the gate would be joined and unusable; and an account several apps
// share would have its gates set to one app's at every start. Refused, naming
// the setting — and an app with no mounts on such an account has nothing set.
func TestAGateNeedsAnAccountWithAGroupOfItsOwn(t *testing.T) {
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: t.TempDir()}}, t.TempDir())
	shared := &AppAccount{Username: "toft-apps-shared", UID: 1, GID: 1}
	mounts := []Mount{{Gate: gateFor("nightly"), Name: "nightly", Path: "/data/r", Kind: MountVolume,
		Access: AccessWrite, Root: "data"}}
	_, err := f.Attach("app-1", shared, mounts)
	if err == nil || !strings.Contains(err.Error(), "APP_USER_PER_APP") {
		t.Errorf("attach = %v, want a refusal naming APP_USER_PER_APP", err)
	}
	if _, err := f.Attach("app-1", shared, nil); err != nil {
		t.Fatalf("attach with no mounts: %v", err)
	}
	if len(f.sets) != 0 {
		t.Errorf("a shared account's gates were set: %v", f.sets)
	}
}

// A partitioned resource's folders are each their own gate, and everything
// above a folder is passable and nothing more: `0711 root:root` on the
// resource's directory and on every component between it and the folder,
// `2770 root:<gate>` on the folder. An app on teams/a reaches its folder and
// cannot list, write or enter anything beside it.
func TestAPartitionedFolderIsTheOnlyGate(t *testing.T) {
	rootDir := t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, t.TempDir())
	account := &AppAccount{Username: "toft-a", UID: 1, GID: 1, GroupGID: 7000, GroupName: "toft-a"}
	gate := Gate{Group: FolderGateName("share", "teams/a"), GID: 62010}
	mounts := []Mount{{Gate: gate, Name: "share", Folder: "teams/a", Partitioned: true,
		Path: "/data/a", Kind: MountVolume, Access: AccessFull, Root: "data"}}
	if _, err := f.Attach("app-1", account, mounts); err != nil {
		t.Fatalf("attach: %v", err)
	}

	for _, c := range []struct {
		rel  string
		gid  int
		mode os.FileMode
	}{
		{"share", 0, passMode},
		{"share/teams", 0, passMode},
		{"share/teams/a", 62010, gatedMode},
	} {
		dir := filepath.Join(rootDir, c.rel)
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s: %v", c.rel, err)
		}
		if got := info.Mode() & (os.ModePerm | os.ModeSetgid); got != c.mode {
			t.Errorf("%s mode = %v, want %v", c.rel, got, c.mode)
		}
		if got := f.chowns[dir]; got != [2]int{0, c.gid} {
			t.Errorf("%s chown = %v, want root:%d", c.rel, got, c.gid)
		}
	}
	if len(f.sets) != 1 || f.sets[0] != "toft-a:"+gate.Group {
		t.Errorf("gates set = %v, want the folder's gate alone", f.sets)
	}

	// The resource itself is never mapped whole. The database refuses that
	// first; this is the floor.
	mounts[0].Folder = ""
	if _, err := f.Attach("app-1", account, mounts); err == nil || !strings.Contains(err.Error(), "only by folder") {
		t.Errorf("a whole mapping of a partitioned resource = %v, want a refusal", err)
	}
}

// A partitioned folder re-mapped at another depth takes a new gate, and what
// people uploaded there still carries the old one. Its contents follow the
// folder onto the new gate — once, when the folder's own group shows it moved,
// not at every start.
func TestARegatedFolderRegroupsWhatIsAlreadyInside(t *testing.T) {
	rootDir := t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: rootDir}}, t.TempDir())
	folder := filepath.Join(rootDir, "share", "teams", "a")
	if err := os.MkdirAll(filepath.Join(folder, "sub"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "sub", "f.csv"), []byte("x"), 0o660); err != nil {
		t.Fatal(err)
	}
	account := &AppAccount{Username: "toft-a", UID: 1, GID: 1, GroupGID: 7000, GroupName: "toft-a"}
	attach := func(gid int) {
		t.Helper()
		gate := Gate{Group: FolderGateName("share", "teams/a"), GID: gid}
		mounts := []Mount{{Gate: gate, Name: "share", Folder: "teams/a", Partitioned: true,
			Path: "/data/a", Kind: MountVolume, Access: AccessFull, Root: "data"}}
		if _, err := f.Attach("app-1", account, mounts); err != nil {
			t.Fatalf("attach: %v", err)
		}
	}

	// The directory is on this test's own group, which is not the gate's.
	attach(62010)
	if want := folder + ":62010"; len(f.regroups) != 1 || f.regroups[0] != want {
		t.Fatalf("regroups = %v, want [%s]", f.regroups, want)
	}
	// Once the folder is on its gate — here, the group it already carries —
	// a start walks nothing.
	f.regroups = nil
	attach(os.Getgid())
	if len(f.regroups) != 0 {
		t.Errorf("a folder already on its gate was walked: %v", f.regroups)
	}
	// A folder the start creates holds nothing to move.
	fresh := filepath.Join(rootDir, "share", "teams", "b")
	gate := Gate{Group: FolderGateName("share", "teams/b"), GID: 62011}
	if _, err := f.Attach("app-2", account, []Mount{{Gate: gate, Name: "share", Folder: "teams/b", Partitioned: true,
		Path: "/data/b", Kind: MountVolume, Access: AccessFull, Root: "data"}}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	for _, r := range f.regroups {
		if strings.HasPrefix(r, fresh) {
			t.Errorf("a folder just created was walked: %v", f.regroups)
		}
	}
}

// The real walk reaches beneath the folder, tolerates a planted link (it is
// Lchowned, never followed), and writes nothing to an entry already on the gid.
func TestRegroupSubtreeReachesBeneathTheFolder(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "a", "sub"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "sub", "f.csv"), []byte("x"), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "a", "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// To the group this process already has: the one Lchown an unprivileged
	// test may make, and every entry is already on it, so nothing is written.
	if err := regroupSubtree(root, "a", os.Getgid()); err != nil {
		t.Fatalf("regroupSubtree to own gid: %v", err)
	}
	// Into a group this process is not in: refused per entry, proving the walk
	// reaches beneath the folder rather than stopping at it.
	if os.Getuid() != 0 {
		if err := regroupSubtree(root, "a", 62999); err == nil {
			t.Error("regrouping to a foreign gid as an unprivileged user succeeded, so the walk changed nothing")
		}
	}
}

// A folder's gate name is stable, short enough for groupadd whatever it names,
// and different for every folder — including two that differ only past the
// point a prefix would have cut them.
func TestAFolderGateNameIsStableAndShort(t *testing.T) {
	a := FolderGateName("share", "teams/a")
	if a != FolderGateName("share", "teams/a") {
		t.Error("the name moved between two calls")
	}
	long := FolderGateName("a-very-long-resource-slug-indeed", "and/a/folder/nested/several/levels/deep")
	if len(a) != 20 || len(long) != 20 || !strings.HasPrefix(a, "toft-sf-") || !isGateName(a) {
		t.Errorf("names %q and %q, want toft-sf- plus 12 hex characters", a, long)
	}
	if a == FolderGateName("share", "teams/b") || a == FolderGateName("shared", "teams/a") {
		t.Error("two folders share a gate name")
	}
}

// The create's question, asked of the disk: anything at the resource's path —
// a directory, a file, a link — is in the way, and only a missing one is free.
// A store has no directory to collide with.
func TestExistingDirectorySeesAnythingAtThePath(t *testing.T) {
	root := t.TempDir()
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: root}}, t.TempDir())
	res := func(slug string) StorageResource {
		return StorageResource{Slug: slug, Kind: MountVolume, Backing: BackingHost, Root: "data"}
	}

	if path, taken, err := f.ExistingDirectory(res("fresh")); err != nil || taken || path != filepath.Join(root, "fresh") {
		t.Errorf("a missing directory = %q %v %v, want its path, free", path, taken, err)
	}
	if err := os.Mkdir(filepath.Join(root, "made"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"made", "file", "link"} {
		if _, taken, err := f.ExistingDirectory(res(slug)); err != nil || !taken {
			t.Errorf("%s = taken %v, %v; want taken", slug, taken, err)
		}
	}
	if _, taken, _ := f.ExistingDirectory(StorageResource{Slug: "made", Kind: MountStore}); taken {
		t.Error("a store was reported as colliding with a directory")
	}
	if _, _, err := f.ExistingDirectory(StorageResource{Slug: "x", Kind: MountVolume, Root: "gone"}); err == nil {
		t.Error("an undeclared root was answered rather than refused")
	}
}

// BB-027 (fourth 2026-11-03 black-box list): at the namespace tier a folder
// mapping is the folder — its resource's directory is masked and the folder
// kept, so the parent the link resolves into lists nothing beside it. A
// resource also mapped whole is left alone, and a store has nothing to hide.
func TestAFolderMappingIsMaskedToItsFolderAtTheNamespaceTier(t *testing.T) {
	f := NewSharedStorage([]StorageRoot{{Name: "ebs", Path: "/mnt/ebs"}}, "/apps", "")
	mask, keep := f.NamespaceView([]Mount{
		{Name: "v", Root: "ebs", Kind: MountVolume, Folder: "a"},
		{Name: "v", Root: "ebs", Kind: MountVolume, Folder: "c/d"},
		{Name: "w", Root: "ebs", Kind: MountVolume, Folder: "x"},
		{Name: "w", Root: "ebs", Kind: MountVolume},
		{Name: "s", Kind: MountStore, Folder: "f"},
	})
	if strings.Join(mask, ",") != "/mnt/ebs/v" {
		t.Errorf("mask = %v, want only v's directory", mask)
	}
	if strings.Join(keep, ",") != "/mnt/ebs/v/a,/mnt/ebs/v/c/d" {
		t.Errorf("keep = %v, want v's two folders", keep)
	}
	var none *SharedStorage
	if m, k := none.NamespaceView([]Mount{{Name: "v", Root: "ebs", Kind: MountVolume, Folder: "a"}}); m != nil || k != nil {
		t.Errorf("no provisioner: %v %v", m, k)
	}
}

// A root that is not there is a mount that is not there: Attach refuses rather
// than making the mountpoint on the local disk and starting the app on an empty
// "durable" directory whose writes vanish when the mount returns (code review
// 2026-11-11, Q2-1). The resource's own directory under a present root is
// still made.
func TestAttachRefusesAnAbsentRoot(t *testing.T) {
	appsDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "efs-not-mounted")
	f := newFakeShared(t, []StorageRoot{{Name: "data", Path: missing}}, appsDir)
	account := &AppAccount{Username: "toft-reports", UID: 5000, GID: 6000, GroupGID: 7000, GroupName: "toft-reports"}
	mounts := []Mount{{Gate: gateFor("nightly"), Name: "nightly", Path: "/data/reports", Kind: MountVolume,
		Access: AccessWrite, Root: "data"}}
	if _, err := f.Attach("app-1", account, mounts); err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("attach with the root absent: err = %v, want a refusal naming the mount", err)
	}
	if _, err := os.Lstat(missing); err == nil {
		t.Fatal("the absent root was created on the local disk")
	}
}

// A gated folder is chowned and chmodded on its own descriptor, opened with no
// link followed: a link a gate member swapped in at the name is refused by the
// open itself, where Lstat-then-Chown by name left a window (code review
// 2026-11-11, Q2-4).
func TestOpenDirNoFollowRefusesALink(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "real"), 0o755)
	os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link"))
	os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if f, err := openDirNoFollow(root, "real"); err != nil {
		t.Fatalf("a plain directory: %v", err)
	} else {
		f.Close()
	}
	if _, err := openDirNoFollow(root, "link"); err == nil {
		t.Error("a link was opened as the directory")
	}
	if _, err := openDirNoFollow(root, "file"); err == nil {
		t.Error("a file was opened as a directory")
	}
	if err := chmodDirNoFollow(root, "link", 0o770); err == nil {
		t.Error("a link was chmodded by name")
	}
}
