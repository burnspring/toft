package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Isolation controls which OS identity deployed apps run as.
//
// By default an app runs as the same user as Toft itself, which means it
// can read the platform database, every other app's files, and the process
// manager's state. Running apps as a different user is the single largest
// reduction in what a hostile or compromised app can reach.
//
// Two modes are supported:
//
//	RunAsUser              every app runs as one unprivileged account, which
//	                       separates apps from the platform but not from each
//	                       other;
//	PerApp + UserPrefix    each app runs as its own account named
//	                       "{prefix}{slug}", which separates apps from each
//	                       other too.
//
// Accounts are pre-provisioned by the operator by default — creating them is
// a decision with system-wide consequences on a shared host. AutoCreate opts
// into creating missing accounts at deploy time instead, which is the only
// workable model inside a container (the image cannot know future app slugs,
// and the account namespace is the container's own). Auto-created accounts are
// system accounts with no login shell and Group as their primary group, so a
// single group-owner firewall rule can cover every app process.
type Isolation struct {
	RunAsUser  string
	PerApp     bool
	UserPrefix string
	AutoCreate bool
	// Group is the shared primary group for auto-created accounts. Egress
	// rules (e.g. the AWS metadata block in docker/entrypoint.sh) match on it.
	Group string
	// Numbers is the recorded number of an auto-created per-app account — its
	// uid, and its per-app group's gid — allocated on first ask (ADR §122,
	// database.AppAccountNumber). Nil keeps useradd's own choice, which is right
	// only where /etc/passwd outlives the platform, such as a test.
	Numbers func(name string) (int, error)
}

// AppAccount is a resolved OS identity for a deployed app's processes.
type AppAccount struct {
	Username string
	UID      int
	// GID is the account's *primary* group, which every app account shares.
	// The egress firewall rule keys on it (`iptables -m owner --gid-owner`,
	// docker/entrypoint.sh) and it must stay shared for that rule to match.
	GID int
	// GroupGID is a group this account is in and no other app account is —
	// supplementary, never primary, which is the whole reason both can be true
	// at once. It is what gates the app's directory tree; 0 means this install
	// has no such group for this app and the gate is not in force.
	GroupGID int
	// GroupName exists to say which group in an error or a log line, because a
	// bare gid is not something an operator can act on.
	GroupName string
}

