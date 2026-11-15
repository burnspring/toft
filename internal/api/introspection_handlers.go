package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/runtime"
	"toft/internal/toolchain"
	"toft/internal/version"
	"toft/sdk"
)

// handleAppPermissions returns the caller's effective role + per-action allow flags for an app.
func (s *Server) handleAppPermissions(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)

	read := auth.Authorize(p, auth.ActionRead, obj, s.db)
	// This route is the one exception to "every app route refuses a `list`
	// holder the same way", and it is the exception that makes the rest of them
	// usable: it is the route that *describes* the refusal rather than serving
	// anything the refusal is about. A client that got 403 here as well would
	// have no way to tell the gate apart from an error page, and would have to
	// infer the difference from the shape of a failure.
	//
	// It discloses nothing the listing did not: every flag below is false for a
	// caller at the rung, and `effective_role` is the word the owner wrote.
	if !read.Allow && read.Reason != "deny_list_only" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "toft apps lists the apps you can open"})
		return
	}

	edit := auth.Authorize(p, auth.ActionEditContent, obj, s.db)
	del := auth.Authorize(p, auth.ActionDelete, obj, s.db)
	mg := auth.Authorize(p, auth.ActionManageGrants, obj, s.db)
	cv := auth.Authorize(p, auth.ActionChangeVisibility, obj, s.db)
	to := auth.Authorize(p, auth.ActionTransferOwnership, obj, s.db)
	ar := auth.Authorize(p, auth.ActionApproveAccessRequest, obj, s.db)
	pub := auth.Authorize(p, auth.ActionPublish, obj, s.db)

	// can_request_access means what its name says: **this caller may create a
	// request for this app** and it would not be refused on the spot.
	//
	// It used to be true for anybody who was not owner or editor, which
	// included a viewer on a public app who has nothing to ask for. Nothing
	// read it, so the meaning could be corrected for free — and shipping the
	// client on the old one would have put an "ask for access" button in front
	// of people with nothing to ask for.
	canRequest := false
	if p != nil && p.Type == database.PrincipalTypeUser {
		switch {
		case read.EffectiveRole == database.ObjectRoleOwner || read.EffectiveRole == database.ObjectRoleEditor:
			// Everything this route can grant, they already hold.
		case read.EffectiveRole == database.ObjectRoleList:
			// The rung the whole feature exists for: what they would ask for is
			// viewer, and nothing refuses that in advance.
			canRequest = true
		case read.Allow:
			// They can read it, so the only thing left to ask for is editor —
			// which a viewer-system-role account is auto-denied on arrival
			// (they need a publisher promotion first, and that is a different
			// ladder with its own request).
			canRequest = p.SystemRole != database.SystemRoleViewer
		}
	}

	// The request itself rather than a boolean, so the app detail page resolves
	// in one round trip and the gate does not flash between "you may ask" and
	// "you already did". Nil when there is no open request, which is what the
	// omitempty renders as absent.
	var pending *database.ObjectAccessRequestDetail
	if p != nil && p.Type == database.PrincipalTypeUser {
		pending, _ = s.db.PendingObjectAccessRequest(p.ID, app.ID)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"effective_role":              read.EffectiveRole,
		"can_read":                    read.Allow,
		"can_edit":                    edit.Allow,
		"can_delete":                  del.Allow,
		"can_manage_grants":           mg.Allow,
		"can_change_visibility":       cv.Allow,
		"can_transfer_ownership":      to.Allow,
		"can_approve_access_requests": ar.Allow,
		"can_publish":                 pub.Allow,
		"can_request_access":          canRequest,
		"pending_access_request":      pending,
		// Whether the sharing panel may offer to email somebody about a share.
		//
		// The *capability*, not a permission: it says a message would actually
		// go out — email configured, switched on, and the app audience not
		// silenced — and it is here rather than being assumed by the UI because
		// a checkbox that promises mail an administrator has switched off is
		// "copy may not promise mail" broken by a control instead of a
		// sentence. Absent capability, absent control.
		"can_notify_on_share": s.mailer != nil && s.mailer.AppNotificationsEnabled(),
	})
}

