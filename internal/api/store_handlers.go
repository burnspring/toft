package api

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	gopath "path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"

	"toft/internal/artifact"
	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/hoststore"
	"toft/internal/runtime"
)

// Uploading to a store from outside the platform.
//
// A store has always been reachable by an app that mounts it. This is the other
// door: a person holding a grant, with no app and no filesystem, getting a URL.
//
// **The one rule this file must not break: the URL is never more generous than
// the mount.** The ladder is expressed twice now — once as Mountpoint options in
// internal/kube, once as signed conditions here — and a second expression that
// drifts upward is a permission escalation no test would notice, because
// neither expression is wrong on its own. presignFor is the only place the
// translation happens, and TestPresignLadderMatchesTheMount compares it to
// kube.StoreMountOptions rather than to a copied constant.
//
// The platform signs and never sees the bytes. That is deliberate — streaming
// arbitrary-size uploads through the control plane would need the same
// credential anyway — and it is why the audit record is the mint rather than
// the landing. See handleStoreUpload.

// storeURLTTL is how long a minted URL lives.
//
// Long enough for a slow link to finish a large file — a URL that expires
// mid-upload turns a working nightly into a mysterious failure near the end,
// the worst possible time. Short enough that a leaked URL is not indefinitely
// useful. Shorter than the artifact store's PresignTTL because there is no job
// here holding one open: a client asks, uploads, and is done.
const storeURLTTL = 1 * time.Hour

// maxSinglePutBytes is S3's ceiling on one PUT, and therefore this feature's.
//
// Refused by name, with the number, rather than left for S3 to reject at the
// 5 GiB mark with an error nobody can read. Multipart is three more routes —
// create, sign part, complete — and belongs to the first workload that needs
// it.
const maxSinglePutBytes int64 = 5 * 1024 * 1024 * 1024

// maxStoreListing is one page of a listing. S3's own maximum is 1000 and there
// is no reason to page more finely than the thing being paged.
const maxStoreListing int32 = 1000

// storeUploadsOffered reports whether this install can mint store URLs at all.
//
// Two conditions, because they are two different absences: no storage backend
// means there are no stores, and no signer means there is nothing to sign
// with — neither a bucket signer (the operator granted the platform a
// permission on their buckets) nor the host data plane. Either way the routes
// 404 — absent, not dormant. Which of the two serves a resource is
// dataPlaneOffered's question.
func (s *Server) storeUploadsOffered() bool {
	return s.storage != nil && (s.storeSigner != nil || s.hostStore != nil)
}

// dataPlaneOffered is the per-resource half: a bucket needs the bucket signer,
// a directory under a host root needs the host data plane. A resource whose
// signer is absent is not reachable from outside here, which is a 404 like any
// other store a caller does not reach.
func (s *Server) dataPlaneOffered(res database.StorageResource) bool {
	switch {
	case res.HasBucket():
		return s.storeSigner != nil
	case res.OnHostRoot():
		return s.hostStore != nil
	}
	return false
}

// signerFor is the signer for one resource, bound — on a host root — to the
// folder whose grant authorized the request and the gate a write takes. A
// bucket's signer needs neither: the bucket enforces the key it was signed
// for, and an app's files there are nobody's group.
func (s *Server) signerFor(r *http.Request, res database.StorageResource, scope string, gid int) artifact.Signer {
	if res.HasBucket() || !res.OnHostRoot() {
		return s.storeSigner
	}
	subject := ""
	if p := auth.GetPrincipal(r.Context()); p != nil {
		subject = p.ID
	}
	return s.hostStore.For(hoststore.Binding{Resource: res.ID, Root: res.Root, Slug: res.Slug,
		Scope: scope, Subject: subject, GID: gid})
}

// writeGate is the gid a host-root write takes: the gate holding the name, or
// 0 on a bucket, which has no gate.
//
// **A partitioned resource takes a write only inside a folder an app is mapped
// to, on every backing.** On a host root that is the gate's rule — beside the
// folders a file would be root's and no app's to open — and a bucket keeps it
// for the same reason rather than because it has to: a partitioned resource is
// its mapped folders, and an object beside them is in no app's mount. One rule
// on both is what lets a person, or a script, learn it once (ADR §12a).
//
// The refusal is a 403 with its own code, naming the mapped folders the caller
// can see — the ones their grants reach, never a sibling's — or saying that none
// is mapped, which is the state a newly created resource is in.
func (s *Server) writeGate(w http.ResponseWriter, scopes *database.StoreScopes, name string) (int, bool) {
	res := scopes.Resource
	if res.Partitioned {
		mapped, err := s.db.MappedFolders(res.ID)
		if err != nil {
			log.Printf("listing the mapped folders of %s: %v", res.Slug, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return 0, false
		}
		if !database.InsideMappedFolder(mapped, name) {
			writeJSON(w, http.StatusForbidden, outsideMappedFolders(scopes, mapped))
			return 0, false
		}
	}
	if res.HasBucket() || !res.OnHostRoot() {
		return 0, true
	}
	gid, err := s.db.GateForKey(res.ID, name)
	if errors.Is(err, database.ErrNoGateForKey) {
		writeJSON(w, http.StatusForbidden, outsideMappedFolders(scopes, nil))
		return 0, false
	}
	if err != nil {
		log.Printf("finding the gate for %s in %s: %v", name, res.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return 0, false
	}
	return gid, true
}

// outsideMappedFolders is the partition refusal: what it is, where a write
// would land instead, and who can change that.
func outsideMappedFolders(scopes *database.StoreScopes, mapped []string) map[string]any {
	visible := []string{}
	for _, scope := range scopes.Scopes {
		visible = append(visible, writableWithin(scope.Folder, mapped)...)
	}
	visible = dedupeSorted(visible)
	body := map[string]any{"code": "outside_mapped_folders", "folders": visible,
		"hint": "write inside a folder an app is mapped to; an administrator maps folders of a resource to apps on its page under Admin → Storage"}
	switch {
	case len(mapped) == 0:
		body["error"] = fmt.Sprintf("%q is partitioned and no folder of it is mapped to an app yet, "+
			"so it takes no writes — a partitioned resource holds files only in the folders apps are mapped to",
			scopes.Resource.Slug)
	case len(visible) == 0:
		body["error"] = fmt.Sprintf("%q is partitioned and takes writes only in the folders apps are mapped to, "+
			"and none of them is inside what you were granted", scopes.Resource.Slug)
	default:
		body["error"] = fmt.Sprintf("%q is partitioned, and this is outside every folder an app is mapped to — "+
			"write inside %s", scopes.Resource.Slug, strings.Join(quoteAll(visible), ", "))
	}
	return body
}

// writableWithin is where, inside one granted folder, a partitioned resource
// takes writes: each mapped folder the grant covers, and the granted folder
// itself when a mapped folder covers it. Nothing a grant does not reach is
// named, so the answer discloses no sibling's folder.
func writableWithin(granted string, mapped []string) []string {
	var out []string
	for _, m := range mapped {
		switch {
		case granted == "" || m == granted || strings.HasPrefix(m, granted+"/"):
			out = append(out, m)
		case strings.HasPrefix(granted, m+"/"):
			out = append(out, granted)
		}
	}
	return out
}

func dedupeSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) requireStoreUploads(w http.ResponseWriter) bool {
	if s.storeUploadsOffered() {
		return true
	}
	// Still a 404 -- absent, which is all the SPA reads -- but named, so a
	// client asking for one store can tell "this install has none" from "no
	// store by that name". The fact is not a secret: whoami states it as
	// store_uploads_available.
	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": "this install does not offer store uploads",
		"code":  "store_uploads_off"})
	return false
}

