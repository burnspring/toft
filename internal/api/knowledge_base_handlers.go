package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"toft/internal/auth"
	"toft/internal/database"
)

// The install's own knowledge base: one app an administrator points everybody
// at, reached from a word in the header rather than from the apps list.
//
// **The whole feature is a settings row and a link**, and the two things that
// are not obvious are both here rather than in the frontend.
//
// The first is disclosure. An administrator picks an app; the pointer is then
// rendered to other people. `auth.Authorize` hands a system_admin an
// owner-shaped answer on every app (internal/auth/authorize.go), so the one
// person doing the picking is the one person who cannot tell by looking whether
// the pick resolves for anybody else — and a pointer that named a private app
// in everyone's chrome would disclose that app to the whole install. So
// resolution asks the read gate, per caller, and **"there is no knowledge
// base", "the app it named is gone" and "you may not read it" are the same
// bytes**: `{}`. A distinguishable answer turns this route into an oracle for
// "does this install have an app called X".
//
// The second is that **nothing here reads apps.framework**. The obvious
// implementation of "pick a static app to hold the handbook" filters the picker
// to the static family, and it is wrong: `static` is what the detector
// concluded, not what an app is for, and the disqualifier list in
// internal/builder/detector.go means a Docusaurus or MkDocs handbook is never
// detected as static from source. What decides whether an app may be picked is
// what already decides whether anybody may read it, and there is no second
// qualification for this to get wrong.
const knowledgeBaseKey = "knowledge_base"

// The header row does not wrap. Two words is the guidance; this is the cap
// that keeps a paragraph out of the chrome.
const knowledgeBaseLabelMaxLen = 32

// The description is the sentence under the label on the Docs page, and it is
// the one piece of this feature an install can put its own words in — "kept on
// this install" is true everywhere and useful nowhere in particular. It is
// optional: blank means the client renders the product's own sentence, which
// is why an install that never touches this reads exactly as it did before.
//
// The cap is larger than the label's because this one has a card to sit in and
// two lines to wrap onto, and small enough that it cannot become the page.
const knowledgeBaseDescriptionMaxLen = 200

// knowledgeBasePick is what is stored under the key. It holds the app **id**
// rather than the slug, because a rename is a supported operation and a stored
// slug would go stale silently — the link would 404 on the gateway and report
// that 404 as the app's own answer. It is not a URL, because the address of an
// app has one construction per side of the wire and a stored one would be a
// third.
type knowledgeBasePick struct {
	AppID string `json:"app_id"`
	Label string `json:"label"`
	// Optional, and omitted when blank so a pick saved before this field
	// existed round-trips unchanged rather than growing an empty string.
	Description string `json:"description,omitempty"`
}

