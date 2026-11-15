// Package events records what breaks in a hosted app, and tells the people who
// can fix it.
//
// It exists because Toft used to tell nobody. An expired clone token made
// auto-deploy stop working forever with nothing but a line on stdout; a process
// that died at 2am left its row saying "running" until somebody noticed the
// 502s; a scheduled job that failed every night wrote history rows no one read.
//
// The seam is one call, Record, from a background worker. Three properties are
// what make it safe to sprinkle through code that is doing something else:
//
//   - It never returns an error and never blocks. Everything it does is either a
//     write that is allowed to fail with a log line, or a send that was already
//     fire-and-forget.
//   - It is nil-receiver safe throughout, so a caller with no recorder needs no
//     branch and a test needs no double.
//   - It always writes the row, and only sometimes sends the mail. The cooldown
//     and the app's own notify_failures switch decide the second question; they
//     have no say in the first. An owner can silence the noise without erasing
//     the platform's memory of their app breaking.
package events

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"toft/internal/database"
	mailer "toft/internal/mail"
	"toft/internal/notify"
	"toft/internal/textutil"
)

// Kind classifies what went wrong. It is the unit the cooldown, the Clear call
// and the UI's filters all work in.
type Kind string

const (
	// KindDeployFailed is an automatic deploy that did not finish. Manual
	// deploys are deliberately not recorded here — a person is watching the
	// streamed build log, and mailing them about what they are looking at is
	// noise.
	KindDeployFailed Kind = "deploy_failed"
	// KindGitAuthFailed is the headline case: a personal access token that
	// expired or was revoked, a repository that moved, or a rotated
	// ENCRYPTION_KEY that has made the stored credential unreadable. All three
	// look identical from the app's side — auto-deploy silently stops.
	KindGitAuthFailed Kind = "git_auth_failed"
	// KindGitFetchFailed is a transient reach problem: DNS, a timeout, a branch
	// that no longer exists. A warning rather than an error, because the next
	// poll may well fix it.
	KindGitFetchFailed Kind = "git_fetch_failed"
	// KindAppCrashed is a workload the platform believes should be running and
	// cannot find, or finds in an error state.
	KindAppCrashed Kind = "app_crashed"
	// KindAppFlapping is a workload that keeps restarting. Distinct from
	// crashed because the app is nominally up and the symptom is intermittent.
	KindAppFlapping Kind = "app_flapping"
	// KindAppRestarted is one exit of a workload its backend restarted, with
	// the exit code when the backend knows it. Recorded silently and closed at
	// once — it is a fact about the past, not a condition — so `toft events`
	// can answer "it exited with code 7 at 21:52" for an app below the flapping
	// threshold, which otherwise left no trace anywhere.
	KindAppRestarted Kind = "app_restarted"
	// KindAppNotRestarting is the platform's supervisor declaring an ending:
	// a crashed isolated workload it restarted the full number of times
	// without it staying up. Distinct from KindAppCrashed because it is a
	// different fact — not "your app broke" but "we have stopped trying, and
	// nothing will happen now until you act". Kubelet never says this and that
	// is the defect it is copied away from: unbounded retry against a broken
	// release reads as activity.
	KindAppNotRestarting Kind = "app_not_restarting"
	// KindAppMemoryLimit is a workload being restarted for crossing the memory
	// cap the install applied to it.
	//
	// Its own kind because it is the one restart cause the platform can name
	// with certainty and the app cannot explain at all. A workload killed for
	// memory is sent a signal: there is no error, no traceback and no exit
	// message, so an R app's log ends at a bare "Execution halted" and a
	// Python one at nothing whatsoever. KindAppFlapping's detail sends the
	// reader to "the app's logs", which in this one case is the only place the
	// answer is guaranteed not to be.
	//
	// Distinct from KindAppFlapping in when it fires as well as in what it
	// says. Flapping needs five restarts inside one sweep, which a memory kill
	// does not reach: an app that takes two minutes to grow into its cap
	// restarts about once a sweep, forever, and trips nothing. One restart
	// with the reading against the cap is already both certain and actionable.
	KindAppMemoryLimit Kind = "app_memory_limit"
	// KindAppOutOfMemory is the ending of the restart loop KindAppMemoryLimit
	// narrates: the platform stopped the workload itself rather than let the
	// backend keep killing and restarting it, and the app is `crashed`.
	//
	// Two kinds because they are two facts and only one of them is a verdict.
	// The warning above is "this happened, and it will keep happening"; this is
	// "it kept happening, so the app is down until somebody acts". An owner who
	// gets the first and then the first again five more times has been told
	// about a symptom; an owner who gets this has been told the outcome.
	//
	// Error severity, and the same severity the backend's own kill deserves —
	// an app being restarted around a memory cap is still answering some
	// requests, and one stopped for it is answering none.
	KindAppOutOfMemory Kind = "app_out_of_memory"
	// KindAppCPUThrottled is an app held at its CPU limit for a quarter of
	// its work over an hour: requests are slow and nothing restarts, so
	// without this nothing on any screen would say so. Raised and cleared by
	// the health watcher from the runner's throttling counters, fingerprinted
	// on the release; a cold start hits the cap by design, so one burst never
	// raises it. Jobs get none — their figure is on the run.
	KindAppCPUThrottled Kind = "app_cpu_throttled"
	// KindJobRunFailed is a background job run that ended in a terminal
	// non-success state.
	KindJobRunFailed Kind = "job_run_failed"
	// KindLimitClamped is a job running below what it declared, because an
	// administrator lowered the install's ceiling under it. Not a failure —
	// the run goes, at the lower figure — and recorded so the app page and the
	// next toft check can say so.
	KindLimitClamped Kind = "limit_clamped"
	// KindReleasesPruned is old versions a deploy retired because they fell
	// past the app's retention (the owner's releases_kept, default 3, under the
	// install's max_versions_per_app). Info and silent — the platform working
	// as configured, never mailed — but recorded, because "where did version
	// 12 go" otherwise has only a server log line for an answer (BB-009).
	KindReleasesPruned Kind = "releases_pruned"
	// KindSourceBlocked is an app whose repository is no longer on this
	// install's approved-source list, so its pushes have stopped deploying.
	//
	// It is the one event here that is not a malfunction: nothing is broken, an
	// administrator made a decision, and the app is still serving. Recorded
	// anyway, and recorded loudly, because the alternative is an owner watching
	// commits land and nothing happen — "my app stopped updating" diagnosed by
	// inference is exactly what this package exists to prevent.
	KindSourceBlocked Kind = "source_blocked"
	// KindScheduleDisabled is a job schedule the platform switched off because
	// its expression stopped parsing. Recorded because the alternative — the
	// existing behaviour — is a job that silently never runs again.
	KindScheduleDisabled Kind = "schedule_disabled"
	// KindScheduleRemoved is a `[restart]` or `[render]` a deploy deleted
	// because the release's toft.toml no longer declares it. The person who
	// removed a line and the people who read the report are often different
	// people, and a report that stops on a weekday should be a sentence in
	// somebody's inbox, not a gap they find later.
	KindScheduleRemoved Kind = "schedule_removed"
	// KindJobScheduleSkipped is a scheduled job firing the queue refused —
	// almost always the app's limit on runs in flight. The claim has already
	// moved the schedule on, so the firing is gone; without this it left only a
	// log line, and "it did not happen" had to be inferred from a gap in the
	// run history. Its fingerprint leads with the definition, so the job's next
	// success resolves it.
	KindJobScheduleSkipped Kind = "job_schedule_skipped"
	// KindJobRunOverdue is a job with an expect_within that has gone that long
	// without a successful run (ADR §105). The failure notice already covers a
	// job that fails; this is the one that stopped running at all — paused and
	// forgotten, queue-refused, never placed — which otherwise says nothing.
	KindJobRunOverdue Kind = "job_run_overdue"
	// KindDependencyBlocked is a deploy the dependency policy refused. An
	// error, and recorded whether the deploy was automatic or not — the
	// KindSourceBlocked precedent: nothing is broken and an administrator's
	// decision is what stopped it, which is exactly the kind of thing an owner
	// otherwise diagnoses by inference.
	KindDependencyBlocked Kind = "dependency_blocked"
	// KindDependencyAdvisory is a *serving* release that has started matching
	// an advisory.
	//
	// A **warning**, never an error, and the distinction is the whole design:
	// nothing is broken, the app is still serving, and this feature never
	// stops it. The fingerprint is the advisory id, so one open row per
	// (app, advisory) — never one per sweep, which runs every six hours
	// forever and would bury the one that explains the problem.
	KindDependencyAdvisory Kind = "dependency_advisory"
)

