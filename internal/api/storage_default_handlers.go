package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// The install's default delegations (ADR § Storage somebody else can attach):
// Admin › Storage's list of subjects every resource is delegated to when it is
// created. Administrator only, and refused to an API key — they change who may
// do what.

func (s *Server) handleListStorageDefaults(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	defs, err := s.db.ListStorageDefaultManagers()
	if err != nil {
		log.Printf("listing storage defaults: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, defs)
}

type putStorageDefaultRequest struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	MaxAccess   string `json:"max_access"`
	// Access is a grant's field, read only to name it in the refusal (D-02).
	Access string `json:"access"`
	// ApplyExisting also writes the delegation on every resource where the
	// subject has none, and reports the ones where it has one at another
	// ceiling — left alone.
	ApplyExisting bool `json:"apply_existing"`
}

func (s *Server) handlePutStorageDefault(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	var req putStorageDefaultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request", "hint": badBodyHint})
		return
	}
	if req.SubjectType != database.StoreSubjectUser && req.SubjectType != database.StoreSubjectGroup {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "subject_type is user or group", "hint": `send "subject_type": "user" or "group"`})
		return
	}
	if strings.TrimSpace(req.MaxAccess) == "" {
		missingMaxAccess(w, req.Access, []string{"read", "full"})
		return
	}
	if !database.ValidDefaultAccess(req.MaxAccess) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "a default is read or full — the two levels every kind of storage has", "hint": `send "max_access": "read" or "full"`})
		return
	}
	// A claim-mapped group is accepted: a default is a delegation, and a
	// delegation's subject is any user or group that exists.
	if !s.storeSubjectExists(req.SubjectType, req.SubjectID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such " + req.SubjectType, "hint": "GET /api/v1/users lists user ids and GET /api/v1/groups lists group ids"})
		return
	}
	meta := requestMeta(r.Context())
	d := &database.StorageDefaultManager{SubjectType: req.SubjectType, SubjectID: req.SubjectID, MaxAccess: req.MaxAccess}
	if err := s.db.PutStorageDefaultManager(d, p.ID, meta); err != nil {
		log.Printf("putting a storage default: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	body := map[string]any{"default": d}
	if req.ApplyExisting {
		report, err := s.db.ApplyStorageDefault(req.SubjectType, req.SubjectID, req.MaxAccess, p.ID, meta)
		if err != nil {
			log.Printf("applying a storage default: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		body["applied"] = report.Applied
		body["differing"] = report.Differing
	}
	writeJSON(w, http.StatusOK, body)
}

// handleDeleteStorageDefault stops future creates delegating to the subject.
// With revoke_existing=true it also revokes every delegation a default wrote
// for that subject — one resource at a time through the revocation, which
// collects their mappings — and restarts the union of affected apps, the same
// restart a single revoke makes.
func (s *Server) handleDeleteStorageDefault(w http.ResponseWriter, r *http.Request) {
	if !s.requireStorageAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())
	meta := requestMeta(r.Context())
	// Decoded first: the SPA sends the subject percent-encoded, chi hands the
	// raw segment back, and "user%3A<id>" had no colon to cut on — every
	// removal from Admin › Storage was a 400 (code review 2026-11-11, N-1).
	subject := chi.URLParam(r, "subject")
	if decoded, err := url.PathUnescape(subject); err == nil {
		subject = decoded
	}
	subjectType, subjectID, found := strings.Cut(subject, ":")
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": `the subject is "user:<id>" or "group:<id>"`, "hint": "name it in the path as user:<id> or group:<id>"})
		return
	}
	revokeExisting := r.URL.Query().Get("revoke_existing") == "true"
	if err := s.db.DeleteStorageDefaultManager(subjectType, subjectID, p.ID, meta); err != nil {
		// A default already gone is not a refusal when the revocations were
		// asked for: the default row is deleted first and each revoke is its
		// own transaction, so "run it again" after one failed was a 404 that
		// left the rest standing (code review 2026-11-11, B-7).
		if errors.Is(err, sql.ErrNoRows) && !revokeExisting {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/storage/defaults lists the defaults that are set"})
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("deleting a storage default: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	body := map[string]any{"revoked": 0}
	if revokeExisting {
		resources, err := s.db.DefaultDelegationResources(subjectType, subjectID)
		if err != nil {
			log.Printf("listing default delegations: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		seen := map[string]bool{}
		var affected []database.StorageManagedApp
		for _, id := range resources {
			apps, err := s.db.DeleteStorageManagerAndMappings(id, subjectType, subjectID,
				p.ID, meta.RequestID, meta.IP, meta.UserAgent)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				// Stop rather than press on, and say how far it got: each
				// revoke is its own transaction, so what is done is done.
				log.Printf("revoking default delegation on %s: %v", id, err)
				s.supervisor.Remount(restartTargets(affected), "a default storage delegation was revoked")
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error":      "revoked some of this default's delegations and then failed — run it again",
					"restarting": affected})
				return
			}
			for _, a := range apps {
				if !seen[a.ID] {
					seen[a.ID] = true
					affected = append(affected, a)
				}
			}
		}
		s.supervisor.Remount(restartTargets(affected), "a default storage delegation was revoked")
		body["revoked"] = len(resources)
		body["restarting"] = affected
		body["note"] = revocationNote(affected)
	}
	writeJSON(w, http.StatusOK, body)
}
