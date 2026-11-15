package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/database"
	"toft/internal/scim"
)

// The SCIM endpoint: what a directory may tell this install about its people.
//
// ADR §48 said the `users` row is a shadow that never learns its original died,
// and settled for a dormancy sweep because a sweep was the only answer available
// from inside the sign-in path. This is the other answer, and it does not retire
// the sweep — the sweep covers every install without a feed, which will stay
// most of them.
//
// **A feed has no session, and every rule in this product assumed one.** Three
// consequences shape everything here: the audit actor is a provider rather than
// a person, a refusal has to be renderable into a protocol rather than read as a
// sentence, and a retried write must write nothing at all.

// scimProviderKey carries the authenticated directory down the request.
type scimProviderCtxKey struct{}

func scimProvider(ctx context.Context) *database.AuthProvider {
	p, _ := ctx.Value(scimProviderCtxKey{}).(*database.AuthProvider)
	return p
}

// scimAuth authenticates a directory by its bearer token.
//
// Presence of a stored hash **is** the switch: an install that has minted no
// token has no feed, and every route here answers 401. That is deliberately not
// the 404 this product uses elsewhere for an absent capability — an IdP's
// diagnostics distinguish "not authorized" from "wrong URL", and the
// administrator reading them is looking at the IdP's screen, not ours. A 404
// would send them to check the base URL they pasted correctly.
func (s *Server) scimAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeSCIMError(w, http.StatusUnauthorized, "", "a bearer token is required")
			return
		}
		sum := sha256.Sum256([]byte(token))
		provider, err := s.db.SCIMProviderByTokenHash(hex.EncodeToString(sum[:]))
		if err != nil || provider == nil {
			if err != nil && !errors.Is(err, database.ErrNoSCIMToken) {
				log.Printf("scim: resolving token: %v", err)
			}
			writeSCIMError(w, http.StatusUnauthorized, "", "that token is not configured on this install")
			return
		}
		// Constant-time on the hash we just matched, which is belt and braces
		// over an equality the database already did — cheap, and it keeps the
		// comparison from ever becoming a `LIKE` in a later edit.
		want := hex.EncodeToString(sum[:])
		if subtle.ConstantTimeCompare([]byte(want), []byte(provider.SCIMTokenHash)) != 1 {
			writeSCIMError(w, http.StatusUnauthorized, "", "that token is not configured on this install")
			return
		}

		// Disabling a provider is the reversible half of removing it, and a
		// directory whose provider is switched off has no more standing to
		// unlock accounts or move group membership than one that was deleted.
		if !provider.Enabled {
			writeSCIMError(w, http.StatusUnauthorized, "", "this token's provider is disabled on this install")
			return
		}

		if err := s.db.TouchSCIMCall(provider.ID); err != nil {
			// Reporting, never authorization: a failed stamp must not refuse a
			// sync. The same bargain `RecordLogin` makes on the sign-in path.
			log.Printf("scim: recording call for %s: %v", provider.ID, err)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scimProviderCtxKey{}, provider)))
	})
}

// writeSCIM renders a body under SCIM's own media type, which at least one IdP
// treats `application/json` as a protocol violation for.
func writeSCIM(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", scim.ContentType)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("scim: writing response: %v", err)
	}
}

func writeSCIMError(w http.ResponseWriter, status int, scimType, detail string) {
	writeSCIM(w, status, scim.NewError(status, scimType, detail))
}

// --- Discovery ---
//
// Okta will not complete a connector's configuration without these three, and
// each of them is this endpoint stating its own subset. A provider that reads
// `ServiceProviderConfig` and sees creation unsupported configures around it;
// one that does not read it discovers the 501 instead, which says the same
// thing.

func (s *Server) handleSCIMServiceProviderConfig(w http.ResponseWriter, r *http.Request) {
	writeSCIM(w, http.StatusOK, map[string]any{
		"schemas": []string{scim.SchemaSPConfig},
		"documentationUri": strings.TrimSuffix(s.cfg.BaseURL, "/") +
			"/_docs/admin/authentication",
		"patch":          map[string]any{"supported": true},
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":         map[string]any{"supported": true, "maxResults": 200},
		"changePassword": map[string]any{"supported": false},
		"sort":           map[string]any{"supported": false},
		"etag":           map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type":        "oauthbearertoken",
			"name":        "OAuth Bearer Token",
			"description": "A token minted on this install, per sign-in provider.",
			"primary":     true,
		}},
	})
}

