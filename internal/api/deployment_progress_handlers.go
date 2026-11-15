package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"toft/internal/builder"
	"toft/internal/database"
)

// Watching one deploy: the two routes something outside a browser needs.
//
// The SPA has never needed either. It holds the whole deployment list already,
// refetches it while a build is in flight, and reads the build log off a
// WebSocket — so "where is my deploy" has always been answerable by re-reading
// a list. A CLI has a narrower question and a worse position from which to ask
// it: it holds one deployment ID, it wants that deployment's state, and it is
// on somebody's laptop wifi. Refetching an app's entire history once a second
// to find one row in it is the wrong shape for that.
//
// Both are reads of a build's own state, so both take the same authorization as
// the list they come from — logsAction, which is stricter than read: a build's
// step names and its failure messages are as much about the app's code as its
// log output is, and a viewer on a public app has no business with either.
// authorizeDeployment pins the deployment to the app in the URL, so a
// deployment ID from one app cannot be read through another.

// heartbeatInterval keeps a stream that has nothing to say alive through
// proxies that drop idle connections, and is how a dead client is noticed on a
// deploy whose build step takes ten minutes to produce anything.
const heartbeatInterval = 20 * time.Second

// handleGetDeployment answers with one build, in the same shape the list uses.
//
// The extra fields matter to the caller that has one ID: `current` and
// `serving` are what tell a CLI that its deploy is the one actually running,
// which after a promote the newest-successful-build guess gets wrong.
func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	app, deploy, ok := s.authorizeDeployment(w, r)
	if !ok {
		return
	}

	// Through the same filter the list applies, so one endpoint cannot show a
	// build the other has already decided is not a version of anything.
	views := s.deploymentViews(app, []database.Deployment{*deploy})
	if len(views) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "deployment not found", "hint": "toft deployments lists this app's deployments with their ids"})
		return
	}
	writeJSON(w, http.StatusOK, views[0])
}

// handleDeployProgressStream streams one deploy's phase and step changes as
// server-sent events, and ends when the deploy does.
//
// SSE rather than the WebSocket the build log uses. This is a one-way stream of
// small JSON frames to a client that may be curl, and SSE needs no upgrade, no
// framing library and no origin dance to consume from a shell. The log stream
// stays exactly as it is: its framing is already a contract with the SPA, and
// interleaving typed frames into it would make every consumer of either parse
// both.
//
// **The terminal frame is the end signal.** A client stops when it reads a
// frame whose status is success or failed; the server closes the connection
// after sending it. A stream that is merely quiet is a deploy still working —
// which is why the heartbeat exists, and why nothing here times a build out.
func (s *Server) handleDeployProgressStream(w http.ResponseWriter, r *http.Request) {
	deploy, ok := s.authorizeDeploymentLogs(w, r)
	if !ok {
		return
	}

	flusher, canStream := w.(http.Flusher)
	if !canStream {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// nginx and friends buffer a response body by default, which for a
	// progress stream means it arrives all at once when the deploy is over.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event builder.ProgressEvent) bool {
		payload, err := json.Marshal(event)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: progress\ndata: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	watch := s.builder.SubscribeProgress(deploy.ID)
	defer s.builder.UnsubscribeProgress(deploy.ID, watch)

	// Re-read after subscribing, not before: a step change between the read and
	// the subscribe would otherwise be the one frame nobody receives.
	// Authorization already used the earlier copy.
	//
	// **No row is no snapshot.** A plain rollback's row is deleted once the
	// restore succeeds, before its success frame is published; a watcher
	// authorized before the delete and subscribed after the frame was sent the
	// stale pre-subscribe copy — `building` — and then only heartbeats, for
	// ever. Ending the stream instead sends the client back to reconnect, where
	// it gets the 404 and settles it against the deployments list (security
	// review 2026-11-14, CS-1).
	current, _ := s.db.GetDeployment(deploy.ID)
	if current == nil {
		return
	}
	if !send(builder.SnapshotProgress(current)) {
		return
	}
	// A deploy that was already over when the watcher arrived — a CLI
	// reconnecting after a dropped connection, or a fast build. The snapshot
	// above was its terminal frame.
	if current.Status == database.DeployStatusSuccess || current.Status == database.DeployStatusFailed {
		return
	}

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-watch:
			if !open {
				return
			}
			if !send(event) {
				return
			}
			if event.Terminal() {
				return
			}
		case <-heartbeat.C:
			// An SSE comment: it keeps the connection warm and is ignored by
			// every client, so nothing has to know what to do with it.
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
