package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/runtime"
	"toft/internal/supervisor"
)

// Delegated storage, from the API's side.
//
// Two new surfaces on top of the admin one. An administrator decides who may
// attach a resource and at what ceiling (`/admin/storage/{id}/managers`); a
// delegate sees what they may attach (`GET /me/storage`) and attaches it to
// their own app (`PUT/DELETE /apps/{appID}/storage/{resourceID}`).
//
// **Three enforcement points write `storage_mappings`, and the list is the
// specification**: handlePutStorageMapping (the administrator's route),
// handleAttachAppStorage (the delegate's), and AuthorizeAttach, the one helper
// the second consults — which returns the decision *and* the ceiling, so no
// caller re-derives it. TestOnlyThreeRoutesWriteStorageMappings walks the
// router and says so.

// --- Who may attach ---

type putStorageManagerRequest struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	MaxAccess   string `json:"max_access"`
	// Access is not this body's field — it is a grant's — and is read only
	// so the refusal can say which one was meant (D-02).
	Access string `json:"access"`
}

// missingMaxAccess is the refusal for a delegation body with no max_access,
// naming the field rather than quoting "" as a level — and, when `access` was
// sent, saying that is the grants' field (D-02, 2026-11-04 black-box list).
func missingMaxAccess(w http.ResponseWriter, access string, levels []string) {
	msg := "a delegation needs max_access — the highest level its holder may attach at, one of " +
		strings.Join(levels, ", ")
	if access != "" {
		msg += fmt.Sprintf(". access is a store grant's field (PUT /admin/storage/{id}/grants); a delegation's is max_access, so send \"max_access\": %q", access)
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "code": "missing_fields",
		"hint": `send "max_access", like {"subject_type": "group", "subject_id": "<id>", "max_access": "read"}`})
}

func (s *Server) handleListStorageManagers(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	res, ok := s.storageResourceOr404(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	managers, err := s.db.ListStorageManagers(res.ID)
	if err != nil {
		log.Printf("listing storage managers: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"managers": managers,
		// What the word "manage" promises here, and the page has to say which:
		// where there are objects behind the resource a delegation also confers
		// the data plane at its ceiling, and where there are none it confers
		// nothing but the ability to attach. Both facts are sent rather than
		// derived in the client, so the copy cannot promise a page that does
		// not exist — and the kind alone stopped being able to answer this the
		// moment a file store could be a bucket mounted as a filesystem.
		"kind":           res.Kind,
		"has_data_plane": s.dataPlaneOffered(*res),
	})
}

func (s *Server) handlePutStorageManager(w http.ResponseWriter, r *http.Request) {
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

	var req putStorageManagerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	if req.SubjectType != database.StoreSubjectUser && req.SubjectType != database.StoreSubjectGroup {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `subject_type must be "user" or "group"`, "hint": `send "subject_type": "user" or "group"`})
		return
	}
	// The ceiling is on the kind's own ladder. Checked here for a readable
	// message and by a trigger underneath, because a constraint only the
	// handler enforces lasts until somebody writes a second caller.
	if strings.TrimSpace(req.MaxAccess) == "" {
		missingMaxAccess(w, req.Access, database.StorageAccessLevels(res.Kind))
		return
	}
	if !database.ValidStorageAccess(res.Kind, req.MaxAccess) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("a %s offers %s — %q is not one of them%s",
				res.Kind, strings.Join(database.StorageAccessLevels(res.Kind), ", "), req.MaxAccess, database.StorageAccessRefusal(req.MaxAccess)), "hint": `send "max_access" as one of the levels listed, like "read"`})
		return
	}
	// A volume under a shared root serves only the root's level (ADR §146), so
	// a ceiling below it is a delegation nobody can use: the root's level is
	// above the ceiling, and every level under it the root cannot serve. It
	// was accepted and then refused at the delegate's first attach (drills
	// 2026-11-03).
	if root, ok := s.sharedRootOf(res); ok && !root.ReadOnly &&
		!database.StoreAccessAtLeast(req.MaxAccess, runtime.AccessFull) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code": "storage_level_unsupported",
			"error": fmt.Sprintf("%q lives under storage root %q, which is mounted read-write, so it is "+
				"attached at full and nothing else — a delegation up to %q could never be used. "+
				"Delegate it at full, or ask an operator for a read-only mount and point the resource at that root",
				res.Name, root.Name, req.MaxAccess), "hint": `send "max_access": "full"`})
		return
	}
	// The subject has to exist. Without this a typo becomes a delegation to
	// nobody that sits in the list looking like it works.
	if !s.storeSubjectExists(req.SubjectType, req.SubjectID) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("no such %s", req.SubjectType), "hint": "GET /api/v1/users lists user ids and GET /api/v1/groups lists group ids"})
		return
	}

	m := &database.StorageManager{
		ResourceID:  res.ID,
		SubjectType: req.SubjectType,
		SubjectID:   req.SubjectID,
		MaxAccess:   req.MaxAccess,
	}
	meta := requestMeta(r.Context())
	if err := s.db.PutStorageManager(m, p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		if errors.Is(err, database.ErrStorageCeiling) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": `send "max_access" as a level this kind of storage offers, like "read"`})
			return
		}
		log.Printf("delegating storage management: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleDeleteStorageManager is the revocation, and the only route in this
// feature that changes what a running install is doing.
//
// Two steps, and only the first is a transaction: the rows land atomically
// because they *are* the authorization, and the restarts happen after it
// because they are cluster I/O that takes seconds and can fail. **The remount
// never fails the revoke** — the same principle every other side effect in this
// product follows.
func (s *Server) handleDeleteStorageManager(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())

	// The subject is one path segment, "user:<id>" or "group:<id>", because a
	// delegation's identity is the pair and a URL carrying only the id could
	// name two different subjects. The same shape as a grant's.
	subjectType, subjectID, found := strings.Cut(chi.URLParam(r, "subject"), ":")
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `the subject is "user:<id>" or "group:<id>"`, "hint": "name it in the path as user:<id> or group:<id>"})
		return
	}

	affected, err := s.db.DeleteStorageManagerAndMappings(chi.URLParam(r, "id"),
		subjectType, subjectID, p.ID, meta.RequestID, meta.IP, meta.UserAgent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/storage/" + chi.URLParam(r, "id") + "/managers lists who manages it"})
			return
		}
		log.Printf("revoking storage management: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Both modes: a shared app on a host root holds the gate's group, which only
	// a start takes away.
	s.supervisor.Remount(restartTargets(affected), "a storage delegation was revoked")

	// The apps come back in the response as well as in the confirmation that
	// preceded it: the administrator has just changed what somebody else's app
	// can reach, and the list is what lets them say so to the right people.
	writeJSON(w, http.StatusOK, map[string]any{
		"restarting": affected,
		"note":       revocationNote(affected),
	})
}

