package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/sources"
)

// The administrator's half of the source policy: reading it, changing it, and
// the two questions that have to be answerable *before* changing it.
//
// An install with forty apps needs to know what it is about to break. That is
// what the preview and the observed-sources endpoints are for, and they are not
// decoration: without them, turning this on means transcribing hostnames by
// hand and discovering the damage afterwards, one refused deploy at a time.
//
// Every route here is admin-only and 404s to everyone else, like
// /system/settings — with one deliberate exception, /effective, which every
// signed-in user may read. A refusal that does not say what *would* work sends
// the developer to guess or to Slack.

type sourcePolicyResponse struct {
	Mode           string                      `json:"mode"`
	UploadsEnabled bool                        `json:"uploads_enabled"`
	Patterns       []database.AllowedGitSource `json:"patterns"`
	// NonCompliant is how many apps the policy *currently stored* already
	// blocks. Shown on the read as well as the preview, because an install can
	// arrive in that state by an app being re-pointed rather than by a policy
	// change, and nobody would think to run a preview to find out.
	NonCompliant int `json:"non_compliant"`
}

func (s *Server) handleGetSourcePolicy(w http.ResponseWriter, r *http.Request) {
	if !s.adminOrNotFound(w, r) {
		return
	}

	policy, err := s.sourcePolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	patterns, err := s.db.ListAllowedGitSourcesWithUsage()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	resp := sourcePolicyResponse{
		Mode: policy.Mode, UploadsEnabled: policy.UploadsEnabled, Patterns: patterns,
	}
	if bad, err := s.db.NonCompliantApps(policy.Policy); err == nil {
		resp.NonCompliant = len(bad)
	}
	writeJSON(w, http.StatusOK, resp)
}

type sourcePolicyRequest struct {
	Mode           *string `json:"mode,omitempty"`
	UploadsEnabled *bool   `json:"uploads_enabled,omitempty"`
	// Patterns is the whole list, not a delta. A list sent as a set is what
	// makes the stored diff meaningful: the server compares what it had with
	// what it was given and records each addition and removal by name.
	Patterns []patternInput `json:"patterns,omitempty"`
}

type patternInput struct {
	Pattern string `json:"pattern"`
	Note    string `json:"note,omitempty"`
}

// parsePatterns canonicalizes an administrator's list, refusing the whole
// request on the first entry that will not parse.
//
// Refused by name rather than dropped: an administrator who typed
// "github.*/acme" meant something, and silently storing nothing is how a list
// comes to disagree with the person who wrote it. The canonical form is what
// comes back, so the surface can show the entry as it will be saved before
// saving it.
func parsePatterns(in []patternInput) ([]database.AllowedGitSource, error) {
	out := make([]database.AllowedGitSource, 0, len(in))
	seen := map[string]bool{}
	for _, p := range in {
		parsed, err := sources.ParsePattern(p.Pattern)
		if err != nil {
			return nil, err
		}
		canonical := parsed.String()
		if seen[canonical] {
			// Two spellings of one entry — "github.com/acme" and
			// "GitHub.com/acme" — collapse rather than 409. The administrator
			// asked for one thing twice.
			continue
		}
		seen[canonical] = true
		out = append(out, database.AllowedGitSource{Pattern: canonical, Note: strings.TrimSpace(p.Note)})
	}
	return out, nil
}