// NotifyCooldown is how long one problem stays quiet after it has been mailed
// about.
//
// Six hours is chosen against the loudest source: the auto-deploy poller runs
// every minute, so an expired token is rediscovered 1,440 times a day. Every
// one of those is recorded; four of them a day are emailed. Short enough that a
// problem which persists across a working day is raised again, long enough that
// nobody filters Toft into a folder they stop reading.
//
// A problem that recovered (and was told so) and then came back inside the
// window is told once more — capped at two notices per fingerprint per window,
// so an app that flaps every minute goes quiet after two failures and two
// recoveries. database.ClaimEventNotify holds the rule.
const NotifyCooldown = 6 * time.Hour

// Event is one occurrence of one problem.
type Event struct {
	AppID string
	Kind  Kind
	// Severity is database.EventSeverity*; empty means error.
	Severity string
	// Source is database.EventSource* — which subsystem noticed.
	Source string
	// Summary is one line, in the words of someone who has to fix it. It is
	// shown in every list and used as the mail subject, so it should name the
	// problem and, where there is one, the fix.
	Summary string
	// Detail is the excerpt that says why: a build-log tail, git's own stderr,
	// an HTTP status. Trimmed to DetailLimit before it is stored.
	Detail string
	// DeploymentID links the event to the deploy it came out of, when there is
	// one. It may name a row retention later prunes.
	DeploymentID string
	// Fingerprint is what makes two occurrences "the same problem" and is
	// therefore what the cooldown collapses on. Same (app, kind, fingerprint)
	// inside the window means one email, not many.
	//
	// Choosing it is the whole art: too coarse and a second, different failure
	// is swallowed; too fine and every poll is a new problem. See the table in
	// docs/dev-reference.md § App events.
	Fingerprint string
	// Silent records the row and tells nobody, whatever the switches say.
	//
	// It exists for a row whose news is told some other way, and never for one
	// that merely seems too noisy to mail — that is the cooldown's job. Three
	// today: a failed deploy a person started and is watching; one restart
	// below the flapping threshold, whose news, if it becomes news, is the
	// flapping event; and the **first** evaluation of an app by the dependency
	// sweep. Every advisory already on
	// every app is a first match, so without this the feature's opening act on
	// an install with forty apps is a burst of mail to forty owners about a
	// hundred findings that are years old — which is how a security feature
	// gets switched off on day one and described afterwards as noise.
	//
	// It suppresses the *message*, never the record: the row is written, the
	// tab shows it, and the estate view counts it, immediately.
	Silent bool
}

