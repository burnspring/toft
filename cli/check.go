package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"toft/cli/internal/bundle"
	"toft/cli/internal/config"
)

// `toft check` — what a deploy of this directory would decide, before it is
// one.
//
// **The install decides, and this client renders it.** The bundle is the one
// `toft deploy` would send, built by the same function, and the answer is the
// build's own detection, manifest and runtime-version decisions run against it
// on the install (`POST /apps/{id}/check`). A check that parsed toft.toml
// here would be a second reading of a file whose readers already disagree
// with each other across versions; the install's reading is the one the
// deploy will have.
//
// It costs an upload and saves a build: the failures it catches — nothing a
// deploy can serve, a manifest that does not parse, a pin nothing satisfies, a
// table nothing reads — are the ones that otherwise cost a queue slot and a
// clone to learn. What it cannot catch it names: the dependency gate, the build
// and the start.

const checkUsage = `toft check [dir] — what a deploy of this directory would decide

With no app linked, or a slug nobody has claimed, it checks the directory as a
new app — what ` + "`toft create`" + ` would decide.

Sends the bundle ` + "`toft deploy`" + ` would send, and the install answers with the
decisions a deploy makes before it builds anything: what the app is, the
interpreter it would get, the jobs toft.toml declares with the timeout and
memory each run would get (and where each figure comes from), what toft.toml carries
that nothing reads, and what unpacking removed. Nothing is deployed and nothing
is recorded. Exits 1 when a deploy would be refused.

` + "`toft deploy --dry-run`" + ` is the same command.

Flags:
  --app <slug>        Which app
  --owner-group <g>   For a new app: the group toft create will name, checked
                      as the create would check it. A deploy key must give one,
                      as toft create must
  --json              The install's answer, as it sent it

The dependency policy, the build and the start are not checked: only a deploy
answers those, and the answer says so.
`

func cmdCheck(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("check", checkUsage)
	g.register(fs)
	ownerGroup := fs.String("owner-group", "", "for a new app: the group that will own it")
	if err := parseOperands(fs, args, 1); err != nil {
		return skipHelp(err)
	}
	if fs.NArg() > 0 {
		e.dir = fs.Arg(0)
	}
	s, err := e.setup(&g)
	if err != nil {
		return manifestCheckRefusal(e, &g, err)
	}
	return checkDirectory(e, s, &g, *ownerGroup)
}

// manifestCheckRefusal is a toft.toml this client could not parse, answered
// the way the install's check answers every other refusal: a problem with
// `reason: manifest_invalid`, `code: manifest_syntax`, the line in its message
// and a hint, and exit 1 — "a deploy would be refused", not a usage refusal of
// the command (BB-015). Any other error is returned as it is.
func manifestCheckRefusal(e *env, g *globalFlags, err error) error {
	var mErr *config.ManifestError
	if !errors.As(err, &mErr) {
		return err
	}
	file := filepath.Base(mErr.Path)
	problem := map[string]string{
		"reason":  "manifest_invalid",
		"code":    "manifest_syntax",
		"message": err.Error(),
		"hint":    "fix the line named in " + file + " so it parses as TOML — a string is quoted, one = per key",
	}
	if g.json {
		json.NewEncoder(e.out.Stdout).Encode(map[string]any{
			"ok": false, "problems": []any{problem}, "warnings": []string{},
			"not_checked": []string{"the rest of this check: " + file + " does not parse"},
		})
	} else {
		e.out.Say("%s %s", e.out.Bold("manifest_syntax"), err.Error())
		e.out.Hint("%s", sentenceCase(problem["hint"]))
	}
	return &resultFailedError{"a deploy of this directory would be refused (manifest_syntax)"}
}

