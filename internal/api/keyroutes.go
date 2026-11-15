package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// The route policy for personal API keys.
//
// It is an **allow-list, and it fails closed**: a route is unreachable by key
// until someone names it here. The alternative — deny the dangerous ones,
// allow the rest — is fine on the day it is written and wrong the first time
// somebody adds an endpoint without thinking about tokens. This way the
// mistake is a broken automation, which someone reports; the other way the
// mistake is a token that can do something nobody intended, which nobody
// reports.
//
// TestEveryRouteIsClassified walks the router and fails on any /api/v1 route
// missing from both maps, so neither list can quietly drift from the router.
//
// The three exempt subtrees are /api/v1/auth (only /auth/me is mounted behind
// apiKeyAuth at all — everywhere else a presented key is simply ignored and
// the caller is anonymous), /api/v1/runner (run-scoped tokens, a different
// audience entirely), and /api/v1/app (an app's own outbound token — the
// opposite direction from a key, and a surface no key may reach).
//
// The first of those is what makes `toft login` safe. Its four routes sit under
// /api/v1/auth precisely so that a presented key is ignored there: the mint is
// authorized by a browser session and by nothing else, so a stolen toftu_ cannot
// mint its replacement. That is this file's central rule holding by
// construction rather than by an entry below, and
// TestAKeyCannotApproveItsOwnLogin states it as behaviour.
//
// What a key may do: read anything its owner can read, and operate their apps
// — deploy, roll back, restart, stop, trigger jobs, manage tags
// and storage. What it may never do is manage a credential or change who may
// do what. A stolen CI token that could mint fresh tokens or promote its owner
// would turn a bounded incident into an unbounded one.
var apiKeyAllowedRoutes = map[string]bool{
	// Apps — the whole point of automating against this platform.
	"GET /api/v1/apps":         true,
	"POST /api/v1/apps":        true,
	"POST /api/v1/apps/upload": true,
	"POST /api/v1/apps/check":  true,
	"GET /api/v1/apps/{appID}": true,
	// Slug availability, which the New App form asks before create. Moved
	// under the guard on 2026-11-11 (anonymous was an existence oracle) and
	// classified here the same day it was found refusing every key (A-5).
	"GET /api/v1/slugs/{slug}/available": true,
	// The same view by the name a person types, for the client that holds a
	// slug and nothing else. Beside the by-id read because it is the by-id
	// read: same handler body, same readable-apps scoping, same 404.
	"GET /api/v1/apps/by-slug/{slug}":                           true,
	"PATCH /api/v1/apps/{appID}":                                true,
	"PATCH /api/v1/apps/{appID}/env":                            true,
	"DELETE /api/v1/apps/{appID}":                               true,
	"POST /api/v1/apps/{appID}/deploy":                          true,
	"POST /api/v1/apps/{appID}/upload":                          true,
	"POST /api/v1/apps/{appID}/check":                           true, // the upload's decisions, nothing recorded
	"POST /api/v1/apps/{appID}/deployments/{deployID}/rollback": true,
	"POST /api/v1/apps/{appID}/restart":                         true,
	"POST /api/v1/apps/{appID}/stop":                            true,
	// Wake a sleeping app ahead of a request the caller is about to make:
	// what a visit would do anyway, at the read rule (ADR §123).
	"POST /api/v1/apps/{appID}/wake": true,

	// The Posit Connect publishing surface (ADR §109) — rsconnect holding a
	// personal key. Each is a translation of a route above and allowed for its
	// reason: create, upload, deploy and poll are what `toft deploy` does, and
	// the environment is the PATCH above narrowed to variables. Not on either
	// deploy-key list: a pipeline has `toft`, and a door that class was never
	// classified for stays shut.
	"GET /api/v1/connect/server_settings":                  true,
	"GET /api/v1/connect/users/current":                    true,
	"GET /api/v1/connect/v1/user":                          true,
	"GET /api/v1/connect/applications":                     true,
	"GET /api/v1/connect/applications/{appID}":             true,
	"POST /api/v1/connect/v1/content":                      true,
	"GET /api/v1/connect/v1/content/{appID}":               true,
	"POST /api/v1/connect/v1/content/{appID}/bundles":      true,
	"POST /api/v1/connect/v1/content/{appID}/deploy":       true,
	"GET /api/v1/connect/v1/content/{appID}/environment":   true,
	"PATCH /api/v1/connect/v1/content/{appID}/environment": true,
	"GET /api/v1/connect/v1/tasks/{taskID}":                true,

	// Reading an app's history and behaviour.
	"GET /api/v1/apps/{appID}/usage":                     true,
	"GET /api/v1/apps/{appID}/usage/viewers":             true,
	"GET /api/v1/apps/{appID}/consumption":               true,
	"GET /api/v1/apps/{appID}/events":                    true,
	"GET /api/v1/apps/{appID}/logs":                      true,
	"GET /api/v1/apps/{appID}/logs/stream":               true,
	"GET /api/v1/apps/{appID}/deployments":               true,
	"DELETE /api/v1/apps/{appID}/deployments":            true,
	"DELETE /api/v1/apps/{appID}/deployments/{deployID}": true,
	// One build's state, and its progress through the three phases. Both are
	// reads, and both exist for an automation rather than for the SPA: a CLI
	// that runs `deploy` and waits for it holds a deployment ID and nothing
	// else.
	"GET /api/v1/apps/{appID}/deployments/{deployID}":          true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}/progress": true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}/logs":     true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}/archive":  true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}/stream":   true,

	// Jobs — the other half of "automation outside Toft".
	"GET /api/v1/apps/{appID}/jobs":                     true,
	"PATCH /api/v1/apps/{appID}/jobs/{jobID}":           true,
	"GET /api/v1/apps/{appID}/job-runs":                 true,
	"POST /api/v1/apps/{appID}/job-runs":                true,
	"GET /api/v1/apps/{appID}/job-runs/{runID}":         true,
	"POST /api/v1/apps/{appID}/job-runs/{runID}/cancel": true,
	"GET /api/v1/apps/{appID}/job-runs/{runID}/logs":    true,
	"POST /api/v1/apps/{appID}/jobs/{job}/runs":         true,
	"POST /api/v1/apps/{appID}/job-runs/{runID}/rerun":  true,
	"GET /api/v1/apps/{appID}/job-runs/{runID}/output":  true,
	// A notebook's re-render schedule. Allowed for the same reason jobs
	// above are: it is unattended work on an app this key's owner can already
	// edit, and what it produces is a deploy they could have asked for by hand.
	// The schedule is the file's; a key reads it, pauses it and keeps its
	// recipients, as it pauses a job.
	"GET /api/v1/apps/{appID}/render-schedule":            true,
	"GET /api/v1/apps/{appID}/render-schedule/deliveries": true,
	"PATCH /api/v1/apps/{appID}/render-schedule":          true,
	"PUT /api/v1/apps/{appID}/render-schedule/recipients": true,
	"GET /api/v1/apps/{appID}/restart-schedule":           true,
	"PATCH /api/v1/apps/{appID}/restart-schedule":         true,
	// Not writes: both answer 405 naming [render], and a key's old client
	// should read that rather than a 403 that hides it.
	"PUT /api/v1/apps/{appID}/render-schedule":    true,
	"DELETE /api/v1/apps/{appID}/render-schedule": true,

	"PUT /api/v1/apps/{appID}/tags":    true,
	"GET /api/v1/apps/{appID}/storage": true,
	// Attaching a resource this key's owner already manages to an app they can
	// already edit. It is the mapping verb wearing another URL, and it decides
	// nothing: the delegation was an administrator's decision, the ceiling is
	// theirs, and the key reaches exactly what its owner reaches. Beside the
	// admin mapping routes below, which are allowed for the same reason.
	"PUT /api/v1/apps/{appID}/storage/{resourceID}":    true,
	"DELETE /api/v1/apps/{appID}/storage/{resourceID}": true,
	// Which resources this key's owner may attach. A self-scoped read of a
	// decision somebody else took, like /me/access beside it.
	"GET /api/v1/me/storage": true,

	// Dependency policy: reading what an app installed and asking the platform
	// to re-derive its findings. Both are reads in effect — a re-check touches
	// no registry, runs no resolver, accepts no risk and changes no setting —
	// so an automation may have them. Granting a waiver may not; see below.
	"GET /api/v1/apps/{appID}/dependencies": true,
	// One release's SBOM: the same inventory as a file a supply-chain tool
	// reads.
	"GET /api/v1/apps/{appID}/deployments/{deployID}/sbom": true,
	// What one app holds, for its owner's own automation. A key reads what its
	// owner can read, and appForEdit has already bounded that to an editor.
	// The estate-wide version is refused by name in apiKeyDeniedRoutes, which
	// is what TestEveryRouteIsClassified forces: a route in neither map is
	// refused because nobody looked at it, and that is the state this pair of
	// lists exists to make impossible.
	"GET /api/v1/apps/{appID}/reach": true,
	// The app's audit file, for an owner's script that files the export each
	// quarter. Every section's own route is on this list, so a key reads
	// nothing through the pack it could not read route by route. **Not on
	// either deploy-key list, deliberately**: a deploy key is owner on every
	// app, and the pack per app would rebuild the estate reach read — refused
	// to every key — one request at a time.
	"GET /api/v1/apps/{appID}/audit-file":          true,
	"GET /api/v1/apps/{appID}/audit-file/export":   true,
	"POST /api/v1/apps/{appID}/dependency-recheck": true,
	"GET /api/v1/admin/dependencies":               true,
	"GET /api/v1/admin/dependencies/search":        true,
	// The governance read, and the reason this feature grew no CLI verb: a
	// CMDB asking "which of our apps embed this SDK" is exactly the caller the
	// HTTP API is for. Still administrator-only in the handler, so the key has
	// to belong to somebody who already is one.
	"GET /api/v1/admin/packages": true,
	// One app's scan history, on the same terms and for the same caller: the
	// script that asks "how long has this app been carrying this" is the CMDB
	// again. Administrator-only in the handler, so the key still has to belong
	// to one.
	"GET /api/v1/admin/apps/{appID}/dependency-scans": true,
	// The language census, for the same caller and on the same terms: a CMDB
	// asking what the estate is written in is exactly what the HTTP API is
	// for, and the handler is administrator-only regardless, so the key has to
	// belong to somebody who already is one.
	"GET /api/v1/admin/estate/languages": true,
	// The same estate as scan targets, for the same caller. The one route
	// under /admin a deploy key also reaches — see deployKeyReadRoutes and
	// canReadEstateTargets; here it is an administrator's key like the two
	// reads above it.
	"GET /api/v1/admin/estate/targets": true,
	// The route inventory as an OpenAPI document, on the same terms as the two
	// reads above and for the most literal version of the reason: the caller is
	// a scanner holding a key, and the whole feature is that it does not need a
	// checkout to find out what to scan. Administrator-only in the handler, so
	// the key still has to belong to one.
	"GET /api/v1/admin/openapi.json":          true,
	"POST /api/v1/admin/dependencies/preview": true,
	"POST /api/v1/admin/dependency-recheck":   true,
	"POST /api/v1/admin/dependency-feed/sync": true,

	// The app's own JSON document, from the outside. The read is metadata only
	// — size, version, when it changed — and the delete is the recovery lever
	// for an app wedged on state its own code cannot read. Both are operating
	// your app, which is what a key is for; neither touches a credential or
	// changes who may do what, and neither can reach the document's contents,
	// because no route serves them.
	"GET /api/v1/apps/{appID}/data":    true,
	"DELETE /api/v1/apps/{appID}/data": true,

	// The gallery, which is app operation: browsing a catalog, deploying from
	// it, and detaching an app from it are the same class of thing as deploying
	// a repository, and each is authorized live against the owner's real role —
	// so a key belonging to a demoted user stops being able to deploy an
	// extension on the next call.
	"GET /api/v1/gallery":                      true,
	"POST /api/v1/gallery/{itemID}/deploy":     true,
	"DELETE /api/v1/apps/{appID}/gallery":      true,
	"POST /api/v1/apps/{appID}/gallery/update": true,

	// Reads. None of these change anything, and all of them are already
	// scoped to what the caller may see.
	// What this install will deploy from, as a developer's automation needs to
	// know it: the same narrow view a person gets, so a script that is about to
	// create an app can say why it will be refused. Nothing here names who set
	// the policy or what it covers.
	"GET /api/v1/system/source-policy/effective": true,

	// Toft's own manuals — which ones are installed here, and on what
	// slug. The apps themselves are static sites on the apps origin and are
	// read there like any other app; this only says where they are.
	"GET /api/v1/documentation": true,

	"GET /api/v1/system/health":       true,
	"GET /api/v1/system/consumption":  true,
	"GET /api/v1/system/active-users": true,
	"GET /api/v1/system/presence":     true,
	"GET /api/v1/system/processes":    true,
	"GET /api/v1/system/agent":        true,
	"GET /api/v1/system/queue":        true,
	"GET /api/v1/system/job-runs":     true,
	"GET /api/v1/system/usage":        true,
	// The estate's audience, the same read-only reporting as the row above,
	// and the handler demands system_admin the same way.
	"GET /api/v1/system/audience": true,
	// Read-only estate reporting, like the row above it, and the handler still
	// demands system_admin. Worth allowing rather than refusing by default: the
	// obvious consumer is the script that posts "nine apps untouched since
	// April" into a channel once a month, which is the whole point of knowing.
	"GET /api/v1/system/quiet-apps": true,
	"GET /api/v1/system/scheduled":  true,
	// The same estate, as firings by day (ADR §156): the script that posts
	// "what runs tonight" into the on-call channel.
	"GET /api/v1/system/scheduled/upcoming": true,

	"GET /api/v1/system/checks":     true,
	"GET /api/v1/system/settings":   true,
	"GET /api/v1/directory/users":   true,
	"GET /api/v1/directory/groups":  true,
	"GET /api/v1/users":             true,
	"GET /api/v1/users/{id}":        true,
	"GET /api/v1/users/{id}/access": true,
	// Which apps a person has been opening — an administrator's read, like
	// the audit feed and active users beside it, so a key gets it too.
	"GET /api/v1/users/{id}/app-usage":     true,
	"GET /api/v1/groups":                   true,
	"GET /api/v1/groups/{id}":              true,
	"GET /api/v1/tags":                     true,
	"GET /api/v1/tags/{tagSlug}/values":    true,
	"GET /api/v1/announcement":             true,
	"GET /api/v1/knowledge-base":           true,
	"GET /api/v1/admin/audit":              true,
	"GET /api/v1/apps/{appID}/permissions": true,
	"GET /api/v1/me/capabilities":          true,
	"GET /api/v1/apps/{appID}/grants":      true,

	// The account's own record. Facts about the identity the key acts as —
	// the same class as GET /auth/me, which a key may already read. Neither
	// takes an id, so neither can be pointed at anybody else.
	"GET /api/v1/me/account": true,
	// What that identity can reach. A script checking its own reach before
	// acting on it is the intended use, and it can already discover the same
	// apps one at a time through GET /apps.
	"GET /api/v1/me/access": true,

	// Asking for access is not granting it, and withdrawing your own request
	// takes nothing from anyone. The same sentence covers both ladders: the
	// object half asks for a grant on one app, the promotion half asks for a
	// system role, and both are decided by a human who is not the key. What
	// the denied map keeps is the deciding, never the asking.
	"POST /api/v1/apps/{appID}/access-requests":           true,
	"GET /api/v1/apps/{appID}/access-requests":            true,
	"DELETE /api/v1/apps/{appID}/access-requests/{reqID}": true,
	"GET /api/v1/me/access-requests":                      true,
	"POST /api/v1/me/promotion-requests":                  true,
	"GET /api/v1/me/promotion-requests":                   true,
	"DELETE /api/v1/me/promotion-requests/{reqID}":        true,

	// Content governance an administrator may reasonably script: naming tags,
	// provisioning durable storage, publishing a banner. None of it decides
	// who may do what.
	"POST /api/v1/admin/tags":                            true,
	"PATCH /api/v1/admin/tags/{tagID}":                   true,
	"DELETE /api/v1/admin/tags/{tagID}":                  true,
	"GET /api/v1/admin/storage":                          true,
	"POST /api/v1/admin/storage":                         true,
	"PATCH /api/v1/admin/storage/{id}":                   true,
	"PATCH /api/v1/admin/storage/drives/{key}":           true,
	"POST /api/v1/admin/storage/drives/{key}/repoint":    true,
	"DELETE /api/v1/admin/storage/{id}":                  true,
	"DELETE /api/v1/admin/storage/deleting/{slug}":       true,
	"GET /api/v1/admin/storage/{id}/mappings":            true,
	"PUT /api/v1/admin/storage/{id}/mappings/{appID}":    true,
	"DELETE /api/v1/admin/storage/{id}/mappings/{appID}": true,

	// Uploading to a store the key's owner already holds a grant on. This is
	// the intended use of the whole feature — an unattended nightly pushing a
	// file — and it decides nothing: the grant was decided by an administrator,
	// and the key reaches exactly what its owner reaches and no more.
	"GET /api/v1/store":                     true,
	"GET /api/v1/store/{slug}/objects":      true,
	"POST /api/v1/store/{slug}/objects":     true,
	"GET /api/v1/store/{slug}/objects/*":    true,
	"DELETE /api/v1/store/{slug}/objects/*": true,
	"POST /api/v1/store/{slug}/folders":     true,
	"DELETE /api/v1/store/{slug}/folders/*": true,
	"PUT /api/v1/admin/announcement":        true,
	"DELETE /api/v1/admin/announcement":     true,

	// Pointing the whole install at a knowledge base is the same kind of act as
	// publishing a banner: content governance an administrator may reasonably
	// script, deciding nothing about who may do what. The pointer grants no
	// access — a colleague who cannot read the app still cannot read it.
	"GET /api/v1/admin/knowledge-base":    true,
	"PUT /api/v1/admin/knowledge-base":    true,
	"DELETE /api/v1/admin/knowledge-base": true,

	// The SDK this install carries, for `toft sdk vendor`. A read of an artifact
	// that is already inside the binary, the runner image and every Python and
	// Node release on the install — it decides nothing and discloses nothing
	// the key's owner could not read from an app they own.
	"GET /api/v1/sdk/{lang}": true,
}

