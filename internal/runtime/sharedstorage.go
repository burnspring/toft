package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Durable storage for shared-mode workloads.
//
// The rules here are the same three that govern the isolated half, answered by
// a different mechanism rather than excepted:
//
//   - **The durable resource always pre-exists**, and so does the mount. An
//     operator mounts a file system on the host and declares it as a *root*;
//     Toft creates a directory under it and never a mount. A privileged
//     mount from the platform binary would need CAP_SYS_ADMIN, in a product
//     whose whole posture is that the credential lives below it.
//   - **Containment is what is mounted, not what is permitted** — and here the
//     second half of that sentence needs care, because this containment *is* a
//     permission. It is the one the release directory already uses: a
//     root-owned directory the app does not own and therefore cannot chmod,
//     gated by a system group the app is in only while it is mapped. The app
//     cannot widen it, and an app that is not mapped cannot traverse it at all.
//   - **No workload holds a storage credential**, which is trivially true: there
//     is nothing to authenticate to.
//
// What a *whole* resource does not buy is per-app containment within it. Two
// apps mapped to one share a directory, and inside it the operating system tells
// them apart only by file ownership and umask — the accepted app↔app risk landing
// on data. A *partitioned* resource is the answer to that: each folder is its own
// gate, and the directories above it can be passed through and nothing more.
// See docs/invariants.md § App storage.

// StorageRoot is a directory an operator mounted and declared.
//
// ReadOnly is a fact about the mount, never something the platform applies: a
// read-only level in shared mode is a *second operator mount* of the same file
// system, because POSIX ACLs would make the platform the enforcer of a
// permission on a file system it does not own.
type StorageRoot struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// ReadOnly is a fact about the mount, and the admin surface shows it
	// because it is what decides which levels a resource under this root can be
	// mapped at.
	ReadOnly bool `json:"readOnly,omitempty"`
}

// ParseStorageRoots reads the operator's declaration.
//
// The form is `name=/path` or `name=/path:ro`, comma-separated. Every failure
// is an error rather than a skipped entry: a root that silently did not parse
// is a resource that cannot be provisioned, discovered at the first deploy that
// needed it.
func ParseStorageRoots(spec string) ([]StorageRoot, error) {
	out := []StorageRoot{}
	seen := map[string]bool{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, rest, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		rest = strings.TrimSpace(rest)
		if !ok || name == "" || rest == "" {
			return nil, fmt.Errorf("storage root %q is not name=/path[:ro]", entry)
		}
		root := StorageRoot{Name: name}
		if trimmed, found := strings.CutSuffix(rest, ":ro"); found {
			root.Path, root.ReadOnly = strings.TrimSpace(trimmed), true
		} else {
			root.Path = rest
		}
		if !filepath.IsAbs(root.Path) {
			return nil, fmt.Errorf("storage root %q must name an absolute path, got %q", name, root.Path)
		}
		if seen[name] {
			return nil, fmt.Errorf("storage root %q is declared twice", name)
		}
		seen[name] = true
		out = append(out, root)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// AccountIDMax is the highest app account number APP_ACCOUNT_ID_RANGE may
// reach: 2097151 is the largest uid a ustar header's seven octal digits hold,
// so every tar a release passes through records it without an extension.
const AccountIDMax = 2097151

// ParseAccountIDRange reads APP_ACCOUNT_ID_RANGE: `lo-hi`, inclusive, the
// numbers auto-created per-app accounts take as uid and per-app gid (ADR §122).
// Empty is the default (database.DefaultAccountIDLow) and is returned as 0, 0.
//
// Not ParseGIDRange: a gate is a group only, and that range keeps the 16-bit
// ceiling older tools treat as special; an account number is never reused, so
// the range has to outlast every app an install will ever delete, and uid_t is
// 32 bits. It may cross 65534 and 65535 only by not containing them — those are
// nobody's and the 16-bit -1.
func ParseAccountIDRange(spec string) (low, high int, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, 0, nil
	}
	lo, hi, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, fmt.Errorf("%q is not lo-hi", spec)
	}
	if low, err = strconv.Atoi(strings.TrimSpace(lo)); err != nil {
		return 0, 0, fmt.Errorf("%q: the low end is not a number", spec)
	}
	if high, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
		return 0, 0, fmt.Errorf("%q: the high end is not a number", spec)
	}
	switch {
	case low < 1000:
		return 0, 0, fmt.Errorf("%q starts below 1000, where system accounts are allocated", spec)
	case high > AccountIDMax:
		return 0, 0, fmt.Errorf("%q ends above %d, the largest uid a plain tar header holds", spec, AccountIDMax)
	case high < low:
		return 0, 0, fmt.Errorf("%q ends before it starts", spec)
	case low <= 65535 && high >= 65534:
		return 0, 0, fmt.Errorf("%q contains 65534 or 65535, which are nobody's and the 16-bit -1", spec)
	}
	return low, high, nil
}

// ParseGIDRange reads SHARED_STORAGE_GID_RANGE: `lo-hi`, inclusive, the
// numbers storage gates are allocated from. Empty is the default, which the
// database owns (database.DefaultGateGIDLow), and is returned as 0, 0.
//
// Strict, like the roots: a range that silently did not parse is a gate that
// cannot be numbered, discovered at the first create that needed one. Below
// 1000 is refused because that is where every distribution's system groups —
// and this platform's own autocreated ones — are allocated, and 65534 and
// above because that is nobody's and the 16-bit boundary older tools still
// treat as special.
func ParseGIDRange(spec string) (low, high int, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, 0, nil
	}
	lo, hi, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, fmt.Errorf("%q is not lo-hi", spec)
	}
	if low, err = strconv.Atoi(strings.TrimSpace(lo)); err != nil {
		return 0, 0, fmt.Errorf("%q: the low end is not a number", spec)
	}
	if high, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
		return 0, 0, fmt.Errorf("%q: the high end is not a number", spec)
	}
	switch {
	case low < 1000:
		return 0, 0, fmt.Errorf("%q starts below 1000, where system groups are allocated", spec)
	case high >= 65534:
		return 0, 0, fmt.Errorf("%q reaches 65534 or above, which is nobody's", spec)
	case high < low:
		return 0, 0, fmt.Errorf("%q ends before it starts", spec)
	}
	return low, high, nil
}

