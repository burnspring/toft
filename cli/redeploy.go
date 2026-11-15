package main

import (
	"fmt"
	"net/http"
	"strings"
)

const redeployUsage = `toft redeploy — build the app again from its own source

Builds from the repository the app names — or, for an app created from an
upload, from the source last sent to it, which is the web UI's "Redeploy current
version". Either way nothing on this disk is read and nothing is uploaded — that
is ` + "`toft deploy`" + `, and it is a different thing.

Flags:
  --app <slug>        Which app
  --logs              Interleave the build log
  --no-watch          Start the build and return; print the deployment id
  --json              NDJSON progress frames on stdout, then one result object
  --verify <path>     Once it succeeds, request this path as you and fail
                      unless it answers 2xx or 3xx (see toft fetch)
  --verify-timeout <d> How long a 5xx may be retried with --verify (default 30s)
`

// cmdRedeploy is its own command rather than a flag on `toft deploy`.
//
// ADR §42 leads with "`toft deploy` pushes what is on the disk, uncommitted edits
// included, for every app." A `toft deploy --from-source` would make that
// sentence conditional, and the condition would be invisible in a script
// skimmed six months later — the deploy that does not deploy what you are
// looking at.
func cmdRedeploy(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("redeploy", redeployUsage)
	g.register(fs)
	showLogs := fs.Bool("logs", false, "interleave the build log")
	e.verifyFlags(fs)
	noWatch := fs.Bool("no-watch", false, "start the build and return")
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}
	if *noWatch && e.verify != "" {
		return usageCodef("conflicting_flags", "--verify needs the deploy watched; drop --no-watch")
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

	// Said before it starts, because on an app that has been pushed to this is
	// precisely the surprise the command exists to make visible: the code about
	// to be built is the repository's, not the code somebody last sent from a
	// laptop. `toft status` states the divergence; this is the command that
	// resolves it, and it should not do so silently.
	if err := sayWhatWillBeBuilt(e, app); err != nil {
		return err
	}

	var deploy struct {
		ID string `json:"id"`
	}
	if err := s.client.Do(http.MethodPost, "/api/v1/apps/"+app.ID+"/deploy", nil, &deploy); err != nil {
		return err
	}

	if *noWatch {
		if g.json {
			return startedDocument(e, s, app, deploy.ID)
		}
		e.out.Answer("%s", deploy.ID)
		suggest(e, "Watch it with", "toft deploy --watch "+deploy.ID, "watches it")
		return nil
	}
	return watchDeploy(e, s, app, deploy.ID, &g, *showLogs)
}

// sayWhatWillBeBuilt names what is about to be built, and refuses when there is
// nothing to build from.
//
// An app created from an upload has `upload://` as its source and no
// repository. The platform rebuilds it from the source last sent — the staged
// upload, else the release serving now — which is what an interpreter upgrade
// or a changed environment variable wants, and what the web UI's "Redeploy
// current version" already does. Only an uploaded app that has never gone live
// has nothing to rebuild, and that is refused here with the command that would
// work, rather than a build that fails on "No source available".
func sayWhatWillBeBuilt(e *env, app *appSummary) error {
	if app.RepoURL == "" || strings.HasPrefix(app.RepoURL, "upload://") {
		if app.CurrentRelease == "" {
			return usageCodef("nothing_to_rebuild", "%s has nothing to rebuild yet — it was created from an upload and "+
				"no deploy of it has succeeded. `toft deploy` sends this directory", app.Slug)
		}
		e.out.Say("Building %s again from %s", e.out.Bold(app.Slug),
			e.out.Accent("the source last uploaded to it"))
		e.out.Dim("  Nothing on this disk is read — `toft deploy` sends this directory instead.")
		return nil
	}
	e.out.Say("Building %s from %s", e.out.Bold(app.Slug),
		e.out.Accent(fmt.Sprintf("%s · %s", app.RepoURL, orDefault(app.Branch, "its default branch"))))
	return nil
}