// keyAnsweredOutsideGuard is every /api/v1 route mounted outside the guarded
// group that a request carrying a personal key is still answered on: /auth/me,
// mounted behind apiKeyAuth so a script can check its token, and the routes
// that answer anyone, where a presented key is ignored and the caller is
// anonymous. It is what x-toft-key-denied reads (keyDenied), and
// TestEveryRouteOutsideTheGuardIsClassified holds it to the router — the
// session-only sign-in steps, the app and runner subtrees and the git webhook
// refuse a key, and are named there instead.
var keyAnsweredOutsideGuard = map[string]bool{
	"GET /api/v1/auth/me":                true,
	"GET /api/v1/auth/providers":         true,
	"DELETE /api/v1/auth/session":        true,
	"POST /api/v1/auth/local/login":      true,
	"POST /api/v1/auth/local/password":   true,
	"POST /api/v1/auth/cli/device":       true,
	"POST /api/v1/auth/cli/device/token": true,
	// The identity token in the body is the credential; a presented key is
	// ignored, so it cannot buy its successor (ADR §157).
	"POST /api/v1/auth/token-exchange":        true,
	"POST /api/v1/auth/token-exchange/revoke": true,
	"GET /api/v1/auth/{provider}/login":       true,
	"GET /api/v1/auth/{provider}/callback":    true,
	"GET /api/v1/config":                      true,
}