// Usernames useradd accepts everywhere; also caps length at the portable 32.
var validUsername = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// resolve returns the account an app should run as, or nil for "same user as
// Toft". An error means isolation was requested but cannot be honoured —
// callers must treat that as a deploy failure rather than silently running the
// app with the platform's own privileges.
func (i Isolation) resolve(slug string) (*AppAccount, error) {
	name := i.RunAsUser
	if i.PerApp {
		if i.UserPrefix == "" {
			return nil, fmt.Errorf("per-app isolation requires an account name prefix")
		}
		name = accountName(i.UserPrefix, slug)
	}
	if name == "" {
		return nil, nil
	}

	u, err := i.lookupOrCreate(name)
	if err != nil {
		return nil, err
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, fmt.Errorf("app account %q has non-numeric uid %q", name, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, fmt.Errorf("app account %q has non-numeric gid %q", name, u.Gid)
	}
	a := &AppAccount{Username: name, UID: uid, GID: gid}

	// The per-app group, which gates the directory tree. Only meaningful when
	// each app has an account of its own — with one shared account there is no
	// "other app" for a group to exclude, and a group per *app* would be a
	// group every app is in.
	//
	// Looked up by name rather than remembered from creation, because an
	// operator's pre-provisioned account may already have one (a user-private
	// group is what useradd makes by default on most distributions) and because
	// autocreated accounts live in a container's /etc/passwd, which dies with
	// it. Absent is not an error: it means this install predates the group or
	// the operator provisioned accounts without one, and the tree keeps the
	// traversable-but-unlistable parents it has always had.
	if i.PerApp {
		if g, err := user.LookupGroup(name); err == nil {
			if ggid, err := strconv.Atoi(g.Gid); err == nil {
				a.GroupGID, a.GroupName = ggid, name
			}
		}
	}
	return a, nil
}

// accountName is the per-app account for slug: prefix+slug whenever that fits
// a username's 32 characters — every account that already exists — and
// otherwise as much of the slug as fits, a dash, and six hex characters of its
// hash. A slug is the app's address and is not limited to 23 characters; the
// account was, and an app whose slug overran it built and then failed at its
// first start, after create had said yes.
func accountName(prefix, slug string) string {
	name := prefix + slug
	if len(name) <= 32 {
		return name
	}
	sum := sha256.Sum256([]byte(slug))
	keep := 32 - len(prefix) - 7
	if keep < 1 {
		return name // a prefix this long is refused by name in createAccount
	}
	return prefix + strings.TrimRight(slug[:keep], "-") + "-" + hex.EncodeToString(sum[:])[:6]
}

// lookupOrCreate finds the account, creating it when AutoCreate allows.
//
// The lock is held across the lookup and the create: two starts of one app
// (the app and a host job, or a Remount racing a deploy) would otherwise both
// find the account missing, and the second useradd would fail on the first's.
func (i Isolation) lookupOrCreate(name string) (*user.User, error) {
	defer lockShadow()()
	if i.AutoCreate && i.PerApp && i.Numbers != nil {
		n, err := i.Numbers(name)
		if err != nil {
			return nil, fmt.Errorf("the recorded number of app account %q: %w", name, err)
		}
		return i.ensureNumbered(name, n)
	}
	u, err := user.Lookup(name)
	if err == nil {
		return u, nil
	}
	if !i.AutoCreate {
		return nil, fmt.Errorf("app account %q does not exist (set APP_USER_AUTOCREATE=1 to create accounts at deploy time): %w", name, err)
	}
	return i.createAccount(name)
}

// createAccount provisions a locked-down system account for an app. It needs
// root (or CAP_CHOWN + the shadow suite); failure fails the deploy. The caller
// holds lockShadow.
func (i Isolation) createAccount(name string) (*user.User, error) {
	if !validUsername.MatchString(name) {
		return nil, fmt.Errorf("app account name %q is not a valid username (32 chars max; shorten the slug or prefix)", name)
	}
	group := i.Group
	if group == "" {
		group = "toft-apps"
	}
	if _, err := user.LookupGroup(group); err != nil {
		if out, err := runShadow("groupadd", "--system", group); err != nil {
			return nil, fmt.Errorf("creating app group %q: %s: %w", group, out, err)
		}
	}
	// A group of this app's own, and the account joins it as a SUPPLEMENTARY
	// member while the shared group stays primary. That split is the whole
	// design: a file-permission check honours the process's full supplementary
	// list, so this group can gate the app's directories, while
	// `iptables -m owner --gid-owner` matches the *effective* GID only, so the
	// metadata-egress rule keyed on the shared group goes on matching. Giving
	// each app its own primary group would gate the directories just as well
	// and silently stop every app process matching that firewall rule.
	//
	// It takes the account's own name (Q4): one name to reason about in `id`
	// output, and one fewer setting nobody would set.
	if i.PerApp {
		if _, err := user.LookupGroup(name); err != nil {
			if out, err := runShadow("groupadd", "--system", name); err != nil {
				return nil, fmt.Errorf("creating per-app group %q: %s: %w", name, out, err)
			}
		}
	}
	args := []string{"--system",
		"--gid", group,
		"--no-create-home", "--home-dir", "/nonexistent",
		"--shell", "/usr/sbin/nologin",
	}
	if i.PerApp {
		args = append(args, "--groups", name)
	}
	out, err := runShadow("useradd", append(args, name)...)
	if err != nil {
		return nil, fmt.Errorf("creating app account %q: %s: %w", name, out, err)
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("app account %q missing after creation: %w", name, err)
	}
	return u, nil
}

// ensureNumbered makes the account and its per-app group exist at exactly n —
// created at it, or moved to it where this host's account database has them at
// another number (every container that predates the record, ADR §122). The
// caller holds lockShadow.
//
// **A number another name holds is refused, never shared.** n is the
// database's and unique there; a host account already at it is one the
// operator made by hand or an install that ran with an overlapping range, and
// starting the app as it would hand that account's files to the app.
func (i Isolation) ensureNumbered(name string, n int) (*user.User, error) {
	if !validUsername.MatchString(name) {
		return nil, fmt.Errorf("app account name %q is not a valid username (32 chars max; shorten the slug or prefix)", name)
	}
	num := strconv.Itoa(n)
	group := i.Group
	if group == "" {
		group = "toft-apps"
	}
	if _, err := user.LookupGroup(group); err != nil {
		if out, err := runShadow("groupadd", "--system", group); err != nil {
			return nil, fmt.Errorf("creating app group %q: %s: %w", group, out, err)
		}
	}

	if g, err := user.LookupGroupId(num); err == nil && g.Name != name {
		return nil, fmt.Errorf("gid %d is app account %q's, and this host has it as group %q — "+
			"move %q off that number or set APP_ACCOUNT_ID_RANGE away from it", n, name, g.Name, g.Name)
	}
	switch g, err := user.LookupGroup(name); {
	case err != nil:
		if out, err := runShadow("groupadd", "--system", "--gid", num, name); err != nil {
			return nil, fmt.Errorf("creating per-app group %q at gid %d: %s: %w", name, n, out, err)
		}
	case g.Gid != num:
		if out, err := runShadow("groupmod", "--gid", num, name); err != nil {
			return nil, fmt.Errorf("moving per-app group %q to gid %d: %s: %w", name, n, out, err)
		}
	}

	if u, err := user.LookupId(num); err == nil && u.Username != name {
		return nil, fmt.Errorf("uid %d is app account %q's, and this host has it as user %q — "+
			"move %q off that number or set APP_ACCOUNT_ID_RANGE away from it", n, name, u.Username, u.Username)
	}
	switch u, err := user.Lookup(name); {
	case err != nil:
		args := []string{"--system", "--uid", num, "--gid", group, "--groups", name,
			"--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", name}
		if out, err := runShadow("useradd", args...); err != nil {
			return nil, fmt.Errorf("creating app account %q at uid %d: %s: %w", name, n, out, err)
		}
	case u.Uid != num:
		if out, err := runShadow("usermod", "--uid", num, name); err != nil {
			return nil, fmt.Errorf("moving app account %q to uid %d: %s: %w", name, n, out, err)
		}
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("app account %q missing after creation: %w", name, err)
	}
	return u, nil
}

// AccountName is the per-app account an app with this slug runs as, or "" when
// apps do not get accounts of their own.
func (i Isolation) AccountName(slug string) string {
	if !i.PerApp || i.UserPrefix == "" {
		return ""
	}
	return accountName(i.UserPrefix, slug)
}

// RemoveAccount deletes a deleted app's auto-created account and its per-app
// group from this host. Only an account Toft made: a pre-provisioned one is the
// operator's, and the shared RunAsUser account is every app's. Absent is not an
// error — a container that never started the app never had it. The number
// stays recorded (database.RetireAppAccount) and is never handed out again.
func (i Isolation) RemoveAccount(slug string) error {
	name := i.AccountName(slug)
	if name == "" || !i.AutoCreate {
		return nil
	}
	defer lockShadow()()
	if _, err := user.Lookup(name); err == nil {
		if out, err := runShadow("userdel", name); err != nil {
			return fmt.Errorf("removing app account %q: %s: %w", name, out, err)
		}
	}
	if _, err := user.LookupGroup(name); err == nil {
		if out, err := runShadow("groupdel", name); err != nil {
			return fmt.Errorf("removing per-app group %q: %s: %w", name, out, err)
		}
	}
	return nil
}

func command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Env = BaseEnv()
	return cmd
}

