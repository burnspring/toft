package builder

import (
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"toft/internal/runtime"
)

// A build command with an account is the same command behind the privilege
// drop a workload gets, with the account's own HOME, TMPDIR and caches in place
// of the platform's — and a cache the command was never pointed at stays unset.
func TestABuildCommandIsWrappedForItsAccount(t *testing.T) {
	me, err := user.Current() // the group list is read from the account database
	if err != nil {
		t.Skip(err)
	}
	id := &buildIdentity{
		account: &runtime.AppAccount{Username: me.Username, UID: 1001, GID: 1000},
		home:    "/cache/accounts/toft-demo/home",
		tmp:     "/cache/accounts/toft-demo/tmp",
		caches:  map[string]string{"npm_config_cache": "/cache/accounts/toft-demo/npm", "GOCACHE": "/cache/accounts/toft-demo/go-build"},
	}
	name, args, env, creds, err := id.command("npm", []string{"ci"}, map[string]string{
		"npm_config_cache": "/cache/npm", "APP_VAR": "kept", "LD_PRELOAD": "./evil.so",
	})
	if err != nil {
		t.Fatal(err)
	}
	// No privilege-dropping program in front: the drop is the clone's, so the
	// first exec — the umask shell — already runs as the account, and a loader
	// variable among the app's is honored only there (security review
	// 2026-11-10, E-1).
	if name != "/bin/sh" || slices.Contains(args, "setpriv") || args[len(args)-2] != "npm" || args[len(args)-1] != "ci" {
		t.Errorf("argv = %s %v", name, args)
	}
	if creds == nil || creds.uid != 1001 || creds.gid != 1000 {
		t.Errorf("credentials = %+v, want the account's", creds)
	}
	if env["npm_config_cache"] != "/cache/accounts/toft-demo/npm" || env["HOME"] != id.home || env["TMPDIR"] != id.tmp {
		t.Errorf("env = %v", env)
	}
	if _, ok := env["GOCACHE"]; ok {
		t.Error("a cache the command was never pointed at was added")
	}
	if env["APP_VAR"] != "kept" {
		t.Error("the app's own variable was lost")
	}

	var none *buildIdentity
	if n, a, e, c, _ := none.command("npm", []string{"ci"}, map[string]string{"X": "1"}); n != "npm" || len(a) != 1 || e["X"] != "1" || c != nil {
		t.Errorf("without an account the command changed: %s %v %v %v", n, a, e, c)
	}
}

