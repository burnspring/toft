package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/runtime"
)

// Durable storage, from the API's side.
//
// Two surfaces, and the split is the whole authorization model: an
// administrator creates resources and maps them onto named apps, and an app's
// own readers can see what their app has without being able to change it.
// There is no self-service provisioning — a volume is a real cost and a real
// blast radius, and 1,000 access points per filesystem is a hard quota that
// per-app auto-provisioning would burn through.
//
// Every route 404s when the install offers no storage. Absent configuration
// means the capability is not offered, which is the same rule the artifact
// store, isolated mode, and local auth all follow: no dormant paths, and
// nothing in the UI for a thing that cannot work here.

// storageOffered reports whether this install can bind durable storage at all.
func (s *Server) storageOffered() bool { return s.storage != nil }

// requireStorage is the gate every storage route passes through.
func (s *Server) requireStorage(w http.ResponseWriter) bool {
	if s.storageOffered() {
		return true
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "ask an operator to set up app storage on this install; until then there is none to manage"})
	return false
}

// requireStorageAdmin gates the admin surface. 404 when storage is not
// configured, then 401, then 403 — and the 403 is pinned: an unconfigured
// install answers "absent" to everyone, which is the rule for an optional
// capability, and a configured one tells a non-administrator it exists, which
// is not a secret the admin page keeps.
func (s *Server) requireStorageAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireStorage(w) {
		return false
	}
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	if !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "only an administrator can manage app storage", "hint": "ask an administrator to make the change; `toft storage ls` shows the storage you can attach to an app"})
		return false
	}
	return true
}

// storageKindLister is implemented by a backend that can say which kinds it can
// actually provision, so the admin surface offers only what will work.
//
// **Two interfaces rather than one with two methods**, and the reason is worth
// keeping: widening this one to carry `StorageBackings` silently un-implemented
// it for every fake that only had `StorageKinds`, and an install that offered
// nothing began offering everything. A capability check that fails open is the
// one shape this must never have, so each capability asks for exactly the
// method it needs.
type storageKindLister interface{ StorageKinds() []string }

// storageBackingLister is the same question one level down.
type storageBackingLister interface{ StorageBackings(kind string) []string }

func (s *Server) storageKinds() []string {
	if l, ok := s.storage.(storageKindLister); ok {
		return l.StorageKinds()
	}
	return []string{runtime.MountVolume, runtime.MountStore}
}

// storageBackings answers per kind, because a form asks the question that way:
// the kind is chosen first and the backings follow from it.
//
// A backend that cannot say — there is none today, but the interface is
// optional and the fallback has to be honest — answers with the backing each
// kind has always had, never with one that needs configuration nobody has
// confirmed. Offering S3 Files on an install that cannot provision it is a form
// that fails on submit, which is the failure this whole path exists to avoid.
// storageRootLister is implemented by a provisioner that puts resources under
// host directories an operator mounted, rather than under a cluster's bindings.
type storageRootLister interface{ Roots() []runtime.StorageRoot }

// storageDirectoryChecker is a provisioner whose resources are directories it
// creates itself, and so can say whether one is already in the way.
type storageDirectoryChecker interface {
	ExistingDirectory(res runtime.StorageResource) (string, bool, error)
}

// storageClassLister is a cluster's drives. A backend that is not one has no
// class to name, so a create naming one there is refused.
type storageClassLister interface {
	DeclaredClasses() []runtime.DeclaredClass
}

func (s *Server) declaredClasses() []runtime.DeclaredClass {
	if l, ok := s.storage.(storageClassLister); ok {
		return l.DeclaredClasses()
	}
	return nil
}

// refuseUndeclaredClass answers a create naming a class this install does not
// offer under that backing, and reports whether it did.
func (s *Server) refuseUndeclaredClass(w http.ResponseWriter, class, backing string) bool {
	if class == "" {
		return false
	}
	var offered []string
	for _, c := range s.declaredClasses() {
		if c.Serves(backing) {
			if c.Name == class {
				return false
			}
			offered = append(offered, c.Name)
		}
	}
	msg := fmt.Sprintf("this install does not offer storage class %q for %s", class, backing)
	if len(offered) > 0 {
		msg += " — it offers " + strings.Join(offered, ", ")
	} else {
		msg += " — leave the class empty to use the install's default"
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg, "hint": "name one of the offered classes in storage_class, or leave storage_class empty for the install's default"})
	return true
}

// storageRoots is what the create form offers. Empty on every install with a
// cluster, and on a shared install that declares none — which is the same
// answer for two different reasons and needs no distinction here, because in
// both cases there is no root to choose.
// storesMounted reports whether a store on this install is mounted into the
// apps it is mapped to, which is true exactly when the provisioner is a
// cluster's: a shared host has no CSI driver to mount one with.
func (s *Server) storesMounted() bool {
	_, shared := s.storage.(*runtime.SharedStorage)
	return s.storage != nil && !shared
}

func (s *Server) storageRoots() []runtime.StorageRoot {
	if l, ok := s.storage.(storageRootLister); ok {
		return l.Roots()
	}
	return nil
}

func (s *Server) storageBackings(kind string) []string {
	if l, ok := s.storage.(storageBackingLister); ok {
		return l.StorageBackings(kind)
	}
	return []string{database.DefaultStorageBacking(kind)}
}

// --- Resources ---

type storageDeleting struct {
	Slug      string    `json:"slug"`
	Kind      string    `json:"kind"`
	StartedAt time.Time `json:"started_at"`
	// Kept is a finished delete whose volume directory was left on the file
	// system. The name is held until DELETE /admin/storage/deleting/{slug}.
	Kept bool `json:"kept,omitempty"`
}

type storageListResponse struct {
	Resources []database.StorageResource `json:"resources"`
	// Kinds is what this install can provision. A form that offered a volume on
	// an install with no shared filesystem would fail on submit for a reason the
	// admin cannot see from the page.
	Kinds []string `json:"kinds"`
	// Backings is the same answer one level down, keyed by kind: which classes
	// an operator actually configured. It rides on this admin-only response
	// rather than on `/api/v1/config`, which is the unauthenticated probe — what
	// storage an install has is not something an anonymous caller needs, and the
	// only surface that asks is this page, which already fetches this.
	Backings map[string][]string `json:"backings"`
	// Drives is every host root and declared cluster class, labelled, and any
	// name resources still record that nothing declares (ADR §12a). It replaced
	// `roots`: a root is a drive of kind "root", and the create form asks for
	// one only where a backing needs it.
	Drives []storageDrive `json:"drives"`
	// Deleting is every resource deleted here whose volumes the cluster has
	// not finished deleting: its slug is held until then, and the page shows
	// it rather than letting a create fail for a reason nothing on screen
	// explains.
	Deleting []storageDeleting `json:"deleting"`
	// StoresMounted is false on an install with no cluster, where a store is
	// never mounted and is reached over signed URLs alone — by people, and by
	// the apps it is mapped to. The form says so rather than offering
	// "Mountpoint" on an install that has none.
	StoresMounted bool `json:"stores_mounted"`
	// The two caps, sent rather than restated in the page, so "N of 25" cannot
	// drift from the number the create refuses at.
	MaxResources   int `json:"max_resources"`
	MaxAppMappings int `json:"max_app_mappings"`
	// PartitionedRoots says whether a partitioned resource may be created under
	// a host root here: only where every app runs under an account of its own,
	// since a folder's gate would otherwise exclude nobody. The form leaves the
	// choice out rather than offering one the create refuses.
	PartitionedRoots bool `json:"partitioned_roots"`
}

