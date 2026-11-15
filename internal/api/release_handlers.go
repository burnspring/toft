package api

import (
	"fmt"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
)

// Deleting old builds is how an operator reclaims storage between deploys.
// Retention already bounds each app to a handful of releases, but a handful of
// Python or Node dependency trees is not a small number of bytes, and an app
// that is deployed once and then left alone keeps every one of them forever.
//
// Two rules govern everything here:
//
//   - A deployment owns the release named after it, and nothing else. A
//     transient promote row (and any failed or legacy rollback row) owns no
//     storage at all, so deleting one costs history and frees nothing — the
//     bytes it appears to represent belong to the deployment it promoted.
//   - The live release and the live deployment row can be different rows. A
//     promote makes the source row current again, so the two normally agree;
//     but a promote in flight, and rows written before promotes were folded
//     back onto their source, still split them. Both sides are protected;
//     deleting either would leave a running app with no record of what it is
//     running, or no code to run.
//
// Deletion is irreversible in the way that matters: once a build's directory
// and artifact are gone, it can never be made current again. That is the point
// — it is also the reason promote checks the artifact store before refusing.

// deletionState is what every deletion decision needs: which release the app is
// actually serving, and which row records that deploy.
type deletionState struct {
	liveReleaseID string
	liveRowName   string
	// rollbackSources are the releases that queued or building rollbacks are
	// returning to. Read from the rows, because the rollback that recorded
	// them may still be waiting for the worker.
	rollbackSources map[string]bool
	// jobPinned are the releases an unfinished job run will start from — a
	// queued retry waits with its first attempt's. Unreadable, it is nil and
	// pinnedUnknown says so, which protects every row: a deletion is not
	// worth a guess.
	jobPinned     map[string]bool
	pinnedUnknown bool
}

func (s *Server) deletionStateFor(app *database.App) deletionState {
	pinned, err := builder.PinnedByJobRuns(s.db, app.ID)
	if err != nil {
		log.Printf("reading the releases %s's job runs are pinned to: %v", app.Slug, err)
	}
	return deletionState{
		liveReleaseID: builder.LiveReleaseID(s.cfg.AppsDir, app),
		liveRowName:   app.CurrentRelease,
		jobPinned:     pinned,
		pinnedUnknown: err != nil,
	}
}

// protects reports why a deployment may not be deleted, or "" if it may.
func (st deletionState) protects(app *database.App, d *database.Deployment) string {
	why, _ := st.protection(app, d)
	return why
}

// protection is why a deployment may not be deleted and what would let it be,
// or two empty strings if it may.
//
// **The remedy is the reason's own.** Every refusal once ended "deploy or
// promote another build first", which is the way past exactly two of them: a
// release a job run is pinned to is freed by the run finishing, and deploying
// something else does nothing for it.
func (st deletionState) protection(app *database.App, d *database.Deployment) (why, remedy string) {
	switch {
	case d.Status == database.DeployStatusPending || d.Status == database.DeployStatusBuilding:
		return "that build is still in progress", "wait for it to finish"
	case st.liveReleaseID != "" && d.ID == st.liveReleaseID:
		return "that build is the release this app is serving", "deploy or promote another build first"
	case builder.ReleaseNameIsDeployment(st.liveRowName, d.ID):
		return "that build is this app's current deployment", "deploy or promote another build first"
	case st.rollbackSources[d.ID]:
		return "a rollback to that build is queued", "wait for the rollback to finish"
	case st.jobPinned[d.ID]:
		return "a job run that has not finished will start from that build",
			"wait for the run to finish or cancel it (`toft job runs` lists them, `toft job wait <run>` " +
				"waits for one, `toft job cancel <run>` ends it) — the build can be deleted once no run will start from it"
	case st.pinnedUnknown:
		return "which builds this app's job runs need could not be read", "try again in a moment"
	}
	return "", ""
}

