package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/tags"
)

const (
	// maxTagsPerApp bounds one request's work, mirroring maxCronJobsPerApp.
	maxTagsPerApp = 20
	// maxTagValueLen keeps values chip-sized; they are labels, not prose.
	maxTagValueLen = 64
	maxTagNameLen  = 40
	// auditedAppIDs bounds the app list recorded when a tag is force-deleted.
	auditedAppIDs = 100
)

// tagPalette is the allowed set of chip colors. An allowlist rather than free
// hex: admin input stays bounded and chips stay inside the design system.
var tagPalette = map[string]bool{
	"indigo": true, "cyan": true, "fuchsia": true, "green": true, "amber": true,
	"red": true, "violet": true, "orange": true, "slate": true,
}

// tagSlugRe strips anything that can't appear in a URL path segment.
var tagSlugRe = slugRe

// tagSlug derives a URL-safe slug from arbitrary text. Unlike sanitizeSlug
// (shared with app creation) it has no "app" fallback — a value that slugs to
// nothing is rejected by the caller instead.
func tagSlug(s string) string {
	s = tagSlugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

// scopeFor builds the app-visibility scope for a principal. system_admin sees
// every app, matching the ListAllApps path used elsewhere.
func scopeFor(p *auth.Principal) database.AppScope {
	if p == nil {
		return database.AppScope{}
	}
	if p.SystemRole == database.SystemRoleAdmin {
		return database.AppScope{All: true}
	}
	// A deploy key acts as owner on every app (auth.Authorize step 1a), so the
	// listing has to say so. Without this it has no user id and no groups, the
	// scoped query matches nothing, and `GET /apps` — which is on the class's
	// allow-list — answers `[]` on an install full of apps: a route that is
	// permitted and useless, and worse than a refusal because an empty list
	// reads as an answer. The single-object reads beside it already grant this,
	// so nothing here widens the class; it stops one route from lying about it.
	if p.IsDeployKey() {
		return database.AppScope{All: true}
	}
	return database.AppScope{PrincipalID: p.ID, PrincipalType: p.Type, Groups: p.Groups}
}

// requireAdmin is the system_admin gate. There is no admin middleware — each
// handler checks for itself, as in announcement_handlers.go.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil
	}
	if p.SystemRole != database.SystemRoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden", "hint": "ask an administrator to make this change"})
		return nil
	}
	return p
}

// GET /api/v1/tags — the vocabulary, readable by anyone. Counts are scoped to
// the caller so they can't be used to probe for private apps.
func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())

	list, err := s.db.ListTags(scopeFor(p))
	if err != nil {
		log.Printf("Error listing tags: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if list == nil {
		list = []database.Tag{}
	}
	writeJSON(w, http.StatusOK, list)
}

// GET /api/v1/tags/{tagSlug}/values?q= — values in use, for the picker's
// suggestions. Scoped: unscoped, the autocomplete would reveal values that only
// exist on apps the caller cannot see.
func (s *Server) handleListTagValues(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())

	tag, err := s.db.GetTagBySlug(chi.URLParam(r, "tagSlug"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if tag == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tag not found", "hint": "GET /api/v1/tags lists the tags"})
		return
	}

	scope := scopeFor(p)
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	// A system tag's values are derived from the app column, so they come from
	// the registry with counts computed over readable apps — never from app_tags.
	if tag.Kind == database.TagKindSystem {
		counts, err := s.db.FrameworkValueCounts(scope)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		out := []database.TagValue{}
		for _, v := range tags.FrameworkValues() {
			n, inUse := counts[v.Slug]
			if !inUse {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(v.Value), strings.ToLower(q)) {
				continue
			}
			out = append(out, database.TagValue{Value: v.Value, Slug: v.Slug, AppCount: n})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	values, err := s.db.ListTagValues(tag.ID, q, scope)
	if err != nil {
		log.Printf("Error listing tag values: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if values == nil {
		values = []database.TagValue{}
	}
	writeJSON(w, http.StatusOK, values)
}

type createTagRequest struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// POST /api/v1/admin/tags — system_admin only.
func (s *Server) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}

	var req createTagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required", "hint": `send "name", like {"name": "Department"}`})
		return
	}
	if len(req.Name) > maxTagNameLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name too long", "hint": fmt.Sprintf("shorten name to %d characters or fewer", maxTagNameLen)})
		return
	}
	if req.Color == "" {
		req.Color = "indigo"
	}
	if !tagPalette[req.Color] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid color", "hint": `send "color" as one of indigo, cyan, fuchsia, green, amber, red, violet, orange or slate`})
		return
	}

	slug, err := s.uniqueTagSlug(req.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name must contain letters or digits", "hint": `send a "name" with at least one letter or digit, like "Department"`})
		return
	}

	tag := &database.Tag{
		ID: uuid.New().String(), Name: req.Name, Slug: slug,
		Kind: database.TagKindCustom, Color: req.Color,
		Description: strings.TrimSpace(req.Description), CreatedBy: p.ID,
	}

	tx, err := s.db.Conn().Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer tx.Rollback()

	if err := s.db.CreateTagTx(tx, tag); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "a tag with that name already exists", "code": "name_taken", "hint": "choose another name — GET /api/v1/tags lists the tags in use"})
			return
		}
		log.Printf("Error creating tag: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := s.auditTag(tx, r, p, "tag.create", tag.ID, "", jsonValStr(tag)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, tag)
}