// apiKeyDeniedRoutes exists to make the refusal deliberate rather than
// accidental. A route here is refused because someone decided it should be;
// a route in neither map is refused because nobody has looked at it yet, and
// the drift test turns that second case into a build failure.
var apiKeyDeniedRoutes = map[string]bool{
	// rsconnect's token registration starts a sign-in — an `toft login` approval
	// under another name — and no credential may start a credential. Anonymous
	// is the only caller it is for (ADR §102).
	"POST /api/v1/connect/tokens": true,
	// rsconnect's identity federation mints an exchanged key (ADR §157), so
	// it is a credential route; the identity token in its body is the whole
	// authorization, and a presented key must not be able to buy another.
	"POST /api/v1/connect/v1/oauth/integrations/credentials": true,

	// Unlinking an app from Garth is a person's act on the app page; Garth's
	// own key only deploys, and the mark it leaves is set by the deploy routes
	// (ADR §158).
	"DELETE /api/v1/apps/{appID}/garth-link": true,

	// Who may vouch for a person, and the keys their word bought: changing
	// who may do what, which is a session's (ADR §157).
	"GET /api/v1/admin/trusted-issuers":               true,
	"POST /api/v1/admin/trusted-issuers":              true,
	"PATCH /api/v1/admin/trusted-issuers/{issuerID}":  true,
	"DELETE /api/v1/admin/trusted-issuers/{issuerID}": true,
	"GET /api/v1/admin/exchanged-keys":                true,
	"DELETE /api/v1/admin/exchanged-keys/{keyID}":     true,

	// What this install tells an assessor: the control mapping and the live
	// audit report.
	//
	// Denied to every key class, and this is **stricter than the reach read
	// below it** on purpose. That one is refused because it is "the inventory
	// of everything the estate was given, in one response". This is that
	// inventory plus the settings document, the identity providers, the
	// credential inventory, the connected git hosts and the source policy —
	// the whole install described in one place, assembled for somebody whose
	// job is to find weaknesses in it.
	//
	// There is no automation case on the other side of the trade. An assessment
	// is a thing a person does, once, from a browser; nothing schedules it and
	// nothing pipes it. So the only caller left after this is an administrator
	// holding a session, which is the platform's own page.
	//
	// `/admin/estate/targets` stays allowed and is the deliberate contrast: a
	// scanner needs addresses, and addresses are not a posture.
	"GET /api/v1/admin/compliance":               true,
	"GET /api/v1/admin/compliance/controls":      true,
	"GET /api/v1/admin/compliance/controls.pdf":  true,
	"GET /api/v1/admin/compliance/report":        true,
	"GET /api/v1/admin/compliance/report/export": true,

	// What every app on the install holds — every credential name, every mount
	// and its level, every channel, every key that can act as an app. Refused
	// to a key, and this is the one place on this subtree where that differs
	// from its neighbours, so the difference is argued rather than left to be
	// read as an oversight.
	//
	// /admin/estate/languages says what the estate is *written in* and
	// /admin/estate/targets says where it *is*; both are allowed above,
	// because a CMDB asking either is the reason the HTTP API exists. This one
	// is the inventory of everything the estate was *given*, in one response —
	// the single most useful thing on the install to a stolen token, and the
	// one read whose value to an attacker is strictly greater than the sum of
	// the routes it replaces. An administrator reads it in a browser, where
	// the credential behind it is a session rather than a bearer string
	// sitting in a pipeline's environment.
	//
	// The per-app read stays on the allow-list: it is bounded to one app by
	// appForEdit, which is a grant its owner already holds.
	"GET /api/v1/admin/estate/reach": true,

	// Refreshing the catalog is an install-wide administrative action rather
	// than app operation: it decides what every deploy button on the install
	// offers, and it reaches a third-party host. Nothing an automation needs.
	"POST /api/v1/gallery/sync": true,

	// Accepting risk on the organization's behalf. Granting a waiver says "this
	// install will deploy a package with a known critical advisory", which is
	// authorization-shaped rather than app operation — and a key does not do
	// that. Reading the findings and re-checking them sit on the other side of
	// the line: they accept nothing and reveal nothing the caller could not
	// already read.
	"POST /api/v1/apps/{appID}/dependency-waivers":              true,
	"DELETE /api/v1/apps/{appID}/dependency-waivers/{waiverID}": true,
	// The mirror credential, for the reason every credential is here.
	"PUT /api/v1/admin/dependency-feed/credential": true,

	// Granting a person access to a store. The upload routes above are on the
	// allow list and these are not, and the line between them is the same line
	// this file is built on: using a store is operation, and deciding who may
	// use it is authorization. A key that could grant itself `full` on a store
	// would be a key that promotes its own holder.
	//
	// Listed rather than omitted so the refusal is a decision somebody took,
	// not an endpoint nobody looked at.
	"GET /api/v1/admin/storage/{id}/grants":              true,
	"PUT /api/v1/admin/storage/{id}/grants":              true,
	"DELETE /api/v1/admin/storage/{id}/grants/{subject}": true,

	// Delegating storage management, on the same line and for a sharper version
	// of the same reason: a key that could delegate management to its own
	// holder would be a key that grants its own holder a store, since managing
	// a store confers its data plane. Attaching is on the allow list above,
	// because attaching is the operation and this is the decision about who
	// may. The revoke is denied for the third reason as well — it restarts
	// somebody else's app, and that is not an automation's call.
	"GET /api/v1/admin/storage/{id}/managers":              true,
	"PUT /api/v1/admin/storage/{id}/managers":              true,
	"DELETE /api/v1/admin/storage/{id}/managers/{subject}": true,

	// The install's default delegations, for the same reason at the scale of
	// every resource: a default is a delegation written onto each one created,
	// and its revoke_existing restarts apps across the install.
	"GET /api/v1/admin/storage/defaults":              true,
	"PUT /api/v1/admin/storage/defaults":              true,
	"DELETE /api/v1/admin/storage/defaults/{subject}": true,

	// Locking and unlocking an app. Denied for the same reason the waivers
	// above are: it is a decision about who may reach an app, taken on the
	// organization's behalf, rather than the operation of one. A key may
	// restart, deploy and stop its owner's apps all day — none of those decide
	// whether anybody else can open them, and this does.
	//
	// It is also the one control that stops a compromised key deploying: a
	// token that could lift its own app's lock would be able to undo the
	// containment applied to it.
	"POST /api/v1/apps/{appID}/lock":   true,
	"DELETE /api/v1/apps/{appID}/lock": true,

	// A person's own sign-in history. Denied because it is a governance read
	// about the *human's* sessions rather than about the credential asking —
	// a different subject, not merely a stricter one. A key's own history is
	// GET /me/api-keys/{keyID}/usage, which it already has.
	"GET /api/v1/me/sign-ins": true,
	// Ending every session the owner has. This is credential management by any
	// reading of it, and it is the rule this file exists to hold: a stolen CI
	// token that could log its owner out everywhere turns a bounded incident
	// into an outage.
	"POST /api/v1/me/sessions/revoke": true,
	// The zone the SPA shows its owner's times in. Denied because it is about
	// a person reading pages, which a key never does — nothing to break, and
	// nothing a script has a reason to change on somebody's behalf.
	"PUT /api/v1/me/time-zone": true,

	// Credentials. A token must not be able to mint or inspect tokens — its
	// own or anyone else's — or the blast radius of one leak is every leak.
	"GET /api/v1/me/api-keys":               true,
	"POST /api/v1/me/api-keys":              true,
	"PATCH /api/v1/me/api-keys/{keyID}":     true,
	"DELETE /api/v1/me/api-keys/{keyID}":    true,
	"GET /api/v1/me/api-keys/{keyID}/usage": true,
	"GET /api/v1/admin/api-keys":            true,
	"PATCH /api/v1/admin/api-keys/{keyID}":  true,
	"DELETE /api/v1/admin/api-keys/{keyID}": true,
	// One account's keys. "Anyone else's" includes the owner's: GET
	// /api/v1/users/{id} is allowed and stops at the clock, and that is the
	// line — a token may learn that an account is in use, never which
	// credentials are the ones using it.
	"GET /api/v1/users/{id}/api-keys":               true,
	"GET /api/v1/users/{id}/api-keys/{keyID}/usage": true,

	// Identity infrastructure. Whoever can add a provider can add a door they
	// control; reading the list is denied with it because there is no
	// automation case for it and it is the surface an attacker would map.
	"GET /api/v1/admin/auth-providers":                 true,
	"POST /api/v1/admin/auth-providers":                true,
	"PATCH /api/v1/admin/auth-providers/{providerID}":  true,
	"DELETE /api/v1/admin/auth-providers/{providerID}": true,
	"POST /api/v1/admin/auth-providers/discover":       true,

	// The clone credential lives behind these, and it is the install's rather
	// than anybody's: whoever can create a connection can point this platform's
	// builds at a host they control. Reading is denied with writing on the same
	// rule as email's — a key that could list the organization's connected
	// hosts, their base URLs and what they cover is mapping infrastructure.
	"GET /api/v1/admin/git-connections":                     true,
	"POST /api/v1/admin/git-connections":                    true,
	"PATCH /api/v1/admin/git-connections/{connectionID}":    true,
	"DELETE /api/v1/admin/git-connections/{connectionID}":   true,
	"GET /api/v1/admin/git-connections/{connectionID}/apps": true,
	// The test spends the install's own clone credential against the host. A
	// key that could fire it could confirm a stolen connection still works and
	// probe the estate's git hosts for reachability, both without ever holding
	// the credential.
	"POST /api/v1/admin/git-connections/{connectionID}/test": true,
	// Minting a webhook secret hands its caller the one value that makes a
	// delivery verify. A key that could rotate it could forge pushes for every
	// app on the connection, and lock the real host out while doing it.
	"POST /api/v1/admin/git-connections/{connectionID}/webhook-secret":   true,
	"DELETE /api/v1/admin/git-connections/{connectionID}/webhook-secret": true,
	// The developer's view of the same rows. Denied for the same reason as the
	// admin read above and one more: a key cannot set `git_connection_id`
	// (`refusedConnectionByKey`), so a key that could list the connections
	// could only ever be mapping them.
	"GET /api/v1/git/connections": true,
	// The picker. A person's own credential against the host, minted by a
	// browser session and by nothing else — the same rule that makes `toft login`
	// safe, applied to the one other place this platform signs somebody in to a
	// third party.
	"GET /api/v1/git/connections/{connectionID}/authorize": true,
	"GET /api/v1/git/connections/{connectionID}/callback":  true,
	"GET /api/v1/git/connections/{connectionID}/repos":     true,

	// The announcement an administrator sends. Denied entire, and deliberately
	// the opposite of the banner two entries above — which a key *may* publish,
	// filed under content governance an administrator may reasonably script.
	//
	// The banner is one idempotent string that DELETE takes back. A send
	// reaches every person on the install, cannot be taken back, and leaves a
	// permanent row saying it happened. A leaked `toftu_` that could mail the
	// entire directory in this install's voice is a phishing platform rather
	// than a bounded incident, and the reads are denied with the writes because
	// the history of what an organization has told its staff is not something
	// automation needs.
	"GET /api/v1/admin/announcement-emails":              true,
	"POST /api/v1/admin/announcement-emails":             true,
	"GET /api/v1/admin/announcement-emails/estimate":     true,
	"GET /api/v1/admin/announcement-emails/{id}":         true,
	"PATCH /api/v1/admin/announcement-emails/{id}":       true,
	"DELETE /api/v1/admin/announcement-emails/{id}":      true,
	"POST /api/v1/admin/announcement-emails/{id}/clone":  true,
	"POST /api/v1/admin/announcement-emails/{id}/test":   true,
	"POST /api/v1/admin/announcement-emails/{id}/send":   true,
	"POST /api/v1/admin/announcement-emails/{id}/resend": true,

	// The relay credential lives behind these.
	"GET /api/v1/admin/email-settings":       true,
	"PUT /api/v1/admin/email-settings":       true,
	"POST /api/v1/admin/email-settings/test": true,

	// The collector credential lives behind these, and the read is denied with
	// the write on the same rule as email's: a key that could tell you where an
	// install ships its telemetry is mapping infrastructure, and one that could
	// *change* it could quietly redirect every trace and metric this platform
	// produces to a collector of its holder's choosing.
	"GET /api/v1/admin/telemetry-settings":       true,
	"PUT /api/v1/admin/telemetry-settings":       true,
	"POST /api/v1/admin/telemetry-settings/test": true,

	// Features. Denied entire, reads included.
	//
	// The write is the obvious half: switching a feature on is what opens an
	// outbound flow to a model vendor and puts an app on the platform's own
	// reserved slug, and that is a capability grant — a key never makes one.
	// The read is denied with it on the same rule as email's and telemetry's: a
	// key that could enumerate what an install has bought is mapping the
	// install, and the answer is not something automation needs.
	"GET /api/v1/admin/features":        true,
	"PUT /api/v1/admin/features/{name}": true,

	// The integration catalog. Every connection holds a credential — a bot
	// token, a client secret, a routing key, an HMAC key — so every route here
	// is denied by the file's central rule: a key never manages a credential.
	//
	// The reads are denied too, and not out of tidiness. The destinations list
	// enumerates an organization's channels and document-library folders; the
	// placements list says what every app has written into them, by name. Both
	// are an administrator's view of what this install reaches, which is not
	// something a stolen CI token should be able to survey.
	"GET /api/v1/admin/integrations/connections":                       true,
	"POST /api/v1/admin/integrations/connections":                      true,
	"PATCH /api/v1/admin/integrations/connections/{connID}":            true,
	"DELETE /api/v1/admin/integrations/connections/{connID}":           true,
	"POST /api/v1/admin/integrations/connections/{connID}/test":        true,
	"GET /api/v1/admin/integrations/connections/{connID}/destinations": true,
	"GET /api/v1/admin/integrations/connections/{connID}/placements":   true,
	// Where a Teams bot may post, and the package that installs it.
	"GET /api/v1/admin/integrations/connections/{connID}/places":              true,
	"POST /api/v1/admin/integrations/connections/{connID}/places":             true,
	"DELETE /api/v1/admin/integrations/connections/{connID}/places/{placeID}": true,
	"GET /api/v1/admin/integrations/connections/{connID}/teams-app":           true,

	// App integrations — what an app sends *itself*.
	//
	// Denied entire, reads included. Three separate reasons converge on it, and
	// any one would be enough:
	//
	//   - Attaching email to an app lets that app mail any address on the
	//     internet carrying this install's sending identity; attaching
	//     SharePoint lets it write files into the organization's document
	//     library. Those are authorization grants, and a key never makes one.
	//   - The send ledger holds every recipient address in full, third parties
	//     included. It is the record an incident is answered from, not a
	//     reporting endpoint for automation.
	//   - The test send is a real send, recorded like any other. A stolen CI
	//     token that could make an app mail somebody is a worse incident than
	//     one that cannot.
	//
	// The destination routes are denied for the reason the old Slack ones were:
	// a key that could repoint an app's alerts at a channel of its holder's
	// choosing — or silence them by pointing them somewhere nobody watches — is
	// changing what the platform tells people about an incident, which is not
	// something automation gets to do quietly.
	"GET /api/v1/apps/{appID}/integrations":                     true,
	"POST /api/v1/apps/{appID}/integrations/test":               true,
	"GET /api/v1/apps/{appID}/sends":                            true,
	"PUT /api/v1/apps/{appID}/integrations/{kind}":              true,
	"DELETE /api/v1/apps/{appID}/integrations/{kind}":           true,
	"PUT /api/v1/apps/{appID}/integrations/{kind}/destination":  true,
	"GET /api/v1/apps/{appID}/integrations/{kind}/destinations": true,
	"GET /api/v1/admin/integration-settings":                    true,
	"PUT /api/v1/admin/integration-settings":                    true,
	"GET /api/v1/admin/integration-sends":                       true,

	// Who may do what. Every one of these changes an authorization decision.
	"DELETE /api/v1/users/{id}":      true,
	"PATCH /api/v1/users/{id}/role":  true,
	"POST /api/v1/users/{id}/lock":   true,
	"POST /api/v1/users/{id}/unlock": true,
	// Not an authorization decision — the mark never grants — but it decides
	// whose traffic goes uncounted, which is a person's call in a browser.
	"POST /api/v1/users/{id}/scanner":   true,
	"DELETE /api/v1/users/{id}/scanner": true,
	// Activation both admits an account and chooses its role, which is
	// PATCH /role and POST /unlock in one act. Denied for both reasons.
	"POST /api/v1/users/{id}/activate":            true,
	"POST /api/v1/groups":                         true,
	"DELETE /api/v1/groups/{id}":                  true,
	"PUT /api/v1/groups/{id}/members/{userId}":    true,
	"DELETE /api/v1/groups/{id}/members/{userId}": true,
	// Attaching a directory to a group decides who is in it on every future
	// sign-in, which is as much "who may do what" as a grant is.
	// The credential store: a key that could attach one would be a credential
	// that hands out credentials.
	"GET /api/v1/admin/credentials":                      true,
	"GET /api/v1/admin/credentials/{id}/apps":            true,
	"POST /api/v1/admin/credentials":                     true,
	"PATCH /api/v1/admin/credentials/{id}":               true,
	"DELETE /api/v1/admin/credentials/{id}":              true,
	"PUT /api/v1/admin/apps/{appID}/credentials/{id}":    true,
	"DELETE /api/v1/admin/apps/{appID}/credentials/{id}": true,
	// Minting the credential a directory uses to deactivate accounts is as
	// close to "manage credentials" as this list gets.
	"POST /api/v1/admin/auth-providers/{providerID}/scim-token":   true,
	"DELETE /api/v1/admin/auth-providers/{providerID}/scim-token": true,
	"GET /api/v1/groups/{id}/claim-mappings":                      true,
	"PUT /api/v1/groups/{id}/claim-mappings":                      true,
	"DELETE /api/v1/groups/{id}/claim-mappings":                   true,
	// A group's address decides where its apps' failure mail goes, and a group
	// can own apps — so this redirects notifications about an estate. Not an
	// authorization change on its face, and denied with the ones that are.
	"PATCH /api/v1/groups/{id}": true,
	// Bulk ownership transfer is ownership transfer, N times. The single-app
	// version is denied two lines below and this must not be the way around it.
	"POST /api/v1/users/{id}/transfer-apps": true,
	// No key mints a key, including the class that has no owner. This is the
	// same rule that keeps /api/v1/auth's mint behind a browser session, said
	// for the third credential class.
	"GET /api/v1/deploy-keys":                                          true,
	"POST /api/v1/deploy-keys":                                         true,
	"PATCH /api/v1/deploy-keys/{keyID}":                                true,
	"DELETE /api/v1/deploy-keys/{keyID}":                               true,
	"PUT /api/v1/apps/{appID}/grants/{principalType}/{principalID}":    true,
	"DELETE /api/v1/apps/{appID}/grants/{principalType}/{principalID}": true,
	"POST /api/v1/apps/{appID}/transfer":                               true,
	"POST /api/v1/apps/{appID}/access-requests/{reqID}/approve":        true,
	"POST /api/v1/apps/{appID}/access-requests/{reqID}/deny":           true,

	// Role promotion's deciding half. The requester's own three are allowed
	// above, beside the object half they were inconsistent with.
	"GET /api/v1/admin/promotion-requests":                  true,
	"POST /api/v1/admin/promotion-requests/{reqID}/approve": true,
	"POST /api/v1/admin/promotion-requests/{reqID}/deny":    true,

	// Turning capabilities on and off is an operator's decision made in a
	// browser — including, pointedly, the switch that governs API keys.
	"PUT /api/v1/system/settings": true,

	// Emptying the shared build cache. The checks page beside it reads, and
	// GET /system/checks is allowed above; this is the one destructive
	// maintenance verb the platform has, and it acts on state every app's next
	// build depends on. A key is a grant on one app or an identity, and
	// neither is a thing that gets to slow down the estate.
	"POST /api/v1/system/build-cache/reclaim": true,

	// The source policy. Authorization-adjacent in the way that matters: a
	// credential that could widen the approved-source list could then deploy
	// from wherever it liked, and the audit trail would show a token doing what
	// only a person in a browser is supposed to do. The reads are denied with
	// the writes — the full list carries provenance, notes and usage counts,
	// which is a map of the fence rather than a fact an automation needs, and
	// /effective is allowed above for the case that does.
	"GET /api/v1/system/source-policy":          true,
	"PUT /api/v1/system/source-policy":          true,
	"POST /api/v1/system/source-policy/preview": true,
	"GET /api/v1/system/source-policy/observed": true,
}

