package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

type createAccessRequestBody struct {
	Role string `json:"role"`
	// Note is why you are asking, in the requester's own words.
	//
	// An ask you cannot explain is one people route around into chat, which is
	// the outcome this whole feature exists to prevent. It is stored in
	// `access_requests.reason`, which until now was written only on a denial —
	// so `reason` is the request's free text at whichever end last wrote it,
	// and the API returns it either way. A second column would have made the
	// client ask which one to show, and the two writers never collide: a
	// request is denied once and withdrawn never.
	Note string `json:"note"`
}

type denyAccessRequestBody struct {
	Reason string `json:"reason"`
}

func (s *Server) handleCreateAccessRequest(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.Type != database.PrincipalTypeUser {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "service accounts cannot request access", "hint": "sign in with a person's own account to request access"})
		return
	}
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	// Requesting access to an app the caller cannot see is how the approval IDOR
	// was staged: the response returns the request id, which was then approved
	// through an app the attacker did own. It is also plain enumeration.
	//
	// **Or holds the `list` rung**, which is the change this route was waiting
	// for. Until the rung existed, the only people who could ask for access were
	// people who already had it, and a client on top of that was a form nobody
	// could reach. The enumeration argument survives untouched: a `list` grant
	// is a row an owner deliberately wrote, so nothing reaches this that was not
	// already told the app exists.
	// The decision, not a second derivation of it: `deny_list_only` is exactly
	// "this caller was told the app exists", and it is the same fact
	// writeAuthzError answers 403 on. Re-deriving it from the effective role
	// would also miss the administrator, whose implicit rung lives in Authorize
	// rather than in the grant rows.
	obj := objectFromApp(app)
	read := auth.Authorize(p, auth.ActionRead, obj, s.db)
	if !read.Allow && read.Reason != "deny_list_only" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "`toft apps` lists the apps you can see"})
		return
	}
	var body createAccessRequestBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Role != database.ObjectRoleEditor && body.Role != database.ObjectRoleViewer {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role", "hint": `send "role": "viewer" or "editor"`})
		return
	}

	if len(body.Note) > noteLimit {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "the note is too long — keep it under " + strconv.Itoa(noteLimit) + " characters", "hint": "shorten the note and send it again"})
		return
	}

	autoDeny := false
	reason := ""
	if p.SystemRole == database.SystemRoleViewer && body.Role == database.ObjectRoleEditor {
		autoDeny = true
		reason = "viewer system role cannot hold editor object role; request a publisher promotion first"
	}

	// Routed, not assigned. ApproverID is left empty and written at decision
	// time with whoever actually acted; what is set here is where the request
	// goes. For a group-owned app that is the group, and any member may decide
	// it — the authorization check on approve is against the app, and a member
	// resolves to owner through the owner grant, so no route needed widening.
	req := &database.AccessRequest{
		RequesterID:     p.ID,
		RequestType:     database.AccessReqTypeObject,
		ObjectID:        appID,
		RequestedRole:   body.Role,
		ApproverGroupID: app.OwnerGroupID,
		// The requester's words, unless the request is being auto-denied — in
		// which case CreateAccessRequest overwrites this with the refusal's
		// reason, which is the more useful thing to have kept.
		Reason: strings.TrimSpace(body.Note),
	}
	if err := s.db.CreateAccessRequest(r.Context(), req, autoDeny, reason, requestMeta(r.Context())); err != nil {
		if errors.Is(err, database.ErrDuplicateRequest) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "a pending request already exists", "hint": "wait for the pending request to be answered — GET /api/v1/me/access-requests shows it"})
			return
		}
		log.Printf("create access request: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, req)

	// After the response, and best-effort: the row is written and the queue is
	// already showing it, so a relay that is down costs a nudge and nothing
	// else. The requester is loaded rather than assembled from the principal —
	// a JWT carries a login, and this message names a person and needs their
	// address.
	requester, err := s.db.GetUser(p.ID)
	if err != nil {
		log.Printf("access request notify: loading the requester: %v", err)
		return
	}
	s.notifyAccessRequested(app, req, requester)
}

// handleListMyAccessRequests is the requester's own list, and it exists because
// until now they could create a request and then not see it: the object list
// route requires ActionApproveAccessRequest, which is the decider's permission.
//
// The twin of /me/promotion-requests, same shape and same placement. It takes
// no id and is scoped to the caller inside the query, so the IDOR rule has
// nothing to bite on.
func (s *Server) handleListMyAccessRequests(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	out, err := s.db.ListObjectAccessRequestsForUser(p.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	if out == nil {
		out = []database.ObjectAccessRequestDetail{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListAccessRequests(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionApproveAccessRequest, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	out, err := s.db.ListAccessRequestsForObject(appID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	if out == nil {
		out = []database.ObjectAccessRequestDetail{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleApproveAccessRequest(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	reqID := chi.URLParam(r, "reqID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	// Only the app owner may approve object access requests (not generic system_admin).
	// However system_admin bypass applies via authorize() — this matches PRD §12.
	if d := auth.Authorize(p, auth.ActionApproveAccessRequest, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	// An approval is a grant; under a lock it is refused as the grant PUT is
	// (security review 2026-11-15, LSA-2). Denying stays open.
	if s.refuseIfLocked(w, app) {
		return
	}
	// appID, not just reqID: the grant must land on the app this caller was
	// authorized against.
	granted, err := s.db.ApproveObjectAccessRequest(r.Context(), reqID, appID, p.ID, requestMeta(r.Context()))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/apps/" + chi.URLParam(r, "appID") + "/access-requests lists the requests on this app"})
			return
		}
		if errors.Is(err, database.ErrRequestNotPending) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "request cannot be approved", "hint": "GET /api/v1/apps/" + chi.URLParam(r, "appID") + "/access-requests shows whether it is still pending"})
			return
		}
		// Anything else is this install's failure, not an "already decided".
		log.Printf("approve access request %s: %v", reqID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the request could not be approved"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
	// The role that was actually written, not the one that was asked for. They
	// differ when the requester already held more, and a message naming the
	// smaller one would tell somebody they had been demoted when nothing moved.
	s.notifyAccessDecided(app, reqID, granted, true)
}

func (s *Server) handleDenyAccessRequest(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	reqID := chi.URLParam(r, "reqID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionApproveAccessRequest, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return
	}
	var body denyAccessRequestBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.db.DenyObjectAccessRequest(r.Context(), reqID, appID, p.ID, body.Reason, requestMeta(r.Context())); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/apps/" + chi.URLParam(r, "appID") + "/access-requests lists the requests on this app"})
			return
		}
		log.Printf("access request update: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
	// No role: nothing was granted. The reason travels on the row, which this
	// has just written, so the notifier reads it back rather than being handed
	// a copy that could disagree with what is stored.
	s.notifyAccessDecided(app, reqID, "", false)
}

func (s *Server) handleWithdrawAccessRequest(w http.ResponseWriter, r *http.Request) {
	reqID := chi.URLParam(r, "reqID")
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	// The request has to be this app's: the URL's app id scopes the lookup,
	// and a request on another app, or no app, is not found here. One answer
	// with the withdraw's own, so neither tells a caller which ids exist.
	req, err := s.db.GetAccessRequest(reqID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("access request lookup: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if req == nil || req.ObjectID != chi.URLParam(r, "appID") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/me/access-requests lists your requests"})
		return
	}
	if err := s.db.WithdrawAccessRequest(r.Context(), reqID, p.ID, requestMeta(r.Context())); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/me/access-requests lists your requests"})
			return
		}
		log.Printf("access request update: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
