package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Durable storage, from the seam's side.
//
// Three rules shape everything here, and each one is load-bearing:
//
//   - **The durable resource always pre-exists.** The filesystem and the bucket
//     are created by the infrastructure module. A backend creates only the
//     binding objects that attach them to a workload — the same rule as
//     "Toft never creates buckets", extended rather than excepted.
//   - **Containment is what is mounted, not what is permitted.** Every isolated
//     workload runs as the same UID, so filesystem modes cannot tell one app
//     from another. Isolation comes from the access point's root-directory jail
//     and the object mount's prefix, both enforced below the workload. Never
//     mount a parent and subdivide it with permissions.
//   - **No workload holds a storage credential.** The mount is provided by
//     infrastructure the workload does not authenticate to. This is the same
//     invariant that keeps releases and job I/O on presigned URLs.

// StorageResource is a durable location, as a backend needs to see it.
type StorageResource struct {
	// Slug names the resource and, through BindingName, its backend objects. It
	// is what appears in TOFT_STORAGE.
	Slug string
	Kind string // MountVolume | MountStore
	// Backing is what is underneath: BackingEFS or BackingS3Files for a file
	// store, BackingMountpoint for a store. A backend reads it to choose a
	// class and to name a prerequisite, never to decide what the mount means —
	// what an app's code experiences is the kind.
	Backing string
	// StorageClass is the class this resource is provisioned from, and empty
	// means the backing's default. It is here because otherwise an install has
	// exactly one filesystem: the operator creates every class and a resource
	// names which of them binds it.
	StorageClass string
	// Root names the host directory this resource lives under on an install
	// with no cluster, and empty means it is provisioned through a backend's
	// bindings instead. It is a name rather than a path because the operator may
	// move a mount, and a row holding the old path would point at nothing with
	// no way to say which mount it meant.
	Root string
	// Bucket and Prefix apply to a store. The prefix is the isolation
	// boundary, so an empty one means the whole bucket and is a mistake a
	// backend should refuse rather than honour.
	Bucket string
	Prefix string
	// Access is the level this *binding* provides, and it is part of the
	// resource here rather than only of a mapping for a mechanical reason: a
	// store's access flags are mount options, and mount options live on the
	// binding object, not on the pod that consumes it. So a store gets one
	// binding per level in use — at most four, shared by every app at that
	// level, all with the same prefix and the same flags.
	//
	// A volume needs none of this: read-only is expressed on the pod's own
	// mount, so one binding serves every level.
	Access string
	// Folder narrows the binding to one directory of the resource, and empty is
	// the resource itself. On a store it goes into the mount's prefix, so a
	// folder-scoped binding is a different object with a different containment
	// boundary — which is what keeps "the folder is mounted" a true sentence
	// rather than a permission applied above a wider mount. On a volume it
	// names no binding of its own: the folder is a subPath inside the volume's
	// one claim.
	Folder string
}

// BindingName is the backend object name for one resource at one access level,
// and — where one is named — one folder of it.
//
// The prefix is not decoration: binding objects share a namespace with workload
// names, and a resource called "demo" colliding with an app called "demo" is a
// failure at pod creation with no useful message.
//
// **A store's folder arrives as a digest, and is absent entirely when empty.**
// A backend object name is a DNS label and a folder is arbitrary path text, so
// it cannot be spelled here. Absent rather than the digest of the empty string,
// because every binding that existed before folders has to keep the name it
// has: a store rebound under a new name would be a binding nothing is claiming.
//
// **A volume's folder is not in the name at all** (ADR §12a, K8-D1): a folder
// mapping of a volume is a directory of the volume, so it binds the volume's
// one claim and the pod bind-mounts the folder as a subPath inside it. A
// per-folder claim was a second access point rooted at its own directory —
// a tree disjoint from the whole volume's — and is retired.
func BindingName(slug, kind, access, folder string) string {
	name := "lpv-" + slug
	if kind != MountStore {
		return name
	}
	if access == "" {
		access = AccessRead
	}
	name += "-" + access
	if folder != "" {
		name += "-" + folderDigest(folder)
	}
	return name
}

// BindingNamesClash reports whether a volume called volumeSlug and a store
// called storeSlug would name one cluster object: the volume's claim is
// lpv-<volume>, a store's are lpv-<store>-<level>[-<8 hex>], so volume
// reports-full and store reports share lpv-reports-full. A level-suffixed
// volume name alone is harmless; it is the pair that is refused.
func BindingNamesClash(volumeSlug, storeSlug string) bool {
	rest, ok := strings.CutPrefix(volumeSlug, storeSlug+"-")
	if !ok {
		return false
	}
	for _, level := range []string{AccessRead, AccessWrite, AccessFull, legacyAccessOverwrite} {
		if rest == level {
			return true
		}
		if digest, ok := strings.CutPrefix(rest, level+"-"); ok && isFolderDigest(digest) {
			return true
		}
	}
	return false
}