func (s *Server) handleListStorageResources(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	resources, err := s.db.ListStorageResources()
	if err != nil {
		log.Printf("listing storage resources: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	kinds := s.storageKinds()
	backings := make(map[string][]string, len(kinds))
	for _, k := range kinds {
		backings[k] = s.storageBackings(k)
	}
	drives, err := s.storageDrives(resources)
	if err != nil {
		log.Printf("listing storage drives: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	deletions, err := s.db.ListStorageDeletions()
	if err != nil {
		log.Printf("listing storage deletions: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	deleting := make([]storageDeleting, 0, len(deletions))
	for _, d := range deletions {
		deleting = append(deleting, storageDeleting{Slug: d.Slug, Kind: d.Kind, StartedAt: d.StartedAt, Kept: d.Kept})
	}
	writeJSON(w, http.StatusOK, storageListResponse{
		Resources: resources, Kinds: kinds, Backings: backings, Drives: drives, Deleting: deleting,
		StoresMounted: s.storesMounted(), MaxResources: database.MaxStorageResources,
		MaxAppMappings: database.MaxAppStorageMappings, PartitionedRoots: s.cfg.AppUserPerApp})
}

type createStorageRequest struct {
	// Name is the identifier, in database.ValidateStorageName's format. `slug`
	// is accepted only as the same string, for a client that still sends both.
	Name string `json:"name"`
	Slug string `json:"slug"`
	Kind string `json:"kind"`
	// Backing is what to put underneath. Optional, and absent means the one
	// this kind has always had — a client written before backings existed
	// creates exactly what it created.
	Backing string `json:"backing"`
	// StorageClass overrides the class this resource is provisioned from.
	// Empty is the backing's default, which is what every install that has one
	// filesystem per backing wants.
	StorageClass string `json:"storage_class"`
	// Root names one of the operator's host mounts. Required by a backing that
	// lives under one, refused when this install declares none.
	Root        string `json:"storage_root"`
	Bucket      string `json:"bucket"`
	Prefix      string `json:"prefix"`
	Description string `json:"description"`
	// Partitioned makes a resource that is mapped only by folder, each folder
	// its own gate on a host root. Fixed here and never again.
	Partitioned bool `json:"partitioned"`
}

// The description is generous because it is the one place an administrator
// explains what a volume is *for* to the next person on the rota — and, since
// the name became the identifier, the only place for a phrase with spaces in
// it. Bounded so the column never becomes a document store.
const maxStorageDescriptionLen = 2000

// handleCreateStorageResource records a resource and binds it in the cluster.
//
// Order matters and is deliberate: the row is written first, then the binding.
// A row with no binding is visible and repairable — the next create of the same
// slug re-runs the ensure, which is idempotent. A binding with no row is
// invisible, and an orphaned access point on a shared filesystem is exactly the
// thing nobody finds until the quota runs out.
func (s *Server) handleCreateStorageResource(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	// The license gate: writing new configuration here needs one. Reads are
	// untouched so the page renders, removal is untouched so nothing is
	// trapped, and what is already configured keeps being honoured.
	if s.refusedUnlicensed(w, r, entitlements.FeatureAppStorage) {
		return
	}
	p := auth.GetPrincipal(r.Context())

	var req createStorageRequest
	if !decodeStrictBody(w, r, &req) {
		return
	}

	// **Validated, never derived.** A name is what app code and the CLI type,
	// so the string an administrator enters is the string they get back — a
	// derived slug was a second name nobody was shown.
	name := strings.TrimSpace(req.Name)
	// Every missing field at once: one at a time is a round trip per field.
	var missing []string
	if name == "" {
		missing = append(missing, "name")
	}
	if strings.TrimSpace(req.Kind) == "" {
		missing = append(missing, "kind (volume or store)")
	}
	if len(missing) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a storage resource needs " + strings.Join(missing, " and "), "code": "missing_fields", "hint": `send both, like {"name": "sales-data", "kind": "volume"}`})
		return
	}
	if err := database.ValidateStorageName(name); err != nil {
		// 400, as every invalid_* is: a malformed field, not a request this
		// install cannot serve (BB-019).
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(),
			"code": "invalid_storage_name",
			"hint": "use 3–40 lowercase letters, digits and single hyphens, starting with a letter, like sales-data"})
		return
	}
	if slug := strings.TrimSpace(req.Slug); slug != "" && slug != name {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a resource has one name, which apps and the CLI both use — send it as name and leave slug out", "hint": `send the name in "name" alone, like {"name": "sales-data", "kind": "volume"}`})
		return
	}
	if len(strings.TrimSpace(req.Description)) > maxStorageDescriptionLen {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": fmt.Sprintf("a description is at most %d characters", maxStorageDescriptionLen), "hint": fmt.Sprintf("shorten description to %d characters or fewer", maxStorageDescriptionLen)})
		return
	}
	if !database.ValidStorageKind(req.Kind) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("kind must be %q or %q", database.StorageKindVolume, database.StorageKindStore),
			"code":  "invalid_storage_kind", "hint": `send "kind": "volume" or "kind": "store"`})
		return
	}
	if !offersKind(s.storageKinds(), req.Kind) {
		// 422, not 400: the request is well-formed and the kind is real, but
		// this install cannot provide it. The message names the missing setting.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": explainKindUnavailable(req.Kind), "code": "storage_kind_unavailable",
			"hint": offeredHint("kind", s.storageKinds())})
		return
	}

	// The backing, on the same two-step: is it a backing this kind can have,
	// and is it one this install configured a class for. Absent means the one
	// the kind has always had, so a client written before this field existed
	// keeps working unchanged.
	backing := strings.TrimSpace(req.Backing)
	if backing == "" {
		backing = defaultBackingOffered(req.Kind, s.storageBackings(req.Kind))
		if backing == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": fmt.Sprintf("this install offers a %s on %s, so a create names which",
					req.Kind, strings.Join(s.storageBackings(req.Kind), " or ")),
				"code": "storage_backing_required", "hint": offeredHint("backing", s.storageBackings(req.Kind))})
			return
		}
	}
	if !database.ValidStorageBacking(req.Kind, backing) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("a %s is backed by %s — %q is not one of them",
				req.Kind, strings.Join(database.StorageBackings(req.Kind), " or "), backing),
			"code": "invalid_storage_backing", "hint": offeredHint("backing", s.storageBackings(req.Kind))})
		return
	}
	if !offersKind(s.storageBackings(req.Kind), backing) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": explainBackingUnavailable(backing), "code": "storage_backing_unavailable",
			"hint": offeredHint("backing", s.storageBackings(req.Kind))})
		return
	}

	slug := name
	class := strings.TrimSpace(req.StorageClass)
	if s.refuseUndeclaredClass(w, class, backing) {
		return
	}
	// **The class is recorded, not resolved later.** A resource naming none used
	// to follow whatever KUBE_STORAGE_CLASS said at each binding, so changing the
	// variable moved where its next folder's claim was made — and which drive it
	// is listed under. On a cluster it now takes the backing's default, or the
	// one declared class serving the backing, at create.
	if class == "" && req.Kind == database.StorageKindVolume {
		resolved, ok := runtime.ResolveDefaultClass(s.declaredClasses(), backing)
		if !ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "this install offers more than one drive for " + backing + " and none is the default — choose one",
				"code":  "storage_class_required",
				"hint":  "name one in storage_class — GET /admin/storage lists the drives"})
			return
		}
		class = resolved
	}

	// The root, where the backing lives under one. Validated against what the
	// operator actually declared rather than taken on trust: a resource naming
	// a root nobody mounted is a directory that cannot be created, discovered
	// at the first deploy that needed it.
	roots := s.storageRoots()
	root := strings.TrimSpace(req.Root)
	// A store lives in a bucket and is never under a root — the database
	// refuses the pair — so naming one is a mistake to say, not to store.
	if root != "" && req.Kind == database.StorageKindStore {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "an object store lives in a bucket, not under a storage root — leave the root empty",
			"code":  "storage_root_not_taken", "hint": "leave storage_root out for a store"})
		return
	}
	if root != "" && !knownRoot(roots, root) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": fmt.Sprintf("this install declares no storage root called %q%s", root, rootsAvailable(roots)),
			"code":  "storage_root_unknown", "hint": offeredHint("storage_root", rootNames(roots))})
		return
	}
	// A root is required by the backing — `host` names no directory without one
	// — and also by the *install*, which is the half that is easy to miss. An
	// install provisioning under host roots has no cluster to bind a claim in,
	// so a resource created there with no root is one nothing can ever mount:
	// the create would succeed and the first mapping would fail at the app's
	// next deploy. The two conditions are separate because they are separate
	// facts, and either one alone is enough. **A store is exempt from the
	// second**: it is reached over signed URLs on such an install and needs no
	// directory at all.
	needsRoot := database.NeedsStorageRoot(backing) || (len(roots) > 0 && req.Kind == database.StorageKindVolume)
	// **A root named by nobody is the one the install declares, when it
	// declares one**, as a backing named by nobody is the one it offers: an
	// install with a single root has no choice to make, and refusing the
	// documented {name, kind} body to ask for it was a round trip to learn
	// the only answer (BB-046, fourth 2026-11-03 black-box list). Several
	// roots are still a choice, and still asked.
	if root == "" && needsRoot && len(roots) == 1 {
		root = roots[0].Name
	}
	if root == "" && needsRoot {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "storage on this install lives under a root an operator mounted on the host, " +
				"so it needs one named" + rootsAvailable(roots),
			"code": "storage_root_required", "hint": offeredHint("storage_root", rootNames(roots))})
		return
	}

	res := &database.StorageResource{
		Name: name, Slug: slug, Kind: req.Kind, Backing: backing,
		StorageClass: class,
		Root:         root,
		Partitioned:  req.Partitioned,
		Description:  strings.TrimSpace(req.Description),
	}
	// A gate has to exclude somebody. With one account for every app — or the
	// platform's own — a folder's group would hold every app at once, so the
	// isolation a partitioned resource promises is not one this install can
	// keep, and it is not offered rather than offered and not enforced.
	if res.Partitioned && root != "" && !s.cfg.AppUserPerApp {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "a partitioned resource keeps apps out of each other's folders with a group per folder, " +
				"and this install runs apps without an account each (APP_USER_PER_APP), so no group could " +
				"keep one app out. Create it whole, or run apps under accounts of their own.",
			"code": "partitioned_unavailable", "hint": `leave "partitioned" out to create it whole`})
		return
	}

	// The bucket is asked for by *backing*, not by kind. A store lives in one,
	// and so does an S3 Files file store — it is a bucket mounted as a file
	// system, and naming it is what gives the resource a data plane at all.
	//
	// **The list of those backings is `database.BackingNeedsBucket` and not a
	// condition written here**, because a condition written here is what dropped
	// a `blob` store's container and prefix on the floor: the form asked for both
	// and this branch did not run.
	if database.BackingNeedsBucket(backing) {
		res.Bucket = strings.TrimSpace(req.Bucket)
		res.Prefix = strings.Trim(strings.TrimSpace(req.Prefix), "/")
		if res.Bucket == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": bucketRequiredFor(backing), "code": "bucket_required",
				"hint": `name it in "bucket", with a "prefix" inside it`})
			return
		}
		if res.Prefix == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "this resource needs a prefix. The prefix is what contains the app: " +
					"mounting a " + containerWord(backing) + " root would give every app mapped " +
					"to anything in that " + containerWord(backing) + " access to all of it",
				"code": "prefix_required", "hint": `name a folder inside the ` + containerWord(backing) + ` in "prefix", like "team/reports"`})
			return
		}
		// **The prefix is validated as a path, by the validator a folder goes
		// through**, because the two are concatenated into one string and that
		// string is the containment on both drivers. Mountpoint matches a prefix
		// literally and would carry a `..` segment harmlessly into a key nobody
		// has; blobfuse2 puts `--subdirectory` through `filepath.Join`, which
		// *cleans* the path — so `team/../` mounts the container root, verified
		// against a real blobfuse2. A traversal must never reach the mount, and
		// the place it can be refused once is where it is written down.
		//
		// Validated after the slashes come off rather than before, which is the
		// opposite of validatedFolder and for the stated reason: a folder has
		// three callers that have to accept the identical set, and this has one
		// writer. A pasted "/team/" is noise a prefix never stores; a `..` is not.
		if err := ValidateStorePath(res.Prefix); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "prefix: " + err.Error(), "hint": `name a folder inside it in "prefix", like "team/reports", with no ".." segment`})
			return
		}
		// The blob driver splits mount options on spaces, so whitespace in a
		// blob prefix would be a second blobfuse2 argument, not a path.
		if backing == database.StorageBackingBlob && hasSpace(res.Prefix) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "prefix: an Azure Blob prefix cannot contain whitespace — the mount passes it on a command line",
				"hint":  `name a folder inside the container in "prefix", like "team/reports"`})
			return
		}
	}

	// **A name in use is name_taken, before the directory is asked about.**
	// The directory of an existing resource is that resource's, so asking
	// first answered a duplicate name with directory_exists and a host path
	// the administrator never chose (BB-019). The unique index below still
	// settles a race between two creates.
	if held, deleting, err := s.db.StorageResourceByName(name); err != nil {
		log.Printf("checking storage name %s: %v", name, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	} else if held != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "a storage resource called “" + name + "” already exists", "code": "name_taken",
			"hint": "choose another name — GET /api/v1/admin/storage lists the names in use"})
		return
	} else if deleting {
		kept, class, err := s.db.StorageNameKept(name)
		if err != nil {
			log.Printf("checking storage name %s: %v", name, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if kept {
			writeStorageDirectoryKept(w, name, class)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "“" + name + "” is still being deleted. A finished job can hold its storage " +
				"for up to an hour; the name is free once the cluster has let go of every volume.",
			"code": "storage_deleting", "hint": "try again once the deletion finishes, or create it under another name now"})
		return
	}

	// **A directory already at the path is refused, not taken over.** The
	// directory is Toft's from here — gated at the first start, removed with
	// the resource — so one an operator made, or one an earlier delete could not
	// remove, would become this resource's and go with its delete. Asked before
	// the row, so a refusal leaves nothing behind.
	if c, ok := s.storage.(storageDirectoryChecker); ok && root != "" {
		path, taken, err := c.ExistingDirectory(storageResourceFor(res, runtime.AccessRead, ""))
		if err != nil {
			log.Printf("checking storage directory for %s: %v", name, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if taken {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": path + " already exists on storage root “" + root + "”. Toft creates a resource's " +
					"directory and deletes it with the resource, so it will not use one it did not make — " +
					"choose another name, or move the directory aside on the host.",
				"code": "directory_exists", "hint": `send a different "name", or have an operator move the existing directory aside on the host`})
			return
		}
	}

	meta := requestMeta(r.Context())
	if err := s.db.CreateStorageResource(res, p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		if errors.Is(err, database.ErrGateRangeExhausted) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error() +
				" — an operator widens SHARED_STORAGE_GID_RANGE", "hint": "ask an operator to widen SHARED_STORAGE_GID_RANGE, then try again"})
			return
		}
		if errors.Is(err, database.ErrStorageResourceCap) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error() +
				". Partition one of them and map folders of it instead: a partitioned resource's folders do not count.", "hint": "partition an existing resource and map its folders instead, or delete one nobody uses — GET /api/v1/admin/storage lists them"})
			return
		}
		if errors.Is(err, database.ErrStorageBindingClash) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(),
				"code": "storage_name_clash", "hint": "choose another name — GET /api/v1/admin/storage lists the names in use"})
			return
		}
		if errors.Is(err, database.ErrStorageDirectoryKept) {
			_, class, _ := s.db.StorageNameKept(name)
			writeStorageDirectoryKept(w, name, class)
			return
		}
		if errors.Is(err, database.ErrStorageDeleting) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "“" + name + "” is still being deleted. A finished job can hold its storage " +
					"for up to an hour; the name is free once the cluster has let go of every volume.",
				"code": "storage_deleting", "hint": "try again once the deletion finishes, or create it under another name now"})
			return
		}
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "a storage resource with that name already exists", "code": "name_taken", "hint": "choose another name — GET /api/v1/admin/storage lists the names in use"})
			return
		}
		log.Printf("creating storage resource: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// The binding for the base level. A store's further levels are created when
	// a mapping first asks for one, because the flags that distinguish them are
	// mount options and mount options live on the binding.
	ctx, cancel := storageContext(r)
	defer cancel()
	if err := s.ensureBinding(ctx, res, runtime.AccessRead, ""); err != nil {
		log.Printf("binding storage resource %s: %v", res.Slug, err)
		// **The row stays, so the sentence has to say what to do with it.** It
		// used to end in the backend's "try again once it is gone", and trying
		// again is the one thing that cannot work: the resource was recorded a
		// few lines above, so the retry the operator was told to make answers
		// *a storage resource with that name already exists*. The row
		// is kept rather than rolled back because the binding may have half
		// succeeded — a volume created and its claim refused — and deleting the
		// row would leave cluster objects nothing here knows about, which is
		// worse than a resource that is visible and deletable.
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "the resource was recorded but the cluster refused the binding: " + err.Error() +
				". It exists here with nothing behind it: delete it and create it again once " +
				"the cluster is ready.",
			"code": "binding_refused"})
		return
	}
	// **Answer with the row as committed, not the struct built before it.** The
	// create's own transaction applies the default delegations (and anything
	// else a default adds), so the counts on `res` are the zero values of a
	// resource nobody manages — an answer that tells the administrator the
	// opposite of what the install now does with the files.
	if committed, err := s.db.GetStorageResource(res.ID); err == nil && committed != nil {
		res = committed
	} else if err != nil {
		log.Printf("re-reading storage resource %s after create: %v", res.Slug, err)
	}
	writeJSON(w, http.StatusCreated, res)
}