func (s *Server) handleUpdateSourcePolicy(w http.ResponseWriter, r *http.Request) {
	if !s.adminOrNotFound(w, r) {
		return
	}
	p := auth.GetPrincipal(r.Context())

	var req sourcePolicyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	// The license gate, asked after the body is read because the body is what
	// says whether this write configures or removes. Configuring is the
	// allowlist: switching to it, or supplying patterns. Narrowing to `none`,
	// widening back to `any`, turning uploads off or on, and clearing the list
	// are removals of one kind or another, and removal is never gated — an
	// administrator whose trial has ended must still be able to take the
	// policy away.
	configuring := (req.Mode != nil && strings.TrimSpace(*req.Mode) == sources.ModeAllowlist) ||
		len(req.Patterns) > 0
	if configuring && s.refusedUnlicensed(w, r, entitlements.FeatureTrustedSources) {
		return
	}

	current, err := s.sourcePolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	mode := current.Mode
	if req.Mode != nil {
		mode = strings.TrimSpace(*req.Mode)
		if !sources.ValidMode(mode) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "mode must be one of " + sources.ModeAny + ", " + sources.ModeAllowlist +
					", " + sources.ModeNone + ".", "hint": `send "mode" as any, allowlist or none`})
			return
		}
	}
	uploads := current.UploadsEnabled
	if req.UploadsEnabled != nil {
		uploads = *req.UploadsEnabled
	}

	// The list is kept even under "any" and "none". An administrator trying a
	// mode and going back should find their patterns where they left them;
	// deleting the list as a side effect of a mode change would be an
	// irreversible act dressed up as a reversible one.
	entries, err := s.db.ListAllowedGitSources()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if req.Patterns != nil {
		if entries, err = parsePatterns(req.Patterns); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "write each pattern as a host and an optional path, like github.com/acme"})
			return
		}
	}

	// Read before the write, so the notification below can say what changed
	// rather than what the policy now is.
	beforePatterns := current.PatternStrings()

	meta := requestMeta(r.Context())
	if err := s.db.SetSourcePolicy(mode, uploads, entries, p.ID, meta.RequestID, meta.IP, meta.UserAgent); err != nil {
		log.Printf("sources: saving the source policy: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	saved, err := s.sourcePolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	patterns, _ := s.db.ListAllowedGitSourcesWithUsage()
	resp := sourcePolicyResponse{Mode: saved.Mode, UploadsEnabled: saved.UploadsEnabled, Patterns: patterns}

	// Somebody is told that where this install may reach for code has widened.
	// **After the write and never before it**: the send is optional and never
	// load-bearing, so it must not be able to decide whether the policy was
	// saved. Only a widening — announcing a narrowing too would train an
	// administrator to ignore the message.
	afterPatterns := make([]string, 0, len(entries))
	for _, e := range entries {
		afterPatterns = append(afterPatterns, e.Pattern)
	}
	if detail := widenedPolicy(current, saved, beforePatterns, afterPatterns); detail != "" {
		s.notifySourceReachWidened("Where this install may deploy from has widened", detail)
	}

	// Warnings rather than refusals, every one of them. Each names a real
	// consequence the administrator may well have intended, and refusing a
	// deliberate choice is how a control becomes something people work around.
	var warning string
	if bad, err := s.db.NonCompliantApps(saved.Policy); err == nil {
		resp.NonCompliant = len(bad)
		if len(bad) > 0 {
			subject, pronoun, possessive := "apps deploy", "They keep", "their"
			if len(bad) == 1 {
				subject, pronoun, possessive = "app deploys", "It keeps", "its"
			}
			warning = strconv.Itoa(len(bad)) + " " + subject + " from a source this policy does not allow. " +
				pronoun + " running and serving, and rollback still works — what stops is " +
				possessive + " next deploy, and automatic deploys."
		}
	}
	if saved.Mode == sources.ModeAllowlist && len(entries) == 0 {
		warning = "The approved-source list is empty, so nothing can be deployed from git at all. " +
			"Add a pattern, or choose \"no git\" if that is what you meant."
	}
	if saved.Restricted() && saved.UploadsEnabled && warning == "" {
		// The fence with a gate next to it. Said out loud, because an
		// administrator who believes git is now the only way in has a control
		// they do not have.
		warning = "ZIP uploads are still allowed. A developer refused by the source policy can " +
			"upload the same code as a ZIP."
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": resp, "warning": warning})
}

type sourcePolicyPreview struct {
	Mode           string                 `json:"mode"`
	Patterns       []string               `json:"patterns"`
	NonCompliant   []database.AppSource   `json:"non_compliant"`
	Count          int                    `json:"count"`
	Invalid        []patternParseFailure  `json:"invalid,omitempty"`
	Canonicalized  []patternCanonicalized `json:"canonicalized,omitempty"`
	UploadsEnabled bool                   `json:"uploads_enabled"`
}

type patternParseFailure struct {
	Pattern string `json:"pattern"`
	Error   string `json:"error"`
}

// patternCanonicalized is an entry that will be stored differently from how it
// was typed — a bare host that gains its "/*", a mixed-case path, a ".git"
// suffix. Shown before saving, because a pattern that changes under
// normalization and is only revealed afterwards reads like the platform
// ignoring what was asked for.
type patternCanonicalized struct {
	Typed  string `json:"typed"`
	Stored string `json:"stored"`
}

// handlePreviewSourcePolicy answers "what would this do", and mutates nothing.
//
// Nothing is saved until the administrator has seen this. It is the difference
// between adopting the feature and abandoning it on an install that already has
// apps.
func (s *Server) handlePreviewSourcePolicy(w http.ResponseWriter, r *http.Request) {
	if !s.adminOrNotFound(w, r) {
		return
	}

	var req sourcePolicyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}

	current, err := s.sourcePolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	preview := sourcePolicyPreview{Mode: current.Mode, UploadsEnabled: current.UploadsEnabled}
	if req.Mode != nil && sources.ValidMode(strings.TrimSpace(*req.Mode)) {
		preview.Mode = strings.TrimSpace(*req.Mode)
	}
	if req.UploadsEnabled != nil {
		preview.UploadsEnabled = *req.UploadsEnabled
	}

	proposed := sources.Policy{Mode: preview.Mode}
	inputs := req.Patterns
	if inputs == nil {
		for _, e := range current.PatternStrings() {
			inputs = append(inputs, patternInput{Pattern: e})
		}
	}
	// Unparseable entries are reported alongside the impact rather than failing
	// the whole preview: the administrator is mid-edit, and an error that
	// replaces the answer they asked for is an error they will click past.
	for _, in := range inputs {
		parsed, err := sources.ParsePattern(in.Pattern)
		if err != nil {
			preview.Invalid = append(preview.Invalid, patternParseFailure{Pattern: in.Pattern, Error: err.Error()})
			continue
		}
		proposed.Patterns = append(proposed.Patterns, parsed)
		if canonical := parsed.String(); canonical != strings.TrimSpace(in.Pattern) {
			preview.Canonicalized = append(preview.Canonicalized,
				patternCanonicalized{Typed: strings.TrimSpace(in.Pattern), Stored: canonical})
		}
	}
	preview.Patterns = proposed.PatternStrings()

	bad, err := s.db.NonCompliantApps(proposed)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	preview.NonCompliant, preview.Count = bad, len(bad)
	writeJSON(w, http.StatusOK, preview)
}

// handleObservedSources offers what this install already deploys from, so that
// a starting list is a few clicks rather than transcription.
func (s *Server) handleObservedSources(w http.ResponseWriter, r *http.Request) {
	if !s.adminOrNotFound(w, r) {
		return
	}
	observed, err := s.db.ObservedSources()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": observed})
}

// handleEffectiveSourcePolicy is the one route here a non-administrator may
// read: the mode and the patterns, and nothing about who set them or what they
// cover.
//
// It exists so the New App form can say what would work instead of refusing
// blindly, and so an install with no source permitted at all can explain itself
// rather than rendering a form that cannot succeed.
func (s *Server) handleEffectiveSourcePolicy(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	policy, err := s.sourcePolicy()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, newEffectiveSourcePolicy(policy))
}

// adminOrNotFound is the gate the admin half of this file shares with
// /system/settings: not 403, because the shape of an install is infrastructure
// detail and a non-administrator has no business learning it.
func (s *Server) adminOrNotFound(w http.ResponseWriter, r *http.Request) bool {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return false
	}
	return true
}
