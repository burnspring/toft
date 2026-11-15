package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"toft/cli/internal/bundle"
	"toft/cli/internal/client"
	"toft/cli/internal/config"
	"toft/cli/internal/ui"
)

const deployUsage = `toft deploy [dir] — bundle this directory and deploy it

Sends the working directory as it is, uncommitted edits included. For an app
backed by a repository this is still what happens, and the app is not converted
by it: the repository wins next, and the next deploy from it replaces this code.

Flags:
  --app <slug>        Which app
  --logs              Interleave the build log
  --no-watch          Start the deploy and return; print the deployment id
  --watch <deployID>  Rejoin a deploy that was started earlier (6+ characters
                      of the id are enough)
  --json              NDJSON progress frames on stdout, then one result object
  --yes               Do not ask anything
  --dry-run           Check instead of deploying — the same as toft check
  --verify <path>     Once it succeeds, request this path as you and fail
                      unless it answers 2xx or 3xx (see toft fetch)
  --verify-timeout <d> How long a 5xx may be retried with --verify (default 30s)
`

// deployMaxBytes is the API's cap, checked here so a bundle nobody can accept
// is refused before it travels.
const deployMaxBytes = 100 << 20

func cmdDeploy(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("deploy", deployUsage)
	g.register(fs)
	showLogs := fs.Bool("logs", false, "interleave the build log")
	e.verifyFlags(fs)
	noWatch := fs.Bool("no-watch", false, "start the deploy and return")
	dryRun := fs.Bool("dry-run", false, "check instead of deploying: toft check")
	watchID := fs.String("watch", "", "rejoin a deploy already running")
	if err := parseOperands(fs, args, 1); err != nil {
		return skipHelp(err)
	}
	if e.verify != "" {
		if err := refuseAbsoluteURL(e.verify); err != nil {
			return err
		}
		if err := refuseDotSegments("/" + strings.TrimPrefix(e.verify, "/")); err != nil {
			return err
		}
	}
	if *noWatch && e.verify != "" {
		return usageCodef("conflicting_flags", "--verify needs the deploy watched; drop --no-watch")
	}

	if fs.NArg() > 0 {
		e.dir = fs.Arg(0)
	}
	s, err := e.setup(&g)
	if err != nil {
		if *dryRun {
			return manifestCheckRefusal(e, &g, err)
		}
		return err
	}
	// `toft check` by another name, so it decides what to check the way
	// check does: an unlinked directory is checked as a new app rather than
	// refused for having no app to deploy to.
	if *dryRun {
		return checkDirectory(e, s, &g, "")
	}

	appValue, err := s.app()
	if err != nil {
		return err
	}
	app, err := s.resolveApp(appValue.Value)
	if err != nil {
		return err
	}
	e.noteInstallVersion(s)

	// Rejoining a deploy started earlier — a dropped connection, a second
	// terminal, or `--no-watch` followed by second thoughts.
	if *watchID != "" {
		e.rejoined = true
		// A prefix copied from `toft deployments list` names the deployment
		// here as it does for `logs --deploy` and `deployments delete`.
		id, err := resolveDeploymentID(s, app.ID, *watchID)
		if err != nil {
			return err
		}
		return watchDeploy(e, s, app, id, &g, *showLogs)
	}

	deployID, err := uploadBundle(e, s, app, &g)
	if err != nil {
		return err
	}

	if *noWatch {
		if g.json {
			return startedDocument(e, s, app, deployID)
		}
		e.out.Answer("%s", deployID)
		e.out.Dim("Rejoin it with: toft deploy --watch %s", deployID)
		return nil
	}
	// Nothing serving yet is nothing to roll back to: a failed --verify on
	// such an app names the log, not `toft rollback` (BB-045).
	if app.CurrentRelease == "" {
		firstDeploys[app.Slug] = true
		defer delete(firstDeploys, app.Slug)
	}
	return watchDeploy(e, s, app, deployID, &g, *showLogs)
}

// withDeployment is a frame naming another deployment.
func withDeployment(f client.ProgressEvent, id string) client.ProgressEvent {
	f.DeploymentID = id
	return f
}

