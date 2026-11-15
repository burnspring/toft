package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"unicode"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
)

// attachEntry is one mapping as either caller states it: the console's request
// body, or one `[[storage]]` entry of toft.toml.
type attachEntry struct {
	Path   string
	Access string
	Folder string
}

// attachRefusal is why validateAttach said no — a reason code the builder
// turns into a refusal, and the status and sentence the console answers with.
// The two callers share the sentence, so a toml entry is refused in the words
// the console uses for the same input.
type attachRefusal struct {
	Code    string
	Status  int
	Message string
	// Hint is the next step, where the refusal knows a better one than its
	// code's.
	Hint string
	// Extra is any field the console's body already carried beside "error"
	// (the static app's `code`), kept so the console's responses do not move.
	Extra map[string]string
}

// The reason codes. They are the manifest's (`toft check`, the deploy's
// refusal); the console reads Status and Message.
const (
	attachPathInvalid      = "storage_path_invalid"
	attachLevelUnsupported = "storage_level_unsupported"
	attachOnStaticApp      = "storage_on_static_app"
	attachFolderInvalid    = "storage_folder_invalid"
	attachFolderRequired   = "storage_folder_required"
	attachFolderOverlaps   = "storage_folder_overlaps"
	attachAboveCeiling     = "storage_level_above_ceiling"
	attachCapReached       = "storage_cap_reached"
	attachPathTaken        = builder.StoragePathTaken
)

type attachOptions struct {
	// Ceiling is the highest level the authority behind this attach reaches;
	// empty is the kind's own ladder, an administrator's.
	Ceiling string
	// NameApps lets the partition overlap name the apps on the other folder —
	// an administrator may read them, a delegate may not.
	NameApps bool
	// CheckRoom asks the per-app cap against the table. The manifest counts its
	// whole plan instead, so it asks for the cap once rather than per entry.
	CheckRoom bool
	// ExceptManifestOf leaves this app's own manifest rows out of the
	// partition overlap: the flip removes what the new file no longer
	// declares before it writes, so a folder moving from `team` to
	// `team/2026` in one deploy was refused against the row it replaces
	// (code review 2026-11-11, B-5).
	ExceptManifestOf string
}

