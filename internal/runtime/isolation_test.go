package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIsolationDisabledByDefault(t *testing.T) {
	var i Isolation

	if i.Enabled() {
		t.Error("Enabled() should be false with no configuration")
	}

	got, err := i.resolve("demo")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != nil {
		t.Errorf("resolve = %+v, want nil (run as the platform user)", got)
	}
}

func TestIsolationResolvesExistingAccount(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("cannot determine current user")
	}

	i := Isolation{RunAsUser: me.Username}
	got, err := i.resolve("demo")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got == nil || got.Username != me.Username {
		t.Fatalf("resolve = %+v, want account %q", got, me.Username)
	}
	if got.UID != os.Getuid() || got.GID != os.Getgid() {
		t.Errorf("resolve uid/gid = %d/%d, want %d/%d", got.UID, got.GID, os.Getuid(), os.Getgid())
	}
}

// A missing account must fail the deploy rather than quietly running the app
// with the platform's own privileges.
func TestIsolationFailsWhenAccountMissing(t *testing.T) {
	i := Isolation{RunAsUser: "toft-no-such-account-9d2f1a"}

	if _, err := i.resolve("demo"); err == nil {
		t.Error("expected an error for a missing account")
	}
}

func TestPerAppIsolationFailsWhenAccountMissing(t *testing.T) {
	i := Isolation{PerApp: true, UserPrefix: "toft-app-"}

	if _, err := i.resolve("no-such-app-9d2f1a"); err == nil {
		t.Error("expected an error when the per-app account does not exist")
	}
}

func TestPerAppIsolationRequiresPrefix(t *testing.T) {
	i := Isolation{PerApp: true}

	if _, err := i.resolve("demo"); err == nil {
		t.Error("expected an error when no account prefix is configured")
	}
}

// Auto-create must refuse account names useradd would choke on (or that would
// smuggle flags): the name is built from the slug, so this is load-bearing.
func TestAutoCreateRejectsInvalidUsernames(t *testing.T) {
	// A long slug is shortened to fit (accountName), so what is still too long
	// is a prefix that leaves no room for one. Not a long slug: its shortened
	// name is valid, and the case would pass or fail on whether an earlier run
	// as root left that account behind.
	cases := []struct {
		name   string
		prefix string
		slug   string
	}{
		{"prefix too long", "toft-app-this-prefix-is-too-long-", "demo"},
		{"uppercase", "toft-app-", "MixedCase"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := Isolation{PerApp: true, UserPrefix: tc.prefix, AutoCreate: true}
			if _, err := i.resolve(tc.slug); err == nil {
				t.Errorf("expected a validation error for slug %q", tc.slug)
			}
		})
	}
}

// Chown-to-self is permitted for unprivileged users, so the handover logic is
// testable without root; the cross-account case needs root and runs in the
// container smoke test instead.
func TestOwnTree(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("cannot determine current user")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(sub, "dangling-target-ok")); err != nil {
		t.Fatal(err)
	}

	i := Isolation{RunAsUser: me.Username}
	account, err := i.resolve("demo")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := ownTree(dir, account); err != nil {
		t.Fatalf("ownTree: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("release dir mode = %o, want 0700", perm)
	}

	// The reconciler hands over the `current` symlink, not the directory. The
	// hand-over must land on the real tree — WalkDir does not follow a symlink
	// root, so without resolution it would chown the link and skip everything.
	link := filepath.Join(t.TempDir(), "current")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ownTree(link, account); err != nil {
		t.Fatalf("ownTree via symlink: %v", err)
	}
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("release dir mode after symlink hand-over = %o, want 0700 (hand-over followed the link?)", perm)
	}
}

// requireRootAccounts gates the tests that can only be written with real
// accounts and a real root.
//
// SAY THIS OUT LOUD, because a skipped test reads as a passing one and that is
// the exact failure this whole boundary is being fixed for: in an ordinary
// `go test ./...` run as a developer, the gate below is NOT exercised. What
// proves it is the container smoke test and a run as root. The unprivileged
// half of these tests covers what it can — the no-group case, and the rule that
// the per-app group never becomes the process's effective GID — and neither of
// those is the traversal check.
func requireRootAccounts(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: chowning a directory to root:group and creating app accounts")
	}
}

