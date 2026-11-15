package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"toft/cli/internal/client"
)

// The lifecycle verbs: restart, start, stop.
//
// Flat verbs, beside `toft deploy` and `toft rollback`, because they are done to
// the app this directory is linked to and naming it twice — `toft app restart` —
// would make `toft deploy` the odd one out.

const restartUsage = `toft restart — restart the app, starting it if it is stopped

Flags:
  --app <slug>        Which app
  --wait              Wait until the app is up, and fail if it does not come up
  --wait-timeout <d>  How long to wait with --wait (default 2m)
  --json              The outcome as JSON
`

const startUsage = `toft start — start the app, if it is not already running

A running app is left alone: the answer is "already running" (--json: outcome
already_running) and nothing is restarted, so "make sure it is up" is safe to
say twice. ` + "`toft restart`" + ` is the bounce.

Flags:
  --app <slug>        Which app
  --wait              Wait until the app is up, and fail if it does not come up
  --wait-timeout <d>  How long to wait with --wait (default 2m)
  --json              The outcome as JSON
`

const stopUsage = `toft stop — stop the app

Flags:
  --app <slug>        Which app
  --json              The outcome as JSON
`

// defaultWaitTimeout bounds --wait. An isolated start is a pod schedule and an
// image pull, and the server's own readiness bound is 90 seconds; two minutes
// leaves room for that to complete rather than cutting it off just before.
const defaultWaitTimeout = 2 * time.Minute

// restartResponse is what /restart answers with, in both its shapes.
//
// 200 is the start having completed inside the wake hold; 202 is it still
// running, with the server's own sentence about why that takes a minute. Both
// are successes and the client must not turn either into the other.
type restartResponse struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Action is "start" or "restart": which of the two the route judged this
	// was, the judgement its audit row records. Absent from an older install.
	Action string `json:"action,omitempty"`
	// Note is what the restart did not do, where that is the part a caller
	// came for — a document's page changes only at the next deploy.
	Note string `json:"note,omitempty"`
}

// word is the past tense to print: the server's status, except that a
// completed restart of an app that was not running is said as the start it
// was (docs/bugs 2026-10-28 D20) — by the route's own `action`, never a
// guess from a status this client read before the call.
func (r restartResponse) word() string {
	if r.Status == "restarted" && r.Action == "start" {
		return "started"
	}
	return r.Status
}

// wordFor is word for an app of this framework. A restart of a static app or
// a document runs no process of the app's own, so "restarted" beside a note
// saying nothing changed contradicts it (drills 2026-10-28 evening #2): say
// what happened instead. A start of a stopped one is still "started".
func (r restartResponse) wordFor(framework string) string {
	w := r.word()
	if w != "restarted" {
		return w
	}
	switch {
	case framework == "static":
		return "unchanged"
	case isDocumentFramework(framework):
		return "served again"
	}
	return w
}

// settledWordFor is wordFor once --wait has seen the start finish: a 202's
// "restarting" has by then become the restart it announced.
func (r restartResponse) settledWordFor(framework string) string {
	if r.Status == "restarting" {
		r.Status = "restarted"
	}
	return r.wordFor(framework)
}

// staticRestartNote is restartNote's sentence for a static app, which the
// install does not send: its files are served as they are, by no process of
// its own, so a restart changes nothing and an environment variable reaches
// nothing (drills 2026-10-28 #8).
const staticRestartNote = "This is a static app: its files are served as they are, and no process of its own " +
	"reads an environment — the restart changed nothing, and a variable set on it reaches nothing."

// noteFor is the install's note, or this client's for the one framework the
// install says nothing about — never beside a start, which served a stopped
// static app again and so changed something.
func (r restartResponse) noteFor(framework string) string {
	if r.Note == "" && r.Status != "" && framework == "static" && r.Action != "start" {
		return staticRestartNote
	}
	return r.Note
}

func cmdRestart(e *env, args []string) error { return restartOrStart(e, args, "restart", restartUsage) }
func cmdStart(e *env, args []string) error   { return restartOrStart(e, args, "start", startUsage) }