// ownTree hands a release directory to the app account: every entry is chowned
// to it, and the top-level directory is made 0700. Symlinks (venvs are full of
// them) are re-owned without being followed.
//
// That 0700 is defence in depth and **not** where per-app secrecy lives, which
// is what this comment used to claim. The app owns this directory, so chmod on
// it is an ordinary permitted operation and one line of a compromised
// dependency undoes it. The boundary is gateAppDir's 0710 on a root-owned
// parent, which the app cannot chown, chmod, or traverse without being in the
// group.
func ownTree(dir string, a *AppAccount) error {
	// The reconciler hands over the app's `current` symlink rather than the
	// release directory itself. WalkDir does not follow a symlink root — it
	// would chown the link and silently skip the tree, leaving every file on
	// whatever UID owned it before. That matters because autocreated app
	// accounts live in the container's /etc/passwd and die with it, so after a
	// container replacement the same numeric UIDs come back in a different
	// order; this re-own on every hand-over is what makes that reshuffle
	// harmless at spawn time.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolving release dir: %w", err)
	}
	// **The walk is confined to the tree by an os.Root.** This runs as root over
	// a directory the app account owns, and on a restart the old process is
	// still alive and can swap a subdirectory for a link to /etc between the
	// listing and the chown. Through a Root every listing and every Lchown
	// resolves inside dir and refuses a link that leaves it, so the worst a
	// swap does is make one entry fail to be re-owned.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("opening release dir: %w", err)
	}
	defer root.Close()
	return ownRoot(root, a)
}