// withRollbackSources reads which releases the in-progress rollbacks among
// `rows` are returning to, so that deleting one of them is refused while the
// rollback waits.
func (st deletionState) withRollbackSources(rows []database.Deployment) deletionState {
	for _, d := range rows {
		if d.SourceDeployID == "" {
			continue
		}
		if d.Status == database.DeployStatusPending || d.Status == database.DeployStatusBuilding {
			if st.rollbackSources == nil {
				st.rollbackSources = map[string]bool{}
			}
			st.rollbackSources[d.SourceDeployID] = true
		}
	}
	return st
}

// reclaimable sums what a deployment's recorded sizes say deleting it frees.
// Unmeasured releases contribute nothing, so the figure understates rather than
// promising space that may not exist.
func reclaimable(d *database.Deployment) (disk, artifact int64) {
	if d.DiskBytes != nil {
		disk = *d.DiskBytes
	}
	if d.ArtifactBytes != nil {
		artifact = *d.ArtifactBytes
	}
	return disk, artifact
}

type deleteReleasesResponse struct {
	Deleted            int   `json:"deleted"`
	FreedDiskBytes     int64 `json:"freed_disk_bytes"`
	FreedArtifactBytes int64 `json:"freed_artifact_bytes"`
	// Kept is what a prune passed over and why, so "deleted: 3" is never read
	// as "only the serving release is left". Absent on a single delete.
	Kept []keptRelease `json:"kept,omitempty"`
}

// keptRelease is one deployment a prune did not delete.
type keptRelease struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Remedy string `json:"remedy"`
}

// handleDeleteDeployment — DELETE /apps/{appID}/deployments/{deployID}.
//
// Removes the build's release from disk and from the artifact store, then the
// row itself. Nothing references a deployment row — a rollback copies its
// source's branch and commit rather than pointing at it — so the only thing
// lost besides the bytes is that build's log.
func (s *Server) handleDeleteDeployment(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appForReleaseWrite(w, r)
	if !ok {
		return
	}

	// Pinned to the app in the URL: authorization was evaluated against that
	// app, so a deployment ID on its own would be an IDOR.
	d, err := s.db.GetDeployment(chi.URLParam(r, "deployID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if d == nil || d.AppID != app.ID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "deployment not found", "hint": "toft deployments lists this app's deployments with their ids"})
		return
	}

	rows, err := s.db.ListDeployments(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if why, remedy := s.deletionStateFor(app).withRollbackSources(rows).protection(app, d); why != "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": why + " — " + remedy,
			"code":  "deployment_protected",
			"hint":  remedy,
		})
		return
	}

	disk, artifactBytes, err := s.deleteRelease(r, app, d)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, deleteReleasesResponse{
		Deleted: 1, FreedDiskBytes: disk, FreedArtifactBytes: artifactBytes,
	})
}

