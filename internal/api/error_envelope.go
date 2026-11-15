package api

import "net/http"

// Every refusal this API writes has the same four keys.
//
// **The reader that needs them is the unattended one.** A person reads the
// sentence and knows whether to try again; a program has to be told. About a
// thousand call sites write `{"error": "<sentence>"}`, a sixth of them with a
// `code` beside it, and an agent calling `toft api` or an SDK got English for
// the rest — which it either parses or escalates. The two outcomes that cost
// most are the two the envelope exists to prevent: retrying something that
// will refuse identically forever, and stopping to fetch a human over
// something a retry would have fixed.
//
// So `writeJSON` completes any error body on the way out rather than each call
// site being converted:
//
//   - `error` is the sentence, unchanged.
//   - `code` is the call site's own when it named one, otherwise the status's
//     generic name (`not_found`, `conflict`, …). Never absent, so a client
//     branches on one field rather than on "code, or else the status".
//   - `retryable` is the call site's own when it said, otherwise the code's
//     (`queue_full`, `rate_limited`, `slow_down`), otherwise the status's: 429
//     and the 5xx that are not "not implemented".
//   - `hint` is the next step, only for codes that have one worth naming.
//
// **Completion only adds.** A key the call site set wins, so a handler that
// knows better says so in the one place that knows. It touches maps carrying
// an `error` key and nothing else — a struct a handler built is already a
// deliberate shape, and SCIM's errors are RFC 7644's, keyed `detail`.
//
// The rsconnect compatibility subtree opts out through `verbatimErrors`: it
// answers in Posit Connect's shape, where `code` is an integer, and a string
// there is a client a vendor ships tripping over a field it believes it knows.
// The opt-out is the subtree's rather than each call site's because those
// handlers share the authorization, lock and upload helpers with everything
// else, and every one of them writes through writeJSON.

// errorCodeForStatus is the generic code for a status whose call site named
// none. These are deliberately the status's name and no more: a generic code
// is a promise that a client can branch on *something*, not a diagnosis.
var errorCodeForStatus = map[int]string{
	http.StatusBadRequest:            "bad_request",
	http.StatusUnauthorized:          "unauthorized",
	http.StatusPaymentRequired:       "payment_required",
	http.StatusForbidden:             "forbidden",
	http.StatusNotFound:              "not_found",
	http.StatusMethodNotAllowed:      "method_not_allowed",
	http.StatusNotAcceptable:         "not_acceptable",
	http.StatusConflict:              "conflict",
	http.StatusGone:                  "gone",
	http.StatusPreconditionFailed:    "precondition_failed",
	http.StatusRequestEntityTooLarge: "too_large",
	http.StatusUnsupportedMediaType:  "unsupported_media_type",
	http.StatusUnprocessableEntity:   "invalid",
	http.StatusLocked:                "locked",
	http.StatusPreconditionRequired:  "precondition_required",
	http.StatusTooManyRequests:       "rate_limited",
	http.StatusInternalServerError:   "internal",
	http.StatusNotImplemented:        "not_implemented",
	http.StatusBadGateway:            "bad_gateway",
	http.StatusServiceUnavailable:    "unavailable",
	http.StatusGatewayTimeout:        "timeout",
}

// retryableCodes are the codes whose answer is "yes" whatever their status —
// a 409 that means *wait*, rather than *you asked for something impossible*.
var retryableCodes = map[string]bool{
	"queue_full":    true,
	"deploy_queued": true,
	"rate_limited":  true,
	"slow_down":     true,
}