// renameStorageRequest is what may change about a resource after it exists:
// its description, and nothing else.
//
// A pointer so that "not supplied" and "cleared" are different requests —
// clearing a description is a real edit and there has to be a way to make it.
// `name` and `slug` are deliberately absent, and a client that sends either is
// told so by name rather than having it silently dropped: the name is what every
// mounted binding is named after and what app code looks the resource up by,
// so moving it would strand live data and break every app that uses it.
type renameStorageRequest struct {
	Description *string `json:"description"`
}

// handleRenameStorageResource — PATCH /admin/storage/{id}. It edits the
// description; the name is fixed at create (ADR §71).
//
// Nothing here touches the cluster: a description is a fact about the row, and
// no binding, mapping, grant or mount is derived from it. That is
// what makes this the one write on this page with no `EnsureStorage` behind it,
// and why it cannot fail halfway.
func (s *Server) handleRenameStorageResource(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	// The license gate, on the same reading as the create above: writing new
	// configuration needs one, and reads and removals never do.
	if s.refusedUnlicensed(w, r, entitlements.FeatureAppStorage) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	id := chi.URLParam(r, "id")

	res, err := s.db.GetStorageResource(id)
	if err != nil || res == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/storage lists the storage resources with their ids"})
		return
	}

	// Refused by name rather than ignored, the way the settings endpoint
	// refuses a read-only key: an administrator who believed they had moved a
	// resource's address would find out from a mount that still says otherwise.
	var raw map[string]json.RawMessage
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || json.Unmarshal(body, &raw) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	if _, present := raw["partitioned"]; present {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "whether a resource is partitioned is fixed when it is created. Its folders' gates " +
				"are drawn on the directories already, so changing it would re-draw them under the apps " +
				"using them — create a new resource instead.", "hint": `leave "partitioned" out of the update, or create a new resource with the setting you want`})
		return
	}
	// The name is refused even unchanged-in-spirit (a different case, a
	// trimmed space): one spelling, fixed at create. Sending the current name
	// back is harmless and allowed, so a form that echoes it still works.
	for _, field := range []string{"name", "slug"} {
		v, present := raw[field]
		if !present {
			continue
		}
		var sent string
		if json.Unmarshal(v, &sent) == nil && sent == res.Name {
			continue
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a resource's name cannot be changed. Apps look it up by that name and every " +
				"mounted binding is named after it, so moving it would break both — create a new " +
				"resource if you need a different name, and say what this one is for in its description.", "hint": `leave "name" out and change "description" instead, or create a new resource under the new name`})
		return
	}

	var req renameStorageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	if req.Description == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "nothing to update", "hint": `send "description", the field that can change, like {"description": "Monthly sales exports"}`})
		return
	}

	name, description := res.Name, res.Description
	if req.Description != nil {
		description = strings.TrimSpace(*req.Description)
		if len(description) > maxStorageDescriptionLen {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": fmt.Sprintf("a description is at most %d characters", maxStorageDescriptionLen), "hint": fmt.Sprintf("shorten description to %d characters or fewer", maxStorageDescriptionLen)})
			return
		}
	}
	if name == res.Name && description == res.Description {
		// Not a change, so it writes nothing and audits nothing — the rule the
		// visibility PATCH established, for the same reason.
		writeJSON(w, http.StatusOK, res)
		return
	}

	meta := requestMeta(r.Context())
	if err := s.db.RenameStorageResource(id, name, description, p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "a storage resource with that name already exists", "code": "name_taken", "hint": "choose another name — GET /api/v1/admin/storage lists the names in use"})
			return
		}
		log.Printf("renaming storage resource %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	updated, err := s.db.GetStorageResource(id)
	if err != nil || updated == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteStorageResource releases the binding. It never deletes data.
func (s *Server) handleDeleteStorageResource(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	id := chi.URLParam(r, "id")

	res, err := s.db.GetStorageResource(id)
	if err != nil || res == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/storage lists the storage resources with their ids"})
		return
	}

	// The row goes and the tombstone that holds its slug is written, in one
	// transaction: from here nobody reaches the resource from outside, and
	// nobody can create another under its name until the data is gone.
	meta := requestMeta(r.Context())
	if err := s.db.DeleteStorageResource(id, p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		if errors.Is(err, database.ErrStorageInUse) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "hint": "GET /api/v1/admin/storage/" + id + "/mappings lists the apps it is mapped to; remove each mapping, then delete it"})
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/storage lists the storage resources with their ids"})
			return
		}
		log.Printf("deleting storage resource: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	ctx, cancel := storageContext(r)
	defer cancel()
	report, pending, err := s.dispatchStorageDeletion(ctx, res.Slug)
	if err != nil {
		// **The row is gone either way, and the tombstone keeps the work.** The
		// reconciler runs the destroy again — every step of it is safe to repeat
		// — so the answer is that it is under way, not that it failed.
		log.Printf("destroying storage %s: %v (the reconciler retries)", res.Slug, err)
		writeJSON(w, http.StatusAccepted, storageDeleteResponse{State: "deleting"})
		return
	}
	if pending {
		writeJSON(w, http.StatusAccepted, storageDeleteResponse{State: "deleting", DestroyReport: report})
		return
	}
	writeJSON(w, http.StatusOK, storageDeleteResponse{State: "deleted", DestroyReport: report})
}