// startedDocument is what `--no-watch --json` answers: the deploy started and
// nothing more is known, said as one object -- the shape `toft create
// --no-watch --json` already answers. A bare id on stdout was not JSON, so
// `jq` failed on the one mode built for scripts.
func startedDocument(e *env, s *session, app *appSummary, deployID string) error {
	doc := map[string]any{
		"app":           app.Slug,
		"app_id":        app.ID,
		"url":           s.publicURL(app),
		"deployment_id": deployID,
	}
	// What this client saw in the bundle is known before anything is built,
	// so an unwatched deploy says it as a watched one does (BB-017). The
	// install's own warnings come with the build, which nothing here waits
	// for.
	warnings, details := clientWarnings(e)
	if len(warnings) > 0 {
		doc["warnings"], doc["warning_details"] = warnings, details
	}
	return json.NewEncoder(e.out.Stdout).Encode(doc)
}

// clientWarnings are this client's own warnings about the bundle it sent —
// an env file left out, a secret-looking file sent — with their codes.
func clientWarnings(e *env) ([]string, []any) {
	var warnings []string
	var details []any
	for _, f := range e.bundleEnvFiles {
		msg := envFileSkippedWarning(f)
		warnings = append(warnings, msg)
		details = append(details, map[string]any{"code": "env_file_skipped", "path": f, "message": msg})
	}
	for _, secret := range e.bundleSecrets {
		msg := secretWarning(secret)
		warnings = append(warnings, msg)
		details = append(details, map[string]any{"code": "secret_in_bundle", "path": secret, "message": msg})
	}
	for _, dir := range e.bundleWorkspaceLeftOut {
		msg := workspaceLeftOutWarning(dir)
		warnings = append(warnings, msg)
		details = append(details, map[string]any{"code": "go_work_dir_excluded", "table": "go.work", "path": dir, "message": msg})
	}
	return warnings, details
}

// appSummary is the part of an app's page this client reads.
type appSummary struct {
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
	// RepoSubdir is the folder of RepoURL the app is built from; empty is
	// the repository root.
	RepoSubdir string `json:"repo_subdir"`
	// PublicURL is where the install says the app answers now — its own
	// hostname once a move to one has finished. Empty from an install older
	// than the field, or one with no apps origin.
	PublicURL  string `json:"public_url"`
	Status     string `json:"status"`
	AutoDeploy bool   `json:"auto_deploy"`
	// Framework is what the app is; a batch-* app serves nothing (ADR §107).
	Framework string `json:"framework"`
	// CurrentRelease is the deployment serving now, empty before the first
	// successful deploy. An uploaded app redeploys from it (toft redeploy).
	CurrentRelease string `json:"current_release"`
	// LockedAt is the platform lock (ADR §54), nil when the app is not locked.
	LockedAt *time.Time `json:"locked_at,omitempty"`
}

// locked reports whether an administrator (or the dependency policy) has
// locked the app.
func (a *appSummary) locked() bool { return a != nil && a.LockedAt != nil }

// resolveApp turns a slug into the app, through the by-slug route rather than
// by fetching the estate and filtering it: this runs on every command, from a
// laptop.
func (s *session) resolveApp(slug string) (*appSummary, error) {
	var body struct {
		App appSummary `json:"app"`
	}
	err := s.client.Do(http.MethodGet, "/api/v1/apps/by-slug/"+url.PathEscape(slug)+"?view=brief", nil, &body)
	if err != nil {
		var apiErr *client.Error
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			// 404 covers "no such app" and "not yours", deliberately, and this
			// message has to be true of both.
			return nil, s.noSuchApp(slug)
		}
		return nil, err
	}
	return &body.App, nil
}

// noSuchApp is the refusal for a slug the install does not show this caller,
// naming the layer the slug was read from when it was not typed on this command
// line. A stale uncommitted .toft.toml overriding the committed toft.toml read
// as "the app does not exist" with nothing pointing at the file that said so;
// the resolver records the origin for exactly this (docs/bugs
// 2026-10-22-e2e-cli #4).
func (s *session) noSuchApp(slug string) error {
	from := ""
	if s.resolver != nil && !s.slugTyped {
		if v := s.resolver.App(s.InstallURL); v.Value == slug && v.Origin != "" && v.Origin != "--app" {
			from = " (read from " + v.Origin + ")"
		}
	}
	return usageCodef("not_found", "no app called %q here%s, or you cannot see it — `toft apps` lists what you can", slug, from)
}

