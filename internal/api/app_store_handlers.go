package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/artifact"
	"toft/internal/database"
	"toft/internal/runtime"
)

// An app reaching a store as itself, over signed URLs.
//
// **The mapping is the grant.** A person reaches a store by a `store_grants`
// row; an app reaches one by the `storage_mappings` row an administrator or a
// storage manager wrote — the same row that mounts it on a cluster, at the same
// level and under the same folder. So on a cluster a mapped store is a path
// *and* these routes, and on a shared host, where nothing can mount it, it is
// these routes alone; an app written against them moves between the two
// unchanged. That is ADR §11's rule for job outputs, applied here: if one mode
// were quietly more capable, an app written against it could not move.
//
// What this keeps from the person-facing routes is everything that is not who
// is asking: the ladder (presignFor), the validator (ValidateStorePath), the
// key (storeKeyFor) and the audit row per mint. What it changes is the level's
// source, the URL's lifetime and the actor on the row.
//
// The mapping is read on every request, so attaching lands on the next call and
// detaching refuses it. A URL already minted lives out appStoreURLTTL.

// appStoreURLTTL is how long a URL minted for an app lives. Shorter than a
// person's hour: a person hashes and uploads a large file from a laptop, while
// an app already holds the bytes and uses a URL within seconds — the long case
// is a download handed to a browser, which needs minutes.
const appStoreURLTTL = 15 * time.Minute

// appStoreMapping is one store, as one app reaches it.
type appStoreMapping struct {
	Resource database.StorageResource
	Mapping  database.StorageMapping
}

// within is a caller's name or path under the mapping's folder, which is what
// the store's own key space calls it. The folder was validated when the
// mapping was written; the name is validated by the caller before this.
func (m appStoreMapping) within(name string) string {
	if m.Mapping.Folder == "" {
		return name
	}
	return strings.TrimSuffix(m.Mapping.Folder, "/") + "/" + name
}

// resolveAppStore finds the one mapping of this app on the store `{slug}`.
//
// `?folder=` is how the SDK names one mapping when a resource is mapped twice —
// `toft.storage(slug, folder=…)` — and absent means *do not care*, which is
// ambiguous only when there is more than one. The ambiguity is a 409 listing
// the folders rather than a guess, for the reason the SDKs raise on it.
//
// **No mapping is a 404 that does not say whether the store exists.** An app
// learns nothing about resources it was not given.
func (s *Server) resolveAppStore(w http.ResponseWriter, r *http.Request) (*appStoreMapping, bool) {
	return s.resolveAppStoreFor(w, r, false, func(res database.StorageResource) bool {
		return res.HasBucket() && s.storeSigner != nil
	})
}

// resolveAppStoreFor is resolveAppStore with the last question — is this
// resource one the route serves — asked by the caller. The signing routes
// serve only a bucket (an app on a host root has the directory itself); a
// storage link serves any resource a person could open it on, since nothing
// is signed for the app — which is why it takes a volume too (volumes): a
// volume on a host root is somewhere a person opens files through the host
// data plane, and its app writes there by path.
func (s *Server) resolveAppStoreFor(w http.ResponseWriter, r *http.Request, volumes bool, serves func(database.StorageResource) bool) (*appStoreMapping, bool) {
	// Absent, as the capability is — but explained, because the caller here is
	// an app holding a mapping it can see in mounts(): a bare 404 on a store it
	// was given read as "not mapped" and sent its author to an administrator
	// about the wrong thing. The people-facing /api/v1/store answers the same
	// code (requireStoreUploads); its 404 is how the SPA learns there is
	// nothing to show.
	if !s.storeUploadsOffered() {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this install has no data plane for stores (no store signer is " +
				"configured), so signed URLs cannot be issued; a store mounted into this " +
				"app is still readable at its mount path",
			"code": "store_uploads_off"})
		return nil, false
	}
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return nil, false
	}
	slug := chi.URLParam(r, "slug")
	mappings, err := s.db.ListStorageMappingsForApp(token.Token.AppID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, false
	}
	var named []database.StorageMapping
	for _, m := range mappings {
		if m.ResourceSlug == slug {
			named = append(named, m)
		}
	}
	// The table is the serving release's (ADR § Storage somebody else can
	// attach), and an app token is the app's rather than a release's, so a
	// release that declares a new store in toft.toml cannot mint for it until
	// its own deploy flips — the sentence says so, because an app that mints at
	// boot fails readiness on exactly that first deploy.
	notMapped := map[string]string{
		"error": fmt.Sprintf("no store named %q is attached to this app yet — a store declared in "+
			"toft.toml is reachable at the end of the deploy that adds it", slug),
		"code": "store_not_mapped"}
	if len(named) == 0 {
		writeJSON(w, http.StatusNotFound, notMapped)
		return nil, false
	}
	// A volume is reached at its path, never over a URL: on EFS there is
	// nothing to presign, and on S3 Files an app writing by path and reading by
	// URL reads its own write as missing until the two views converge.
	if named[0].ResourceKind != database.StorageKindStore && !volumes {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("%q is a volume, which this app reaches at its mapped path — "+
				"only an object store is reached through these routes", slug),
			"code": "not_a_store", "hint": "read and write it at its mount path — GET /api/v1/app/storage lists where each is mounted"})
		return nil, false
	}

	q := r.URL.Query()
	var picked *database.StorageMapping
	if q.Has("folder") {
		folder := strings.Trim(strings.TrimSpace(q.Get("folder")), "/")
		for i := range named {
			if named[i].Folder == folder {
				picked = &named[i]
			}
		}
		if picked == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": fmt.Sprintf("%q is mapped to this app, but not at folder %q — mapped here: %s",
					slug, folder, describeMappedFolders(named)),
				"code": "store_not_mapped"})
			return nil, false
		}
	} else if len(named) == 1 {
		picked = &named[0]
	} else {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("%q is mapped to this app %d times — %s. Name one with ?folder=",
				slug, len(named), describeMappedFolders(named)),
			"code": "store_ambiguous", "hint": "add ?folder= naming one of the folders listed"})
		return nil, false
	}

	res, err := s.db.GetStorageResource(picked.ResourceID)
	if err != nil || res == nil || !serves(*res) {
		writeJSON(w, http.StatusNotFound, notMapped)
		return nil, false
	}
	return &appStoreMapping{Resource: *res, Mapping: *picked}, true
}

