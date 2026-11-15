package api

import (
	"log"
	"net/http"
	"time"

	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/deploysteps"
)

// The upload phase, which happens here rather than in the builder.
//
// A deploy from a repository is all one worker's job: the platform is handed a
// URL and everything after that, clone included, happens inside the build. An
// uploaded deploy is not. Its first two steps are an HTTP request — bytes
// arriving over somebody's connection, then a ZIP being expanded — and they are
// over before any builder is handed anything.
//
// The row is created before the bytes are read, so those two steps land on a
// deployment rather than nowhere. What that buys, in order of how much it
// matters: an upload that dies half way is a *failed deploy with a reason*
// instead of a bare 400 that leaves no trace; a client that aborts mid-upload
// is handled by the same path, because the read fails; and the timeline on the
// app's page starts where the deploy really started, which on a slow connection
// is the phase that took the longest.
//
// What it costs is that a rejected upload — the wrong file type, a corrupt
// archive — now appears in the app's history. That is the trade, taken
// deliberately: a deploy that failed is a thing that happened, and the person
// who cannot work out why their upload is not live is better served by a row
// that says "unpack: not a ZIP" than by silence.
//
// Everything the platform refuses *before* the row exists — an app that
// deploys from git, a queue that already holds a build, an install that has
// stopped accepting uploads — stays as it was, and leaves no row. A refusal is
// not a deploy.
type uploadPhase struct {
	s      *Server
	deploy *database.Deployment
	steps  *builder.StepLog
	// buildLog is what the deploy's log shows if the upload never reaches a
	// builder. Without it the row's only account of itself would be a status.
	buildLog string
}

// beginUpload creates the deployment and enters the first step of it. It
// answers the caller itself, and returns nil when there is nothing to deploy.
// via is how the code arrived — upload, or connect for a Posit publish — and
// the deployment's app.deploy row names whoever made the request.
func (s *Server) beginUpload(w http.ResponseWriter, r *http.Request, app *database.App, branch, via string) *uploadPhase {
	deploy := &database.Deployment{
		AppID:       app.ID,
		Branch:      branch,
		Status:      database.DeployStatusPending,
		TriggeredBy: database.DeployTriggerManual,
	}
	stampDeployer(r, deploy)
	if err := s.db.CreateDeployment(deploy, s.deployRecord(r, via)); err != nil {
		log.Printf("Error creating deployment: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create deployment"})
		return nil
	}
	s.noteGarthDeploy(r, deploy.AppID)
	u := &uploadPhase{s: s, deploy: deploy, steps: builder.NewStepLog(s.db, s.builder, deploy)}
	u.enter(builder.StepReceive)
	return u
}

// enter moves to the next step, recording it and telling anything watching.
func (u *uploadPhase) enter(step builder.BuildStep) {
	u.buildLog += "==> " + uploadStepNarration[step] + "\n"
	u.steps.Enter(step)
}

// uploadStepNarration is what the build log says about a step that produced no
// output of its own. A deploy whose log is empty until a build worker picks it
// up reads as a deploy that has not started.
var uploadStepNarration = map[builder.BuildStep]string{
	builder.StepReceive: "Receiving the upload...",
	builder.StepUnpack:  "Unpacking the archive...",
}

// fail ends the deploy here, records why on the row, and answers the caller.
//
// The message is written to the build log as well as returned, because the two
// have different audiences and only one of them is still connected: the caller
// reads the response, and everyone who looks at the app afterwards reads the
// row.
//
// **Both halves of why, exactly as the builder records them.** This is the
// other path a deploy can fail on, and for a long time it was the one that
// classified nothing: the status said `failed` and `failure_reason` stayed
// empty, which every surface is required to read as *unclassified* — a row
// written before the column existed. So a corrupt archive uploaded today was
// indistinguishable from a deploy that failed in 2025, `toft` sent the caller to
// the build log by the unknown-reason fallback rather than because anything had
// decided that, and `receive` and `unpack` — the two steps reachable only from
// here — had a `reasonOf` entry that nothing could produce.
//
// The reason is the step's own default rather than a constant, for the reason
// `builder.fail` takes it from there too: a new failure site in this phase is
// classified without having to know this exists.
func (u *uploadPhase) fail(w http.ResponseWriter, status int, msg string) {
	u.failAs(w, status, "", msg, nil)
}