// ownSubtree is ownTree for a directory that lies **inside a tree the app can
// write**, reached through a trusted root: parent is a directory only the
// platform controls (apps/<id>), rel is the app-writable child. Resolving rel
// through parent means a child the app has swapped for a link out of parent is
// refused rather than followed — EvalSymlinks on the joined path would follow it
// and hand an arbitrary directory to the app.
func ownSubtree(parent *os.Root, rel string, a *AppAccount) error {
	sub, err := parent.OpenRoot(rel)
	if err != nil {
		return fmt.Errorf("opening %s: %w", rel, err)
	}
	defer sub.Close()
	return ownRoot(sub, a)
}

// ownRoot re-owns every entry of root to the account, without following links,
// and makes the top directory 0700.
//
// **An entry the account already owns is read, not written** (ADR §123). The
// walk runs on every start and every wake, and on the second start of a release
// nearly every entry is already right: an R library is thousands of files, and a
// chown to the same owner is still an inode write each — on network storage a
// round-trip each. The lstat the walk does anyway is what decides, so a tree
// whose numbers moved under it (a container replacement reshuffling accounts) is
// re-owned exactly as before. Nothing is remembered between walks: a marker
// would live in a tree the app can write, and the only thing to trust is the
// inode itself.
func ownRoot(root *os.Root, a *AppAccount) error {
	err := fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			// Removed, or swapped for a link that leaves the tree, mid-walk.
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || isEscape(err) {
				return nil
			}
			return err
		}
		if ownedBy(d, a) {
			return nil
		}
		if err := root.Lchown(rel, a.UID, a.GID); err != nil {
			if errors.Is(err, fs.ErrNotExist) || isEscape(err) {
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("chown to %s (isolation needs Toft to run as root): %w", a.Username, err)
	}
	if err := root.Chmod(".", 0700); err != nil {
		return fmt.Errorf("restricting release dir: %w", err)
	}
	return nil
}

// ownedBy reports whether the entry already belongs to the account, read
// without following a link. Any failure to read is "no", so the caller chowns.
func ownedBy(d fs.DirEntry, a *AppAccount) bool {
	info, err := d.Info()
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == a.UID && int(st.Gid) == a.GID
}

// isEscape reports whether an os.Root refused a path because it resolves
// outside the root — the signature of a link planted mid-walk.
func isEscape(err error) bool {
	return err != nil && strings.Contains(err.Error(), "path escapes from parent")
}

// gateAppDir puts the app's tree behind a directory the app does not own.
//
// apps/<id> and apps/<id>/releases become root-owned 0710 with the app's own
// supplementary group: traversal for this app's account, nothing at all for any
// other, and no listing for either. Reaching a release therefore means passing
// two directories the app cannot chmod, which is what the release directory's
// own 0700 was wrongly believed to be doing on its own — a mode bit on a
// directory its subject owns is a permission the subject holds, and release IDs
// not being enumerable through a 0711 parent was the only thing actually
// standing in the way. That was a guess about path discovery doing work a
// permission check was credited with.
//
// Applied at every hand-over rather than once at directory creation, for the
// same reason ownTree is: autocreated accounts die with a container and their
// UIDs and GIDs come back in a different order, so the only durable statement
// is the one re-made at spawn. It is also what covers a release restored from
// the artifact store, whose directories are created by a caller with no account
// in hand.
//
// A zero GroupGID means this install has no per-app group — a shared account,
// or accounts an operator provisioned without one. The parents keep the 0711
// they have always had: no worse than before, and no false claim that a gate is
// in force. It is logged, because a boundary that quietly does not apply is the
// failure this whole change is about.
func gateAppDir(appsDir, appID string, a *AppAccount) error {
	if appsDir == "" || appID == "" || a == nil {
		return nil
	}
	if a.GroupGID == 0 {
		log.Printf("isolation: %s has no per-app group; apps/%s stays traversable by every app account",
			a.Username, appID)
		return nil
	}
	for _, dir := range []string{
		filepath.Join(appsDir, appID),
		filepath.Join(appsDir, appID, "releases"),
		// A host job's directory lives under runs/, which is created by the
		// platform and was left 0700 root: the run's own directory was handed
		// to the app account and its identity file was unreachable through
		// the parent. Absent until the first run, and skipped while it is.
		filepath.Join(appsDir, appID, "runs"),
	} {
		if _, err := os.Stat(dir); os.IsNotExist(err) && filepath.Base(dir) == "runs" {
			continue
		}
		if err := gateDir(dir, a); err != nil {
			return err
		}
	}
	return nil
}