func (s *Server) handleSCIMResourceTypes(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimSuffix(s.cfg.BaseURL, "/") + "/scim/v2"
	writeSCIM(w, http.StatusOK, scim.NewListResponse([]any{
		map[string]any{
			"schemas": []string{scim.SchemaResourceTy}, "id": "User", "name": "User",
			"endpoint": "/Users", "schema": scim.SchemaUser,
			"meta": map[string]string{"resourceType": "ResourceType", "location": base + "/ResourceTypes/User"},
		},
		map[string]any{
			"schemas": []string{scim.SchemaResourceTy}, "id": "Group", "name": "Group",
			"endpoint": "/Groups", "schema": scim.SchemaGroup,
			"meta": map[string]string{"resourceType": "ResourceType", "location": base + "/ResourceTypes/Group"},
		},
	}))
}

func (s *Server) handleSCIMSchemas(w http.ResponseWriter, r *http.Request) {
	writeSCIM(w, http.StatusOK, scim.NewListResponse([]any{
		map[string]any{"id": scim.SchemaUser, "name": "User"},
		map[string]any{"id": scim.SchemaGroup, "name": "Group"},
	}))
}

// --- Users ---

// handleSCIMListUsers answers the lookup an IdP does before it decides whether
// to create an account.
//
// A filter matching nothing is `totalResults: 0`, never a 404: the difference is
// what the IdP branches on, and a 404 here reads as a broken endpoint rather
// than an absent person. An unfiltered list is refused rather than answered —
// this endpoint will not enumerate an install's people for a credential in a
// third-party console, and no IdP needs it to.
func (s *Server) handleSCIMListUsers(w http.ResponseWriter, r *http.Request) {
	provider := scimProvider(r.Context())

	filter := r.URL.Query().Get("filter")
	if strings.TrimSpace(filter) == "" {
		writeSCIMError(w, http.StatusBadRequest, scim.TypeInvalidFilter,
			`a filter is required: this endpoint answers 'userName eq "value"' and does not enumerate accounts`)
		return
	}
	userName, err := scim.ParseUserNameFilter(filter)
	if err != nil {
		writeSCIMError(w, http.StatusBadRequest, scim.TypeInvalidFilter, err.Error())
		return
	}

	found, err := s.db.UserByProviderLogin(provider.ID, userName)
	if errors.Is(err, database.ErrAmbiguousSCIMLogin) {
		writeSCIMError(w, http.StatusConflict, "uniqueness",
			"more than one account holds this userName; resolve it by id")
		return
	}
	if err != nil {
		log.Printf("scim: looking up %q for %s: %v", userName, provider.ID, err)
		writeSCIMError(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if found == nil {
		writeSCIM(w, http.StatusOK, scim.NewListResponse(nil))
		return
	}
	writeSCIM(w, http.StatusOK, scim.NewListResponse([]any{s.renderSCIMUser(found)}))
}

func (s *Server) handleSCIMGetUser(w http.ResponseWriter, r *http.Request) {
	provider := scimProvider(r.Context())
	found, err := s.db.UserByProviderAndID(provider.ID, chi.URLParam(r, "id"))
	if err != nil {
		log.Printf("scim: reading user: %v", err)
		writeSCIMError(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if found == nil {
		writeSCIMError(w, http.StatusNotFound, "", "no such user")
		return
	}
	writeSCIM(w, http.StatusOK, s.renderSCIMUser(found))
}

// handleSCIMPatchUser is deprovisioning, and it is the whole of it.
//
// `active: false` locks; `active: true` unlocks. Nothing here deletes: deleting
// an account that owns apps is refused in a message written for a person to
// read, and a feed cannot make the decision that refusal exists to force.
//
// **A repeat writes nothing.** IdPs retry generously and forever, and a lock
// bumps `token_version` — so a no-change write that wrote anyway would end the
// person's other sessions on whatever schedule the IdP happens to keep.
func (s *Server) handleSCIMPatchUser(w http.ResponseWriter, r *http.Request) {
	provider := scimProvider(r.Context())

	var req scim.PatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSCIMError(w, http.StatusBadRequest, scim.TypeInvalidSyntax, "the request body is not a PatchOp")
		return
	}

	found, err := s.db.UserByProviderAndID(provider.ID, chi.URLParam(r, "id"))
	if err != nil {
		log.Printf("scim: reading user for patch: %v", err)
		writeSCIMError(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if found == nil {
		writeSCIMError(w, http.StatusNotFound, "", "no such user")
		return
	}

	active, err := scim.ActiveFromPatch(req)
	if errors.Is(err, scim.ErrNoSupportedOperation) {
		// Understood and had nothing to do. Answering 200 with the unchanged
		// resource is what an IdP expects for an attribute it manages and this
		// endpoint does not — a 400 here would fail a sync over a display name.
		writeSCIM(w, http.StatusOK, s.renderSCIMUser(found))
		return
	}
	if err != nil {
		writeSCIMError(w, http.StatusBadRequest, scim.TypeInvalidValue, err.Error())
		return
	}

	audit := database.AuditEntry{
		ActorType: database.PrincipalTypeProvider, ActorID: provider.ID,
		RequestID: requestMeta(r.Context()).RequestID,
		IP:        requestMeta(r.Context()).IP,
		After:     jsonValStr(map[string]any{"via": "scim", "provider_id": provider.ID, "external_id": found.ExternalID}),
	}

	if active {
		if _, err := s.db.UnlockUser(r.Context(), found.User.ID, audit); err != nil {
			log.Printf("scim: unlocking %s: %v", found.User.ID, err)
			writeSCIMError(w, http.StatusInternalServerError, "", "internal error")
			return
		}
	} else {
		_, err := s.db.LockUser(r.Context(), found.User.ID, audit)
		if errors.Is(err, database.ErrLastAdmin) {
			// The one refusal this endpoint makes, and it is refused whoever
			// asks: an install whose only administrator who can sign in is
			// locked cannot be administered at all, including to undo it.
			log.Printf("scim: %s refused to deactivate %s — last administrator", provider.ID, found.User.ID)
			s.auditSCIMRefusal(r, provider, found.User.ID, "last_admin")
			writeSCIMError(w, http.StatusConflict, scim.TypeMutability,
				database.ErrLastAdmin.Error()+". "+
					"Make somebody else an administrator here first, then deactivate this account.")
			return
		}
		if err != nil {
			log.Printf("scim: locking %s: %v", found.User.ID, err)
			writeSCIMError(w, http.StatusInternalServerError, "", "internal error")
			return
		}
	}

	updated, err := s.db.UserByProviderAndID(provider.ID, found.User.ID)
	if err != nil || updated == nil {
		writeSCIMError(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	writeSCIM(w, http.StatusOK, s.renderSCIMUser(updated))
}

func (s *Server) auditSCIMRefusal(r *http.Request, provider *database.AuthProvider, userID, reason string) {
	meta := requestMeta(r.Context())
	if err := s.db.WriteAuditRow(database.AuditEntry{
		ActorType: database.PrincipalTypeProvider, ActorID: provider.ID,
		Action: "scim.refused", TargetType: "user", TargetID: userID,
		After:     jsonValStr(map[string]string{"reason": reason, "provider_id": provider.ID}),
		RequestID: meta.RequestID, IP: meta.IP,
	}); err != nil {
		log.Printf("scim: recording refusal: %v", err)
	}
}

// renderSCIMUser is the platform's account as one directory sees it.
//
// `active` is the inverse of locked, which is the whole of what this endpoint
// lets a directory read about somebody's standing here. Not their system role
// and not their groups: a feed says who exists and whether they are active, and
// what they may do stays this product's own.
func (s *Server) renderSCIMUser(u *database.SCIMUser) scim.User {
	out := scim.User{
		Schemas:    []string{scim.SchemaUser},
		ID:         u.User.ID,
		ExternalID: u.ExternalID,
		UserName:   u.Login,
		Active:     !u.User.Locked(),
		Meta: scim.Meta{
			ResourceType: "User",
			Created:      u.User.CreatedAt.UTC().Format(time.RFC3339),
			Location:     strings.TrimSuffix(s.cfg.BaseURL, "/") + "/scim/v2/Users/" + u.User.ID,
		},
	}
	if u.User.Name != "" {
		out.Name = &scim.Name{Formatted: u.User.Name}
	}
	if u.User.Email != "" {
		out.Emails = []scim.Email{{Value: u.User.Email, Primary: true}}
	}
	return out
}

// --- Groups ---

// handleSCIMPatchGroup applies member operations to a mapped group.
//
// The group a directory names is resolved through the *same*
// `group_claim_mappings` row a sign-in claim resolves through, so an
// administrator maps a directory group to a local one once and both paths find
// it. A group nobody mapped is ignored in silence — the same treatment an
// unmapped claim value gets, and for the same reason: an install with 400
// directory groups should not acquire 400 rows nobody curated.
//
// Creating or naming a group over SCIM is refused elsewhere (`POST` is 501).
// ADR §51 decided a group is a local row an administrator created and named,
// and a feed that could create them would reverse that through a side door.
func (s *Server) handleSCIMPatchGroup(w http.ResponseWriter, r *http.Request) {
	provider := scimProvider(r.Context())

	var req scim.PatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSCIMError(w, http.StatusBadRequest, scim.TypeInvalidSyntax, "the request body is not a PatchOp")
		return
	}

	// The id in the path is the directory's own name for the group, which is
	// what it maps by. Resolved through the mapping table rather than treated
	// as a local group id: a feed does not know this install's ids, and a
	// caller that could address a local group directly could reach one nobody
	// mapped.
	groupID, err := s.db.GroupByClaimValue(provider.ID, chi.URLParam(r, "id"))
	if err != nil {
		log.Printf("scim: resolving group: %v", err)
		writeSCIMError(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if groupID == "" {
		writeSCIMError(w, http.StatusNotFound, "",
			"no group on this install takes its membership from that directory group. "+
				"An administrator attaches it under the group's Directory section.")
		return
	}

	changes, err := scim.MembersFromPatch(req)
	if errors.Is(err, scim.ErrNoSupportedOperation) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeSCIMError(w, http.StatusBadRequest, scim.TypeInvalidSyntax, err.Error())
		return
	}

	meta := requestMeta(r.Context())
	audit := database.AuditEntry{
		ActorType: database.PrincipalTypeProvider, ActorID: provider.ID,
		RequestID: meta.RequestID, IP: meta.IP,
	}
	var capped int
	for _, c := range changes {
		// A member is named by this install's user id, which is what the
		// `/Users` read handed the IdP. One that names somebody the directory
		// has no identity for is skipped rather than refused: the rest of the
		// operation is still correct, and failing the whole PATCH would stall
		// a sync on one stale row.
		found, err := s.db.UserByProviderAndID(provider.ID, c.UserID)
		if err != nil || found == nil {
			continue
		}
		if c.Add {
			_, wasCapped, err := s.db.AddDirectoryMembership(r.Context(), groupID, found.User.ID, provider.ID, audit)
			if err != nil {
				log.Printf("scim: adding %s to %s: %v", found.User.ID, groupID, err)
				writeSCIMError(w, http.StatusInternalServerError, "", "internal error")
				return
			}
			if wasCapped {
				capped++
			}
			continue
		}
		if _, err := s.db.RemoveDirectoryMembership(r.Context(), groupID, found.User.ID, provider.ID, audit); err != nil {
			log.Printf("scim: removing %s from %s: %v", found.User.ID, groupID, err)
			writeSCIMError(w, http.StatusInternalServerError, "", "internal error")
			return
		}
	}
	if capped > 0 {
		// Recorded rather than refused, exactly as the claim reconciler does:
		// there is no administrator in front of this to be told, and failing
		// the sync would stall every other membership in the request.
		log.Printf("scim: %s: %d membership(s) refused by the %d-group cap",
			provider.ID, capped, database.GroupMembershipCap)
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- The refusals ---

// handleSCIMUnsupportedCreate answers a POST.
//
// An account is created by the sign-in that first presents an identity. A second
// creation path would have to invent answers for `default_role`, for pending
// activation and for `link_by_verified_email` with no sign-in behind any of
// them — so there is one provisioning path, and the IdP is *told* rather than
// left to discover that its creates vanished.
func (s *Server) handleSCIMUnsupportedCreate(w http.ResponseWriter, r *http.Request) {
	writeSCIMError(w, http.StatusNotImplemented, "",
		"this install does not create accounts from a feed. An account is created the first time "+
			"somebody signs in through this provider; deactivating and reactivating an existing one is supported.")
}

// handleSCIMUnsupportedGroups answers the group shapes this install does not
// implement — reading and replacing groups — with the same 501 and the same
// reason, so a directory pre-checking for a group finds a SCIM error body and
// not chi's 404.
func (s *Server) handleSCIMUnsupportedGroups(w http.ResponseWriter, r *http.Request) {
	writeSCIMError(w, http.StatusNotImplemented, "",
		"this install does not read or replace groups from a feed. Group membership is taken "+
			"through PATCH /Groups/{id}; groups themselves are made on the platform.")
}

// handleSCIMUnsupportedDelete answers a DELETE.
//
// Deleting an account that owns apps is refused here in a message written for a
// person, offering the transfer that has to happen first. A feed cannot make
// that decision, so deactivation is the whole of what it may do.
func (s *Server) handleSCIMUnsupportedDelete(w http.ResponseWriter, r *http.Request) {
	writeSCIMError(w, http.StatusNotImplemented, "",
		"this install does not delete accounts from a feed — an account may own apps, and moving those "+
			"to somebody else is a decision a person makes. Set active to false to deactivate instead.")
}