// storageDeleteResponse is what a delete did: "deleted" when the data is
// already gone or was never going to be touched, "deleting" while the cluster
// finishes and the slug is held.
type storageDeleteResponse struct {
	State string `json:"state"`
	runtime.DestroyReport
}

// dispatchStorageDeletion runs the destroy for one tombstone and records what
// it did. It reports pending when the cluster still has volumes to delete;
// otherwise the delete is finished here and the slug freed.
//
// The handler and the reconciler both come through here, which is what makes a
// destroy cut short — by a restart, by a cluster that timed out — the same code
// path as one that was never interrupted.
//
// **One pass per slug at a time** (AP-4): the reconciler's tick can land while
// the handler is still inside DestroyStorage, and two passes racing on one
// tombstone was how a flipped volume got forgotten. The volumes a pass flipped
// are merged into the row, on success and on error alike, and "nothing
// pending" is read from the merged row rather than from this pass alone.
func (s *Server) dispatchStorageDeletion(ctx context.Context, slug string) (runtime.DestroyReport, bool, error) {
	unlock := lockStorageDeletion(slug)
	defer unlock()
	all, err := s.db.ListStorageDeletions()
	if err != nil {
		return runtime.DestroyReport{}, false, err
	}
	var d *database.StorageDeletion
	for i := range all {
		if all[i].Slug == slug {
			d = &all[i]
		}
	}
	if d == nil {
		return runtime.DestroyReport{}, false, nil
	}
	report, err := s.storage.DestroyStorage(ctx, runtime.StorageResource{
		Slug: d.Slug, Kind: d.Kind, Backing: d.Backing, StorageClass: d.StorageClass, Root: d.Root,
	}, d.Claims)
	if err != nil {
		// What this pass flipped before it failed is recorded, report left
		// NULL, so the next pass — which will find those claims released —
		// still waits for their volumes.
		if rerr := s.db.AddStorageDeletionVolumes(d.Slug, report.Pending); rerr != nil {
			log.Printf("recording the volumes a cut-short destroy of %s flipped: %v", d.Slug, rerr)
		}
		return report, false, err
	}
	// A volume made before every claim was recorded may have folders the
	// sweep released back then, which nothing names now. On a host root they
	// were under the resource's directory and went with it.
	if d.PredatesClaims && d.Kind == runtime.MountVolume && d.Root == "" {
		report.LeftBehind = append(report.LeftBehind, runtime.LeftBehind{
			What: "folders unmapped before this version",
			Reason: "folders whose last app was detached before Toft recorded every claim may remain " +
				"on the file system; nothing names them",
		})
	}
	volumes, err := s.db.SetStorageDeletionReport(d.Slug, report.Pending, report)
	if err != nil {
		return report, false, err
	}
	if len(volumes) > 0 {
		return report, true, nil
	}
	return report, false, s.db.FinishStorageDeletion(d.Slug)
}

