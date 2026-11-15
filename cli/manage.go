package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"toft/cli/internal/ui"
)

// The verbs that used to be `toft api` and a hand-written body: who may open
// the app, removing it, and its deployment history.
//
// Each is a rendering of one route the Settings and Deployments tabs already
// call, and none adds a rule of its own — the install decides who may make an
// app public or delete it, and says so in the envelope when it refuses.

const visibilityUsage = `toft visibility — who may open the app

Usage:
  toft visibility                    Print it
  toft visibility <level>            Change it: private, authenticated or public

  private        only people granted access (a new app starts here)
  authenticated  anybody signed in to this install
  public         anybody, signed in or not

Flags:
  --app <slug>        Which app
  --json              Machine-readable output on stdout

An install can refuse public apps, and a license can too; the refusal says
which. A private app answers an anonymous request with "authentication
required" — that is the app working, and your own key opens it (toft fetch).
`

var visibilityLevels = map[string]bool{"private": true, "authenticated": true, "public": true}

func cmdVisibility(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("visibility", visibilityUsage)
	g.register(fs)
	level, rest := takeOperand(args)
	if err := parseAfter(fs, rest, level); err != nil {
		return skipHelp(err)
	}
	if level == "" && fs.NArg() > 0 {
		level = fs.Arg(0)
	}
	if level != "" && !visibilityLevels[level] {
		return usageCodef("invalid_visibility", "%q is not a visibility — private, authenticated or public", level)
	}
	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}

	before, err := s.visibility(app.ID)
	if err != nil {
		return err
	}
	if level != "" && level != before {
		if err := s.client.Do(http.MethodPatch, "/api/v1/apps/"+app.ID,
			map[string]string{"visibility": level}, nil); err != nil {
			return err
		}
	}
	now := before
	if level != "" {
		now = level
	}
	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{
			"app": app.Slug, "visibility": now, "changed": now != before})
	}
	switch {
	case level == "":
		e.out.Answer("%s  %s", e.out.Bold(app.Slug), now)
	case now == before:
		e.out.Answer("%s  %s (unchanged)", e.out.Bold(app.Slug), now)
	default:
		e.out.Answer("%s  %s → %s", e.out.Bold(app.Slug), before, now)
	}
	return nil
}

func (s *session) visibility(appID string) (string, error) {
	var body struct {
		App struct {
			Visibility string `json:"visibility"`
		} `json:"app"`
	}
	err := s.client.Do(http.MethodGet, "/api/v1/apps/"+appID, nil, &body)
	return body.App.Visibility, err
}

const deleteUsage = `toft delete <slug> — delete an app

Removes the app, every release it has, its environment variables, schedules,
grants and data. There is no undo.

The slug is required, as the operand or as --app, and is never taken
from toft.toml or TOFT_APP: a directory being linked to an app is not a
statement that it should go.

Flags:
  --yes               Do not ask. Required when nothing can be asked
  --json              Machine-readable output on stdout
`

func cmdDelete(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("delete", deleteUsage)
	g.register(fs)
	slug, rest := takeOperand(args)
	if err := parseAfter(fs, rest, slug); err != nil {
		return skipHelp(err)
	}
	if slug == "" {
		slug = operandOr("", fs)
	}
	// --app is typed on this command line too, so it names the app as surely
	// as the operand; what is refused is an app inferred from TOFT_APP or
	// toft.toml, which g.app never holds (setup resolves those later).
	if slug == "" {
		slug = g.app
	}
	if slug == "" {
		return usageCodef("missing_argument", "toft delete needs the app's slug, typed out: toft delete <slug> — it is never taken from TOFT_APP or the linked directory")
	}
	if g.app != "" && g.app != slug {
		return usageCodef("unexpected_argument", "toft delete %s was given --app %s as well; name one app", slug, g.app)
	}
	s, err := e.setup(&g)
	if err != nil {
		return err
	}
	// Typed out, always: the refusal names no file it was read from
	// (BB-020, third 2026-11-03 list).
	s.slugTyped = true
	app, err := s.resolveApp(slug)
	if err != nil {
		return err
	}
	if err := confirm(e, &g, fmt.Sprintf("permanently delete %s with every release and setting it has", app.Slug)); err != nil {
		return err
	}
	if err := s.client.Do(http.MethodDelete, "/api/v1/apps/"+app.ID, nil, nil); err != nil {
		return err
	}
	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]string{"deleted": app.Slug, "id": app.ID})
	}
	e.out.Answer("%s deleted", app.Slug)
	return nil
}