// checkDirectory is `toft check` and `toft deploy --dry-run` once the session
// exists: the linked app when there is one, a new app otherwise.
func checkDirectory(e *env, s *session, g *globalFlags, ownerGroup string) error {
	// **Before `toft create` is when a check is most wanted**, so no linked app,
	// or a slug nothing has claimed yet, checks the bundle as a new app — the
	// create route's doors, and the mode a create would give it. And the
	// create's owner rule with them: a deploy key's check of a new app that
	// names no group would pass a tree `toft create` then refuses. The group
	// goes with the bundle as the id create would send, so the install can run
	// the create's owner resolution too — a group nobody in it could administer
	// is refused by the check, not first by the create.
	newApp := func() error {
		groupID, err := ownerGroupFor(s, "toft check (and toft create)", ownerGroup)
		if err != nil {
			return err
		}
		return runCheck(e, s, nil, g, groupID)
	}
	appValue, err := s.app()
	if err != nil {
		return newApp()
	}
	app, err := s.resolveApp(appValue.Value)
	var usage *usageError
	if errors.As(err, &usage) {
		// A name nothing has claimed is checked as the create it would be —
		// and the create refuses one that is not a slug, so the check does
		// too, with the same code (M5, 2026-11-04 black-box list).
		if err := refuseStatedNonSlug(appValue.Value, appValue.Origin, true); err != nil {
			return err
		}
		return newApp()
	}
	if err != nil {
		return err
	}
	return runCheck(e, s, app, g, "")
}

