package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/gallery"
)

// The gallery: a catalog the Toft team publishes, and the three things an
// install does with it — browse it, deploy from it, and (for extensions) take
// an update.
//
// Two rules run through every handler here.
//
// The catalog is data, never authority. An entry names code and may ask for
// environment variables; it cannot grant a capability, change a setting, or
// widen anyone's permissions. Everything an extension needs beyond its own code
// is an administrator's separate, audited action afterwards.
//
// And the switch curates rather than contains. `gallery_deploy_mode` decides
// what the gallery offers; it does not decide where apps may come from, because
// anyone who may publish can still paste the same repository into New App. The
// admin panel says so beside the control, and nothing here should be written as
// though the switch were an allow-list.

// galleryAvailable answers "does this install offer the gallery at all", which
// is one question with two sources: whether this build was started with the
// service, and whether an administrator has turned it on. Both mean absent
// rather than disabled — the routes 404, exactly as the storage and local
// sign-in routes do.
func (s *Server) galleryAvailable() (string, bool) {
	if s.gallery == nil {
		return database.GalleryOff, false
	}
	mode := s.gallery.Mode()
	return mode, mode != database.GalleryOff
}

type galleryInstalled struct {
	AppID   string `json:"app_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// Mine distinguishes the copy this person deployed from somebody else's,
	// which for an example is the whole question being asked.
	Mine bool `json:"mine"`
	// UpdateAvailable is per instance, because two administrators may hold
	// copies on different versions and each is entitled to its own answer.
	UpdateAvailable bool `json:"update_available,omitempty"`
	// VersionUnknown means the installed or catalog version could not be
	// compared. Reported rather than swallowed: "cannot tell" must never
	// render as "up to date".
	VersionUnknown bool `json:"version_unknown,omitempty"`
	// VersionNotRecorded is the *reason* for the commonest VersionUnknown, and
	// it is a different sentence: nothing was written down at install, rather
	// than something written down that will not parse. An app installed before
	// the version was recorded at all is the case here, and telling its owner
	// the platform "cannot determine" a version reads as a fault in a working
	// app. Only ever set once the app has actually deployed — before that the
	// empty version means the first build has not landed, which the copy
	// already had a word for.
	VersionNotRecorded bool `json:"version_not_recorded,omitempty"`
}

type galleryItemResponse struct {
	gallery.Item
	Deployable    bool               `json:"deployable"`
	BlockedReason string             `json:"blocked_reason,omitempty"`
	Installed     []galleryInstalled `json:"installed"`
	// UpdateAvailable is true when *any* installed instance is behind.
	UpdateAvailable bool `json:"update_available"`
}

type galleryResponse struct {
	Mode  string                       `json:"mode"`
	Items []galleryItemResponse        `json:"items"`
	Sync  database.GalleryCatalogCache `json:"sync"`
	Admin bool                         `json:"admin"`
	// SourceBlocked carries the one banner shown above the whole catalog when
	// this install's source policy is restricted. Empty when it is not.
	SourceBlocked string `json:"source_blocked,omitempty"`
}

// gallerySourceBlocked reports why every Deploy button on this page is off, or
// "" when they are not.
//
// **The gallery's one-click deploy is off entirely whenever the source policy
// is restricted**, extensions included. The catalog is fetched from a
// repository the Toft team controls and its extensions point at public
// GitHub, so exempting it would leave a working one-click deploy from
// github.com on an install that had just blocked github.com — the single most
// embarrassing possible gap in this control. Curation by the vendor is not the
// same fact as approval by this install's administrator, and only the second
// one is what an allowlist encodes.
//
// A per-kind rule would be more precise, and it is available with no schema
// change: extensions follow the git policy, examples follow the upload switch,
// and an install that allowlists the vendor's organization would keep its
// catalog working. It is not what is built, because this is a rule an
// administrator can predict without holding two models in their head — and
// because gallery extension deploys are already administrator-only, which caps
// what the coarseness costs.
func (s *Server) gallerySourceBlocked() string {
	policy, err := s.sourcePolicy()
	if err != nil {
		log.Printf("gallery: reading the source policy, blocking gallery deploys: %v", err)
		return "This install's source policy could not be read, so deploying from the gallery is " +
			"unavailable."
	}
	if policy.Restricted() {
		return "Administrators have blocked deploying from the gallery on this install. " +
			"Templates below are shown for reference."
	}
	if !policy.UploadsEnabled {
		// The git policy is open, so extensions are fine. Examples are ZIPs by
		// design and reach an install through the upload switch, so they are
		// refused per item below rather than behind this banner.
		return ""
	}
	return ""
}

// exampleUploadsBlocked is the other half: an example is an upload, so it is
// governed by the upload switch even when git is wide open. Without this, the
// one path that fetches an archive on the platform's own initiative would be
// the one path that ignored the switch turning archives off.
func (s *Server) exampleUploadsBlocked(kind string) string {
	if kind != gallery.KindExample {
		return ""
	}
	policy, err := s.sourcePolicy()
	if err != nil || !policy.UploadsEnabled {
		return "Administrators have switched off deploying apps from an uploaded archive on this install, " +
			"and an example is a ZIP."
	}
	return ""
}

// handleListGallery returns what this viewer may see.
//
// The filtering is server-side and is an access control, not a convenience: a
// UI that hid extensions from a publisher while the API listed them would be
// disclosing which add-ons an install runs to everyone who can sign in.
func (s *Server) handleListGallery(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.galleryAvailable()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "an administrator can turn the gallery on with gallery_deploy_mode in PUT /api/v1/system/settings"})
		return
	}
	p := auth.GetPrincipal(r.Context())
	// The gallery is a list of things to deploy. Someone who may not publish
	// has nothing to do with it, and a catalogue you cannot act on is noise.
	if d := auth.Authorize(p, auth.ActionPublish, nil, s.db); !d.Allow {
		writeAuthzError(w, r, d, true)
		return
	}

	admin := p.IsAdmin()
	items := s.gallery.ItemsFor(mode, admin)

	resp := galleryResponse{Mode: mode, Admin: admin, Items: make([]galleryItemResponse, 0, len(items))}
	if sync, err := s.db.GetGalleryCatalog(); err == nil {
		resp.Sync = sync
	}
	// The catalog stays browsable — it has reference value, and hiding it would
	// tell an administrator nothing about why. What goes is every Deploy button,
	// behind one banner rather than the same sentence repeated per card.
	resp.SourceBlocked = s.gallerySourceBlocked()

	for _, it := range items {
		entry := galleryItemResponse{Item: it}
		entry.Deployable, entry.BlockedReason = s.gallery.Deployable(it, mode, admin)
		if resp.SourceBlocked != "" {
			entry.Deployable, entry.BlockedReason = false, resp.SourceBlocked
		} else if why := s.exampleUploadsBlocked(it.Kind); why != "" {
			entry.Deployable, entry.BlockedReason = false, why
		}
		entry.Installed = s.installedInstances(p, it)
		for _, inst := range entry.Installed {
			if inst.UpdateAvailable {
				entry.UpdateAvailable = true
			}
		}
		resp.Items = append(resp.Items, entry)
	}

	writeJSON(w, http.StatusOK, resp)
}

// installedInstances answers "is this already installed" from live app rows.
// Never a stored flag: an app that is deleted stops being installed the moment
// it is gone, and a boolean on the item would go on saying otherwise.
//
// The two kinds ask different questions. An extension is an install-wide fact —
// an administrator wants to know whether this add-on is running here at all.
// An example is a personal one: "did I already take a copy of this".
func (s *Server) installedInstances(p *auth.Principal, it gallery.Item) []galleryInstalled {
	apps, err := s.db.ListAppsByGalleryItem(it.ID)
	if err != nil {
		log.Printf("gallery: listing installs of %s: %v", it.ID, err)
		return nil
	}

	out := make([]galleryInstalled, 0, len(apps))
	for i := range apps {
		app := &apps[i]
		// "Mine" means mine or my group's. It is a display predicate rather
		// than an authorization one — CanRead below is what actually decides —
		// and an owner who cannot find their own app under "mine" because a
		// group holds the title is exactly the confusion group ownership is
		// meant to remove.
		mine := p != nil && (app.OwnerID == p.ID || (app.OwnerGroupID != "" && slices.Contains(p.Groups, app.OwnerGroupID)))
		if it.Kind == gallery.KindExample && !mine {
			continue
		}
		// An instance the viewer may not read is left out entirely rather than
		// counted-but-not-linked. The question does not arise today —
		// extensions are shown only to administrators, who may read every app,
		// and an example is filtered to the viewer's own — but if that ever
		// changes, omitting is the behaviour that cannot leak a slug.
		if !auth.CanRead(p, objectFromApp(app), s.db) {
			continue
		}
		inst := galleryInstalled{
			AppID: app.ID, Slug: app.Slug, Name: app.Name,
			Version: app.GalleryVersion, Mine: mine,
		}
		// Only extensions have an update channel. An example is a snapshot its
		// owner owns from the moment it lands, and telling them a newer one
		// exists would be an offer this platform cannot honour.
		if it.Kind == gallery.KindExtension {
			if _, ok := gallery.CompareVersions(it.Version, app.GalleryVersion); !ok {
				inst.VersionUnknown = true
				inst.VersionNotRecorded = app.GalleryVersion == "" && app.CurrentRelease != ""
			} else {
				inst.UpdateAvailable = gallery.IsNewer(it.Version, app.GalleryVersion)
			}
		}
		out = append(out, inst)
	}
	return out
}

// handleGallerySync is the administrator's Sync now.
func (s *Server) handleGallerySync(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.galleryAvailable(); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "an administrator can turn the gallery on with gallery_deploy_mode in PUT /api/v1/system/settings"})
		return
	}
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return
	}

	if err := s.gallery.SyncNow(r.Context()); err != nil {
		if errors.Is(err, gallery.ErrSyncTooSoon) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": "The catalog was refreshed less than a minute ago."})
			return
		}
		log.Printf("gallery: manual sync: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	sync, err := s.db.GetGalleryCatalog()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, sync)
}

type galleryDeployRequest struct {
	// Both optional: the catalog's name and suggested slug are the defaults,
	// and a second copy of an item needs a different name rather than a
	// different item.
	Name string `json:"name"`
	Slug string `json:"slug"`

	EnvVars      map[string]string `json:"env_vars"`
	EnvSensitive map[string]bool   `json:"env_sensitive"`

	// ExecutionMode is the deployer's, on the same terms as the other two
	// create paths — and it is read from the *caller*, never from the catalog
	// item. The catalog is data, never authority: a JSON file the platform
	// fetched from somewhere else does not get to decide that an install
	// spends elastic capacity.
	ExecutionMode string `json:"execution_mode"`
}

// handleGalleryDeploy creates an app from a catalog item.
//
// Both kinds land in machinery that already exists — an extension is a normal
// git-sourced app, an example is the upload path with the platform doing the
// fetching — and that is deliberate. A gallery deploy must not be a third way
// to run code with rules of its own.
func (s *Server) handleGalleryDeploy(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.galleryAvailable()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "an administrator can turn the gallery on with gallery_deploy_mode in PUT /api/v1/system/settings"})
		return
	}

	p := auth.GetPrincipal(r.Context())
	if d := auth.Authorize(p, auth.ActionPublish, nil, s.db); !d.Allow {
		writeAuthzError(w, r, d, true)
		return
	}

	itemID := chi.URLParam(r, "itemID")
	item, found := s.gallery.Item(itemID)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such gallery item", "hint": "GET /api/v1/gallery lists the items you can deploy"})
		return
	}
	// An extension a publisher cannot see must also be one they cannot name.
	// 404 rather than 403 for the same reason unreadable objects are 404
	// everywhere else: a refusal that confirms the thing exists is a disclosure.
	if item.Kind == gallery.KindExtension && !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such gallery item", "hint": "GET /api/v1/gallery lists the items you can deploy"})
		return
	}
	if item.Kind == gallery.KindExample && mode != database.GalleryAll {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such gallery item", "hint": "GET /api/v1/gallery lists the items you can deploy"})
		return
	}
	if deployable, why := s.gallery.Deployable(item, mode, p.IsAdmin()); !deployable {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": why, "hint": "GET /api/v1/gallery lists the items you can deploy"})
		return
	}
	// Enforcement point 6. The route refuses on its own rather than relying on
	// the button being hidden: points 1 to 3 do not cover this path — a gallery
	// deploy goes through neither POST /apps nor a deploy route — and a hidden
	// button is not an access control.
	if why := s.gallerySourceBlocked(); why != "" {
		s.auditSourceRefusal(r, galleryItemSource(item), why, "")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": why, "code": "source_not_allowed"})
		return
	}
	if why := s.exampleUploadsBlocked(item.Kind); why != "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": why, "code": "uploads_not_allowed"})
		return
	}

	var req galleryDeployRequest
	if r.Body != nil {
		// An empty body is a legitimate request: take the catalog's own name
		// and slug and deploy it.
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = item.Name
	}
	if err := validateAppName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": `send a shorter "name" with no control characters, or leave it out to use the item's own`})
		return
	}
	if refuseEnvNames(w, req.EnvVars, nil) {
		return
	}

	// Before the slug is allocated and before anything is written, in the same
	// position the other two create paths check it.
	execMode, refusal := s.createMode(p, req.ExecutionMode)
	if refusal != nil {
		refusal.write(w)
		return
	}

	// Tier enforcement point 3, and well before `stageExampleArchive` below —
	// so a refused install fetches no archive, writes no row and leaves nothing
	// behind to explain.
	if s.refusedTierCap(w, r) {
		return
	}

	slug := req.Slug
	if slug == "" {
		slug = item.SuggestedSlug
	}
	if slug == "" {
		slug = defaultSlug(name, "")
	}
	// A proposal, whether the body's or the catalog's, so cut to fit as the
	// other create paths cut an unstated slug (slug_length.go).
	slug, err := s.ensureUniqueSlug(fitSlug(sanitizeSlug(slug), maxSlugLen))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// The catalog is data, never authority — including about what an app may
	// be called. No item, whatever it names, can land in the reserved `_`
	// namespace: the guides that once did are a public site now, and nothing
	// lives there.
	if rejectReservedSlug(w, slug) {
		return
	}

	// Who owns it, on the same terms as every other creation path — one
	// function, so a fourth path cannot answer differently.
	ownerID := appOwnerID(p)

	// What the item asked to be born as. The catalog is data and never
	// authority, so this moves exactly two columns the deploying administrator
	// could set by hand a minute later on the app's Sharing tab, and it can
	// never reach `public` — anonymous reach is the install's decision, gated on
	// a transition and withdrawable install-wide, and a document fetched over
	// the network does not get to reach across that.
	//
	// The value is already one of three words: gallery.Parse normalizes it, so
	// there is no fourth case here and no fallback to forget.
	visibility, unlisted := database.VisibilityPrivate, false
	switch item.Visibility {
	case gallery.BornViewable:
		visibility = database.VisibilityAuthenticated
	case gallery.BornHidden:
		unlisted = true
	}

	app := &database.App{
		Slug:          slug,
		Name:          name,
		OwnerID:       ownerID,
		Status:        database.AppStatusBuilding,
		Visibility:    visibility,
		BornUnlisted:  unlisted,
		ExecutionMode: string(execMode),
		// Provenance is written with the row, not by a follow-up update: an app
		// that existed for even a moment without the record of where it came
		// from is one whose update path silently does not exist.
		GalleryItemID: item.ID,
		GalleryKind:   item.Kind,
	}

	switch item.Kind {
	case gallery.KindExtension:
		app.RepoURL = item.RepoURL
		app.Branch = item.Ref
		app.RepoSubdir = item.Path
	case gallery.KindExample:
		// The example half is the upload path with the platform doing the
		// fetching. Deliberately so: builder.IsUploadedSource then reports
		// true, and every existing guard that means "this app has no remote" —
		// no branch, no auto-deploy, no git token — applies with no new code.
		app.RepoURL = builder.UploadScheme + exampleArchiveName(item)
		app.Branch = "upload"
	}

	// The example's archive is fetched and staged *before* the row exists, so a
	// download that fails leaves nothing behind to explain.
	var staged func() ([]builder.RemovedEntry, error)
	if item.Kind == gallery.KindExample {
		var cleanup func()
		staged, cleanup, err = s.stageExampleArchive(r, item, app)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		// The unpack removes the file when it runs; this is for the paths
		// below that return before it does.
		defer cleanup()
	}

	meta := requestMeta(r.Context())
	actorType, actorID := auditActor(p)
	if err := s.db.CreateAppTx(app, actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		log.Printf("gallery: creating app: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create app"})
		return
	}
	var removed []builder.RemovedEntry
	if staged != nil {
		var err error
		if removed, err = staged(); err != nil {
			// The app row is removed rather than left pointing at a directory
			// that was never written — the same repair the upload route makes.
			s.db.DeleteApp(app.ID)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "an administrator can sync the catalog with POST /api/v1/gallery/sync; then deploy the item again"})
			return
		}
	}

	// Env vars the deployer filled in. The catalog *asked*; it did not grant —
	// these are ordinary app env vars and nothing about the entry gives the app
	// anything it would not otherwise have.
	for k, v := range req.EnvVars {
		encrypted, err := s.encryptor.Encrypt(v)
		if err != nil {
			log.Printf("gallery: encrypting env var: %v", err)
			continue
		}
		s.db.SetEnvVar(app.ID, k, encrypted, req.EnvSensitive[k])
	}

	s.auditGallery(r, "gallery.deploy", app.ID, map[string]string{
		"item_id":  item.ID,
		"kind":     item.Kind,
		"version":  item.Version,
		"name":     item.Name,
		"source":   galleryItemSource(item),
		"app_slug": app.Slug,
	})

	deploy := &database.Deployment{
		AppID:  app.ID,
		Branch: app.Branch,
		Status: database.DeployStatusPending,
		// Carried through the build so the app row can be advanced on success
		// and only on success.
		GalleryVersion: item.Version,
	}
	stampDeployer(r, deploy)
	if err := s.db.CreateDeployment(deploy, s.deployRecord(r, "gallery")); err != nil {
		log.Printf("gallery: creating deployment: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create deployment"})
		return
	}
	s.enqueueBuild(&builder.BuildJob{
		App: app, Deployment: deploy,
		SkipClone: item.Kind == gallery.KindExample,
		Removed:   removed,
	})

	body := map[string]interface{}{
		"app":        app,
		"deployment": deploy,
	}
	if len(removed) > 0 {
		body["removed"] = removed
	}
	writeJSON(w, http.StatusCreated, body)
}