// validateAttach is every rule an attach applies before it writes, for both
// callers: the console's two routes and the manifest's plan (ADR § Storage
// somebody else can attach). It returns the entry normalized — the level
// defaulted, the path trimmed, the folder without its trailing slash — or the
// refusal. An error is the database failing, never the input.
//
// **One validator, because two drift**, and the first drift would be a toml
// passing something the console refuses.
func (s *Server) validateAttach(app *database.App, res *database.StorageResource, e attachEntry, opt attachOptions) (attachEntry, *attachRefusal, error) {
	e.Path = strings.TrimSpace(e.Path)
	// A path named by nobody is where toft.toml would put it, /storage/<name>
	// or /storage/<name>/<folder>: the console's body took a path as
	// required and refused an empty one as "not absolute" (BB-046, fourth
	// 2026-11-03 black-box list). Code reads TOFT_STORAGE by name, so the
	// default is a default and not a contract.
	if e.Path == "" {
		e.Path = builder.DefaultStoragePath(res.Slug, strings.Trim(strings.TrimSpace(e.Folder), "/"))
	}
	if err := database.ValidateMountPath(e.Path); err != nil {
		return e, &attachRefusal{Code: attachPathInvalid, Status: http.StatusBadRequest, Message: err.Error()}, nil
	}
	// The level a shared root can serve is decided by how the operator
	// mounted it, not by Toft (checkRootLevel, ADR §146): a read-write root
	// serves full and nothing else, a read-only one read. So an entry that
	// names no level takes the root's, and one naming a level the root cannot
	// serve is refused here — before a build — rather than at the app's start,
	// where it used to fail after `toft check` and the upload had both passed
	// (docs/bugs 2026-11-03 #1).
	if root, ok := s.sharedRootOf(res); ok {
		serves := runtime.AccessFull
		if root.ReadOnly {
			serves = runtime.AccessRead
		}
		if e.Access == "" {
			e.Access = serves
		} else if e.Access != serves {
			how := "read-write, so it serves full"
			if root.ReadOnly {
				how = "read-only, so it serves read"
			}
			return e, &attachRefusal{Code: attachLevelUnsupported, Status: http.StatusBadRequest,
				Message: fmt.Sprintf("%q lives under storage root %q, which is mounted %s and nothing else — "+
					"on this install a level is which mount the resource is under, not a permission Toft "+
					"applies. Attach it at %s (or leave the level out), or ask an operator for a %s mount "+
					"and point the resource at that root",
					res.Name, root.Name, how, serves, map[bool]string{true: "read-write", false: "read-only"}[root.ReadOnly]),
				Hint: fmt.Sprintf(`send "access": %q, or leave access out`, serves)}, nil
		}
	}
	if e.Access == "" {
		e.Access = runtime.AccessRead
	}
	if !database.ValidStorageAccess(res.Kind, e.Access) {
		return e, &attachRefusal{Code: attachLevelUnsupported, Status: http.StatusBadRequest,
			Message: fmt.Sprintf("a %s offers %s — %q is not one of them%s",
				res.Kind, strings.Join(database.StorageAccessLevels(res.Kind), ", "), e.Access,
				database.StorageAccessRefusal(e.Access))}, nil
	}
	// A static app is a folder of files the platform serves: no process and no
	// job to mount anything into, which is why its [jobs] are refused too.
	// Accepted, the mapping said "takes effect on the next deployment" — which
	// never came — and took one of the app's five places.
	if app.Framework == frameworkStatic {
		return e, &attachRefusal{Code: attachOnStaticApp, Status: http.StatusConflict,
			Message: "this is a static app: a folder of files the platform serves, with no process " +
				"or job to mount storage into. Deploy it as an app in a language Toft supports to give it storage.",
			Hint:  "attach the storage to an app with a process or jobs; to give this one storage, redeploy it in a language Toft runs",
			Extra: map[string]string{"code": "static_app"}}, nil
	}
	// The ladder's attach-time half: a level the driver cannot express as a
	// mount is refused rather than approximated. Today that is `write` on a
	// blob store — blobfuse2 cannot create while forbidding a clobber, and
	// mounting read-write instead would be a silent grant of `full`. `write`
	// stays available over a signed URL (ADR §82), which the sentence says.
	if !database.ValidStorageMappingAccess(res.Kind, res.Backing, e.Access) {
		return e, &attachRefusal{Code: attachLevelUnsupported, Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"%q is backed by %s, which mounts through blobfuse2 — a driver with no "+
					"allow-overwrite and no allow-delete, so it can mount %s and nothing between "+
					"them. %q cannot be a mount here. It is still available over a signed URL "+
					"(`toft store put`), where the permission can be expressed exactly.",
				res.Name, res.Backing,
				strings.Join(database.StorageMountableAccessLevels(res.Kind, res.Backing), " or "),
				e.Access)}, nil
	}
	// The folder goes through the listing path's validator — two copies of
	// "no `..`" is how a traversal comes to be refused by one caller and
	// permitted by another — validated first and normalized second, so
	// `/escape` is refused by name rather than trimmed into acceptance.
	folder := strings.TrimSpace(e.Folder)
	if err := ValidateStorePath(folder); err != nil {
		return e, &attachRefusal{Code: attachFolderInvalid, Status: http.StatusBadRequest,
			Message: "folder: " + err.Error()}, nil
	}
	e.Folder = strings.TrimSuffix(folder, "/")
	// A blob folder lands in a blobfuse2 argument, and the blob CSI driver
	// splits its mount options on spaces into argv — so a space here is not a
	// character of the folder but the start of a second flag, and a second
	// `--subdirectory=` moves the mount out of the store's prefix. Other
	// backings keep spaces; their folder never reaches a command line.
	if res.Backing == database.StorageBackingBlob && hasSpace(e.Folder) {
		return e, &attachRefusal{Code: attachFolderInvalid, Status: http.StatusBadRequest,
			Message: fmt.Sprintf("folder: %q is backed by Azure Blob, whose mount cannot carry whitespace in a folder name", res.Name)}, nil
	}

	// A partitioned resource's two rules, asked here so the refusal can name
	// what is in the way; the trigger underneath holds both. The overlap names
	// the apps only for an administrator, who may read them.
	if res.Partitioned {
		if e.Folder == "" {
			return e, &attachRefusal{Code: attachFolderRequired, Status: http.StatusUnprocessableEntity,
				Message: fmt.Sprintf("%q is partitioned, so it is mapped by folder — name the folder this app gets", res.Name),
				Hint:    `name the app's folder, like "folder": "team-a" — a partitioned resource has no whole-resource mapping`}, nil
		}
		other, apps, err := s.db.PartitionConflict(res.ID, e.Folder, opt.ExceptManifestOf)
		if err != nil {
			return e, nil, fmt.Errorf("checking folder %q of %s: %w", e.Folder, res.Slug, err)
		}
		if other != "" {
			to := ""
			if opt.NameApps {
				to = " (to " + strings.Join(apps, ", ") + ")"
			}
			return e, &attachRefusal{Code: attachFolderOverlaps, Status: http.StatusUnprocessableEntity,
				Message: fmt.Sprintf("%q overlaps %q, which is already mapped on %q%s. A partitioned "+
					"resource's folders never sit inside one another, because each is its own gate — map %q "+
					"itself to share it, or pick a folder beside it.",
					e.Folder, other, res.Name, to, other),
				Hint: fmt.Sprintf(`send "folder": %q to share it, or a folder that is neither inside nor above it`, other)}, nil
		}
	}
	if opt.Ceiling != "" && !database.StoreAccessAtLeast(opt.Ceiling, e.Access) {
		return e, &attachRefusal{Code: attachAboveCeiling, Status: http.StatusBadRequest,
			Message: fmt.Sprintf("you may attach %q at %s at most, not %s", res.Name, opt.Ceiling, e.Access),
			Hint:    fmt.Sprintf("attach it at %s, or ask an administrator to raise your delegation on %q", opt.Ceiling, res.Name)}, nil
	}
	if opt.CheckRoom {
		if ref, err := s.pathRefusal(app.ID, res.ID, e); err != nil || ref != nil {
			return e, ref, err
		}
		if err := s.db.StorageMappingHasRoom(res.ID, app.ID, e.Folder); err != nil {
			if errors.Is(err, database.ErrAppMappingCap) {
				return e, capRefusal(), nil
			}
			return e, nil, fmt.Errorf("storage mapping cap: %w", err)
		}
	}
	return e, nil, nil
}

