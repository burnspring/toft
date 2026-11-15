package api

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
)

// handleCheckBundle is `toft check`: the decisions a deploy of this bundle
// would make, without the deploy.
//
// **The doors are the upload's doors.** The same permission, the same upload
// switch and the same lock, refused the same way — a check that said yes to a
// bundle the upload route would refuse at its first line is the worst answer it
// could give. Past them, nothing is recorded: no deployment row, no event, no
// staging directory left behind, and the bundle is unpacked into a directory of
// its own so a check can run beside a deploy that is in flight.
//
// The answer is builder.Preflight's, plus what unpacking removed — the
// archive's own refusals (a link, a path that escapes) are the first thing a
// deploy would say about it.
func (s *Server) handleCheckBundle(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	if !builder.IsUploadedSource(app.RepoURL) && s.uploadsRefused(w) {
		return
	}
	// The deploy's lock rule, not PATCH's: a deploy is a dependency-locked
	// app's one way out, and `toft check` of the fixed tree was refused where
	// `toft deploy` of it was admitted (code review 2026-11-11, P1-6).
	if s.refuseDeployIfLocked(w, app) {
		return
	}
	// The version cap, as every deploy door asks it before reading a byte: it
	// is a standing state of the app, so a check that skipped it said ok to a
	// deploy that would be refused every time (AP-8).
	if why := s.capBlocks(app); why != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": why, "hint": "delete versions you no longer need: toft deployments prune deletes every one but the serving one, or DELETE /api/v1/apps/{id}/deployments"})
		return
	}
	deployed, _ := s.db.ListJobDefinitions(app.ID)
	s.checkBundle(w, r, filepath.Join(s.cfg.AppsDir, app.ID), app.Slug, app.DesiredMode(), app, deployed...)
}

// handleCheckNewBundle is `toft check` before `toft create`: the same answer,
// for an app that does not exist yet.
//
// **The doors are the create-by-upload route's** — publishing, the upload
// switch, the tier cap — and the mode is the one a create by this caller would
// be born in, `defaultCreateMode`, the same derivation `/me/capabilities`
// reports. Without it the check is unavailable at the one moment an agent most
// wants it, which is before it has claimed a name.
//
// **The owner is the create's owner.** owner_group_id, the form field the
// create-by-upload route reads and with the same meaning — the id of the group
// that will own the app, which `toft` resolves from a name before sending
// either — is answered by refusedOwnerGroup, the create routes' own check: a
// deploy key without one is owner_group_required, a group that cannot own is
// 422 with the sentence the create would give. A check that passed a group the
// create then refused is a CI gate that lied (docs/bugs 2026-10-28 #2).
func (s *Server) handleCheckNewBundle(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if d := auth.Authorize(p, auth.ActionPublish, nil, s.db); !d.Allow {
		writeAuthzError(w, r, d, true)
		return
	}
	if s.uploadsRefused(w) {
		return
	}
	if s.refusedTierCap(w, r) {
		return
	}
	if !parseBundleForm(w, r) {
		return
	}
	if s.refusedOwnerGroup(w, p, strings.TrimSpace(r.FormValue("owner_group_id"))) {
		return
	}
	mode := runtime.ModeShared
	if settings, err := s.db.GetPlatformSettings(); err == nil {
		mode = defaultCreateMode(s.capabilitiesFrom(settings), settings.DefaultExecutionMode)
	}
	// The app the create would make, as far as storage asks: its owner. A
	// caller creating with no group owns it themselves — a service account's
	// app is its owning user's, as the create makes it.
	sketch := &database.App{OwnerGroupID: strings.TrimSpace(r.FormValue("owner_group_id"))}
	if sketch.OwnerGroupID == "" && p != nil {
		sketch.OwnerID = p.ID
		if p.IsServiceAccount() {
			sketch.OwnerID = p.OwningUserID
		}
	}
	s.checkBundle(w, r, filepath.Join(s.cfg.AppsDir, ".preflight"), "a new app", mode, sketch)
}

// checkBundle unpacks the request's bundle under parent, runs the preflight
// for mode, answers, and removes what it unpacked.
func (s *Server) checkBundle(w http.ResponseWriter, r *http.Request, parent, label string, mode runtime.Mode,
	app *database.App, deployed ...database.JobDefinition) {
	if !parseBundleForm(w, r) {
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file field is required", "hint": `send the bundle as a multipart form with the archive in the "file" field`})
		return
	}
	defer file.Close()
	if archiveSuffix(header.Filename) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": unsupportedArchive, "hint": "send a .zip or a .tar.gz"})
		return
	}

	var suffix [6]byte
	rand.Read(suffix[:])
	dir := filepath.Join(parent, "preflight-"+hex.EncodeToString(suffix[:]))
	defer os.RemoveAll(dir)
	removed, err := extractArchive(file, header.Size, dir, header.Filename)
	if err != nil {
		log.Printf("check: extracting a bundle for %s: %v", label, err)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "failed to extract the archive: " + err.Error(), "code": "archive_invalid", "hint": "check that the archive opens on your own machine, then send it again"})
		return
	}

	var settings *database.PlatformSettings
	if ps, err := s.db.GetPlatformSettings(); err == nil {
		settings = &ps
	}
	pre := builder.RunPreflight(dir, mode, settings, s.envelope(), deployed...)
	// The storage plan, judged as the deploy would judge it: against this
	// app's table and owner, and against the caller as the deployer — a
	// check by a deploy key is a deploy key's deploy.
	if err := s.addStoragePlan(r, app, pre); err != nil {
		log.Printf("check: planning storage for %s: %v", label, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*builder.Preflight
		OK      bool                   `json:"ok"`
		Mode    string                 `json:"execution_mode"`
		Removed []builder.RemovedEntry `json:"removed,omitempty"`
	}{pre, pre.OK(), string(mode), removed})
}

// addStoragePlan judges the tree's [[storage]] table as the deploy would —
// against this app's table and owner, and against the caller as the deployer —
// and records the plan, its refusals among pre's problems. Both `toft check`
// and the two upload routes call it, so a refusal check names is the refusal
// the upload answers with, before a slug is claimed or a build queued.
func (s *Server) addStoragePlan(r *http.Request, app *database.App, pre *builder.Preflight) error {
	entries, parsed := pre.DeclaredStorage()
	if !parsed {
		return nil
	}
	sketch := *app
	if sketch.ID == "" || sketch.Framework == "" {
		sketch.Framework = pre.Framework
	}
	var d database.Deployment
	stampDeployer(r, &d)
	plan, err := s.PlanManifestStorage(builder.StoragePlanRequest{
		App: &sketch, Entries: entries, Deployer: builder.DeployerOf(&d)})
	if err != nil {
		return err
	}
	pre.AddStoragePlan(plan)
	return nil
}

// parseBundleForm caps the body and parses the check's multipart form,
// answering 400 and false when it did not arrive. Safe to call twice: a form
// already parsed is not read again, which is what lets the new-app check read
// owner_group_id before checkBundle asks for the file.
func parseBundleForm(w http.ResponseWriter, r *http.Request) bool {
	if r.MultipartForm != nil {
		return true
	}
	limitUploadBody(w, r)
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "the bundle did not arrive: invalid multipart form or file too large (max 100MB)", "hint": "send the bundle as a multipart form under 100MB; leave large data out of the bundle"})
		return false
	}
	return true
}