// handleDetachGallery makes an attached app an ordinary app.
//
// It is not an escape hatch from a restriction — there is none to escape, since
// nothing about an example's provenance changes what it may do. It is how
// somebody who has taken an extension in a direction of their own stops being
// offered updates that would overwrite it.
func (s *Server) handleDetachGallery(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.galleryAvailable(); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "an administrator can turn the gallery on with gallery_deploy_mode in PUT /api/v1/system/settings"})
		return
	}
	app, p, ok := s.appForEdit(w, r)
	if !ok {
		return
	}
	if app.GalleryItemID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this app did not come from the gallery", "hint": "leave it as it is — only an app deployed from the gallery has an attachment to remove"})
		return
	}

	before := map[string]string{"item_id": app.GalleryItemID, "kind": app.GalleryKind, "version": app.GalleryVersion}
	if err := s.db.ClearAppGallery(app.ID); err != nil {
		log.Printf("gallery: detaching %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	_ = p
	s.auditGallery(r, "gallery.detach", app.ID, before)

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "detached",
		"message": "This app is no longer linked to the gallery. It keeps running, and it now " +
			"behaves like any other app.",
	})
}

// detachGalleryAfterSourceChange unlinks an attached app whose code no longer
// comes from where the catalog said it did, and reports whether it did.
//
// Both callers are edits to an app's source: a branch change and a re-point.
// The alternative — keeping the attachment and later offering an "update" that
// would clone over the ref or the repository somebody deliberately chose — is a
// data-loss trap, and an installed version that no longer describes what is
// checked out is a claim this platform should not be making.
//
// A failure to detach is logged and swallowed: the source change has already
// happened and is the thing the caller asked for.
func (s *Server) detachGalleryAfterSourceChange(r *http.Request, app *database.App, why string) bool {
	if app.GalleryItemID == "" {
		return false
	}
	before := map[string]string{
		"item_id": app.GalleryItemID, "kind": app.GalleryKind, "version": app.GalleryVersion,
	}
	if err := s.db.ClearAppGallery(app.ID); err != nil {
		log.Printf("gallery: detaching %s after a %s: %v", app.Slug, why, err)
		return false
	}
	s.auditGallery(r, "gallery.detach", app.ID, before)
	app.GalleryItemID, app.GalleryKind, app.GalleryVersion = "", "", ""
	return true
}