// The whole of it, as root, with a real second account: the release is handed
// over, the build's own code runs as the app's account and cannot read the
// platform's environment, its HOME and caches are its own, and the platform's
// writes inside the tree are the account's too. sudo go test ./internal/builder/.
func TestABuildRunsAsItsAppsAccount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: a build can only drop to an account from root")
	}
	nobody := &runtime.AppAccount{Username: "nobody", UID: 65534, GID: 65534}
	base := t.TempDir()
	for _, d := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cache := filepath.Join(base, "cache")
	release := filepath.Join(base, "apps", "app-1", "releases", "dep-1")
	if err := os.MkdirAll(release, 0o711); err != nil {
		t.Fatal(err)
	}
	for d := filepath.Join(base, "apps"); d != release; d = filepath.Join(d, strings.Split(strings.TrimPrefix(release, d+"/"), "/")[0]) {
		os.Chmod(d, 0o711)
	}
	if err := os.WriteFile(filepath.Join(release, "app.py"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Builder{opts: Options{AppsDir: filepath.Join(base, "apps"), CacheDir: cache,
		BuildAccount: func(string) (*runtime.AppAccount, error) { return nobody, nil }}}
	id, done, err := b.beginBuildAs("demo", release)
	if err != nil {
		t.Fatalf("beginBuildAs: %v", err)
	}
	defer done()

	if st := statOwner(t, filepath.Join(release, "app.py")); st != nobody.UID {
		t.Errorf("the release's files are owned by %d, want the account's", st)
	}

	script := `id -u > uid; echo "$HOME" > home; echo "$npm_config_cache" > npmcache; ` +
		`if cat /proc/1/environ >/dev/null 2>&1; then echo yes; else echo no; fi > environ`
	if err := b.runCommandFor(time.Minute, "", release, "sh", []string{"-c", script},
		map[string]string{"npm_config_cache": filepath.Join(cache, "npm")}, func(string) {}); err != nil {
		t.Fatalf("the build command: %v", err)
	}
	read := func(name string) string {
		raw, _ := os.ReadFile(filepath.Join(release, name))
		return strings.TrimSpace(string(raw))
	}
	if got := read("uid"); got != "65534" {
		t.Errorf("the build ran as uid %s, want the account's", got)
	}
	if got := read("environ"); got != "no" {
		t.Error("the build could read the platform's own environment")
	}
	if got := read("home"); got != id.home || !strings.HasPrefix(got, filepath.Join(cache, accountsDirName, "nobody")) {
		t.Errorf("HOME = %q, want the account's own", got)
	}
	if got := read("npmcache"); got != id.caches["npm_config_cache"] {
		t.Errorf("npm's cache = %q, want the account's own", got)
	}

	if err := b.asBuild(release, func() error {
		return os.WriteFile(filepath.Join(release, "platform-wrote"), []byte("x"), 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	if st := statOwner(t, filepath.Join(release, "platform-wrote")); st != nobody.UID {
		t.Errorf("a platform write inside the release is owned by %d, want the account's", st)
	}

	// The reclaim's escalation empties every account's caches.
	if step := b.reclaimAccounts(); step.Outcome != ReclaimDone {
		t.Errorf("reclaim = %+v", step)
	}
	if entries, _ := os.ReadDir(filepath.Join(cache, accountsDirName)); len(entries) != 0 {
		t.Errorf("%d account directories survived the reclaim", len(entries))
	}
}

func statOwner(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(info.Sys().(*syscall.Stat_t).Uid)
}

// **A deleted app's build directory never reaches the next app with its
// name.** On trytoft every EPA app recreated after a delete restored from its
// predecessor's renv cache. Two ways it is closed: the delete removes it
// (ForgetAccount), and a directory a different uid owns — left by an older
// platform — is emptied rather than handed over. sudo go test ./internal/builder/.
func TestAnAccountDirectoryIsNotInheritedByTheNextAppWithTheName(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: the directory changes owner")
	}
	base := t.TempDir()
	for _, d := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b := &Builder{opts: Options{AppsDir: filepath.Join(base, "apps"), CacheDir: filepath.Join(base, "cache")}}
	old := &runtime.AppAccount{Username: "toft-app-demo", UID: 65534, GID: 65534}
	id, err := b.accountDirs(old)
	if err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(id.caches["RENV_PATHS_CACHE"], "dplyr")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}

	// The same name at another uid: what is there is not the new account's.
	next := &runtime.AppAccount{Username: "toft-app-demo", UID: 65533, GID: 65534}
	id, err = b.accountDirs(next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(planted); !os.IsNotExist(err) {
		t.Errorf("the previous owner's cache survived a change of uid: %v", err)
	}
	if st := statOwner(t, filepath.Dir(id.home)); st != next.UID {
		t.Errorf("the directory is owned by %d, want the new account's", st)
	}

	// The delete removes it outright, and a second delete is not an error.
	if err := b.ForgetAccount(next.Username); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(id.home)); !os.IsNotExist(err) {
		t.Errorf("the account directory survived ForgetAccount: %v", err)
	}
	if err := b.ForgetAccount(next.Username); err != nil {
		t.Errorf("forgetting it twice: %v", err)
	}
	if err := b.ForgetAccount("../cache"); err == nil {
		t.Error("a path was accepted as an account name")
	}
}
