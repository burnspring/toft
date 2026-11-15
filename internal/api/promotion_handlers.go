package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

func (s *Server) handleCreatePromotionRequest(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.Type != database.PrincipalTypeUser {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "service accounts cannot request promotion", "hint": "sign in as a person to ask for the publisher role; an administrator changes a service account's role"})
		return
	}
	if p.SystemRole != database.SystemRoleViewer {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "only viewers may request promotion", "hint": "nothing to request — your role is already above viewer; GET /api/v1/me/account shows it"})
		return
	}
	req := &database.AccessRequest{
		RequesterID:   p.ID,
		RequestType:   database.AccessReqTypePromotion,
		RequestedRole: database.SystemRolePublisher,
	}
	if err := s.db.CreateAccessRequest(r.Context(), req, false, "", requestMeta(r.Context())); err != nil {
		if errors.Is(err, database.ErrDuplicateRequest) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "a pending request already exists", "hint": "wait for an administrator to decide it; GET /api/v1/me/promotion-requests shows it, and DELETE withdraws it"})
			return
		}
		log.Printf("creating promotion request: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Notify after the request is durable, and never let notification failure
	// touch the response: the requester's ask has landed either way.
	if requester, err := s.db.GetUser(p.ID); err == nil && requester != nil {
		s.notifyPromotionRequested(req, requester)
	}

	writeJSON(w, http.StatusCreated, req)
}

// handleListMyPromotionRequests answers "what did I ask for, and what happened
// to it". Scoped to the caller by the query, not by a filter the handler could
// forget.
func (s *Server) handleListMyPromotionRequests(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	out, err := s.db.ListPromotionRequestsForUser(p.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	if out == nil {
		out = []database.PromotionRequestDetail{}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWithdrawMyPromotionRequest takes back a pending request. Scoped to the
// caller as requester inside the UPDATE — anything else would let one person
// withdraw another's request by id.
func (s *Server) handleWithdrawMyPromotionRequest(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	reqID := chi.URLParam(r, "reqID")
	if err := s.db.WithdrawAccessRequest(r.Context(), reqID, p.ID, requestMeta(r.Context())); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Covers "no such request", "not yours", and "already decided"
			// with one answer, deliberately: distinguishing them would tell a
			// caller which request ids exist.
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/me/promotion-requests lists your requests with their ids"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListPromotionRequests(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "sign in as an administrator to read promotion requests; GET /api/v1/me/promotion-requests shows your own"})
		return
	}
	// Pending by default: the queue is what an administrator came for, and the
	// history is a filter away.
	status := r.URL.Query().Get("status")
	if status == "" {
		status = database.AccessReqStatusPending
	}
	switch status {
	case database.AccessReqStatusPending, database.AccessReqStatusApproved,
		database.AccessReqStatusDenied, database.AccessReqStatusWithdrawn,
		database.AccessReqStatusAll:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "status must be pending, approved, denied, withdrawn, or all", "hint": `pass status as pending, approved, denied, withdrawn or all, like ?status=pending`})
		return
	}
	out, err := s.db.ListPromotionRequests(status)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	if out == nil {
		out = []database.PromotionRequestDetail{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleApprovePromotionRequest(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "sign in as an administrator to approve promotion requests"})
		return
	}
	reqID := chi.URLParam(r, "reqID")
	if err := s.db.ApprovePromotionRequest(r.Context(), reqID, p.ID, requestMeta(r.Context())); err != nil {
		s.writePromotionDecisionError(w, reqID, err)
		return
	}
	s.notifyPromotionDecided(reqID, true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDenyPromotionRequest(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "sign in as an administrator to deny promotion requests"})
		return
	}
	reqID := chi.URLParam(r, "reqID")
	var body denyAccessRequestBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.db.DenyPromotionRequest(r.Context(), reqID, p.ID, body.Reason, requestMeta(r.Context())); err != nil {
		s.writePromotionDecisionError(w, reqID, err)
		return
	}
	s.notifyPromotionDecided(reqID, false)
	w.WriteHeader(http.StatusNoContent)
}

// writePromotionDecisionError maps a decision's failure to a status the
// server can stand behind: 404 for a request that does not exist, 409 for the
// one refusal the database makes on purpose — it was already decided — and
// 500, logged, for everything else. The driver's own sentence never reaches
// the client: a dropped connection rendered as "already decided" is a request
// the queue page then lets somebody approve again.
func (s *Server) writePromotionDecisionError(w http.ResponseWriter, reqID string, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/promotion-requests lists the requests with their ids"})
	case errors.Is(err, database.ErrRequestNotPending):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "that request has already been decided", "hint": "GET /api/v1/admin/promotion-requests?status=all shows how it was decided; nothing more to do"})
	default:
		log.Printf("deciding promotion request %s: %v", reqID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}