// appForEdit resolves the app in the URL and checks the caller may edit it,
// writing the refusal itself. Every handler here scopes by the id in the URL,
// which is the rule for anything that takes one.
func (s *Server) appForEdit(w http.ResponseWriter, r *http.Request) (*database.App, *auth.Principal, bool) {
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return nil, nil, false
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return nil, nil, false
	}
	return app, p, true
}

// galleryPolicy is one app's view of its catalog attachment, for the policy
// block on GET /apps/{id} (ADR §20: a platform decision about one app is
// visible on that app).
//
// Returns nil for an app that did not come from the gallery, and for every app
// on an install where the gallery is off — off means the gallery is not part of
// this install, and a page describing something the viewer cannot act on is
// worse than one that says nothing.
func (s *Server) galleryPolicy(app *database.App) *appGalleryPolicy {
	if app.GalleryItemID == "" {
		return nil
	}
	if _, ok := s.galleryAvailable(); !ok {
		return nil
	}

	p := &appGalleryPolicy{
		ItemID:           app.GalleryItemID,
		Kind:             app.GalleryKind,
		InstalledVersion: app.GalleryVersion,
		Updatable:        app.GalleryKind == gallery.KindExtension,
	}

	item, listed := s.gallery.Item(app.GalleryItemID)
	if !listed {
		// The item was in the catalog when this app was installed and is not
		// now. Nothing can be offered, and saying so is the only honest
		// rendering — an app whose item was withdrawn is not "up to date".
		p.Retired = true
		return p
	}
	p.Name, p.Source, p.CatalogVersion = item.Name, galleryItemSource(item), item.Version

	if !p.Updatable {
		return p
	}
	if _, ok := gallery.CompareVersions(item.Version, app.GalleryVersion); !ok {
		p.VersionUnknown = true
		p.VersionNotRecorded = app.GalleryVersion == "" && app.CurrentRelease != ""
		return p
	}
	p.UpdateAvailable = gallery.IsNewer(item.Version, app.GalleryVersion)
	return p
}

