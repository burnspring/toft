package builder

import (
	"fmt"
	"strings"
	"time"

	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/schedule"
)

// `[restart]` and `[render]` — the app's other two schedules, declared beside
// its jobs (ADR §144).
//
//	[restart]
//	schedule = "0 4 * * *"
//	timezone = "Europe/London"   # optional; absent is UTC
//
//	[render]
//	schedule = "0 6 * * 1"
//
// One of each per app, so neither is named: there is one serving workload to
// restart and one document to re-render. The keys are a job's spelling of the
// same two facts, with the job's rules: the zone is its own key, a `CRON_TZ=`
// prefix beside it is refused, and `enabled` is refused rather than warned
// about — a key that reads as an off switch and is merely warned about is a
// schedule its author believes is off.
//
// **The file is the only writer** (ADR §117's rule for jobs, extended). They
// version and roll back with the code they describe: an app that needs a
// nightly bounce because of a leak in this release stops needing it in the
// release that fixes the leak, and a report's timetable changes in the same
// commit as the report. Judged at detect (checkManifest), recorded at the flip
// (applyManifestSchedules), like `[jobs]` (ADR §121).

// manifestSchedule is the shape both tables share.
type manifestSchedule struct {
	Schedule string `toml:"schedule"`
	Timezone string `toml:"timezone"`
	// Enabled is read only to be refused.
	Enabled any `toml:"enabled"`
}

// ScheduleFloor is the shortest interval either table may fire at, and
// scheduleFloorWindow how far ahead it is checked: eight days, so a weekly
// expression is checked across its own boundary.
const (
	ScheduleFloor       = time.Hour
	scheduleFloorWindow = 8 * 24 * time.Hour
)

// The refusal codes D5 adds, beside removed_key.
const (
	ManifestRestartNoService      = "restart_no_service"
	ManifestRestartChangesNothing = "restart_changes_nothing"
	ManifestRenderNotADocument    = "render_not_a_document"
	ManifestScheduleTooFrequent   = "schedule_too_frequent"
)

// ManifestDecl is everything ParseManifestAll reads out of a release that the
// deploy records at the flip: the jobs, and the two one-per-app schedules (nil
// when the table is absent).
type ManifestDecl struct {
	Jobs    []database.JobDefinition
	Restart *database.ManifestSchedule
	Render  *database.ManifestSchedule
	// Storage is `[[storage]]`; StorageDeclared says the table is present at
	// all, which an empty list cannot.
	Storage         []StorageEntry
	StorageDeclared bool
	// plan is checkManifest's storage plan, carried to the start and the flip.
	plan *StoragePlan
}

// parseManifestSchedule judges one table on its own — the parts that do not
// depend on what the release is. checkSchedulesFit does the rest.
func parseManifestSchedule(table string, s *manifestSchedule, tolerateRemoved bool, warnings *[]string) (*database.ManifestSchedule, error) {
	if s == nil {
		return nil, nil
	}
	if s.Enabled != nil && tolerateRemoved {
		*warnings = append(*warnings, table+".enabled")
	} else if s.Enabled != nil {
		return nil, ManifestError{Table: table, Field: "enabled", Code: ManifestRemovedKey,
			Msg: "is not read", Hint: "remove enabled from [" + table + "]: pause it on the app's Settings tab, " +
				"or remove the table to remove the schedule"}
	}
	expr := strings.TrimSpace(s.Schedule)
	if expr == "" {
		return nil, ManifestError{Table: table, Field: "schedule", Msg: "is required",
			Hint: fmt.Sprintf(`add schedule = "0 4 * * *" to [%s]`, table)}
	}
	zone := strings.TrimSpace(s.Timezone)
	if zone != "" {
		if err := schedule.ValidateZone(zone); err != nil {
			return nil, ManifestError{Table: table, Field: "timezone", Msg: err.Error()}
		}
	}
	if z, rest := schedule.Split(expr); z != "" {
		return nil, ManifestError{Table: table, Field: "schedule",
			Msg:  "names its zone with a prefix; put the zone in `timezone` instead",
			Hint: zonePrefixHint(z, rest)}
	}
	joined := schedule.Join(zone, expr)
	if err := schedule.Validate(joined); err != nil {
		return nil, ManifestError{Table: table, Field: "schedule",
			Msg: "is not a valid 5-field cron expression: " + err.Error()}
	}
	gap, err := schedule.MinGap(joined, scheduleFloorWindow, ScheduleFloor)
	if err != nil {
		return nil, ManifestError{Table: table, Field: "schedule", Msg: err.Error()}
	}
	if gap < ScheduleFloor {
		return nil, ManifestError{Table: table, Field: "schedule", Code: ManifestScheduleTooFrequent,
			Msg:  fmt.Sprintf("fires %s apart, and a [%s] schedule may fire at most once an hour", gap, table),
			Hint: `fire at most hourly, like schedule = "0 4 * * *" for once a day`}
	}
	out := &database.ManifestSchedule{Schedule: joined}
	// From now, for the reason a job's is: the value for a new or changed
	// schedule only, since ReconcileManifestSchedules keeps the stored one of
	// a live schedule the deploy left alone.
	if at, err := schedule.Next(joined, time.Now().UTC()); err == nil {
		out.NextRunAt = &at
	}
	return out, nil
}