// DetailLimit bounds what is stored. A build log can be megabytes; what is
// useful here is the tail, and what is safe to put in an email body is less
// than that.
const DetailLimit = 4000

// Store is the slice of the database this package needs. Declared here rather
// than taken as a *database.DB so a test needs no Postgres, following the
// api.Mailer precedent.
type Store interface {
	InsertAppEvent(e *database.AppEvent) error
	ClaimEventNotify(eventID, appID, kind, fingerprint string, cooldown time.Duration) (bool, error)
	MarkEventNoticeSent(eventID string) error
	ResolveAppEvents(appID, kind, fingerprintPrefix string) (int, error)
	AppNotifyFailures(appID string) (bool, error)
	ListAppRecipients(appID string) ([]database.Recipient, error)
	GetApp(id string) (*database.App, error)
}

// Notifier is the slice of the mailer this package needs. A nil Notifier is
// fine and means nothing is sent — the same shape an install with no relay
// configured already has.
type Notifier interface {
	SendAsync(msg mailer.Message)
	BaseURL() string
}

// Poster is the slice of a *posting* channel this package needs, and it is a
// second interface rather than a second implementation of Notifier because the
// two kinds of channel do not address a message the same way: mail takes a
// recipient list this package computes, a poster takes an app id and works out
// the destination itself. A seam that hid that difference would have to invent
// a lowest common denominator neither wants.
//
// **The interface is unchanged and mail is still outside it.** internal/notify
// argued against a Channel abstraction over both and is still right about that.
// What it did not anticipate is seven channels addressed the way Slack is,
// which makes mail the outlier rather than half of a pair — so the narrow
// reversal is the *number* of posters, not the shape of one.
//
// An empty slice is fine and means nothing is posted.
type Poster interface {
	PostAsync(template, appID string, d notify.TemplateData) bool
	BaseURL() string
}