// userKeyScopeWrites answers whether a key of this scope may issue this method
// against a route already on the allow-list. It is the whole of the `read`
// scope, and it is a **derived** rule rather than a second map.
//
// The alternative was a hand-written read list like deployKeyReadRoutes, and it
// loses on both halves. Containment would need a test to defend, where the
// derived rule makes it true by construction. And a curated list introduces a
// failure this cannot have: a GET added to the allow-list and forgotten in the
// read map, where a read-scoped key is refused something it should reach and
// nobody finds out until a customer's script breaks at 3am.
//
// The deploy class hand-writes its read list because that scope is a *product*
// decision about what a promotion tool needs, and it deliberately withholds
// GETs a person may read. This scope is not that. Its owner can already read
// every one of these routes in a browser, so there is nothing to curate: the
// only question is whether the key writes.
//
// **What the rule rests on** is that no GET handler on the platform API writes
// anything — a measured property of the code rather than a convention it
// promises, and the first place in this codebase where the request method
// carries security meaning at all. TestNoGetHandlerWrites is what keeps it
// true; without that test this function is a guess.
//
// Anything but `full` narrows, `read` included. A row carrying a word nobody
// has implemented must not be the widest credential on the install, which is
// deployKeyRoutesForScope's rule stated a second time rather than inferred.
func userKeyScopeWrites(scope string) bool { return scope == database.UserKeyScopeFull }