// storageDeletionLocks serializes dispatchStorageDeletion per slug, in this
// process. Entries are never removed: a slug's mutex is a few bytes, and
// deleting one while another goroutine holds it would let a third take a
// fresh one beside it.
var storageDeletionLocks sync.Map

func lockStorageDeletion(slug string) (unlock func()) {
	m, _ := storageDeletionLocks.LoadOrStore(slug, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ensureBinding records a claim and then makes it exist. **Recorded first**: a
// claim the cluster created and nothing recorded is exactly what the resource
// delete could not find to destroy. Every EnsureStorage for a mapping or a
// create comes through here, so a new call site cannot forget.
//
// A volume's claims are all recorded; a store's only when folder-scoped, since
// its root bindings are the closed set RemoveStorage enumerates. A store's
// folder binding left behind would otherwise be inherited by the next store
// of the same name (AP-1), so the delete has to be able to find it.
func (s *Server) ensureBinding(ctx context.Context, res *database.StorageResource, access, folder string) error {
	binding := storageResourceFor(res, access, folder)
	if binding.Kind == runtime.MountVolume || folder != "" {
		claim := runtime.BindingName(binding.Slug, binding.Kind, binding.Access, binding.Folder)
		// A volume's folder is a directory of its one claim (K8-D1), so the
		// claim it records is the root's whatever the folder.
		claimFolder := folder
		if binding.Kind == runtime.MountVolume {
			claimFolder = ""
		}
		if err := s.db.RecordStorageClaim(res.ID, claim, claimFolder); err != nil {
			return fmt.Errorf("recording claim %s: %w", claim, err)
		}
	}
	return s.storage.EnsureStorage(ctx, binding)
}

// --- Mappings ---

func (s *Server) handleListStorageMappings(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	id := chi.URLParam(r, "id")
	res, err := s.db.GetStorageResource(id)
	if err != nil || res == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/storage lists the storage resources with their ids"})
		return
	}
	mappings, err := s.db.ListStorageMappingsForResource(id)
	if err != nil {
		log.Printf("listing storage mappings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, mappings)
}

type putStorageMappingRequest struct {
	Path   string `json:"path"`
	Access string `json:"access"`
	// Folder is which directory of the resource to mount, and absent is the
	// whole resource. It is part of the mapping's identity, so a second PUT
	// naming a different folder attaches a second mount rather than moving the
	// first.
	Folder string `json:"folder"`
}

// handlePutStorageMapping maps a resource onto an app, or moves an existing
// mapping.
//
// It takes effect on the app's *next* deployment and never on a running pod.
// That is not a limitation to be worked around: a mount appearing under a
// running process is exactly the kind of change that produces a bug nobody can
// reproduce, and the deploy is the moment the platform already has for
// "everything about this app changes at once."
func (s *Server) handlePutStorageMapping(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	// The license gate: writing new configuration here needs one. Reads are
	// untouched so the page renders, removal is untouched so nothing is
	// trapped, and what is already configured keeps being honoured.
	if s.refusedUnlicensed(w, r, entitlements.FeatureAppStorage) {
		return
	}
	p := auth.GetPrincipal(r.Context())

	res, ok := s.storageResourceOr404(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	var req putStorageMappingRequest
	if !decodeStrictBody(w, r, &req) {
		return
	}

	// The same validator the delegate's route and the manifest use. The
	// binding for this level and this folder has to exist before an app is
	// told it has one — a mapping whose claim is absent produces a pod stuck
	// Pending with an event nobody reads — and the cap is asked before it.
	entry, refusal, err := s.validateAttach(app, res,
		attachEntry{Path: req.Path, Access: req.Access, Folder: req.Folder},
		attachOptions{NameApps: true, CheckRoom: true})
	if err != nil {
		log.Printf("validating attach: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if refusal != nil {
		writeAttachRefusal(w, refusal)
		return
	}
	path, access, folder := entry.Path, entry.Access, entry.Folder
	if s.refuseManifestMapping(w, res, app.ID, &folder) {
		return
	}

	ctx, cancel := storageContext(r)
	defer cancel()
	if err := s.ensureBinding(ctx, res, access, folder); err != nil {
		log.Printf("binding storage %s at %s: %v", res.Slug, access, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "the cluster refused the binding for this access level: " + err.Error()})
		return
	}

	mapping := &database.StorageMapping{
		ResourceID: res.ID, AppID: app.ID, Path: path, Access: access, Folder: folder,
		// Stamped `admin`, and an administrator's write **adopts** what it
		// touches: the stamp is the authority of the last write, so a mapping
		// an administrator has edited never disappears with somebody else's
		// delegation being revoked.
		Authority: database.AuthorityAdmin,
	}
	meta := requestMeta(r.Context())
	if err := s.db.PutStorageMapping(mapping, p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		if mappingRuleRefused(w, err) {
			return
		}
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this app already mounts something at that path", "code": attachPathTaken,
				"hint": "send a \"path\" of its own, or leave path out for /storage/<name>[/<folder>]"})
			return
		}
		log.Printf("mapping storage: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.rebindAfterPut(ctx, res, access, folder, app.Slug)

	writeJSON(w, http.StatusOK, map[string]any{
		"mapping": mapping,
		// Said plainly rather than left to be discovered. The admin has just
		// changed what an app can reach and nothing about the running app moved.
		"note": storageTakesEffectNote(app, s.sharedStorageOffered()),
	})
}

func (s *Server) handleDeleteStorageMapping(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())

	folder, ok := storageFolderQuery(w, r)
	if !ok {
		return
	}
	removed, err := s.db.DeleteStorageMapping(chi.URLParam(r, "id"), chi.URLParam(r, "appID"),
		folder, p.ID, meta.RequestID, meta.IP, meta.UserAgent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Named, with the read that shows what is mapped (BB-068): a bare
			// "not found" did not say whether the resource, the app or the
			// folder was the missing one.
			msg := "this app has no mapping to that resource"
			if folder != nil {
				msg = fmt.Sprintf("this app has no mapping to that resource at folder %q", *folder)
			}
			writeJSON(w, http.StatusNotFound, map[string]string{"error": msg, "code": "not_found",
				"hint": "toft api GET admin/storage/" + chi.URLParam(r, "id") + "/mappings lists what the resource is mapped to, folder by folder"})
			return
		}
		log.Printf("unmapping storage: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.sweepStorageBindings(r, chi.URLParam(r, "id"), removed)
	body := map[string]any{"removed": len(removed)}
	// No folder named is "detach this resource from this app", every folder
	// of it — the documented identity rule — and the answer says which
	// folders went and how to name one, so a caller who meant one learns the
	// selector rather than discovering the rest gone (BB-046).
	if folder == nil && len(removed) > 1 {
		folders := make([]string, 0, len(removed))
		for _, m := range removed {
			folders = append(folders, strconv.Quote(m.Folder))
		}
		body["note"] = "No folder was named, so every mapping of this resource on the app was removed (folders " +
			strings.Join(folders, ", ") + "). ?folder=<name> removes one, and ?folder= the whole-resource one."
	}
	for _, m := range removed {
		if m.Source == database.MappingSourceManifest {
			// Allowed, and said: the next deploy restores it while the owner
			// can attach it, so a detach that is meant to last is a
			// revocation or an edit to toft.toml.
			body["note"] = "This mapping is declared in the app's toft.toml, so the next deploy " +
				"restores it while the app's owner can attach it. To keep it detached, revoke the owner's " +
				"delegation or remove the entry from toft.toml."
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// --- The app's own view ---

type appStorageResponse struct {
	Mappings []database.StorageMapping `json:"mappings"`
	// Pending is true when the app's mappings differ from what its running
	// release was started with — which, since mappings only take effect on a
	// deployment, is the question an owner actually has.
	Pending bool   `json:"pending"`
	Note    string `json:"note,omitempty"`
}

// handleAppStorage lets anyone who can read the app see what it mounts.
//
// Read-only on purpose: an owner needs to know their app has /data/reports and
// what it may do there, and must not be able to grant themselves another one.
func (s *Server) handleAppStorage(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorage(w) {
		return
	}
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	if d := auth.Authorize(p, auth.ActionRead, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, false)
		return
	}

	mappings, err := s.db.ListStorageMappingsForApp(appID)
	if err != nil {
		log.Printf("listing app storage: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := appStorageResponse{Mappings: mappings}
	if len(mappings) > 0 {
		shared := s.sharedStorageOffered()
		resp.Note = storageTakesEffectNote(app, shared)
		// The running release is only carrying these mounts if it was started
		// as an isolated workload, or as a shared process on an install whose
		// storage roots reach shared mode. Anything else means a deploy is owed.
		resp.Pending = app.RunningModeOrShared() != runtime.ModeIsolated && !shared
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Helpers ---

// mappingRuleRefused answers a mapping write the database refused on a rule
// this file states too — a race past validateAttach's folder rules, a gate range with
// nothing left, or a folder whose gate name another folder holds — and reports whether it did.
func mappingRuleRefused(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, database.ErrGateRangeExhausted):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error() +
			" — an operator widens SHARED_STORAGE_GID_RANGE", "hint": "ask an operator to widen SHARED_STORAGE_GID_RANGE, then try again"})
	case errors.Is(err, database.ErrGateNameTaken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(),
			"code": "gate_name_taken", "hint": `send a different "folder"`})
	case errors.Is(err, database.ErrAppMappingCap):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error() +
			", counting every folder and every whole resource — detach one first", "hint": "`toft storage ls --app <slug>` lists what the app has attached; detach one before adding another"})
	case strings.Contains(err.Error(), "is partitioned, so a mapping names a folder"),
		strings.Contains(err.Error(), "which is already mapped on partitioned resource"):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "another mapping on this resource was written at the same moment and this folder now " +
				"overlaps it — reload and try again", "hint": "reload the resource's mappings and pick a folder that does not overlap one already mapped"})
	default:
		return false
	}
	return true
}

