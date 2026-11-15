package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"toft/cli/internal/client"
	"toft/cli/internal/ui"
)

const statusUsage = `toft status — what is serving, since when, and from what

Flags:
  --app <slug>        Which app
  --json              The app and its serving deployment as JSON
  --session           As the stored password session, not the key
`

// deployment is the part of a deployment row this client reads. `serving` and
// `current` are the two the API adds for exactly this caller: after a promote,
// the newest successful build is not what is running, and guessing gets it
// wrong.
type deployment struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	Branch     string     `json:"branch"`
	CommitSHA  string     `json:"commit_sha"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Current    bool       `json:"current"`
	Serving    bool       `json:"serving"`
	Phase      string     `json:"phase"`
	Step       string     `json:"current_step"`
	// FailureReason is a failed deployment's reason, in the install's words
	// (not_ready, build_failed, …).
	FailureReason string `json:"failure_reason,omitempty"`
	// TriggeredBy is what started it; "rollback" on a success row is a
	// rollback the install rebuilt for the app's current base path, which owns
	// a fresh tree and so sits at the top of the list (BB-005).
	TriggeredBy string `json:"triggered_by,omitempty"`
	// RestoredFrom is, on a rollback's row, the deployment it restored — kept
	// when the install rebuilds the rollback for a changed base path, which is
	// what lets the next rollback go further back rather than forward.
	RestoredFrom string `json:"restored_from,omitempty"`
}

func cmdStatus(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("status", statusUsage)
	g.register(fs)
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}

	s, err := e.setup(&g)
	if err != nil {
		return err
	}
	appValue, err := pickApp(e, s)
	if err != nil {
		return err
	}
	app, err := s.resolveAppWithClocks(appValue.Value)
	if err != nil {
		return err
	}
	deployments, err := s.deployments(app.ID)
	if err != nil {
		return err
	}
	serving := servingRelease(deployments)
	failed := newerFailure(deployments, serving)

	if g.json {
		doc := map[string]any{
			"app":               &app.appSummary,
			"serving":           serving,
			"serving_since":     app.ServingSince,
			"status_changed_at": app.StatusChangedAt,
			"url":               s.publicURL(&app.appSummary),
			"lock":              app.lock(),
		}
		if failed != nil {
			doc["latest"] = failed
		}
		return json.NewEncoder(e.out.Stdout).Encode(doc)
	}

	where := appWhere(s, &app.appSummary)
	if e.out.Rich() && !isBatchApp(app.Framework) {
		where = e.out.Link(where, where)
	}
	e.out.Answer("%s  %s", e.out.Bold(app.Slug), e.out.Dimmed(where))
	// The status's own clock, never the build's: "stopped" beside the time a
	// release was built read as "stopped for fifty minutes" to everyone.
	if app.StatusChangedAt != nil {
		e.out.Field(12, "status", e.out.Badge(app.Status)+" "+
			e.out.Dimmed("for "+roughAge(time.Since(*app.StatusChangedAt))))
	} else {
		e.out.Field(12, "status", e.out.Badge(app.Status))
	}
	// The lock is the reason a locked app is stopped, and the gateway's 503
	// sends people here to read it — so it is said, not left as "stopped".
	if l := app.lock(); l != nil {
		line := lockedBy(l.Source)
		if l.At != nil {
			line += ", " + roughAge(time.Since(*l.At)) + " ago"
		}
		if e.out.Rich() {
			line = e.out.Caution("■ " + line)
		}
		e.out.Field(12, "locked", line)
		if l.Message != "" {
			e.out.Field(12, "", l.Message)
		}
		e.out.Dim("  Nothing deploys, starts or changes it until an administrator unlocks it.")
	}
	if app.RepoSubdir != "" {
		e.out.Field(12, "folder", app.RepoSubdir)
	}
	if serving == nil {
		e.out.Field(12, "serving", "nothing deployed yet")
		sayNewerFailure(e, failed, false)
		return nil
	}

	from := serving.Branch
	if from == "" {
		from = "—"
	}
	if serving.CommitSHA != "" {
		from += " · " + shortSHA(serving.CommitSHA)
	}
	// `serving` only while something is: a stopped app's current release is
	// what a start would bring back, not what anyone is reaching.
	label := "serving"
	if !liveStatus(app.Status) {
		label = "release"
	}
	e.out.Field(12, label, from)
	// Two clocks, each labelled: when this release became the current one (the
	// flip — a rollback's is minutes ago, though its build may be weeks old),
	// and when it was built. An install older than serving_since sends only
	// the second, and it is never passed off as the first.
	since, built := "", ""
	if app.ServingSince != nil {
		since = roughAge(time.Since(*app.ServingSince)) + " ago"
	}
	if serving.FinishedAt != nil {
		built = roughAge(time.Since(*serving.FinishedAt)) + " ago"
	}
	switch {
	case e.out.Rich() && since != "" && since == built:
		// One line when the two clocks say the same thing — an ordinary
		// deploy, built and flipped in the same minute.
		e.out.Field(12, "since", since+e.out.Dimmed(" · built then"))
	default:
		if since != "" {
			e.out.Field(12, "since", since)
		}
		if built != "" {
			e.out.Field(12, "built", built)
		}
	}
	id := serving.ID
	if e.out.Rich() {
		id = e.out.Bold(ui.ShortID(id)) + e.out.Dimmed(strings.TrimPrefix(id, ui.ShortID(id)))
	}
	e.out.Field(12, "deployment", id)
	sayNewerFailure(e, failed, true)

	// C2's sentence, for as long as it is true. The same fact Overview shows,
	// said where somebody who never opens the browser will meet it.
	if serving.Branch == "upload" && app.RepoURL != "" && !strings.HasPrefix(app.RepoURL, "upload://") {
		e.out.Say("")
		e.out.Say("  %s", e.out.Accent("This is code pushed from a machine, not from "+app.RepoURL+"."))
		e.out.Dim("  The next deploy from %s replaces it.", orDefault(app.Branch, "the repository"))
	}
	return nil
}

// appWithClocks is the app as `toft status` reads it: the summary every command
// resolves, plus the two clocks only this command renders. Absent on an install
// older than them, and then nil rather than a guess.
type appWithClocks struct {
	appSummary
	ServingSince    *time.Time `json:"serving_since"`
	StatusChangedAt *time.Time `json:"status_changed_at"`
	LockedAt        *time.Time `json:"locked_at"`
	LockedMessage   *string    `json:"locked_message"`
	LockedSource    string     `json:"locked_source"`
}

// appLock is `toft status --json`'s `lock`: null when the app is not locked.
type appLock struct {
	At      *time.Time `json:"at"`
	Source  string     `json:"source"`
	Message string     `json:"message,omitempty"`
}

func (a *appWithClocks) lock() *appLock {
	if a.LockedAt == nil {
		return nil
	}
	l := &appLock{At: a.LockedAt, Source: a.LockedSource}
	if a.LockedMessage != nil {
		l.Message = *a.LockedMessage
	}
	return l
}

// lockedBy names who placed a lock, from the API's locked_source.
func lockedBy(source string) string {
	switch source {
	case "dependency":
		return "by the dependency policy"
	case "", "admin":
		return "by an administrator"
	}
	return "by " + source
}

// resolveAppWithClocks is resolveApp's one request, decoded into the wider
// struct — a second round trip for two timestamps would be the expensive way
// to read them.
func (s *session) resolveAppWithClocks(slug string) (*appWithClocks, error) {
	var body struct {
		App appWithClocks `json:"app"`
	}
	err := s.client.Do(http.MethodGet, "/api/v1/apps/by-slug/"+url.PathEscape(slug)+"?view=brief", nil, &body)
	if err != nil {
		var apiErr *client.Error
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			return nil, s.noSuchApp(slug)
		}
		return nil, err
	}
	return &body.App, nil
}

// liveStatus is whether the status means the current release is reachable or
// about to stay so: running, a batch app's ready, or a redeploy in flight over
// a serving release.
func liveStatus(status string) bool {
	switch status {
	case "running", "ready", "building":
		return true
	}
	return false
}

// deployments is the app's history, newest first, as the API returns it: a bare
// array of the same view the Deployments tab reads.
func (s *session) deployments(appID string) ([]deployment, error) {
	var body []deployment
	err := s.client.Do(http.MethodGet, "/api/v1/apps/"+appID+"/deployments", nil, &body)
	return body, err
}

// servingRelease is the row that owns the code actually executing — which after
// a promote is not the newest successful build, and the API says so rather than
// leaving anyone to guess.
// newerFailure is the newest deployment when it failed and is not the one
// serving: a deploy that went wrong in the background (--no-watch, CI) while
// the previous release kept serving. `toft status` said nothing about it, and
// the only way to learn was `toft deployments list --json` (docs/bugs
// 2026-10-28 #5). The list is newest first.
func newerFailure(deployments []deployment, serving *deployment) *deployment {
	if len(deployments) == 0 {
		return nil
	}
	newest := &deployments[0]
	if newest.Status != "failed" || (serving != nil && newest.ID == serving.ID) {
		return nil
	}
	return newest
}

// sayNewerFailure is newerFailure's line, and the command that explains it.
// serving says whether a release is serving: with none, there is no "release
// before it" to name (drills 2026-11-02 — status said both).
func sayNewerFailure(e *env, failed *deployment, serving bool) {
	if failed == nil {
		return
	}
	line := ui.ShortID(failed.ID) + " failed"
	if failed.FailureReason != "" {
		line += " — " + failed.FailureReason
	}
	if e.out.Rich() {
		line = e.out.Caution(line)
	}
	e.out.Field(12, "latest", line)
	if !serving {
		e.out.Dim("  No deploy has succeeded, so nothing serves yet. toft logs --deploy %s", ui.ShortID(failed.ID))
		return
	}
	e.out.Dim("  The newest deploy failed; what serves is the release before it. toft logs --deploy %s",
		ui.ShortID(failed.ID))
}

func servingRelease(deployments []deployment) *deployment {
	for i := range deployments {
		if deployments[i].Serving {
			return &deployments[i]
		}
	}
	for i := range deployments {
		if deployments[i].Current {
			return &deployments[i]
		}
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// roughAge is how long ago, at the precision a person says it out loud.
func roughAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return countOf(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return countOf(int(d.Hours()), "hour")
	}
	return countOf(int(d.Hours()/24), "day")
}

// sourceLabel is where an app's code comes from, as a table cell: "uploaded",
// or the repository without its scheme, linked to it.
func sourceLabel(e *env, repoURL string) string {
	if repoURL == "" || strings.HasPrefix(repoURL, "upload://") {
		return e.out.Dimmed("uploaded")
	}
	short := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(repoURL, "https://"), "http://"), ".git")
	return e.out.Link(repoURL, short)
}

// countOf is "1 minute", "2 minutes".
func countOf(n int, unit string) string {
	return fmt.Sprintf("%d %s", n, plural(n, unit, unit+"s"))
}

const appsUsage = `toft apps — the apps you can see

