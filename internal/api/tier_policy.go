package api

import (
	"log"
	"net/http"
	"strconv"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
)

// How many apps this install may have, as the create paths see it.
//
// **Three enforcement points and the list is the specification**: POST /apps,
// POST /apps/upload, and the gallery's deploy route. All three are here rather
// than in `CreateAppTx`, and that is the whole design decision in this file.
//
// Not in the database layer, for three reasons. `CreateApp` is the
// non-transactional sibling every test fixture in this package builds apps
// with, so a cap there would be enforced on half the tree and bypassed by the
// other half — and every test that happened to create an eleventh app would
// fail for a reason that has nothing to do with what it is testing. The
// database layer has no `http.ResponseWriter` to refuse into and no principal
// to audit. And a cap enforced at the write is a cap enforced *after* the
// archive has been fetched and the upload parsed, which is the pattern the two
// existing create-time refusals — `refusedSource` and `uploadsRefused` — were
// written to avoid.
//
// So this sits beside those two, in their shape, and each call site is
// numbered in a comment the way theirs are.

// tierCapCode is the code on the refusal body.
//
// **403 and not 402.** `402 Payment Required` is semantically exact and it falls
// out of `client.Error.Unauthorized()` in `toft`, which would report a commercial
// refusal as exit 1 — a build failure. 403 already means exit 3, "refused", and
// the code plus the sentence is what tells it apart from a permission problem.
const tierCapCode = "tier_limit"

// tier is the resolved tier, and there is one place it is read from.
func (s *Server) tier() entitlements.Tier { return s.entitlements().Tier() }

// refusedTierCap answers the three create paths: it counts the apps this
// install has, and when the tier's cap is reached it writes the 403 and the
// audit row itself and returns true.
//
// **It fails open.** A count that cannot be read is not a cap that should be
// enforced — the version cap beside it makes the same choice for the same
// reason, and the safe direction costs nothing here because creating an app row
// is not going to work either if the database is unavailable. A security
// control would fail closed; this is bookkeeping and friction.
func (s *Server) refusedTierCap(w http.ResponseWriter, r *http.Request) bool {
	tier := s.tier()
	limit, capped := entitlements.AppLimit(tier)
	if !capped {
		return false
	}

	// A deleted app does not count; a locked, retired or stopped one does,
	// because it is a row.
	count, err := s.db.CountApps()
	if err != nil {
		log.Printf("tier: counting apps, allowing the create: %v", err)
		return false
	}
	if count < limit {
		return false
	}

	s.auditTierRefusal(r, tier, limit, count)
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": tierCapSentence(tier, limit, count),
		"code":  tierCapCode, "hint": errorHints[tierCapCode],
	})
	return true
}

// tierCapSentence is a **commercial artefact, not an error string.**
//
// The eleventh app is the first time this product says no for a reason that is
// not about permission, and copy written like an error reads as a trap rather
// than as a tier. So it says what the limit is, what raises it, and — because
// this is the sentence somebody will screenshot — that nothing they already
// have has been touched.
func tierCapSentence(tier entitlements.Tier, limit, count int) string {
	s := "This install is on " + tier.Describe() + ", which runs up to " +
		strconv.Itoa(limit) + " apps, and it has " + strconv.Itoa(count) + ". "
	switch tier {
	case entitlements.TierDemo:
		free, _ := entitlements.AppLimit(entitlements.TierFree)
		s += "A free license key raises that to " + strconv.Itoa(free) +
			" and costs nothing; a paid license removes the limit. "
	default:
		s += "A paid license removes the limit. "
	}
	return s + "Nothing has been switched off: every app already here keeps serving, and " +
		"deploying a new release of one is unaffected."
}