func restartOrStart(e *env, args []string, name, usage string) error {
	var g globalFlags
	fs := newFlags(name, usage)
	g.register(fs)
	wait := fs.Bool("wait", false, "wait until the app is up")
	timeout := fs.Duration("wait-timeout", defaultWaitTimeout, "how long to wait with --wait")
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
	app, err := s.resolveApp(appValue.Value)
	if err != nil {
		return err
	}

	if name == "start" && app.Status == appStatusRunning {
		// **Start is idempotent; restart is the bounce** (BB-041, fourth
		// 2026-11-03 list). The one route does both, so a start of a
		// running app was a restart with downtime — the wrong answer to "make
		// sure it is up". The status read here can race a stop; that start is
		// the next one, and nothing is lost by not bouncing.
		if g.json {
			// status is the outcome's word, as on every other start/restart
			// document (BB-069); settled carries the app's own state.
			doc := map[string]any{"app": app.Slug, "status": "already_running", "outcome": "already_running",
				"action": "none", "detail": "the app is already running — `toft restart` restarts it"}
			if *wait {
				doc["settled"] = app.Status
			}
			return json.NewEncoder(e.out.Stdout).Encode(doc)
		}
		e.out.Answer("%s  %s", e.out.Bold(app.Slug), actionBadge(e, "already running"))
		e.out.Dim("  Nothing was restarted — `toft restart --app %s` does that.", app.Slug)
		return nil
	}

	var got restartResponse
	if err := s.client.Do(http.MethodPost, "/api/v1/apps/"+app.ID+"/restart", nil, &got); err != nil {
		return err
	}
	got.Note = got.noteFor(app.Framework)

	// The server's word, not a substitute for it. One route serves both, and
	// the platform has already decided — correctly, in the audit log — which
	// of the two this was; its `action` says so, and word() reads it. A
	// client-side second opinion would be a racing one.
	if !*wait {
		if g.json {
			// `outcome` is the word the line prints — "unchanged" for a
			// static app, never "restarted" (drills 2026-10-29) — and
			// `status` is the same word: the install's raw "restarted" beside
			// a start, or beside an unchanged static app, contradicted it
			// (BB-062, fifth 2026-11-03 list).
			doc := map[string]any{
				"app":     app.Slug,
				"status":  got.wordFor(app.Framework),
				"outcome": got.wordFor(app.Framework),
				"detail":  got.Detail,
			}
			if got.Action != "" {
				doc["action"] = got.Action
			}
			if got.Note != "" {
				doc["note"] = got.Note
			}
			return json.NewEncoder(e.out.Stdout).Encode(doc)
		}
		e.out.Answer("%s  %s", e.out.Bold(app.Slug), actionBadge(e, got.wordFor(app.Framework)))
		if got.Detail != "" {
			e.out.Say("  %s", got.Detail)
			e.out.Dim("  Pass --wait to wait for it and fail if it does not come up.")
		}
		if got.Note != "" {
			e.out.Say("  %s", got.Note)
		}
		return nil
	}

	final, err := waitForStart(e, s, app.Slug, *timeout)
	if g.json {
		doc := map[string]any{
			"app":     app.Slug,
			"outcome": got.wordFor(app.Framework),
			"detail":  got.Detail,
			"settled": final,
		}
		if err == nil {
			doc["outcome"] = got.settledWordFor(app.Framework)
		}
		doc["status"] = doc["outcome"] // never the install's contradicting "restarted" (BB-062)
		if got.Action != "" {
			doc["action"] = got.Action
		}
		if got.Note != "" {
			doc["note"] = got.Note
		}
		encodeErr := json.NewEncoder(e.out.Stdout).Encode(doc)
		if err != nil {
			return err
		}
		return encodeErr
	}
	if err != nil {
		return err
	}
	// Settled, the line says what happened — the word --json carries as
	// outcome — rather than the state it settled in: "running" after a restart
	// read as though nothing had been done (drills 2026-10-30). Every state
	// waitForStart returns without an error is the start having worked.
	e.out.Answer("%s  %s", e.out.Bold(app.Slug), actionBadge(e, got.settledWordFor(app.Framework)))
	if final == appStatusSleeping {
		// Reached only from `running`: the idle reaper sleeps running apps and
		// nothing else. So this is the start having worked and the app having
		// since been slept for want of traffic — not a failure to come up.
		e.out.Dim("  It came up and has since been slept for being idle. The next request wakes it.")
	}
	if got.Note != "" {
		e.out.Say("  %s", got.Note)
	}
	return nil
}

