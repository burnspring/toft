package builder

import (
	"testing"

	"toft/internal/database"
)

// TOFT-BB-001, second list: retention keeps the release a serving rebuild
// restored, because its row is where the serving release stands in history.
func TestRestoredByNamesTheReleaseARebuildRestored(t *testing.T) {
	rows := []database.Deployment{
		{ID: "R1", TriggeredBy: database.DeployTriggerRollback, RestoredFrom: "v1"},
		{ID: "M", TriggeredBy: database.DeployTriggerMove, RestoredFrom: "v2"},
		{ID: "P", TriggeredBy: database.DeployTriggerRollback, RestoredFrom: "v2", SourceDeployID: "v2"},
		{ID: "v2"},
		{ID: "v1"},
	}
	for serving, want := range map[string]string{"R1": "v1", "M": "v2", "P": "", "v2": "", "gone": ""} {
		if got := restoredBy(rows, serving); got != want {
			t.Errorf("restoredBy(%s) = %q, want %q", serving, got, want)
		}
	}
}