// auditTierRefusal records who hit the fence, on what tier, at what count.
//
// One write on a request path that is already refusing, so it costs nothing
// anybody is waiting on — and it is exactly the record that answers *is anybody
// hitting this fence*, which is the number that decides whether the cap is set
// right. `audit_log` rather than a table of its own, unlike the source policy's
// refusals: a source refusal is a *customer's* configuration being tested, and
// a cap refusal is the tier's.
func (s *Server) auditTierRefusal(r *http.Request, tier entitlements.Tier, limit, count int) {
	p := auth.GetPrincipal(r.Context())
	actorType, actorID := database.PrincipalTypeUser, ""
	if p != nil {
		actorType, actorID = auditActor(p)
	}
	meta := requestMeta(r.Context())
	if err := s.db.WriteAuditRow(database.AuditEntry{
		ActorType:  actorType,
		ActorID:    actorID,
		Action:     "app.create_refused_tier",
		TargetType: "install",
		After: `{"tier":"` + string(tier) + `","limit":` + strconv.Itoa(limit) +
			`,"apps":` + strconv.Itoa(count) + `}`,
		RequestID: meta.RequestID,
		IP:        meta.IP,
		UserAgent: meta.UserAgent,
		APIKeyID:  meta.APIKeyID,
	}); err != nil {
		log.Printf("tier: auditing a refused create: %v", err)
	}
}

// The license gate on a configuration surface, and the list of what it covers.
//
// **Seven features and the list is `entitlements.Known`; the enforcement points
// are the write verbs below and that list is this table.** A gated surface is a
// catalog entry rather than a second helper beside one, because two places that
// answer "may this install use X" is what `internal/entitlements` exists to
// prevent — and because a feature with no catalog entry cannot be rendered as
// locked, which D9 requires that it can.
//
// # Configuration, never operation
//
// What a license decides is whether **new configuration may be written**. It
// never decides whether configuration already written is honoured: an OIDC
// provider configured under a trial keeps signing people in when the trial
// ends, a mounted volume stays mounted, an attached Slack channel keeps
// receiving. That is `docs/invariants.md` § The license's *a license never
// takes down running code* carried forward without amendment, and it is why the
// gate is on the write verbs only.
//
// **Removal is never gated.** An administrator must always be able to take
// something away — deleting a provider, unmapping a volume, detaching a
// channel, removing a member. A license that trapped configuration in place
// would be the one shape of this feature that could make an install worse to
// own than no license at all.
//
// **Reads are never gated**, so the page renders: it shows what is there,
// labelled locked, with one line saying what a license would unlock. A feature
// nobody can see sells nothing.
//
// Body is the configuring shape, for the routes where the same verb also
// carries a removal: telemetry and the source policy decode first and gate
// only the configuring branches, so the guard that walks this table has to
// send a body the gate applies to. Empty means `{}` is enough.
var licensedWrites = []struct {
	Method, Route, Feature string
	Body                   string
}{
	{Method: "POST", Route: "/api/v1/admin/auth-providers", Feature: entitlements.FeatureIdentityProviders},
	{Method: "PATCH", Route: "/api/v1/admin/auth-providers/{providerID}", Feature: entitlements.FeatureIdentityProviders},
	{Method: "POST", Route: "/api/v1/admin/auth-providers/discover", Feature: entitlements.FeatureIdentityProviders},

	{Method: "POST", Route: "/api/v1/groups", Feature: entitlements.FeatureGroups},
	{Method: "PATCH", Route: "/api/v1/groups/{id}", Feature: entitlements.FeatureGroups},
	{Method: "PUT", Route: "/api/v1/groups/{id}/members/{userId}", Feature: entitlements.FeatureGroups},

	{Method: "POST", Route: "/api/v1/admin/storage", Feature: entitlements.FeatureAppStorage},
	{Method: "PUT", Route: "/api/v1/admin/storage/{id}/mappings/{appID}", Feature: entitlements.FeatureAppStorage},
	{Method: "PUT", Route: "/api/v1/admin/storage/{id}/managers", Feature: entitlements.FeatureAppStorage},
	{Method: "PUT", Route: "/api/v1/admin/storage/{id}/grants", Feature: entitlements.FeatureAppStorage},

	{Method: "POST", Route: "/api/v1/admin/integrations/connections", Feature: entitlements.FeatureIntegrations},
	{Method: "PUT", Route: "/api/v1/apps/{appID}/integrations/{kind}", Feature: entitlements.FeatureIntegrations},

	// The test route is deliberately absent: it writes nothing, and a license
	// gates configuration, never operation.
	{Method: "PUT", Route: "/api/v1/admin/telemetry-settings", Feature: entitlements.FeatureTelemetry,
		Body: `{"enabled":true,"endpoint":"http://collector.example:4318","traces":true}`},

	{Method: "PUT", Route: "/api/v1/system/source-policy", Feature: entitlements.FeatureTrustedSources,
		Body: `{"mode":"allowlist","patterns":[{"pattern":"github.com/acme/*"}]}`},
}