// uploadBundle builds the archive, says what is in it, and sends it.
func uploadBundle(e *env, s *session, app *appSummary, g *globalFlags) (string, error) {
	built, err := bundle.Build(e.dir, bundle.Options{KeepPaths: staticRoots(e.dir)})
	if err != nil {
		return "", err
	}
	defer os.Remove(built.Path)

	archive, err := os.Open(built.Path)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return "", err
	}

	where := describeDir(e.dir)
	e.out.Say("%s %s from %s, %d %s", e.out.Bold(app.Slug),
		e.out.Dimmed("←"), where, len(built.Files), plural(len(built.Files), "file", "files"))
	// A manifest may name the install, and the deploy does not ask (ADR §42):
	// what stands in for the question is saying where it is going and why,
	// before the upload, so a cloned tree cannot retarget a deploy unseen.
	if inst := s.resolver.Install(); manifestOrigin(inst.Origin) {
		e.out.Dim("to %s — the install %q is named by %s", s.InstallURL, inst.Value, inst.Origin)
	}

	// The divergence, said at the point of use. Somebody deploying a working
	// tree at an app whose page names a repository should meet that fact here,
	// not later when a colleague's push replaces their code.
	if app.RepoURL != "" && !strings.HasPrefix(app.RepoURL, "upload://") {
		e.out.Dim("This app deploys from %s — the next deploy from %s replaces what you are pushing.",
			app.RepoURL, orDefault(app.Branch, "its branch"))
	}

	for _, skipped := range built.EnvFiles {
		e.out.Dim("not uploading %s — toft env set --from-file %s sends its values", skipped, skipped)
	}
	// One dimmed line, never a refusal: a key in the tree is sometimes the
	// app's own.
	for _, secret := range built.Secrets {
		e.out.Dim("including %s", secret)
	}
	e.bundleSecrets, e.bundleEnvFiles = built.Secrets, built.EnvFiles
	e.bundleWorkspaceLeftOut = built.WorkspaceLeftOut
	for _, dir := range built.WorkspaceLeftOut {
		e.out.Warn("%s", workspaceLeftOutWarning(dir))
	}

	if info.Size() > deployMaxBytes {
		return "", tooBig(e, built, info.Size())
	}

	var deploy struct {
		ID string `json:"id"`
	}
	err = s.client.Upload(fmt.Sprintf("/api/v1/apps/%s/upload", app.ID),
		app.Slug+".zip", archive, info.Size(),
		func(sent, total int64) { e.out.Meter("upload", sent, total) },
		&deploy)
	e.out.ClearLine()
	if err != nil {
		return "", err
	}
	return deploy.ID, nil
}

// stepTimings reads what the deployment row already knows about steps that are
// over. Silent on failure: this is what makes the display complete, not what
// makes it correct, and a deploy whose timings could not be read is still a
// deploy worth watching.
func (s *session) stepTimings(appID, deployID string) []ui.StepTiming {
	var row struct {
		StepTimings string `json:"step_timings"`
	}
	path := fmt.Sprintf("/api/v1/apps/%s/deployments/%s", appID, deployID)
	if err := s.client.Do(http.MethodGet, path, nil, &row); err != nil || row.StepTimings == "" {
		return nil
	}
	var timings []ui.StepTiming
	if err := json.Unmarshal([]byte(row.StepTimings), &timings); err != nil {
		return nil
	}
	return timings
}

// tooBig refuses, and names the five largest paths — because the answer is
// nearly always one directory nobody meant to include.
func tooBig(e *env, built *bundle.Result, compressed int64) error {
	var b strings.Builder
	fmt.Fprintf(&b, "the bundle is %s, over this install's %s limit. The largest paths in it:",
		ui.FormatBytes(compressed), ui.FormatBytes(deployMaxBytes))
	for i, f := range built.Files {
		if i == 5 {
			break
		}
		fmt.Fprintf(&b, "\n    %-10s %s", ui.FormatBytes(f.Size), f.Rel)
	}
	b.WriteString("\n  Add what should not ship to .gitignore or .toftignore.")
	return &usageError{msg: b.String(), code: "bundle_too_large"}
}