// --- The ladder, expressed once ---

// presignVerb is what a caller is asking to do. Not http.Method, because
// "download" and "list" are both GET at the mount and only one of them is a
// signature.
type presignVerb string

const (
	verbGet    presignVerb = "get"
	verbPut    presignVerb = "put"
	verbDelete presignVerb = "delete"
)

// presignFor turns a granted level and a verb into either the conditions a URL
// must carry, or a refusal.
//
// This is the ladder's second expression, and the table it implements is the
// same table kube.StoreMountOptions implements:
//
//	read  | GET                                  | mounted read-only
//	write | + PUT If-None-Match:*                | no allow-overwrite → new objects only
//	full  | + PUT unconditional, + DELETE        | allow-overwrite, allow-delete
//
// `If-None-Match: *` is an S3 conditional write, and it is what makes `write`
// mean on the wire what it means at the mount — new objects only — rather than
// "we did not add allow-overwrite, so please do not overwrite."
func presignFor(access string, verb presignVerb) (cond artifact.PutConditions, ok bool) {
	switch verb {
	case verbGet:
		return artifact.PutConditions{}, database.StoreAccessAtLeast(access, runtime.AccessRead)
	case verbPut:
		switch {
		case database.StoreAccessAtLeast(access, runtime.AccessFull):
			return artifact.PutConditions{}, true
		case database.StoreAccessAtLeast(access, runtime.AccessWrite):
			return artifact.PutConditions{IfNoneMatch: true}, true
		}
		return artifact.PutConditions{}, false
	case verbDelete:
		return artifact.PutConditions{}, database.StoreAccessAtLeast(access, runtime.AccessFull)
	}
	return artifact.PutConditions{}, false
}

// --- Object names ---

// maxStoreObjectName is generous enough for a dated report under a couple of
// folders and short enough that the resulting key stays inside S3's 1024-byte
// key limit once the store's prefix is on the front.
const maxStoreObjectName = 512