// revocationNote says what the platform is now doing, in the administrator's
// terms, and never promises more than a best effort.
func revocationNote(affected []database.StorageManagedApp) string {
	if len(affected) == 0 {
		return "The delegation is revoked. No app was mounting this through it, so nothing was restarted."
	}
	names := make([]string, 0, len(affected))
	for _, a := range affected {
		names = append(names, a.Name)
	}
	return fmt.Sprintf("The delegation is revoked and its mappings are gone. %s %s being restarted "+
		"onto its current release without the mount — no rebuild and no code change. An app that "+
		"does not come back will show as crashed.",
		strings.Join(names, ", "), pluralIsAre(len(affected)))
}

func pluralIsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// --- What the caller may attach ---

// handleMyAttachableStorage is "which resources may I attach, and at what
// ceiling" — the delegate's whole view of the feature.
//
// It is `/me/storage` rather than `/storage/resources`: there is no `/storage`
// prefix on this API at all — admin storage is `/admin/storage/*`, a store's own
// surface is `/store/*` — and `/me/*` is where this product puts the caller's
// own answer, beside `/me/capabilities` and `/me/access`.
//
// **No bucket and no prefix.** Those are the containment boundary and belong to
// an administrator; a delegate needs to pick a resource and a mount path, and
// neither requires knowing which bucket it is.
func (s *Server) handleMyAttachableStorage(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorage(w) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	resources, err := s.db.ListAttachableStorage(p.ID)
	if err != nil {
		log.Printf("listing attachable storage: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Whether a shared-mode app can hold storage at all on this install, so the
	// attach dialog warns when it would be attaching something the next deploy
	// refuses — and, just as importantly, stops warning where it would not.
	// Read from the same place the two refusals read it, so a delegate is never
	// told one thing while the deploy does another.
	writeJSON(w, http.StatusOK, map[string]any{
		"resources": resources, "shared_mode": s.sharedStorageOffered()})
}

// sharedStorageOffered asks the backend whether shared mode has anywhere to
// attach durable storage. False where nothing can answer, which is the same
// direction the builder's own question defaults in: offering a capability
// nobody confirmed is how a deploy succeeds and the bytes go nowhere.
func (s *Server) sharedStorageOffered() bool {
	l, ok := s.storage.(interface{ StorageKinds() []string })
	if !ok {
		return false
	}
	if _, isRoots := s.storage.(storageRootLister); !isRoots {
		return false
	}
	return len(l.StorageKinds()) > 0
}

// --- Attaching, from the app's side ---

// attachDecision is what AuthorizeAttach returns: whether this caller may
// attach this resource to this app, and the ceiling it may do it at.
type attachDecision struct {
	// Authority is the stamp the mapping carries — `admin`, or the delegation
	// this caller resolved through.
	Authority string
	// Ceiling is the highest level this caller may ask for. Returned rather
	// than re-derived by the caller, so the two enforcement points cannot
	// disagree about it.
	Ceiling string
}

// AuthorizeAttach is the one helper both writing routes consult.
//
// **Both halves are required, and there are three enforcement points.** A
// caller needs management on the resource at or above the level they are asking
// for, and `ActionEditContent` on the app — the same authorization that governs
// every other change to it. An administrator has the first unconditionally and
// keeps today's behaviour: any resource, any app.
//
// There is deliberately **no owner-consent step**. A delegate who may attach
// storage to an app may already change its environment, its branch and its
// start command; a consent gate here would be a control on the smallest of the
// things they can already do.
func (s *Server) AuthorizeAttach(r *http.Request, app *database.App, res *database.StorageResource) (attachDecision, bool) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		return attachDecision{}, false
	}
	if !auth.Authorize(p, auth.ActionEditContent, objectFromApp(app), s.db).Allow {
		return attachDecision{}, false
	}
	if p.IsAdmin() {
		// The resource's own ladder is the only ceiling an administrator has,
		// and the mapping is stamped `admin` — which is what makes an
		// administrator's edit of a delegate's mapping adopt it.
		return attachDecision{Authority: database.AuthorityAdmin, Ceiling: topLevelFor(res.Kind)}, true
	}
	delegation, ok, err := s.db.AttachAuthority(p.ID, res.ID)
	if err != nil {
		log.Printf("resolving attach authority: %v", err)
		return attachDecision{}, false
	}
	if !ok {
		return attachDecision{}, false
	}
	return attachDecision{Authority: delegation.AuthorityKey(), Ceiling: delegation.MaxAccess}, true
}