// Recorder is the seam. Construct one at startup and hand it to every worker
// that can notice something break.
type Recorder struct {
	db   Store
	mail Notifier
	// posters is every channel that addresses a destination. A slice where
	// there was a field, which is the whole of the platform seam's change: a
	// second chat tool is an entry here rather than a second field, a second
	// nil check and a second `told = told || …`.
	posters []Poster
	// cooldown is a field rather than the constant so a test does not have to
	// wait six hours to prove the window reopens.
	cooldown time.Duration
}

// NewRecorder builds a recorder. Every dependency may be nil: a nil Store makes
// the whole thing inert, and a nil channel records everything and tells that
// channel nothing.
//
// It holds no base URL of its own. The address a link points at is a channel's
// answer to give — they are the ones that know whether an administrator has set
// a public address — and a copy here would be a second thing to keep in step,
// which is how two notifications end up naming two different installs. Both
// channels resolve it through database.ResolvePublicBaseURL, so asking either
// gets the same answer; baseURL asks whichever one exists.
func NewRecorder(db Store, m Notifier, posters ...Poster) *Recorder {
	// A typed-nil interface holding a nil *mail.Service would pass a plain
	// != nil check and then panic on the first call, so callers pass a real one
	// or nothing; drop the obvious case here rather than guarding at every use.
	kept := make([]Poster, 0, len(posters))
	for _, p := range posters {
		if p != nil {
			kept = append(kept, p)
		}
	}
	return &Recorder{db: db, mail: m, posters: kept, cooldown: NotifyCooldown}
}

// baseURL is where links in a notification point. Either channel can answer and
// both answer identically; this exists only so an install with one of them
// configured and not the other still produces a working link.
func (r *Recorder) baseURL() string {
	if r.mail != nil {
		return r.mail.BaseURL()
	}
	for _, p := range r.posters {
		if base := p.BaseURL(); base != "" {
			return base
		}
	}
	return ""
}

// SetCooldown overrides the notification window. For tests.
func (r *Recorder) SetCooldown(d time.Duration) {
	if r != nil && d > 0 {
		r.cooldown = d
	}
}

// Record persists the event and, if this is the first occurrence of the problem
// inside the cooldown, emails the app's owners and editors.
//
// It returns nothing on purpose. Nothing a caller does should change based on
// whether a row was written or a relay answered: the deploy has already failed,
// the process has already died, and reporting that is not allowed to become a
// second failure.
func (r *Recorder) Record(ev Event) { r.RecordNotified(ev) }