// checkSchedulesFit refuses a schedule the release cannot have, asking the
// release's own detection — the same function the API, the scheduler and the
// SPA ask (Rerenderable).
func checkSchedulesFit(framework string, d ManifestDecl) error {
	if d.Restart != nil {
		switch {
		case IsBatchFramework(framework):
			return ManifestError{Table: "restart", Code: ManifestRestartNoService,
				Msg:  "is for a serving app, and a batch app has no process to restart",
				Hint: "schedule its work as a job"}
		case framework == string(FrameworkStatic):
			return ManifestError{Table: "restart", Code: ManifestRestartChangesNothing,
				Msg:  "would change nothing",
				Hint: "a static app's files are served as they are"}
		case Rerenderable(framework):
			return ManifestError{Table: "restart", Code: ManifestRestartChangesNothing,
				Msg:  "would serve the same pages again",
				Hint: "to re-run the document, declare [render]"}
		}
	}
	if d.Render != nil && !Rerenderable(framework) {
		if framework == string(FrameworkMarimo) {
			return ManifestError{Table: "render", Code: ManifestRenderNotADocument,
				Msg: "has nothing to re-render: a marimo app executes its notebook for each viewer, " +
					"so it is already showing whatever the code does now",
				Hint: "remove [render]"}
		}
		// Every refusal here names its next step (drills 2026-10-30: this one
		// was the one answering hint null).
		return ManifestError{Table: "render", Code: ManifestRenderNotADocument,
			Msg:  "is for a document — a notebook, R Markdown or Quarto — and this release is not one",
			Hint: "remove [render]; to restart a serving app on a timetable, declare [restart]"}
	}
	return nil
}

// applyManifestSchedules records the schedules checkManifest judged, once the
// release that declares them is the one serving — beside applyManifestJobs, on
// the same terms.
//
// **A schedule the deploy removes is announced** (D15): an app event to the
// owners and a deploy warning a `--json` caller reads. A re-render's own flip
// runs this too, which is how a `[render]` deleted by an edit nobody noticed
// stops — and why that must be a sentence rather than a gap.
func (b *Builder) applyManifestSchedules(app *database.App, deploy *database.Deployment, d ManifestDecl, keepPauses bool, appendLog func(string)) error {
	removed, err := b.db.ReconcileManifestSchedulesKeeping(app.ID, d.Restart, d.Render, keepPauses)
	if err != nil {
		return fmt.Errorf("recording declared schedules: %w", err)
	}
	if d.Restart != nil {
		appendLog("==> Restart schedule: " + describeManifestSchedule(d.Restart))
	}
	if d.Render != nil {
		appendLog("==> Re-render schedule: " + describeManifestSchedule(d.Render))
	}
	details := removedScheduleWarnings(removed)
	sentences := make([]string, 0, len(details))
	for _, w := range details {
		appendLog("==> Warning: " + w.Message)
		sentences = append(sentences, w.Message)
	}
	b.addDeployWarningDetails(deploy, details)
	if len(sentences) > 0 && b.opts.Events != nil {
		b.opts.Events.Record(events.Event{
			AppID:    app.ID,
			Kind:     events.KindScheduleRemoved,
			Severity: database.EventSeverityWarning,
			Source:   database.EventSourceBuilder,
			Summary:  strings.Join(sentences, " "),
			Detail:   "Declare the table in toft.toml again and deploy to bring it back.",
			// One per deploy: two removals in two deploys are two events.
			Fingerprint: deploy.ID,
		})
	}
	return nil
}

// describeManifestSchedule is the build log's line for a schedule: the
// expression, its zone, and the next firing.
func describeManifestSchedule(s *database.ManifestSchedule) string {
	zone, expr := schedule.Split(s.Schedule)
	if zone == "" {
		zone = "UTC"
	}
	out := fmt.Sprintf("%s (%s)", expr, zone)
	if s.NextRunAt != nil {
		out += ", next " + s.NextRunAt.UTC().Format(time.RFC3339)
	}
	return out
}

// zonePrefixHint is the two lines a zone-prefixed schedule becomes.
func zonePrefixHint(zone, expr string) string {
	return fmt.Sprintf("write timezone = %q and schedule = %q", zone, strings.TrimSpace(expr))
}

// removedScheduleWarnings is a schedule the previous release declared and
// this one does not, as coded warnings — schedule_removed, Table naming the
// table — so warning_details carries it (BB-017).
func removedScheduleWarnings(removed database.RemovedSchedules) []Warning {
	var details []Warning
	if removed.Restart {
		s := "The scheduled restart was removed: this release's toft.toml declares no [restart]."
		details = append(details, Warning{Code: WarningScheduleRemoved, Table: "restart", Message: s})
	}
	if removed.Render {
		s := "The re-render schedule was removed: this release's toft.toml declares no [render]."
		switch removed.RenderRecipients {
		case 0:
		case 1:
			s += " The 1 person on its recipient list will no longer receive it."
		default:
			s += fmt.Sprintf(" The %d people on its recipient list will no longer receive it.", removed.RenderRecipients)
		}
		details = append(details, Warning{Code: WarningScheduleRemoved, Table: "render", Message: s})
	}
	return details
}
