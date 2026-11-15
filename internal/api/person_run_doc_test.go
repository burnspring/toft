package api

import (
	"encoding/json"
	"strings"
	"testing"

	"toft/internal/database"
)

// L7 (2026-11-04 black-box list): a person's run document carries rerun_of,
// scheduled_for and failure_reason as null when empty — present, as the app
// surface carries them — and a start's or a rerun's answer keeps
// deduplicated beside them.
func TestAPersonsRunDocumentCarriesItsEmptyFieldsAsNull(t *testing.T) {
	run := &database.JobRun{ID: "r1", JobName: "nightly", Status: "queued"}
	b, err := json.Marshal(struct {
		personRunDoc
		Deduplicated bool `json:"deduplicated"`
	}{personRun(run), false})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"rerun_of":null`, `"scheduled_for":null`, `"failure_reason":null`, `"deduplicated":false`, `"id":"r1"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	run.RerunOf, run.FailureReason = "r0", "exit 1"
	b, _ = json.Marshal(personRun(run))
	if strings.Count(string(b), `"rerun_of"`) != 1 || !strings.Contains(string(b), `"rerun_of":"r0"`) ||
		!strings.Contains(string(b), `"failure_reason":"exit 1"`) {
		t.Errorf("a filled run = %s", b)
	}
}