// ValidateStoreObjectName is ValidateMountPath's discipline in the other
// direction: that one insists on an absolute path, this one refuses anything
// that is not a plain relative name.
//
// This is the one place in this feature where a bug is a containment breach
// rather than an error message. The client supplies a name; the *server* builds
// the key, from the resource's prefix and this. A name that could escape the
// prefix would let a caller granted one store address another one — or the
// bucket root — with a URL the platform signed itself.
func ValidateStoreObjectName(name string) error {
	if name == "" {
		return fmt.Errorf("a name is required")
	}
	if len(name) > maxStoreObjectName {
		return fmt.Errorf("a name may be at most %d characters, got %d", maxStoreObjectName, len(name))
	}
	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("a name is relative to the store, so it must not start with a slash: %q", name)
	}
	if strings.HasSuffix(name, "/") {
		return fmt.Errorf("a name must not end in a slash, got %q", name)
	}
	if strings.Contains(name, `\`) {
		// A backslash is a separator on the machine the file came from and a
		// literal character in a key. Refusing it stops a Windows path from
		// becoming one very strangely-named object.
		return fmt.Errorf("a name must use forward slashes, got %q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		switch seg {
		case "":
			return fmt.Errorf("a name must not contain an empty segment, got %q", name)
		case ".", "..":
			return fmt.Errorf("a name must not contain %q segments, got %q", seg, name)
		}
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("a name must not contain control characters, got %q", name)
		}
	}
	return nil
}

// ValidateStorePath is ValidateStoreObjectName with exactly one relaxation: a
// folder may end in a slash, and the empty string is the store's root.
//
// **The relaxation is the whole of the risk here**, which is why this delegates
// rather than restates. Everything the sibling refuses — `..`, an empty
// segment, a leading slash, a backslash, a control character, the 512-character
// cap — it still refuses, because the only thing this does before handing over
// is take one trailing slash off the end. A second implementation of that list
// would be a second place for a traversal to be permitted.
func ValidateStorePath(path string) error {
	if path == "" {
		return nil // the store root, which every caller with a grant may list
	}
	trimmed := strings.TrimSuffix(path, "/")
	if trimmed == "" {
		return fmt.Errorf("a path is relative to the store, so %q is not one", path)
	}
	return ValidateStoreObjectName(trimmed)
}

// storePathPrefix is the validated path as a listing prefix: slash-terminated,
// so "reports" lists inside the folder rather than matching "reports-archive"
// beside it — the same trailing-slash discipline storeKeyFor applies to the
// store's own prefix, for the same reason.
func storePathPrefix(path string) string {
	if path == "" || strings.HasSuffix(path, "/") {
		return path
	}
	return path + "/"
}

// storeKeyFor builds the object key the platform will sign.
//
// The prefix is the containment boundary and it is the server's, never the
// client's. The trailing slash is forced for the same reason the mount forces
// it: Mountpoint treats a prefix as a literal string, so "team" without the
// slash also matches "team-archive", and the two expressions of a store's
// extent have to agree about where it ends.
func storeKeyFor(res database.StorageResource, name string) string {
	prefix := strings.TrimPrefix(res.Prefix, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + name
}

// --- The caller's own view ---

type storeSummary struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Kind is volume or store — not where it lives, which stays withheld, but
	// how app code reaches it, which the usage examples beside the row need.
	Kind string `json:"kind"`
	// Folder is which directory of the store this row is about, and empty is
	// the store itself. **A slug can appear more than once here**: a caller
	// granted two folders of one store holds two scopes, and each is somewhere
	// the browser can be rooted. One row per store carrying a list of folders
	// would make every consumer handle the root as a special case beside an
	// array, permanently, to keep a uniqueness nothing depends on.
	Folder string `json:"folder,omitempty"`
	// Access is the level resolved for this caller on this scope — the highest
	// of their own grant, their groups' and any delegation. Never the grant row,
	// because there may be several and the caller holds the maximum of them.
	Access string `json:"access"`
	// Partitioned says the store takes writes only in the folders apps are
	// mapped to, and WritableFolders is which of those this row reaches — empty
	// on a partitioned store is "no write lands anywhere here", which a level
	// alone would have hidden behind `full`. Absent on a whole store, and
	// always present on a partitioned one — a pointer, because `omitempty` on
	// a slice dropped the empty list, and the empty list is the answer
	// (docs/bugs 2026-10-22-e2e-storage #4).
	Partitioned     bool      `json:"partitioned,omitempty"`
	WritableFolders *[]string `json:"writable_folders,omitempty"`
	// The bucket and prefix are deliberately absent. A person who may upload has
	// no business knowing which bucket the platform signs against, and a client
	// that could see the prefix would build keys out of it.
}

// handleListStores — GET /store. The stores this caller can reach.
//
// The /me/access shape: self-scoped, no id in the URL, so it cannot be pointed
// at anybody else. A personal key gets its owner's reach, which is the intended
// use — a script checking what it can write to before trying.
func (s *Server) handleListStores(w http.ResponseWriter, r *http.Request) {
	if !s.requireStoreUploads(w) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	reach, err := s.db.ListReachableStores(p.ID)
	if err != nil {
		log.Printf("listing reachable stores: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	out := make([]storeSummary, 0, len(reach))
	for _, sr := range reach {
		if !s.dataPlaneOffered(sr.Resource) {
			continue
		}
		var mapped []string
		if sr.Resource.Partitioned {
			if mapped, err = s.db.MappedFolders(sr.Resource.ID); err != nil {
				log.Printf("listing the mapped folders of %s: %v", sr.Resource.Slug, err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}
		for _, scope := range sr.Scopes {
			row := storeSummary{
				Slug: sr.Resource.Slug, Name: sr.Resource.Name,
				Description: sr.Resource.Description, Kind: sr.Resource.Kind,
				Folder: scope.Folder, Access: scope.Access,
				Partitioned: sr.Resource.Partitioned,
			}
			if row.Partitioned {
				wf := append([]string{}, dedupeSorted(writableWithin(scope.Folder, mapped))...)
				row.WritableFolders = &wf
			}
			out = append(out, row)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stores": out})
}

// resolveStore is the gate every per-store route passes through: the caller's
// scopes on this slug, or a 404.
//
// 404 over 403 throughout. A caller holding no scope on a slug gets the same
// answer as a caller naming a slug that does not exist, so the route cannot be
// used to enumerate an install's stores.
//
// **It no longer answers "at what level", because that is not a question about
// the store.** A folder-scoped grant makes the level a property of the key, so
// the routes below resolve the store first, work out what is being addressed,
// and only then ask what the caller may do to *that* — which is the order
// `holdsKey` and `holdsPath` exist to make hard to get wrong.
func (s *Server) resolveStore(w http.ResponseWriter, r *http.Request) (*database.StoreScopes, bool) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil, false
	}

	scopes, err := s.db.StoreScopesBySlug(p.ID, chi.URLParam(r, "slug"))
	if err != nil {
		log.Printf("resolving store reach: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, false
	}
	// A store whose signer this install lacks is one nobody here reaches: the
	// same 404, so the route still cannot tell an absent signer from an
	// absent grant.
	if scopes == nil || !s.dataPlaneOffered(scopes.Resource) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "`toft store ls` lists the stores you can reach"})
		return nil, false
	}
	return scopes, true
}

// holdsKey resolves what this caller may do to one object and refuses when it
// is not enough.
//
// The refusal is a 403 rather than a 404 and that is deliberate: the caller
// already holds a scope on this store, so they know it exists, and the
// disclosure rule that makes an unknown slug a 404 does not reach here. What it
// must not do is name the object — "you may not delete this" and "there is
// nothing here" are the same sentence to somebody probing.
func holdsKey(w http.ResponseWriter, scopes *database.StoreScopes, name, need string) (string, bool) {
	have := scopes.LevelForKey(name)
	if !database.StoreAccessAtLeast(have, need) {
		writeJSON(w, http.StatusForbidden, refusalFor(scopes, have, need))
		return "", false
	}
	return have, true
}

// holdsPath is holdsKey for a folder a caller is trying to list.
func holdsPath(w http.ResponseWriter, scopes *database.StoreScopes, path, need string) (string, bool) {
	have := scopes.LevelForPath(path)
	if !database.StoreAccessAtLeast(have, need) {
		writeJSON(w, http.StatusForbidden, refusalFor(scopes, have, need))
		return "", false
	}
	return have, true
}

// refusalFor says why, in the caller's own terms.
//
// Two different refusals wear one status code and a reader has to be able to
// tell them apart: *your level here is too low* and *this is not somewhere you
// were granted at all*. The second names the folders they do hold, because a
// person granted `analytics/` who typed `marketing/` needs to know which of the
// two mistakes they made, and the folders are already theirs to know.
func refusalFor(scopes *database.StoreScopes, have, need string) map[string]any {
	if have == "" {
		folders := scopes.GrantedFolders()
		return map[string]any{
			"error": fmt.Sprintf(
				"this is outside what you were granted in this store — you have %s",
				strings.Join(quoteAll(folders), ", ")),
			"folders": folders,
			// insufficient_access, as the app surface's: a code a client can
			// branch on rather than the envelope's status word (BB-015).
			"code": "insufficient_access",
			"hint": "use a path under one of the folders listed in folders, or ask an administrator for a grant here; `toft store ls` shows what you hold",
		}
	}
	return map[string]any{
		"error": fmt.Sprintf("your access here is %q, and this needs %q", have, need),
		"code":  "insufficient_access",
		"hint":  fmt.Sprintf("ask an administrator for %q or above on this store; `toft store ls` shows your level on each", need),
	}
}

func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, strconv.Quote(v+"/"))
	}
	return out
}

// handleListStoreObjects — GET /store/{slug}/objects.
func (s *Server) handleListStoreObjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireStoreUploads(w) {
		return
	}
	scopes, ok := s.resolveStore(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	path := strings.TrimSpace(q.Get("path"))
	if err := ValidateStorePath(path); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path: " + err.Error(), "hint": `send a path relative to the store, like ?path=reports/2026, or leave it out for the top`})
		return
	}

	// **The root of a caller who does not reach the root is synthesized**, and
	// this is the strong form of hiding a folder rather than a refinement of it:
	// a folder somebody can see and cannot open teaches them that a team, a
	// customer or a project exists, which is the disclosure a scoped grant is
	// for. What comes back is assembled from their own grant rows, so no request
	// reaches the object store at all and there is nothing for a sibling to leak
	// through. Below a granted folder the listing is the real one — a grant is
	// on the whole subtree, so every row of it is a row the caller may open.
	if path == "" {
		if folders := scopes.GrantedFolders(); len(folders) > 0 {
			// No level at the root, because there is none: a caller granted one
			// folder may not write beside it. The page draws its controls from
			// this, so what it offers here is nothing, which is correct.
			writeJSON(w, http.StatusOK, storeListing{
				ObjectPage: artifact.ObjectPage{Prefixes: withTrailingSlash(folders)},
			})
			return
		}
	}
	level, ok := holdsPath(w, scopes, path, runtime.AccessRead)
	if !ok {
		return
	}
	_, scope := scopes.ScopeForPath(path)

	// The caller supplies a path, never a key. The prefix stays the server's:
	// this is storeKeyFor doing what it already does for a name, so a store's
	// extent remains something a client can neither see nor address around.
	prefix := storeKeyFor(scopes.Resource, storePathPrefix(path))

	// A delimiter unless the caller asked for everything beneath. Folders are
	// the default because a browser has levels and the flat answer is the one
	// that cannot be paged honestly.
	delimiter := "/"
	if q.Get("flat") == "1" {
		delimiter = ""
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	page, err := s.signerFor(r, scopes.Resource, scope, 0).ListObjects(ctx, scopes.Resource.Bucket,
		prefix, q.Get("cursor"), delimiter, maxStoreListing)
	if err != nil {
		log.Printf("listing store %s: %v", scopes.Resource.Slug, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "the object store did not answer"})
		return
	}
	writeJSON(w, http.StatusOK, storeListing{ObjectPage: *page, Access: level})
}

// storeListing is a page of a store, and the level the caller holds *here*.
//
// The level travels with the listing because it is a property of the folder
// being shown rather than of the store: somebody granted `read` on
// `analytics/` and `full` on `finance/q1/` has two different sets of controls
// inside one store, and a page that drew them from a store-wide number would
// offer a delete button that always 403s. The page renders from this and never
// from a comparison of its own — the same rule the affordance table already
// follows, now with the server answering per folder.
type storeListing struct {
	artifact.ObjectPage
	Access string `json:"access,omitempty"`
}

type storeUploadRequest struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// SHA256 and MD5 are hex or base64 digests of the bytes about to be sent.
	// Both are signed into the URL: the SHA-256 is what AWS stores beside the
	// object for later verification, the MD5 is what every S3-compatible server
	// actually refuses a mismatched body on. See artifact.PutConditions.
	SHA256 string `json:"sha256"`
	MD5    string `json:"md5"`
}

type storeSignedResponse struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	Key       string            `json:"key"`
	ExpiresAt time.Time         `json:"expires_at"`
	// ExistsStatus is the status the store answers a create-only PUT of a
	// name that exists with — 412 or, on Azure, 403 — so an SDK raises
	// already_exists on every backend (I-5). Absent on a PUT that may replace.
	ExistsStatus int `json:"exists_status,omitempty"`
}

// handleStoreUpload — POST /store/{slug}/objects. Mints a presigned PUT.
//
// **The audit record is the mint, not the landing, and says so.** The platform
// signs a URL and never sees the bytes, so this records who asked, for what
// key, at what level, and for how long — and cannot record whether an upload
// happened. That is the price of not proxying, stated rather than left for an
// auditor to infer a delivery from a signature. An install that needs proof of
// the write has it in S3 server access logs or CloudTrail data events, which is
// where the object actually landed.
func (s *Server) handleStoreUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireStoreUploads(w) {
		return
	}
	scopes, ok := s.resolveStore(w, r)
	if !ok {
		return
	}

	var req storeUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	name := req.Name
	if err := ValidateStoreObjectName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": `send a name relative to the store, like "reports/2026/q1.csv", with no leading or trailing slash`})
		return
	}
	// The level is resolved against the key, which is why it is resolved here
	// and not at the top: until the name is validated there is nothing to
	// resolve it against.
	level, ok := holdsKey(w, scopes, name, runtime.AccessWrite)
	if !ok {
		return
	}
	sha, md5sum, ok := uploadDigests(w, req)
	if !ok {
		return
	}

	cond, allowed := presignFor(level, verbPut)
	if !allowed {
		// Unreachable through holdsKey's gate, and here anyway: presignFor is
		// the ladder, and a route that trusted its own gate instead would be the
		// second expression this file exists to prevent.
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this store does not take uploads at your access level", "code": "insufficient_access", "hint": "ask an administrator for write access to this store; `toft store ls` shows your level on each"})
		return
	}
	cond.ChecksumSHA256, cond.ContentMD5, cond.ContentLength = sha, md5sum, req.Size
	gid, ok := s.writeGate(w, scopes, name)
	if !ok {
		return
	}
	_, scope := scopes.ScopeForKey(name)

	key := storeKeyFor(scopes.Resource, name)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	signed, err := s.signerFor(r, scopes.Resource, scope, gid).PresignPut(ctx, scopes.Resource.Bucket, key, storeURLTTL, cond)
	if err != nil {
		log.Printf("presigning upload to %s: %v", scopes.Resource.Slug, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the object store could not be addressed"})
		return
	}

	s.auditStorePresign(r, scopes, key, name, level, verbPut)
	writeJSON(w, http.StatusOK, storeSignedResponse{
		URL: signed.URL, Method: signed.Method, Headers: signed.Headers,
		Key: name, ExpiresAt: time.Now().UTC().Add(storeURLTTL), ExistsStatus: signed.ExistsStatus,
	})
}

// uploadDigests checks what an upload says it is about to send — its size and
// both digests — and returns the digests normalized for signing. One function
// for the person's route and the app's, because what an upload URL may sign is
// one rule whoever asks for it.
func uploadDigests(w http.ResponseWriter, req storeUploadRequest) (sha, md5sum string, ok bool) {
	// Zero is a size: an empty `__init__.py` is part of the tree a recursive
	// copy carries. Its URL signs no length, and needs none — the Content-MD5
	// of the empty body is signed on S3, so nothing else verifies.
	if req.Size < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "size must be the exact number of bytes about to be sent", "hint": `send "size" as the file's exact length in bytes, like 1048576`})
		return "", "", false
	}
	if req.Size > maxSinglePutBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf(
				"a single upload is at most %d bytes (5 GiB) and this is %d — the object store takes one PUT per object, and objects above that need multipart, which this install does not offer",
				maxSinglePutBytes, req.Size)})
		return "", "", false
	}
	sha, err := normalizeDigest(req.SHA256, 32)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sha256: " + err.Error(), "hint": `send "sha256" as the file's SHA-256 digest in hex, which sha256sum prints`})
		return "", "", false
	}
	md5sum, err = normalizeDigest(req.MD5, 16)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "md5: " + err.Error(), "hint": `send "md5" as the file's MD5 digest in hex, which md5sum prints`})
		return "", "", false
	}
	return sha, md5sum, true
}