// handleDeleteNonCurrentDeployments — DELETE /apps/{appID}/deployments.
//
// The bulk form of the same operation: everything except the release being
// served, the row that records it, anything still building, and anything a
// queued rollback or an unfinished job run will start from. What it passed over
// is in `kept`, each with its reason and remedy. One failure
// stops the sweep rather than silently skipping — a partial reclaim reported as
// a whole one is how an operator ends up believing space was freed that wasn't.
func (s *Server) handleDeleteNonCurrentDeployments(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appForReleaseWrite(w, r)
	if !ok {
		return
	}

	deployments, err := s.db.ListDeployments(app.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	state := s.deletionStateFor(app).withRollbackSources(deployments)
	resp := deleteReleasesResponse{Kept: []keptRelease{}}
	for i := range deployments {
		d := &deployments[i]
		if why, remedy := state.protection(app, d); why != "" {
			resp.Kept = append(resp.Kept, keptRelease{ID: d.ID, Reason: why, Remedy: remedy})
			continue
		}
		disk, artifactBytes, err := s.deleteRelease(r, app, d)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		resp.Deleted++
		resp.FreedDiskBytes += disk
		resp.FreedArtifactBytes += artifactBytes
	}

	writeJSON(w, http.StatusOK, resp)
}

// deleteRelease removes one build's storage and then its row, in that order:
// storage first so a failure leaves a deployment that still records what it
// still costs and can simply be retried. The reverse order loses the artifact
// key and with it the only way to ever reclaim the object.
func (s *Server) deleteRelease(r *http.Request, app *database.App, d *database.Deployment) (disk, artifactBytes int64, err error) {
	if err := builder.DeleteRelease(s.cfg.AppsDir, s.artifacts, app.ID, d.ID, d.ArtifactKey); err != nil {
		log.Printf("deleting release %s of %s: %v", d.ID, app.Slug, err)
		return 0, 0, fmt.Errorf("failed to delete the release: %w", err)
	}

	if _, err := s.db.DeleteDeployment(app.ID, d.ID); err != nil {
		log.Printf("deleting deployment row %s of %s: %v", d.ID, app.Slug, err)
		return 0, 0, fmt.Errorf("the release was removed but its record could not be deleted")
	}

	disk, artifactBytes = reclaimable(d)
	s.auditRelease(r, app.ID, d, disk, artifactBytes)
	return disk, artifactBytes, nil
}

// auditRelease records the deletion. Best-effort: the storage is already gone,
// and refusing to report a completed deletion because the audit write failed
// helps nobody. The commit is recorded because it is the only thing left that
// identifies what was deleted.
func (s *Server) auditRelease(r *http.Request, appID string, d *database.Deployment, disk, artifactBytes int64) {
	meta := requestMeta(r.Context())
	// Through auditActor, as auditPromote below: whoever the route admits is
	// recorded as what it is, never as a user whose id is something else's.
	actorType, actorID := auditActor(auth.GetPrincipal(r.Context()))
	if err := s.db.WriteAuditRow(database.AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action:     "app.release_deleted",
		TargetType: "app", TargetID: appID,
		Before: jsonString(map[string]any{
			"deployment_id":  d.ID,
			"commit_sha":     d.CommitSHA,
			"branch":         d.Branch,
			"status":         d.Status,
			"artifact_key":   d.ArtifactKey,
			"disk_bytes":     disk,
			"artifact_bytes": artifactBytes,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	}); err != nil {
		log.Printf("releases: audit delete of %s: %v", d.ID, err)
	}
}

// auditPromote records who made an earlier build current. Written when the
// request is accepted, because the transient deployment row that tracks the
// promote is deleted once it succeeds — this is the record that survives.
// Best-effort for the same reason auditRelease is.
func (s *Server) auditPromote(r *http.Request, app *database.App, source *database.Deployment) {
	meta := requestMeta(r.Context())
	// Through auditActor, as every other audit site: a deploy key promoting a
	// release is recorded as the key, not as a user with no id.
	actorType, actorID := auditActor(auth.GetPrincipal(r.Context()))
	if err := s.db.WriteAuditRow(database.AuditEntry{
		ActorType: actorType, ActorID: actorID,
		Action:     "app.release_promoted",
		TargetType: "app", TargetID: app.ID,
		After: jsonString(map[string]any{
			"deployment_id": source.ID,
			"commit_sha":    source.CommitSHA,
			"branch":        source.Branch,
		}),
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
	}); err != nil {
		log.Printf("releases: audit promote of %s: %v", source.ID, err)
	}
}

// appForReleaseWrite resolves the app in the URL and authorizes the caller to
// change its releases. Deleting a build is gated on edit_content, the same
// right that deploys and promotes: anyone who can replace an app's release can
// reclaim the space an old one is holding.
func (s *Server) appForReleaseWrite(w http.ResponseWriter, r *http.Request) (*database.App, bool) {
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return nil, false
	}

	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	if d := auth.Authorize(p, auth.ActionEditContent, obj, s.db); !d.Allow {
		writeAuthzError(w, r, d, readable)
		return nil, false
	}
	return app, true
}