// topLevelFor is the top of a kind's ladder — an administrator's ceiling, which
// is to say no ceiling beyond what the kind offers.
func topLevelFor(kind string) string {
	levels := database.StorageAccessLevels(kind)
	return levels[len(levels)-1]
}

type attachAppStorageRequest struct {
	Path   string `json:"path"`
	Access string `json:"access"`
	// Folder is which directory of the resource to mount, and absent is the
	// whole resource. A delegate may name one freely: a folder is strictly less
	// than the resource they already manage, so the ceiling test is unchanged
	// and narrowing needs no permission of its own.
	Folder string `json:"folder"`
}

// handleAttachAppStorage is the mapping verb wearing the app's URL.
//
// Everything it refuses, it refuses as a 404 unless the caller has already been
// told the thing exists: an unknown or unreachable resource is "not found",
// never "forbidden", so these routes cannot enumerate an install's storage.
func (s *Server) handleAttachAppStorage(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorage(w) {
		return
	}
	app, res, ok := s.attachTarget(w, r)
	if !ok {
		return
	}
	decision, allowed := s.AuthorizeAttach(r, app, res)
	if !allowed {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "`toft storage ls` lists the storage you can attach; `toft storage ls --app <slug>` lists what an app has attached"})
		return
	}
	// After the 404, never before it: a caller with no delegation learns
	// nothing about the app. An attach is what the app runs with when the
	// lock lifts, the reason PATCH /env is refused (security review
	// 2026-11-15, LSA-2). Detaching stays open.
	if s.refuseIfLocked(w, app) {
		return
	}

	var req attachAppStorageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	// Every rule the administrator's attach applies, through the one validator
	// the manifest uses too — a refusal one caller skipped would be a mapping
	// the other could not have made. The cap is asked before the binding: a
	// refused row leaves its cluster object behind, so the cap has to bound the
	// objects, not only the rows.
	entry, refusal, err := s.validateAttach(app, res,
		attachEntry{Path: req.Path, Access: req.Access, Folder: req.Folder},
		attachOptions{Ceiling: decision.Ceiling, CheckRoom: true})
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

	// The binding for this level has to exist before an app is told it has one.
	// A delegate attaching therefore makes the platform create cluster objects
	// — accepted, and the bound is the ceiling an administrator already chose:
	// a store gets one binding per access level *in use*, so the whole install
	// tops out at four per resource.
	ctx, cancel := storageContext(r)
	defer cancel()
	if err := s.ensureBinding(ctx, res, access, folder); err != nil {
		// The detail goes to the log and not to the delegate. An app owner is
		// not an operator, and a driver's error string is an install's
		// internals; the admin route says more because its caller is the person
		// who would act on it.
		log.Printf("binding storage %s at %s for %s: %v", res.Slug, access, app.Slug, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "this install could not bind that storage. Nothing was changed — ask an administrator to look at the platform log."})
		return
	}

	mapping := &database.StorageMapping{
		ResourceID: res.ID, AppID: app.ID, Path: path, Access: access, Folder: folder,
		Authority: decision.Authority,
	}
	meta := requestMeta(r.Context())
	if err := s.db.PutStorageMapping(mapping, principalID(r), meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		if mappingRuleRefused(w, err) {
			return
		}
		switch {
		case errors.Is(err, database.ErrStorageNotManaged):
			// The delegation went away between AuthorizeAttach and the write.
			// The transaction re-read it under a lock, which is what makes this
			// a refusal rather than a mapping no revocation would ever collect.
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "`toft storage ls` lists the storage you can attach; `toft storage ls --app <slug>` lists what an app has attached"})
		case errors.Is(err, database.ErrStorageCeiling):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "send a lower access — `toft storage ls` shows your ceiling on each resource"})
		case isUniqueViolation(err):
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this app already mounts something at that path", "code": attachPathTaken,
				"hint": "send a \"path\" of its own, or leave path out for /storage/<name>[/<folder>]"})
		default:
			log.Printf("attaching storage: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
		return
	}
	s.rebindAfterPut(ctx, res, access, folder, app.Slug)

	writeJSON(w, http.StatusOK, map[string]any{
		"mapping": mapping,
		"note":    storageTakesEffectNote(app, s.sharedStorageOffered()),
	})
}