// emptyBodyConditions signs the digests of a zero-byte body, which is what a
// folder marker is.
func emptyBodyConditions(cond artifact.PutConditions) artifact.PutConditions {
	emptyMD5 := md5.Sum(nil)
	emptySHA := sha256.Sum256(nil)
	cond.ContentMD5 = base64.StdEncoding.EncodeToString(emptyMD5[:])
	cond.ChecksumSHA256 = base64.StdEncoding.EncodeToString(emptySHA[:])
	return cond
}

// presignObject mints a GET or a DELETE for one key. The download's
// disposition names the object's own last segment, built from the name the
// server already validated — never a string the client sent alongside it.
func presignObject(ctx context.Context, signer artifact.Signer, bucket, key, name string, verb presignVerb,
	attachment bool, ttl time.Duration) (*artifact.SignedRequest, error) {
	switch {
	case verb == verbDelete:
		return signer.PresignDelete(ctx, bucket, key, ttl)
	case attachment:
		return signer.PresignGet(ctx, bucket, key, ttl, gopath.Base(name))
	default:
		return signer.PresignGet(ctx, bucket, key, ttl, "")
	}
}

// handleStoreDownload — GET /store/{slug}/objects/{key...}. Mints a presigned
// GET.
//
// Not in the four routes the design named, and needed by the `toft store get` the
// same design specifies: a download has to be a signed URL for the same reason
// an upload does, because the alternative is the bytes crossing the control
// plane. Beside the delete below because they share a path shape.
func (s *Server) handleStoreDownload(w http.ResponseWriter, r *http.Request) {
	s.signStoreObject(w, r, verbGet, runtime.AccessRead)
}