// apiKeyRouteGuard enforces the allow-list, and then the key's own scope.
// Cookie sessions never consult it: this is a limit on what a *credential a
// script holds* may do, not on what its owner may do.
//
// **The order names which fence stopped it.** The list is asked first, so a
// read-scoped key attempting POST /admin/users is told the route needs a
// session — because it does, and a wider key would fail identically. Asking the
// scope first would send that caller off to mint a full-scope key that changes
// nothing, which is the afternoon-on-the-wrong-remedy failure deployKeyRouteGuard
// already names one class over.
func (s *Server) apiKeyRouteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := auth.GetPrincipal(r.Context())
		if !p.IsAPIKey() {
			next.ServeHTTP(w, r)
			return
		}
		pattern, resolved := s.routeMatch(r)
		// **A path that matches no route is not a refusal.** Nothing here
		// decides whether this credential may do it, because there is no "it":
		// the request reaches no handler whoever presents it. Answering the
		// sentence below would be a confident, specific, wrong diagnosis — and
		// as a 403 it tells an unattended caller to mint a new key or fetch an
		// administrator, neither of which is retryable, for a typo. So it falls
		// through to the router, which 404s.
		//
		// This is not the fail-open half of the fail-closed rule below. A
		// nil mux cannot resolve anything, so it leaves `resolved` false and
		// `pattern` empty, which is in neither map and is refused as before.
		if s.mux != nil && !resolved {
			next.ServeHTTP(w, r)
			return
		}
		if !apiKeyAllowedRoutes[r.Method+" "+pattern] {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "this route requires an interactive session — API keys cannot manage " +
					"credentials or change who may do what",
				"code": "session_required",
			})
			return
		}
		// An exchanged key (ADR §157) is the person, bounded to the deploy
		// surface and its owner's storage. Asked after the allow-list for the same reason the scope
		// is: a route no key reaches is a session's, whatever this key is.
		if p.IsExchangedKey() {
			if !exchangedKeyAllowedRoutes[r.Method+" "+pattern] {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "this is a short-lived deploy key — it may create, push, watch and roll back " +
						"apps, and use its owner's storage, and nothing else",
					"code": "key_deploy_scope",
					"hint": "sign in through the browser, or use a personal key, for anything else",
				})
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && !userKeyScopeWrites(p.APIKeyScope) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "this key is read-scoped — it may read anything you can read and " +
					"change nothing. A full-scope key can do this",
				"code": "key_read_scope",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// routePattern resolves the route this request will land on, before it lands