func storageResourceFor(res *database.StorageResource, access, folder string) runtime.StorageResource {
	return runtime.StorageResource{
		Slug:         res.Slug,
		Kind:         res.Kind,
		Backing:      res.Backing,
		StorageClass: res.StorageClass,
		Root:         res.Root,
		Bucket:       res.Bucket,
		Prefix:       res.Prefix,
		Access:       access,
		Folder:       folder,
	}
}

// storageFolderQuery reads the folder a DELETE is pointed at.
//
// **Absent and empty are different**, and keeping them apart is what makes the
// old route mean what it always meant: no `folder` parameter at all is "detach
// this resource from this app", which before folders was the only thing a
// DELETE could say and is what a caller written against it intended. An empty
// one names the root mapping specifically. Collapsing them would leave an app
// holding mounts whose owner believed they were gone.
func storageFolderQuery(w http.ResponseWriter, r *http.Request) (*string, bool) {
	raw, present := r.URL.Query()["folder"]
	if !present {
		return nil, true
	}
	folder, ok := validatedFolder(w, raw[0])
	if !ok {
		return nil, false
	}
	return &folder, true
}

// sweepStorageBindings releases the bindings the mappings just removed were the
// last users of.
//
// **Off the request path's transaction, and never able to fail it.** The rows
// are the authorization and they are already committed; holding a transaction
// open across cluster I/O would make one administrator's detach fail because of
// another app's cluster state, which is the trade the revocation path already
// refused. A sweep that fails leaves an orphaned binding object — and never a
// byte, because the reclaim policy is Retain.
//
// **Only folder-scoped bindings are swept.** A resource's own root bindings are
// created with the resource, are at most four, and are released by
// RemoveStorage when it is deleted; sweeping them here would delete a binding
// the next mapping is about to ask for.
func (s *Server) sweepStorageBindings(r *http.Request, resourceID string, removed []database.StorageMapping) {
	s.sweepBindings(r.Context(), resourceID, removed)
}

