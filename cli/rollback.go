package main

import (
	"fmt"
	"net/http"
)

const rollbackUsage = `toft rollback [<deployID>] — make an earlier deployment current again

With no argument it names the successful deployment before the one serving and
rolls back to that — so a second rollback goes further back, never forward to
the release the first one left. Name a newer deployment to go forward. Its
directory is started as it was built, health-checked, and traffic moves onto
it — unless it was built for an address the app no longer has (a rename, a
hostname switched), when it is rebuilt from its commit as a new deployment.

Flags:
  --app <slug>        Which app
  --yes               Do not ask. Required when nothing can be asked —
                      in CI, or with stdin redirected, this refuses rather
                      than hanging on a prompt nobody can see.
  --no-watch          Start it and return
  --json              NDJSON progress frames on stdout, then one result object
  --verify <path>     Once it succeeds, request this path as you and fail
                      unless it answers 2xx or 3xx (see toft fetch)
  --verify-timeout <d> How long a 5xx may be retried with --verify (default 30s)
`

func cmdRollback(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("rollback", rollbackUsage)
	g.register(fs)
	noWatch := fs.Bool("no-watch", false, "start it and return")
	e.verifyFlags(fs)
	if err := parseOperands(fs, args, 1); err != nil {
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

	deployments, err := s.deployments(app.ID)
	if err != nil {
		return err
	}
	target, err := rollbackTarget(deployments, fs.Arg(0))
	if err != nil {
		return err
	}

	// **Named before it acts.** "Roll back" with no argument is a command whose
	// effect the person cannot see from what they typed, and the fix is to say
	// what it resolved to before asking.
	from := target.Branch
	if target.CommitSHA != "" {
		from += " · " + shortSHA(target.CommitSHA)
	}
	when := ""
	if target.FinishedAt != nil {
		when = fmt.Sprintf(", built %s ago", roughAge(nowSince(*target.FinishedAt)))
	}
	// Forward is a switch, not a rollback: naming a deployment newer than the
	// one serving moves the app ahead, and the sentence says which way.
	verb, question := "Rolling %s back to %s (%s%s)", fmt.Sprintf("roll %s back", app.Slug)
	if newerThanServing(deployments, target) {
		verb, question = "Switching %s forward to %s (%s%s)", fmt.Sprintf("switch %s forward", app.Slug)
	}
	e.out.Say(verb, e.out.Bold(app.Slug), target.ID, from, when)

	if err := confirm(e, &g, question); err != nil {
		return err
	}

	var started struct {
		ID string `json:"id"`
		// SourceDeployID is the release this rollback restores — the one that
		// is serving once it succeeds. The row named by ID is the platform's
		// transient record of the switch and is deleted on success.
		SourceDeployID string `json:"source_deploy_id"`
		// Rebuild is the install saying the release was built for another
		// address and is rebuilt: the row named by ID then owns a fresh tree
		// and is what serves, so it is the id to name (TOFT-BB-003).
		Rebuild bool `json:"rebuild"`
	}
	path := fmt.Sprintf("/api/v1/apps/%s/deployments/%s/rollback", app.ID, target.ID)
	if err := s.client.Do(http.MethodPost, path, nil, &started); err != nil {
		return err
	}

	if *noWatch {
		// A document under --json, as deploy --no-watch prints: a bare id on
		// stdout was not JSON, and jq failed on it (code review 2026-11-11,
		// L-3).
		if g.json {
			return startedDocument(e, s, app, started.ID)
		}
		e.out.Answer("%s", started.ID)
		return nil
	}

	// The same watcher a deploy uses, because a rollback is a deploy: it is
	// started, it walks the same steps, it can fail at `start` or `ready`, and
	// a caller told to parse one shape must not meet a second one here.
	// `--json` used to be accepted and ignored on this command, which is worse
	// than not offering it — a script asking for JSON silently got a table.
	if started.Rebuild {
		e.out.Say("%s was built for another address; it is rebuilt as deployment %s.", target.ID, started.ID)
	} else {
		e.servingAfter = orDefault(started.SourceDeployID, target.ID)
	}
	return watchDeploy(e, s, app, started.ID, &g, false)
}

// rollbackTarget resolves the release to restore: the one named, or the
// previous successful one.
//
// "Previous" means the newest successful release **older than the one
// serving** — not simply the second row, because the rows above it may be
// failed builds, and not "any release but the serving one", which after a
// rollback was the release just rolled away from: a second argument-less
// rollback went forward. Rows are newest first, so older is below the serving
// row. With nothing serving, the newest success.
func rollbackTarget(deployments []deployment, named string) (*deployment, error) {
	if named != "" {
		id, err := matchDeploymentID(deployments, named)
		if err != nil {
			return nil, err
		}
		for i := range deployments {
			if deployments[i].ID == id {
				return &deployments[i], nil
			}
		}
	}

	serving := servingRelease(deployments)
	// A rebuilt rollback is serving a copy of an older release, from a row
	// newer than everything: "older than serving" is older than the release
	// it restored, or a third rollback goes forward (BB-005).
	anchor := servingAnchor(deployments, serving)
	// The serving row names the release it rebuilt and that row is gone —
	// pruned by an install from before it kept it, or deleted. Where the
	// serving release stands is then unknown, and every row is as likely to
	// be newer as older: refuse, rather than guess and go forward to the
	// release the last rollback left (TOFT-BB-001, 2026-11-04 second list).
	if serving != nil && anchor == serving && rebuiltFrom(serving) != "" {
		return nil, usageCodef("nothing_older", "the serving deployment (%s) is a rebuild of %s, which is no "+
			"longer in the history, so which deployments are older than it is unknown. Name the one to "+
			"restore: `toft deployments list` lists them, `toft rollback <deployID>` restores one",
			serving.ID, rebuiltFrom(serving))
	}
	below := serving == nil
	var newer *deployment
	for i := range deployments {
		d := &deployments[i]
		if serving != nil && (d.ID == serving.ID || d.ID == anchor.ID) {
			if d.ID == anchor.ID {
				below = true
			}
			continue
		}
		if d.Status != "success" {
			continue
		}
		// Another rebuild of the release that is serving is that release.
		if anchor != serving && (sameRelease(d, anchor) || d.RestoredFrom == anchor.ID) {
			continue
		}
		if below {
			return d, nil
		}
		if newer == nil {
			newer = d
		}
	}
	if newer != nil {
		return nil, usageCodef("nothing_older", "there is no successful deployment older than the "+
			"serving one (%s) to roll back to. %s is newer — `toft rollback %s` switches forward to it",
			serving.ID, newer.ID, newer.ID)
	}
	return nil, usageCodef("nothing_older", "there is no earlier successful deployment to roll back to")
}

// servingAnchor is where the serving release stands in the history. Usually
// its own row; for a rollback the install rebuilt for a changed base path (its
// row triggered by "rollback", owning a fresh tree, newest of all), the row it
// was rebuilt from — the next older success with the same commit and branch.
//
// An install that records restored_from says which row that is; the commit
// match is for an older one that does not.
func servingAnchor(deployments []deployment, serving *deployment) *deployment {
	if serving != nil && serving.RestoredFrom != "" {
		for i := range deployments {
			if deployments[i].ID == serving.RestoredFrom {
				return &deployments[i]
			}
		}
	}
	if serving == nil || serving.TriggeredBy != "rollback" || serving.CommitSHA == "" {
		return serving
	}
	past := false
	for i := range deployments {
		d := &deployments[i]
		if d.ID == serving.ID {
			past = true
			continue
		}
		if past && d.Status == "success" && d.TriggeredBy != "rollback" && sameRelease(d, serving) {
			return d
		}
	}
	return serving
}

// rebuiltFrom is the release a row rebuilt — a move's, or a rollback the
// install rebuilt for a new address — as the install records it; "" for any
// other row.
func rebuiltFrom(d *deployment) string {
	if d.RestoredFrom != "" && d.RestoredFrom != d.ID &&
		(d.TriggeredBy == "move" || d.TriggeredBy == "rollback") {
		return d.RestoredFrom
	}
	return ""
}

// sameRelease is two rows built from one commit of one branch.
func sameRelease(a, b *deployment) bool {
	return a.CommitSHA != "" && a.CommitSHA == b.CommitSHA && a.Branch == b.Branch
}

// newerThanServing reports whether target sits above the serving release in
// the newest-first list.
func newerThanServing(deployments []deployment, target *deployment) bool {
	serving := servingRelease(deployments)
	if serving == nil || serving.ID == target.ID {
		return false
	}
	anchor := servingAnchor(deployments, serving)
	for _, d := range deployments {
		switch d.ID {
		case serving.ID:
			if anchor == serving {
				return false
			}
		case target.ID:
			return true
		case anchor.ID:
			return false
		}
	}
	return false
}