// RecordNotified is Record, reporting whether this occurrence told anybody —
// false when it was silent, collapsed under the cooldown, or nobody is set up
// to hear. A caller that has a message of its own to send for the same
// occurrence asks this first, so one occurrence is one message.
func (r *Recorder) RecordNotified(ev Event) bool {
	if r == nil || r.db == nil || ev.AppID == "" || ev.Kind == "" {
		return false
	}

	severity := ev.Severity
	if severity == "" {
		severity = database.EventSeverityError
	}
	row := &database.AppEvent{
		AppID:        ev.AppID,
		Kind:         string(ev.Kind),
		Severity:     severity,
		Source:       ev.Source,
		Summary:      summaryLine(ev.Summary),
		Detail:       tail(ev.Detail, DetailLimit),
		DeploymentID: ev.DeploymentID,
		Fingerprint:  ev.Fingerprint,
		OccurredAt:   time.Now().UTC(),
	}
	if row.Summary == "" {
		row.Summary = string(ev.Kind)
	}
	if err := r.db.InsertAppEvent(row); err != nil {
		// The app was deleted while this was in flight. Expected rather than
		// broken: recording is out of the request path on purpose, so a
		// watcher observing a crash and an administrator pressing Delete are
		// always able to cross. Said at debug so the error level keeps meaning
		// what it says — the platform could not write down what it saw.
		if errors.Is(err, database.ErrAppGone) {
			slog.Debug("app event dropped: the app was deleted", "app_id", ev.AppID, "kind", ev.Kind)
			return false
		}
		slog.Error("recording app event failed", "app_id", ev.AppID, "kind", ev.Kind, "error", err)
		return false
	}

	if ev.Silent {
		// Recorded, and nobody told. The claim is deliberately *not* taken:
		// this occurrence is not the one anybody wanted to hear about, and
		// consuming the cooldown here would silence the next one, which is.
		slog.Debug("app event recorded silently",
			"app_id", ev.AppID, "kind", ev.Kind, "fingerprint", row.Fingerprint)
		return false
	}

	// Claim the right to send before consulting the switch. Taking the claim
	// either way is what stops a backlog of suppressed mail from arriving the
	// moment someone turns notifications back on.
	claimed, err := r.db.ClaimEventNotify(row.ID, row.AppID, row.Kind, row.Fingerprint, r.cooldown)
	if err != nil {
		slog.Error("claiming event notification failed", "app_id", ev.AppID, "kind", ev.Kind, "error", err)
		return false
	}
	if !claimed {
		slog.Debug("app event recorded, notification collapsed into an earlier one",
			"app_id", ev.AppID, "kind", ev.Kind, "fingerprint", row.Fingerprint)
		return false
	}

	// Record that anyone was actually told, which is a different fact from
	// holding the claim: the app's switch may be off. Clear reads this to
	// decide whether a recovery notice would mean anything.
	if r.notify("app_event", row.AppID, row.Summary, row.Detail, row.OccurredAt, severity) {
		if err := r.db.MarkEventNoticeSent(row.ID); err != nil {
			slog.Error("marking an app event as notified failed", "event_id", row.ID, "error", err)
		}
		return true
	}
	return false
}

// Notify sends one named template to an app's people, writing no event row.
//
// It exists for the platform decisions that are not malfunctions and are not
// the app's own history: the dependency policy locking an app is the first.
// Those already have a record — an audit row nobody was present for, and the
// app's own Dependencies tab — so a second copy in `app_events` would be a
// weaker duplicate of both, and the fingerprint machinery has nothing to
// collapse on when the thing being announced happens exactly once.
//
// Everything Record's send does, this does: the app's own notify_failures
// switch, every poster, and mail to the owners and editors. What it skips is
// the cooldown, because a guarded transition already guarantees one message.
//
// It reports whether anyone was told, and it never returns an error: **no send
// may fail or block the thing that triggered it.**
func (r *Recorder) Notify(template, appID, summary, detail, severity string) bool {
	if r == nil || r.db == nil || appID == "" || template == "" {
		return false
	}
	return r.notify(template, appID, summary, detail, time.Now().UTC(), severity)
}

// NotifyRequested is Notify for a message the app asked for that is not a
// failure — a job's `notify = "always"` success. The owner's failure switch
// ("email when it breaks") governs failures; turning it off silenced a success
// the app's own toft.toml declared, with nothing on any screen saying why
// (BB-97). Everything else is Notify's: the posters, the mail, no cooldown.
func (r *Recorder) NotifyRequested(template, appID, summary, detail, severity string) bool {
	if r == nil || r.db == nil || appID == "" || template == "" {
		return false
	}
	return r.send(template, appID, summary, detail, time.Now().UTC(), severity)
}