const deploymentsUsage = `toft deployments — the app's deployment history

Usage:
  toft deployments list              Newest first, marking what is serving
  toft deployments prune             Delete every deployment but the serving one
  toft deployments delete <id>       Delete one

Flags:
  --app <slug>        Which app
  --yes               Answer the confirmation (prune, delete)
  --json              Machine-readable output on stdout

A deleted deployment is no longer a rollback target. The serving release, a
build still running and the release a rollback is returning to are never
deleted: prune skips them, and delete refuses one and says why.
`

func cmdDeployments(e *env, args []string) error {
	if len(args) == 0 {
		ui.Help(os.Stderr, deploymentsUsage)
		return usageCodef("missing_argument", "toft deployments needs a subcommand: list, prune or delete")
	}
	switch args[0] {
	case "list":
		return deploymentsList(e, args[1:])
	case "prune":
		return deploymentsDelete(e, "prune", args[1:])
	case "delete":
		return deploymentsDelete(e, "delete", args[1:])
	case "-h", "--help", "help":
		ui.Help(os.Stdout, deploymentsUsage)
		return nil
	}
	return unknownSubcommand("deployments", args, "list, prune or delete")
}

func deploymentsList(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("deployments list", deploymentsUsage)
	g.register(fs)
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}
	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	// The install's own answer under --json, the `toft deps` rule: a struct
	// re-encoded here dropped every field this binary does not name, and
	// invented a zero for one it named wrong.
	if g.json {
		var raw json.RawMessage
		if err := s.client.Do(http.MethodGet, "/api/v1/apps/"+app.ID+"/deployments", nil, &raw); err != nil {
			return err
		}
		// One deployment per line, as every --json list is (BB-035, fourth
		// 2026-11-03 list); each is the install's object, unchanged.
		var rows []json.RawMessage
		if err := json.Unmarshal(undoubleStepTimings(raw), &rows); err != nil {
			return fmt.Errorf("the install's deployment list did not parse: %w", err)
		}
		for _, row := range rows {
			var line bytes.Buffer
			if err := json.Compact(&line, row); err != nil {
				return err
			}
			fmt.Fprintln(e.out.Stdout, line.String())
		}
		return nil
	}
	rows, err := s.deployments(app.ID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		e.out.Say("%s has no deployments yet.", app.Slug)
		return nil
	}
	if e.out.Rich() {
		printDeploymentsRich(e, rows)
		return nil
	}
	for _, d := range rows {
		mark := " "
		if d.Serving {
			mark = "*"
		}
		from := orDefault(d.Branch, "—")
		if d.CommitSHA != "" {
			from += " · " + shortSHA(d.CommitSHA)
		}
		when := "—"
		if d.StartedAt != nil {
			when = roughAge(time.Since(*d.StartedAt)) + " ago"
		}
		e.out.Answer("%s %-36s  %-9s  %-24s  %s", mark, d.ID, d.Status, from, when)
	}
	e.out.Dim("* is serving.")
	return nil
}

// printDeploymentsRich is the history as a table: the id at the length
// `deployments delete` and `rollback` take back (they accept a prefix), a
// badge on the status, and the serving row said in words rather than starred.
func printDeploymentsRich(e *env, rows []deployment) {
	t := e.out.NewTable("", "deployment", "status", "from", "started").Flex(3)
	for _, d := range rows {
		mark := " "
		if d.Serving {
			mark = e.out.OK("▶")
		}
		from := orDefault(d.Branch, "—")
		if d.CommitSHA != "" {
			from += e.out.Dimmed(" · " + shortSHA(d.CommitSHA))
		}
		when := e.out.Dimmed("—")
		if d.StartedAt != nil {
			when = ui.Ago(*d.StartedAt)
		}
		id := ui.ShortID(d.ID)
		if d.Serving {
			id = e.out.Bold(id)
		} else {
			id = e.out.Dimmed(id)
		}
		t.Row(mark, id, e.out.RunBadge(d.Status), from, when)
	}
	t.Print()
	e.out.Dim("▶ is serving. An id prefix is enough for rollback and deployments delete.")
}

type deleteReleases struct {
	Deleted            int   `json:"deleted"`
	FreedDiskBytes     int64 `json:"freed_disk_bytes"`
	FreedArtifactBytes int64 `json:"freed_artifact_bytes"`
	// Kept is what a prune passed over, each with why and what would free it,
	// so "1 deleted" is never read as "only the serving release is left" (D8).
	Kept []keptRelease `json:"kept,omitempty"`
}