// The mode-and-group half of the gate: root-owned 0710 with the app's own
// group, on every parent — runs/ included, or a host job's identity file sits
// behind a directory its own account cannot traverse.
func TestGateAppDir(t *testing.T) {
	requireRootAccounts(t)

	appsDir := t.TempDir()
	const appID = "app-1"
	if err := os.MkdirAll(filepath.Join(appsDir, appID, "releases"), 0711); err != nil {
		t.Fatal(err)
	}
	// runs/ as a host job leaves it: created by the platform, 0700.
	if err := os.MkdirAll(filepath.Join(appsDir, appID, "runs"), 0700); err != nil {
		t.Fatal(err)
	}
	// A group that exists on every Linux box, standing in for the per-app one.
	g, err := user.LookupGroup("daemon")
	if err != nil {
		t.Skip("no 'daemon' group to stand in for a per-app group")
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		t.Fatal(err)
	}

	a := &AppAccount{Username: "toft-app-one", UID: 65534, GID: 65534, GroupGID: gid, GroupName: "daemon"}
	if err := gateAppDir(appsDir, appID, a); err != nil {
		t.Fatalf("gateAppDir: %v", err)
	}

	for _, dir := range []string{
		filepath.Join(appsDir, appID),
		filepath.Join(appsDir, appID, "releases"),
		filepath.Join(appsDir, appID, "runs"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0710 {
			t.Errorf("%s mode = %o, want 0710", dir, perm)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("%s: no stat", dir)
		}
		// Root-owned is the point: an owner may chmod, and the app must not be
		// able to widen the directory that excludes every other app.
		if st.Uid != 0 {
			t.Errorf("%s owner uid = %d, want 0 (the app could chmod its own gate)", dir, st.Uid)
		}
		if int(st.Gid) != gid {
			t.Errorf("%s group = %d, want %d", dir, st.Gid, gid)
		}
	}
}

// No per-app group means the gate is not in force and the parents keep the
// traversable mode they have always had — absent, not silently half-applied.
// This one runs unprivileged, because nothing is chowned.
func TestGateAppDirWithoutGroupLeavesModeAlone(t *testing.T) {
	appsDir := t.TempDir()
	const appID = "app-1"
	dir := filepath.Join(appsDir, appID)
	if err := os.MkdirAll(filepath.Join(dir, "releases"), 0711); err != nil {
		t.Fatal(err)
	}
	if err := gateAppDir(appsDir, appID, &AppAccount{Username: "toft-app-one", UID: 1, GID: 1}); err != nil {
		t.Fatalf("gateAppDir with no per-app group: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0711 {
		t.Errorf("app dir mode = %o, want 0711 unchanged", perm)
	}
	// And a nil account — no isolation at all — must not touch anything either.
	if err := gateAppDir(appsDir, appID, nil); err != nil {
		t.Fatalf("gateAppDir with no account: %v", err)
	}
}

// A build runs as the app's account through the same credentials a workload's
// clone gets (AppAccount.Credentials): the effective GID stays the account's
// primary group, shared by every app account and what `iptables -m owner
// --gid-owner` matches to block the metadata endpoints, and the per-app group
// reaches the process as a supplementary one. Naming the per-app group as the
// effective GID would gate the tree as well and silently stop every build
// matching that rule. And nothing runs in front of the command as the
// platform: SpawnCommand is the umask stage alone (security review
// 2026-11-10, E-1).
func TestSpawnKeepsTheSharedPrimaryGroup(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	const sharedGID, perAppGID = 4000, 4001
	a := &AppAccount{Username: me.Username, UID: 5000, GID: sharedGID, GroupGID: perAppGID, GroupName: "toft-app-one"}
	uid, gid, groups, err := a.Credentials()
	if err != nil {
		t.Fatal(err)
	}
	if uid != 5000 || gid != sharedGID {
		t.Errorf("credentials uid=%d gid=%d, want 5000 and the shared primary group", uid, gid)
	}
	if !slices.Contains(groups, perAppGID) {
		t.Errorf("groups %v lack the per-app group, so the gated directory is unreachable", groups)
	}
	argv := SpawnCommand("npm", "ci")
	if argv[0] != umaskShell || !strings.HasSuffix(strings.Join(argv, " "), "npm ci") {
		t.Errorf("spawn argv %q is not the umask stage in front of the command", argv)
	}
}

// The attack the gate exists to stop, modelled as it actually happens: app A
// widens its own release directory — which it owns, so this is a permitted
// operation and not a bug — and app B still cannot reach anything inside it,
// because reaching it means traversing two directories A does not own.
//
// This is the assertion that has been missing. Before the gate it passed for B
// only until A ran one chmod.
func TestOtherAppCannotTraverseAWidenedRelease(t *testing.T) {
	requireRootAccounts(t)

	i := Isolation{PerApp: true, UserPrefix: "toft-test-", AutoCreate: true, Group: "toft-test-apps"}
	a, err := i.resolve("alpha")
	if err != nil {
		t.Skipf("cannot provision test accounts: %v", err)
	}
	b, err := i.resolve("bravo")
	if err != nil {
		t.Skipf("cannot provision test accounts: %v", err)
	}
	if a.GroupGID == 0 {
		t.Fatal("alpha has no per-app group; the gate cannot be in force")
	}
	if a.GID != b.GID {
		t.Fatalf("the two accounts must share a primary group (%d != %d) or the egress rule is already broken",
			a.GID, b.GID)
	}

	appsDir := t.TempDir()
	if err := os.Chmod(appsDir, 0755); err != nil {
		t.Fatal(err)
	}
	const appID = "alpha-app"
	release := filepath.Join(appsDir, appID, "releases", "d1")
	if err := os.MkdirAll(release, 0711); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(release, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := gateAppDir(appsDir, appID, a); err != nil {
		t.Fatalf("gateAppDir: %v", err)
	}
	if err := ownTree(release, a); err != nil {
		t.Fatalf("ownTree: %v", err)
	}

	// A owns its release directory, so this succeeds — that is the premise.
	if err := os.Chmod(release, 0777); err != nil {
		t.Fatalf("A widening its own release dir should be permitted: %v", err)
	}

	if err := openAs(b, secret); err == nil {
		t.Error("app B could read a file inside app A's widened release; the parent gate did not hold")
	} else if !os.IsPermission(err) {
		t.Errorf("app B was refused, but not by a permission: %v", err)
	}
}

// openAs opens a path with the account's uid, primary gid and supplementary
// groups, in a child process — the only honest way to ask "could that app read
// this", since a root test process bypasses every check it is trying to make.
func openAs(a *AppAccount, path string) error {
	cmd := command("cat", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    uint32(a.UID),
			Gid:    uint32(a.GID),
			Groups: []uint32{uint32(a.GID)},
		},
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "Permission denied") {
			return os.ErrPermission
		}
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}

func TestEnabled(t *testing.T) {
	cases := []struct {
		name string
		i    Isolation
		want bool
	}{
		{"none", Isolation{}, false},
		{"shared account", Isolation{RunAsUser: "toft-apps"}, true},
		{"per app", Isolation{PerApp: true, UserPrefix: "toft-app-"}, true},
		{"autocreate alone is not isolation", Isolation{AutoCreate: true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.i.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The posture Admin → Info reports. The precedence case is the one worth
// asserting: an install that sets both must report the stronger of the two,
// because that is what resolve actually does with them — a page that said
// "one shared account" while every app ran under its own would be the
// misdescription this whole boundary is being fixed for.
func TestAccountsFor(t *testing.T) {
	cases := []struct {
		name      string
		runAsUser string
		perApp    bool
		want      AppAccounts
	}{
		{"nothing set", "", false, AccountsPlatform},
		{"shared account", "toft-apps", false, AccountsShared},
		{"per app", "", true, AccountsPerApp},
		{"per app wins over a shared account", "toft-apps", true, AccountsPerApp},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AccountsFor(tc.runAsUser, tc.perApp)
			if got != tc.want {
				t.Errorf("AccountsFor(%q, %v) = %q, want %q", tc.runAsUser, tc.perApp, got, tc.want)
			}
			// The two answers are one answer, which is why Accounts exists.
			i := Isolation{RunAsUser: tc.runAsUser, PerApp: tc.perApp}
			if i.Accounts() != got {
				t.Errorf("Isolation.Accounts() = %q, want %q", i.Accounts(), got)
			}
			if enabled := i.Enabled(); enabled != (got != AccountsPlatform) {
				t.Errorf("Enabled() = %v but posture is %q", enabled, got)
			}
		})
	}
}

// A directory link inside the tree is re-owned as a link and never followed:
// what it points at, outside the release, is left exactly as it was.
func TestOwnTreeDoesNotFollowALinkOutOfTheTree(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("cannot determine current user")
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "shadow")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "zz")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	account, err := (Isolation{RunAsUser: me.Username}).resolve("demo")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := ownTree(dir, account); err != nil {
		t.Fatalf("ownTree: %v", err)
	}
	after, _ := os.Stat(victim)
	if !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Error("ownTree touched a file outside the tree through a link")
	}
}

// The umask stage runs, not only appears. A gated folder's file is shared with
// the other apps on the gate by its group bits alone, so a stage that left the
// umask at 0022 would leave every co-mapped app able to read a file and not
// edit it — silently, because nothing fails until the second app tries. The
// stage is also an exec in a shell, so it is the one place an argument holding
// a space could be split; the app's own arguments must arrive as they left.
func TestTheSpawnStageSetsTheUmaskAndKeepsTheArguments(t *testing.T) {
	if _, err := os.Stat(umaskShell); err != nil {
		t.Skipf("no %s: %v", umaskShell, err)
	}
	stage := append(spawnArgs(), umaskShell, "-c", `umask; printf '%s|%s\n' "$0" "$1"`, "app", "a b")
	out, err := exec.Command(stage[0], stage[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("running the stage: %v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "0007\napp|a b"; got != want {
		t.Errorf("the stage printed %q, want %q", got, want)
	}
}

// A slug longer than the account name allows still gets an account: the name
// is unchanged whenever it fits, and otherwise truncated and made unique by a
// hash of the whole slug, so two long slugs sharing a prefix do not collide.
func TestALongSlugGetsAValidStableAccountName(t *testing.T) {
	if got := accountName("toft-app-", "drills-go"); got != "toft-app-drills-go" {
		t.Errorf("a short slug's account = %q, want it unchanged", got)
	}
	a := accountName("toft-app-", "this-slug-is-far-too-long-for-it")
	b := accountName("toft-app-", "this-slug-is-far-too-long-for-that")
	for _, n := range []string{a, b} {
		if !validUsername.MatchString(n) {
			t.Errorf("%q is not a valid username", n)
		}
	}
	if a == b {
		t.Errorf("two long slugs share the account %q", a)
	}
	if a != accountName("toft-app-", "this-slug-is-far-too-long-for-it") {
		t.Error("the name is not stable across calls")
	}
}

// A second hand-over of a tree the account already owns writes nothing: a
// chown to the same owner still moves the inode's ctime, so an unmoved ctime is
// the proof the walk read the entry and left it alone.
func TestOwnTreeLeavesAlreadyOwnedEntriesUnwritten(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("cannot determine current user")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	account, err := Isolation{RunAsUser: me.Username}.resolve("demo")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := ownTree(dir, account); err != nil {
		t.Fatalf("ownTree: %v", err)
	}
	ctime := func() syscall.Timespec {
		info, err := os.Lstat(f)
		if err != nil {
			t.Fatal(err)
		}
		return info.Sys().(*syscall.Stat_t).Ctim
	}
	before := ctime()
	time.Sleep(20 * time.Millisecond)
	if err := ownTree(dir, account); err != nil {
		t.Fatalf("second ownTree: %v", err)
	}
	if after := ctime(); after != before {
		t.Errorf("ctime moved from %v to %v: an entry already owned by the account was chowned again", before, after)
	}
}