// galleryFollow is what a plain redeploy of a gallery-linked app resolved to:
// the ref that will actually be cloned, the catalog version the build carries,
// and whether taking it moves the app forward.
type galleryFollow struct {
	// Ref is what to clone. It is the app's own branch unless the catalog has
	// moved the extension somewhere else.
	Ref string
	// RepoURL and Subdir are where to clone it from, and they move with Ref:
	// a catalog that moves an extension into a repository shared with other
	// items names a new repository and a folder of it in the same version.
	RepoURL string
	Subdir  string
	// Version is what the builder records on success, empty when this deploy
	// says nothing about the catalog.
	Version string
	// Item is the catalog entry that was followed, valid only when Version is
	// set.
	Item gallery.Item
	// Update is true when this redeploy takes a version newer than the one the
	// app is running — an update in everything but the button that started it,
	// so it is audited as one.
	Update bool
}

// galleryRedeployFollow decides what a plain redeploy of an extension builds.
//
// **A redeploy of an extension follows the catalog.** The alternative —
// rebuilding the ref recorded when the extension was installed — is what this
// replaced, and it made Redeploy the one button on an extension that quietly
// meant "put the old version back": the ref an extension carries is the ref the
// catalog named at install time, so a catalog that has since moved to a new tag
// was ignored by every redeploy, restart-after-crash repair and rollback-then-
// redeploy an administrator did. The gallery's own Update button was the only
// door to a version an administrator had already been told about.
//
// It follows only where taking an update is already allowed, and where it is
// not the answer is the ref on the app rather than a refusal — a redeploy has
// to keep working. So: extensions only (an example is a snapshot with no update
// channel), administrators only (ADR §28 — extensions are updated by
// administrators, and which button was pressed does not change that), never
// while the gallery's deploys are blocked by the source policy, never for an
// item that has left the catalog, and never onto a version this build is too
// old for. Those are the update route's gates, minus the two refusals that only
// make sense when someone asked for an update by name.
//
// The second case is the one that is not an update at all: an app already on
// the catalog's ref, whose recorded version is missing or does not compare —
// the shape a failed first install leaves behind. The build is the code the
// catalog names, so it carries the catalog's number and the app stops claiming
// a version it never had.
func (s *Server) galleryRedeployFollow(app *database.App, p *auth.Principal) galleryFollow {
	f := galleryFollow{Ref: app.Branch, RepoURL: app.RepoURL, Subdir: app.RepoSubdir}
	if app.GalleryItemID == "" || app.GalleryKind != gallery.KindExtension {
		return f
	}
	if _, on := s.galleryAvailable(); !on {
		return f
	}
	if p == nil || !p.IsAdmin() {
		return f
	}
	// The gallery's coarse rule, not `refusedSource`'s per-app one: this is the
	// one-click deploy arriving through a different button, and an install that
	// has blocked deploying from the catalog has blocked this too. It does not
	// add an eighth source enforcement point — the deploy route is already the
	// third, and it has refused before this is reached. A blocked follow is not
	// a refusal: the app redeploys the code it is running.
	if s.gallerySourceBlocked() != "" {
		return f
	}
	item, listed := s.gallery.Item(app.GalleryItemID)
	if !listed {
		return f
	}
	if ok, _ := item.SatisfiesPlatform(s.gallery.PlatformVersion()); !ok {
		return f
	}
	switch {
	case gallery.IsNewer(item.Version, app.GalleryVersion):
		f.Ref, f.Version, f.Item, f.Update = item.Ref, item.Version, item, true
		f.RepoURL, f.Subdir = item.RepoURL, item.Path
	case item.Ref == app.Branch && item.Version != "":
		f.Version, f.Item = item.Version, item
	}
	return f
}

