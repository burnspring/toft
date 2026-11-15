package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/google/uuid"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/telemetry"
	"toft/internal/textutil"
)

func jsonValStr(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// writeAuthzError converts an authorize decision into the appropriate HTTP
// response. Must only be called when d.Allow == false.
//   - no principal → 401
//   - principal holds the `list` rung on the object → 403 `deny_list_only`
//   - principal cannot read the object → 404 (prevents enumeration)
//   - principal can read but not act → 403, carrying the decision's reason
//
// The 403 body gained a "code" so a client can tell "your system role is too
// low" from "you have no grant on this app" and offer the right next step — a
// viewer who tries to create an app should be pointed at the publisher-request
// flow, not left with a bare "forbidden". It is the same string the authorizer
// already reasons in (deny_system_role, deny_no_action, …); the human-readable
// half stays "forbidden", because a code is for the UI and refusals should not
// narrate the policy that produced them.
func writeAuthzError(w http.ResponseWriter, r *http.Request, d auth.Decision, readable bool) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	// The third state, and the scoped exception to *404 over 403*.
	//
	// A caller holding the `list` rung has been told this app exists by an
	// owner who wrote them a grant saying so, and 404 on an id the same API
	// just listed teaches whoever reads it that the list is unreliable — which
	// is word for word the reasoning the invariant already carries for an
	// environment-configured sign-in provider. It is the general case of that
	// special one.
	//
	// The exception is bounded by the grant. A caller with no relationship to
	// the app still gets 404 below, so nothing is enumerable that was not
	// listed, and this is checked ahead of `readable` because it is decided by
	// the decision rather than by the call site: it must answer the same way on
	// **every** app route, not only the detail read. A gate on one route and a
	// raw 404 on the rest is how a caller learns what exists by trying.
	if d.Reason == "deny_list_only" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "forbidden", "code": d.Reason, "hint": "you can see that this app exists but not open it; POST /api/v1/apps/{id}/access-requests asks its owner"})
		return
	}
	if !readable {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/apps lists the apps you can see, with their ids; toft apps from the CLI"})
		return
	}
	body := map[string]string{"error": "forbidden"}
	if d.Reason != "" {
		body["code"] = d.Reason
	}
	writeJSON(w, http.StatusForbidden, body)
}

// objectFromApp constructs an auth.Object describing the given app row.
// objectFromApp is the app as the authorizer sees it.
//
// OwnerID is the owning *user* and is empty when a group owns the app — which
// is why group ownership needed no change here. A group owner arrives through
// the resolver as an ordinary owner grant, so the denormalised check below it
// simply does not fire, and the answer is the same one it always was.
func objectFromApp(a *database.App) *auth.Object { return a.AuthObject() }

// appObject is the four-fact form, for the access review, which builds its
// objects from grant rows rather than from app rows.
//
// Both are one-line forwards to [database.AuthObjectOf], which is where the
// effective visibility is computed and where the argument for it is written.
// It moved there when internal/cronjobs gained a third reader.
// listableToAll is false here and deliberately so: this form is built from
// grant rows for the access review, which reports what a person's **explicit
// relationships** confer. The install-wide rung is not a relationship, and
// folding it in would make every app in the estate look like something the
// subject had been granted.
func appObject(id, ownerID, visibility string, anonymityWithheld bool) *auth.Object {
	return database.AuthObjectOf(id, ownerID, visibility, anonymityWithheld, false)
}

// requestMetaMiddleware captures request_id, IP, and user agent into context so
// audit writes can attribute them to the correct request.
func requestMetaMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Resolved upstream by requestIDMiddleware, which is mounted on the root
		// router above every route this one is on and is also what puts the id
		// on the response. The fallback covers a test that mounts this
		// middleware by itself; in a served request it never fires, and an
		// audit row with an id nobody was told is the thing it prevents.
		rid := requestIDFrom(r.Context())
		if rid == "" {
			rid = uuid.New().String()
		}
		// Mounted after the authentication middlewares precisely so this is
		// answerable: an audited mutation should name the credential that made
		// it, not just the person it was made as.
		p := auth.GetPrincipal(r.Context())
		keyID := ""
		if p.IsAPIKey() {
			keyID = p.APIKeyID
		}
		// The same fact, told to the trace. The telemetry middleware starts the
		// span before any of the authentication middlewares run, so this is the
		// first point in the chain that can say who the request was for. It is
		// a span attribute only and it is dropped on the way out unless the
		// operator set OTEL_RECORD_ENDUSER_ID — see ADR §31 D5 and D7.
		if p != nil {
			telemetry.SetRequestEnduser(r.Context(), p.ID)
		}
		// And to the access log, which is outermost and therefore learns this
		// the same way — from a slot rather than from the context it was given.
		setAccessPrincipal(r.Context(), p)
		ctx := database.WithRequestMeta(r.Context(), database.RequestMeta{
			RequestID: rid,
			// The address without its ephemeral source port — the same helper
			// the API-key path has always used for last_used_ip. Audit rows
			// stored the port and the key table did not, so one column held
			// `10.0.0.4` and the other `10.0.0.4:51234` for the same caller,
			// and neither could be searched with what the other showed.
			IP: clientIP(r),
			// The one request string stored as sent: bytes Postgres refuses
			// failed every best-effort audit row of the request (LB-7).
			UserAgent: textutil.Clip(r.UserAgent(), 512),
			APIKeyID:  keyID,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestMeta(ctx context.Context) database.RequestMeta {
	return database.GetRequestMeta(ctx)
}

// auditEntryFor pre-fills the request-scoped half of an audit entry. The actor
// and the action are the caller's to set — this is the part every entry has and
// nobody should retype.
func auditEntryFor(r *http.Request) database.AuditEntry {
	meta := requestMeta(r.Context())
	return database.AuditEntry{
		RequestID: meta.RequestID,
		IP:        meta.IP,
		UserAgent: meta.UserAgent,
		APIKeyID:  meta.APIKeyID,
	}
}

// appOr404 loads the app a route names and answers for the caller when it
// cannot: a database error is 500 "internal error" and logged, a missing row
// is 404 "app not found", and the caller returns on false.
//
// The two cases are kept apart on purpose. Forty-eight handlers once answered
// "app not found" to a database blip, which taught every client — the SPA,
// `toft`, the SDKs — that 404 means what 5xx means, and a client that has learnt
// that acts on a false fact instead of retrying. One helper, one spelling; a
// handler that loads an app by id calls this and nothing else. Authorization
// stays with the caller: most sites go on to auth.Authorize and the 404/403
// split, and that is a different shape from this one.
func (s *Server) appOr404(w http.ResponseWriter, appID string) (*database.App, bool) {
	app, err := s.db.GetApp(appID)
	if err != nil {
		log.Printf("loading app %s: %v", appID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, false
	}
	if app == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "app not found", "hint": "GET /api/v1/apps lists the apps you can see, with their ids; toft apps from the CLI"})
		return nil, false
	}
	return app, true
}
