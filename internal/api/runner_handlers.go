package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"toft/internal/appint"
	"toft/internal/applog"
	"toft/internal/artifact"
	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/jobs"
	"toft/internal/resource"
	"toft/internal/runner"
	"toft/internal/runtime"
	"toft/internal/schedule"
	"toft/internal/textutil"
	"toft/internal/toolchain"
	"toft/sdk"
)

// The platform side of the runner contract.
//
// These endpoints are reached only by workloads, never by browsers or users,
// and they authenticate with run-scoped credentials rather than sessions.
//
// Two different kinds, and the difference is who issued them. Bootstrap — the
// only route that hands anything sensitive back — takes a credential the
// *infrastructure* issued and the platform verifies: a projected ServiceAccount
// token, bound to the pod, scoped to an audience the API server itself refuses.
// Every other route takes the run token the platform minted and handed to app
// code, whose audience is load-bearing in exactly the way it always was.
// Widening any of these to accept the other undoes the credential narrowing the
// runner is built around.
//
// They live outside the /api/v1 authenticated group for the same reason: the
// session middleware has nothing to say about a workload, and running these
// through it would only create the temptation to accept a session here.

// runTokenGrace is how long a run's token outlives its deadline — long enough
// for the runner to flush what the run printed after the platform stopped it
// (the host backend kills ten seconds after SIGTERM; a pod's grace is similar).
const runTokenGrace = 2 * time.Minute

// runnerAuth extracts and validates a workload credential for one audience.
//
// Returning the same opaque 401 for every failure is deliberate. A workload
// that presents the wrong token learns only that it was rejected — telling it
// *why* would confirm which audiences exist and which endpoints accept which,
// and app code holds one of these tokens.
func (s *Server) runnerAuth(w http.ResponseWriter, r *http.Request, audience string) *auth.RunClaims {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil
	}

	claims, err := s.jwt.ValidateRunToken(strings.TrimPrefix(header, "Bearer "), audience)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil
	}

	// Version skew is reported rather than tolerated: runner and platform
	// deploy independently, and a partial upgrade must fail loudly instead of
	// half-working in ways nobody traces back to it.
	if v := r.Header.Get(runner.VersionHeader); v != "" {
		if err := runner.CheckVersion(v, s.cfg.RunnerVersion); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "hint": "run a runner image built from this install's version — rebuild or re-pull the runner images, then restart the app"})
			return nil
		}
	}
	return claims
}

// handleRunnerBootstrap hands a workload everything it needs to run.
//
// This is the only place an app's decrypted environment leaves the platform,
// and it is a *response* rather than an injected value on purpose: a pod spec
// or an ECS task override is readable by anyone with describe permission, and
// ECS caps overrides at about 8KB besides.
// bootstrapAuth verifies a workload's identity with the infrastructure that
// issued it, and answers with the app and workload the *platform* labelled that
// pod with.
//
// Nothing here is a claim the workload made. The old shape — a JWT the platform
// minted, carrying the app id, travelling through the workload — was the
// defect: it had to expire, it sat in an immutable pod spec, and so an in-place
// container restart could never succeed. This asks the cluster who is calling
// and then reads the answer off the pod's own labels.
//
// Absent an isolated backend there is nothing to verify against, and the route
// says so with the same opaque 401 as every other failure: an install with no
// cluster has no workloads, and a caller here is not one.
func (s *Server) bootstrapAuth(w http.ResponseWriter, r *http.Request) *auth.RunClaims {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil
	}

	if v := r.Header.Get(runner.VersionHeader); v != "" {
		if err := runner.CheckVersion(v, s.cfg.RunnerVersion); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "hint": "run a runner image built from this install's version — rebuild or re-pull the runner images, then restart the app"})
			return nil
		}
	}

	// A host run's credential first (ADR §106). It is the platform's own
	// signature, so it is checked here rather than asked of a backend, and it
	// is good once: the run records its bootstrap, and a second presentation of
	// the same file — by anything else on the machine that could read it after
	// the runner did — is refused.
	if claims, err := s.jwt.ValidateRunToken(strings.TrimPrefix(header, "Bearer "), auth.AudienceHostBootstrap); err == nil {
		if claims.RunID == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return nil
		}
		first, err := s.db.MarkJobRunBootstrapped(claims.RunID)
		if err != nil || !first {
			if err != nil {
				log.Printf("runner bootstrap: recording host bootstrap of %s: %v", claims.RunID, err)
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return nil
		}
		return &auth.RunClaims{AppID: claims.AppID, WorkloadName: claims.WorkloadName,
			RunID: claims.RunID, Host: true}
	}

	// A shared service's identity, issued by the shared-mode agent for one
	// process and good only while that process lives (ADR
	// §130). Asked of the agent, as a pod's is asked of the cluster: nothing the
	// caller says is trusted, and a token the agent does not recognise falls
	// through to the cluster, which is where a pod's goes.
	if shared, ok := s.rts.Shared().(runtime.IdentityVerifier); ok && shared != nil {
		if id, err := shared.VerifyWorkloadIdentity(r.Context(), strings.TrimPrefix(header, "Bearer ")); err == nil {
			return &auth.RunClaims{AppID: id.AppID, WorkloadName: id.WorkloadName, Host: true}
		}
	}

	verifier, ok := s.rts.Isolated().(runtime.IdentityVerifier)
	if !ok || verifier == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil
	}

	id, err := verifier.VerifyWorkloadIdentity(r.Context(), strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		// Logged, because unlike a workload guessing at an audience this is
		// nearly always a misconfiguration an operator has to see — a missing
		// ServiceAccount, an unbound system:auth-delegator, a cluster that does
		// not surface the pod binding.
		log.Printf("runner bootstrap: %v", err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil
	}

	// Shaped as RunClaims so everything downstream of here is unchanged. The
	// fields mean more than they did: they are facts about a pod rather than
	// assertions inside a token.
	return &auth.RunClaims{
		AppID:        id.AppID,
		WorkloadName: id.WorkloadName,
		RunID:        id.RunID,
	}
}

