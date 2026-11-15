package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runner"
	"toft/internal/version"
)

// The Posit Connect publishing surface: the dozen routes the `rsconnect` R
// package calls to publish, so an RStudio or Workbench user deploys to
// Toft with the Publish button and `rsconnect::deployApp()` they already
// use (ADR §109).
//
// **A translation, not a second platform.** Every route here is a door onto
// something that already exists: content is an app, a bundle is an uploaded
// archive, a deploy is the upload deploy, a task is a deployment and its build
// log. The gates are the ones the upload routes apply, called in the same
// order, so nothing a Connect client does is a decision `toft deploy` could not
// have made. What Connect has and Toft does not — content settings,
// ACLs, schedules, vanity URLs — has no route, and a client asking for one
// gets the router's 404 rather than an imitation.
//
// **It lives under /api/v1/connect and is reached as /__api__.** rsconnect
// appends `/__api__` to whatever server address it is given, and that is the
// path it sends; connectAPIRewrite moves the request onto this subtree before
// routing, so the session middleware, both key authenticators, both route
// guards, the usage counter and the origin check all run exactly as they do
// for the rest of the API. A parallel mount with its own copy of that stack
// would be a second definition of who may do what, and the first one somebody
// forgot to update.

// connectAPIPrefix is the path a Connect client sends; connectRoutePrefix is
// where it is served. Both are fixed by the client and the router respectively.
const (
	connectAPIPrefix   = "/__api__"
	connectRoutePrefix = "/api/v1/connect"
)

// connectCompatVersion is the Connect server version this surface reports.
//
// It is a statement about the wire protocol, not about Toft, and it is
// read for exactly one thing: rsconnect refuses to send Node content to a
// server older than 2026.04.0. Toft serves Node, so it says a version
// that admits it. Toft's own version travels beside it, under its own
// name, so nobody reading the response mistakes one for the other.
const connectCompatVersion = "2026.04.0"

// connectBundleTTL is how long an uploaded bundle waits to be deployed.
// rsconnect deploys the bundle it uploaded in the very next request, so an hour
// is generous; what outlives it was abandoned by a client that crashed.
const connectBundleTTL = time.Hour

// connectError writes the error shape rsconnect prints: it reads `error` off
// the body and shows it verbatim, so the sentence is the whole of the message.
func connectError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// handleConnectServerSettings is the probe rsconnect makes when a server is
// registered, unauthenticated, to decide whether the address is a Connect
// server at all. It answers JSON and nothing a stranger could not learn from
// the login page.
func (s *Server) handleConnectServerSettings(w http.ResponseWriter, r *http.Request) {
	// Both ways in are personal keys — presented, or bound to a signed token —
	// so both are as available as the capability is.
	keys := false
	if settings, err := s.db.GetPlatformSettings(); err == nil {
		keys = settings.APIKeysEnabled
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":            connectCompatVersion,
		"toft_version":       version.Version,
		"product":            "Toft",
		"authentication":     map[string]any{"name": "Toft", "handles_credentials": false},
		"api_key_auth":       keys,
		"tokens_enabled":     keys,
		"oauth_integrations": false,
	})
}

// handleConnectCurrentUser answers "who does this credential belong to", which
// is what rsconnect asks before writing the account down — after
// `connectApiUser()`, and as the poll RStudio's Add account repeats until its
// sign-in is approved (connect_auth.go).
func (s *Server) handleConnectCurrentUser(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsDeployKey() {
		connectError(w, http.StatusUnauthorized, "add this server as an account in RStudio, "+
			"or sign in with a personal API key: rsconnect::connectApiUser(server = ..., apiKey = \"toftu_...\")")
		return
	}
	userID := p.ID
	if p.IsServiceAccount() {
		userID = p.OwningUserID
	}
	u, err := s.db.GetUser(userID)
	if err != nil || u == nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	first, last, _ := strings.Cut(strings.TrimSpace(u.Name), " ")
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         u.ID,
		"guid":       u.ID,
		"username":   u.Login,
		"first_name": first,
		"last_name":  last,
		"email":      u.Email,
		"user_role":  connectRole(p.SystemRole),
	})
}

