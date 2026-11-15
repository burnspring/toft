package builder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"toft/internal/runtime"
	"toft/sdk"
)

// Which account a build runs as (ADR §29).
//
// A build runs the repository's code — `npm run build`, a Python build backend,
// `renv::restore`, a notebook's cells — and it used to run all of it as the
// platform's own uid, root in the image and the chart. Root can read
// /proc/1/environ, which holds JWT_SECRET and ENCRYPTION_KEY, so publishing an
// app was enough to forge a session as anyone. A start's repair was the same
// thing reached from a running app: it rebuilt a release in place, as root, in a
// tree the app had owned since its last start.
//
// **A build now runs as the account its app runs as.** Not a build account of
// its own, and that is the decision: the app's account is the one identity that
// gains nothing by building, because everything a build could do as it the app
// can already do at run time. A shared build account would put every app's
// build under one uid, and two concurrent builds could then rewrite each other's
// trees. An install that runs apps as the platform itself runs builds that way
// too, and says so at start.
//
// Three things change for a build that has an account, and each answers a way
// the account would otherwise not be able to finish it or not be contained:
//
//   - **The tree is handed over first.** Everything before the first build
//     command — the clone, the scan, detection, the manifest — reads a tree only
//     root can write, so no app process can change what those steps judged.
//     Then runtime.HandOver gives it to the account through an os.Root, exactly
//     as a start does, and from there the account writes it.
//   - **The platform's own writes after that point are made as the account**
//     (asBuild, runtime.WithAccountFS): the Next.js base-path wrapper, the SDKs,
//     the R library directory, the render marker. Root never resolves a path in
//     a tree the account can rewrite.
//   - **Caches, HOME and TMPDIR are the account's own**, under
//     CACHE_DIR/accounts/<account>. The shared caches stay what they were for an
//     install with no accounts. Shared between accounts they would be one app's
//     build poisoning the package another app's build installs; the price is that
//     an app's first build fetches what another app already has.

// buildIdentity is the account one build runs its commands as, and the
// directories it writes instead of the platform's.
type buildIdentity struct {
	account *runtime.AppAccount
	home    string
	tmp     string
	// caches is each cache variable's directory for this account. Applied only
	// to a variable the command was already given, so a command the platform
	// never pointed at a cache is not pointed at one now.
	caches map[string]string
}

// accountsDirName is where per-account caches live under CACHE_DIR.
const accountsDirName = "accounts"

// accountCacheDirs are the cache variables and the directory each takes inside
// an account's own tree — the same four buildEnvironment points at the shared
// cache, plus renv's, which only renv's commands carry.
var accountCacheDirs = map[string]string{
	"npm_config_cache": "npm",
	"UV_CACHE_DIR":     "uv",
	"GOCACHE":          "go-build",
	"GOMODCACHE":       "go-mod",
	"RENV_PATHS_CACHE": "renv",
}

// beginBuildAs resolves slug's account and, when there is one, hands dir to it
// and records that commands run in dir run as it. The returned func forgets the
// record; call it when the build or repair is over. No account is no identity
// and no error: the build runs as the platform, as it always did.
func (b *Builder) beginBuildAs(slug, dir string) (*buildIdentity, func(), error) {
	none := func() {}
	if b.opts.BuildAccount == nil {
		return nil, none, nil
	}
	acct, err := b.opts.BuildAccount(slug)
	if err != nil {
		return nil, none, fmt.Errorf("resolving the account this app builds as: %w", err)
	}
	if acct == nil {
		return nil, none, nil
	}
	id, err := b.accountDirs(acct)
	if err != nil {
		return nil, none, err
	}
	if err := runtime.HandOver(dir, acct); err != nil {
		return nil, none, fmt.Errorf("handing the release to %s for its build: %w", acct.Username, err)
	}
	key := filepath.Clean(dir)
	b.identities.Store(key, id)
	return id, func() { b.identities.Delete(key) }, nil
}

// identityFor is the account commands run in dir run as, or nil.
func (b *Builder) identityFor(dir string) *buildIdentity {
	if dir == "" {
		return nil
	}
	v, ok := b.identities.Load(filepath.Clean(dir))
	if !ok {
		return nil
	}
	id, _ := v.(*buildIdentity)
	return id
}

// asBuild runs one of the platform's own file operations on a release as the
// account building it, or as the platform when nobody is.
func (b *Builder) asBuild(dir string, fn func() error) error {
	id := b.identityFor(dir)
	if id == nil {
		return fn()
	}
	return runtime.WithAccountFS(id.account, fn)
}

// scratchFor is where a build in dir may put a temporary directory its own
// commands must read: the account's TMPDIR, or the process's ("").
func (b *Builder) scratchFor(dir string) string {
	if id := b.identityFor(dir); id != nil {
		return id.tmp
	}
	return ""
}

// accountCreds is what a build command's clone is given to run as the account:
// the drop happens in the fork, before the first exec, so no program running
// as the platform ever sees the build's environment (runtime.SpawnCommand).
type accountCreds struct {
	uid, gid int
	groups   []int
}