// Clear closes every open event of this kind for the app and, if any of them
// had been mailed about, sends one recovery notice.
//
// Called from the success path of whatever failed: a poll that reaches the
// remote again, a deploy that finishes, a process the health watcher finds
// running. Nobody is told a problem is over unless they were told it started.
func (r *Recorder) Clear(appID string, kind Kind) {
	r.clear(appID, kind, "", recoverySummary(kind))
}

// ClearAutomation closes the open failures of one job or task — the events
// whose fingerprint starts with `id:` — and names it in the recovery notice.
//
// Not Clear, because a job's success says nothing about another job: closing
// the whole kind told an owner their failing nightly job had recovered the
// moment an hourly one succeeded.
func (r *Recorder) ClearAutomation(appID string, kind Kind, id, name string) {
	if id == "" {
		return
	}
	r.clear(appID, kind, id+":", automationRecoverySummary(kind, name))
}

func (r *Recorder) clear(appID string, kind Kind, prefix, summary string) {
	if r == nil || r.db == nil || appID == "" || kind == "" {
		return
	}
	notified, err := r.db.ResolveAppEvents(appID, string(kind), prefix)
	if err != nil {
		slog.Error("clearing app events failed", "app_id", appID, "kind", kind, "error", err)
		return
	}
	if notified == 0 {
		return
	}
	r.notify("app_event_recovered", appID, summary, "", time.Now().UTC(), database.EventSeverityInfo)
}

// automationRecoverySummary is recoverySummary with the automation named,
// because only that one recovered.
func automationRecoverySummary(kind Kind, name string) string {
	if name == "" {
		return recoverySummary(kind)
	}
	switch kind {
	case KindJobRunFailed, KindJobRunOverdue, KindJobScheduleSkipped:
		return fmt.Sprintf("The job %q is succeeding again", name)
	}
	return recoverySummary(kind)
}

// notify renders and dispatches to every configured channel, and reports
// whether anyone was told through any of them. Every early return here is a
// normal outcome, not a failure: no channel configured, notifications off for
// this app, nobody with an address to send to.
//
// "Told" is as far as truth goes — both dispatches are fire-and-forget, and an
// install with no relay or no token drops them with a debug line. That is
// deliberate: no caller of this package may behave differently because someone
// else's server was down.
//
// This is the one place the app owner's notify_failures switch is applied, and
// it governs both channels — for failures: NotifyRequested, a success the app
// asked to be told about, goes past it through send. It is deliberately not duplicated per channel:
// "stop telling people when my app breaks" is one wish, and an owner who has to
// express it twice will express it once and be surprised.
func (r *Recorder) notify(template, appID, summary, detail string, when time.Time, severity string) bool {
	if r.mail == nil && len(r.posters) == 0 {
		return false
	}

	on, err := r.db.AppNotifyFailures(appID)
	if err != nil {
		slog.Error("reading an app's notification setting failed", "app_id", appID, "error", err)
		return false
	}
	if !on {
		slog.Debug("app event recorded, its app has failure notifications switched off", "app_id", appID)
		return false
	}
	return r.send(template, appID, summary, detail, when, severity)
}