// uniqueTagSlug derives a slug and suffixes it until it is free.
func (s *Server) uniqueTagSlug(name string) (string, error) {
	base := tagSlug(name)
	if base == "" {
		return "", nil
	}
	candidate := base
	for i := 2; i < 100; i++ {
		taken, err := s.db.TagSlugExists(candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return uuid.New().String()[:8], nil
}

type updateTagRequest struct {
	Name        *string `json:"name"`
	Slug        *string `json:"slug"`
	Color       *string `json:"color"`
	Description *string `json:"description"`
}

// PATCH /api/v1/admin/tags/{tagID} — system_admin only.
//
// Renaming deliberately leaves the slug alone; a caller that wants the
// permalink changed must ask for it explicitly, having been warned that
// existing /tags/{slug} links will stop resolving.
func (s *Server) handleUpdateTag(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}

	tag := s.mutableTag(w, r)
	if tag == nil {
		return
	}
	before := *tag

	var req updateTagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name required", "hint": `send "name", like {"name": "Department"}`})
			return
		}
		if len(name) > maxTagNameLen {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name too long", "hint": fmt.Sprintf("shorten name to %d characters or fewer", maxTagNameLen)})
			return
		}
		tag.Name = name
	}
	if req.Slug != nil {
		slug := tagSlug(*req.Slug)
		if slug == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid slug", "hint": `send a "slug" with at least one letter or digit, like "department"`})
			return
		}
		tag.Slug = slug
	}
	if req.Color != nil {
		if !tagPalette[*req.Color] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid color", "hint": `send "color" as one of indigo, cyan, fuchsia, green, amber, red, violet, orange or slate`})
			return
		}
		tag.Color = *req.Color
	}
	if req.Description != nil {
		tag.Description = strings.TrimSpace(*req.Description)
	}

	tx, err := s.db.Conn().Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer tx.Rollback()

	if err := s.db.UpdateTagTx(tx, tag); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "a tag with that name or slug already exists", "code": "name_taken", "hint": "choose another name or slug — GET /api/v1/tags lists the ones in use"})
			return
		}
		log.Printf("Error updating tag: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := s.auditTag(tx, r, p, "tag.update", tag.ID, jsonValStr(before), jsonValStr(tag)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, tag)
}

// DELETE /api/v1/admin/tags/{tagID}?force=true — system_admin only.
//
// A tag in use answers 409 with the count rather than silently stripping itself
// from every app; ?force=true is the acknowledgement, and the cascade then does
// the removal.
func (s *Server) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}

	tag := s.mutableTag(w, r)
	if tag == nil {
		return
	}

	inUse, err := s.db.CountAppsWithTag(tag.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if inUse > 0 && r.URL.Query().Get("force") != "true" {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error": fmt.Sprintf("tag is set on %d apps", inUse),
			"apps":  inUse,
			"hint":  "add ?force=true to delete it and take it off those apps",
		})
		return
	}

	appIDs, err := s.db.AppIDsWithTag(tag.ID, auditedAppIDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	tx, err := s.db.Conn().Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer tx.Rollback()

	if err := s.db.DeleteTagTx(tx, tag.ID); err != nil {
		log.Printf("Error deleting tag: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	before := jsonValStr(map[string]interface{}{
		"tag": tag, "app_count": inUse, "app_ids": appIDs,
	})
	if err := s.auditTag(tx, r, p, "tag.delete", tag.ID, before, ""); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mutableTag loads the tag named in the URL and refuses platform-owned ones.
// System tags have no editable identity: their name and values come from the
// registry, so changing them means changing code.
func (s *Server) mutableTag(w http.ResponseWriter, r *http.Request) *database.Tag {
	tag, err := s.db.GetTag(chi.URLParam(r, "tagID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil
	}
	if tag == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tag not found", "hint": "GET /api/v1/tags lists the tags"})
		return nil
	}
	if tag.Kind == database.TagKindSystem {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "system tags cannot be modified", "hint": "create a tag of your own with POST /api/v1/admin/tags instead"})
		return nil
	}
	return tag
}

func (s *Server) auditTag(tx *database.Tx, r *http.Request, p *auth.Principal, action, tagID, before, after string) error {
	meta := requestMeta(r.Context())
	return database.WriteAuditTx(tx, database.AuditEntry{
		ActorType: p.Type, ActorID: p.ID,
		Action: action, TargetType: "tag", TargetID: tagID,
		Before: before, After: after,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	})
}