// gateDir makes one directory traversable by the app's group alone — root's,
// 0710. gateAppDir's rule for each level it covers, and the rule for an app's
// level under JOB_RUN_DIR, which lies outside APPS_DIR.
func gateDir(dir string, a *AppAccount) error {
	if a == nil || a.GroupGID == 0 {
		return nil
	}
	if err := os.Chown(dir, 0, a.GroupGID); err != nil {
		return fmt.Errorf("gating %s to group %s: %w", dir, a.GroupName, err)
	}
	if err := os.Chmod(dir, 0710); err != nil {
		return fmt.Errorf("restricting %s: %w", dir, err)
	}
	return nil
}

// AppAccounts names which OS identity deployed apps run as in shared mode.
// It exists so the startup warning and the posture Admin → Info reports cannot
// give an install two different answers about what it is.
type AppAccounts string

const (
	// AccountsPerApp — one account per app: apps are separated from the
	// platform and from each other. The shipped posture.
	AccountsPerApp AppAccounts = "per-app"
	// AccountsShared — one account for every app: separated from the platform,
	// not from each other.
	AccountsShared AppAccounts = "shared"
	// AccountsPlatform — no separation at all. Every app can read the platform
	// database, every other app's files, and the process manager's socket.
	AccountsPlatform AppAccounts = "platform"
)

// AccountsFor names the posture the two settings that decide it describe.
//
// Package-level rather than only a method, because the caller that has to
// *report* the posture holds configuration rather than a resolved Isolation,
// and building a half-populated struct to ask a question is how two answers
// drift apart. PerApp wins over RunAsUser because resolve does.
func AccountsFor(runAsUser string, perApp bool) AppAccounts {
	switch {
	case perApp:
		return AccountsPerApp
	case runAsUser != "":
		return AccountsShared
	default:
		return AccountsPlatform
	}
}

// Accounts is AccountsFor for a resolved Isolation.
func (i Isolation) Accounts() AppAccounts { return AccountsFor(i.RunAsUser, i.PerApp) }

// Enabled reports whether apps are separated from the platform's own identity.
func (i Isolation) Enabled() bool {
	return i.Accounts() != AccountsPlatform
}

// Account is the account an app's processes run as — and, since ADR §29's
// build rule, the account its builds run as too. nil for "the platform's own".
func (i Isolation) Account(slug string) (*AppAccount, error) { return i.resolve(slug) }

// HandOver gives a tree to an account through an os.Root, as a start does
// (ownTree). Exported for the builder, which hands a release to the account
// its build commands are about to run as.
//
// **And the two levels above a release, gated as a start gates them.** A build
// writes as the account into apps/<id>/releases/<rid>, and only a start ran
// gateAppDir: after an account changed number the app directory was still gated
// to the old group, and a deploy's first write as the account was refused
// before any start could fix it (drills on trytoft, 2026-10-23).
func HandOver(dir string, a *AppAccount) error {
	if err := ownTree(dir, a); err != nil {
		return err
	}
	releases := filepath.Dir(filepath.Clean(dir))
	if filepath.Base(releases) != "releases" {
		return nil
	}
	if err := gateDir(filepath.Dir(releases), a); err != nil {
		return err
	}
	return gateDir(releases, a)
}

// SpawnCommand is the argv that runs name with args behind the umask stage
// (spawnArgs). It drops nothing by itself: the caller execs it with the
// account's Credentials set on the clone, the way the agent places a workload.
//
// **Never through a privilege-dropping helper.** The argv used to start with
// `setpriv`, a dynamically linked program exec'd *as root* with the build's
// whole environment — the app's own variables among them — so a loader
// variable (`LD_PRELOAD`) set in `toft.toml [env]` ran the repository's code as
// the platform before the drop (security review 2026-11-10, E-1). Set on the
// clone, the first program to see that environment already runs as the account.
func SpawnCommand(name string, args ...string) []string {
	return append(append(spawnArgs(), name), args...)
}

// Credentials is the uid, primary gid and whole group list a process run as a
// is given — the list the agent sets on a workload's clone (accountGroups).
func (a *AppAccount) Credentials() (uid, gid int, groups []int, err error) {
	groups, err = accountGroups(a)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("reading %s's groups: %w", a.Username, err)
	}
	return a.UID, a.GID, groups, nil
}
