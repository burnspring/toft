package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"toft/cli/internal/client"
)

const depsUsage = `toft deps — what this install's dependency policy says about the app

The packages a deployment installed, the advisories against them, and — when a
deploy has been refused — the versions that would fix it. This is the command
a refused deploy points at, and the answer it gives is the same one the app's
Dependencies tab shows.

Flags:
  --app <slug>        Which app
  --json              The install's whole answer, unaltered
`

// `toft deps` exists because a refusal that a caller cannot act on is a refusal
// that gets escalated.
//
// The gate already writes a good paragraph into the build log: the packages,
// the advisory ids, the fixed versions and the way out. That paragraph is the
// right answer for a person and the wrong one for a program, and the program is
// most of this client's traffic — an agent that gets `REFUSED: this install
// does not deploy packages with known high advisories` and no structure has
// nothing to bump.
//
// **It re-states nothing.** The install computes all of this for its own tab;
// this command is a rendering of that one response and, under `--json`, is that
// response byte for byte. A second shape of the same feature in this binary
// would be a second thing to keep true.

// dependencyState is the part of the install's answer this command renders.
// Findings ride as raw JSON so the fields this binary does not name still reach
// a `--json` caller and cannot drift.
type dependencyState struct {
	Mode      string `json:"mode"`
	Enforcing bool   `json:"enforcing"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	FeedAsOf  *time.Time `json:"feed_as_of,omitempty"`
	FeedStale bool       `json:"feed_stale"`
	HasData   bool       `json:"has_data"`

	Scanned    bool   `json:"scanned"`
	ScanReason string `json:"scan_reason,omitempty"`

	Serving *struct {
		DeploymentID string              `json:"deployment_id"`
		Findings     []dependencyFinding `json:"findings"`
		Counts       struct {
			Total   int `json:"total"`
			Malware int `json:"malware"`
			Counted int `json:"counted"`
			Waived  int `json:"waived"`
			Unknown int `json:"unknown"`
		} `json:"counts"`
		Ecosystems []string `json:"ecosystems,omitempty"`
		Inventory  []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"inventory"`
	} `json:"serving,omitempty"`

	Refused *struct {
		DeploymentID string              `json:"deployment_id"`
		At           *time.Time          `json:"at,omitempty"`
		Reason       string              `json:"reason,omitempty"`
		Findings     []dependencyFinding `json:"findings"`
	} `json:"refused,omitempty"`

	Waivers []struct {
		AdvisoryID string     `json:"advisory_id"`
		ExpiresAt  *time.Time `json:"expires_at,omitempty"`
		Reason     string     `json:"reason,omitempty"`
	} `json:"waivers"`
	CanWaive         bool `json:"can_waive"`
	WaiversAvailable bool `json:"waivers_available"`
	LockedByPolicy   bool `json:"locked_by_policy"`
}

// dependencyFinding is one advisory against one installed package.
type dependencyFinding struct {
	AdvisoryID string `json:"advisory_id"`
	Kind       string `json:"kind"`
	Severity   string `json:"severity"`
	Package    string `json:"package"`
	Version    string `json:"version"`
	FixedIn    string `json:"fixed_in,omitempty"`
	Path       string `json:"path,omitempty"`
	Direct     bool   `json:"direct"`
	WouldBlock bool   `json:"would_block"`
	Waived     bool   `json:"waived"`
}

func cmdDeps(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("deps", depsUsage)
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

	// Under --json the install's answer is passed through unaltered. Decoding
	// and re-encoding it would silently drop every field this binary is too
	// old to name, on a route whose whole value is that it is complete.
	var raw json.RawMessage
	if err := s.client.Do(http.MethodGet, "/api/v1/apps/"+app.ID+"/dependencies", nil, &raw); err != nil {
		var apiErr *client.Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
			// The tab is edit-gated on purpose: an exact package-and-version
			// manifest of a running internal tool is a target map. Said plainly
			// rather than as "forbidden", which reads as a broken credential.
			return fmt.Errorf("reading %s's dependencies takes edit access to it — "+
				"a viewer cannot see an app's package list", app.Slug)
		}
		return err
	}
	if g.json {
		_, err := fmt.Fprintln(e.out.Stdout, string(raw))
		return err
	}

	var state dependencyState
	if err := json.Unmarshal(raw, &state); err != nil {
		return err
	}
	cleanDependencyState(&state)
	printDependencyState(e, app, &state)
	return nil
}