// checkResult is the part of the install's answer this rendering reads. Under
// --json the whole answer is passed through — `warnings` are the sentences a
// deploy's result carries, `warning_details` each with a code and its table — so a field this binary is too old
// to name still reaches the caller.
type checkResult struct {
	OK         bool     `json:"ok"`
	Framework  string   `json:"framework"`
	Language   string   `json:"language"`
	Entrypoint string   `json:"entrypoint"`
	Mode       string   `json:"execution_mode"`
	Jobs       []string `json:"jobs"`
	Warnings   []string `json:"warnings"`
	Runtime    *struct {
		Version        string `json:"version"`
		Pin            string `json:"pin"`
		RequiresPython string `json:"requires_python"`
		Where          string `json:"where"`
	} `json:"runtime"`
	// JobLimits is each declared job's effective timeout and memory, with
	// where each figure came from — what will stop a run, before one does.
	JobLimits []struct {
		Name   string    `json:"name"`
		Limits jobLimits `json:"limits"`
	} `json:"job_limits"`
	// Restart and Render are toft.toml's [restart] and [render] as the deploy
	// would record them; absent when the table is, or when it is refused.
	Restart *checkSchedule `json:"restart"`
	Render  *checkSchedule `json:"render"`
	// Storage is toft.toml's [[storage]] planned against the app's table:
	// what the deploy's flip keeps, adds, changes and removes, and the console
	// mappings it leaves alone. Absent when nothing is declared or mapped.
	Storage        *checkStorage `json:"storage"`
	WarningDetails []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"warning_details"`
	Problems []struct {
		Reason  string `json:"reason"`
		Code    string `json:"code"`
		Hint    string `json:"hint"`
		Message string `json:"message"`
	} `json:"problems"`
	Removed []struct {
		Path  string `json:"path"`
		Files int    `json:"files"`
	} `json:"removed"`
	NotChecked []string `json:"not_checked"`
}

// checkStorage is the storage plan on the check answer.
type checkStorage struct {
	Keep    []plannedStorage `json:"keep"`
	Add     []plannedStorage `json:"add"`
	Change  []plannedStorage `json:"change"`
	Remove  []plannedStorage `json:"remove"`
	Console []plannedStorage `json:"console"`
}

// plannedStorage is one mapping the plan names.
type plannedStorage struct {
	Name      string `json:"name"`
	Folder    string `json:"folder"`
	Access    string `json:"access"`
	Path      string `json:"path"`
	WasAccess string `json:"was_access"`
	WasPath   string `json:"was_path"`
}

func (p plannedStorage) label() string {
	if p.Folder == "" {
		return p.Name
	}
	return p.Name + "/" + p.Folder
}

// storageLines is the plan as `toft check` prints it, one line per mapping,
// in the deploy log's marks: = kept, + added, ~ changed, - detached.
func storageLines(s *checkStorage) []string {
	var out []string
	for _, p := range s.Keep {
		out = append(out, fmt.Sprintf("= %s, %s at %s", p.label(), p.Access, p.Path))
	}
	for _, p := range s.Console {
		out = append(out, fmt.Sprintf("= %s, kept: attached in the console", p.label()))
	}
	for _, p := range s.Add {
		out = append(out, fmt.Sprintf("+ %s, %s at %s", p.label(), p.Access, p.Path))
	}
	for _, p := range s.Change {
		out = append(out, fmt.Sprintf("~ %s, %s at %s (was %s at %s)", p.label(), p.Access, p.Path, p.WasAccess, p.WasPath))
	}
	for _, p := range s.Remove {
		out = append(out, fmt.Sprintf("- %s, detached (its files are kept)", p.label()))
	}
	return out
}

// checkSchedule is one of the two one-per-app schedules on the check answer.
type checkSchedule struct {
	Schedule  string `json:"schedule"`
	Timezone  string `json:"timezone"`
	NextRunAt string `json:"next_run_at"`
}

// scheduleLine is a schedule as `toft check` prints it: the expression, its
// zone, and when it would first fire.
func scheduleLine(e *env, s *checkSchedule) string {
	zone := s.Timezone
	if zone == "" {
		zone = "UTC"
	}
	line := s.Schedule + " (" + zone + ")"
	if s.NextRunAt != "" {
		line += " " + e.out.Dimmed("next "+s.NextRunAt)
	}
	return line
}

// runCheck sends the bundle and renders the answer. A nil app checks it as a
// new one, owned by groupID when that is not "" — the `owner_group_id` field
// the create-from-a-bundle route takes.
func runCheck(e *env, s *session, app *appSummary, g *globalFlags, groupID string) error {
	built, err := bundle.Build(e.dir, bundle.Options{KeepPaths: staticRoots(e.dir)})
	if err != nil {
		return err
	}
	defer os.Remove(built.Path)
	archive, err := os.Open(built.Path)
	if err != nil {
		return err
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return err
	}
	route, name, label := "/api/v1/apps/check", "bundle.zip", "a new app"
	if app != nil {
		route, name, label = fmt.Sprintf("/api/v1/apps/%s/check", app.ID), app.Slug+".zip", app.Slug
	}
	e.out.Say("%s %s checking %s, %d %s", e.out.Bold(label), e.out.Dimmed("←"),
		describeDir(e.dir), len(built.Files), plural(len(built.Files), "file", "files"))
	for _, skipped := range built.EnvFiles {
		e.out.Dim("not uploading %s — toft env set --from-file %s sends its values", skipped, skipped)
	}
	// The deploy's own lines, here too: a check is where somebody looks before
	// they ship, and what goes with the bundle — and what does not — is a
	// thing to see then.
	for _, secret := range built.Secrets {
		e.out.Dim("including %s", secret)
	}
	// A go.work naming a directory the bundle leaves out fails the build, and
	// the install's check reads only the bundle — so this client says it
	// (BB-028).
	if !g.json {
		for _, dir := range built.WorkspaceLeftOut {
			e.out.Warn("%s", workspaceLeftOutWarning(dir))
		}
	}
	if info.Size() > deployMaxBytes {
		return tooBig(e, built, info.Size())
	}

	var fields map[string]string
	if app == nil && groupID != "" {
		fields = map[string]string{"owner_group_id": groupID}
	}
	var raw json.RawMessage
	err = s.client.UploadWithFields(route, name,
		archive, info.Size(), fields, func(sent, total int64) { e.out.Meter("upload", sent, total) }, &raw)
	e.out.ClearLine()
	if err != nil {
		return err
	}
	var got checkResult
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("this install sent a check this version of toft does not understand — upgrade toft (%v)", err)
	}

	if g.json {
		fmt.Fprintln(e.out.Stdout, strings.TrimSpace(string(withBundleNotices(raw, built.EnvFiles, built.Secrets, built.WorkspaceLeftOut...))))
	} else {
		renderCheck(e, &got)
	}
	if len(got.Jobs) > 0 && got.Framework != "static" && jobsRefused(s) {
		e.out.Warn("toft.toml declares %s, and this install runs no jobs — the deploy will "+
			"succeed and `toft job run` will refuse. `toft whoami` says jobs_available: false",
			plural(len(got.Jobs), "a job", "jobs"))
	}
	if !got.OK {
		why := "a deploy of this directory would be refused"
		if len(got.Problems) > 0 {
			why += " (" + orDefault(got.Problems[0].Code, got.Problems[0].Reason) + ")"
		}
		return &resultFailedError{why}
	}
	return nil
}

// jobsRefused is whoami's jobs_available being false, read the way `toft whoami`
// reads it. Silent on failure, because a warning that cannot be checked is not
// raised.
func jobsRefused(s *session) bool {
	var caps capabilities
	if err := s.client.Do(http.MethodGet, "/api/v1/me/capabilities", nil, &caps); err != nil || caps.Install == nil {
		return false
	}
	return !caps.Install.Jobs
}

func renderCheck(e *env, got *checkResult) {
	what := got.Framework
	if got.Entrypoint != "" {
		what += " · " + got.Entrypoint
	}
	e.out.Field(12, "app", checked(e)+what)
	if got.Runtime != nil && got.Runtime.Version != "" {
		line := got.Language + " " + got.Runtime.Version
		if got.Runtime.Pin != "" {
			line += " (pinned " + got.Runtime.Pin + ")"
		} else if got.Runtime.RequiresPython != "" {
			line += " (requires-python " + got.Runtime.RequiresPython + ")"
		}
		e.out.Field(12, "runtime", checked(e)+line+" "+e.out.Dimmed("from "+got.Runtime.Where))
	}
	if got.Mode != "" {
		e.out.Field(12, "runs", checked(e)+got.Mode)
	}
	// A static app runs no jobs and its [jobs] tables are a problem below, so
	// a ticked jobs line above that refusal read as half-accepted (docs/bugs
	// 2026-10-28 D14).
	static := got.Framework == "static"
	if len(got.JobLimits) > 0 && !static {
		// Each job with the limits a run of it will get, and their sources:
		// a clamp shows up here, before the run it would have stopped.
		width := 0
		for _, j := range got.JobLimits {
			width = max(width, len(j.Name))
		}
		for i, j := range got.JobLimits {
			label := ""
			if i == 0 {
				label = "jobs"
			}
			e.out.Field(12, label, fmt.Sprintf("%s%-*s  %s", checked(e), width, j.Name,
				e.out.Dimmed(limitText("timeout", j.Limits.Timeout, durationText)+" · "+
					limitText("memory", j.Limits.Memory, mbText)+" · "+
					limitText("cpu", j.Limits.CPU, cpuText))))
		}
	} else if len(got.Jobs) > 0 && !static {
		e.out.Field(12, "jobs", checked(e)+strings.Join(got.Jobs, ", "))
	}
	if got.Restart != nil {
		e.out.Field(12, "restart", checked(e)+scheduleLine(e, got.Restart))
	}
	if got.Render != nil {
		e.out.Field(12, "re-render", checked(e)+scheduleLine(e, got.Render))
	}
	if got.Storage != nil {
		for i, line := range storageLines(got.Storage) {
			label := ""
			if i == 0 {
				label = "storage"
			}
			e.out.Field(12, label, checked(e)+line)
		}
	}
	// A clamp is a warning of its own kind — the deploy goes ahead and a run
	// is stopped sooner than toft.toml says — so it is said as one, with its
	// code, and left out of the plain sentences below.
	clamped := map[string]bool{}
	for _, w := range got.WarningDetails {
		if w.Code == "limit_clamped" {
			e.out.Warn("%s (limit_clamped)", w.Message)
			clamped[w.Message] = true
		}
	}
	var rest []string
	for _, w := range got.Warnings {
		if !clamped[w] {
			rest = append(rest, w)
		}
	}
	printWarnings(e, rest)
	for _, r := range got.Removed {
		e.out.Dim("  unpacking removed %s (%d %s)", r.Path, r.Files, plural(r.Files, "file", "files"))
	}
	for _, p := range got.Problems {
		e.out.Say("%s %s", e.out.Fail("✗"), p.Message)
		// The code is what a script branches on and the hint is the line to
		// change; an install older than either sent the reason alone.
		e.out.Dim("  %s", orDefault(p.Code, p.Reason))
		if p.Hint != "" {
			e.out.Next("%s", p.Hint)
		}
	}
	if got.OK {
		if e.out.Rich() {
			e.out.Say("")
			e.out.Say("%s %s %s", e.out.OK("✓"), e.out.Bold("A deploy would get this far."),
				e.out.Dimmed("Not checked: "+strings.Join(got.NotChecked, ", ")+"."))
			return
		}
		e.out.Say("%s a deploy would get this far. Not checked: %s.", e.out.OK("✓"),
			strings.Join(got.NotChecked, ", "))
	}
}

// checked is the tick a decided line of `toft check` opens with, on a
// terminal: the report reads as a checklist of what a deploy would decide.
func checked(e *env) string {
	if e.out.Rich() {
		return e.out.OK("✓ ")
	}
	return ""
}

// withBundleNotices adds what this client says about the bundle — a skipped
// .env, a secret-looking file that ships — to the install's answer, as
// `warnings` sentences and `warning_details` entries: the text rendering's
// dimmed lines, which a --json caller otherwise never saw (BB-016). The answer
// is passed through unchanged when there is nothing to add or it is not an
// object.
func withBundleNotices(raw json.RawMessage, envFiles, secrets []string, workspaceLeftOut ...string) json.RawMessage {
	if len(envFiles) == 0 && len(secrets) == 0 && len(workspaceLeftOut) == 0 {
		return raw
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return raw
	}
	var warnings []string
	var details []map[string]any
	if w, ok := doc["warnings"]; ok {
		json.Unmarshal(w, &warnings)
	}
	if d, ok := doc["warning_details"]; ok {
		json.Unmarshal(d, &details)
	}
	add := func(code, path, msg string) {
		warnings = append(warnings, msg)
		d := map[string]any{"code": code, "path": path, "message": msg}
		// The install's go_work_dir_excluded carries table and path; this
		// client's carries the same two (BB-007).
		if code == "go_work_dir_excluded" {
			d["table"] = "go.work"
		}
		details = append(details, d)
	}
	for _, f := range envFiles {
		add("env_file_skipped", f, envFileSkippedWarning(f))
	}
	for _, f := range secrets {
		add("secret_in_bundle", f, secretWarning(f))
	}
	for _, d := range workspaceLeftOut {
		// An install that checks go.work itself already said this one; the
		// client's is for an install that does not.
		if serverSaidGoWork(details, d) {
			continue
		}
		add("go_work_dir_excluded", d, workspaceLeftOutWarning(d))
	}
	doc["warnings"], _ = json.Marshal(warnings)
	doc["warning_details"], _ = json.Marshal(details)
	out, err := json.Marshal(doc)
	if err != nil {
		return raw
	}
	return out
}

// serverSaidGoWork reports whether the install's own warning_details already
// name dir as a go.work entry this tree lacks.
func serverSaidGoWork(details []map[string]any, dir string) bool {
	for _, d := range details {
		code, _ := d["code"].(string)
		msg, _ := d["message"].(string)
		path, _ := d["path"].(string)
		if code == "go_work_dir_excluded" && (path == dir || strings.Contains(msg, "go.work uses "+dir+",")) {
			return true
		}
	}
	return false
}
