package api

import (
	"log"
	"net/http"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/sources"
)

// Which repositories this install will deploy from, as the API sees it.
//
// **Seven points enforce it and the list is the specification**: POST /apps,
// the re-point in PATCH /apps/{id}, POST /apps/{id}/deploy, the builder
// immediately before the clone, the auto-deploy poller before ls-remote, and
// the gallery's deploy and update routes. Three of those are in
// app_handlers.go, two in gallery_handlers.go, and two outside this package.
// An eighth path that becomes a running app without consulting the resolver is
// a bug of the same class as a handler that forgets to authorize — which is why
// the builder checks again regardless, as the last place where the decision is
// still cheap.
//
// **The upload switch is a different control and is not on that list.**
// `uploadsRefused` answers `source_uploads_enabled` on the two upload routes, and
// `refusedSource` short-circuits out of an uploaded source entirely
// (`IsUploadedSource`): an app with no remote is never non-compliant on a git
// policy. Counting the upload routes as source-policy points — which this comment
// and `docs/invariants.md` both once did — makes the list wrong in both
// directions at once, since it named two paths the policy never reaches and
// omitted the re-point and the manual deploy, which it does.
//
// Nothing in this file decides anything itself: it reads
// database.ResolveSourcePolicy and asks sources.Policy, and the sentence it
// shows is the one that package phrases.

// sourcePolicy reads the install's policy, **failing closed**.
//
// The version cap beside it fails open, deliberately, and the two are worth
// telling apart: a cap the platform cannot read is not a cap it should enforce,
// while a security control the platform cannot read is not one it should
// ignore. In practice the safe direction costs little — a settings read that
// fails means the database is unavailable, and creating an app row is not going
// to work either.
func (s *Server) sourcePolicy() (database.SourcePolicy, error) {
	return s.db.ResolveSourcePolicy()
}

// refusedSource answers the deploy and create paths: it checks one repository
// URL against the policy, and when the answer is no it writes the 403 and the
// audit row itself and returns true.
//
// **403, not 400.** The URL is perfectly well-formed and the install simply
// refuses it; 400 would say "you typed it wrong" and invite a client to retry
// with a "corrected" URL. The body carries the refusal in full — what was
// refused, and what would work — because a refusal a person can act on says
// why, and one that does not sends the developer to guess or to Slack.
//
// appID is empty on the create path, where no app exists yet.
func (s *Server) refusedSource(w http.ResponseWriter, r *http.Request, repoURL, appID string) bool {
	if builder.IsUploadedSource(repoURL) {
		// An uploaded app has no remote. It is governed by the upload switch, at
		// creation, and is never non-compliant on the git policy.
		return false
	}

	policy, err := s.sourcePolicy()
	if err != nil {
		log.Printf("sources: reading the source policy, refusing the deploy: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return true
	}
	refusal := policy.Allows(repoURL)
	if refusal == nil {
		return false
	}

	s.auditSourceRefusal(r, repoURL, refusal.Error(), appID)
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": refusal.Error(),
		"code":  "source_not_allowed",
	})
	return true
}

// auditSourceRefusal records who tried to deploy what, from where.
//
// One write on a request path that is already refusing, so it costs nothing
// anyone is waiting on — and it is exactly the record that makes "is anybody
// hitting this fence" answerable. That number, and the ratio of refusals to
// uploads immediately afterwards, is what decides whether leaving out an
// exception-request workflow was right.
func (s *Server) auditSourceRefusal(r *http.Request, repoURL, reason, appID string) {
	p := auth.GetPrincipal(r.Context())
	actorID := ""
	if p != nil {
		actorID = p.ID
	}
	meta := requestMeta(r.Context())
	if err := s.db.RecordSourceRefusal(actorID, repoURL, reason, appID,
		meta.RequestID, meta.IP, meta.UserAgent, meta.APIKeyID); err != nil {
		log.Printf("sources: auditing a refused source: %v", err)
	}
}

// uploadsRefused answers the upload paths. Same fail-closed reading, and the same
// 403: uploads being off is a decision, not a malformed request.
func (s *Server) uploadsRefused(w http.ResponseWriter) bool {
	policy, err := s.sourcePolicy()
	if err != nil {
		log.Printf("sources: reading the source policy, refusing the upload: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return true
	}
	if policy.UploadsEnabled {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "Administrators have switched off deploying apps from an uploaded archive on this install.",
		"code":  "uploads_not_allowed",
	})
	return true
}

// appSourcePolicy is one app's own view of the rule: may it deploy from where
// it deploys from, and if not, why not.
//
// It rides on GET /apps/{id} because a platform decision about one app has to
// be visible on that app (ADR §20). Without it an owner meets the policy only
// as a deploy that mysteriously refuses, and /system/settings — which is
// admin-only and 404s to everyone else — is no help to them.
//
// Degrades to "allowed" on a read failure, like everything else in the policy
// block: this is explanatory material on a page with plenty else to show, and
// the deploy paths do their own checking. A page that wrongly said "blocked"
// would be worse than one that said nothing.
func (s *Server) appSourcePolicy(app *database.App) (allowed bool, reason string) {
	if builder.IsUploadedSource(app.RepoURL) {
		return true, ""
	}
	policy, err := s.sourcePolicy()
	if err != nil {
		log.Printf("sources: reading the source policy for %s's policy block: %v", app.Slug, err)
		return true, ""
	}
	if refusal := policy.Allows(app.RepoURL); refusal != nil {
		return false, refusal.Error()
	}
	return true, ""
}

// effectiveSourcePolicy is what a non-administrator is allowed to know: the
// mode, the patterns, and whether uploads are on.
//
// Disclosing internal hostnames to signed-in staff on an internal platform is
// mild, and the alternative fails the standing rule that a refusal a person can
// act on says why: a New App form that refuses without naming what *would* work
// sends every developer to the same Slack channel.
type effectiveSourcePolicy struct {
	Mode           string   `json:"mode"`
	Patterns       []string `json:"patterns"`
	UploadsEnabled bool     `json:"uploads_enabled"`
	// GitAllowed is false under mode "none" — the New App form uses it to offer
	// upload only, rather than rendering a field that cannot succeed.
	GitAllowed bool `json:"git_allowed"`
}

func newEffectiveSourcePolicy(p database.SourcePolicy) effectiveSourcePolicy {
	return effectiveSourcePolicy{
		Mode:           p.Mode,
		Patterns:       p.PatternStrings(),
		UploadsEnabled: p.UploadsEnabled,
		GitAllowed:     p.Mode != sources.ModeNone,
	}
}