// describeMappedFolders lists one slug's mappings with the level of each, which
// is usually the reason there are two.
func describeMappedFolders(named []database.StorageMapping) string {
	parts := make([]string, 0, len(named))
	for _, m := range named {
		parts = append(parts, fmt.Sprintf("folder %q at %s", m.Folder, m.Access))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// appLevel is the mapping's level, checked against what a verb needs. The
// mapping is the whole of an app's authority here, so there is no per-key
// resolution: a mapping covers its folder and everything under it.
func appLevel(w http.ResponseWriter, m *appStoreMapping, verb presignVerb) (artifact.PutConditions, bool) {
	cond, allowed := presignFor(m.Mapping.Access, verb)
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": fmt.Sprintf("this app's mapping of %q is at %q, which does not permit this",
				m.Resource.Slug, m.Mapping.Access),
			"code": "insufficient_access",
			"hint": fmt.Sprintf("this needs the mapping at %q or above — an administrator raises it on %q's page "+
				"under Admin → Storage, or use a folder this app is mapped to at that level",
				storeLevelFor(verb), m.Resource.Slug)})
	}
	return cond, allowed
}

// storeLevelFor is the least mapping level presignFor allows a verb at.
func storeLevelFor(verb presignVerb) string {
	switch verb {
	case verbPut:
		return runtime.AccessWrite
	case verbDelete:
		return runtime.AccessFull
	}
	return runtime.AccessRead
}

// handleAppListStoreObjects — GET /api/v1/app/store/{slug}/objects.
func (s *Server) handleAppListStoreObjects(w http.ResponseWriter, r *http.Request) {
	m, ok := s.resolveAppStore(w, r)
	if !ok {
		return
	}
	if _, ok := appLevel(w, m, verbGet); !ok {
		return
	}
	q := r.URL.Query()
	path := strings.TrimSpace(q.Get("path"))
	if err := ValidateStorePath(path); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path: " + err.Error(), "hint": `send a path relative to the store, like ?path=reports/2026, or leave it out for the top`})
		return
	}
	listed := m.Mapping.Folder
	if path != "" {
		listed = m.within(path)
	}
	prefix := storeKeyFor(m.Resource, storePathPrefix(listed))
	delimiter := "/"
	if q.Get("flat") == "1" {
		delimiter = ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	page, err := s.storeSigner.ListObjects(ctx, m.Resource.Bucket, prefix, q.Get("cursor"), delimiter, maxStoreListing)
	if err != nil {
		log.Printf("listing store %s for app %s: %v", m.Resource.Slug, m.Mapping.AppID, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the object store did not answer"})
		return
	}
	writeJSON(w, http.StatusOK, storeListing{ObjectPage: *page, Access: m.Mapping.Access})
}

// maxStoreRequestBytes caps the store's JSON bodies, which name an object and
// its digests and never carry its bytes — those go straight to the bucket on
// the presigned URL.
const maxStoreRequestBytes = 64 << 10

// handleAppStoreUpload — POST /api/v1/app/store/{slug}/objects. A presigned PUT.
func (s *Server) handleAppStoreUpload(w http.ResponseWriter, r *http.Request) {
	m, ok := s.resolveAppStore(w, r)
	if !ok {
		return
	}
	var req storeUploadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStoreRequestBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	name := req.Name
	if err := ValidateStoreObjectName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": `send a name relative to the store, like "reports/2026/q1.csv", with no leading or trailing slash`})
		return
	}
	cond, ok := appLevel(w, m, verbPut)
	if !ok {
		return
	}
	sha, md5sum, ok := uploadDigests(w, req)
	if !ok {
		return
	}
	cond.ChecksumSHA256, cond.ContentMD5, cond.ContentLength = sha, md5sum, req.Size
	s.mintAppPut(w, r, m, name, cond)
}