// watchProgress is the progress watch both watchers use: WatchProgress, with
// the one 404 that is a success settled.
//
// A plain rollback is watched through a row the platform deletes once the
// restore succeeds, and the progress route answers 404 once that row is gone.
// A watcher that connects — or reconnects — after the delete was told
// "deployment not found" and exited 2 for a rollback that worked (security
// review 2026-11-14, CS-1). So on a 404 while a rollback is watched, the
// deployments list decides: when the release the rollback restores is current
// or serving, the rollback succeeded and the watch ends on a terminal frame of
// its own making; otherwise the 404 stands.
func watchProgress(e *env, s *session, app *appSummary, deployID string, onFrame func(client.ProgressEvent)) (client.ProgressEvent, error) {
	final, err := s.client.WatchProgress(e.ctx, app.ID, deployID, onFrame)
	var apiErr *client.Error
	if err == nil || e.servingAfter == "" || !errors.As(err, &apiErr) || !apiErr.NotFound() {
		return final, err
	}
	rows, listErr := s.deployments(app.ID)
	if listErr != nil {
		return final, err
	}
	for _, d := range rows {
		if d.ID == e.servingAfter && (d.Current || d.Serving) {
			frame := client.ProgressEvent{
				DeploymentID: deployID,
				Status:       client.StatusSuccess,
				Phase:        "deploy",
				Step:         "flip",
				At:           time.Now(),
			}
			onFrame(frame)
			return frame, nil
		}
	}
	return final, err
}