// sweepBindings is sweepStorageBindings off a request — the deploy's flip,
// removing what toft.toml no longer declares.
func (s *Server) sweepBindings(parent context.Context, resourceID string, removed []database.StorageMapping) {
	if s.storage == nil || len(removed) == 0 {
		return
	}
	res, err := s.db.GetStorageResource(resourceID)
	if err != nil || res == nil {
		return
	}
	// **Never while a deploy is in flight** (AP-3). A deploy binds at plan time
	// and records its mapping only at the flip, minutes later, so a count of
	// committed rows cannot see a binding a building release already mounts.
	// Skipping leaves an orphan, which is harmless — Retain, and the resource
	// delete releases a store's recorded folder bindings — where releasing a
	// live one leaves the next start of that app Pending. The flip's own sweep
	// meets its own building row and so always defers to the delete.
	active, err := s.db.ListActiveDeployments()
	if err != nil {
		log.Printf("listing deployments before a binding sweep of %s: %v", res.Slug, err)
		return
	}
	if len(active) > 0 {
		return
	}
	for _, m := range removed {
		// **A volume's claim lives as long as the volume**, and its folders are
		// subPaths inside it that name no claim of their own (K8-D1). Only a
		// store's folder bindings are swept: a store is released, never
		// destroyed.
		if m.Folder == "" || res.Kind == runtime.MountVolume {
			continue
		}
		// Reference-counted rather than assumed: a mapping moved from one
		// folder to another, or a second app mounting the same folder, means
		// this binding is still somebody's mount.
		inUse, err := s.db.CountMappingsForFolder(resourceID, m.Folder)
		if err != nil {
			log.Printf("counting mappings for %s folder %q: %v", res.Slug, m.Folder, err)
			continue
		}
		if inUse > 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(parent, storageCallTimeout)
		if err := s.storage.RemoveStorage(ctx, storageResourceFor(res, m.Access, m.Folder)); err != nil {
			log.Printf("releasing binding for %s folder %q: %v", res.Slug, m.Folder, err)
		}
		cancel()
	}
}

// rebindAfterPut ensures an attach's binding once more after its mapping row
// is committed. Between the first ensure and the row a detach of the same
// folder elsewhere could count no rows and release the binding (AP-3); once
// the row exists no sweep will, so asking again here closes the window. A
// failure is logged, not returned: the mapping stands, and a claim still being
// released is the platform's to sort out, not the caller's.
func (s *Server) rebindAfterPut(ctx context.Context, res *database.StorageResource, access, folder, appSlug string) {
	if s.storage == nil {
		return
	}
	if err := s.ensureBinding(ctx, res, access, folder); err != nil {
		log.Printf("re-binding storage %s at %s for %s after the mapping was written: %v", res.Slug, access, appSlug, err)
	}
}

// validatedFolder is the single answer to "is this a folder", and every route
// that takes one goes through it.
//
// **Validate first, normalize second, and never the other way round.** Trimming
// a leading slash before validating would silently accept `/escape` — a string
// `ValidateStorePath` refuses by name, and the one shape a traversal attempt
// actually looks like — while the listing route went on refusing it. The three
// callers have to accept and refuse the identical set, or the folder means one
// thing where it is written and another where it is read.
//
// The trailing slash comes off afterwards because a folder is stored without
// one: `storePathPrefix` owns putting it back, and a slash in the column would
// be a second representation of one fact.
func validatedFolder(w http.ResponseWriter, raw string) (string, bool) {
	folder := strings.TrimSpace(raw)
	if err := ValidateStorePath(folder); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "folder: " + err.Error(), "hint": `send a folder relative to the resource, like "2026/reports", with no leading slash and no ".." segment`})
		return "", false
	}
	return strings.TrimSuffix(folder, "/"), true
}

// storageTakesEffectNote says when a mapping change reaches the app, and what
// is standing in the way if something is. sharedOffered is
// sharedStorageOffered(): on an install whose storage roots reach shared mode
// a shared app mounts the resource like an isolated one, and warning that its
// deploy will fail there is a false alarm the deploy then contradicts.
func storageTakesEffectNote(app *database.App, sharedOffered bool) string {
	switch {
	case sharedOffered && app.DesiredMode() != runtime.ModeIsolated:
		return "Takes effect at the app's next start — a restart or a deployment — and at each job's next " +
			"run; the running process keeps what it started with. This install's storage roots reach " +
			"shared-mode apps, so the app needs no change of mode."
	case app.DesiredMode() != runtime.ModeIsolated:
		return "This app is set to shared mode, so its next deploy will fail: durable storage " +
			"is attached below a workload's own isolation boundary, which a shared process does " +
			"not have. Promote the app to isolated mode first."
	case app.RunningModeOrShared() != runtime.ModeIsolated:
		return "Takes effect at the app's next deployment, and at each job's next run. The app is " +
			"currently running as a shared process and will move to its own workload when it is next deployed."
	default:
		// A job run is a workload of its own, described when it starts, so it
		// reads the mappings of that moment: an every-minute firing saw a new
		// mapping a minute later, with no deploy between (docs/bugs
		// 2026-10-28 D12). Only the running app waits for a deployment.
		return "Takes effect at the app's next start — a restart or a deployment — and at each job's " +
			"next run; a mapping change never alters a running workload."
	}
}

// explainKindUnavailable names the configuration a kind needs, so an admin
// reads a decision rather than a malfunction.
func explainKindUnavailable(kind string) string {
	if kind == database.StorageKindVolume {
		return "Volumes are not offered on this install. On a cluster they need a StorageClass " +
			"backed by a shared filesystem — an operator sets KUBE_STORAGE_CLASS to a class created " +
			"with provisioningMode: efs-ap and reclaimPolicy: Retain. With no cluster they need a " +
			"root an operator mounted, declared in SHARED_STORAGE_ROOTS."
	}
	return "Object stores are not offered on this install. On a cluster they need the Mountpoint " +
		"CSI driver (KUBE_STORE_CSI_DRIVER names it if it is installed under a different name). " +
		"With no cluster they need the platform to be able to sign into the bucket: an operator " +
		"sets STORE_UPLOADS=1 and grants the platform's credential access to it."
}