// send is notify past the switch: render and dispatch to every channel.
func (r *Recorder) send(template, appID, summary, detail string, when time.Time, severity string) bool {
	if r.mail == nil && len(r.posters) == 0 {
		return false
	}
	summary = summaryLine(summary)
	app, err := r.db.GetApp(appID)
	if err != nil || app == nil {
		slog.Error("loading the app for a failure notification failed", "app_id", appID, "error", err)
		return false
	}

	name := app.Name
	if name == "" {
		name = app.Slug
	}
	base := r.baseURL()
	data := notify.TemplateData{
		BaseURL:     base,
		AppName:     name,
		EventTitle:  summary,
		EventDetail: detail,
		EventWhen:   when.Format("2 Jan 2006 at 15:04 MST"),
		Severity:    severity,
		// A pointer, not a credential. Whoever follows it signs in and is
		// authorized normally — the same rule the promotion mail follows. The
		// **id**, for the reason on notify.AppPageURL.
		Link: notify.AppPageURL(base, app.ID),
	}

	told := false

	// The posters first, and independently of each other and of mail: each is
	// addressed to a destination rather than to people, so an app whose owners
	// have no email address on file — the case the warning below exists for —
	// still reaches the team that operates it. Whether this app has a given
	// integration at all is PostAsync's question, not this function's.
	//
	// Every poster is asked, and one refusing does not stop the next: an
	// administrator who attached both Slack and Teams to an app meant both.
	for _, p := range r.posters {
		if p.PostAsync(template, appID, data) {
			told = true
		}
	}

	if r.mail != nil {
		people, err := r.db.ListAppRecipients(appID)
		if err != nil {
			slog.Error("listing app recipients failed", "app_id", appID, "error", err)
			return told
		}
		if len(people) == 0 {
			// A warning rather than silence: this is an app that will keep
			// breaking with nobody being emailed, and the fix is a grant or an
			// address, both of which an administrator can make.
			// A warning rather than silence: this is an app that will keep
			// breaking with nobody being emailed. For a group-owned app the
			// fix has one more shape than it used to — an address on the
			// group, as well as a grant or a personal address.
			slog.Warn("app failed with nobody to email — no owner or editor has an email address on file, "+
				"and no owning group has one either", "app", app.Slug)
			return told
		}
		to := make([]string, 0, len(people))
		for _, u := range people {
			to = append(to, u.Address)
		}
		msg, err := mailer.Render(template, data)
		if err != nil {
			slog.Error("rendering a failure notification failed", "template", template, "error", err)
			return told
		}
		msg.To = to
		r.mail.SendAsync(msg)
		told = true
	}

	return told
}

// recoverySummary is what a recovery notice says happened. Phrased as the
// problem that ended rather than the kind that was cleared, because the reader's
// last mail from us was about the problem.
func recoverySummary(kind Kind) string {
	switch kind {
	case KindGitAuthFailed:
		return "The repository credential is working again"
	case KindGitFetchFailed:
		return "The repository is reachable again"
	case KindDeployFailed:
		return "A deploy succeeded"
	case KindAppCrashed:
		return "The app is running again"
	case KindAppFlapping:
		return "The app has settled"
	case KindAppNotRestarting:
		return "The app is running again"
	case KindAppCPUThrottled:
		return "The app is no longer held at its CPU limit"
	case KindJobRunFailed:
		return "Job runs are succeeding again"
	case KindSourceBlocked:
		return "The app's repository is an approved source again"
	case KindDependencyBlocked:
		return "A deploy passed this install's dependency policy"
	default:
		return fmt.Sprintf("%s has cleared", strings.ReplaceAll(string(kind), "_", " "))
	}
}

// tail keeps the last n bytes, which for a build log is where the failure is.
// It cuts at a line boundary when it can, so the excerpt does not open
// mid-token, and says it was truncated so nobody reads it as the whole story.
//
// The output is app-controlled, so it is cleaned of a NUL or invalid UTF-8 and
// cut on a rune boundary: either would make Postgres refuse the row, and the
// failure whose output was strange enough to carry one would go unrecorded.
func tail(s string, n int) string {
	if len(s) <= n {
		return textutil.Clean(s)
	}
	cut := textutil.ClipTail(s, n)
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return "… (earlier output trimmed)\n" + cut
}

// SummaryLimit bounds an event's summary, which is the Events row, the mail
// subject and a Slack section title.
const SummaryLimit = 500

// summaryLine reduces a summary to its first line, clipped to SummaryLimit
// bytes and cleaned. A summary can carry text a job's own code reported, so no
// caller may produce an unbounded subject; the whole text stays in the detail.
func summaryLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return textutil.Clip(s, SummaryLimit)
}
