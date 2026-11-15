package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
)

// Locking and unlocking an app.
//
// The control an administrator reaches for when an app should stop being
// available but must not be deleted: its author left, its purpose ended, or
// nobody has opened it since the quarter it was built. Deleting is
// irreversible and takes a decision nobody wants to make in a hurry; leaving it
// running spends a port, a process and a memory footprint on something nobody
// wants. A lock is the reversible middle.
//
// **Administrator-only, and that is the point.** Stop and Restart authorize
// with ActionEditContent, so an app's owner may leave the `stopped` state
// whenever they like — which is right for a stop and would make a lock
// meaningless. Locking is therefore its own admin-gated pair of routes rather
// than a value the owner's PATCH can reach. See docs/invariants.md § App
// locking.

// lockRequest is what an administrator types when locking.
type lockRequest struct {
	// Message is shown to whoever opens the app. Optional: empty falls through
	// to the install's default and then to the built-in sentence, so a lock is
	// never silent even when the administrator says nothing.
	Message string `json:"message"`
}

// handleLockApp — POST /apps/{appID}/lock.
//
// Locking stops the workload as well as refusing traffic. That is deliberate
// rather than incidental: reclaiming the resources of apps nobody uses is half
// of why anybody locks one, and a "locked" app still holding a port and 400MB
// resident would be a label rather than a control.
//
// Locking an already-locked app is not an error — it replaces the message,
// which is what an administrator fixing their own wording expects.
func (s *Server) handleLockApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	// Admin before the app is even looked up, so every non-administrator gets
	// the same answer whatever the id: looked up first, a made-up id was a 404
	// and a real one a 403, which confirmed an unlisted app to a stranger.
	// The handleTransferUserApps shape.
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	var req lockRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	msg := strings.TrimSpace(req.Message)
	if len([]rune(msg)) > messageLimit {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "the message must be " + strconv.Itoa(messageLimit) + " characters or fewer", "hint": "shorten the message to the length the sentence names and send it again"})
		return
	}

	// The lock and its audit row commit together. They used to be two writes
	// with the workload stop between them; see SetAppLock for why a lock is
	// authorization-shaped enough to owe the transaction.
	now := time.Now().UTC()
	meta := requestMeta(r.Context())
	if err := s.db.SetAppLock(app.ID, p.ID, msg, now, app.Slug, meta); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to lock the app"})
		return
	}

	// Stop the workload after the lock is stored, never before. In this order a
	// failure to stop leaves an app that is locked and still running — refused
	// at the proxy, which is the safe half — while the other order would leave
	// one stopped and unlocked, which reads to its owner as a platform fault
	// and is theirs to undo.
	//
	// A stop that fails is logged rather than fatal: the lock is the decision
	// and it has been made. The reaper and the next deploy both honour it, and
	// an administrator retrying gets the same answer.
	// A batch app has no service; its runs are its workload, and the jobs sweep
	// cancels those for a locked app. Writing `stopped` here would strand it,
	// since nothing starts a batch app and unlocking never starts anything.
	if builder.IsBatchFramework(app.Framework) {
		// Nothing to stop.
	} else if rt, rerr := s.runtimeFor(app); rerr == nil {
		// An absent workload is already down, which is what locking was for.
		serr := rt.Stop(app.ProcessName())
		if serr != nil && runtime.AbsenceReason(serr) == "" {
			log.Printf("lock: stopping %s after locking it: %v", app.Slug, serr)
		} else {
			_ = s.db.UpdateAppStatus(app.ID, database.AppStatusStopped)
		}
	}

	fresh, _ := s.db.GetApp(app.ID)
	if fresh == nil {
		fresh = app
	}
	writeJSON(w, http.StatusOK, fresh)
}

// handleUnlockApp — DELETE /apps/{appID}/lock.
//
// Unlocking does not start the app. A lock that lasted three months means the
// release may no longer be one anybody wants running, and starting a workload
// as a side effect of a permission change is how an administrator gets a
// surprise. The app comes back `stopped` and somebody presses Start — which is
// now a button the owner has.
func (s *Server) handleUnlockApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	// Admin first, for the reason handleLockApp gives.
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	app, ok := s.appOr404(w, appID)
	if !ok {
		return
	}

	if err := s.db.ClearAppLock(app.ID, p.ID, app.Slug, app, requestMeta(r.Context())); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to unlock the app"})
		return
	}

	fresh, _ := s.db.GetApp(app.ID)
	if fresh == nil {
		fresh = app
	}
	writeJSON(w, http.StatusOK, fresh)
}

// refuseDeployIfLocked is refuseIfLocked for the two doors into a build —
// deploy and upload — and it admits one lock: the one the dependency policy
// placed.
//
// **A dependency lock is about the release, and a deploy is a new release.**
// Refusing it left the owner who could fix the app with no way to, and the
// administrator — who cannot — as the only exit. The build judges what it
// deploys and lifts the lock only if that release no longer carries what the
// lock acts on (Builder.liftDependencyLock); otherwise the deploy fails and
// the lock stands. An administrator's lock is a decision about the app, not
// its packages, and no deploy answers it. Rollback stays on refuseIfLocked: it
// serves an older release, which is the thing that was flagged.
func (s *Server) refuseDeployIfLocked(w http.ResponseWriter, app *database.App) bool {
	if app.LockedByPolicy() {
		return false
	}
	return s.refuseIfLocked(w, app)
}

// refuseIfLocked is the one enforcement point every mutating path calls.
//
// **The list of callers is the specification**, the same way the dependency
// policy's and the trusted-source policy's are: proxy (serve the message and
// never wake), restart, deploy, upload, auto-deploy, job runs, scheduled task
// runs, and the app's own update (visibility, environment, every PATCH field). A new path that starts or changes an app and does not call this is
// a hole, and there is no second implementation of the question — App.Locked()
// is a method for exactly that reason.
//
// It reports whether the request was refused, so a caller is one `if` rather
// than an error to unwrap:
//
//	if s.refuseIfLocked(w, app) { return }
//
// 409 rather than 403: the caller is very often allowed to do this — the owner
// really may deploy their app — and what stands in the way is the app's state,
// not their permission. A 403 would send them to ask for access they already
// have.
func (s *Server) refuseIfLocked(w http.ResponseWriter, app *database.App) bool {
	if !app.Locked() {
		return false
	}
	// The administrator's own words, not a generic refusal. Whoever is being
	// stopped is usually the owner, and "an administrator locked this app on 4
	// March: decommissioned, use Finance Hub" ends the conversation that
	// "conflict" starts.
	//
	// **Who placed it, said out loud.** The platform can lock an app by itself
	// now, and telling the owner an administrator did it sends them to a person
	// who will not know what they are talking about — the same failure the
	// stored message exists to prevent, one clause earlier. `source` carries
	// the machine-readable half, so a client can say something more useful than
	// the sentence without matching on it.
	by := "an administrator"
	wayOut := ""
	if app.LockedByPolicy() {
		by = "this install's dependency policy"
		// The one lock the owner can answer, and the refusal is where they
		// learn how: every path but a deploy is closed to them.
		wayOut = " Deploy a release that no longer carries the package and the lock lifts, " +
			"or ask an administrator for a waiver."
	}
	settings, _ := s.db.GetPlatformSettings()
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":  "this app is locked by " + by + ": " + database.ResolveLockMessage(app, settings) + wayOut,
		"code":   "app_locked",
		"source": app.LockSource(),
		"locked": app.LockedAt.UTC().Format(time.RFC3339),
	})
	return true
}