// cleanDependencyState makes the decoded answer safe to draw on a terminal.
// Package names, versions and paths come from a deployed tree's own lockfile
// and reasons from tool output, passed through by the install; an escape in
// one could retitle the window or forge a row (security review 2026-11-15,
// CL-6). --json returns the exact bytes before this.
func cleanDependencyState(state *dependencyState) {
	clean := func(fs []dependencyFinding) {
		for i := range fs {
			f := &fs[i]
			f.Package, f.Version = listedName(f.Package), listedName(f.Version)
			f.AdvisoryID, f.FixedIn, f.Path = listedName(f.AdvisoryID), listedName(f.FixedIn), listedName(f.Path)
		}
	}
	state.Reason = plainText(state.Reason)
	if state.Serving != nil {
		clean(state.Serving.Findings)
		for i := range state.Serving.Ecosystems {
			state.Serving.Ecosystems[i] = listedName(state.Serving.Ecosystems[i])
		}
	}
	if state.Refused != nil {
		state.Refused.Reason = plainText(state.Refused.Reason)
		clean(state.Refused.Findings)
	}
	for i := range state.Waivers {
		state.Waivers[i].AdvisoryID = listedName(state.Waivers[i].AdvisoryID)
		state.Waivers[i].Reason = plainText(state.Waivers[i].Reason)
	}
}

// printDependencyState is the terminal rendering.
//
// Three sentences of this feature's honesty are load-bearing and are why this
// is not a table: **every number is scoped to the corpus it was checked
// against**, so the feed and its age lead; **recorded is not scanned**, so an
// unscanned release never renders as clean; and **a refusal is about a release
// that is not the one serving**, so the two are never merged into one list.
func printDependencyState(e *env, app *appSummary, state *dependencyState) {
	mode := state.Mode
	if state.Enforcing {
		mode += " · enforcing"
	} else if state.Mode != "" && state.Mode != "off" {
		mode += " · recording only"
	}
	e.out.Answer("%s  %s", e.out.Bold(app.Slug), e.out.Dimmed(mode))

	// Off or unlicensed is the whole answer, and it comes first: the install
	// sends no release and no package list then, so the corpus sentence —
	// "nothing below has been checked" — used to stand above a blank.
	if !state.Available {
		e.out.Answer("  %s", orDefault(state.Reason, "The dependency policy is not available on this install."))
		e.out.Answer("  %s", e.out.Dimmed("The install lists an app's packages here only while the policy is on."))
		return
	}

	// The corpus first, because it scopes everything under it.
	switch {
	case !state.HasData:
		e.out.Answer("  %s", e.out.Fail("No advisory data has ever loaded on this install, "+
			"so nothing below has been checked against anything."))
	case state.FeedAsOf != nil:
		line := "advisory data synced " + state.FeedAsOf.UTC().Format("2006-01-02 15:04 UTC")
		if state.FeedStale {
			line += " — older than this install's staleness threshold"
		}
		e.out.Answer("  %s", e.out.Dimmed(line))
	}

	if state.LockedByPolicy {
		e.out.Answer("")
		e.out.Answer("  %s", e.out.Fail("This app is locked by the dependency policy. "+
			"Unlocking it does not clear the finding, and the next pass locks it again."))
	}

	if state.Refused != nil {
		when := ""
		if state.Refused.At != nil {
			when = " · " + roughAge(nowSince(*state.Refused.At)) + " ago"
		}
		e.out.Answer("")
		// Named apart from what is serving, and first. After a blocked deploy
		// the app is still on the previous release while the new one was turned
		// away, and a reader who conflates them fixes the wrong package.
		e.out.Answer("  %s  %s", e.out.Fail(richGlyph(e, "✕ ")+"Refused"),
			e.out.Dimmed("deployment "+shortSHA(state.Refused.DeploymentID)+when))
		printFindings(e, state.Refused.Findings, true)
		if len(state.Refused.Findings) == 0 && state.Refused.Reason != "" {
			// Refused with no advisory: the scan could not list what the build
			// installs. The reason is the whole explanation.
			e.out.Answer("    %s", state.Refused.Reason)
		}
	}

	if state.Serving != nil {
		e.out.Answer("")
		e.out.Answer("  %s  %s", e.out.Bold(richGlyph(e, "● ")+"Serving"),
			e.out.Dimmed(servingSummary(state)))
		printFindings(e, state.Serving.Findings, false)
	} else if state.Refused == nil {
		e.out.Answer("")
		e.out.Answer("  Nothing is serving yet.")
	}

	if len(state.Waivers) > 0 {
		e.out.Answer("")
		e.out.Answer("  %s", e.out.Bold("Waivers"))
		for _, w := range state.Waivers {
			until := "no expiry recorded"
			if w.ExpiresAt != nil {
				until = "until " + w.ExpiresAt.UTC().Format("2006-01-02")
			}
			e.out.Answer("    %-24s %s  %s", w.AdvisoryID, until, e.out.Dimmed(w.Reason))
		}
	}

	if state.Refused != nil {
		e.out.Answer("")
		e.out.Answer("  %s", e.out.Dimmed(wayOut(state)))
	}
}