// isFolderDigest is whether s could be a folderDigest: eight lowercase hex.
func isFolderDigest(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// folderDigest is the folder as a name can carry it: eight hex characters of
// its SHA-256. Long enough that a collision within one resource is not a thing
// that happens, short enough to leave room under the 63-character label limit
// for a slug an administrator chose.
func folderDigest(folder string) string {
	sum := sha256.Sum256([]byte(folder))
	return hex.EncodeToString(sum[:4])
}

// legacyAccessOverwrite is a level that no longer exists, kept for one reason:
// a store binding's name carries its level, so a Mountpoint mapping at
// `overwrite` before migration 00074 merged it into `full` left a
// `lpv-<slug>-overwrite[-<digest>]` binding behind. Nothing claims it after the
// mapping's next start, and it holds no data — a store is a prefix in a bucket
// the operator owns — so it waits for the resource's delete, which is where
// Bindings enumerates it. No reconciler sweeps it sooner, deliberately.
const legacyAccessOverwrite = "overwrite"

// Bindings lists every binding a resource could need, which is what a delete
// has to clean up: the mappings are gone by then, so there is nothing left to
// ask which levels were in use.
//
// **Folders are the caller's to supply**, because unlike the ladder they are
// not a closed set — there is no enumerating "every folder this resource could
// have had". The resource's own Folder is always one of them, so releasing a
// single folder-scoped binding is this function with that folder set, and
// releasing a resource is this function with none — the root, whose bindings
// are the ones a resource is created with.
//
// A volume has one binding whatever the folders: its folders are directories
// inside its one claim (BindingName).
func Bindings(res StorageResource, folders ...string) []StorageResource {
	if res.Kind != MountStore {
		r := res
		r.Access, r.Folder = AccessRead, ""
		return []StorageResource{r}
	}
	scopes := append([]string{res.Folder}, folders...)
	seen := map[string]bool{}
	out := make([]StorageResource, 0, len(scopes)*4)
	for _, folder := range scopes {
		if seen[folder] {
			continue
		}
		seen[folder] = true
		for _, a := range []string{AccessRead, AccessWrite, AccessFull, legacyAccessOverwrite} {
			r := res
			r.Access, r.Folder = a, folder
			out = append(out, r)
		}
	}
	return out
}

// ErrStorageNotSupported means this backend has nowhere to attach durable
// storage. Distinct from a failure: a shared workload has no isolation
// boundary to attach one *behind*, which is a property of the mode rather than
// a defect to be fixed.
var ErrStorageNotSupported = errors.New("durable storage requires isolated mode")

// StorageProvisioner creates and removes the binding objects for a durable
// resource. Implemented by an isolated backend; nil on installs that offer no
// isolated mode, where the capability is simply not on offer.
type StorageProvisioner interface {
	// EnsureStorage makes the binding exist. Idempotent: an admin retrying a
	// half-failed create must not produce a second object.
	EnsureStorage(ctx context.Context, res StorageResource) error
	// RemoveStorage releases the binding. It never deletes data — the reclaim
	// policy on the binding is what guarantees that, and a backend whose
	// binding would cascade into the data must refuse to create it in the
	// first place.
	RemoveStorage(ctx context.Context, res StorageResource) error
	// DestroyStorage deletes a resource's data along with its bindings, and
	// says what it removed and what it could not. **The resource delete is its
	// only caller** (ADR §12a): every other path that lets go of storage —
	// detaching an app, the mapping sweep — releases, and a destroy one flag
	// away from a release would be reachable from code written for the
	// release. `claims` is every claim the resource was ever given
	// (storage_claims): a volume's claims, and a store's folder bindings,
	// which a store's destroy releases beside its root ones. A backend whose
	// data is not behind claims ignores it.
	DestroyStorage(ctx context.Context, res StorageResource, claims []string) (DestroyReport, error)
	// VolumesGone reports whether every volume a destroy left Pending has been
	// deleted by the cluster. A backend that destroys synchronously never
	// leaves any, and answers true.
	VolumesGone(ctx context.Context, volumes []string) (bool, error)
}

// DestroyReport is what a destroy did, said rather than implied: a delete
// never claims files are gone that are not.
type DestroyReport struct {
	// RemovesData is whether the bytes go at all, which the dialog has already
	// told the administrator from the drive's verdict.
	RemovesData bool `json:"removes_data"`
	// Removed names what is already gone: a directory, or a claim that never
	// bound to anything.
	Removed []string `json:"removed,omitempty"`
	// Pending is the PersistentVolumes the cluster has been told to delete and
	// has not yet. Not sent: the reconciler reads it from the tombstone.
	Pending []string `json:"-"`
	// LeftBehind is everything the destroy could not remove, each with why.
	LeftBehind []LeftBehind `json:"left_behind,omitempty"`
}

// LeftBehind is one thing a destroy did not remove, and the reason.
type LeftBehind struct {
	What   string `json:"what"`
	Reason string `json:"reason"`
}

// RejectMounts is the refusal a backend with nowhere to attach durable storage
// owes its caller. It names only the mounts that need a root: a store is never
// attached on a shared host, so it is not storage with nowhere to go. Shared backends call it first thing in Deploy, and **only
// when they have no storage roots** — which is the whole of what changed when
// shared mode gained storage: what is refused is storage with nowhere to put
// it, never storage in this mode.
//
// The message names the app and the resources, because the person reading it is
// looking at a deploy that used to work and needs to know which of three things
// to change: the install's roots, the app's mode, or its mappings.
func RejectMounts(spec WorkloadSpec) error {
	names := make([]string, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		if m.NeedsRoot() {
			names = append(names, m.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s has storage mapped (%s) and is deploying in shared mode, where "+
		"durable storage lives under a root the operator mounted — this install declares none. "+
		"An operator sets SHARED_STORAGE_ROOTS, or an administrator promotes the app to "+
		"isolated mode or removes its storage mappings",
		ErrStorageNotSupported, spec.ProcessName(), strings.Join(names, ", "))
}

// MountDescriptor is one entry of TOFT_STORAGE.
//
// Mounted paths are *declared, not discovered*: an app locates a resource by
// name and reads the path from here, so an administrator can move a mount
// without editing the app.
type MountDescriptor struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Access string `json:"access"`
	// Folder is which directory of the resource this mount is rooted at.
	// Omitted when it is the whole resource, so an install that names no folder
	// hands its apps exactly the JSON it always has.
	Folder string `json:"folder,omitempty"`
}

// MountDescriptors is TOFT_STORAGE before it is a string — what
// `GET /api/v1/app/storage` answers with, so the route and the environment are
// one rendering of one slice rather than two that agree by coincidence.
func MountDescriptors(mounts []Mount) []MountDescriptor {
	out := make([]MountDescriptor, 0, len(mounts))
	for _, m := range mounts {
		access := m.Access
		if access == "" {
			access = AccessRead
		}
		out = append(out, MountDescriptor{
			Name: m.Name, Kind: m.Kind, Path: m.Path, Access: access, Folder: m.Folder})
	}
	return out
}

// DescribeMounts renders the TOFT_STORAGE value for a set of mounts.
//
// The backend that builds the mounts also builds this string, from the same
// slice, so the description and the filesystem cannot disagree — which they
// would if the platform computed it separately and an administrator changed a
// mapping in the window between.
func DescribeMounts(mounts []Mount) string {
	b, err := json.Marshal(MountDescriptors(mounts))
	if err != nil {
		return "[]"
	}
	return string(b)
}

// DeclaredClass is one StorageClass a cluster install offers volumes from: a
// drive on a cluster. Backing is what the class provides, or empty where the
// provisioner cannot say (a class Toft cannot read, or one whose driver serves
// both EFS and Azure Files' family), in which case either volume backing may
// use it.
type DeclaredClass struct {
	Name    string `json:"name"`
	Backing string `json:"backing,omitempty"`
	// Default is a class named by KUBE_STORAGE_CLASS or
	// KUBE_S3FILES_STORAGE_CLASS: what a resource naming no class uses.
	Default bool `json:"default,omitempty"`
	// DeleteRemovesData is whether deleting a volume on this class removes its
	// files, read from the class each time it is listed (kube.deleteVerdict).
	DeleteRemovesData bool `json:"delete_removes_data,omitempty"`
}

// Serves reports whether this class can be used under a backing.
func (c DeclaredClass) Serves(backing string) bool {
	return c.Backing == backing || c.Backing == ""
}

// ParseStorageClasses reads KUBE_STORAGE_CLASSES: further classes a cluster
// offers volumes from, beyond the two defaults, as `name[=backing]`,
// comma-separated. The backing is optional where the provisioner can say it
// (EFS or Azure Files) and **required for S3 Files**, whose classes share the
// EFS driver, so nothing read from the class could tell. Strict, like the roots:
// a class that silently did not parse is a drive nobody can pick.
func ParseStorageClasses(spec string) ([]DeclaredClass, error) {
	out := []DeclaredClass{}
	seen := map[string]bool{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, backing, _ := strings.Cut(entry, "=")
		name, backing = strings.TrimSpace(name), strings.TrimSpace(backing)
		if name == "" {
			return nil, fmt.Errorf("storage class %q is not name[=backing]", entry)
		}
		switch backing {
		case "", BackingEFS, BackingAzureFiles, BackingS3Files:
		default:
			return nil, fmt.Errorf("storage class %q: backing %q is not efs, azurefiles or s3files", name, backing)
		}
		if seen[name] {
			return nil, fmt.Errorf("storage class %q is declared twice", name)
		}
		seen[name] = true
		out = append(out, DeclaredClass{Name: name, Backing: backing})
	}
	return out, nil
}

// ResolveDefaultClass is the class a volume naming none is created on: the
// backing's default, or the only declared class serving it. Empty and true where
// the install declares no classes at all (a host install, or a cluster backend
// that lists none); false where several serve the backing and none is default.
func ResolveDefaultClass(classes []DeclaredClass, backing string) (string, bool) {
	var serving []DeclaredClass
	for _, c := range classes {
		if !c.Serves(backing) {
			continue
		}
		if c.Default {
			return c.Name, true
		}
		serving = append(serving, c)
	}
	switch len(serving) {
	case 0:
		return "", true
	case 1:
		return serving[0].Name, true
	}
	return "", false
}
