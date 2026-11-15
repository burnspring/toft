package autodeploy

import (
	"testing"

	"toft/internal/database"
)

// The whole reason `sleeping` is a status of its own rather than a reuse of
// `stopped`: an idle app must still pick up a git push. Reusing `stopped` would
// have silently switched auto-deploy off for every app the reaper touched.
func TestDeploysOnPush_SleepingAppStillDeploys(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{database.AppStatusRunning, true},
		{database.AppStatusSleeping, true},
		{database.AppStatusCrashed, true},
		{database.AppStatusFailed, true},
		{database.AppStatusBuilding, true},
		{database.AppStatusStopped, false},
	} {
		if got := deploysOnPush(tc.status); got != tc.want {
			t.Errorf("deploysOnPush(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}