// connectRole names a Toft system role in Connect's three words. rsconnect
// reads none of them; a person comparing the two products does.
func connectRole(role string) string {
	switch role {
	case database.SystemRoleAdmin:
		return "administrator"
	case database.SystemRoleViewer:
		return "viewer"
	default:
		return "publisher"
	}
}

// connectContent is one app in the shape both Connect APIs describe content
// in. The v0 `applications` reader wants `url`, the v1 `content` reader wants
// `content_url`; both are sent from both, because the cost is one string and
// the alternative is two structs that drift.
//
// `name` is the slug — the name in the URL, which is what Connect's `name` is
// — and `id` and `guid` are both the app id, because rsconnect records the
// first and addresses v1 routes by the second and Toft has only one.
type connectContent struct {
	ID            string `json:"id"`
	GUID          string `json:"guid"`
	Name          string `json:"name"`
	Title         string `json:"title"`
	URL           string `json:"url"`
	ContentURL    string `json:"content_url"`
	DashboardURL  string `json:"dashboard_url"`
	OwnerGUID     string `json:"owner_guid"`
	OwnerUsername string `json:"owner_username,omitempty"`
	CreatedTime   string `json:"created_time"`
}

func (s *Server) connectContentFor(app *database.App) connectContent {
	owner := app.OwnerID
	if owner == "" {
		owner = app.OwnerGroupID
	}
	c := connectContent{
		ID:           app.ID,
		GUID:         app.ID,
		Name:         app.Slug,
		Title:        app.Name,
		URL:          builder.PublicURL(s.cfg.AppsBaseURL, app),
		DashboardURL: strings.TrimSuffix(s.cfg.BaseURL, "/") + "/apps/" + app.ID,
		OwnerGUID:    owner,
		CreatedTime:  app.CreatedAt.UTC().Format(time.RFC3339),
	}
	// Builder's PublicURL answers "" without an apps origin; the content then
	// lives under the platform's own address, which is where the proxy serves
	// it on a single-origin install.
	if c.URL == "" {
		c.URL = builder.PublicURL(s.cfg.BaseURL, app)
	}
	c.ContentURL = c.URL
	if app.OwnerID != "" {
		if u, err := s.db.GetUser(app.OwnerID); err == nil && u != nil {
			c.OwnerUsername = u.Login
		}
	}
	return c
}

// handleConnectListApplications is the v0 listing rsconnect pages through,
// asked in practice for one name: "is there already content called this?"
// The answer is drawn from the one definition of readable apps, narrowed to
// the ones the caller can open — an app held at the `list` rung is in their
// list and 404s by id, and a listing that offered it would send rsconnect to
// update content it cannot reach. A name the caller cannot open is reported as
// absent, and the create that follows is refused as taken, which is the
// answer Connect gives.
func (s *Server) handleConnectListApplications(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if refuseAnonymousConnect(w, p) {
		return
	}
	apps, err := s.listAppsFor(p, "", "")
	if err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// `filter=name:x&filter=account_id:y`, repeated, in rsconnect's spelling.
	var name, account string
	for _, f := range r.URL.Query()["filter"] {
		k, v, _ := strings.Cut(f, ":")
		switch k {
		case "name":
			name = v
		case "account_id":
			account = v
		}
	}

	matched := []connectContent{}
	for i := range apps {
		a := &apps[i]
		if name != "" && a.Slug != name && a.Slug != sanitizeSlug(name) {
			continue
		}
		if account != "" && a.OwnerID != account {
			continue
		}
		if !auth.CanRead(p, objectFromApp(a), s.db) {
			continue
		}
		matched = append(matched, s.connectContentFor(a))
	}

	start, _ := strconv.Atoi(r.URL.Query().Get("start"))
	count, _ := strconv.Atoi(r.URL.Query().Get("count"))
	if start < 0 || start > len(matched) {
		start = len(matched)
	}
	if count <= 0 || count > 500 {
		count = 100
	}
	end := min(start+count, len(matched))
	writeJSON(w, http.StatusOK, map[string]any{
		"applications": matched[start:end],
		"count":        end - start,
		"total":        len(matched),
		"continuation": strconv.Itoa(end),
	})
}