// errorHints is the next step for a code, in the platform's words, so the CLI,
// the SDKs and a raw `curl` read one sentence rather than three.
//
// **A hint names an act, never a policy.** It says what the caller can do —
// mint a key, ask an administrator, wait — and nothing about the rule that
// refused them that the code has not already said. A code without an act
// worth naming has no entry, because "this failed" restated is noise a reader
// learns to skip, and then skips the hint that mattered.
var errorHints = map[string]string{
	"route_not_found":  "check the method and the path — a path without a leading slash is read as relative to /api/v1/; nothing was attempted",
	"unauthorized":     "send a credential: Authorization: Bearer <key>, or run `toft login`",
	"app_key_removed":  "use a personal key (toftu_…) of an account granted on the app — `toft login` mints one",
	"invalid_key":      "this install does not know the key — it was revoked, or never issued here; check TOFT_TOKEN, or run `toft login`",
	"key_expired":      "mint a new key — `toft login` replaces the stored one; retrying will not help",
	"key_revoked":      "the key was revoked; mint a new one — retrying will not help",
	"key_disabled":     "an administrator disabled this key; ask them, or use a different one",
	"owner_locked":     "the account this key belongs to is locked; an administrator unlocks it",
	"key_read_scope":   "this key was minted read-only and cannot be widened — mint a full-scope key",
	"session_required": "this route needs a signed-in session, never an API key — sign in through the browser, or from the CLI with `toft login --password-stdin` and repeat the call with `--session`",
	"deny_system_role": "this account's role does not allow that; POST /api/v1/me/promotion-requests asks an administrator for the publisher role",
	"deny_list_only":   "you can see that this app exists but not open it; POST /api/v1/apps/{id}/access-requests asks its owner",
	// Acting on an app your role on it does not allow — a viewer of the app
	// deploying, rolling back, changing its settings. Not deny_system_role,
	// which is only *creating* an app: the system role gates ActionPublish
	// alone, and everything else is the app's own ladder. The editor role is
	// what deploys, and an account whose system role is viewer cannot hold it
	// (handleCreateAccessRequest), so the second step is named too
	// (black-box BB-1, 2026-11-04).
	"deny_no_action": `your role on this app does not allow that — its owner can make you an editor: POST /api/v1/apps/{id}/access-requests with {"role":"editor"} asks them (an account with the viewer role needs the publisher role first: POST /api/v1/me/promotion-requests)`,
	// Deleting an app, changing who can see it and managing who has access
	// are the owner's alone, so no grant a caller could ask for unlocks them:
	// the next step is the owner, not an access request (black-box BB-32).
	"deny_owner_only":      "only the app's owner, or an administrator, can delete it, change who can see it or manage who has access — ask them; no role you could request allows it",
	"app_locked":           "nothing was attempted; an administrator has to lift the lock, and retrying will refuse identically",
	"uploads_not_allowed":  "this install does not take uploaded bundles; deploy from a repository an administrator allows",
	"source_not_allowed":   "this install does not allow that source; an administrator manages the allowed sources",
	"tier_limit":           "a license limit on this install, not a permission problem; an administrator can raise it",
	"not_licensed":         "this install's license does not include that feature",
	"public_apps_disabled": "this install does not allow public apps; keep the app private or ask an administrator",
	"queue_full":           "wait for a run in flight to finish, then try again",
	"deploy_queued":        "wait for the queued deployment to finish, then try again",
	"rate_limited":         "slow down, and honor Retry-After when it is sent",
	"slow_down":            "poll at the interval you were given",
	"jobs_unavailable":     "this install cannot run jobs for this app; the sentence says why — configuration, not the job",
	"name_taken":           "choose another name",
	"key_limit_reached":    "revoke a key first — an expired or disabled key holds its slot until it is revoked",
	"store_not_mapped":     "no store by that name is attached to this app; an administrator maps one",
	"too_large":            "send less — the sentence names the limit, and nothing over it is truncated",
	"version_conflict":     "read it again, reapply your change, and write with the version you just read",
	"unknown_kind":         "check the kind's name — the sentence quotes what was sent",
	"store_uploads_off":    "read the store at its mount path instead, or ask an administrator to configure a store signer",
}

// completeErrorBody adds the envelope's missing keys to an error body, or
// returns data unchanged when it is not one. It copies rather than mutating,
// because a call site may reuse its map.
func completeErrorBody(status int, data any) any {
	var body map[string]any
	switch m := data.(type) {
	case map[string]string:
		if _, ok := m["error"]; !ok {
			return data
		}
		body = make(map[string]any, len(m)+3)
		for k, v := range m {
			body[k] = v
		}
	case map[string]any:
		if _, ok := m["error"]; !ok {
			return data
		}
		body = make(map[string]any, len(m)+3)
		for k, v := range m {
			body[k] = v
		}
	default:
		return data
	}

	code, _ := body["code"].(string)
	if code == "" {
		code = errorCodeForStatus[status]
		if code == "" {
			code = "error"
		}
		body["code"] = code
	}
	if _, ok := body["retryable"]; !ok {
		body["retryable"] = retryableCodes[code] ||
			status == http.StatusTooManyRequests ||
			(status >= 500 && status != http.StatusNotImplemented)
	}
	if _, ok := body["hint"]; !ok {
		if hint := errorHints[code]; hint != "" {
			body["hint"] = hint
		}
	}
	return body
}

// verbatimErrorWriter marks a response whose error bodies are written as given.
type verbatimErrorWriter struct{ http.ResponseWriter }

// Unwrap lets http.ResponseController reach the writer underneath, so a
// handler in the subtree that flushes or sets a deadline still can.
func (v verbatimErrorWriter) Unwrap() http.ResponseWriter { return v.ResponseWriter }

// verbatimErrors is the middleware that marks a subtree's responses.
func verbatimErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(verbatimErrorWriter{w}, r)
	})
}

// badBodyHint is the next step for a request body that did not decode: the
// one refusal dozens of routes share, so it is said once.
const badBodyHint = "send one JSON object with this route's fields, with Content-Type: application/json — " +
	"`toft api METHOD PATH --data '{...}'` sends one"