// knowledgeBaseResponse is what a signed-in caller is told. Every field is
// omitempty so that "no knowledge base" serialises as `{}` — and so does every
// other negative answer.
//
// The slug is returned rather than a URL: the SPA has its own single
// construction in web/src/lib/app-url.ts, which is already correct on a
// single-origin install where /api/v1/config deliberately reports no apps
// origin. An absolute URL from here would hardcode a host into a link this
// product goes out of its way to keep relative.
type knowledgeBaseResponse struct {
	AppID       string `json:"app_id,omitempty"`
	Slug        string `json:"slug,omitempty"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// adminKnowledgeBaseResponse is the administrator's view of the same row, and
// it is a different shape on purpose: it answers with what is *stored*, so the
// page that owns the pick can tell "nothing is set" from "this names an app
// that no longer exists". The resolving route cannot answer that question
// without becoming the oracle described above.
type adminKnowledgeBaseResponse struct {
	AppID string `json:"app_id"`
	Label string `json:"label"`
	// Not omitempty: this shape answers with what is *stored*, and the page
	// that owns the pick has to be able to tell an empty description from an
	// absent field to seed its form.
	Description string `json:"description"`
}

// readKnowledgeBasePick returns the stored pick, or nil when the key has never
// been set or holds something unreadable. A corrupt value is treated as no
// pick rather than as an error: the failure mode of a header item that cannot
// render should be an absent header item.
func (s *Server) readKnowledgeBasePick() (*knowledgeBasePick, error) {
	setting, err := s.db.GetSetting(knowledgeBaseKey)
	if err != nil {
		return nil, err
	}
	if setting == nil || setting.Value == "" {
		return nil, nil
	}
	var pick knowledgeBasePick
	if err := json.Unmarshal([]byte(setting.Value), &pick); err != nil {
		return nil, nil
	}
	if pick.AppID == "" {
		return nil, nil
	}
	return &pick, nil
}

// GET /api/v1/knowledge-base — the pointer, resolved for this caller.
//
// Signed-in callers only, and that is a decision rather than an omission. The
// ask is "available to all logged in users"; the only surfaces an anonymous
// visitor sees are the landing page and the login form, neither of which is a
// place for an organization's handbook; and whether a `public` app is *actually*
// readable anonymously is a moving target, because the install-wide switch
// subtracts anonymity without touching apps.visibility. Refusing anonymously by construction means this route has no
// opinion left to get wrong.
//
// `{}` rather than 204 so the client can hold it on a query key and clear stale
// state, which is why the announcement returns an empty message rather than
// nothing.
func (s *Server) handleGetKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.ID == "" {
		writeJSON(w, http.StatusOK, knowledgeBaseResponse{})
		return
	}

	pick, err := s.readKnowledgeBasePick()
	if err != nil {
		log.Printf("knowledge base: reading setting: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if pick == nil {
		writeJSON(w, http.StatusOK, knowledgeBaseResponse{})
		return
	}

	app, err := s.db.GetApp(pick.AppID)
	if err != nil {
		log.Printf("knowledge base: loading app %s: %v", pick.AppID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// A dangling pick — the app was deleted and the settings table has no
	// foreign key to have noticed. Answered as no pick, and never repaired
	// here: nothing is written on a read path. The administrator's page says
	// it is dangling and offers to clear it.
	if app == nil {
		writeJSON(w, http.StatusOK, knowledgeBaseResponse{})
		return
	}

	// The one gate, and the same one the app detail page uses. It routes
	// through database.AuthObjectOf, so the anonymity suppressors carried on
	// the app row are already accounted for.
	//
	// Locked, stopped and asleep are deliberately not consulted. A locked app
	// answers with the administrator's words, which is the entire point of that
	// copy and is unreachable if the link disappears; a sleeping app is woken by
	// the gateway as it is for anybody else.
	if d := auth.Authorize(p, auth.ActionRead, objectFromApp(app), s.db); !d.Allow {
		writeJSON(w, http.StatusOK, knowledgeBaseResponse{})
		return
	}

	writeJSON(w, http.StatusOK, knowledgeBaseResponse{
		AppID:       app.ID,
		Slug:        app.Slug,
		Label:       pick.Label,
		Description: pick.Description,
	})
}

// GET /api/v1/admin/knowledge-base — what is stored, for the page that sets it.
func (s *Server) handleGetAdminKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	if !s.knowledgeBaseAdmin(w, r) {
		return
	}
	pick, err := s.readKnowledgeBasePick()
	if err != nil {
		log.Printf("knowledge base: reading setting: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if pick == nil {
		writeJSON(w, http.StatusOK, adminKnowledgeBaseResponse{})
		return
	}
	writeJSON(w, http.StatusOK, adminKnowledgeBaseResponse{AppID: pick.AppID, Label: pick.Label, Description: pick.Description})
}

// PUT /api/v1/admin/knowledge-base — system_admin only.
func (s *Server) handlePutKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	if !s.knowledgeBaseAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())

	var req knowledgeBasePick
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	req.AppID = strings.TrimSpace(req.AppID)
	req.Label = strings.TrimSpace(req.Label)
	req.Description = strings.TrimSpace(req.Description)
	if req.AppID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "app_id required", "hint": `send the app to link, as {"app_id": "…", "label": "Handbook"} — GET /api/v1/apps lists the ids`})
		return
	}
	if req.Label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "label required", "hint": `send a short label, as "label": "Handbook"`})
		return
	}
	if len([]rune(req.Label)) > knowledgeBaseLabelMaxLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "label too long", "hint": "shorten the label to " + strconv.Itoa(knowledgeBaseLabelMaxLen) + " characters or fewer"})
		return
	}
	// Blank is allowed and means "use the product's sentence". Only the cap is
	// enforced, because what an install says about its own handbook is not
	// something this server is in a position to have an opinion about.
	if len([]rune(req.Description)) > knowledgeBaseDescriptionMaxLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "description too long", "hint": "shorten the description to " + strconv.Itoa(knowledgeBaseDescriptionMaxLen) + " characters or fewer, or leave it empty"})
		return
	}

	// The app has to exist at the moment it is picked. Nothing else about it is
	// asked — not its framework, not its status, not its visibility. What it is
	// set to is the administrator's business and the page says what the
	// consequence is; refusing the save would be this feature inventing a
	// second gate.
	app, err := s.db.GetApp(req.AppID)
	if err != nil {
		log.Printf("knowledge base: loading app %s: %v", req.AppID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if app == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no such app", "hint": "GET /api/v1/apps lists the apps with their ids"})
		return
	}

	before, err := s.readKnowledgeBasePick()
	if err != nil {
		log.Printf("knowledge base: reading setting: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	raw, _ := json.Marshal(req)
	entry := auditEntryFor(r)
	entry.ActorType = database.PrincipalTypeUser
	entry.ActorID = p.ID
	entry.Action = "knowledge_base.set"
	entry.TargetType = "app"
	entry.TargetID = app.ID
	if before != nil {
		entry.Before = jsonValStr(before)
	}
	entry.After = jsonValStr(req)

	if _, err := s.db.PutSettingAudited(knowledgeBaseKey, string(raw), entry); err != nil {
		log.Printf("knowledge base: saving: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, adminKnowledgeBaseResponse{AppID: req.AppID, Label: req.Label, Description: req.Description})
}

// DELETE /api/v1/admin/knowledge-base — system_admin only. Clearing one that
// was never set is a no-op and still writes the row.
func (s *Server) handleDeleteKnowledgeBase(w http.ResponseWriter, r *http.Request) {
	if !s.knowledgeBaseAdmin(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())

	before, err := s.readKnowledgeBasePick()
	if err != nil {
		log.Printf("knowledge base: reading setting: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	entry := auditEntryFor(r)
	entry.ActorType = database.PrincipalTypeUser
	entry.ActorID = p.ID
	entry.Action = "knowledge_base.clear"
	entry.TargetType = "platform"
	entry.TargetID = knowledgeBaseKey
	if before != nil {
		entry.Before = jsonValStr(before)
		entry.TargetType = "app"
		entry.TargetID = before.AppID
	}

	if err := s.db.DeleteSettingAudited(knowledgeBaseKey, entry); err != nil {
		log.Printf("knowledge base: clearing: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// knowledgeBaseAdmin is the gate the three administrator routes share. Each
// handler checks for itself rather than sitting behind a middleware, which is
// how the announcement's routes are written and what the authorization
// invariant asks for.
func (s *Server) knowledgeBaseAdmin(w http.ResponseWriter, r *http.Request) bool {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to change the knowledge base"})
		return false
	}
	return true
}