// SharedStorage provisions durable storage under the operator's roots.
//
// It implements StorageProvisioner, so the admin surface creates and releases a
// shared-mode resource through exactly the path it uses for a cluster one.
type SharedStorage struct {
	roots   []StorageRoot
	appsDir string
	// storeBacking is the backing a store on this install has — the one the
	// platform's signer speaks — and empty when it has no signer, which is the
	// whole of "stores are not offered". A store here is never mounted, so this
	// is the only thing the provisioner knows about one.
	storeBacking string
	// ensureGroup and setGates are the shadow-suite calls, injectable so a
	// test can assert what would have been run on a box where it cannot run
	// them. ensureGroup makes one gate exist at its recorded number; setGates
	// makes an account's Toft-owned groups exactly the given set.
	ensureGroup func(g Gate) error
	setGates    func(account *AppAccount, gates []string) error
	// chown and own are the two privileged filesystem calls here. Injectable
	// for the same reason the group calls are: the containment this file builds
	// needs root, and a test that could only run as root is a test that does not
	// run. What is asserted is that the right call was made about the right
	// path, which is the half a bug lives in.
	//
	// chown re-owns one folder inside a resource, through the os.Root that
	// confines the walk (path is only for the test to read); own re-owns the
	// mount-point tree under apps/<id>, through the root-owned apps/<id>.
	chown func(root *os.Root, rel, path string, uid, gid int) error
	own   func(parent *os.Root, rel string, a *AppAccount) error
	// regroup moves everything beneath a partitioned folder onto its gate,
	// when the folder's own group shows the gate has changed under it (path
	// is only for the test to read); regroupSubtree is the real one.
	regroup func(root *os.Root, rel, path string, gid int) error
	// attaching holds one mutex per app id. A service start and several job
	// runs of one app attach at the same moment — the dispatcher starts runs
	// concurrently — and every one of them writes the same links under
	// apps/<id>/storage.
	attaching sync.Map
}

// NewSharedStorage returns a provisioner for the declared roots and, when the
// platform can sign into an object store, for stores in that backing — or nil
// when there is neither.
//
// **Nil is the whole of "not configured".** An install with no root and no
// signer offers no shared-mode storage at all: the admin surface does not offer
// it, and a deploy that has mappings anyway is refused with the reason. Absent,
// not dormant. The two halves are independent: a root offers volumes, a signer
// offers stores, and neither implies the other.
func NewSharedStorage(roots []StorageRoot, appsDir, storeBacking string) *SharedStorage {
	if len(roots) == 0 && storeBacking == "" {
		return nil
	}
	return &SharedStorage{
		roots:        roots,
		appsDir:      appsDir,
		storeBacking: storeBacking,
		ensureGroup:  systemGroupEnsure,
		setGates:     systemSetGates,
		chown: func(root *os.Root, rel, _ string, uid, gid int) error {
			// On a descriptor of the directory itself, opened with no link
			// followed: Lstat-then-Chown by name was two operations, and
			// between them a member of the gate could swap the folder for a
			// link to another app's file inside the resource, which root then
			// chowned to the gate (code review 2026-11-11, Q2-4).
			f, err := openDirNoFollow(root, rel)
			if err != nil {
				return err
			}
			defer f.Close()
			return f.Chown(uid, gid)
		},
		own: ownSubtree,
		regroup: func(root *os.Root, rel, _ string, gid int) error {
			return regroupSubtree(root, rel, gid)
		},
	}
}

// Roots lists what the operator declared, for the admin surface's create form.
func (s *SharedStorage) Roots() []StorageRoot {
	if s == nil {
		return nil
	}
	return append([]StorageRoot(nil), s.roots...)
}

// Root resolves one by name.
func (s *SharedStorage) Root(name string) (StorageRoot, bool) {
	if s == nil {
		return StorageRoot{}, false
	}
	for _, r := range s.roots {
		if r.Name == name {
			return r, true
		}
	}
	return StorageRoot{}, false
}

// StorageKinds is what an install with no cluster can provision.
//
// A volume where an operator declared a root. A store where the platform can
// sign: on a shared host it is never mounted — there is no CSI driver to mount
// one with — and is reached over signed URLs instead, by people and by the apps
// it is mapped to.
func (s *SharedStorage) StorageKinds() []string {
	kinds := []string{}
	if s.HasRoots() {
		kinds = append(kinds, MountVolume)
	}
	if s != nil && s.storeBacking != "" {
		kinds = append(kinds, MountStore)
	}
	return kinds
}