// handleGalleryUpdate takes an extension to the version the catalog now names.
//
// It is a deployment on the *same app*: env vars, grants, storage, schedules,
// the URL and the release history all survive, which is what makes rollback the
// repair path when an update goes wrong. A new app would have thrown all of
// that away and left the old one running beside it.
func (s *Server) handleGalleryUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.galleryAvailable(); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "an administrator can turn the gallery on with gallery_deploy_mode in PUT /api/v1/system/settings"})
		return
	}
	app, p, ok := s.appForEdit(w, r)
	if !ok {
		return
	}
	// Extensions are an administrator's business at every step, not only at
	// the first one: taking an update deploys new code onto an app the whole
	// install depends on.
	if !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "Extensions are updated by administrators.", "hint": "ask an administrator to take the update"})
		return
	}
	// An update is a new release, so it takes the deploy rule of the lock
	// (docs/invariants.md § App locking); without it a locked extension had its
	// ref moved and a build spent before the builder refused the start (RO-4).
	if s.refuseDeployIfLocked(w, app) {
		return
	}

	if app.GalleryItemID == "" || app.GalleryKind != gallery.KindExtension {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "This app is not an extension installed from the gallery. Examples are " +
				"snapshots and have no update channel; deploy a fresh copy instead.", "hint": "deploy a fresh copy with POST /api/v1/gallery/{itemID}/deploy"})
		return
	}
	// Enforcement point 7. An update is a re-clone at a new ref on an app that
	// already exists, so neither of the creation points can catch it — only this
	// one and the builder. Refused for the same reason a first deploy is: the code is
	// coming from somewhere this install no longer deploys from.
	if why := s.gallerySourceBlocked(); why != "" {
		s.auditSourceRefusal(r, app.RepoURL, why, app.ID)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": why, "code": "source_not_allowed"})
		return
	}

	item, listed := s.gallery.Item(app.GalleryItemID)
	if !listed {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "This extension is no longer listed in the gallery, so there is no version " +
				"to update to.", "hint": "keep the installed version, or detach the app from the gallery with DELETE /api/v1/apps/{appID}/gallery"})
		return
	}
	if ok, why := item.SatisfiesPlatform(s.gallery.PlatformVersion()); !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "This version needs Toft " + why + " or newer.", "hint": "ask an operator to upgrade Toft, then take the update"})
		return
	}
	if _, comparable := gallery.CompareVersions(item.Version, app.GalleryVersion); !comparable {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "The installed version (" + orNone(app.GalleryVersion) + ") and the catalog's (" +
				orNone(item.Version) + ") cannot be compared, so the platform cannot tell whether " +
				"this would be an update. Deploy it by hand if you mean to.", "hint": "deploy the version you want by hand with `toft deploy`"})
		return
	}
	if !gallery.IsNewer(item.Version, app.GalleryVersion) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "This app is already running " + orNone(app.GalleryVersion) +
				", and the catalog offers " + item.Version + ".", "hint": "nothing to do — GET /api/v1/gallery shows when a newer version is offered"})
		return
	}

	pending, err := s.db.HasPendingDeployment(app.ID, time.Now().UTC().Add(-pendingDeployWindow))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if pending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a deployment is already queued for this app", "hint": "wait for the queued deployment to finish — `toft deployments list` shows it — then try again"})
		return
	}

	// The ref is what will actually be cloned, so it moves before the build is
	// enqueued. The recorded *version* does not — the builder writes that when
	// the build succeeds, which is what keeps a failed update from leaving the
	// app claiming code that never started.
	if err := s.moveToGallerySource(app, item.RepoURL, item.Ref, item.Path); err != nil {
		log.Printf("gallery: setting the source for %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	deploy := &database.Deployment{
		AppID:          app.ID,
		Branch:         item.Ref,
		Status:         database.DeployStatusPending,
		GalleryVersion: item.Version,
	}
	stampDeployer(r, deploy)
	if err := s.db.CreateDeployment(deploy, s.deployRecord(r, "gallery")); err != nil {
		log.Printf("gallery: creating the update deployment for %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create deployment"})
		return
	}

	s.auditGallery(r, "gallery.update", app.ID, map[string]string{
		"item_id": item.ID,
		"from":    app.GalleryVersion,
		"to":      item.Version,
		"source":  galleryItemSource(item),
	})
	s.enqueueBuild(&builder.BuildJob{App: app, Deployment: deploy})

	writeJSON(w, http.StatusAccepted, deploy)
}