// pathRefusal is the console's storage_path_taken: a path another of the app's
// mappings already holds, or one nesting with it. The equal half is UNIQUE
// (app_id, path) too, which answered only after the binding was made and
// without a code; the nested half nothing held, and both attaches were taken
// until every start failed on the inner link (BB-048, fifth 2026-11-03
// black-box list). The manifest asks the same of its plan in planStorage.
func (s *Server) pathRefusal(appID, resourceID string, e attachEntry) (*attachRefusal, error) {
	current, err := s.db.ListStorageMappingsForApp(appID)
	if err != nil {
		return nil, fmt.Errorf("reading this app's storage: %w", err)
	}
	for _, m := range current {
		if m.ResourceID == resourceID && m.Folder == e.Folder {
			continue // the mapping this PUT replaces
		}
		what := m.ResourceSlug
		if m.Folder != "" {
			what = fmt.Sprintf("folder %q of %s", m.Folder, m.ResourceSlug)
		}
		switch {
		case m.Path == e.Path:
			return &attachRefusal{Code: builder.StoragePathTaken, Status: http.StatusConflict,
				Message: fmt.Sprintf("this app already mounts %s at %s", what, m.Path),
				Hint:    `send a "path" of its own, or leave path out for /storage/<name>[/<folder>]`}, nil
		case database.MountPathsNest(m.Path, e.Path):
			return &attachRefusal{Code: builder.StoragePathTaken, Status: http.StatusConflict,
				Message: fmt.Sprintf("%s nests with %s, where this app mounts %s — one mount cannot sit "+
					"inside another, so no two of an app's mount paths sit one inside the other", e.Path, m.Path, what),
				Hint: fmt.Sprintf("detach %s first, or send a \"path\" that is not inside or above %s", what, m.Path)}, nil
		}
	}
	return nil, nil
}

func capRefusal() *attachRefusal {
	return &attachRefusal{Code: attachCapReached, Status: http.StatusUnprocessableEntity,
		Message: database.ErrAppMappingCap.Error() + ", counting every folder and every whole resource — detach one first",
		Hint:    "`toft storage ls --app <slug>` lists this app's mappings; detach one before attaching another"}
}

// writeAttachRefusal is the console's answer to a refusal: the sentence, the
// refusal's own code — the one toft check names for the same entry — and the
// next step. Without the code the envelope filled in the status's generic one,
// `invalid` or `bad_request`, which no document lists (BB-046, fourth
// 2026-11-03 black-box list).
func writeAttachRefusal(w http.ResponseWriter, ref *attachRefusal) {
	body := map[string]string{"error": ref.Message, "code": ref.Code}
	if ref.Hint != "" {
		body["hint"] = ref.Hint
	}
	for k, v := range ref.Extra {
		body[k] = v
	}
	writeJSON(w, ref.Status, body)
}

// refuseManifestMapping is D13: a mapping toft.toml declares is the deploy's to
// write, so the console's PUT on one is a 409 rather than an edit the next
// deploy silently reverts.
func (s *Server) refuseManifestMapping(w http.ResponseWriter, res *database.StorageResource, appID string, folder *string) bool {
	n, err := s.db.ManifestMappingsOn(res.ID, appID, folder)
	if err != nil {
		log.Printf("checking for a manifest mapping: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return true
	}
	if n == 0 {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": fmt.Sprintf("%q is attached to this app by its toft.toml, so it changes there: edit the "+
			"[[storage]] entry and deploy", res.Name),
		"code": "declared_in_manifest", "hint": "change the [[storage]] entry in toft.toml, then run `toft deploy`"})
	return true
}

// sharedRootOf is the operator's host root a volume lives under, on an install
// whose provisioner places volumes under roots (shared mode). False for a
// store — reached over signed URLs, never mounted there — and on a cluster.
func (s *Server) sharedRootOf(res *database.StorageResource) (runtime.StorageRoot, bool) {
	if res.Kind != database.StorageKindVolume || res.Root == "" {
		return runtime.StorageRoot{}, false
	}
	for _, r := range s.storageRoots() {
		if r.Name == res.Root {
			return r, true
		}
	}
	return runtime.StorageRoot{}, false
}

// hasSpace reports whether s holds any whitespace rune — the argv boundary of
// a blob store's mount options, refused in a blob folder and a blob prefix.
func hasSpace(s string) bool { return strings.IndexFunc(s, unicode.IsSpace) >= 0 }
