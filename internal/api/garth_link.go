package api

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"toft/internal/auth"
	"toft/internal/database"
)

// garthLink is the "Open in Garth" address for one app, or "" when there is
// none to offer (ADR §158) — no Garth address, or an app no Garth session
// deployed (or one somebody unlinked):
//
//	{garth}/open/toft?toft={origin}&app={slug}&app_id={id}&repo={repo}&subdir={subdir}
//
// Built here rather than in the SPA so the page never holds Garth's address
// for a reader who may not see the link, and so the repository URL's userinfo
// — a token somebody pasted into it — is stripped by the one function that
// knows to. repo and subdir are left out for an app with no repository.
//
// Toft never dials it. The link only navigates, and Garth confirms with the
// person before it does anything.
func garthLink(garth, origin string, app *database.App) string {
	garth = strings.TrimSuffix(strings.TrimSpace(garth), "/")
	if garth == "" || origin == "" || app == nil || app.GarthLinkedAt == nil {
		return ""
	}
	q := url.Values{}
	q.Set("toft", origin)
	q.Set("app", app.Slug)
	// The id is what Garth keys its link on, since a slug can be reused by
	// another app after this one is deleted; the slug stays for display.
	q.Set("app_id", app.ID)
	if repo := repoWithoutUserinfo(app.RepoURL); repo != "" {
		q.Set("repo", repo)
		if app.RepoSubdir != "" {
			q.Set("subdir", app.RepoSubdir)
		}
	}
	// Encode sorts the keys and percent-encodes every value; a space becomes
	// "+", which URLSearchParams on Garth's side reads back as a space.
	return garth + "/open/toft?" + q.Encode()
}

// repoWithoutUserinfo is the repository URL with any user name or password
// removed. A value that does not parse as a URL with a scheme — an scp-style
// git@host:path — has no userinfo to carry in this sense and is passed as it
// stands, its "git@" being the conventional user rather than a credential.
func repoWithoutUserinfo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	u.User = nil
	return u.String()
}

// garthLinkFor is garthLink for the app page: the stored address, and this
// install's origin — scheme and host only, which is what Garth matches its
// list of Toft installs against.
func (s *Server) garthLinkFor(r *http.Request, app *database.App) string {
	settings, err := s.db.GetPlatformSettings()
	if err != nil || settings.GarthURL == "" {
		return ""
	}
	origin := s.installOrigin(r)
	if u, err := url.Parse(origin); err == nil && u.Scheme != "" && u.Host != "" {
		origin = u.Scheme + "://" + u.Host
	}
	return garthLink(settings.GarthURL, origin, app)
}

// garthDeployHeader is what Garth's proxy sets on every request a session's
// toft makes. A deploy that carries it links the app to Garth, which is what
// makes the app page offer Open in Garth. It is a claim, not a credential: the
// caller already holds a key that may deploy this app, and the mark buys
// nothing but a link on that app's page for the people who may edit it.
const garthDeployHeader = "X-Garth-Deploy"

// noteGarthDeploy marks the app linked when the deploy that just started
// came through Garth. Only a key's request counts — Garth always holds one,
// and a browser's request never carries the header — and a failure to write
// the mark is logged, never the deploy's failure.
func (s *Server) noteGarthDeploy(r *http.Request, appID string) {
	if r.Header.Get(garthDeployHeader) == "" {
		return
	}
	if p := auth.GetPrincipal(r.Context()); p == nil || !p.IsAPIKey() {
		return
	}
	if err := s.db.MarkAppGarthLinked(appID); err != nil {
		slog.Warn("marking app deployed from Garth", "app", appID, "error", err)
	}
}

// handleUnlinkGarth is DELETE /apps/{appID}/garth-link: the app page stops
// offering Open in Garth until a Garth session deploys it again. Anyone who
// may edit the app, since they are who the link is shown to. It changes
// nothing on Garth, which Toft never dials; the project there keeps its own
// list, and its owner unlinks it in the project's settings.
func (s *Server) handleUnlinkGarth(w http.ResponseWriter, r *http.Request) {
	app, _, ok := s.appForEdit(w, r)
	if !ok {
		return
	}
	actorType, actorID := auditActor(auth.GetPrincipal(r.Context()))
	meta := requestMeta(r.Context())
	if err := s.db.UnlinkAppFromGarth(app.ID, actorType, actorID, meta.RequestID, meta.IP, meta.UserAgent); errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "app not found",
			"hint": "the app was deleted while it was being unlinked; reload the app list"})
		return
	} else if err != nil {
		slog.Error("unlinking app from Garth", "app", app.ID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