// orNone renders an empty version as words. An app whose first build has not
// finished has no version, and "already running  and the catalog offers 1.4.0"
// is a sentence with a hole in it.
func orNone(v string) string {
	if strings.TrimSpace(v) == "" {
		return "no recorded version"
	}
	return v
}

// enqueueBuild hands a job to the build queue, and does nothing when there is
// no queue to hand it to.
//
// A Server without a builder is a test server — production wires one
// unconditionally — and the alternative here is a nil dereference in the middle
// of a handler whose interesting behaviour is everything that happened before
// this line. The row and its provenance are already written when this runs, so
// a dropped job leaves an app stuck `building`, which is exactly what a test
// wants and cannot happen anywhere else.
func (s *Server) enqueueBuild(job *builder.BuildJob) {
	if s.builder == nil {
		return
	}
	s.builder.Enqueue(job)
}

// queueMove rebuilds an app's running code at the address it should have now,
// when that differs from the one its release was built for (builder.QueueMove).
// Best effort, and logged: the change that called it is already saved, and the
// next flip of the app reconciles a move that was never queued.
func (s *Server) queueMove(appID string) {
	if s.builder == nil {
		return
	}
	if _, err := s.builder.QueueMove(appID); err != nil {
		log.Printf("apps: queueing a move of %s: %v", appID, err)
	}
}