// handleMyCapabilities returns system-level capabilities for the current principal.
func (s *Server) handleMyCapabilities(w http.ResponseWriter, r *http.Request) {
	p := auth.GetPrincipal(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	canCreate := p.SystemRole == database.SystemRoleAdmin || p.SystemRole == database.SystemRolePublisher
	// A deploy key has no system role and still creates apps — given the group
	// that will own them (owner_group_required). Its answer is the route list
	// that bounds it, the same one the guard reads, so a read-scoped key says
	// no and a deploy-scoped one says yes.
	if p.IsDeployKey() {
		canCreate = deployKeyRoutesForScope(p.DeployKeyScope)["POST /api/v1/apps"]
	}
	canRequestPromo := p.Type == database.PrincipalTypeUser && p.SystemRole == database.SystemRoleViewer
	canCreateSA := p.Type == database.PrincipalTypeUser && (p.SystemRole == database.SystemRoleAdmin || p.SystemRole == database.SystemRolePublisher)
	// A personal key carries its owner's role and a scope of its own, and a
	// read-scoped one changes nothing: apiKeyRouteGuard refuses its every
	// write key_read_scope, the create included. The answer is that guard's,
	// so `toft whoami` cannot promise a create the key will be refused
	// (docs/bugs 2026-10-28 D7).
	if p.IsExchangedKey() {
		// The deploy surface: an app yes, a service account no.
		canCreateSA = false
	} else if p.IsAPIKey() && !userKeyScopeWrites(p.APIKeyScope) {
		canCreate, canCreateSA = false, false
	}

	// What an app *this caller* creates is born as when they name no mode.
	//
	// Not the setting — the setting is part of the install's shape and lives on
	// GET /settings, which is administrator-only. This is the answer to "what
	// am I about to make", which is a different question and one anybody who
	// may create an app is entitled to: on an isolated-by-default install a
	// developer is about to wait tens of seconds longer for a deploy that
	// behaves differently, and a review screen that omits that is lying by
	// selection.
	//
	// Derived through the same rule the create path applies, degrade included,
	// so the screen cannot promise a mode the create would not produce.
	newAppMode := runtime.ModeShared
	// Whether the Usage tab draws an anonymous-browsers column. A switch's
	// state rather than a figure, so it is anybody's to know: the per-app
	// count it gates is already readable by whoever can read the app.
	audience := false
	if settings, err := s.db.GetPlatformSettings(); err == nil {
		newAppMode = defaultCreateMode(s.capabilitiesFrom(settings), settings.DefaultExecutionMode)
		audience = settings.AudienceMeasurementEnabled
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"system_role":                p.SystemRole,
		"principal_type":             p.Type,
		"can_create_objects":         canCreate,
		"can_request_publisher_role": canRequestPromo,
		"can_create_service_account": canCreateSA,
		"new_app_execution_mode":     string(newAppMode),
		"credential":                 s.callerCredential(p),
		"install":                    s.installCapabilities(newAppMode),
		"audience_measurement":       audience,
	})
}

// callerCredential is which credential made this request, what it may reach,
// and when it stops working — `toft whoami`'s second line.
//
// **An unattended caller learns its limits by asking, or by being refused.**
// The second is the one this exists to replace: a read-scoped key found out at
// its first deploy, and an expiring one at whatever hour it expired. Nothing
// here is a secret the caller does not already hold — it is the record of the
// key it presented, and the prefix is not on it.
func (s *Server) callerCredential(p *auth.Principal) map[string]any {
	switch {
	case p.DeployKeyID != "":
		c := map[string]any{"kind": "deploy_key", "id": p.DeployKeyID, "name": p.DeployKeyName,
			"scope": p.DeployKeyScope}
		if k, err := s.db.GetDeployKey(p.DeployKeyID); err == nil && k != nil && k.ExpiresAt != nil {
			c["expires_at"] = k.ExpiresAt
		}
		return c
	case p.IsExchangedKey():
		c := map[string]any{"kind": "exchanged_key", "id": p.APIKeyID, "name": p.APIKeyName,
			"scope": p.APIKeyScope, "issuer_id": p.APIKeyIssuer}
		if k, err := s.db.GetExchangedKey(p.APIKeyID); err == nil && k != nil {
			c["expires_at"] = k.ExpiresAt
		}
		return c
	case p.APIKeyID != "":
		c := map[string]any{"kind": "api_key", "id": p.APIKeyID, "name": p.APIKeyName,
			"scope": p.APIKeyScope}
		if k, err := s.db.GetUserAPIKey(p.ID, p.APIKeyID); err == nil && k != nil && k.ExpiresAt != nil {
			c["expires_at"] = k.ExpiresAt
		}
		return c
	}
	return map[string]any{"kind": "session"}
}

// installCapabilities is what this install will do for anybody, as far as it
// is the install's decision rather than the caller's: the switches a deploy
// meets before its first byte, and the interpreters a new app is judged
// against.
//
// Each is the value the refusing code path reads, never a restatement of it —
// the upload switch from the source policy, public apps from the setting and
// the license together, jobs from the same capability set the Jobs tab reads,
// and the versions from the set a pin is resolved against for the mode a new
// app is born in. A read that fails answers false: the fail-closed reading the
// paths themselves take.
func (s *Server) installCapabilities(mode runtime.Mode) map[string]any {
	uploads := false
	if policy, err := s.sourcePolicy(); err == nil {
		uploads = policy.UploadsEnabled
	}
	public := false
	if settings, err := s.db.GetPlatformSettings(); err == nil {
		public = settings.PublicAppsEnabled &&
			s.entitlements().Entitled(entitlements.FeaturePublicApps).Entitled
	}
	runtimes := map[string][]string{}
	for _, lang := range []string{toolchain.Node, toolchain.Python, toolchain.Go, toolchain.R} {
		versions := []string{}
		for _, v := range builder.EffectiveVersions(lang, mode) {
			versions = append(versions, v.Version)
		}
		runtimes[lang] = versions
	}
	return map[string]any{
		"version":             version.Version,
		"uploads_allowed":     uploads,
		"public_apps_allowed": public,
		"jobs_available":      s.capabilities().BackgroundJobsAvailable(),
		// The same test every storage route's 404 is: the SPA hides Admin →
		// Storage on this rather than requesting the inventory and reading its
		// 404, which put a failed request on every admin page of an install
		// without storage.
		"storage_available": s.storageOffered(),
		// Whether `toft store` and the SDKs' signed-URL calls have a data
		// plane: a store can be mounted into an app on an install with no
		// signer, and `uploads_allowed` beside it is about source uploads, so
		// without this nothing said which one a refusal was about.
		"store_uploads_available": s.storeUploadsOffered(),
		"sdk_languages":           sdk.Languages(),
		"runtimes":                runtimes,
	}
}