// unlicensedRemovals are the shapes on the gated routes that must pass on an
// unlicensed install, because they take configuration away rather than adding
// it. `TestRemovalIsNeverGated` walks them beside the table above; a shape
// that is missing here is one a future gate could trap.
var unlicensedRemovals = []struct {
	Method, Route, Body string
}{
	{"PUT", "/api/v1/admin/telemetry-settings", `{"use_environment":true}`},
	{"PUT", "/api/v1/admin/telemetry-settings", `{"enabled":false}`},
	{"POST", "/api/v1/admin/telemetry-settings/test", `{}`},
	{"PUT", "/api/v1/system/source-policy", `{"mode":"none"}`},
	{"PUT", "/api/v1/system/source-policy", `{"mode":"any"}`},
	{"PUT", "/api/v1/system/source-policy", `{"uploads_enabled":false}`},
	{"PUT", "/api/v1/system/source-policy", `{"patterns":[]}`},
}

// refusedUnlicensed is the gate. It writes the 403 and returns true.
//
// `not_licensed` rather than `tier_limit`: the two are different refusals and
// only one of them is `toft`'s business. `not_licensed` is already the code gate 1
// answers with on `handleUpdateFeature`, and a surface that is locked is locked
// for the same reason there.
//
// The status is 403 and not 404. These routes exist, an administrator can see
// the page they belong to, and 404 is what *absence* looks like — that is the
// distinction D9 writes down, and collapsing it here would make a paywalled
// surface indistinguishable from an unconfigured one on the one screen where
// the difference is the whole message.
func (s *Server) refusedUnlicensed(w http.ResponseWriter, r *http.Request, feature string) bool {
	lic := s.entitlements().Entitled(feature)
	if lic.Entitled {
		return false
	}
	d := entitlements.Describe(feature)
	title := feature
	if d != nil {
		title = d.Title
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "This install is not licensed for " + title + ". " + lic.Reason +
			" Everything already configured here keeps working; what a license unlocks is " +
			"changing it.",
		"code": "not_licensed",
	})
	return true
}

// refusedPublicApps answers, and refuses, the one transition an install-wide
// switch closes: an app becoming reachable without signing in (ADR §70).
//
// It lives beside refusedUnlicensed because the two guard the same line of the
// same handler and a reader comparing them should not have to go looking. They
// are not the same kind of refusal, though, and the copy says so: a license
// refusal names a tier and can be lifted by buying something, while this one is
// this install's own administrator saying no, and the only honest next step is
// to go and ask them.
//
// 403 with a code, matching the gate beside it: the SPA already handles one
// shape for "you may not publish this", and to the owner reading it the two
// reasons differ only in the sentence.
func (s *Server) refusedPublicApps(w http.ResponseWriter) bool {
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		// Fail closed. A settings read that will not answer is not evidence
		// that anonymous access is allowed, and the cost of being wrong in
		// this direction is a refused edit rather than an app on the internet
		// nobody meant to publish.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return true
	}
	if settings.PublicAppsEnabled {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "This install does not publish apps to people who are not signed in. " +
			"An administrator turned that off for every app here; Anyone signed in is " +
			"the widest this app can be shared without one.",
		"code": "public_apps_disabled",
	})
	return true
}