// handleAppStoreFolderCreate — POST /api/v1/app/store/{slug}/folders. A
// presigned PUT of the empty marker. There is no app route to delete one: a
// marker holds nothing, and an app that wants a folder gone removes what is
// under it.
func (s *Server) handleAppStoreFolderCreate(w http.ResponseWriter, r *http.Request) {
	m, ok := s.resolveAppStore(w, r)
	if !ok {
		return
	}
	var req storeFolderRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStoreRequestBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	path, ok := folderPath(w, req.Path)
	if !ok {
		return
	}
	cond, ok := appLevel(w, m, verbPut)
	if !ok {
		return
	}
	s.mintAppPut(w, r, m, path+"/", emptyBodyConditions(cond))
}

func (s *Server) mintAppPut(w http.ResponseWriter, r *http.Request, m *appStoreMapping, name string, cond artifact.PutConditions) {
	key := storeKeyFor(m.Resource, m.within(name))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	signed, err := s.storeSigner.PresignPut(ctx, m.Resource.Bucket, key, appStoreURLTTL, cond)
	if err != nil {
		log.Printf("presigning upload to %s for app %s: %v", m.Resource.Slug, m.Mapping.AppID, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the object store could not be addressed"})
		return
	}
	s.auditAppStorePresign(r, m, key, verbPut)
	writeJSON(w, http.StatusOK, storeSignedResponse{
		URL: signed.URL, Method: signed.Method, Headers: signed.Headers,
		Key: name, ExpiresAt: time.Now().UTC().Add(appStoreURLTTL), ExistsStatus: signed.ExistsStatus,
	})
}

// handleAppStoreDownload — GET /api/v1/app/store/{slug}/objects/*.
func (s *Server) handleAppStoreDownload(w http.ResponseWriter, r *http.Request) {
	s.signAppStoreObject(w, r, verbGet)
}

// handleAppStoreDelete — DELETE /api/v1/app/store/{slug}/objects/*. `full` only.
func (s *Server) handleAppStoreDelete(w http.ResponseWriter, r *http.Request) {
	s.signAppStoreObject(w, r, verbDelete)
}

func (s *Server) signAppStoreObject(w http.ResponseWriter, r *http.Request, verb presignVerb) {
	m, ok := s.resolveAppStore(w, r)
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
	if _, ok := appLevel(w, m, verb); !ok {
		return
	}
	key := storeKeyFor(m.Resource, m.within(name))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	signed, err := presignObject(ctx, s.storeSigner, m.Resource.Bucket, key, name, verb,
		r.URL.Query().Get("disposition") == "attachment", appStoreURLTTL)
	if err != nil {
		log.Printf("presigning %s on %s for app %s: %v", verb, m.Resource.Slug, m.Mapping.AppID, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the object store could not be addressed"})
		return
	}
	s.auditAppStorePresign(r, m, key, verb)
	writeJSON(w, http.StatusOK, storeSignedResponse{
		URL: signed.URL, Method: signed.Method, Headers: signed.Headers,
		Key: name, ExpiresAt: time.Now().UTC().Add(appStoreURLTTL),
	})
}

// auditAppStorePresign records the mint with the app as the actor — the
// person-facing row's shape, with the mapping's folder where a grant's would be
// and the workload whose token asked. Never fatal, for the reason the person's
// row is not: the URL is already signed.
func (s *Server) auditAppStorePresign(r *http.Request, m *appStoreMapping, key string, verb presignVerb) {
	token := appTokenFromContext(r.Context())
	workload := ""
	if token != nil {
		workload = token.Token.Workload
	}
	meta := requestMeta(r.Context())
	entry := database.AuditEntry{
		ActorType: database.PrincipalTypeApp, ActorID: m.Mapping.AppID,
		Action:     "store.presign",
		TargetType: "storage_resource", TargetID: m.Resource.ID,
		After: jsonString(map[string]string{
			"key":      key,
			"verb":     string(verb),
			"access":   m.Mapping.Access,
			"folder":   m.Mapping.Folder,
			"ttl":      appStoreURLTTL.String(),
			"workload": workload,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}
	if err := s.db.WriteAuditRow(entry); err != nil {
		log.Printf("audit: store.presign for app %s on %s: %v", m.Mapping.AppID, m.Resource.Slug, err)
	}
}