// explainBackingUnavailable is the same sentence one level down: the kind is on
// offer and this way of backing it is not. The prerequisites differ per backing,
// which is one of the two things the stored backing exists to tell the platform.
func explainBackingUnavailable(backing string) string {
	if backing == database.StorageBackingS3Files {
		return "Amazon S3 Files is not offered on this install. It needs a StorageClass backed " +
			"by an S3 Files file system — an operator sets KUBE_S3FILES_STORAGE_CLASS to a class " +
			"created against it, through the EFS CSI driver, with reclaimPolicy: Retain. The file " +
			"system, its mount targets and its bucket all pre-exist; Toft binds to them."
	}
	if backing == database.StorageBackingHost {
		return "Storage under a host mount is not offered on this install. It needs a root an " +
			"operator mounted and declared — SHARED_STORAGE_ROOTS, as name=/path or " +
			"name=/path:ro. Toft creates a directory under the root and never the mount."
	}
	if backing == database.StorageBackingEFS {
		return "EFS-backed file stores are not offered on this install. They need a StorageClass " +
			"an operator created with provisioningMode: efs-ap and reclaimPolicy: Retain, set as " +
			"KUBE_STORAGE_CLASS — which needs a cluster."
	}
	return "That backing is not offered on this install."
}

// bucketRequiredFor asks for the bucket in the words of the thing being made.
//
// **The S3 Files sentence carries a warning the platform cannot enforce.**
// Nothing here can check that the named bucket is the one behind the named file
// system: that would need an AWS call the platform holds no credential for, and
// naming the wrong one produces a Files page listing somebody else's objects.
// Copy and an audit entry are the whole mitigation, which is weaker than this
// product usually accepts and is said out loud rather than discovered.
func bucketRequiredFor(backing string) string {
	if backing == database.StorageBackingBlob {
		return "a store needs the container it lives in — Toft binds to it and never creates it"
	}
	if backing == database.StorageBackingS3Files {
		return "an S3 Files file store needs the bucket its file system is built on — this is " +
			"what people reach from the browser, and Toft cannot verify it is the same " +
			"bucket the file system mounts. Name the wrong one and this page will list another " +
			"bucket's objects."
	}
	return "a store needs the bucket it lives in — Toft binds to it and never creates it"
}

// containerWord is Azure's word for a bucket, and it exists for the reason the
// column is still called `bucket`: the name in the database crosses the API, the
// CLI and the audit log and is not worth a migration, but a refusal an
// administrator reads should use the word on the form in front of them.
func containerWord(backing string) string {
	if backing == database.StorageBackingBlob {
		return "container"
	}
	return "bucket"
}

// knownRoot and rootsAvailable are the two things ever asked of a roots list:
// whether this is one, and what to say when it is not.
func knownRoot(roots []runtime.StorageRoot, name string) bool {
	for _, r := range roots {
		if r.Name == name {
			return true
		}
	}
	return false
}

// rootNames is the declared roots' names, for a hint.
func rootNames(roots []runtime.StorageRoot) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		out = append(out, r.Name)
	}
	return out
}

// offeredHint is the next step for a create field the install answers from a
// list: the values it offers, or that an operator has to offer one first.
func offeredHint(field string, offered []string) string {
	if len(offered) == 0 {
		return "this install offers none — an operator configures storage first"
	}
	quoted := make([]string, 0, len(offered))
	for _, o := range offered {
		quoted = append(quoted, strconv.Quote(o))
	}
	return "send " + field + " as " + strings.Join(quoted, " or ") + ", or leave it out when the install offers one"
}

func rootsAvailable(roots []runtime.StorageRoot) string {
	if len(roots) == 0 {
		return ". This install declares none — an operator sets SHARED_STORAGE_ROOTS"
	}
	names := make([]string, 0, len(roots))
	for _, r := range roots {
		if r.ReadOnly {
			names = append(names, r.Name+" (read-only)")
			continue
		}
		names = append(names, r.Name)
	}
	return ". This install declares " + strings.Join(names, ", ")
}

// offersKind is the only question ever asked of a kinds list.
func offersKind(kinds []string, kind string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// isUniqueViolation distinguishes "that name is taken" from a real failure, so
// a collision answers 409 rather than 500. Matched on the driver's message for
// the same reason the tag and cron handlers do: pgx surfaces it as a text
// constraint error and there is one phrase to match.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}

// storageContext bounds a cluster call made on a request's behalf. Deliberately
// derived from the request: an admin who navigates away should not leave a
// binding call running against the API server.
func storageContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), storageCallTimeout)
}

// storageCallTimeout is a single API call against the platform's own namespace.
// A slow one means the API server is in trouble, and waiting longer helps
// nobody.
const storageCallTimeout = 30 * time.Second

// defaultBackingOffered is the backing a create naming none gets: the kind's
// historical default where this install offers it, and otherwise the one
// backing it does offer. A shared install offers a volume only under a host
// root, so the historical `efs` refused every volume created without the
// field — with a sentence about EFS, on an install that has none, to a caller
// who never mentioned it. Several on offer and the default not among them is
// `host` when that is one of them — a shared install with S3 Files beside its
// roots, where a host directory is the volume that needs nothing more — and
// otherwise "", which the handler refuses as storage_backing_required naming
// the choices; picking efs there was the EFS sentence again (BB-051, fifth
// 2026-11-03 black-box list).
func defaultBackingOffered(kind string, offered []string) string {
	def := database.DefaultStorageBacking(kind)
	switch {
	case len(offered) == 0 || offersKind(offered, def):
		return def
	case len(offered) == 1:
		return offered[0]
	case offersKind(offered, database.StorageBackingHost):
		return database.StorageBackingHost
	}
	return ""
}

// writeStorageDirectoryKept refuses a name whose deleted volume's files are
// still on the file system. The class's access points are rooted at the
// claim's name, so a new volume of that name would be handed them — another
// team's files, as far as anything here can tell (ADR §12a).
func writeStorageDirectoryKept(w http.ResponseWriter, name, class string) {
	on := "the install's default storage class"
	if class != "" {
		on = "storage class “" + class + "”"
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": "a deleted volume called “" + name + "” left its files at /lpv-" + name + " on " + on +
			", and a new volume of that name would be handed them",
		"code": "storage_directory_kept",
		"hint": "choose another name, or remove that directory on the file system and then release the name " +
			"(DELETE /api/v1/admin/storage/deleting/" + name + ", or Admin → Storage)"})
}

// handleReleaseKeptStorageName frees a name a finished delete held because its
// volume's files were kept. The administrator is saying the directory has
// been dealt with — removed, or meant for whoever creates the name next.
func (s *Server) handleReleaseKeptStorageName(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	slug := chi.URLParam(r, "slug")
	meta := requestMeta(r.Context())
	if err := s.db.ReleaseKeptStorageName(slug, p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "no deleted resource holds “" + slug + "” with its files kept",
				"hint":  "GET /api/v1/admin/storage lists held names under deleting; one still being deleted is freed by the cluster, not by this route"})
			return
		}
		log.Printf("releasing storage name %s: %v", slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