Flags:
  --mine              Only apps that are yours: you own them, a group you are
                      in owns them, or you were granted them
  --json              The list as JSON
`

// errMineNeedsAPerson refuses --mine to a deploy key. "Mine" is a question
// about a person, and a deploy key is not one — it belongs to nobody by
// design, so it is refused with the reason.
//
// Exit 2 and coded: the flag is the wrong question for this credential, and
// nothing was sent (BB-015).
var errMineNeedsAPerson = usageHinted("needs_personal_credential",
	"drop --mine to list every app the deploy key can see, or use a personal key (`toftu_…`, `toft login` mints one) to ask what is yours",
	"--mine needs a personal credential: the credential in use is a deploy key, which belongs to nobody and owns nothing")

func cmdApps(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("apps", appsUsage)
	g.register(fs)
	mine := fs.Bool("mine", false, "only apps that are yours")
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}

	s, err := e.setup(&g)
	if err != nil {
		return err
	}

	var apps []struct {
		appSummary
		OwnerID string `json:"owner_id"`
		// Always present, null when no group owns the app (BB-027): a
		// consumer testing for the key learns the field exists.
		OwnerGroupID *string `json:"owner_group_id"`
		// Mine is the install's own answer — owned by you or a group you are
		// in, or granted to you — and the one the web console splits its list
		// on. Comparing owner_id with your id missed every group-owned app
		// (drills 2026-10-28 evening #3).
		Mine           bool       `json:"mine"`
		LastDeployedAt *time.Time `json:"last_deployed_at"`
	}
	if *mine && s.IsDeployKey() {
		return errMineNeedsAPerson
	}
	if err := s.client.Do(http.MethodGet, "/api/v1/apps", nil, &apps); err != nil {
		return err
	}

	rows := apps[:0]
	for _, a := range apps {
		if a.OwnerGroupID != nil && *a.OwnerGroupID == "" {
			a.OwnerGroupID = nil
		}
		if *mine && !a.Mine {
			continue
		}
		rows = append(rows, a)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Slug < rows[j].Slug })

	if g.json {
		return writeNDJSON(e.out.Stdout, rows)
	}
	if len(rows) == 0 {
		e.out.Say("No apps you can see on %s.", s.InstallURL)
		return nil
	}

	if e.out.Rich() {
		t := e.out.NewTable("app", "status", "kind", "source", "deployed").Flex(3)
		for _, a := range rows {
			deployed := e.out.Dimmed("never")
			if a.LastDeployedAt != nil {
				deployed = ui.Ago(*a.LastDeployedAt)
			}
			t.Row(e.out.Bold(a.Slug), e.out.Badge(a.Status), e.out.Dimmed(orDefault(a.Framework, "—")),
				sourceLabel(e, a.RepoURL), deployed)
		}
		t.Print()
		return nil
	}

	width := 0
	for _, a := range rows {
		if len(a.Slug) > width {
			width = len(a.Slug)
		}
	}
	for _, a := range rows {
		source := a.RepoURL
		if strings.HasPrefix(source, "upload://") {
			source = "uploaded"
		}
		e.out.Answer("%-*s  %-9s %s", width, a.Slug, a.Status, e.out.Dimmed(source))
	}
	return nil
}