// releaseForWorkload answers "which release is *this pod* meant to run", which
// is not the same question as "which release is this app serving" — and the
// difference is a deadlock if you answer the second one.
//
// A pod bootstraps during step 8 of its own deploy, while the builder is
// waiting for it to become ready. `apps.release_ref` is not written until step
// 9, the flip, which is deliberately after that wait: the app row is a
// statement about what is *serving*, and a deploy that failed must not leave it
// naming a release no visitor ever reached. So on the first isolated deploy of
// an app the row is still empty, and reading it here refused the pod the very
// artifact the build had just published — after which readiness could not
// succeed, the flip never ran, and the next attempt failed the same way.
//
// The workload's name carries the deployment it was started for, so the release
// key is derivable without the app row and without any column the flip owns:
// artifact.ReleaseKey is a pure function of (app, deployment). A promote is
// covered by the same rule, because its workload is named after the *source*
// deployment — which is exactly the artifact the builder points a promote at.
//
// ArtifactBytes is the published-or-not test: the builder records it
// immediately after a successful publish and before the start, so it is the one
// fact about an artifact that is already true when a pod asks. (ArtifactKey is
// written at the flip and is therefore useless here — the same trap one column
// over.) Falling back to the app's own reference keeps every later start —
// restart, wake, boot reconcile — reading exactly what it read before.
func (s *Server) releaseForWorkload(app *database.App, workloadName string) runtime.ReleaseRef {
	if d := s.deploymentForWorkload(app, workloadName); d != nil && d.ArtifactBytes != nil {
		return runtime.ArtifactRelease(artifact.ReleaseKey(app.ID, d.ID))
	}
	return runtime.ParseReleaseRef(app.ReleaseRef)
}

// deploymentForWorkload is the release one workload belongs to, or nil.
//
// The name carries the deployment it was started for, so a workload resolves to
// its own row without reading a single column the flip owns — which is what
// makes it answerable *during* a deploy, when the app row is still describing
// the release being replaced. Nil when the name matches nothing: a workload
// named before the rule above existed, or a deployment row since pruned.
//
// A scan of one app's deployments rather than a lookup, because the name is
// lossy (builder.DeploymentForWorkload). It reads the narrow
// projection rather than whole rows: this runs per request from a workload, and
// a deployment row carries its whole build log.
func (s *Server) deploymentForWorkload(app *database.App, workloadName string) *database.DeploymentRelease {
	if workloadName == "" {
		return nil
	}
	deploys, err := s.db.ListDeploymentReleases(app.ID)
	if err != nil {
		return nil
	}
	return builder.DeploymentForWorkload(deploys, app.Slug, workloadName)
}