// richGlyph is a heading's glyph, drawn only on a terminal.
func richGlyph(e *env, glyph string) string {
	if e.out.Rich() {
		return glyph
	}
	return ""
}

// servingSummary is the line beside the serving release, and it is where
// "recorded is not scanned" has to hold: a release whose packages are in no
// ecosystem this install trusts the silence of has been *recorded*, and saying
// "no known advisories" about it would be the clean bill of health this whole
// feature refuses to write.
func servingSummary(state *dependencyState) string {
	parts := []string{"deployment " + shortSHA(state.Serving.DeploymentID)}
	if n := len(state.Serving.Inventory); n > 0 {
		eco := ""
		if len(state.Serving.Ecosystems) > 0 {
			eco = " (" + strings.Join(state.Serving.Ecosystems, ", ") + ")"
		}
		parts = append(parts, fmt.Sprintf("%d packages%s", n, eco))
	}
	switch {
	case !state.Scanned:
		parts = append(parts, "not scanned — "+scanReasonText(state.ScanReason)+
			", which is not a clean bill of health")
	case state.Serving.Counts.Total == 0:
		parts = append(parts, "no known advisories")
	default:
		parts = append(parts, fmt.Sprintf("%d known, %d at or above the threshold",
			state.Serving.Counts.Total, state.Serving.Counts.Counted))
	}
	return strings.Join(parts, "  ·  ")
}

// printFindings lists advisories, worst first. `blockingOnly` is the refusal's
// rendering: the findings that caused it are the whole content of that block,
// and the ones that did not are noise under a sentence about being stopped.
func printFindings(e *env, findings []dependencyFinding, blockingOnly bool) {
	rows := make([]dependencyFinding, 0, len(findings))
	for _, f := range findings {
		if blockingOnly && (!f.WouldBlock || f.Waived) {
			continue
		}
		rows = append(rows, f)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return severityRank(rows[i]) < severityRank(rows[j])
	})

	if e.out.Rich() {
		printFindingsRich(e, rows)
		return
	}

	width := 0
	for _, f := range rows {
		if n := len(f.Package + " " + f.Version); n > width {
			width = n
		}
	}
	for _, f := range rows {
		fix := "no fixed version published"
		if f.FixedIn != "" {
			fix = "fixed in " + f.FixedIn
		}
		label := severityLabel(f)
		if f.Waived {
			label += " (waived)"
		}
		e.out.Answer("    %-*s  %-22s %-20s %s", width, f.Package+" "+f.Version,
			f.AdvisoryID, label, e.out.Dimmed(fix))
		// The chain, and only when it is one. "Update lodash" is not actionable
		// when the app's own manifest has never named lodash.
		if f.Path != "" && !f.Direct {
			e.out.Answer("    %-*s  %s", width, "", e.out.Dimmed("via "+f.Path))
		}
	}
}