type setAppTagsRequest struct {
	// Tags maps a tag slug to the value the app should carry. An empty value
	// clears the tag. Slugs left out of the map are untouched, so the UI can
	// set one tag without resending the rest.
	Tags map[string]string `json:"tags"`
}

// PUT /api/v1/apps/{appID}/tags — owner/editor only (ActionEditContent).
func (s *Server) handleSetAppTags(w http.ResponseWriter, r *http.Request) {
	app := s.editableApp(w, r)
	if app == nil {
		return
	}
	p := auth.GetPrincipal(r.Context())

	var req setAppTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	if len(req.Tags) > maxTagsPerApp {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "too many tags", "hint": fmt.Sprintf("send at most %d tags", maxTagsPerApp)})
		return
	}

	// Resolve and validate every entry before writing any of them, so a bad
	// slug halfway through can't leave the app half-updated.
	type change struct {
		tag       *database.Tag
		value     string
		valueSlug string
		clear     bool
	}
	changes := make([]change, 0, len(req.Tags))

	for slug, raw := range req.Tags {
		tag, err := s.db.GetTagBySlug(slug)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if tag == nil {
			// A code of its own, 400 like every other unknown_* name a caller
			// sent: the envelope's generic `invalid` at 422 left a CLI caller
			// at exit 1 for what is a usage error (D-106).
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("unknown tag %q", slug),
				"code":  "unknown_tag",
				"hint":  "send tags by their slug — GET /api/v1/tags lists the tags you can set; an administrator creates new ones",
			})
			return
		}
		if tag.Kind == database.TagKindSystem {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": fmt.Sprintf("%s is assigned automatically and cannot be set", tag.Name),
				"hint":  "leave it out, since Toft sets it — GET /api/v1/tags lists the tags you can set",
			})
			return
		}

		value := strings.TrimSpace(raw)
		if value == "" {
			changes = append(changes, change{tag: tag, clear: true})
			continue
		}
		if len(value) > maxTagValueLen {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("value for %s is too long", tag.Name),
				"hint":  fmt.Sprintf("shorten the value to %d characters or fewer", maxTagValueLen),
			})
			return
		}
		valueSlug := tagSlug(value)
		if valueSlug == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("value for %s must contain letters or digits", tag.Name),
				"hint":  "send a value with at least one letter or digit",
			})
			return
		}
		// Snap to the spelling already in use for this slug, so "engineering"
		// joins "Engineering" instead of forking the vocabulary.
		canonical, err := s.db.CanonicalValue(tag.ID, valueSlug)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if canonical != "" {
			value = canonical
		}
		changes = append(changes, change{tag: tag, value: value, valueSlug: valueSlug})
	}

	existing, err := s.db.TagsForApp(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	previous := map[string]string{}
	for _, t := range existing {
		previous[t.TagID] = t.Value
	}

	tx, err := s.db.Conn().Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	defer tx.Rollback()

	meta := requestMeta(r.Context())
	for _, c := range changes {
		was := previous[c.tag.ID]
		if c.clear {
			if was == "" {
				continue // nothing to clear; keep the audit log free of no-ops
			}
			if err := s.db.ClearAppTagTx(tx, app.ID, c.tag.ID); err != nil {
				log.Printf("Error clearing app tag: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		} else {
			if was == c.value {
				continue
			}
			if err := s.db.SetAppTagTx(tx, app.ID, c.tag.ID, c.value, c.valueSlug, p.ID); err != nil {
				log.Printf("Error setting app tag: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}

		action := "app.tag_set"
		if c.clear {
			action = "app.tag_clear"
		}
		if err := database.WriteAuditTx(tx, database.AuditEntry{
			ActorType: p.Type, ActorID: p.ID,
			Action: action, TargetType: "app", TargetID: app.ID,
			Before:    jsonValStr(map[string]string{"tag": c.tag.Slug, "value": was}),
			After:     jsonValStr(map[string]string{"tag": c.tag.Slug, "value": c.value}),
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	result, err := s.appTags(app)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// appTags returns an app's full tag list: its stored values plus the derived
// Framework pair. Nothing is ever stored for Framework, so it is merged here
// rather than read back from app_tags.
func (s *Server) appTags(app *database.App) ([]database.AppTag, error) {
	stored, err := s.db.TagsForApp(app.ID)
	if err != nil {
		return nil, err
	}
	return withFrameworkTag(app, stored), nil
}

// withFrameworkTag prepends the derived Framework pair, if the app has one.
func withFrameworkTag(app *database.App, stored []database.AppTag) []database.AppTag {
	out := make([]database.AppTag, 0, len(stored)+1)
	if value, slug, ok := tags.FrameworkValue(app.Framework); ok {
		out = append(out, database.AppTag{
			TagID: tags.FrameworkTagID, TagName: "Framework", TagSlug: tags.FrameworkTagSlug,
			Kind: database.TagKindSystem, Color: "slate",
			Value: value, ValueSlug: slug,
		})
	}
	return append(out, stored...)
}
