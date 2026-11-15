package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/telemetry"
)

// handleWake starts waking a sleeping app ahead of the visit about to reach it
// (ADR §123): the SPA calls it when a pointer or the keyboard reaches an Open
// button, so the wake runs while the person is still deciding to click.
//
// **It is the visit's own wake, moved earlier — nothing more.** It needs what a
// visit needs (the app's read rule), wakes only what a visit would wake (an
// app that is `sleeping`, unlocked, with a release — in either mode, and an
// isolated one is where it pays most, since its wake is a pod start), joins the same
// WakeGroup so a click arriving mid-wake waits for this one rather than starting
// another, and like a visit's wake writes no audit row: Start and Restart are
// the acts somebody chose, and pointing at a button is not one. It counts no
// visit either — the request that follows the click does that.
//
// 202 when a wake was started or joined, 204 when there was nothing to wake.
// Never an error a page would show: this is a hint, and a failed wake is the
// gateway's to report when the visit arrives.
func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return
	}
	p := auth.GetPrincipal(r.Context())
	if d := auth.Authorize(p, auth.ActionRead, objectFromApp(app), s.db); !d.Allow {
		writeAuthzError(w, r, d, false)
		return
	}
	if s.builder == nil || app.Status != database.AppStatusSleeping || app.Locked() ||
		app.CurrentRelease == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	go func() {
		began := time.Now()
		ctx := context.Background()
		// The visit's guard too: a hint that read `sleeping` just before another
		// wake finished must not start the workload a second time — for an
		// isolated app that replaces the pod somebody is already being served by.
		err := s.wakes.Do(ctx, app.ID, func() error {
			if now, err := s.db.GetApp(app.ID); err == nil && now != nil && now.Status != database.AppStatusSleeping {
				return nil
			}
			return s.builder.Wake(app)
		})
		telemetry.RecordWake(ctx, app.ID, telemetry.Outcome(err), time.Since(began))
		if err != nil {
			log.Printf("wake: %s (ahead of a visit): %v", app.Slug, err)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
}