// command is the argv, environment and credentials a build command runs with:
// unchanged and nil without an identity, and with one the umask stage in
// front, the account's own HOME, TMPDIR and caches in place of the platform's,
// and the account's uid and group list for the clone.
func (id *buildIdentity) command(name string, args []string, env map[string]string) (string, []string, map[string]string, *accountCreds, error) {
	if id == nil {
		return name, args, env, nil, nil
	}
	uid, gid, groups, err := id.account.Credentials()
	if err != nil {
		return "", nil, nil, nil, err
	}
	out := make(map[string]string, len(env)+2)
	for k, v := range env {
		out[k] = v
	}
	for k, dir := range id.caches {
		if _, ok := out[k]; ok {
			out[k] = dir
		}
	}
	out["HOME"] = id.home
	out["TMPDIR"] = id.tmp
	argv := runtime.SpawnCommand(name, args...)
	return argv[0], argv[1:], out, &accountCreds{uid: uid, gid: gid, groups: groups}, nil
}

// accountDirs makes sure acct has its own HOME, TMPDIR and caches, and returns
// them.
//
// The parent, CACHE_DIR/accounts, is root's and 0711: an account can reach its
// own directory and cannot list or create beside it, so nothing it does can put
// a link where root later looks.
//
// **A directory some other uid owns is emptied, never handed over.** An
// account name is the app's slug, so a deleted app's directory is the one a
// new app with the same slug would be given — its HOME, and caches its
// running processes could write — and the new app's builds would install
// what the old one left there (trytoft, 2026-10-24: every EPA app recreated
// after a delete restored from its predecessor's renv cache). ForgetAccount
// removes it at the delete; this catches what an older platform left behind,
// and an account whose uid moved for any other reason, where whose files they
// are cannot be told either.
func (b *Builder) accountDirs(acct *runtime.AppAccount) (*buildIdentity, error) {
	base := b.accountsBase()
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", base, err)
	}
	if err := ensureTraversable(base); err != nil {
		return nil, err
	}
	parent := filepath.Join(base, accountsDirName)
	if err := os.MkdirAll(parent, 0o711); err != nil {
		return nil, fmt.Errorf("creating %s: %w", parent, err)
	}
	if err := ensureTraversable(parent); err != nil {
		return nil, err
	}

	own := filepath.Join(parent, acct.Username)
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", parent, err)
	}
	defer root.Close()
	if err := root.Mkdir(acct.Username, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("creating %s: %w", own, err)
	}
	info, err := root.Lstat(acct.Username)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", own, err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != acct.UID {
		if err := root.RemoveAll(acct.Username); err != nil {
			return nil, fmt.Errorf("emptying %s, which another account owned: %w", own, err)
		}
		if err := root.Mkdir(acct.Username, 0o700); err != nil {
			return nil, fmt.Errorf("creating %s: %w", own, err)
		}
		if err := runtime.HandOver(own, acct); err != nil {
			return nil, fmt.Errorf("giving %s its build directory: %w", acct.Username, err)
		}
	}

	id := &buildIdentity{
		account: acct,
		home:    filepath.Join(own, "home"),
		tmp:     filepath.Join(own, "tmp"),
		caches:  make(map[string]string, len(accountCacheDirs)),
	}
	dirs := []string{id.home, id.tmp}
	for k, sub := range accountCacheDirs {
		id.caches[k] = filepath.Join(own, sub)
		dirs = append(dirs, id.caches[k])
	}
	// Made as the account, inside its own directory: created there, they are
	// its own without a chown.
	if err := runtime.WithAccountFS(acct, func() error {
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("creating %s's build directories: %w", acct.Username, err)
	}
	return id, nil
}

// accountsBase is the directory CACHE_DIR/accounts hangs from: the cache
// directory, or APPS_DIR/.build on an install with none.
func (b *Builder) accountsBase() string {
	if b.opts.CacheDir != "" {
		return b.opts.CacheDir
	}
	return filepath.Join(b.opts.AppsDir, ".build")
}

// ForgetAccount removes a deleted app's account directory — its HOME, TMPDIR
// and caches — so the next app given the same account name starts with none of
// it. Absent is not an error. Through an os.Root on the root-owned parent, so a
// link the account planted inside its own directory is removed, never followed.
func (b *Builder) ForgetAccount(username string) error {
	if username == "" || username != filepath.Base(username) || username == "." || username == ".." {
		return fmt.Errorf("not an account name: %q", username)
	}
	root, err := os.OpenRoot(filepath.Join(b.accountsBase(), accountsDirName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(username)
}

// EnsureTraversable is ensureTraversable for the boot path (APPS_DIR).
func EnsureTraversable(dir string) error { return ensureTraversable(dir) }

// ensureTraversable adds search permission for everyone to a directory that
// lacks it, and nothing else — no listing, no writing.
func ensureTraversable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o001 != 0 {
		return nil
	}
	return os.Chmod(dir, info.Mode().Perm()|0o011)
}

// materializeSDK unpacks an embedded SDK where the command installing it can
// read it: as the account building dir, into its TMPDIR.
func (b *Builder) materializeSDK(dir, lang string) (string, error) {
	var out string
	err := b.asBuild(dir, func() error {
		var err error
		out, err = sdk.Materialize(b.scratchFor(dir), lang)
		return err
	})
	return out, err
}