// refuseAnonymousConnect answers a Connect app read that carries no
// credential. The platform API answers no app read to a request without one
// (docs/invariants.md § Anonymous access), and the Connect surface sits
// outside the /api/v1/apps patterns that rule is enforced on: a bare GET of
// `/__api__/applications` listed every public app's owner and URLs (security
// review 2026-11-10, B-1). server_settings, tokens and the token exchange
// stay open, because rsconnect asks them before it has a credential.
func refuseAnonymousConnect(w http.ResponseWriter, p *auth.Principal) bool {
	if p != nil && p.ID != "" {
		return false
	}
	connectUnauthorized(w, "this request needs an API key")
	return true
}

// connectAppForRead loads the app in the URL and answers 404 for one the caller
// cannot read, the same split every by-id route makes.
func (s *Server) connectAppForRead(w http.ResponseWriter, r *http.Request) (*database.App, bool) {
	if refuseAnonymousConnect(w, auth.GetPrincipal(r.Context())) {
		return nil, false
	}
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return nil, false
	}
	if !auth.CanRead(auth.GetPrincipal(r.Context()), objectFromApp(app), s.db) {
		connectError(w, http.StatusNotFound, "content not found")
		return nil, false
	}
	return app, true
}

// connectAppForEdit is connectAppForRead with the edit check every route that
// changes an app makes, refused the way those routes refuse.
func (s *Server) connectAppForEdit(w http.ResponseWriter, r *http.Request) (*database.App, bool) {
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return nil, false
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, auth.CanRead(p, obj, s.db))
		return nil, false
	}
	return app, true
}

// handleConnectGetContent serves both spellings of "this one item": v0
// `/applications/{id}`, which rsconnect uses to find a recorded deployment
// again, and v1 `/content/{guid}`.
func (s *Server) handleConnectGetContent(w http.ResponseWriter, r *http.Request) {
	app, ok := s.connectAppForRead(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.connectContentFor(app))
}