// watchDeploy follows one deploy to its terminal frame.
func watchDeploy(e *env, s *session, app *appSummary, deployID string, g *globalFlags, showLogs bool) error {
	if g.json {
		return watchAsJSON(e, s, app, deployID)
	}

	// The tail is always collected and only *printed* when asked, or when the
	// deploy fails — at which point it is the only thing that matters, and
	// having to re-run the command to see it would be the wrong answer to the
	// wrong question.
	timeline := ui.NewTimeline(e.out)
	// What finished before this client was watching: the two upload steps, or
	// everything so far on a deploy being rejoined.
	timeline.Seed(s.stepTimings(app.ID, deployID))
	timeline.Start()
	// On a live run the log is printed above the tree, by the program that
	// draws it; off one, to stderr as a line-per-step run always has.
	logs := followBuildLog(e, s, app, deployID, showLogs, timeline.Writer())
	s.client.OnReconnect = func(error) {
		fmt.Fprintln(timeline.Writer(), e.out.Dimmed("lost the connection to "+s.InstallURL+"; retrying"))
	}
	defer func() { s.client.OnReconnect = nil }()
	lastStep := ""
	final, err := watchProgress(e, s, app, deployID, func(frame client.ProgressEvent) {
		// **The log a step wrote lands before the line saying it finished.**
		// The follower polls once a second, so the frame that ends a step
		// usually arrives before the step's last log lines have been fetched,
		// and "✓ build" printed above the end of the build's own output. One
		// fetch at each step boundary, before the marker, puts them in order.
		boundary := showLogs && ((frame.Step != "" && frame.Step != lastStep) || frame.Terminal())
		if boundary {
			// Up to the new step's opening header; the rest after the ✓.
			logs.pullToStep(e, lastStep)
		}
		if frame.Step != "" {
			lastStep = frame.Step
			logs.setStep(frame.Step)
		}
		timeline.Frame(frame.Status, frame.Phase, frame.Step)
		if boundary {
			logs.pull(e)
		}
	})
	timeline.Stop()
	// Stopped here rather than deferred, so the log's last lines land before
	// the summary block rather than after it.
	logs.close(e)

	if err != nil {
		if errors.Is(err, e.ctx.Err()) && e.ctx.Err() != nil {
			// **Ctrl-C does not cancel the deploy**, and says so on the way
			// out. The build is running on the platform and will finish; there
			// is no cancel route for a deploy, and inventing a client-side
			// illusion of one would be worse than the honest sentence.
			e.out.Say("")
			e.out.Say("Stopped watching. The deploy is still running on %s.", s.InstallURL)
			e.out.Say("Rejoin it with: %s", e.out.Bold("toft deploy --watch "+deployID))
		}
		var unreachable *client.UnreachableError
		if errors.As(err, &unreachable) {
			// The watch gave up, not the deploy: the build may well be
			// running, and the same two lines Ctrl-C prints say where to pick
			// it up (security review 2026-11-14, CS-4).
			e.out.Say("")
			e.out.Say("Stopped watching: %s did not answer. The deploy may still be running.", s.InstallURL)
			e.out.Say("Rejoin it with: %s", e.out.Bold("toft deploy --watch "+deployID))
		}
		return err
	}

	elapsed := timeline.Elapsed()
	if e.rejoined {
		if took, ok := s.recordedDuration(app.ID, deployID); ok {
			elapsed = took
		}
	}
	if final.Status == client.StatusFailed {
		printWarnings(e, deployWarningLines(final))
		phase, step := timeline.Where()
		e.out.Say("")
		sentence := failureSentence(phase, step, final.Reason, final.Message)
		if e.out.Quiet {
			// -q drops the narration, not the reason: a failed deploy that
			// exits 1 having printed nothing leaves a script nothing to log.
			e.out.Failed("%s", sentence)
		} else {
			e.out.Say("%s %s", e.out.Fail("✗"), sentence)
		}
		// The build log, unasked, for the failures whose detail is in it —
		// and **not** for the ones whose detail is somewhere else. A refusal
		// has already said everything it has to say, and thirty lines of
		// dependency-resolution output under it buries the sentence that
		// matters. An app that built and then would not answer has a build log
		// ending in "Ready.", which is thirty lines of nothing.
		if !showLogs && whereTheDetailIs(final.Reason) == detailBuildLog {
			logs.printTail(e, 30)
		}
		if next := nextStep(final.Reason, deployID); next != "" {
			e.out.Dim("  %s", next)
		}
		return &deployFailedError{phase: phase, step: step, reason: final.Reason}
	}

	// Before the address, because the address is the answer and this is a
	// caveat on it. A deploy that succeeded is exactly when a warning is
	// easiest to miss: nothing failed, so nothing sends anybody to the build
	// log, and the developer walks away believing they declared something the
	// platform never read.
	printWarnings(e, deployWarningLines(final))

	e.out.Say("")
	if e.out.Rich() && !e.out.Quiet {
		// On a terminal the address is the panel's, not a second line under
		// it: nobody is capturing stdout that is a terminal, and -q — which
		// drops the panel — takes the plain path and still gets the answer.
		where, headline := appWhere(s, app), " is deployed"
		if !isBatchApp(app.Framework) {
			where, headline = e.out.Link(where, where), " is live"
		}
		e.out.Panel(
			e.out.OK("✓ ")+e.out.Bold(app.Slug)+headline,
			"  "+where,
			"  "+e.out.Dimmed(fmt.Sprintf("%s · deployment %s", ui.FormatDuration(elapsed),
				orDefault(e.servingAfter, deployID))),
		)
	} else {
		e.out.Say("    %s  %s", e.out.Bold(app.Slug), e.out.Dimmed(appWhere(s, app)))
		e.out.Say("    %s", e.out.Dimmed(fmt.Sprintf("%s · deployment %s", ui.FormatDuration(elapsed),
			orDefault(e.servingAfter, deployID))))
		e.out.Answer("%s", s.publicURL(app))
	}
	if e.verify != "" {
		waiting := e.out.Waiting(e.ctx, "    asking "+e.verify)
		got, err := verifyDeploy(e.ctx, s, app, e.verify, e.verifyWithin)
		waiting.Stop()
		if err != nil || !got.ok() {
			if got != nil {
				e.out.Say("    %s %s answered %d", e.out.Fail("✗"), e.verify, got.Status)
				if strings.TrimSpace(got.Body) != "" {
					// Every line indented under the ✗, not only the first.
					for _, line := range strings.Split(strings.TrimRight(tailOf(got.Body, 10), "\n"), "\n") {
						// The app's own answer: no escape of it drives the
						// terminal (security review 2026-11-14, CS-5).
						e.out.Dim("    %s", plainText(strings.TrimRight(line, " \t\r")))
					}
				}
			}
			return verifyFailed(e.verify, app.Slug, got, err)
		}
		e.out.Say("    %s %s answered %d", e.out.OK("✓"), e.verify, got.Status)
	}
	return nil
}

