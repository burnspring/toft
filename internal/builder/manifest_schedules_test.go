package builder

import (
	"errors"
	"strings"
	"testing"

	"toft/internal/database"
)

func parseDecl(t *testing.T, body string) (ManifestDecl, []string, error) {
	t.Helper()
	return ParseManifestAll(writeManifest(t, body))
}

func manifestCode(err error) string {
	var me ManifestError
	if errors.As(err, &me) {
		return me.Code
	}
	return ""
}

// The zone is its own key and is joined into the stored schedule, as a job's is.
func TestRestartAndRenderAreReadWithTheirZone(t *testing.T) {
	d, warnings, err := parseDecl(t, `
[restart]
schedule = "0 4 * * *"
timezone = "Europe/London"

[render]
schedule = "0 6 * * 1"
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for two tables that are read", warnings)
	}
	if d.Restart == nil || d.Restart.Schedule != "CRON_TZ=Europe/London 0 4 * * *" || d.Restart.NextRunAt == nil {
		t.Errorf("restart = %+v", d.Restart)
	}
	if d.Render == nil || d.Render.Schedule != "0 6 * * 1" {
		t.Errorf("render = %+v", d.Render)
	}
}

// Absent is nil, which is what removes a row at the flip.
func TestNoTableIsNoSchedule(t *testing.T) {
	d, _, err := parseDecl(t, "[jobs.a]\ncommand = \"x\"\n")
	if err != nil || d.Restart != nil || d.Render != nil {
		t.Errorf("= %+v, %v", d, err)
	}
}

// The judgements that need nothing but the table, D5 and D2 row by row.
func TestScheduleTablesRefuseWhatTheyHalfUnderstand(t *testing.T) {
	for _, c := range []struct {
		name, body, code, contains string
	}{
		{"enabled", "[restart]\nschedule = \"0 4 * * *\"\nenabled = false\n", ManifestRemovedKey, "Settings tab"},
		{"enabled on render", "[render]\nschedule = \"0 4 * * *\"\nenabled = true\n", ManifestRemovedKey, "[render]"},
		{"no schedule", "[restart]\ntimezone = \"UTC\"\n", "", "is required"},
		{"bad zone", "[restart]\nschedule = \"0 4 * * *\"\ntimezone = \"Mars/Base\"\n", "", "timezone"},
		{"prefix", "[render]\nschedule = \"CRON_TZ=Europe/London 0 4 * * *\"\n", "", "prefix"},
		{"bad cron", "[restart]\nschedule = \"every night\"\n", "", "cron"},
		{"every five minutes", "[restart]\nschedule = \"*/5 * * * *\"\n", ManifestScheduleTooFrequent, "once an hour"},
		{"two an hour apart is fine, under is not", "[render]\nschedule = \"0,30 6 * * 1\"\n", ManifestScheduleTooFrequent, ""},
	} {
		_, _, err := parseDecl(t, c.body)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if manifestCode(err) != c.code || !strings.Contains(err.Error(), c.contains) {
			t.Errorf("%s: code %q, %v", c.name, manifestCode(err), err)
		}
	}
}

// Hourly is the floor, and allowed.
func TestHourlyIsAllowed(t *testing.T) {
	if _, _, err := parseDecl(t, "[restart]\nschedule = \"0 * * * *\"\n"); err != nil {
		t.Errorf("hourly refused: %v", err)
	}
}

// A key under either table that nothing reads is warned about, by its path.
func TestAnUnknownKeyUnderAScheduleIsWarned(t *testing.T) {
	_, warnings, err := parseDecl(t, "[restart]\nschedule = \"0 4 * * *\"\ntimezon = \"Europe/London\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0] != "restart.timezon" {
		t.Errorf("warnings = %v", warnings)
	}
}

// What the release is decides what it may schedule — the release's own
// detection, through Rerenderable, the one list.
func TestSchedulesFitTheRelease(t *testing.T) {
	r := &database.ManifestSchedule{Schedule: "0 4 * * *"}
	for _, c := range []struct {
		framework       Framework
		restart, render bool
		code            string
	}{
		{FrameworkFlask, true, false, ""},
		{FrameworkBatchPython, true, false, ManifestRestartNoService},
		{FrameworkStatic, true, false, ManifestRestartChangesNothing},
		{FrameworkNotebook, true, false, ManifestRestartChangesNothing},
		{FrameworkNotebook, false, true, ""},
		{FrameworkQuarto, false, true, ""},
		{FrameworkMarimo, false, true, ManifestRenderNotADocument},
		{FrameworkFlask, false, true, ManifestRenderNotADocument},
	} {
		var d ManifestDecl
		if c.restart {
			d.Restart = r
		}
		if c.render {
			d.Render = r
		}
		err := checkSchedulesFit(string(c.framework), d)
		if got := manifestCode(err); got != c.code || (c.code == "") != (err == nil) {
			t.Errorf("%s restart=%v render=%v: %v", c.framework, c.restart, c.render, err)
		}
	}
}

// The refusal points a document at [render].
func TestARestartOnADocumentPointsAtRender(t *testing.T) {
	err := checkSchedulesFit(string(FrameworkRMarkdown), ManifestDecl{Restart: &database.ManifestSchedule{}})
	if err == nil || !strings.Contains(err.Error(), "declare [render]") || !strings.Contains(err.Error(), "[restart]") {
		t.Errorf("err = %v", err)
	}
}

// A schedule removed by a deploy is a coded warning, so warning_details has
// an entry for it rather than a sentence alone (BB-017).
func TestARemovedScheduleIsACodedWarning(t *testing.T) {
	got := removedScheduleWarnings(database.RemovedSchedules{Restart: true, Render: true, RenderRecipients: 2})
	if len(got) != 2 || got[0].Code != WarningScheduleRemoved || got[0].Table != "restart" ||
		got[1].Code != WarningScheduleRemoved || got[1].Table != "render" ||
		!strings.Contains(got[1].Message, "2 people") {
		t.Errorf("warnings = %+v", got)
	}
	if got := removedScheduleWarnings(database.RemovedSchedules{}); len(got) != 0 {
		t.Errorf("nothing removed = %+v", got)
	}
}