// handleStoreDelete — DELETE /store/{slug}/objects/{key...}. `full` only.
func (s *Server) handleStoreDelete(w http.ResponseWriter, r *http.Request) {
	s.signStoreObject(w, r, verbDelete, runtime.AccessFull)
}

// --- Folders ---

// A folder is a zero-byte object whose key ends in a slash: the marker S3's own
// console writes, which Mountpoint shows as a directory and every listing here
// already reports as a prefix. **These two routes are the only way such a key is
// built**: the object routes still refuse a trailing slash, so a marker can never
// be addressed as a file and a file can never be addressed as a marker. Both take
// a *path* — the folder itself, which a grant on that folder covers — and the
// marker key is built here, from the validated path, exactly once.
//
// A marker is optional and holds nothing: a folder with objects in it exists
// without one, and deleting one deletes no object under it.

type storeFolderRequest struct {
	Path string `json:"path"`
}

// handleStoreFolderCreate — POST /store/{slug}/folders. Mints a PUT of an empty
// body to `<path>/`, at `write` or above. At `write` the URL carries the same
// If-None-Match every upload does, so creating a folder that already has a
// marker is the object store's 412, which a client reads as "already there".
func (s *Server) handleStoreFolderCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireStoreUploads(w) {
		return
	}
	scopes, ok := s.resolveStore(w, r)
	if !ok {
		return
	}
	var req storeFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	path, ok := folderPath(w, req.Path)
	if !ok {
		return
	}
	level, ok := holdsPath(w, scopes, path, runtime.AccessWrite)
	if !ok {
		return
	}
	cond, allowed := presignFor(level, verbPut)
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this store does not take uploads at your access level", "code": "insufficient_access", "hint": "ask an administrator for write access to this store; `toft store ls` shows your level on each"})
		return
	}
	cond = emptyBodyConditions(cond)

	name := path + "/"
	gid, ok := s.writeGate(w, scopes, name)
	if !ok {
		return
	}
	_, scope := scopes.ScopeForPath(path)
	key := storeKeyFor(scopes.Resource, name)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	signed, err := s.signerFor(r, scopes.Resource, scope, gid).PresignPut(ctx, scopes.Resource.Bucket, key, storeURLTTL, cond)
	if err != nil {
		log.Printf("presigning folder in %s: %v", scopes.Resource.Slug, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the object store could not be addressed"})
		return
	}
	s.auditStorePresign(r, scopes, key, name, level, verbPut)
	writeJSON(w, http.StatusOK, storeSignedResponse{
		URL: signed.URL, Method: signed.Method, Headers: signed.Headers,
		Key: name, ExpiresAt: time.Now().UTC().Add(storeURLTTL), ExistsStatus: signed.ExistsStatus,
	})
}