// watchAsJSON emits the frames exactly as the server sends them, one per line,
// then one final result object. stdout is the answer; the narration that would
// otherwise be here is suppressed entirely.
//
// **The result object has to be sufficient on its own.** The reader here is a
// program, usually an agent, and it is the one reader that gets no build log:
// the human path prints a tail under a failure without being asked, and this
// path never started the log follower at all. A caller left holding
// `"message": "REFUSED: this install does not deploy packages with known high
// advisories."` knows it failed, knows nothing it can act on, and has no reason
// to suspect there is more. So a failure carries `reason`, the whole message,
// and whichever kind of detail the reason says exists — the build log's tail, or
// the dependency findings themselves.
func watchAsJSON(e *env, s *session, app *appSummary, deployID string) error {
	encoder := json.NewEncoder(e.out.Stdout)
	started := time.Now()
	s.client.OnReconnect = func(error) {
		fmt.Fprintf(e.out.Stderr, "lost the connection to %s; retrying\n", s.InstallURL)
	}
	defer func() { s.client.OnReconnect = nil }()
	final, err := watchProgress(e, s, app, deployID, func(frame client.ProgressEvent) {
		// A rollback watches the platform's own short-lived row, deleted once
		// the restore succeeds; its frames named that row while the result
		// named the restored release, so one answer carried two ids for one
		// deploy (BB-041). The frames name what the result names. The
		// platform's row stays readable as watched_id.
		if e.servingAfter != "" && frame.DeploymentID != "" && frame.DeploymentID != e.servingAfter {
			encoder.Encode(struct {
				client.ProgressEvent
				WatchedID string `json:"watched_id"`
			}{withDeployment(frame, e.servingAfter), frame.DeploymentID})
			return
		}
		encoder.Encode(frame)
	})
	if err != nil {
		return err
	}
	result := map[string]any{
		"deployment_id": deployID,
		// The slug, as documented, and the id beside it — the same two keys
		// every --json result carries: --no-watch answered the whole app
		// object and this a slug, so neither `.app` nor `.app.id` worked in
		// both modes (BB-042).
		"app":     app.Slug,
		"app_id":  app.ID,
		"status":  final.Status,
		"url":     s.publicURL(app),
		"seconds": time.Since(started).Seconds(),
	}
	if e.rejoined {
		if took, ok := s.recordedDuration(app.ID, deployID); ok {
			result["seconds"] = took.Seconds()
		}
	}
	// **The reader that most needs a warning is the one that cannot see the
	// build log.** This path never starts the log follower, so a `[tasks]`
	// table where the file wanted `[jobs]` — silently nothing, by design, so
	// that an older platform can run a newer manifest — reached the build log
	// and stopped there. Reading it cost a second command after every deploy,
	// on the chance that something was ignored.
	//
	// Absent rather than empty when there are none, and absent from an install
	// older than the field: a consumer testing for the key learns "this install
	// does not report warnings" instead of reading [] as "there were none".
	//
	// A secret-looking file in the bundle is one more, said by this client:
	// the text rendering names it (`including server.key`), and a --json caller
	// had no line of it.
	//
	// Each coded warning, as the install sent it — go_toolchain_fetched and
	// the rest, the codes toft check names (BB-037, fourth 2026-11-03 list) —
	// and this client's own two with theirs, path and all, as toft check
	// gives them (L2, 2026-11-04 black-box list).
	warnings := installWarnings(final)
	details := make([]any, 0, len(final.WarningDetails))
	for _, d := range final.WarningDetails {
		details = append(details, d)
	}
	own, ownDetails := clientWarnings(e)
	warnings = append(warnings, own...)
	details = append(details, ownDetails...)
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	if len(details) > 0 {
		result["warning_details"] = details
	}
	if final.Status != client.StatusFailed && e.servingAfter != "" {
		result["deployment_id"] = e.servingAfter
	}
	if final.Status == client.StatusFailed {
		result["phase"] = final.Phase
		result["step"] = final.Step
		// The text without the app's terminal escapes: a crash's message
		// quotes its output, colour codes and all (BB-038, fourth
		// 2026-11-03 list); `toft logs` has the lines as recorded.
		result["message"], _ = jsonLogText(final.Message)
		// Absent rather than empty when the install did not send one: a
		// consumer testing for the key learns "this install does not classify"
		// rather than reading "" as a category.
		if final.Reason != "" {
			result["reason"] = final.Reason
		}
		// **The result is the final, coded answer** — a failed deploy has no
		// second document (report), so this one carries what a refusal's
		// would: a code (the install's, else the reason, else deploy_failed)
		// and retryable false, since the identical deploy fails the same way
		// (BB-018).
		result["code"] = orDefault(final.Code, orDefault(final.Reason, "deploy_failed"))
		result["retryable"] = false
		addFailureDetail(e, s, app, deployID, final.Reason, result)
		// The sentence goes to stderr as plain mode prints it, and the line
		// that names where the rest is becomes the document's hint (BB-084).
		e.out.Failed("%s", failureSentence(final.Phase, final.Step, final.Reason, final.Message))
		if next := nextStep(final.Reason, deployID); next != "" {
			result["hint"] = next
			e.out.Failed("%s", next)
		}
	}
	// In the one result object rather than a second document after it: a
	// verified deploy is still one answer, and a reader that stops at the
	// first object with a `status` must not miss that the app then failed.
	var verifyErr error
	if final.Status != client.StatusFailed && e.verify != "" {
		got, err := verifyDeploy(e.ctx, s, app, e.verify, e.verifyWithin)
		v := map[string]any{"path": e.verify}
		if got != nil {
			v["status"] = got.Status
			v["ok"] = got.ok()
			if !got.ok() {
				v["body"] = got.Body
			}
		}
		if err != nil {
			v["ok"] = false
			v["error"] = err.Error()
		}
		if err != nil || !got.ok() {
			v["next"] = verifyNext(app.Slug)
			verifyErr = verifyFailed(e.verify, app.Slug, got, err)
		}
		result["verify"] = v
	}
	encoder.Encode(result)
	if final.Status == client.StatusFailed {
		return &deployFailedError{phase: final.Phase, step: final.Step, reason: final.Reason}
	}
	return verifyErr
}