// StorageBackings is what can be underneath one.
//
// `host` — a file system the operator mounted, of a kind Toft does not
// know — and `s3files`, for the operator who mounted an S3 Files file system
// and wants people's transfers to go straight to the bucket behind it. Never
// `efs`: an EFS-backed file store is an access point bound by a cluster, and
// there is no cluster here.
//
// **`s3files` only beside an S3 signer.** Naming the bucket buys exactly one
// thing a host root does not already have — signed transfers that bypass the
// platform — and without a signer that speaks S3 it buys nothing but a field
// that can be wrong. So an install with no such signer has one volume backing,
// which the form states rather than asks.
//
// A store has exactly one: the backing the signer speaks. A store whose bytes
// the signer cannot reach is a Files page that fails at the first listing.
func (s *SharedStorage) StorageBackings(kind string) []string {
	switch {
	case kind == MountVolume && s.HasRoots() && s.storeBacking == BackingMountpoint:
		return []string{BackingHost, BackingS3Files}
	case kind == MountVolume && s.HasRoots():
		return []string{BackingHost}
	case kind == MountStore && s != nil && s.storeBacking != "":
		return []string{s.storeBacking}
	}
	return []string{}
}

// StorageGroupName is the system group that gates one resource's directory.
//
// Per *resource*, not per app: it is what an app joins to reach the directory,
// so an app that is not mapped is not in it and cannot traverse the directory
// at all. That is the control this design has, and it is the only one — see the
// note at the top of this file about what happens inside the directory.
func StorageGroupName(slug string) string { return "toft-store-" + slug }

// FolderGateName is the gate on one folder of a partitioned resource:
// `toft-sf-` and twelve hex characters of the SHA-256 of slug/folder. Twenty
// characters whatever the slug and folder, under the 32 groupadd takes. The slug
// rather than the resource's id, because the slug is what names the directory:
// a resource created again onto the same slug is created onto the same folders,
// and their files carry the numbers these names were given.
func FolderGateName(slug, folder string) string {
	sum := sha256.Sum256([]byte(slug + "/" + folder))
	return "toft-sf-" + hex.EncodeToString(sum[:])[:12]
}

// EnsureStorage creates one resource's directory under its root, closed.
//
// It makes the directory and nothing else: `0770 root:root`, which no app can
// enter. **The group and the gate are Attach's**, made at every start from the
// number the database recorded, because this runs at admin time on whichever
// host served the request and the group it made there dies with the container.
// A directory that exists is left exactly as it is — the attach routes call
// this on every mapping write, and re-owning a live directory to root would
// shut every running app on it out until its next start.
//
// Idempotent, for the reason the cluster provisioner is: an administrator
// retrying a half-failed create must not end up with a second directory. A
// directory nobody made through Toft never reaches here from the create, which
// refuses one first (ExistingDirectory).
func (s *SharedStorage) EnsureStorage(ctx context.Context, res StorageResource) error {
	// A store creates nothing here: the bucket pre-exists, Toft never creates
	// one, and a shared host binds nothing to reach it.
	if res.Kind == MountStore {
		return nil
	}
	dir, _, err := s.resourceDir(res)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("shared storage: %s is not a directory", dir)
		}
		return nil
	}
	if err := os.Mkdir(dir, 0o770); err != nil {
		return fmt.Errorf("shared storage: creating %s: %w", dir, err)
	}
	// Mkdir's mode is masked by the process umask — 0022 under sudo or a
	// service manager, which would leave the directory 0750 and the gate's
	// apps unable to write until the first start re-moded it.
	if err := os.Chmod(dir, 0o770); err != nil {
		return fmt.Errorf("shared storage: closing %s: %w", dir, err)
	}
	return nil
}

// ExistingDirectory reports whether anything is already at the resource's path,
// and the path. The create asks before it writes the row, because EnsureStorage
// keeps what it finds and DestroyStorage removes it: a directory Toft did not
// make would otherwise become a resource's, and go with its delete.
func (s *SharedStorage) ExistingDirectory(res StorageResource) (string, bool, error) {
	if res.Kind == MountStore {
		return "", false, nil
	}
	dir, _, err := s.resourceDir(res)
	if err != nil {
		return "", false, err
	}
	if _, err := os.Lstat(dir); err == nil {
		return dir, true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return dir, false, fmt.Errorf("shared storage: %s: %w", dir, err)
	}
	return dir, false, nil
}

// The two directory shapes under a root, and the only two.
//
//   - gatedMode is a gate: `2770 root:<gate>`. The app does not own it, so it
//     cannot chmod it — the property the release directory's gate already
//     relies on, and the reason a mode bit is load-bearing here where it would
//     not be on a directory the app owned. Setgid, so what an app creates inside
//     takes the gate's group rather than the app's primary one, and two apps on
//     one gate can share what they write.
//   - passMode is a partitioned resource's own directory and the components
//     between it and a folder: `0711 root:root`. Anyone may pass through and
//     nobody may list or write, because nothing lives there — a partitioned
//     resource is mapped only by folder. The folder is the gate.
const (
	gatedMode = 0o770 | os.ModeSetgid
	passMode  = 0o711
)

// ensureGated makes a resource's own directory exist in one of the two shapes:
// gated by gid at gatedMode, or at passMode with gid 0.
//
// It sits directly under an operator root no app account can write, so a
// path-based check of it is safe. What is inside it is not: see
// ensureGatedFolder.
func (s *SharedStorage) ensureGated(dir string, gid int, mode os.FileMode) error {
	// The operator's root has to be there already — it is a mount, and a
	// mount that is not there is not something to make. MkdirAll made the
	// mountpoint, or the whole root, on the local disk when a reboot left the
	// file system unmounted, and the app started with an empty "durable"
	// directory whose writes were shadowed when the mount came back (code
	// review 2026-11-11, Q2-1). Only the resource's own directory is made.
	parentDir := filepath.Dir(dir)
	if fi, err := os.Lstat(parentDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("shared storage: the root %s is not mounted or does not exist; nothing is created in its place", parentDir)
	}
	if err := os.Mkdir(dir, 0o770); err != nil && !os.IsExist(err) {
		return fmt.Errorf("shared storage: creating %s: %w", dir, err)
	}
	if fi, err := os.Lstat(dir); err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("shared storage: %s is not a directory", dir)
	}
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return fmt.Errorf("shared storage: opening %s: %w", filepath.Dir(dir), err)
	}
	defer parent.Close()
	name := filepath.Base(dir)
	if err := s.chown(parent, name, dir, 0, gid); err != nil {
		return fmt.Errorf("shared storage: gating %s to gid %d: %w", dir, gid, err)
	}
	if err := chmodDirNoFollow(parent, name, mode); err != nil {
		return fmt.Errorf("shared storage: restricting %s: %w", dir, err)
	}
	return nil
}

