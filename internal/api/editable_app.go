package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// editableApp loads the app from the URL and authorizes ActionEditContent —
// the gate for owner/editor configuration (API keys, tags, render schedules), like
// logs and env vars. Not viewer-visible.
func (s *Server) editableApp(w http.ResponseWriter, r *http.Request) *database.App {
	app, _ := s.editableAppDecision(w, r)
	return app
}

// editableAppDecision is editableApp with the decision that admitted the
// caller, for the one handler that has to bound what it writes by *how much
// authority the caller has* rather than by whether they got in.
//
// Returning the same Decision rather than resolving the role a second time is
// the whole point. "No word above your own role" reads as one sentence and has
// two implementations that disagree about exactly one person: Authorize's first
// step is a system_admin bypass returning EffectiveRole owner, while
// auth.CallerRole goes through EffectiveObjectRole, which deliberately has no
// such bypass and resolves an administrator with no grant to viewer. Asking
// twice would let a Toft administrator mint only the word below the one
// they need, on an app they administer completely — an over-refusal, which is
// quiet, and ADR §56 already names the trap in terms: the read gate decides
// whether there is anybody to describe, the role ladder only describes them,
// and the difference is the whole administrator case.
func (s *Server) editableAppDecision(w http.ResponseWriter, r *http.Request) (*database.App, auth.Decision) {
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return nil, auth.Decision{}
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	d := auth.Authorize(p, auth.ActionEditContent, obj, s.db)
	if !d.Allow {
		writeAuthzError(w, r, d, auth.CanRead(p, obj, s.db))
		return nil, d
	}
	return app, d
}
