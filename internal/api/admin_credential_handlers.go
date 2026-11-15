package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/supervisor"
)

// Credentials an administrator holds and attaches to apps.
//
// **No route here returns a value**, and there is no route that could: the
// store has no read that produces one, and the only function that decrypts is
// unreachable from HTTP. What an administrator manages is which keys exist and
// which apps hold them.
//
// The claim this feature makes is accountability, never containment. App code
// can read an injected value trivially, and no copy in these handlers or in the
// UI they feed may imply otherwise.

func (s *Server) requireCredentialAdmin(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.GetPrincipal(r.Context())
	if p == nil || p.IsAnonymous() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil
	}
	if !p.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "only an administrator can manage credentials",
			"hint":  "ask an administrator to make this change"})
		return nil
	}
	return p
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	if s.requireCredentialAdmin(w, r) == nil {
		return
	}
	creds, err := s.db.ListCredentials()
	if err != nil {
		log.Printf("listing credentials: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": creds})
}

// credentialRequest is a create or an update, and on an update **an absent
// field is one the caller is not changing**.
//
// Pointers rather than strings, because the two update surfaces are two
// different acts against one route: a rename carries a name and a description
// and no values, a rotation carries values and neither of the others. Plain
// strings would make each of them silently clear what the other owns.
type credentialRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	// Values are write-only in both directions: sent here, never returned.
	// Absent on an update leaves the stored values alone, which is what lets an
	// administrator fix a description without retyping a password.
	Values map[string]string `json:"values,omitempty"`
	// Keep, on an update, names whose stored value stays as it is. With it a
	// rotation sends only what changed: every stored name is sent, kept or
	// dropped. Meaningless on a create, where nothing is stored yet.
	Keep []string `json:"keep,omitempty"`
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *Server) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	p := s.requireCredentialAdmin(w, r)
	if p == nil {
		return
	}
	var body credentialRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}
	c, err := s.db.CreateCredential(r.Context(), deref(body.Name), deref(body.Description),
		body.Values, s.encryptor.Encrypt, s.auditFor(r, p))
	switch {
	case errors.Is(err, database.ErrCredentialNameTaken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "hint": "choose another name — GET /api/v1/admin/credentials lists the names in use"})
		return
	case err != nil && strings.Contains(err.Error(), "required"),
		err != nil && strings.Contains(err.Error(), "supply nothing"),
		err != nil && strings.Contains(err.Error(), "cannot be kept"):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "hint": `send a name and the values to store, as {"name": "warehouse", "values": {"DB_PASSWORD": "…"}} — on an update, keep names only keys already stored`})
		return
	case err != nil:
		log.Printf("creating credential: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleUpdateCredential changes the name, the description, the values, or any
// combination — an absent field is one the caller is not changing.
//
// **It does not restart anything**, and that asymmetry with detach is the
// decision rather than an oversight: a rotation means the old value is already
// dead at the vendor, so a running app loses access whether or not anything
// restarts, and bouncing the fleet would turn a routine password change into an
// estate-wide outage at a moment nobody chose. What it does instead is mark
// every attached app as carrying an unapplied change, which the banner those
// apps already have renders. A rename marks nothing: the name is a label, not
// something a workload holds.
func (s *Server) handleUpdateCredential(w http.ResponseWriter, r *http.Request) {
	p := s.requireCredentialAdmin(w, r)
	if p == nil {
		return
	}
	var body credentialRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "hint": badBodyHint})
		return
	}
	err := s.db.UpdateCredential(r.Context(), chi.URLParam(r, "id"), body.Name, body.Description,
		body.Values, body.Keep, s.encryptor.Encrypt, s.auditFor(r, p))
	var collision *database.CollisionError
	switch {
	case errors.As(err, &collision):
		// A rotation that adds a key an attached app already has, from its own
		// variables or another credential — the attach's refusal, naming the app.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": collision.App + " already has values for " + collision.Error() +
				". Rename the credential's keys, or ask the app's owner to remove theirs.",
			"code":       "key_collision",
			"app":        collision.App,
			"collisions": collision.Collisions,
			"hint":       "send the rotation again with the clashing keys renamed — nothing was changed",
		})
		return
	case errors.Is(err, database.ErrCredentialNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such credential", "hint": "GET /api/v1/admin/credentials lists the credentials by id"})
		return
	case errors.Is(err, database.ErrCredentialNameTaken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "hint": "choose another name — GET /api/v1/admin/credentials lists the names in use"})
		return
	case err != nil && strings.Contains(err.Error(), "required"),
		err != nil && strings.Contains(err.Error(), "supply nothing"),
		err != nil && strings.Contains(err.Error(), "cannot be kept"):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "hint": `send a name and the values to store, as {"name": "warehouse", "values": {"DB_PASSWORD": "…"}} — on an update, keep names only keys already stored`})
		return
	case err != nil:
		log.Printf("updating credential: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListCredentialApps is which apps hold one.
//
// **A separate route rather than a field on the inventory**: the inventory is
// every credential on the install, and the apps behind each of them is a
// fan-out nobody browsing it asked for. The count is what the list carries; the
// names are what the credential's own page reads.
//
// It answers with apps, never with anything about the values — this route is
// the same read as the delete refusal that already names them, given to a page
// instead of to an error message.
func (s *Server) handleListCredentialApps(w http.ResponseWriter, r *http.Request) {
	if s.requireCredentialAdmin(w, r) == nil {
		return
	}
	apps, err := s.db.ListCredentialApps(chi.URLParam(r, "id"))
	if err != nil {
		log.Printf("listing credential apps: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	p := s.requireCredentialAdmin(w, r)
	if p == nil {
		return
	}
	id := chi.URLParam(r, "id")
	err := s.db.DeleteCredential(r.Context(), id, s.auditFor(r, p))
	switch {
	case errors.Is(err, database.ErrCredentialNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such credential", "hint": "GET /api/v1/admin/credentials lists the credentials by id"})
		return
	case errors.Is(err, database.ErrCredentialInUse):
		// Named, because an administrator told "in use" and not told by what has
		// to go looking — and because detaching is the act that restarts those
		// apps, so it should be the deliberate one.
		apps, _ := s.db.AppsUsingCredential(id)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this credential is attached to apps — detach it first, which restarts them",
			"code":  "credential_in_use",
			"apps":  apps,
			"hint":  "DELETE /api/v1/admin/apps/{appID}/credentials/" + id + " detaches it from each app named in apps",
		})
		return
	case err != nil:
		log.Printf("deleting credential: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAttachCredential gives one app one credential.
//
// The app is addressed under `/admin` rather than under `/apps/{id}` on purpose:
// this is not something an app's owner may do, and a route living beside the
// ones they can reach is a route somebody eventually opens up.
func (s *Server) handleAttachCredential(w http.ResponseWriter, r *http.Request) {
	p := s.requireCredentialAdmin(w, r)
	if p == nil {
		return
	}
	appID := chi.URLParam(r, "appID")
	if _, ok := s.appOr404(w, appID); !ok {
		return
	}

	err := s.db.AttachCredential(r.Context(), appID, chi.URLParam(r, "id"), s.auditFor(r, p))
	var collision *database.CollisionError
	switch {
	case errors.As(err, &collision):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this app already has values for " + collision.Error() +
				". Rename the credential's keys, or ask the app's owner to remove theirs.",
			"code":       "key_collision",
			"collisions": collision.Collisions,
			"hint":       "rename the clashing keys in the credential, or remove the app's own variables of those names, then attach again",
		})
		return
	case errors.Is(err, database.ErrCredentialNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such credential", "hint": "GET /api/v1/admin/credentials lists the credentials by id"})
		return
	case err != nil:
		log.Printf("attaching credential: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDetachCredential takes it away **and restarts the app**.
//
// The value is in a live process's environment and only a start replaces that.
// Leaving it to the next deploy would keep a revoked credential under a running
// process for however long the app goes without one, which for a stable
// internal app is months — the same gap ADR §12a's storage revocation closed,
// and this permission is more sensitive than a mount.
func (s *Server) handleDetachCredential(w http.ResponseWriter, r *http.Request) {
	p := s.requireCredentialAdmin(w, r)
	if p == nil {
		return
	}
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	if err := s.db.DetachCredential(r.Context(), appID, chi.URLParam(r, "id"), s.auditFor(r, p)); err != nil {
		log.Printf("detaching credential: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	s.supervisor.Remount([]supervisor.RestartTarget{{ID: app.ID, Slug: app.Slug}}, "a credential was detached from this app")

	writeJSON(w, http.StatusOK, map[string]any{
		// Said back, because the administrator has just restarted somebody
		// else's app and the list is what lets them say so to the right people.
		"restarting": []string{app.Slug},
	})
}

func (s *Server) auditFor(r *http.Request, p *auth.Principal) database.AuditEntry {
	meta := requestMeta(r.Context())
	return database.AuditEntry{
		ActorType: database.PrincipalTypeUser, ActorID: p.ID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent,
	}
}