// handleConnectCreateContent is Connect's empty content item: a name and a
// title, and no code until a bundle is deployed into it.
//
// Toft apps are otherwise born with their first deploy, so this is the
// one way to have an app with no deployment, and it is the shape of an
// uploaded app that has not been deployed yet: `upload://` source, stopped,
// nothing to start. Every refusal the upload create makes is made here, in
// its order — who may publish, the upload switch, the license's app cap, the
// execution mode, the name, the reserved namespace — because this is a third
// create path and a credential accepted at one door and refused at another is
// how an estate ends up half-governed.
//
// **The name is not quietly changed.** The upload create suffixes a taken
// slug; this one refuses it, as Connect does, because rsconnect looks the
// content up by the name it sent and would never find a renamed one.
func (s *Server) handleConnectCreateContent(w http.ResponseWriter, r *http.Request) {
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

	var req struct {
		Name  string `json:"name"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		connectError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		connectError(w, http.StatusBadRequest, "name is required")
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = req.Name
	}
	if err := validateAppName(title); err != nil {
		connectError(w, http.StatusBadRequest, err.Error())
		return
	}

	mode, refusal := s.createMode(p, "")
	if refusal != nil {
		refusal.write(w)
		return
	}

	slug := sanitizeSlug(req.Name)
	// The bound every other create route applies: an isolated release's object
	// name is slug + "-" + 8 hex within one DNS label, so a longer slug would
	// share one object name across releases (AP-7). Refused, never cut:
	// rsconnect finds its content again by this name.
	if len(slug) > maxSlugLen {
		connectError(w, http.StatusBadRequest, slugTooLongMessage(slug))
		return
	}
	if rejectReservedSlug(w, slug) {
		return
	}
	taken, err := s.db.SlugExists(slug)
	if err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if taken {
		connectError(w, http.StatusConflict, fmt.Sprintf(
			"the name %q is already in use on this server — deploy with a different appName", slug))
		return
	}

	// Unreachable by a deploy key, which the route guard refuses: what is left
	// is a person, or a service account that answers with its owner.
	ownerID := appOwnerID(p)
	if ownerID == "" {
		connectError(w, http.StatusUnprocessableEntity, "this credential has no owner to give the content")
		return
	}

	app := &database.App{
		Slug:          slug,
		Name:          title,
		OwnerID:       ownerID,
		RepoURL:       builder.UploadScheme + slug + ".tar.gz",
		Branch:        "upload",
		Status:        database.AppStatusStopped,
		Visibility:    database.VisibilityPrivate,
		ExecutionMode: string(mode),
	}
	meta := requestMeta(r.Context())
	actorType, actorID := auditActor(p)
	if err := s.db.CreateAppTx(app, actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		log.Printf("connect: creating app %s: %v", slug, err)
		connectError(w, http.StatusInternalServerError, "failed to create content")
		return
	}
	writeJSON(w, http.StatusOK, s.connectContentFor(app))
}

// connectBundleDir is where a bundle waits between its upload and its deploy:
// inside the app's own directory, so deleting the app takes them with it.
func connectBundleDir(appsDir, appID string) string {
	return filepath.Join(appsDir, appID, "connect-bundles")
}

// validBundleID is the shape newBundleID makes, and the only shape a bundle id
// read back off a request may have — it becomes a file name.
func validBundleID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// handleConnectUploadBundle receives a bundle — the gzipped tar rsconnect
// builds, sent as the raw body — and keeps it until the deploy that names it.
//
// Connect splits upload and deploy into two calls and Toft's upload route
// is both at once, so the bytes wait on disk in between. Nothing is unpacked
// here and no deployment row exists yet: a bundle that is never deployed is
// not a deploy that failed, it is a file that expires.
func (s *Server) handleConnectUploadBundle(w http.ResponseWriter, r *http.Request) {
	app, ok := s.connectAppForEdit(w, r)
	if !ok {
		return
	}
	if !builder.IsUploadedSource(app.RepoURL) && s.uploadsRefused(w) {
		return
	}
	// The deploy rule, as POST /apps/{id}/upload takes it: a dependency
	// policy's lock is lifted by the release that drops the package, so
	// refusing it here refused the way out the 409 names (RO-3).
	if s.refuseDeployIfLocked(w, app) {
		return
	}

	// One upload per app at a time, so two cannot both pass the count below.
	release, claimed := claimConnectBundleSlot(app.ID)
	defer release()
	if !claimed {
		connectError(w, http.StatusTooManyRequests,
			"another bundle upload to this content is in progress; wait for it and try again")
		return
	}

	dir := connectBundleDir(s.cfg.AppsDir, app.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("connect: bundle directory for %s: %v", app.Slug, err)
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	expireConnectBundles(dir, time.Now())
	// Bounded, so an editor cannot park unbounded bytes on the disk every
	// app's releases live on by uploading and never deploying (AP-6). Older
	// bundles are not deleted to make room: a deploy may have one open.
	if entries, err := os.ReadDir(dir); err == nil && len(entries) >= maxWaitingConnectBundles {
		connectError(w, http.StatusTooManyRequests, fmt.Sprintf(
			"%d bundles are already waiting to be deployed for this content; deploy one, or wait an hour for them to expire",
			len(entries)))
		return
	}

	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	id := hex.EncodeToString(raw[:])
	path := filepath.Join(dir, id+".tar.gz")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// The upload routes' own ceiling, counted as the bytes arrive.
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, 100<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			connectError(w, http.StatusRequestEntityTooLarge, "the bundle is larger than 100MB")
			return
		}
		connectError(w, http.StatusBadRequest, "the bundle did not arrive")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           id,
		"content_guid": app.ID,
		"created_time": time.Now().UTC().Format(time.RFC3339),
		"size":         n,
	})
}

// expireConnectBundles removes bundles nobody deployed. Run on the next upload
// into the same app rather than on a timer, because that is the only moment
// anything else is written there.
func expireConnectBundles(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > connectBundleTTL {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// handleConnectDeploy deploys an uploaded bundle: the upload deploy, with the
// bytes read off the disk instead of a form. The gates are POST
// /apps/{id}/upload's — edit, the upload switch for a git-backed app, the
// lock, one deploy at a time, the retention cap — in that order, and the
// deployment it starts is recorded, unpacked and built by the same code. Its
// id is the task id, which is what rsconnect polls.
func (s *Server) handleConnectDeploy(w http.ResponseWriter, r *http.Request) {
	app, ok := s.connectAppForEdit(w, r)
	if !ok {
		return
	}
	if !builder.IsUploadedSource(app.RepoURL) && s.uploadsRefused(w) {
		return
	}
	// The deploy rule, as POST /apps/{id}/upload takes it: a dependency
	// policy's lock is lifted by the release that drops the package, so
	// refusing it here refused the way out the 409 names (RO-3).
	if s.refuseDeployIfLocked(w, app) {
		return
	}

	var req struct {
		BundleID string `json:"bundle_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		connectError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Connect would redeploy the newest bundle when none is named. There is no
	// newest bundle here once one has been deployed — it became a release —
	// and rsconnect always names one, so the absence is refused.
	if !validBundleID(req.BundleID) {
		connectError(w, http.StatusBadRequest, "bundle_id is required — upload a bundle first")
		return
	}
	bundlePath := filepath.Join(connectBundleDir(s.cfg.AppsDir, app.ID), req.BundleID+".tar.gz")
	bundle, err := os.Open(bundlePath)
	if err != nil {
		connectError(w, http.StatusNotFound, "bundle not found — it may have expired; deploy again")
		return
	}
	defer func() {
		bundle.Close()
		os.Remove(bundlePath)
	}()
	info, err := bundle.Stat()
	if err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// The staging directory is the app's one, as on the upload route.
	release, ok := claimUploadSlot(app.ID)
	if !ok {
		connectError(w, http.StatusConflict, "another upload to this app is in progress")
		return
	}
	defer release()

	pending, err := s.db.HasPendingDeployment(app.ID, time.Now().UTC().Add(-pendingDeployWindow))
	if err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if pending {
		connectError(w, http.StatusConflict, "a deployment is already queued for this app")
		return
	}
	if why := s.capBlocks(app); why != "" {
		connectError(w, http.StatusConflict, why)
		return
	}

	up := s.beginUpload(w, r, app, "upload", "connect")
	if up == nil {
		return
	}
	up.enter(builder.StepUnpack)
	stagingDir := builder.UploadStagingDir(s.cfg.AppsDir, app.ID)
	os.RemoveAll(stagingDir)
	removed, err := extractArchive(bundle, info.Size(), stagingDir, "bundle.tar.gz")
	if err != nil {
		log.Printf("connect: extracting bundle for %s: %v", app.Slug, err)
		up.fail(w, http.StatusBadRequest, fmt.Sprintf("failed to extract the bundle: %v", err))
		return
	}

	deploy := up.handOff()
	// Subscribed before the build is queued, so the first line a worker writes
	// is already being kept.
	s.connectTasks.follow(s, deploy.ID)
	s.enqueueBuild(&builder.BuildJob{App: app, Deployment: deploy, SkipClone: true, FromBundle: true, Removed: removed})

	writeJSON(w, http.StatusAccepted, map[string]any{"task_id": deploy.ID})
}

// handleConnectGetEnvironment lists the names of an app's variables — names
// only, as Connect does, because a value set through this API is a secret.
func (s *Server) handleConnectGetEnvironment(w http.ResponseWriter, r *http.Request) {
	app, ok := s.connectAppForEdit(w, r)
	if !ok {
		return
	}
	vars, err := s.db.GetEnvVars(app.ID)
	if err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		names = append(names, v.Key)
	}
	writeJSON(w, http.StatusOK, names)
}

