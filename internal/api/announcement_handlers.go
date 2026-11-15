package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"toft/internal/auth"
	"toft/internal/database"
)

const announcementKey = "announcement"
const announcementMaxLen = 500

type announcementPayload struct {
	Message string `json:"message"`
	Level   string `json:"level"` // "info" | "warning" | "critical"
}

type announcementResponse struct {
	Message   string `json:"message"`
	Level     string `json:"level"`
	UpdatedAt string `json:"updated_at,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

func validLevel(l string) bool {
	return l == "info" || l == "warning" || l == "critical"
}

// GET /api/v1/announcement — readable by anyone (including anonymous). Returns
// {message:""} when none set so the UI can clear any dismissed state.
func (s *Server) handleGetAnnouncement(w http.ResponseWriter, r *http.Request) {
	setting, err := s.db.GetSetting(announcementKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if setting == nil || setting.Value == "" {
		writeJSON(w, http.StatusOK, announcementResponse{})
		return
	}
	var payload announcementPayload
	if err := json.Unmarshal([]byte(setting.Value), &payload); err != nil {
		writeJSON(w, http.StatusOK, announcementResponse{})
		return
	}
	resp := announcementResponse{
		Message:   payload.Message,
		Level:     payload.Level,
		UpdatedAt: setting.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	// Who set it is for the administrators' editor only. The route is open to
	// the anonymous internet, and a banner's reader has no use for an
	// administrator's account name.
	if p := auth.GetPrincipal(r.Context()); p != nil && p.SystemRole == database.SystemRoleAdmin {
		resp.UpdatedBy = setting.UpdatedBy
	}
	writeJSON(w, http.StatusOK, resp)
}

// PUT /api/v1/admin/announcement — system_admin only.
func (s *Server) handlePutAnnouncement(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to change the announcement"})
		return
	}

	var req announcementPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message required", "hint": `send the banner text, as {"message": "Maintenance tonight from 8pm", "level": "info"}`})
		return
	}
	if len(req.Message) > announcementMaxLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message too long", "hint": "shorten the message to " + strconv.Itoa(announcementMaxLen) + " characters or fewer and send it again"})
		return
	}
	if req.Level == "" {
		req.Level = "info"
	}
	if !validLevel(req.Level) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid level", "hint": `send "level": "info", "warning" or "critical"`})
		return
	}

	// Recorded, because `event_handlers.go` has offered an `announcement.*`
	// filter since it was written and nothing has ever written one — so an
	// administrator looking for who put a line across every page on this
	// install found a family that is always empty. The before is what it
	// replaced, which is the question a banner audit is actually asked.
	var before announcementPayload
	if prev, err := s.db.GetSetting(announcementKey); err == nil && prev != nil {
		_ = json.Unmarshal([]byte(prev.Value), &before)
	}
	entry := auditEntryFor(r)
	entry.ActorType = database.PrincipalTypeUser
	entry.ActorID = p.ID
	entry.Action = "announcement.set"
	entry.TargetType = "platform"
	entry.TargetID = announcementKey
	if before.Message != "" {
		entry.Before = jsonValStr(before)
	}
	entry.After = jsonValStr(req)

	raw, _ := json.Marshal(req)
	setting, err := s.db.PutSettingAudited(announcementKey, string(raw), entry)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, announcementResponse{
		Message:   req.Message,
		Level:     req.Level,
		UpdatedAt: setting.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedBy: setting.UpdatedBy,
	})
}

// DELETE /api/v1/admin/announcement — system_admin only.
func (s *Server) handleDeleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to change the announcement"})
		return
	}
	var before announcementPayload
	if prev, err := s.db.GetSetting(announcementKey); err == nil && prev != nil {
		_ = json.Unmarshal([]byte(prev.Value), &before)
	}
	entry := auditEntryFor(r)
	entry.ActorType = database.PrincipalTypeUser
	entry.ActorID = p.ID
	entry.Action = "announcement.clear"
	entry.TargetType = "platform"
	entry.TargetID = announcementKey
	if before.Message != "" {
		entry.Before = jsonValStr(before)
	}

	if err := s.db.DeleteSettingAudited(announcementKey, entry); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