// ensureGatedFolder makes a folder inside a resource exist behind its gate,
// creating each missing component as it goes.
//
// On a whole resource every component is gated by the resource's gate, because
// a folder under a gate the gate does not cover is not gated. On a partitioned
// one only the folder itself is: the components above it are passMode, so an
// app reaches its own folder through them and can see nothing beside it.
//
// **Everything under a gate is writable by every app on it**, so any component
// may be a link an app planted. The walk is therefore confined to the resource
// directory by an os.Root — a component that resolves outside it is refused
// rather than followed — and a component that is a link at all is refused, so
// root never chowns or chmods through one.
func (s *SharedStorage) ensureGatedFolder(resourceDir, folder string, gate Gate, partitioned bool) error {
	root, err := os.OpenRoot(resourceDir)
	if err != nil {
		return fmt.Errorf("shared storage: opening %s: %w", resourceDir, err)
	}
	defer root.Close()
	parts := []string{}
	for _, part := range strings.Split(filepath.ToSlash(folder), "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	cur := ""
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		full := filepath.Join(resourceDir, cur)
		fi, err := root.Lstat(cur)
		existed := err == nil
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Two apps mapped to one folder can start at the same moment.
			if err := root.Mkdir(cur, 0o770); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("shared storage: creating %s: %w", full, err)
			}
		case err != nil:
			return fmt.Errorf("shared storage: %s: %w", full, err)
		case fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
			return fmt.Errorf("shared storage: %s is not a plain directory, so it will not be gated", full)
		}
		gid, mode := gate.GID, gatedMode
		if partitioned && i < len(parts)-1 {
			gid, mode = 0, passMode
		}
		// A partitioned folder re-mapped at another depth (teams -> teams/a, or
		// back) is gated by a new number, and what people uploaded there still
		// carries the old one, which the app on the new gate cannot open. The
		// folder's own group is the signal: its contents follow it before it
		// is re-gated, so a walk cut short is retried at the next start, and a
		// folder already on its gate is not walked at all (ADR §123).
		if partitioned && i == len(parts)-1 && existed && fileGID(fi) != gate.GID {
			if err := s.regroup(root, cur, full, gate.GID); err != nil {
				return fmt.Errorf("shared storage: moving what is in %s onto %s: %w", full, gate.Group, err)
			}
		}
		if err := s.chown(root, cur, full, 0, gid); err != nil {
			return fmt.Errorf("shared storage: gating %s to %s: %w", full, gate.Group, err)
		}
		if err := chmodDirNoFollow(root, cur, mode); err != nil {
			return fmt.Errorf("shared storage: restricting %s: %w", full, err)
		}
	}
	return nil
}

// fileGID is the group a file carries, or -1 when the platform cannot say.
func fileGID(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Gid)
	}
	return -1
}

// regroupSubtree puts every entry beneath rel on gid, leaving owners alone.
//
// **Every entry under a gate is writable by every app on it**, so any of them
// may be a link an app planted: the walk runs on an os.Root opened on the
// folder, changes each entry with Lchown — never through a link — and an entry
// that vanishes or resolves outside the folder mid-walk is skipped, the way
// ownRoot does. An entry already on gid is read, not written.
func regroupSubtree(root *os.Root, rel string, gid int) error {
	judged, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if judged.Mode()&os.ModeSymlink != 0 || !judged.IsDir() {
		return fmt.Errorf("%s is not a plain directory", rel)
	}
	sub, err := root.OpenRoot(rel)
	if err != nil {
		return fmt.Errorf("opening %s: %w", rel, err)
	}
	defer sub.Close()
	// OpenRoot follows a link that stays inside root; the folder walked must
	// be the very directory just judged.
	if opened, err := sub.Stat("."); err != nil || !os.SameFile(judged, opened) {
		return fmt.Errorf("%s changed while it was being opened", rel)
	}
	return fs.WalkDir(sub.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || isEscape(err) {
				return nil
			}
			return err
		}
		if p == "." {
			return nil // the folder itself is chowned by the caller
		}
		if info, err := d.Info(); err == nil && fileGID(info) == gid {
			return nil
		}
		if err := sub.Lchown(p, -1, gid); err != nil {
			if errors.Is(err, fs.ErrNotExist) || isEscape(err) {
				return nil
			}
			return err
		}
		return nil
	})
}

// RemoveStorage releases a binding, which on a host root is nothing: a
// directory has no binding object, and a folder's gate stays recorded. It never
// deletes — DestroyStorage does, for the resource delete alone. It used to be
// this method that removed <root>/<slug>, and the mapping sweep, which calls it
// with a folder, took every folder of a partitioned resource with it.
func (s *SharedStorage) RemoveStorage(ctx context.Context, res StorageResource) error {
	return nil
}