// there. Middleware runs above the inner routing, so chi has not yet filled in
// the pattern; Match runs the same lookup against a throwaway context.
//
// An unresolvable path yields "", which is in neither map and is therefore
// refused — the fail-closed default, stated once here rather than assumed at
// each call site.
func (s *Server) routePattern(r *http.Request) string {
	pattern, _ := s.routeMatch(r)
	return pattern
}

// routeMatch is routePattern with the half the guards need back: *whether the
// router resolved anything at all*.
//
// The two failures collapse into "" and they are not the same thing. A path
// nobody registered would 404 whoever asked, so a credential guard has nothing
// to decide about it; a path that resolves and is not on the list is a refusal
// with a remedy. Reporting the first as the second told an agent its key was
// the problem — exit 3, "mint a new key or ask an administrator", neither
// retryable — for a mistyped path.
//
// **The method decides it too.** A registered path under a method no handler
// serves is the router's 405, not a credential question: PATCH /api/v1/apps
// as a key was refused session_required — exit 3, "your key cannot do that" —
// for a verb nothing on the install takes from anyone (D-23, fourth
// 2026-11-03 black-box list). chi's Find resolves such a request to the
// pattern of a method that *is* served, so the pair is checked against the
// walked route set before it counts as resolved.
func (s *Server) routeMatch(r *http.Request) (string, bool) {
	if s.mux == nil {
		return "", false
	}
	// The same string chi will route on: the encoded path when there is one.
	// Matching the decoded path asked about a different route than the one that
	// runs — `/users/a%2Fb/api-keys` decodes to a path nothing serves, which the
	// guards pass through, and chi then dispatches `/users/{id}/api-keys`.
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	return s.resolveRoute(r.Method, path)
}