func (s *Server) handleRunnerBootstrap(w http.ResponseWriter, r *http.Request) {
	claims := s.bootstrapAuth(w, r)
	if claims == nil {
		return
	}

	var req runner.BootstrapRequest
	json.NewDecoder(r.Body).Decode(&req)

	app, ok := s.appOr404(w, claims.AppID)
	if !ok {
		return
	}

	// The token names the workload it was minted for. A token replayed by some
	// other workload gets nothing, even though it is otherwise valid.
	if claims.WorkloadName != "" && req.WorkloadName != "" && claims.WorkloadName != req.WorkloadName {
		// Logged, because the two names disagreeing is a platform defect far
		// more often than a replay: the one time it happened, every first-party
		// guide on an isolated install crash-looped on this line with nothing
		// anywhere saying why.
		log.Printf("runner bootstrap: %s presented a token bound to workload %q", req.WorkloadName, claims.WorkloadName)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// A pod's first contact with the platform is a workload's narration
	// beginning, which is what reopens its per-start budget: a container
	// restarted in place bootstraps again, and the fifth restart of a crash
	// loop must not be silenced by the four before it.
	//
	// Not for a run — a run's lines go to job_run_logs and never reach the
	// platform log at all.
	if claims.RunID == "" {
		s.platformLog.StartOf(claims.AppID, claims.WorkloadName)
	}

	envVars, err := s.decryptedEnv(app.ID)
	if err != nil {
		log.Printf("runner bootstrap: decrypting env for %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := runner.BootstrapResponse{
		Version:     s.cfg.RunnerVersion,
		AppID:       app.ID,
		AppSlug:     app.Slug,
		BasePath:    builder.ServingBasePath(app),
		Env:         envVars,
		Port:        8080,
		BindAddress: runtime.BindAll,
	}

	// Framework, entrypoint, and start command are what a *service* workload
	// needs to know what to exec. There is no release directory here to
	// re-detect them from, so they come from what the build recorded — on the
	// workload's own deployment. (The app row used to be rewritten at the
	// next build's detect and never reverted by a failed one, so a pod of the
	// serving release restarted mid-build was told the new release's
	// entrypoint; since F-6 the row is written at the flip alone, and this
	// read stays on the deployment because it is the workload's own.) The app
	// row answers only for a deployment that predates the columns.
	resp.Framework, resp.Entrypoint, resp.StartCmd = app.Framework, app.Entrypoint, app.StartCmd
	//
	// The verified identity's name first, the body's only when the identity
	// carries none (the check above has made them agree when both are set).
	// The same name keys the app token and the role key below: keyed by the
	// body's alone, a runner that omitted it got a token under "" that
	// superseded nothing and a role key the gateway never signs with.
	workload := claims.WorkloadName
	if workload == "" {
		workload = req.WorkloadName
	}
	d := s.deploymentForWorkload(app, workload)
	if d != nil && d.Framework != "" {
		resp.Framework, resp.Entrypoint, resp.StartCmd = d.Framework, d.Entrypoint, d.StartCmd
	}
	// The path this workload's own release was built for, by the same rule:
	// mid-move, a new release's pod bootstraps before the flip, and the app
	// row still names the old release's path.
	if d != nil && d.BasePath != nil {
		resp.BasePath = *d.BasePath
	}

	// Which SDK this platform carries, so the runner can say whether the one it
	// is about to install is the same. See BootstrapResponse.SDKBuild: the two
	// binaries embed their own copies and upgrade separately, and until this
	// field nothing compared them.
	if lang := builder.LanguageOf(builder.Framework(resp.Framework)); lang != "" {
		resp.SDKBuild = sdk.BuildOrEmpty(lang)
	}

	// What this install declares the image carries. The runner checks it
	// against its own filesystem and refuses to start a workload the
	// declaration lied about — the confirm half of decide-then-confirm.
	//
	// Sent from the *declaration* rather than from the deployment's recorded
	// resolved version, because the two answer different questions: the
	// recorded version says what the artifact was built against, and this says
	// what the platform believes the image can run. A disagreement between the
	// declaration and the image is exactly what this is for, and reading the
	// deployment row would hide it behind an agreement that was assumed.
	if lang := builder.LanguageOf(builder.Framework(resp.Framework)); lang != "" {
		if declared := toolchain.Configured(lang); len(declared) > 0 {
			resp.RuntimeLanguage = lang
			// The whole declaration, spelled as the variable spells it: the
			// runner requires every series in it, so an image that carries the
			// default and none of the rest is caught here rather than by the
			// first app unlucky enough to resolve to one of the others.
			versions := make([]string, 0, len(declared))
			for _, v := range declared {
				versions = append(versions, v.Version)
			}
			resp.RuntimeVersion = strings.Join(versions, ",")
		}
		// Where an R repair installs from. Only for R, because it is the only
		// family whose in-pod repair reaches a package repository at all: a
		// venv is rebuilt from a requirements.txt against the index uv already
		// knows, and node_modules from a registry npm already knows, while
		// renv's source is an install-level decision the pod cannot infer.
		if lang == toolchain.R {
			resp.RRepos = s.envelope().RRepos
		}
	}

	ctx := r.Context()
	// A service has no run to be bounded by, so it gets the service TTL and
	// exchanges it — see handleRunnerToken. A job replaces this below with its
	// own deadline.
	timeout := runner.ServiceTokenTTL

	// The release a job run was queued against, when it recorded one. A job's
	// workload is named after the run rather than a deployment, so the name
	// cannot carry the release the way a service's does — and the fallback below
	// would hand it the app's *current* release, which a deploy landing while the
	// run sat in the queue has already moved.
	runRelease := ""

	// A job run carries its command, parameters, and I/O; a service workload
	// carries none of that and must not be handed job-shaped fields.
	if claims.RunID != "" {
		run, err := s.db.GetJobRun(claims.RunID)
		if err != nil || run == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found", "hint": "start the run again; GET /api/v1/apps/{id}/job-runs lists this app's runs"})
			return
		}
		if run.AppID != app.ID {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		runRelease = run.ReleaseRef
		resp.RunID = run.ID
		resp.JobName = run.JobName
		resp.Trigger = run.StartedBy
		resp.RerunOf = run.RerunOf
		resp.TriggerRole = s.triggerRoleFor(app, run)
		resp.Params = run.Params
		// A run has no endpoint: it is one-shot work, and handing it a port
		// would invite an app to start listening on it.
		resp.Port, resp.BindAddress = 0, ""

		def, _ := s.db.GetJobDefinition(app.ID, run.DefinitionID)
		if run.ScheduledFor != nil {
			resp.ScheduledFor = scheduledForIn(*run.ScheduledFor, def)
			// An instant a person named with an offset reaches the job as
			// that text, not as the schedule's zone or UTC (BB-045, fourth
			// 2026-11-03 black-box list).
			if database.ScheduledOffset(run.ScheduledFor) != "" {
				resp.ScheduledFor = run.ScheduledFor.Format(time.RFC3339)
			}
		}
		if def != nil {
			resp.Command = def.Command
			resp.Args = def.Args
		}
		// The effective timeout, the same figure the dispatcher stored as the
		// run's deadline: the declaration, or the install's default, under the
		// install's ceiling.
		settings, _ := s.db.GetPlatformSettings()
		timeout = jobs.EffectiveLimits(def, settings, s.envelope()).Timeout()
		resp.TimeoutSeconds = int(timeout.Seconds())
		if run.DeadlineAt != nil {
			resp.Deadline = run.DeadlineAt.UTC().Format(time.RFC3339)
		}

		// Input and output are presigned URLs, never credentials: one that
		// could read this run's input could read every other app's data.
		//
		// The output URL is unconditional: a job decides whether to write an
		// output, and the place to write one has to exist before it decides.
		// Params are a run's only input; large input is the app's storage.
		if p, ok := artifact.PresignerOf(s.artifacts); ok {
			if u, err := p.PresignPut(ctx, artifact.RunOutputKey(app.ID, run.ID, "output"), artifact.RunURLTTL(timeout)); err == nil {
				resp.OutputURL = u
			}
		}
	}

	// A host run reads its release from this machine's disk, resolved and
	// repaired by the backend that started it, so there is no URL to sign and
	// no image to check a toolchain against: the toolchain is this machine's,
	// the one that built the release.
	if claims.Host {
		resp.RuntimeLanguage, resp.RuntimeVersion = "", ""
		// Nor an address: a host workload's is its placer's fact — the port
		// the shared backend allocated, on loopback — and the runner is given
		// it beside this (runner.EnvPort). The pod's 0.0.0.0:8080 here would
		// put a shared app on every interface.
		resp.Port, resp.BindAddress = 0, ""
	} else if !s.bootstrapRelease(ctx, w, app, claims.WorkloadName, runRelease, &resp) {
		return
	}

	// The narrowed credential, minted last. Its life is the run's timeout and
	// a short grace past it: the run is stopped at its deadline, and what it
	// printed in its last seconds — the lines that explain a timeout — is
	// shipped by the runner *after* the stop. With the token dying exactly at
	// the deadline those batches were refused, and a timed-out run kept no
	// output at all (one with a five-second timeout never shipped a line,
	// since the first batch leaves at five seconds). A finished run's token is
	// still inert where it matters: the app surface refuses it by the run's
	// status (run_not_in_flight), and a late result cannot overwrite the
	// terminal one the platform already wrote.
	tokenTTL := timeout
	if claims.RunID != "" {
		tokenTTL += runTokenGrace
	}
	runToken, err := s.jwt.MintRunToken(auth.RunTokenParams{
		RunID:        claims.RunID,
		AppID:        app.ID,
		WorkloadName: claims.WorkloadName,
		TTL:          tokenTTL,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	resp.RunToken = runToken

	// The app's own outbound credential, minted **last** — after every refusal
	// above, because minting supersedes the previous start's token and a
	// bootstrap that then 409s would have revoked a live credential on behalf
	// of a workload that never started.
	//
	// A **service** workload gets one; a job does not, and that asymmetry is
	// ADR §27 D1 rather than an oversight: the argument that a scheduled task
	// must be an HTTP trigger rests on a job workload holding no app token, so
	// a report a job produces has nothing able to mail it.
	//
	// Minted here rather than injected at deploy because by the time a pod
	// bootstraps the secret is gone — it is hashed on the way in and cannot be
	// re-derived. That is why an isolated app had no platform identity at all
	// until now, and why a woken or restored one lost the identity it had.
	if claims.RunID == "" {
		resp.AppToken = s.mintWorkloadToken(app, workload)
		// The key this workload verifies X-Toft-Role with. Derived rather
		// than minted, so unlike the token above it costs no row and supersedes
		// nothing — a pod that bootstraps twice for the same workload name gets
		// the same key both times, which is what lets kubelet restart a
		// container in place without every assertion in flight going stale.
		//
		// A job gets none: it serves no requests, so it has nothing to verify.
		resp.RoleKey = s.jwt.RoleKey(app.ID, workload)
	}

	resp.DependencyRefusal = s.dependencyVerdictFor(app)

	writeJSON(w, http.StatusOK, resp)
}

// handleRunnerToken exchanges a **service** workload's cluster-issued identity
// for a fresh run token, and is the whole answer to a workload outliving its
// credential.
//
// A run token's life is its run's timeout, deliberately: a token from a
// finished run is then inert. A service has no run, so it was given an hour
// instead — and because the runner bootstraps exactly once, that hour was a
// cliff. Sixty minutes after any isolated app started, its consumption reports
// and its stage marks began answering 401 and never recovered: no CPU or memory
// reading for the rest of the workload's life, and an error in the pod's log
// every five seconds until somebody restarted it.
//
// The exchange keeps every property the design had. The runner still holds the
// credential the *cluster* issued and kubelet rotates, which is what this
// verifies — so the run token stays short-lived, nothing long-lived is minted
// into a pod spec, and a leaked run token is still worth less than an hour. The
// alternative, a long TTL for a service, would have removed the cliff by giving
// away exactly the property the short life was chosen for.
//
// It is not a second bootstrap and must never become one. It answers with a
// token and nothing else: re-running bootstrap would decrypt the app's
// environment again, reopen the workload's narration budget, and mint a fresh
// app token — superseding the one the process already running in this pod holds
// in its environment, which would break a live app to refresh a credential it
// was not using.
func (s *Server) handleRunnerToken(w http.ResponseWriter, r *http.Request) {
	claims := s.bootstrapAuth(w, r)
	if claims == nil {
		return
	}

	// A run's credential is bounded by the run. Exchanging one would undo the
	// property that makes a finished run's token inert, which is the reason
	// this route exists for services and not for runs.
	if claims.RunID != "" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "a run's token expires with the run and is not exchanged", "hint": "use the run's token until the run ends; a new run gets a new one",
		})
		return
	}

	// The identity names the workload it was issued to. A caller asking for
	// some other workload's token gets nothing, exactly as at bootstrap.
	var req runner.TokenRequest
	json.NewDecoder(r.Body).Decode(&req)
	if claims.WorkloadName != "" && req.WorkloadName != "" && claims.WorkloadName != req.WorkloadName {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	token, err := s.jwt.MintRunToken(auth.RunTokenParams{
		AppID:        claims.AppID,
		WorkloadName: claims.WorkloadName,
		TTL:          runner.ServiceTokenTTL,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, runner.TokenResponse{
		Version: s.cfg.RunnerVersion, RunToken: token,
	})
}

// dependencyVerdictFor is the stored verdict for the release this workload is
// about to run — never a fresh evaluation (ADR §39 D5).
//
// This is the sharpest edge in the whole feature. An isolated pod is evicted,
// rescheduled, OOM-killed or rolled by a node drain and bootstraps again with
// no new code, no new release, and nothing anybody did; a scheduled job or an
// app-started run bootstraps a workload every time it fires. If any of those
// were judged against *today's* corpus, an app that has been serving happily
// since March would simply not come back — at 3am, because a third party
// published an advisory — and a nightly report would stop on a Tuesday for a
// reason nobody can see. That is precisely what "a serving app is never
// stopped by this feature" forbids, arriving through the one door it does not
// watch.
//
// So a release that was allowed to deploy stays runnable for as long as it is
// the release. What new information changes is the next deploy, plus what the
// tab, the estate view and the events say in the meantime.
func (s *Server) dependencyVerdictFor(app *database.App) string {
	deployID, err := s.db.ServingDeploymentID(app.ID)
	if err != nil || deployID == "" {
		return ""
	}
	v, ok, err := s.db.GetDeploymentVerdict(deployID)
	if err != nil || !ok {
		// No verdict recorded: a release that predates the feature, or one
		// built while the deploy scan was off. Unknown is not a refusal —
		// refusing a running app on no evidence is the failure this design is
		// written against.
		return ""
	}
	if v.Allowed {
		return ""
	}
	return v.Reason
}

// decryptedEnv returns an app's environment in the clear: its own variables
// and the credentials an administrator attached, as the builder assembles them
// for a shared workload (getDecryptedEnvVars).
//
// **The credentials are this function's job too.** An isolated workload — a
// service or a job run — takes its environment from the bootstrap, not from
// the builder, and this read only the app's own variables: on EKS an attached
// credential never reached an isolated app at all (ENV.14), while the same app
// in shared mode had it. An attach refuses a key the app also declares, so the
// merge has no precedence to decide.
func (s *Server) decryptedEnv(appID string) (map[string]string, error) {
	vars, err := s.db.GetEnvVars(appID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(vars))
	for _, v := range vars {
		plain, err := s.encryptor.Decrypt(v.Value)
		if err != nil {
			return nil, err
		}
		out[v.Key] = plain
	}
	brokered, err := s.db.AppCredentialValues(appID, s.encryptor.Decrypt)
	if err != nil {
		return nil, err
	}
	for k, v := range brokered {
		out[k] = v
	}
	return out, nil
}

// handleRunnerLogs ingests a batch of shipped output.
func (s *Server) handleRunnerLogs(w http.ResponseWriter, r *http.Request) {
	claims := s.runnerAuth(w, r, auth.AudienceRun)
	if claims == nil {
		return
	}

	var batch runner.LogBatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLogBatchBytes)).Decode(&batch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid batch", "hint": "send the log batch as one JSON object, from a runner built from this install's version"})
		return
	}

	// A service has no run to attach lines to, and it used to have nowhere
	// else either: this branch formatted every line into the platform's own
	// process log, one Printf at a time, and moved on. `claims.AppID` was
	// already in hand — read, once, to compose the line it was about to throw
	// away — which is what made the attachment point ADR §8 said was missing
	// turn out to have been there all along.
	//
	// **The platform's half is kept; the app's is dropped.** A current runner
	// no longer ships a service's own output at all (the pod's log is where it
	// belongs), so an app-labelled line here comes from a pod carrying the old
	// runner and its home is that pod's stdout, unchanged. An *unlabelled*
	// line is an older runner's too — every line it wrote was the app's, since
	// it labelled nothing — which is why ContractVersion does not move for this
	// and no fleet-wide redeploy is owed. Each workload fixes itself on its
	// next deploy.
	if claims.RunID == "" {
		s.storePlatformLines(claims, batch)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	lines := make([]database.JobLogLine, 0, len(batch.Lines))
	for _, l := range batch.Lines {
		// A job that prints its environment prints its presigned URLs, and
		// the log outlives them; the signature never reaches the table.
		lines = append(lines, database.JobLogLine{
			Seq: l.Seq, At: l.At, Stream: l.Stream, Line: artifact.MaskSignedURLs(l.Line),
		})
	}
	// The runner reports its own drops. Recording them as a line keeps the
	// truncation visible to whoever reads the log later, which is the whole
	// reason the runner counts them rather than dropping silently.
	// At the seq of the line before it, not the next: the runner spends no
	// seq on a drop, so the next real line took the notice's number and a
	// follower that had read the notice skipped it (code review 2026-11-11,
	// H-8). The log is read in (seq, id) order, so the notice follows its
	// line and precedes the next.
	if batch.Dropped > 0 {
		lines = append(lines, database.JobLogLine{
			Seq:    lastSeq(batch.Lines),
			At:     time.Now().UTC(),
			Stream: "stderr",
			Line:   "… " + strconv.Itoa(batch.Dropped) + " line(s) dropped: this run exceeded the log rate cap",
		})
	}

	if err := s.db.AppendJobRunLogs(claims.RunID, lines); err != nil {
		log.Printf("runner logs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.copyRunLines(claims, lines)
	w.WriteHeader(http.StatusNoContent)
}

// copyRunLines puts a shared run's lines on the platform's stream, after they
// are stored: a batch the runner resends after a 500 is copied once per
// stored copy and never ahead of one. Only for a shared run — an isolated
// run's lines are its pod's stdout already, and a copy here would be the
// duplicate the isolated mode refuses. One primary-key read per batch.
func (s *Server) copyRunLines(claims *auth.RunClaims, lines []database.JobLogLine) {
	if s.appOutput == nil || len(lines) == 0 {
		return
	}
	run, err := s.db.GetJobRun(claims.RunID)
	if err != nil || run == nil || run.ExecutionMode != string(runtime.ModeShared) {
		return
	}
	for _, l := range lines {
		at := l.At
		s.appOutput.Line(runtime.OutputLine{
			AppID: run.AppID, Workload: "job:" + run.JobName, Stream: l.Stream,
			At: &at, Line: l.Line, RunID: run.ID, Job: run.JobName,
		})
	}
}

// mintWorkloadToken issues this workload start its own outbound credential and
// supersedes the previous start's.
//
// Best-effort by design, exactly like the deploy path's: an app whose token
// could not be minted still serves traffic, it simply cannot call
// `/api/v1/app/*`, and the SDK says so by name. Refusing to bootstrap — which
// means refusing to start the app — over a capability it may not use would be
// the wrong trade.
func (s *Server) mintWorkloadToken(app *database.App, workload string) string {
	secret, hash, prefix, err := appint.MintAppToken()
	if err != nil {
		log.Printf("runner bootstrap: minting an app token for %s: %v", app.Slug, err)
		return ""
	}
	id := uuid.NewString()
	if err := s.db.CreateAppToken(&database.AppToken{
		ID: id, AppID: app.ID, Workload: workload, TokenHash: hash, Prefix: prefix,
	}); err != nil {
		log.Printf("runner bootstrap: recording an app token for %s: %v", app.Slug, err)
		return ""
	}
	// Only this workload's earlier starts. A different release serving beside
	// it during a deploy keeps its own token and keeps working.
	if err := s.db.SupersedeWorkloadTokens(app.ID, workload, id); err != nil {
		log.Printf("runner bootstrap: superseding app tokens for %s: %v", app.Slug, err)
	}
	return secret
}

// storePlatformLines keeps what a service's runner said in the platform's own
// voice, and drops what its app printed.
//
// The runner's own count of what *it* dropped is relayed as a line, the way
// the run path relays it: truncation counted out loud is the whole reason the
// runner counts it rather than dropping silently. The platform's per-start cap
// applies underneath this — it is the platform's count and not the runner's,
// because a compromised runner could ignore its own.
func (s *Server) storePlatformLines(claims *auth.RunClaims, batch runner.LogBatch) {
	if s.platformLog == nil {
		return
	}
	w := s.platformLog.For(claims.AppID, claims.WorkloadName, "")
	for _, l := range batch.Lines {
		if l.Source != runner.LogSourcePlatform {
			continue
		}
		w.Say(l.Stream, l.Line)
	}
	if batch.Dropped > 0 {
		w.Sayf(applog.StreamStderr,
			"… %d line(s) dropped: this workload exceeded the log rate cap", batch.Dropped)
	}
}

const maxLogBatchBytes = 4 << 20

func lastSeq(lines []runner.LogLine) int {
	n := 0
	for _, l := range lines {
		if l.Seq > n {
			n = l.Seq
		}
	}
	return n
}

// handleRunnerConsumption ingests a workload's self-reported figures.
//
// This is the isolated collector's other half. It folds into exactly the same
// recorder the shared collector feeds, so the storage and reporting layers
// never learn which mode produced a number — which is what makes a mixed
// install comparable.
func (s *Server) handleRunnerConsumption(w http.ResponseWriter, r *http.Request) {
	claims := s.runnerAuth(w, r, auth.AudienceRun)
	if claims == nil {
		return
	}

	var report runner.ConsumptionReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSmallReportBytes)).Decode(&report); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid report", "hint": "send the report as one JSON object, from a runner built from this install's version"})
		return
	}

	// The peak travels on every report, so a run the kernel kills keeps the
	// figure that explains why.
	if claims.RunID != "" {
		peak := report.PeakBytes
		if report.MemoryBytes > peak {
			peak = report.MemoryBytes
		}
		if err := s.db.NoteJobRunPeakMemory(claims.RunID, int((peak+(1<<20)-1)>>20)); err != nil {
			log.Printf("runner consumption: recording the peak of %s: %v", claims.RunID, err)
		}
		if report.ThrottledSeconds != nil {
			if err := s.db.NoteJobRunThrottled(claims.RunID, *report.ThrottledSeconds); err != nil {
				log.Printf("runner consumption: recording the throttling of %s: %v", claims.RunID, err)
			}
		}
	}
	var throttling *resource.Throttling
	if report.Periods != nil && report.ThrottledPeriods != nil {
		throttling = &resource.Throttling{Periods: *report.Periods, ThrottledPeriods: *report.ThrottledPeriods}
		if report.ThrottledSeconds != nil {
			throttling.ThrottledSeconds = *report.ThrottledSeconds
		}
	}

	s.consumption.Record(resource.Sample{
		AppID:    claims.AppID,
		JobRunID: claims.RunID,
		// The workload name distinguishes one run of a workload from the next,
		// so a restart reads as a counter reset rather than a glitch.
		WorkloadID:  claims.WorkloadName,
		CPUSeconds:  report.CPUSeconds,
		MemoryBytes: report.MemoryBytes,
		Throttling:  throttling,
		ObservedAt:  report.ObservedAt,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunnerProgress(w http.ResponseWriter, r *http.Request) {
	claims := s.runnerAuth(w, r, auth.AudienceRun)
	if claims == nil {
		return
	}
	if claims.RunID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only a job run has progress", "hint": "report progress only from a job run; a service has no progress to report"})
		return
	}

	var report runner.ProgressReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSmallReportBytes)).Decode(&report); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid report", "hint": "send the report as one JSON object, from a runner built from this install's version"})
		return
	}
	if err := s.db.SetJobRunProgress(claims.RunID, report.Percent, report.Message); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRunnerOutputURL mints the calling run's output PUT URL at the moment
// it writes (runner.OutputURLPath).
//
// The bootstrap URL is signed once for the run's timeout capped at PresignTTL,
// and on S3 behind a role with a temporary credential that can expire sooner,
// so a long run's write at the end found it dead (security review 2026-11-14,
// AS-4). The key is fixed here to the run's own output — the caller names
// nothing — the life is minutes, and a run that has finished is refused, so the
// run token is the whole of the authority and it reaches one object.
func (s *Server) handleRunnerOutputURL(w http.ResponseWriter, r *http.Request) {
	claims := s.runnerAuth(w, r, auth.AudienceRun)
	if claims == nil {
		return
	}
	if claims.RunID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only a job run has an output",
			"hint": "write an output only from a job run; a service has no output location"})
		return
	}
	run, err := s.db.GetJobRun(claims.RunID)
	if err != nil {
		log.Printf("runner output-url: reading %s: %v", claims.RunID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if run == nil || run.AppID != claims.AppID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found",
			"hint": "this run token names a run this install no longer holds"})
		return
	}
	if database.JobRunTerminal(run.Status) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this run has finished, so it can no longer write an output",
			"hint": "write the output before the job returns"})
		return
	}
	p, ok := artifact.PresignerOf(s.artifacts)
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "this install's artifact store cannot presign URLs, so a run has no output location",
			"code":  "no_object_store", "hint": "ask the operator who runs this install to configure an object store (ARTIFACT_STORE=s3)"})
		return
	}
	u, err := p.PresignPut(r.Context(), artifact.RunOutputKey(run.AppID, run.ID, "output"), artifact.OutputURLTTL)
	if err != nil {
		log.Printf("runner output-url: presigning for %s: %v", run.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, runner.OutputURLResponse{
		URL:       u,
		Headers:   map[string]string{runner.OutputBlobTypeHeader: "BlockBlob"},
		ExpiresAt: time.Now().Add(artifact.OutputURLTTL).UTC().Format(time.RFC3339),
	})
}