// failAs is fail with the reason named rather than read off the step, and
// fields beside the error in the answer — a manifest refusal's code and hint.
func (u *uploadPhase) failAs(w http.ResponseWriter, status int, reason deploysteps.Reason, msg string, extra map[string]any) {
	now := time.Now().UTC()
	u.deploy.Status = database.DeployStatusFailed
	u.deploy.BuildLog = u.buildLog + "ERROR: " + msg + "\n"
	u.deploy.FinishedAt = &now
	if err := u.s.db.UpdateDeployment(u.deploy); err != nil {
		log.Printf("Warning: recording failed upload %s: %v", u.deploy.ID, err)
	}

	// Recorded before the frame is published, so a watcher that reconnects the
	// instant the upload ends reads the same answer off the row that the live
	// frame carried. A dedicated setter because UpdateDeployment does not write
	// these columns — the same split SetDeploymentSizes has.
	if reason == "" {
		reason = deploysteps.ReasonOf(u.deploy.CurrentStep)
	}
	// The refusal's own code, when the caller named one (the upload's
	// preflight: removed_key, storage_unknown) — kept on the row beside the
	// reason, as the builder keeps its own.
	code, _ := extra["code"].(string)
	if err := u.s.db.SetDeploymentFailure(u.deploy.ID, string(reason), code, msg); err != nil {
		log.Printf("Warning: recording the failure of deployment %s: %v", u.deploy.ID, err)
	}

	// Before Close, so the terminal frame names the step the deploy died in
	// rather than nothing at all. The step comes off the row the StepLog keeps
	// current, so there is no second copy of "where are we" to disagree with it.
	failure := builder.NewFailureEvent(u.deploy.ID, builder.BuildStep(u.deploy.CurrentStep), reason, msg)
	failure.Code = code
	u.s.builder.PublishEvent(failure)
	u.steps.Close()

	// The reason rides the answer as well as the row: `toft deploy --json` parses
	// it before anything else, as it does a build's, and the create route's
	// refusal of the same tree carries it too.
	body := map[string]any{"error": msg, "deployment_id": u.deploy.ID, "reason": string(reason)}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

// handOff closes the upload's own record and returns the deployment for the
// build queue. The builder continues the same row, timings included, and writes
// the log itself from the first line of the build — which is why the narration
// above is kept in memory and only ever persisted by fail().
func (u *uploadPhase) handOff() *database.Deployment {
	u.steps.Close()
	return u.deploy
}

// deployRecord is who is starting a build on this request — a person, or a
// deploy key as itself — and how its code came in (database.DeployRecord).
func (s *Server) deployRecord(r *http.Request, via string) *database.DeployRecord {
	actorType, actorID := auditActor(auth.GetPrincipal(r.Context()))
	meta := requestMeta(r.Context())
	return &database.DeployRecord{
		ActorType: actorType, ActorID: actorID,
		RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
		Via: via,
	}
}

// stampDeployer writes the requesting principal onto a deployment about to be
// created: who asked, which the builder judges toft.toml's storage changes
// against (ADR § Storage somebody else can attach). Every API door that creates
// a deployment calls it, a rollback included — a rollback is judged like a
// manual deploy, by whoever asked for it.
func stampDeployer(r *http.Request, d *database.Deployment) {
	p := auth.GetPrincipal(r.Context())
	switch {
	case p == nil || p.IsAnonymous():
	case p.IsDeployKey():
		d.DeployedByType, d.DeployedByID = database.DeployerDeployKey, p.DeployKeyID
	case p.IsServiceAccount():
		// Its owning user: a service account is a claim on a token, with no
		// row a build could read later, and D3 judges it through that user.
		d.DeployedByType, d.DeployedByID = database.DeployerServiceAccount, p.OwningUserID
	default:
		d.DeployedByType, d.DeployedByID = database.DeployerUser, p.ID
	}
}

// deployVia is how a redeploy's code arrives: seeded from an upload, from the
// gallery an extension follows, or cloned from git.
func deployVia(app *database.App) string {
	switch {
	case builder.IsUploadedSource(app.RepoURL):
		return "upload"
	case app.GalleryItemID != "":
		return "gallery"
	}
	return "git"
}