// handleConnectPatchEnvironment sets and removes variables: `[{name, value}]`,
// where a null value deletes. rsconnect sends this when deployApp() is given
// `envVars`, with values read from the publisher's own environment, so every
// variable it sets is stored **sensitive** — write-only, never shown back —
// which is what Connect does with all of them.
//
// The credential-collision refusal PATCH /apps/{id} makes is made here too,
// before anything is written, so a bad request changes nothing.
func (s *Server) handleConnectPatchEnvironment(w http.ResponseWriter, r *http.Request) {
	app, ok := s.connectAppForEdit(w, r)
	if !ok {
		return
	}
	// A locked app's variables are frozen like everything else about it, and
	// a change here is audited as the platform's own route audits it
	// (security review 2026-11-10, B-2).
	if s.refuseIfLocked(w, app) {
		return
	}
	var req []struct {
		Name  string  `json:"name"`
		Value *string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		connectError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	stored := map[string]bool{}
	if existing, err := s.db.GetEnvVars(app.ID); err == nil {
		for _, e := range existing {
			stored[e.Key] = true
		}
	}
	for _, v := range req {
		if strings.TrimSpace(v.Name) == "" {
			connectError(w, http.StatusBadRequest, "every variable needs a name")
			return
		}
		// The names the platform's own routes refuse (refuseEnvNames), on a
		// set only and only for a name not already stored.
		if v.Value != nil && !stored[v.Name] {
			if code, msg := runner.EnvNameProblem(v.Name); code != "" {
				connectError(w, http.StatusBadRequest, msg)
				return
			}
		}
		if from, err := s.db.EnvKeyIsFromCredential(app.ID, v.Name); err == nil && from != "" {
			connectError(w, http.StatusConflict, v.Name+" is supplied by the "+from+
				" credential attached to this app. Ask an administrator to change it, or use a different name.")
			return
		}
	}

	// The last word on each name wins, as the loop that applied them in order
	// had it.
	set := map[string]database.EnvSet{}
	var unset []string
	sensitive := true
	for _, v := range req {
		if v.Value == nil {
			delete(set, v.Name)
			unset = append(unset, v.Name)
			continue
		}
		unset = slices.DeleteFunc(unset, func(n string) bool { return n == v.Name })
		set[v.Name] = database.EnvSet{Value: v.Value, Sensitive: &sensitive}
	}
	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())
	actorType, actorID := auditActor(p)
	if _, err := s.db.ApplyEnvChanges(app.ID, set, unset, false, s.encryptor, database.AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action: "app.env_change", TargetType: "app", TargetID: app.ID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	}); err != nil {
		var refused *database.EnvChangeError
		if errors.As(err, &refused) {
			connectError(w, http.StatusBadRequest, refused.Message)
			return
		}
		log.Printf("connect: environment for %s: %v", app.Slug, err)
		connectError(w, http.StatusInternalServerError, "failed to update environment variables")
		return
	}
	s.handleConnectGetEnvironment(w, r)
}