// handleStoreFolderDelete — DELETE /store/{slug}/folders/{path...}. Mints a
// DELETE of the marker alone, at `full`. Deleting what is under the folder is
// the client's walk over the object routes, one signed URL per object; this
// route never reaches past the marker, and a folder with no marker answers the
// object store's ordinary 204.
func (s *Server) handleStoreFolderDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireStoreUploads(w) {
		return
	}
	scopes, ok := s.resolveStore(w, r)
	if !ok {
		return
	}
	raw, ok := storeWildcard(w, r)
	if !ok {
		return
	}
	path, ok := folderPath(w, raw)
	if !ok {
		return
	}
	level, ok := holdsPath(w, scopes, path, runtime.AccessFull)
	if !ok {
		return
	}
	if _, allowed := presignFor(level, verbDelete); !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": fmt.Sprintf("your access here is %q, which does not permit this", level), "code": "insufficient_access", "hint": "ask an administrator for a higher level on this store; `toft store ls` shows your level on each"})
		return
	}
	name := path + "/"
	_, scope := scopes.ScopeForPath(path)
	key := storeKeyFor(scopes.Resource, name)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	signed, err := s.signerFor(r, scopes.Resource, scope, 0).PresignDelete(ctx, scopes.Resource.Bucket, key, storeURLTTL)
	if err != nil {
		log.Printf("presigning folder delete on %s: %v", scopes.Resource.Slug, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the object store could not be addressed"})
		return
	}
	s.auditStorePresign(r, scopes, key, name, level, verbDelete)
	writeJSON(w, http.StatusOK, storeSignedResponse{
		URL: signed.URL, Method: signed.Method, Headers: signed.Headers,
		Key: name, ExpiresAt: time.Now().UTC().Add(storeURLTTL), ExistsStatus: signed.ExistsStatus,
	})
}

// folderPath validates a folder path and returns it without its trailing
// slash. Validated **before** it is trimmed, through the one validator, and the
// store root is refused: it has no marker, because it has no name.
// storeWildcard is the name or path after /objects/ or /folders/, decoded —
// which is what the key is: the name the caller stored it under, not an escaped
// form of it. chi routes on RawPath whenever the request carries an escape that
// decoding would change (a %2F), and then hands the wildcard back still
// escaped, so `..%2F..%2Fetc` reached the validator as one harmless-looking
// segment and was signed as a literal key. Decoding first puts every name
// through the validator in the form it will be stored under.
func storeWildcard(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := chi.URLParam(r, "*")
	if r.URL.RawPath == "" {
		return raw, true
	}
	name, err := url.PathUnescape(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the name is not a valid escaped path: " + err.Error(), "hint": "percent-encode the object name in the path, like q1%20report.csv"})
		return "", false
	}
	return name, true
}