// The bodies a run's token may post. A result carries a reason (bounded below
// to maxReportedReasonBytes); consumption and progress are a few numbers and a
// line.
const (
	maxResultReportBytes   = 1 << 20
	maxSmallReportBytes    = 64 << 10
	maxReportedReasonBytes = 4000
)

// normalizeResultReport bounds what a job's own code can make of its result.
// The reason becomes the run's failure reason, the event's summary and the
// mail subject: clipped, cleaned of NUL and invalid UTF-8 (a NUL made Postgres
// refuse the row and the run never finished by its report). The stage is the
// event's fingerprint, so a value the runner never sends — a fresh one every
// run would defeat the cooldown — is read as the app's own failure.
func normalizeResultReport(report *runner.ResultReport) {
	report.Reason = textutil.Clip(strings.TrimSpace(report.Reason), maxReportedReasonBytes)
	switch report.Stage {
	case "", runner.StageImagePull, runner.StageRuntimeVersion, runner.StageArtifactFetch,
		runner.StageBuild, runner.StageAppExec, runner.StageCallbackUnreachable:
	default:
		report.Stage = runner.StageAppExec
	}
}

// handleRunnerResult records a run's outcome.
//
// The status mapping is where the runner's stage classification becomes the
// platform's vocabulary, and it preserves the distinctions a developer needs:
// an out-of-memory kill is not a crash, and an image that would not pull is
// not the app's fault at all.
func (s *Server) handleRunnerResult(w http.ResponseWriter, r *http.Request) {
	claims := s.runnerAuth(w, r, auth.AudienceRun)
	if claims == nil {
		return
	}

	// Bounded: a job's own code holds the run token (TOFT_JOB_TOKEN) and may
	// post this report itself, so its size is the app's choice, not the
	// runner's.
	var report runner.ResultReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxResultReportBytes)).Decode(&report); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid report", "hint": "send the report as one JSON object, from a runner built from this install's version"})
		return
	}
	normalizeResultReport(&report)

	if claims.RunID == "" {
		// A service workload exiting is the backend's business, not a run
		// outcome; there is nothing to record.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if report.RecordOnly {
		// A mid-run report from an older SDK's set_result, which no longer
		// stores anything: a run's one output is write_output. The run goes on.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Record whether this run left an output object behind.
	//
	// The runner PUTs to a presigned URL, so the platform never sees the write
	// and cannot learn of it from the report — adding a field to the report
	// would be a contract version bump for a fact one HEAD already answers.
	// Asked here, once, when the run ends.
	//
	// The column is what lets /output tell "produced no artifact" from
	// "produced one that has since expired", and saying *expired* honestly is
	// the whole reason for the distinction. A run that died without reporting
	// never reaches this and is treated as having produced nothing, which is
	// the right answer for a workload the kernel killed mid-write.
	if s.artifacts != nil {
		key := artifact.RunOutputKey(claims.AppID, claims.RunID, "output")
		// Recorded unless the store says the object is not there: a blip at
		// result time dropped the key for good, the run read "produced no
		// output" for ever and the object was never expired (H-7).
		if _, err := s.artifacts.Stat(r.Context(), key); err == nil || !errors.Is(err, artifact.ErrNotFound) {
			if err := s.db.SetJobRunOutputKey(claims.AppID, claims.RunID, key); err != nil {
				log.Printf("runner result: recording the output of %s: %v", claims.RunID, err)
			}
		}
	}

	status := statusForReport(report)
	exit := report.ExitCode
	reason := report.Reason
	if s.jobs != nil {
		// The backend's measured peak first: this report usually closes the
		// run before any poll sees it again, and an out-of-memory reason quotes
		// the figure.
		s.jobs.NoteBackendPeak(r.Context(), claims.RunID)
		reason = s.jobs.ReasonFor(claims.RunID, status, &exit, reason)
	}
	moved, err := s.db.FinishJobRunOnce(claims.RunID, status, &exit, string(report.Stage), reason)
	if err != nil {
		log.Printf("runner result: finishing %s: %v", claims.RunID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// The report is the usual way a run ends, so this is where its outcome is
	// usually recorded: the owners' notification, the job's own notify, the
	// metric. Only when this write moved the row, so a run is reported once.
	if moved && s.jobs != nil {
		if run, err := s.db.GetJobRun(claims.RunID); err == nil && run != nil {
			s.jobs.RecordOutcome(run, status, string(report.Stage), reason)
		}
	}
	if s.jobs != nil {
		s.jobs.Forget(claims.RunID)
	}

	// The run's window is left open: the runner sends one last reading on
	// its way out, after this result, and Flush drops a window nothing has
	// sampled once it has gone quiet. Forgetting it here threw away the last
	// interval — every short run recorded no CPU at all (code review
	// 2026-11-11, H-9).
	w.WriteHeader(http.StatusNoContent)
}

// statusForReport maps a runner's outcome onto a run status.
func statusForReport(report runner.ResultReport) string {
	if report.ExitCode == 0 && report.Stage == "" {
		return database.JobRunSucceeded
	}
	switch report.Stage {
	case runner.StageImagePull, runner.StageArtifactFetch, runner.StageCallbackUnreachable,
		runner.StageBuild, runner.StageAppExec:
		// Failed, with the stage saying whose problem it is: the registry, the
		// object store and the network are not the app's, and a reader told
		// "artifact_fetch" does not go looking in their own code.
		return database.JobRunFailed
	}
	// 137 is SIGKILL, which for a container almost always means the kernel's
	// OOM killer. Distinct from a crash because the fix is a bigger limit
	// rather than a code change.
	if report.ExitCode == 137 {
		return database.JobRunOutOfMemory
	}
	return database.JobRunFailed
}

// triggerRoleFor is the *who* beside a run's *how*: what the person who started
// this run may do to this app, or the platform's own word when no person did.
//
// Derived here rather than recorded on the row, and that is ADR-level rather
// than incidental — job_runs.trigger and .actor_id are already the provenance,
// and a stored role would be a second answer that ages. The cost is stated: the
// role is *today's*, so a run started by an editor who has since been demoted
// reports viewer. For the thing this feeds — a job deciding what work to do —
// current is the right answer; the audit question is answered by actor_id,
// which does not move.
func (s *Server) triggerRoleFor(app *database.App, run *database.JobRun) string {
	switch {
	case run.StartedBy == database.JobStartedBySchedule:
		// The platform firing a schedule. Kept apart from `app` because the run
		// table already distinguishes them, and collapsing them at the edge
		// would make the header disagree with the history.
		return auth.RoleSystem
	case run.ActorID == "":
		// The app acting as itself — an app-started run with no viewer relayed,
		// which is what the absence of an act-for blob has always meant.
		return auth.RoleApp
	}
	p, err := s.db.PrincipalForUser(run.ActorID)
	if err != nil || p == nil {
		// A person deleted since the run was queued. Anonymous is the honest
		// answer and the safe one: it is the bottom of the ladder, so a job
		// gating on a role refuses rather than proceeding on a guess.
		return auth.RoleAnonymous
	}
	role, err := auth.CallerRole(p, objectFromApp(app), s.db)
	if err != nil {
		// Somebody who may only list the app cannot have started a job on it, so
		// this is unreachable — and if it is reached, anonymous is the same
		// answer the deleted-person case gives above: the bottom of the ladder,
		// which a job gating on a role refuses.
		return auth.RoleAnonymous
	}
	return role
}

// scheduledForIn writes a run's instant in its schedule's zone, so one string
// answers both "which day" and "in whose calendar": 2026-10-13T03:00:00+01:00.
// A definition with no zone, or none at all any more, writes UTC.
func scheduledForIn(at time.Time, def *database.JobDefinition) string {
	if def != nil {
		if zone, _ := schedule.Split(def.Schedule); zone != "" {
			if loc, err := time.LoadLocation(zone); err == nil {
				return at.In(loc).Format(time.RFC3339)
			}
		}
	}
	return at.UTC().Format(time.RFC3339)
}

// bootstrapRelease puts a pod's release into the response: a presigned URL and
// the digest the runner checks it against. It answers false, having written the
// refusal, when there is no release a pod can fetch.
func (s *Server) bootstrapRelease(ctx context.Context, w http.ResponseWriter, app *database.App,
	workloadName, runRelease string, resp *runner.BootstrapResponse) bool {
	// The release, as a presigned URL plus the digest the runner verifies it
	// against before executing any of it.
	ref := s.releaseForWorkload(app, workloadName)
	if runRelease != "" {
		ref = runtime.ParseReleaseRef(runRelease)
	}
	if ref.Kind != runtime.ReleaseArtifact {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this app has no published release artifact — an isolated workload " +
				"cannot read APPS_DIR and must fetch its release. Redeploy the app with an " +
				"artifact store configured.", "hint": "redeploy the app once the operator has configured an artifact store, or run it in shared mode",
		})
		return false
	}
	p, ok := artifact.PresignerOf(s.artifacts)
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this install's artifact store cannot presign URLs; isolated workloads " +
				"need an object store (ARTIFACT_STORE=s3)", "hint": "the operator who runs this install has to configure an object store (ARTIFACT_STORE=s3), or run the app in shared mode",
		})
		return false
	}
	url, err := p.PresignGet(ctx, ref.Value, artifact.PresignTTL)
	if err != nil {
		log.Printf("runner bootstrap: presigning %s: %v", ref.Value, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return false
	}
	desc, statErr := s.artifacts.Stat(ctx, ref.Value)
	if runRelease != "" && errors.Is(statErr, artifact.ErrNotFound) {
		// Release retention deleted it while the run waited. Said here rather
		// than left to the fetch, whose 404 inside the pod would read as a
		// store fault; and never quietly swapped for the current release, which
		// is the substitution this branch exists to stop.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the release this run was queued against is no longer stored — " +
				"release retention removed it. Start the run again to use the current release.", "hint": "start the run again; it will use the app's current release",
		})
		return false
	}
	resp.ArtifactURL = url
	if statErr == nil {
		resp.ArtifactSHA256 = desc.SHA256
	}

	return true
}