// DestroyStorage deletes the resource's directory and everything in it.
//
// **Unlike a cluster's class, which keeps reclaimPolicy: Retain**, a resource
// under a host root is a directory Toft made, inside a mount the operator
// handed it for exactly this, and an administrator who deletes the resource
// means the files: a retained directory is storage nobody can reach and nobody
// is shown. The delete is type-to-confirm in the UI for that reason, and
// refused while any app still maps the resource. A read-only root is left alone
// — the file system would refuse, and the files were never Toft's to write —
// and so is a root this install no longer declares, which has no path at all.
//
// os.RemoveAll removes a symbolic link rather than following it, so a link an
// app planted inside the tree cannot carry the delete outside it. The gate's
// group stays on the host: it names nothing now and is recreated at its
// recorded number if the slug is used again.
func (s *SharedStorage) DestroyStorage(ctx context.Context, res StorageResource, _ []string) (DestroyReport, error) {
	if res.Kind == MountStore {
		return DestroyReport{}, nil
	}
	if strings.ContainsAny(res.Slug, `/\`) || res.Slug == "." || res.Slug == ".." || res.Slug == "" {
		return DestroyReport{}, fmt.Errorf("shared storage: %q is not a directory name", res.Slug)
	}
	root, ok := s.Root(res.Root)
	if !ok {
		return DestroyReport{LeftBehind: []LeftBehind{{
			What:   res.Root + "/" + res.Slug,
			Reason: "the storage root " + res.Root + " is not declared on this install, so its directory cannot be reached",
		}}}, nil
	}
	dir := filepath.Join(root.Path, res.Slug)
	if root.ReadOnly {
		return DestroyReport{LeftBehind: []LeftBehind{{
			What: dir, Reason: "the storage root " + res.Root + " is read-only",
		}}}, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return DestroyReport{}, fmt.Errorf("shared storage: removing %s: %w", dir, err)
	}
	return DestroyReport{RemovesData: true, Removed: []string{dir}}, nil
}

// VolumesGone is always true: a host root's destroy is synchronous.
func (s *SharedStorage) VolumesGone(context.Context, []string) (bool, error) { return true, nil }

// resourceDir is the one place a shared-mode resource's path is constructed,
// and the only place that reads a root by name.
func (s *SharedStorage) resourceDir(res StorageResource) (dir, group string, err error) {
	root, ok := s.Root(res.Root)
	if !ok {
		return "", "", fmt.Errorf("shared storage: %s names storage root %q, which this install does not declare — an operator sets SHARED_STORAGE_ROOTS", res.Slug, res.Root)
	}
	if res.Slug == "" {
		return "", "", fmt.Errorf("shared storage: a resource with no slug has no directory")
	}
	return filepath.Join(root.Path, res.Slug), StorageGroupName(res.Slug), nil
}

// Attach makes one app's mounts reachable and returns them with their paths
// resolved, which is what TOFT_STORAGE is built from.
//
// Two things happen per mount, and the order matters for neither because both
// are idempotent:
//
//   - The app's account joins the resource's group. A supplementary group is
//     read at spawn, so **a membership change lands at the next deploy** — which
//     is ADR §12a's "a mapping change takes effect on the next deployment"
//     falling out of the mechanism rather than being enforced on top of it.
//   - A symlink appears under the app's own directory at the mapping's path.
//     The mapping's path is absolute *inside a workload*, and a shared
//     workload's file system is the host's, so honouring it literally would
//     collide between apps and would need root at a place the platform has no
//     business writing. Resolving it under apps/<id>/storage keeps the shape
//     the administrator chose and puts it inside the tree the platform already
//     gates. It is a symlink and not a mount, because Toft mounts nothing.
func (s *SharedStorage) Attach(appID string, account *AppAccount, mounts []Mount) ([]Mount, error) {
	if s == nil {
		return mounts, nil
	}
	// **No mounts is still a membership to set.** An app whose last mapping was
	// removed must leave the group it was in, and this is the only place that
	// happens; returning early here was how an unmapped account kept its gate
	// for as long as the account existed.
	if len(mounts) == 0 {
		if gatedAccount(account) && s.HasRoots() {
			if err := s.setGates(account, nil); err != nil {
				return nil, err
			}
		}
		return mounts, nil
	}
	if appID == "" || s.appsDir == "" {
		return nil, fmt.Errorf("shared storage: no app directory to resolve mounts under")
	}
	mu, _ := s.attaching.LoadOrStore(appID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	// **Every root-side step below goes through apps/<id>**, which is root-owned
	// and which the app cannot write, as an os.Root. Below it the app owns
	// `storage` and everything in it from the previous start, so it can replace
	// any component with a link to anywhere on the host; resolving through the
	// Root refuses a link that leaves apps/<id> instead of letting root delete
	// and re-create an entry at its far end.
	appDir := filepath.Join(s.appsDir, appID)
	if err := os.MkdirAll(filepath.Join(appDir, "storage"), 0o750); err != nil {
		return nil, fmt.Errorf("shared storage: preparing %s: %w", appDir, err)
	}
	appRoot, err := os.OpenRoot(appDir)
	if err != nil {
		return nil, fmt.Errorf("shared storage: opening %s: %w", appDir, err)
	}
	defer appRoot.Close()
	// **Nested mounts cannot both be links**: the inner one would be made
	// inside the outer one's link, which points into the resource, and the
	// Root refuses that as escaping. The attach routes refuse the pair; a row
	// written before they did is named here rather than surfacing as
	// "path escapes from parent" (BB-048, fifth 2026-11-03 black-box list).
	links := map[string]bool{}
	for i, a := range mounts {
		if !a.NeedsRoot() {
			continue
		}
		links[mountLinkRel(a)] = true
		for _, b := range mounts[i+1:] {
			if b.NeedsRoot() && mountPathsNest(a.Path, b.Path) {
				return nil, fmt.Errorf("shared storage: %s is mounted at %s and %s at %s, one inside the other — "+
					"no two of an app's mount paths may sit one inside the other; detach one", a.Name, a.Path, b.Name, b.Path)
			}
		}
	}
	out := make([]Mount, 0, len(mounts))
	gates := []string{}
	for _, m := range mounts {
		// A store is not attached here: no group, no link. It is reached over
		// the platform's signed URLs, and its entry says so by having no path.
		if !m.NeedsRoot() {
			m.Path = ""
			out = append(out, m)
			continue
		}
		root, ok := s.Root(m.Root)
		if !ok {
			return nil, fmt.Errorf("shared storage: %s names storage root %q, which this install does not declare — an operator sets SHARED_STORAGE_ROOTS", m.Name, m.Root)
		}
		if err := checkRootLevel(root, m); err != nil {
			return nil, err
		}
		// **A gate needs an account that is this app's alone, with a group that
		// is too.** Gates are memberships of the account, set to exactly this
		// app's on every start (systemSetGates), so an account several apps
		// share would have its gates set to one app's at each start, taking the
		// others' away — and a gate one app was given would be open to every
		// other app on that account. The per-app group is what marks an account
		// as one app's (gatedAccount).
		if account != nil && !gatedAccount(account) {
			return nil, fmt.Errorf("shared storage: %s runs as %s, which has no group of its own, so it cannot be "+
				"given a gate — host storage needs an account per app (APP_USER_PER_APP), each with a group of its "+
				"own name", m.Name, account.Username)
		}

		// The gate is the database's: its name and, above all, its number. A
		// mount without one was assembled by something that did not read
		// storage_gates, and gating a directory to a number nobody recorded is
		// how a restart hands it to someone else.
		if m.Gate.Group == "" || m.Gate.GID <= 0 {
			return nil, fmt.Errorf("shared storage: %s has no recorded gate, so it cannot be gated — this is a platform bug, not a setting", m.Name)
		}
		// **Every start re-makes the gate**: the group at its recorded number,
		// then the directory behind it. A container restart empties /etc/group,
		// and the first start after it is the first moment anything can put the
		// group back.
		if err := s.ensureGroup(m.Gate); err != nil {
			return nil, err
		}
		target := filepath.Join(root.Path, m.Name)
		resourceDir := target
		if m.Partitioned && m.Folder == "" {
			return nil, fmt.Errorf("shared storage: %s is partitioned, so it is mapped only by folder", m.Name)
		}
		dirGID, dirMode := m.Gate.GID, gatedMode
		if m.Partitioned {
			dirGID, dirMode = 0, passMode
		}
		if err := s.ensureGated(resourceDir, dirGID, dirMode); err != nil {
			return nil, err
		}
		if m.Folder != "" {
			target = filepath.Join(target, m.Folder)
			// The folder is a directory inside the resource's own gate, and
			// nothing else creates it: a cluster gets one from the CSI driver's
			// subPath, and there is no driver here. Made before the link, so a
			// mapping onto a folder that has never been written to is a mount
			// point rather than a dangling symlink the app discovers at its
			// first open. It inherits the resource's gate — 0770 under a
			// root-owned parent the app cannot widen — because a directory
			// under a gate that the gate does not cover is not gated.
			if err := s.ensureGatedFolder(resourceDir, m.Folder, m.Gate, m.Partitioned); err != nil {
				return nil, err
			}
		}
		rel := mountLinkRel(m)
		link := filepath.Join(appDir, rel)
		if err := clearStaleLinks(appRoot, rel, links); err != nil {
			return nil, fmt.Errorf("shared storage: preparing %s: %w", link, err)
		}
		if err := appRoot.MkdirAll(filepath.Dir(rel), 0o750); err != nil {
			return nil, fmt.Errorf("shared storage: preparing %s: %w", link, err)
		}
		if err := replaceLink(appRoot, target, rel); err != nil {
			return nil, fmt.Errorf("shared storage: linking %s: %w", link, err)
		}
		gates = append(gates, m.Gate.Group)
		m.Path = link
		out = append(out, m)
	}
	if gatedAccount(account) {
		// Exactly these gates, joined and left, before the process exists —
		// supplementary groups are read at spawn.
		if err := s.setGates(account, gates); err != nil {
			return nil, err
		}
		if err := s.own(appRoot, "storage", account); err != nil {
			return nil, fmt.Errorf("shared storage: handing the mount points to %s: %w", account.Username, err)
		}
	}
	return out, nil
}

// replaceLink points rel at target. Replaced rather than left, because an
// administrator who re-pointed a mapping at another folder must not leave the
// app following the old one — but **never removed first**: the link is the path
// a running service has open, and every job run of the app attaches again, so a
// remove-then-create left the service a moment with no storage at each run.
// Left alone when it already says the right thing; otherwise a new link is made
// beside it and renamed over it, which is atomic.
//
// **Only a link is ever replaced.** A real file or directory at the mount point
// is something the app wrote there itself, and the rename would take a file
// silently (and fail on a directory with an error nobody could read), so the
// start is refused instead, naming what is in the way and leaving it untouched.
func replaceLink(root *os.Root, target, rel string) error {
	cur, err := root.Readlink(rel)
	if err == nil && cur == target {
		return nil
	}
	if err != nil {
		fi, lerr := root.Lstat(rel)
		switch {
		case lerr == nil && fi.Mode()&os.ModeSymlink == 0:
			what := "a file"
			if fi.IsDir() {
				what = "a directory"
			}
			return fmt.Errorf("%s is %s, not the link the storage mount needs; it is left as it is — move it aside and start the app again", rel, what)
		case lerr != nil && !errors.Is(lerr, fs.ErrNotExist):
			return lerr
		}
	}
	tmp := rel + ".toft-link"
	if err := root.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := root.Symlink(target, tmp); err != nil {
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

// clearStaleLinks removes a symlink standing where rel needs a directory — the
// link an earlier mapping left, such as storage/storage/v from a whole mapping
// since detached, in the way of storage/storage/v/f for a folder of it. Left,
// MkdirAll followed it out of apps/<id> and the Root refused every start, so a
// detach did not recover the app (BB-048, fifth 2026-11-03 black-box list).
// Only a link is removed, and never one a current mapping uses; a real file or
// directory in the way is the app's, and MkdirAll's refusal names it.
func clearStaleLinks(root *os.Root, rel string, current map[string]bool) error {
	parts := strings.Split(filepath.Dir(rel), string(filepath.Separator))
	for i := range parts {
		prefix := filepath.Join(parts[:i+1]...)
		fi, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if current[prefix] {
			return fmt.Errorf("%s is another mount's link, so nothing can be mounted inside it", prefix)
		}
		if err := root.Remove(prefix); err != nil {
			return err
		}
		return nil
	}
	return nil
}

// mountPathsNest is database.MountPathsNest, which runtime cannot import.
func mountPathsNest(a, b string) bool {
	return a != b && (strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/"))
}

// mountLinkRel is where a mapping's path lands under apps/<id>: the one place
// that sentence is spelled, because Attach makes the link and ResolvedMounts
// reports it, and the two disagreeing is an app told one path and given another.
func mountLinkRel(m Mount) string {
	return filepath.Join("storage", filepath.FromSlash(strings.TrimPrefix(m.Path, "/")))
}

// ResolvedMounts is what Attach would return, computed without touching the
// host: the same mounts with their paths resolved under the app's directory.
// It is what `GET /api/v1/app/storage` reads, so the route answers with the
// paths TOFT_STORAGE carries rather than the administrator's.
func (s *SharedStorage) ResolvedMounts(appID string, mounts []Mount) []Mount {
	if s == nil || appID == "" || s.appsDir == "" {
		return unattachedStores(mounts)
	}
	out := make([]Mount, 0, len(mounts))
	for _, m := range mounts {
		if m.NeedsRoot() {
			m.Path = filepath.Join(s.appsDir, appID, mountLinkRel(m))
		} else {
			m.Path = ""
		}
		out = append(out, m)
	}
	return out
}

// NamespaceView is what a namespace-tier workload's mount namespace hides and
// puts back so a folder mapping is the folder and nothing beside it (BB-027,
// fourth 2026-11-03 black-box list): the directory of every resource this app
// maps only by folder is masked, and each mapped folder is kept. The mount
// point stays a link to the folder's path, and in the workload's view that
// path is the folder itself under an empty directory.
//
// A resource the app also maps whole is neither masked nor kept: the whole
// mapping reaches every folder anyway. Below the namespace tier there is no
// mount namespace to do this in, and a folder of a *whole* resource is a view
// rather than a boundary — the app holds the resource's gate. A partitioned
// resource is the boundary at every tier.
func (s *SharedStorage) NamespaceView(mounts []Mount) (mask, keep []string) {
	if s == nil {
		return nil, nil
	}
	whole := map[string]bool{}
	for _, m := range mounts {
		if m.NeedsRoot() && m.Folder == "" {
			whole[m.Root+"\x00"+m.Name] = true
		}
	}
	masked := map[string]bool{}
	for _, m := range mounts {
		if !m.NeedsRoot() || m.Folder == "" || whole[m.Root+"\x00"+m.Name] {
			continue
		}
		root, ok := s.Root(m.Root)
		if !ok || m.Name == "" {
			continue
		}
		dir := filepath.Join(root.Path, m.Name)
		if !masked[dir] {
			masked[dir] = true
			mask = append(mask, dir)
		}
		keep = append(keep, filepath.Join(dir, filepath.FromSlash(m.Folder)))
	}
	return mask, keep
}

// HasRoots reports whether the operator declared anywhere to put a volume.
func (s *SharedStorage) HasRoots() bool { return s != nil && len(s.roots) > 0 }

// checkRootLevel refuses a level the root cannot enforce.
//
// **A level in shared mode is a second operator mount**, never a permission the
// platform applies — POSIX ACLs would make it the enforcer of a rule on a file
// system it does not own. So there are exactly two honest answers: a read-only
// root serves `read`, and a writable one serves `full`. Anything else
// is refused here rather than recorded and not enforced, because an access level
// that is quietly advisory is the same class of lie as a mount that is quietly
// ignored.
func checkRootLevel(root StorageRoot, m Mount) error {
	if root.ReadOnly && !m.ReadOnly() {
		return fmt.Errorf("shared storage: %s is mapped at %s, but storage root %q is mounted read-only — "+
			"a level in shared mode is which mount the resource is under, not a permission Toft applies. "+
			"Map it at read, or point the resource at a writable root",
			m.Name, m.Access, root.Name)
	}
	if !root.ReadOnly && m.ReadOnly() {
		return fmt.Errorf("shared storage: %s is mapped at read, but storage root %q is mounted read-write — "+
			"Toft cannot make it read-only for one app, because a level in shared mode is a second "+
			"operator mount of the same file system rather than a permission. Map it at full, or ask an "+
			"operator for a read-only mount and point the resource at that root",
			m.Name, root.Name)
	}
	return nil
}

// gatedAccount reports whether an account can hold a gate: it is one app's,
// with a group of its own, and so its group list — which the agent puts whole
// on every process it places as the account (accountGroups) — is this app's to
// set.
func gatedAccount(a *AppAccount) bool { return a != nil && a.GroupGID != 0 }

// gatePrefixes are the group names Toft creates for storage, and the whole of
// what setGates may take an account out of. A group with any other name — the
// account's own, the shared primary, anything an operator gave a
// pre-provisioned account — is never Toft's to remove.
var gatePrefixes = []string{"toft-store-", "toft-sf-"}

func isGateName(name string) bool {
	for _, p := range gatePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// membershipChanges is what setting an account's gates to want takes, from the
// groups it holds now: the gates to leave and the gates to join. Pure, so the
// rule about which groups are Toft's is tested where it is decided.
func membershipChanges(have []string, own string, want []string) (leave, join []string) {
	wanted := map[string]bool{}
	for _, g := range want {
		wanted[g] = true
	}
	held := map[string]bool{}
	for _, g := range have {
		held[g] = true
		if isGateName(g) && g != own && !wanted[g] {
			leave = append(leave, g)
		}
	}
	for _, g := range want {
		if !held[g] {
			join = append(join, g)
			held[g] = true
		}
	}
	sort.Strings(leave)
	sort.Strings(join)
	return leave, join
}

// systemGroupEnsure makes one gate exist on this host at its recorded gid.
//
// A machine without the shadow suite is a refusal, not a workload running
// ungated — the same shape as the `setpriv` rule, and for the same reason: the
// gate is the whole of the containment here, so an install that cannot build it
// must not pretend it has.
//
// **A gate whose number moved is a gate on someone else's files**, so both
// mismatches refuse rather than repair: the name present at another gid, and
// the gid present under another name. Either means something other than this
// platform has written the host's account database, and the operator is the
// one who can say which number is right.
func systemGroupEnsure(g Gate) error {
	defer lockShadow()()
	want := strconv.Itoa(g.GID)
	if existing, err := user.LookupGroup(g.Group); err == nil {
		if existing.Gid == want {
			return nil
		}
		return fmt.Errorf("shared storage: group %s is gid %s on this host but gid %d in the database — "+
			"the directories it gates carry %d, so it will not be used at %s", g.Group, existing.Gid, g.GID, g.GID, existing.Gid)
	}
	if other, err := user.LookupGroupId(want); err == nil {
		return fmt.Errorf("shared storage: gid %d belongs to %s on this host, but the database records it as %s — "+
			"move %s off that number or widen SHARED_STORAGE_GID_RANGE away from it", g.GID, other.Name, g.Group, other.Name)
	}
	out, err := runShadow("groupadd", "--gid", want, g.Group)
	if err != nil {
		return fmt.Errorf("shared storage: creating group %q at gid %d (the gate on this resource's directory): %s: %w", g.Group, g.GID, out, err)
	}
	return nil
}

// systemSetGates makes an account's Toft-owned groups exactly want, every one
// a SUPPLEMENTARY membership.
//
// Never a primary group. The primary group is APP_USER_GROUP and it belongs
// to the egress rule — `iptables -m owner --gid-owner` matches the effective gid
// only — so repurposing it would gate this directory and silently stop every app
// process matching the firewall. A file-permission check honours the full
// supplementary list, which is why a supplementary group works at all, and the
// agent's placement (accountGroups, read the way initgroups(3) reads it) is what
// puts that list on the process.
//
// **Leaving is as much the point as joining.** Membership used to be appended
// and never removed, so an app unmapped from a resource stayed in its group for
// as long as its account existed. The read and every write are under one hold
// of the lock, so two starts of one account (an app and its host job) cannot
// both decide to leave a group and the second fail on it.
func systemSetGates(a *AppAccount, want []string) error {
	defer lockShadow()()
	u, err := user.Lookup(a.Username)
	if err != nil {
		return fmt.Errorf("shared storage: looking up %s: %w", a.Username, err)
	}
	gids, err := u.GroupIds()
	if err != nil {
		return fmt.Errorf("shared storage: listing %s's groups: %w", a.Username, err)
	}
	have := make([]string, 0, len(gids))
	for _, id := range gids {
		if g, err := user.LookupGroupId(id); err == nil {
			have = append(have, g.Name)
		}
	}
	leave, join := membershipChanges(have, a.GroupName, want)
	for _, g := range leave {
		if out, err := runShadow("gpasswd", "--delete", a.Username, g); err != nil {
			return fmt.Errorf("shared storage: removing %s from group %q: %s: %w", a.Username, g, out, err)
		}
	}
	for _, g := range join {
		if out, err := runShadow("usermod", "--append", "--groups", g, a.Username); err != nil {
			return fmt.Errorf("shared storage: adding %s to group %q: %s: %w", a.Username, g, out, err)
		}
	}
	return nil
}

// openDirNoFollow opens rel under root as a directory and nothing else: a link
// at the name is refused by the kernel (O_NOFOLLOW), anything that is not a
// directory by O_DIRECTORY. What the descriptor then names cannot be swapped
// from under the chown or chmod made on it.
func openDirNoFollow(root *os.Root, rel string) (*os.File, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.IsDir() {
		f.Close()
		if err == nil {
			err = fmt.Errorf("%s is not a plain directory", rel)
		}
		return nil, err
	}
	return f, nil
}

// chmodDirNoFollow is Chmod on the directory's own descriptor (Q2-4).
func chmodDirNoFollow(root *os.Root, rel string, mode os.FileMode) error {
	f, err := openDirNoFollow(root, rel)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(mode)
}