// App statuses this client reads. The set is the server's
// (internal/database/models.go); these are the ones --wait has to judge.
const (
	appStatusRunning  = "running"
	appStatusSleeping = "sleeping"
	appStatusCrashed  = "crashed"
	appStatusFailed   = "failed"
	// appStatusReady is a batch app with a release: live, and serving nothing
	// because it has no service. Settled and well.
	appStatusReady = "ready"
)

// startSettled says whether an app status ends a --wait, and whether it ended
// it well.
//
// `sleeping` is a success, and that is the one non-obvious entry. A restart
// writes exactly two outcomes — `running` or `crashed` — and the idle reaper
// only ever sleeps an app that is `running`. So an app found sleeping inside
// the wait window is an app that started and was then slept for being idle,
// which is the start having worked. Calling it a failure would fail a pipeline
// for a feature working as designed.
//
// `building`, `stopped` and anything unknown are not terminal: a deploy may be
// in flight, and an unknown word from a newer install is not something this
// binary should rule on. The timeout is what ends those.
func startSettled(status string) (terminal, ok bool) {
	switch status {
	case appStatusRunning, appStatusSleeping, appStatusReady:
		return true, true
	case appStatusCrashed, appStatusFailed:
		return true, false
	}
	return false, false
}

// waitForStart polls the app until its status settles, and turns the settled
// status into an outcome. Only here does the exit code of a restart mean
// anything: without --wait, a 202 asserts that the platform accepted the
// restart and nothing more.
func waitForStart(e *env, s *session, slug string, timeout time.Duration) (string, error) {
	last := ""
	waiting := e.out.Waiting(e.ctx, "waiting for "+slug+" to come up")
	defer waiting.Stop()
	pollErr := client.Poll(e.ctx, client.PollOptions{Timeout: timeout}, func(context.Context) (bool, error) {
		// `starting` before `status`: while a start is in flight the row still
		// says what it said before it, so a running app read `running` at once
		// and --wait returned while the old process was still the one serving.
		// An install too old to send it reads false, which is the old rule.
		var body struct {
			App      appSummary `json:"app"`
			Starting bool       `json:"starting"`
		}
		if err := s.client.Do(http.MethodGet, "/api/v1/apps/by-slug/"+url.PathEscape(slug)+"?view=brief", nil, &body); err != nil {
			var apiErr *client.Error
			if errors.As(err, &apiErr) && apiErr.NotFound() {
				return false, s.noSuchApp(slug)
			}
			return false, err
		}
		last = body.App.Status
		if body.Starting {
			return false, nil
		}
		terminal, _ := startSettled(last)
		return terminal, nil
	})

	if pollErr != nil {
		if errors.Is(pollErr, client.ErrPollTimeout) {
			return last, fmt.Errorf("%s is still %s after %s — `toft events` says what has gone wrong",
				slug, orDefault(last, "unknown"), timeout)
		}
		return last, pollErr
	}
	if _, ok := startSettled(last); !ok {
		return last, fmt.Errorf("%s did not come up: %s — `toft events` says why", slug, last)
	}
	return last, nil
}

func cmdStop(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("stop", stopUsage)
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
	app, err := s.resolveApp(appValue.Value)
	if err != nil {
		return err
	}

	// Still sent when the app already reads stopped — the install is the one
	// that decides — but said as the no-op it was. "stopped" alone, the same
	// word as a stop that did something, left a caller unable to tell.
	was := app.Status
	var got restartResponse
	if err := s.client.Do(http.MethodPost, "/api/v1/apps/"+app.ID+"/stop", nil, &got); err != nil {
		return err
	}
	changed := was != got.Status
	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{
			"app":     app.Slug,
			"status":  got.Status,
			"changed": changed,
		})
	}
	if !changed && got.Status == "stopped" {
		e.out.Answer("%s  %s", e.out.Bold(app.Slug), actionBadge(e, "already stopped"))
		return nil
	}
	e.out.Answer("%s  %s", e.out.Bold(app.Slug), actionBadge(e, got.Status))
	return nil
}

// actionBadge is the word restart, start and stop read back, badged on a
// terminal. The past tenses are the routes' own and not statuses, so they are
// placed here rather than in the kit's status vocabulary.
func actionBadge(e *env, word string) string {
	if !e.out.Rich() {
		return word
	}
	switch word {
	case "restarted", "started":
		return e.out.OK("✓ " + word)
	case "unchanged", "served again":
		return e.out.Dimmed("○ " + word)
	case "already stopped", "already running":
		return e.out.Dimmed("○ " + word)
	}
	return e.out.Badge(word)
}