// galleryItemSource is what the audit row records as "where this came from" —
// the repository and ref for an extension, the archive for an example. Under
// this design a ref is the whole identity of an extension version, so the audit
// entry keeps it rather than a version number alone.
func galleryItemSource(it gallery.Item) string {
	src := it.ZipURL
	if it.Kind == gallery.KindExtension {
		src = it.RepoURL + "@" + it.Ref
	}
	if it.Path != "" {
		src += ":" + it.Path
	}
	return src
}

// moveToGallerySource points an extension's app at the source a catalog
// version names, before the build that clones it is enqueued. The repository
// and the folder move as well as the ref, because an extension whose catalog
// moved it into a shared repository would otherwise clone its old repository
// at a tag only the new one has. Each column is written only when it differs,
// and SetAppRepoURL and SetAppRepoSubdir both clear last_seen_sha.
func (s *Server) moveToGallerySource(app *database.App, repoURL, ref, subdir string) error {
	if repoURL != "" && repoURL != app.RepoURL {
		if err := s.db.SetAppRepoURL(app.ID, repoURL); err != nil {
			return err
		}
		app.RepoURL = repoURL
	}
	if subdir != app.RepoSubdir {
		if err := s.db.SetAppRepoSubdir(app.ID, subdir); err != nil {
			return err
		}
		app.RepoSubdir = subdir
	}
	if ref != app.Branch {
		if err := s.db.SetAppBranch(app.ID, ref); err != nil {
			return err
		}
		app.Branch = ref
	}
	return nil
}

func (s *Server) auditGallery(r *http.Request, action, appID string, detail map[string]string) {
	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())
	actorID := ""
	if p != nil {
		actorID = p.ID
	}
	if err := s.db.WriteAuditRow(database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: actorID,
		Action: action, TargetType: "app", TargetID: appID,
		After:     jsonValStr(detail),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	}); err != nil {
		log.Printf("gallery: auditing %s: %v", action, err)
	}
}