// printFindingsRich is the findings as a table under their heading: the
// severity badged, the fix green where there is one, and the chain in a column
// of its own only when some finding came in through one.
func printFindingsRich(e *env, rows []dependencyFinding) {
	if len(rows) == 0 {
		return
	}
	chains := false
	for _, f := range rows {
		if f.Path != "" && !f.Direct {
			chains = true
		}
	}
	head := []string{"    severity", "package", "advisory", "fix"}
	if chains {
		head = append(head, "via")
	}
	t := e.out.NewTable(head...)
	if chains {
		t.Flex(4)
	} else {
		t.Flex(2)
	}
	for _, f := range rows {
		sev := e.out.Badge(severityLabel(f))
		if f.Kind == "malware" {
			sev = e.out.Fail("✕ malicious package")
		}
		if f.Waived {
			sev = e.out.Dimmed("○ " + severityLabel(f) + " (waived)")
		}
		fix := e.out.Dimmed("no fix published")
		if f.FixedIn != "" {
			fix = e.out.OK("→ " + f.FixedIn)
		}
		row := []string{"    " + sev, e.out.Bold(f.Package) + " " + e.out.Dimmed(f.Version), f.AdvisoryID, fix}
		if chains {
			via := ""
			if f.Path != "" && !f.Direct {
				via = e.out.Dimmed(f.Path)
			}
			row = append(row, via)
		}
		t.Row(row...)
	}
	t.Print()
}

// severityRank orders a finding set the way every surface of this feature reads
// it: malware first, because there is no mode in which somebody should have to
// go looking for it, then by severity, and unscored last — last rather than
// first because it is the one bucket that says nothing about how bad this is.
func severityRank(f dependencyFinding) int {
	if f.Kind == "malware" {
		return 0
	}
	switch f.Severity {
	case "critical":
		return 1
	case "high":
		return 2
	case "moderate":
		return 3
	case "low":
		return 4
	}
	return 5
}

func severityLabel(f dependencyFinding) string {
	if f.Kind == "malware" {
		return "malicious package"
	}
	if f.Severity == "" || f.Severity == "unknown" {
		return "unscored"
	}
	return f.Severity
}

// wayOut is the actionable half, and it varies by what this reader can actually
// do. A rule that names an action the reader cannot take is how a refusal gets
// escalated to somebody's support queue.
func wayOut(state *dependencyState) string {
	if len(state.Refused.Findings) == 0 {
		// No advisory to upgrade past or waive (drills 2026-11-02: marimo,
		// refused because the scan could not resolve it, was told to get a
		// waiver). What fixes it is the tree.
		return "No advisory refused this deploy, so there is nothing to waive: the scan could not " +
			"list what the build installs. Change the tree as the reason says, and deploy again."
	}
	fixable := false
	for _, f := range state.Refused.Findings {
		if f.WouldBlock && !f.Waived && f.FixedIn != "" {
			fixable = true
		}
	}
	switch {
	case fixable:
		return "Upgrade to the fixed versions above and deploy again."
	case state.CanWaive:
		return "No fixed version is published. A waiver is per advisory and expires; " +
			"you can grant one on the app's Dependencies tab."
	case state.WaiversAvailable:
		return "No fixed version is published. An administrator can grant a waiver, " +
			"which is per advisory and expires."
	}
	return "No fixed version is published, and this install does not grant waivers. " +
		"The remaining route is to drop the package."
}

// scanReasonText is a scan_reason in words where the code alone would not say
// what to do; the others print as they are, which --json carries anyway.
func scanReasonText(reason string) string {
	switch reason {
	case "":
		return "no covered ecosystem"
	case "no_corpus":
		// Security review 2026-11-14, DG-5: listed, in a covered ecosystem, and
		// checked against nothing — an administrator's to fix, not a redeploy.
		return "no advisory data has loaded for its ecosystem on this install"
	}
	return reason
}