// addFailureDetail attaches whatever the reason says the caller still needs.
//
// **Silent on its own errors, always.** Everything here is an extra request
// made after the deploy has already been decided; a store that would not answer
// must degrade the result object, never turn a classified failure into a
// command that errored for a different reason than the one it is reporting.
func addFailureDetail(e *env, s *session, app *appSummary, deployID, reason string,
	result map[string]any) {

	switch whereTheDetailIs(reason) {
	case detailDependencies:
		// The one failure with a structured answer already sitting behind an
		// endpoint. Embedded rather than pointed at, because a refusal an agent
		// has to make a second call to understand is a refusal most agents will
		// report as "the deploy failed".
		if refused := s.refusedDependencies(app.ID); refused != nil {
			result["dependencies"] = refused
		}
	case detailRuntimeLog:
		// Deliberately not fetched. The app's log is a stream with its own
		// command and its own flags, and the release that failed may never have
		// written a line; naming where to look is honest, inventing an excerpt
		// is not.
		result["detail"] = "runtime_log"
	case detailMemory:
		// No log to point at: the app was killed by a signal and printed
		// nothing. The key names the setting that fixes it.
		result["detail"] = "memory_limit"
	case detailBuildLog:
		result["detail"] = "build_log"
		if text, err := s.buildLog(app.ID, deployID); err == nil {
			result["log_tail"], _ = jsonLogText(tailOf(text, 40))
		}
	}
}

// refusedDependencies is the app's dependency state, narrowed to the deploy
// this policy just turned away. Nil when the install has nothing to say — the
// feature is off, unlicensed, or this failure was not the policy's after all.
func (s *session) refusedDependencies(appID string) any {
	var state struct {
		Mode      string `json:"mode"`
		Enforcing bool   `json:"enforcing"`
		Refused   *struct {
			DeploymentID string            `json:"deployment_id"`
			Reason       string            `json:"reason"`
			Findings     []json.RawMessage `json:"findings"`
		} `json:"refused"`
		WaiversAvailable bool `json:"waivers_available"`
	}
	if err := s.client.Do(http.MethodGet, "/api/v1/apps/"+appID+"/dependencies", nil, &state); err != nil {
		return nil
	}
	if state.Refused == nil {
		return nil
	}
	return map[string]any{
		"mode":              state.Mode,
		"enforcing":         state.Enforcing,
		"waivers_available": state.WaiversAvailable,
		"deployment_id":     state.Refused.DeploymentID,
		"reason":            state.Refused.Reason,
		"findings":          state.Refused.Findings,
	}
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// describeDir names the directory being bundled the way somebody would say it:
// "./" for here, a relative path for somewhere below, the absolute path
// otherwise.
func describeDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	cwd, err := os.Getwd()
	if err != nil {
		return abs
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return abs
	}
	if rel == "." {
		return "./"
	}
	return "./" + rel
}

// staticRoots reads `[static] root` out of toft.toml, so the always-list
// yields to a static app whose deliverable lives in a directory the list would
// otherwise drop.
func staticRoots(dir string) []string {
	var doc struct {
		Static struct {
			Root string `toml:"root"`
		} `toml:"static"`
	}
	if err := readTOML(filepath.Join(dir, "toft.toml"), &doc); err != nil || doc.Static.Root == "" {
		return nil
	}
	return []string{doc.Static.Root}
}