// resolveRoute is the pattern a method and an already-encoded path land on,
// and whether a handler serves that method there.
func (s *Server) resolveRoute(method, path string) (string, bool) {
	rctx := chi.NewRouteContext()
	if !s.mux.Match(rctx, method, path) {
		return "", false
	}
	pattern := normalizeRoutePattern(rctx.RoutePattern())
	if s.muxRoutes != nil && !s.muxRoutes[method+" "+pattern] {
		return "", false
	}
	return pattern, true
}

// servesMethod reports whether a handler takes method at path.
func (s *Server) servesMethod(method, path string) bool {
	_, ok := s.resolveRoute(method, path)
	return ok
}

// normalizeRoutePattern reconciles chi's two spellings of the same route.
//
// A subrouter's index handler — r.Route("/apps", …) with r.Get("/", …) — is
// reported as "/api/v1/apps/" by chi.Walk and as "/api/v1/apps" by
// Match+RoutePattern. Without this, an entry could satisfy the drift test and
// still miss at runtime, which is the worst failure this file could have: the
// list would look complete and quietly refuse a route it claims to allow.
// Both the guard and the test go through here, so they cannot disagree.
func normalizeRoutePattern(p string) string {
	if len(p) > 1 && p[len(p)-1] == '/' {
		return p[:len(p)-1]
	}
	return p
}

// The route policy for a **read-scoped** deploy key — the narrow half of the
// class (ADR §51).
//
// **read ⊂ deploy ⊂ personal**, one chained assertion in
// TestDeployKeyScopesAreAChainOfSubsets, so a widening of the middle cannot
// pass and neither list can drift from the one above it.
//
// **Why the class reads at all.** "A pipeline pushes code" was written for CI,
// which builds and then pushes what it built. It is not true of *promotion*,
// which does not build: the artefact already exists on another install,
// produced by a build this pipeline did not run, and getting it is a read.
// Downloading a release archive is not a credential and not an authorization
// decision — it is the same bytes POST /apps/{appID}/upload already accepts
// from this same credential, travelling the other way. A class that may write a
// release and may not read one describes a pipeline that can only ever ship
// code it compiled itself, which is the one shape promotion is not.
//
// **Why the scope is narrower than the class rather than a fourth class.** A
// reader key would belong to nobody exactly as a deploy key does, which is the
// blurring the three-classes rule exists to prevent. What differs is blast
// radius, not belonging: an estate tool that pulls releases off this install
// wants a credential that cannot push to the apps on it — its own app included
// — and a scope inside the class is that, with one prefix, one table, one admin
// page and one audit actor.
//
// **Why the group routes are here rather than only on the deploy scope.** Both
// ends of a promotion need them: the pushing end for an owning-group picker,
// the pulling end to name a group to notify. The disclosure is smaller than it
// looks — handleListGroups gates on a non-nil principal and nothing else, so
// every signed-in person on the install already sees every group, its member
// count and its address — and the one thing that *is* protected stays
// protected: handleGetGroup withholds the roster from a principal that is
// neither an administrator nor a member, and a key that belongs to nobody is
// neither. This scope gets a directory of team names and an address; it does
// not get people.
//
// **What is deliberately not pre-provisioned.** POST /apps/{appID}/job-runs
// stays on the deploy scope, although a scan started on the source app is the
// obvious next thing to want here. Widening a scope for a feature nobody has
// built is how a scope stops being a boundary.
var deployKeyReadRoutes = map[string]bool{
	// What this key is and what the install will do with a deploy — `toft
	// whoami`. The key's own record and the install's switches, nothing about
	// anybody else, and the question a pipeline should ask before it deploys
	// rather than learn from the refusal.
	"GET /api/v1/me/capabilities": true,

	// Find the app, and find it by the name a config file holds.
	"GET /api/v1/apps":                true,
	"GET /api/v1/apps/{appID}":        true,
	"GET /api/v1/apps/by-slug/{slug}": true,

	// Read its history, and take the bytes of one release of it. The archive is
	// source as deployed; handleDeploymentArchive requires edit rights, which a
	// deploy key has on every app, and answers 410 when the release has been
	// pruned — so a promotion whose bytes are gone fails loudly.
	"GET /api/v1/apps/{appID}/deployments":                    true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}":         true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}/archive": true,

	// The platform's own verdict on the packages in that release, so whatever
	// is deciding whether to promote it reads the same answer the app page
	// shows. It belongs to the exact release, which is why it is worth having
	// here rather than being re-derived from a package list somewhere else.
	"GET /api/v1/apps/{appID}/dependencies": true,
	// The same inventory as an SBOM, for a pipeline whose next step is a
	// supply-chain tool rather than this verdict.
	"GET /api/v1/apps/{appID}/deployments/{deployID}/sbom": true,

	// The group directory. Names, counts and addresses — never a roster.
	"GET /api/v1/groups":      true,
	"GET /api/v1/groups/{id}": true,

	// The estate as scan targets, and **the only route under /admin either
	// deploy-key scope reaches**. The line the rest of that subtree is on —
	// "everything under /system and /admin: not deploying" — holds because
	// this is not an exception to what a deploy key may *see*: it already
	// enumerates every app on the install through GET /apps, which is two
	// entries above. What it gains is the URL column, and what that buys is a
	// scan pipeline that does not have to be handed an administrator's
	// personal key in order to find out what to scan.
	//
	// It is a read, so it belongs to the read scope, and the chain then puts
	// it in the deploy scope and in a personal key's list as well.
	"GET /api/v1/admin/estate/targets": true,
}