func folderPath(w http.ResponseWriter, raw string) (string, bool) {
	if err := ValidateStorePath(raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path: " + err.Error(), "hint": `send a folder path relative to the store, like "reports/2026"`})
		return "", false
	}
	if raw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path: a folder needs a name — the store's root is not one", "hint": `send the folder's name in "path", like "reports"`})
		return "", false
	}
	return strings.TrimSuffix(raw, "/"), true
}

func (s *Server) signStoreObject(w http.ResponseWriter, r *http.Request, verb presignVerb, need string) {
	if !s.requireStoreUploads(w) {
		return
	}
	scopes, ok := s.resolveStore(w, r)
	if !ok {
		return
	}

	name, ok := storeWildcard(w, r)
	if !ok {
		return
	}
	if err := ValidateStoreObjectName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": `send a name relative to the store, like "reports/2026/q1.csv", with no leading or trailing slash`})
		return
	}
	level, ok := holdsKey(w, scopes, name, need)
	if !ok {
		return
	}
	// The second reading of one value, kept: presignFor is the ladder's other
	// expression and the conditions it puts on the URL are what actually bind.
	// Two readings of a resolved level is not the duplication the invariants
	// object to — two *sources* for it would be.
	if _, allowed := presignFor(level, verb); !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": fmt.Sprintf("your access here is %q, which does not permit this", level), "code": "insufficient_access", "hint": "ask an administrator for a higher level on this store; `toft store ls` shows your level on each"})
		return
	}

	key := storeKeyFor(scopes.Resource, name)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// The disposition is opt-in and absent by default, so `toft store get`,
	// which writes to a path it chose itself, is untouched.
	_, scope := scopes.ScopeForKey(name)
	signed, err := presignObject(ctx, s.signerFor(r, scopes.Resource, scope, 0), scopes.Resource.Bucket, key, name, verb,
		r.URL.Query().Get("disposition") == "attachment", storeURLTTL)
	if err != nil {
		log.Printf("presigning %s on %s: %v", verb, scopes.Resource.Slug, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the object store could not be addressed"})
		return
	}

	s.auditStorePresign(r, scopes, key, name, level, verb)
	writeJSON(w, http.StatusOK, storeSignedResponse{
		URL: signed.URL, Method: signed.Method, Headers: signed.Headers,
		Key: name, ExpiresAt: time.Now().UTC().Add(storeURLTTL), ExistsStatus: signed.ExistsStatus,
	})
}

// auditStorePresign records the mint. Never fatal to the request: the URL is
// already signed by the time this runs, so failing the call here would tell the
// caller nothing happened when something did.
// key is the object key in the bucket, store prefix included, which is what
// the row records; name is the caller's own path under the store, which is what
// the grant folders are written against and what grantingFolder matches.
func (s *Server) auditStorePresign(r *http.Request, scopes *database.StoreScopes, key, name, level string, verb presignVerb) {
	s.auditStorePresignVia(r, scopes, key, name, level, verb, "")
}

// auditStorePresignVia is the same row with how the caller arrived: "link"
// for a storage link being opened (/files/), so an access review can tell a
// click on a mailed link from the store browser. Empty is the API itself and
// writes no `via`, which keeps every existing row's shape.
func (s *Server) auditStorePresignVia(r *http.Request, scopes *database.StoreScopes, key, name, level string, verb presignVerb, via string) {
	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())
	detail := map[string]string{
		"key":  key,
		"verb": string(verb),
		// The level that was resolved *for this key*, and the scope that
		// resolved it. Without the folder an auditor reading a mint under
		// `reports/` cannot tell a grant on the whole store from a grant on
		// that one directory — which is the question a folder-scoped
		// install is auditing for.
		"access": level,
		"folder": grantingFolder(scopes, name, level),
		"ttl":    storeURLTTL.String(),
	}
	if via != "" {
		detail["via"] = via
	}
	entry := database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		Action:     "store.presign",
		TargetType: "storage_resource", TargetID: scopes.Resource.ID,
		After:     jsonString(detail),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}
	if err := s.db.WriteAuditRow(entry); err != nil {
		log.Printf("auditing store presign: %v", err)
	}
}

// grantingFolder is which scope resolved the level that authorized a mint —
// the widest-reaching one at that level, so a caller holding both the root and
// a folder is recorded as having used the root. It is the scope that answers
// "why was this allowed", which a level alone cannot.
//
// name is the path under the store, with the store prefix stripped — the grant
// folders never carry it — and the match is coversKey's, on a separator, the
// same match LevelForKey authorized with. A bare string prefix on the prefixed
// key recorded `""` for a delete authorized by a grant on `reports`, which told
// the auditor the caller held the whole store.
func grantingFolder(scopes *database.StoreScopes, name, level string) string {
	best := ""
	found := false
	for _, sc := range scopes.Scopes {
		if sc.Access != level || !database.CoversKey(sc.Folder, name) {
			continue
		}
		if !found || len(sc.Folder) < len(best) {
			best, found = sc.Folder, true
		}
	}
	return best
}

// withTrailingSlash renders folders the way a listing's prefixes are rendered:
// slash-terminated, relative to the store. One shape for a folder wherever it
// crosses the wire, so the page that turns a row into a key does it exactly
// once whether the row came from S3 or from a grant.
func withTrailingSlash(folders []string) []string {
	out := make([]string, 0, len(folders))
	for _, f := range folders {
		out = append(out, strings.TrimSuffix(f, "/")+"/")
	}
	return out
}

// normalizeDigest accepts hex or base64 and returns the base64 form S3 wants.
//
// Both, because "sha256" means hex to sha256sum and base64 to S3, and a client
// that sends the wrong one would otherwise get a signature that verifies and an
// upload that is refused for a reason naming neither.
func normalizeDigest(value string, wantBytes int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("a digest of the bytes is required")
	}
	if raw, err := hex.DecodeString(value); err == nil && len(raw) == wantBytes {
		return base64.StdEncoding.EncodeToString(raw), nil
	}
	if raw, err := base64.StdEncoding.DecodeString(value); err == nil && len(raw) == wantBytes {
		return base64.StdEncoding.EncodeToString(raw), nil
	}
	return "", fmt.Errorf("expected %d bytes as hex or base64", wantBytes)
}

// --- Administration: who may reach a store ---

type putStoreGrantRequest struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	Access      string `json:"access"`
	// Folder narrows the grant to one directory, and absent is the whole store.
	// It is part of the grant's identity rather than of its payload: a grant on
	// another folder is a different scope, not a second answer about the same
	// one.
	Folder string `json:"folder"`
}

func (s *Server) handleListStoreGrants(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	res, ok := s.storageResourceOr404(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	grants, err := s.db.ListStoreGrants(res.ID)
	if err != nil {
		log.Printf("listing store grants: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"grants": grants,
		// So the admin page can say why the panel is read-only on an install
		// that has stores and has not switched uploads on.
		"uploads_enabled": s.dataPlaneOffered(*res),
	})
}

func (s *Server) handlePutStoreGrant(w http.ResponseWriter, r *http.Request) {
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
	// Refused here rather than on a constraint the administrator cannot see, and
	// the test is the bucket rather than the kind: what a grant needs is objects
	// to sign for, which an S3 Files file store has and an EFS-backed one never
	// will. The sentence names the backing, because that is the thing an
	// administrator can act on — the kind is not the problem and changing it is
	// not the fix.
	if !res.HasDataPlane() {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "this file store is backed by EFS on the cluster, which the platform does not mount — there is nothing to sign, because the only way to write to one is to mount it. A store, a file store backed by Amazon S3 Files, or one under a host root is what takes grants.", "hint": "map it to an app with PUT /api/v1/admin/storage/{id}/mappings/{appID} and reach its files through that app's mount"})
		return
	}
	// A directory under a host root is reached through the host data plane,
	// and where that is absent a grant would be a reach nobody can use.
	if !res.HasBucket() && s.hostStore == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "people cannot reach host-backed storage on this install: " + s.hostStoreAbsent, "hint": "map it to an app with PUT /api/v1/admin/storage/{id}/mappings/{appID} instead, or ask an operator to turn on the host data plane"})
		return
	}

	var req putStoreGrantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	if req.SubjectType != database.StoreSubjectUser && req.SubjectType != database.StoreSubjectGroup {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `subject_type must be "user" or "group"`, "hint": `send "subject_type": "user" or "group"`})
		return
	}
	if !database.ValidStorageAccess(database.StorageKindStore, req.Access) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("a store offers %s — %q is not one of them%s",
				strings.Join(database.StorageAccessLevels(database.StorageKindStore), ", "), req.Access, database.StorageAccessRefusal(req.Access)), "hint": `send "access" as one of the levels listed, like "read"`})
		return
	}
	// The subject has to exist. Without this a typo becomes a grant to nobody
	// that sits in the list looking like it works.
	if !s.storeSubjectExists(req.SubjectType, req.SubjectID) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("no such %s", req.SubjectType), "hint": "GET /api/v1/users lists user ids and GET /api/v1/groups lists group ids"})
		return
	}

	folder, ok := validatedFolder(w, req.Folder)
	if !ok {
		return
	}
	// A read-only root refuses every write at the file system, so a grant
	// above `read` there would be a level that only ever answers an error.
	if !res.HasBucket() && res.OnHostRoot() && s.hostStore.ReadOnly(res.Root) &&
		database.StoreAccessAtLeast(req.Access, runtime.AccessWrite) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": fmt.Sprintf("%q is under a read-only mount, so a grant there is %q at most", res.Name, runtime.AccessRead), "hint": `send "access": "read"`})
		return
	}

	grant := &database.StoreGrant{
		ResourceID:  res.ID,
		SubjectType: req.SubjectType,
		SubjectID:   req.SubjectID,
		Access:      req.Access,
		Folder:      folder,
	}
	meta := requestMeta(r.Context())
	if err := s.db.PutStoreGrant(grant, p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		log.Printf("granting store access: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, grant)
}

func (s *Server) handleDeleteStoreGrant(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())

	// The subject is one path segment, "user:<id>" or "group:<id>", because a
	// grant's identity is the pair and a URL that carried only the id could
	// name two different subjects.
	subjectType, subjectID, found := strings.Cut(chi.URLParam(r, "subject"), ":")
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `the subject is "user:<id>" or "group:<id>"`, "hint": "name it in the path as user:<id> or group:<id>"})
		return
	}

	// The scope, from the query rather than the path: a folder is arbitrary
	// path text and the route pattern is a classified string in keyroutes.go,
	// so it stays as it is. Absent is the root grant, which is what a caller
	// written before folders was revoking.
	folder, ok := validatedFolder(w, r.URL.Query().Get("folder"))
	if !ok {
		return
	}

	err := s.db.DeleteStoreGrant(chi.URLParam(r, "id"), subjectType, subjectID, folder,
		p.ID, meta.RequestID, meta.IP, meta.UserAgent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/storage/" + chi.URLParam(r, "id") + "/grants lists the grants on it"})
			return
		}
		log.Printf("revoking store access: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) storeSubjectExists(subjectType, subjectID string) bool {
	if subjectType == database.StoreSubjectGroup {
		g, err := s.db.GetGroup(subjectID)
		return err == nil && g != nil
	}
	u, err := s.db.GetUser(subjectID)
	return err == nil && u != nil
}