// printWarnings says what the deploy ignored, under a plain `toft deploy`.
//
// Dimmed rather than red: nothing failed, and colouring it as a failure would
// teach a reader to discount it. Named "Warning" in the singular per line
// because each one is a separate thing the platform did not read.
//
// **The one rendering of a warning, for `toft deploy` and `toft check` both.**
// Check used to wrap each in "toft.toml carries %s, which nothing reads",
// which was right while the list held bare table names and garbled every
// sentence once it held the runtime's too.
func printWarnings(e *env, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	e.out.Say("")
	for _, w := range warnings {
		e.out.Dim("  Warning: %s", warningSentence(w))
	}
}

// installWarnings is every warning sentence the install returned: its
// `warnings`, then any coded warning in `warning_details` whose sentence the
// list does not already hold — an install can carry a warning in one and not
// the other, and a reader of either mode is owed all of them (BB-105,
// 2026-11-04 black-box list: schedule_removed reached --json and not text).
func installWarnings(final client.ProgressEvent) []string {
	out := append([]string(nil), final.Warnings...)
	seen := map[string]bool{}
	for _, w := range out {
		seen[w] = true
	}
	for _, d := range warningDetailsOf(final) {
		if d.Message != "" && !seen[d.Message] {
			seen[d.Message] = true
			out = append(out, d.Message)
		}
	}
	return out
}

// deployWarningLines is installWarnings as the text rendering says them: each
// coded one with its code, as --json's warning_details names it, so a log
// reader can grep for schedule_removed as a program branches on it.
func deployWarningLines(final client.ProgressEvent) []string {
	codes := map[string]string{}
	for _, d := range warningDetailsOf(final) {
		if d.Code != "" && codes[d.Message] == "" {
			codes[d.Message] = d.Code
		}
	}
	var out []string
	for _, w := range installWarnings(final) {
		line := warningSentence(w)
		if c := codes[w]; c != "" {
			line += " (" + c + ")"
		}
		out = append(out, line)
	}
	return out
}

// warningDetailsOf decodes the frame's warning_details, skipping any entry
// this binary cannot read.
func warningDetailsOf(final client.ProgressEvent) []struct{ Code, Message string } {
	var out []struct{ Code, Message string }
	for _, raw := range final.WarningDetails {
		var d struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &d) == nil {
			out = append(out, struct{ Code, Message string }{d.Code, d.Message})
		}
	}
	return out
}

// warningSentence is a warning as a sentence. An install older than
// `warning_details` sent a toft.toml key nothing reads as the bare key in a
// check's `warnings`; anything else already is one.
func warningSentence(w string) string {
	if w != "" && !strings.ContainsAny(w, " \t") {
		return w + " in toft.toml is not read by Toft and was ignored"
	}
	return w
}

// envFileSkippedWarning is the --json half of `not uploading .env`.
func envFileSkippedWarning(path string) string {
	return path + " is not uploaded — toft env set --from-file " + path + " sends its values"
}

// workspaceLeftOutWarning is a directory go.work uses that the bundle left
// out — an ignore rule matched it — so the build will not find the module.
func workspaceLeftOutWarning(dir string) string {
	return "go.work uses " + dir + ", and nothing under it is uploaded (an ignore rule leaves it out) — " +
		"the build will fail to load it. Stop ignoring it, or drop it from go.work (go work edit -dropuse=./" + dir + ")"
}

// secretWarning is the --json half of `including .env`.
func secretWarning(path string) string {
	return path + " is in the bundle and looks like a secret — it ships with the release"
}

// recordedDuration is how long the deployment took by its own row, for a
// watch that joined after it began. False when the row has no end yet.
func (s *session) recordedDuration(appID, deployID string) (time.Duration, bool) {
	rows, err := s.deployments(appID)
	if err != nil {
		return 0, false
	}
	for _, d := range rows {
		if d.ID == deployID && d.StartedAt != nil && d.FinishedAt != nil {
			return d.FinishedAt.Sub(*d.StartedAt), true
		}
	}
	return 0, false
}

// manifestOrigin reports whether a resolved setting was read from toft.toml or
// .toft.toml rather than a flag, the environment or the credentials file.
func manifestOrigin(origin string) bool {
	return strings.HasPrefix(origin, config.ManifestFile+" ") || strings.HasPrefix(origin, config.LocalManifestFile+" ")
}