// keptRelease is one deployment a prune did not delete.
type keptRelease struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
	Remedy string `json:"remedy"`
}

func deploymentsDelete(e *env, verb string, args []string) error {
	var g globalFlags
	fs := newFlags("deployments "+verb, deploymentsUsage)
	g.register(fs)
	id, rest := takeOperand(args)
	if err := parseAfter(fs, rest, id); err != nil {
		return skipHelp(err)
	}
	if verb == "delete" {
		id = operandOr(id, fs)
		if id == "" {
			return usageCodef("missing_argument", "toft deployments delete needs a deployment id — `toft deployments list` names them")
		}
	} else if id != "" || fs.NArg() > 0 {
		return usageCodef("unexpected_argument", "toft deployments prune takes no id; `toft deployments delete <id>` removes one")
	}
	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}
	path := "/api/v1/apps/" + app.ID + "/deployments"
	question := fmt.Sprintf("delete every deployment of %s but the serving one — they stop being rollback targets", app.Slug)
	if verb == "delete" {
		if id, err = resolveDeploymentID(s, app.ID, id); err != nil {
			return err
		}
		path += "/" + id
		question = fmt.Sprintf("delete deployment %s of %s — it stops being a rollback target", id, app.Slug)
	}
	if err := confirm(e, &g, question); err != nil {
		return err
	}
	var got deleteReleases
	if err := s.client.Do(http.MethodDelete, path, nil, &got); err != nil {
		return err
	}
	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(got)
	}
	e.out.Answer("%d deleted, %s freed", got.Deleted, bytesWord(got.FreedDiskBytes+got.FreedArtifactBytes))
	for _, k := range got.Kept {
		line := "kept " + ui.ShortID(k.ID) + ": " + k.Reason
		if k.Remedy != "" {
			line += " — " + k.Remedy
		}
		e.out.Answer("%s", line)
	}
	return nil
}

// minDeploymentPrefix is the shortest prefix of a deployment id accepted in its
// place — long enough that a typo is unlikely to land on a real one.
const minDeploymentPrefix = 6

// resolveDeploymentID turns what was typed into a deployment id: the id itself,
// or a unique prefix of one of at least six characters — the length `toft
// deployments list` is read at. A whole id is passed through unlisted, so the
// install's own 404 answers for one that names nothing. No match, or several,
// is exit 2 naming the candidates.
func resolveDeploymentID(s *session, appID, given string) (string, error) {
	if len(given) == 36 && strings.Count(given, "-") == 4 {
		return given, nil
	}
	deployments, err := s.deployments(appID)
	if err != nil {
		return "", err
	}
	return matchDeploymentID(deployments, given)
}

func matchDeploymentID(deployments []deployment, given string) (string, error) {
	var matches []string
	for _, d := range deployments {
		if d.ID == given {
			return given, nil
		}
		if strings.HasPrefix(d.ID, given) {
			matches = append(matches, d.ID)
		}
	}
	if len(given) < minDeploymentPrefix {
		return "", usageCodef("invalid_id", "%q is too short to name a deployment — give at least %d "+
			"characters of the id; `toft deployments list` names them", given, minDeploymentPrefix)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", usageCodef("not_found", "no deployment of this app starts with %q — "+
			"`toft deployments list` names them", given)
	}
	return "", usageCodef("ambiguous_id", "%q is the start of %d deployments — give more of the id:\n    %s",
		given, len(matches), strings.Join(matches, "\n    "))
}

// bytesWord is a size the way a person says it.
func bytesWord(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// undoubleStepTimings is the one field the --json pass-through changes. The
// install stores `step_timings` as JSON text and sends it as a string, so a
// caller got JSON inside a JSON string and had to parse twice; here it is
// the object it encodes. Every other field, and the order of the rows, is the
// install's. Anything that does not have that shape is returned untouched.
func undoubleStepTimings(raw json.RawMessage) json.RawMessage {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return raw
	}
	changed := false
	for _, row := range rows {
		var text string
		if json.Unmarshal(row["step_timings"], &text) != nil || !json.Valid([]byte(text)) {
			continue
		}
		row["step_timings"] = json.RawMessage(text)
		changed = true
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(rows)
	if err != nil {
		return raw
	}
	return out
}