// handleConnectGetTask is the deploy's progress, in the polling shape rsconnect
// reads: the lines after `first`, the index to ask from next, and whether it is
// over. A task is a deployment, authorized as its build log is.
//
// **Where the lines come from.** The build log reaches the row only when the
// build ends, so a deploy started here is followed live (connectTaskLog) and
// its lines are served from memory while it runs. A deployment nobody followed
// — one started elsewhere, or before a restart — is served from the row, and
// reports nothing until it has finished. The two agree on numbering: the
// stored log is exactly the first lines the builder broadcast.
//
// `wait` holds the request open, up to five seconds, until there is something
// new to say, so a client polling in a tight loop costs a request per second
// rather than a hundred.
func (s *Server) handleConnectGetTask(w http.ResponseWriter, r *http.Request) {
	deploy, err := s.db.GetDeployment(chi.URLParam(r, "taskID"))
	if err != nil {
		connectError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if deploy == nil {
		connectError(w, http.StatusNotFound, "task not found")
		return
	}
	app, err := s.db.GetApp(deploy.AppID)
	if err != nil || app == nil {
		connectError(w, http.StatusNotFound, "task not found")
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	if d := auth.Authorize(p, logsAction, obj, s.db); !d.Allow {
		if !auth.CanRead(p, obj, s.db) {
			connectError(w, http.StatusNotFound, "task not found")
			return
		}
		writeAuthzError(w, r, d, true)
		return
	}

	first, _ := strconv.Atoi(r.URL.Query().Get("first"))
	first = max(first, 0)
	waitSec, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	deadline := time.Now().Add(time.Duration(min(max(waitSec, 0), 5)) * time.Second)

	for {
		lines, finished := s.connectTaskState(deploy)
		if finished || len(lines) > first || !time.Now().Before(deadline) {
			writeJSON(w, http.StatusOK, connectTaskBody(deploy, lines, first, finished))
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
		if d, err := s.db.GetDeployment(deploy.ID); err == nil && d != nil {
			deploy = d
		}
	}
}

// connectTaskState is a deployment's lines so far and whether it has ended.
//
// Finished is the row's word, never the stream's, with one wait: a successful
// build writes its row before its last two lines, so a followed task is over
// when its follower has seen the end too — or, if it somehow never does, a few
// seconds after the row says so.
func (s *Server) connectTaskState(deploy *database.Deployment) ([]string, bool) {
	terminal := deploy.Status == database.DeployStatusSuccess || deploy.Status == database.DeployStatusFailed
	stored := splitLogLines(deploy.BuildLog)
	live, done, followed := s.connectTasks.snapshot(deploy.ID)
	if !followed || len(live) < len(stored) {
		return stored, terminal
	}
	if terminal && !done && deploy.Status == database.DeployStatusSuccess &&
		deploy.FinishedAt != nil && time.Since(*deploy.FinishedAt) < 5*time.Second {
		return live, false
	}
	return live, terminal
}

func connectTaskBody(deploy *database.Deployment, lines []string, first int, finished bool) map[string]any {
	first = min(first, len(lines))
	body := map[string]any{
		"id":       deploy.ID,
		"output":   lines[first:],
		"result":   nil,
		"finished": finished,
		"code":     0,
		"error":    "",
		"last":     len(lines),
	}
	if finished && deploy.Status == database.DeployStatusFailed {
		body["code"] = 1
		msg := deploy.FailureMessage
		if msg == "" {
			msg = "the deploy failed — the build log above says where"
		}
		body["error"] = msg
	}
	return body
}

// splitLogLines is a stored build log as the lines the builder wrote.
func splitLogLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}

// connectTaskLog keeps the live lines of the deploys this surface started, so
// a client polling for progress sees the build as it happens rather than all
// at once at the end. Memory only and node-local, like every other follower
// of the builder's stream; a restart loses it and the row takes over.
type connectTaskLog struct {
	mu    sync.Mutex
	tasks map[string]*connectTask
}

type connectTask struct {
	lines []string
	done  bool
}

// connectTaskRetention is how long a finished task's lines are kept after its
// follower stops. rsconnect reads the last of them within a second; the rest
// is slack for a slow client.
const connectTaskRetention = 10 * time.Minute

// follow subscribes to a deployment's log and keeps its lines until it ends.
//
// The builder's stream ends with "DONE" on success and simply stops on
// failure, so the follower also asks the row, every few seconds, whether the
// deploy is over — otherwise a failed build would hold a goroutine and a
// subscription for ever.
func (l *connectTaskLog) follow(s *Server, deployID string) {
	if s.builder == nil {
		return
	}
	l.mu.Lock()
	if l.tasks == nil {
		l.tasks = map[string]*connectTask{}
	}
	backlog, ch := s.builder.SubscribeLogs(deployID)
	t := &connectTask{lines: append([]string{}, backlog...)}
	l.tasks[deployID] = t
	l.mu.Unlock()
	go func() {
		defer func() {
			s.builder.UnsubscribeLogs(deployID, ch)
			l.mu.Lock()
			t.done = true
			l.mu.Unlock()
			time.AfterFunc(connectTaskRetention, func() {
				l.mu.Lock()
				delete(l.tasks, deployID)
				l.mu.Unlock()
			})
		}()
		tick := time.NewTicker(3 * time.Second)
		defer tick.Stop()
		for {
			select {
			case line, ok := <-ch:
				if !ok || line == "DONE" {
					return
				}
				l.mu.Lock()
				t.lines = append(t.lines, line)
				l.mu.Unlock()
			case <-tick.C:
				d, err := s.db.GetDeployment(deployID)
				over := err != nil || d == nil || d.Status == database.DeployStatusFailed ||
					(d.Status == database.DeployStatusSuccess && d.FinishedAt != nil &&
						time.Since(*d.FinishedAt) > 10*time.Second)
				if !over {
					continue
				}
				// What the builder wrote before the row turned is already
				// queued on the channel; take it before letting go.
				for {
					select {
					case line, ok := <-ch:
						if !ok || line == "DONE" {
							return
						}
						l.mu.Lock()
						t.lines = append(t.lines, line)
						l.mu.Unlock()
					default:
						return
					}
				}
			}
		}
	}()
}

// snapshot is a copy of a followed task's lines, whether its follower has
// stopped, and whether it was followed at all.
func (l *connectTaskLog) snapshot(deployID string) ([]string, bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.tasks[deployID]
	if !ok {
		return nil, false, false
	}
	return append([]string(nil), t.lines...), t.done, true
}