// The route policy for a **deploy-scoped** deploy key — `toftd_`, the credential
// that belongs to nobody (ADR §51). The class's default scope, and what every
// key minted before scopes existed still has.
//
// **A strict subset of apiKeyAllowedRoutes, and a test says so.** That is the
// property worth having rather than a second hand-maintained list: whatever a
// deploy key may do, a person's own key may already do, so this map can only
// ever narrow and never widen the surface. TestDeployKeyRoutesAreASubset checks
// it, and TestEveryRouteIsClassified keeps the parent list honest. Since the
// class gained a second scope the property is a chain — read ⊂ deploy ⊂
// personal — and the middle link is asserted in the same test.
//
// It matters more here than for the other two classes, because a deploy key has
// no owner and therefore no role to fall back on: auth.Authorize grants it
// owner on every app, and this list is the only thing bounding that. The
// governing sentence is the same one as for a personal key — it may never
// manage a credential, its own included, or change who may do what.
//
// What is deliberately *absent*, each for a reason:
//
//   - PATCH /apps/{appID} — carries visibility, which is an authorization
//     decision, and git_token, which is a credential.
//   - DELETE /apps/{appID} and DELETE .../deployments — a pipeline pushes code;
//     destroying an app or pruning its history is a person's decision, and a
//     stolen CI token that could delete the estate is a different incident.
//   - jobs, tags, storage — a personal key's "automation outside
//     Toft". A promotion pipeline is not that; it deploys and stops.
//   - /store — the sharpest case of the same line. A store grant belongs to a
//     person and a deploy key belongs to nobody, so there is no answer to
//     "which grants does this key hold": it would either reach every store or
//     none, and the first is not a thing anybody meant to build.
//   - everything under /system and /admin — not deploying.
//
// The reads it holds beyond its own pushes are in deployKeyReadRoutes above,
// repeated here rather than merged at init, because the whole point of both
// lists is that a reader can see what a scope reaches by reading it.
var deployKeyAllowedRoutes = map[string]bool{
	// Everything the read scope reaches. The archive, the dependency verdict
	// and the group directory are the three this scope did not have before
	// promotion needed them; the group routes are what make owner_group_id a
	// picker rather than a typed id the pipeline has to be told.
	"GET /api/v1/apps/{appID}/deployments/{deployID}/archive": true,
	"GET /api/v1/apps/{appID}/dependencies":                   true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}/sbom":    true,
	"GET /api/v1/groups":               true,
	"GET /api/v1/groups/{id}":          true,
	"GET /api/v1/admin/estate/targets": true,

	// Create and push.
	"POST /api/v1/apps":                true,
	"POST /api/v1/apps/upload":         true,
	"POST /api/v1/apps/check":          true,
	"POST /api/v1/apps/{appID}/deploy": true,
	"POST /api/v1/apps/{appID}/upload": true,
	"POST /api/v1/apps/{appID}/check":  true,
	"GET /api/v1/me/capabilities":      true,

	// Move an app between releases and states. Rollback is here and delete is
	// not, which is the line: a pipeline must be able to undo its own deploy
	// without being able to destroy what it deployed onto.
	"POST /api/v1/apps/{appID}/deployments/{deployID}/rollback": true,
	"POST /api/v1/apps/{appID}/restart":                         true,
	"POST /api/v1/apps/{appID}/stop":                            true,

	// Write the app's environment, so a pipeline rotates a secret with the
	// key it already ships with rather than a person's key planted in CI. On
	// the ship side of the line for the reason migration jobs are: a key that
	// can upload arbitrary code to the app can already make that code read its
	// environment, send it anywhere or ignore it, so writing a variable adds no
	// reach. A credential's keys are refused inside the route, and the route
	// carries nothing else — PATCH /apps/{appID}, which also re-points and
	// re-governs, stays off.
	"PATCH /api/v1/apps/{appID}/env": true,

	// Find the app it is about to deploy to. by-slug is the one that matters
	// for a pipeline, which holds a name from a config file and no id.
	"GET /api/v1/apps":                true,
	"GET /api/v1/apps/{appID}":        true,
	"GET /api/v1/apps/by-slug/{slug}": true,

	// Start the work the deploy needs, and watch it. Running a migration job is
	// on the deploy side of the line above: the job is code this same pipeline
	// just shipped, running under the app's own identity, and a pipeline that
	// can push arbitrary code to the app can already cause it to run. What
	// stays out is pausing a job's schedule (PATCH /jobs), because that is an
	// operational decision about the app rather than a step in shipping it.
	//
	// A run started this way is `started_by = cli` with no actor, and the audit
	// row names the key. It cannot be the app's actor_id: that column is a
	// foreign key to users, and a credential that belongs to nobody has no row
	// there. See handleTriggerJobRun.
	//
	// The two lists are here too, so a pipeline finds its job and its runs by
	// name rather than needing an id it was never shown (BB-108): owning the
	// trigger and not the read of what it triggered was a half.
	"GET /api/v1/apps/{appID}/jobs":                    true,
	"GET /api/v1/apps/{appID}/job-runs":                true,
	"POST /api/v1/apps/{appID}/job-runs":               true,
	"POST /api/v1/apps/{appID}/jobs/{job}/runs":        true,
	"POST /api/v1/apps/{appID}/job-runs/{runID}/rerun": true,
	"GET /api/v1/apps/{appID}/job-runs/{runID}":        true,
	"GET /api/v1/apps/{appID}/job-runs/{runID}/logs":   true,
	"GET /api/v1/apps/{appID}/job-runs/{runID}/output": true,

	// Read what it did. A pipeline that cannot see why its deploy failed is a
	// pipeline whose failures get debugged by a human with a browser.
	"GET /api/v1/apps/{appID}/deployments":                     true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}":          true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}/progress": true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}/logs":     true,
	"GET /api/v1/apps/{appID}/deployments/{deployID}/stream":   true,
	"GET /api/v1/apps/{appID}/events":                          true,
	"GET /api/v1/apps/{appID}/logs":                            true,
	"GET /api/v1/apps/{appID}/logs/stream":                     true,

	// Compile against what the platform will compile against. A pipeline that
	// builds or vets the app before pushing it needs the same SDK the deploy
	// would install, and reading it adds nothing a push does not already imply:
	// this key can already ship the code that gets compiled against it.
	"GET /api/v1/sdk/{lang}": true,
}