func (s *Server) handleDetachAppStorage(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorage(w) {
		return
	}
	app, res, ok := s.attachTarget(w, r)
	if !ok {
		return
	}
	if _, allowed := s.AuthorizeAttach(r, app, res); !allowed {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "`toft storage ls` lists the storage you can attach; `toft storage ls --app <slug>` lists what an app has attached"})
		return
	}

	folder, ok := storageFolderQuery(w, r)
	if !ok {
		return
	}
	// A delegate detaches what the console attached; toft.toml's entries go
	// by editing toft.toml. An administrator's detach of one is allowed.
	if s.refuseManifestMapping(w, res, app.ID, folder) {
		return
	}
	meta := requestMeta(r.Context())
	removed, err := s.db.DeleteStorageMapping(res.ID, app.ID, folder,
		principalID(r), meta.RequestID, meta.IP, meta.UserAgent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "`toft storage ls --app <slug>` lists what the app has attached"})
			return
		}
		log.Printf("detaching storage: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.sweepStorageBindings(r, res.ID, removed)
	writeJSON(w, http.StatusOK, map[string]any{"removed": len(removed)})
}

// attachTarget loads the app and the resource, answering 404 for either — the
// app because that is what § Authorization requires of anything taking an id
// from a URL, and the resource because a delegate must not learn which
// resources exist by asking for them one id at a time.
func (s *Server) attachTarget(w http.ResponseWriter, r *http.Request) (*database.App, *database.StorageResource, bool) {
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return nil, nil, false
	}
	res, err := s.db.GetStorageResource(chi.URLParam(r, "resourceID"))
	if err != nil || res == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "`toft storage ls` lists the storage you can attach; `toft storage ls --app <slug>` lists what an app has attached"})
		return nil, nil, false
	}
	return app, res, true
}

func principalID(r *http.Request) string {
	if p := auth.GetPrincipal(r.Context()); p != nil {
		return p.ID
	}
	return ""
}

// restartTargets narrows the storage rows to what the supervisor needs. The
// list in the response stays the fuller shape — an administrator is owed the
// path and the access level they just took away, which the restart is not.
func restartTargets(apps []database.StorageManagedApp) []supervisor.RestartTarget {
	out := make([]supervisor.RestartTarget, 0, len(apps))
	for _, a := range apps {
		out = append(out, supervisor.RestartTarget{ID: a.ID, Slug: a.Slug})
	}
	return out
}
