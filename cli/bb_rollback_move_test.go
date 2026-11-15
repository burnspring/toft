package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TOFT-BB-001: a move (rename, hostname on or off) rebuilds the serving
// release as the newest row, and records which release it rebuilt. A rollback
// after it goes back from that release, never forward to the newest.
func TestRollbackAfterAMoveGoesBackFromTheRebuiltRelease(t *testing.T) {
	// v4 < v5 < v6; rolled back to v5, then renamed: M rebuilds v5.
	rows := []deployment{
		{ID: "M", Status: "success", TriggeredBy: "move", RestoredFrom: "v5", Serving: true},
		{ID: "v6", Status: "success"},
		{ID: "v5", Status: "success"},
		{ID: "v4", Status: "success"},
	}
	if got, err := rollbackTarget(rows, ""); err != nil || got.ID != "v4" {
		t.Errorf("rollback after a move of v5 = %v, %v; want v4", got, err)
	}
	if !newerThanServing(rows, &rows[1]) {
		t.Error("v6 is newer than the release the move rebuilt")
	}
	// The hostname case: the move rebuilt v1, the oldest — nothing older.
	rows = []deployment{
		{ID: "M", Status: "success", TriggeredBy: "move", RestoredFrom: "v1", Serving: true},
		{ID: "v3", Status: "success"},
		{ID: "v2", Status: "success"},
		{ID: "v1", Status: "success"},
	}
	if got, err := rollbackTarget(rows, ""); refusalCode(err) != "nothing_older" || got != nil {
		t.Errorf("rollback after a move of v1 = %v, %v; want nothing_older", got, err)
	}
}

// TOFT-BB-003: a rollback the install rebuilds names the new deployment, which
// is the one serving, not the release it restored.
func TestARebuiltRollbackNamesTheNewDeployment(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(p, "/rollback"):
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":"new-9","source_deploy_id":"dep-0","restored_from":"dep-0","rebuild":true}`)
		case strings.HasSuffix(p, "/progress"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"new-9\",\"status\":\"success\","+
				"\"phase\":\"deploy\",\"step\":\"flip\"}\n\n")
		case strings.HasSuffix(p, "/deployments"):
			fmt.Fprint(w, `[{"id":"dep-1","status":"success","serving":true,"current":true,"branch":"main"},`+
				`{"id":"dep-0","status":"success","branch":"main"}]`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	if err := cmdRollback(e, []string{"--app", "demo", "--yes", "--json"}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var result map[string]any
	json.Unmarshal([]byte(lines[len(lines)-1]), &result)
	if result["deployment_id"] != "new-9" {
		t.Errorf("--json names %v, want the rebuilt new-9:\n%s", result["deployment_id"], buf.String())
	}
}

// TOFT-BB-001, second list: the release a rebuilt rollback restored was
// pruned, so its row is gone. The rollback after it went forward to the move
// row of the release just left; it refuses instead.
func TestRollbackFromARebuildWhoseOriginalWasPrunedDoesNotGoForward(t *testing.T) {
	rows := []deployment{
		{ID: "R1", Status: "success", TriggeredBy: "rollback", RestoredFrom: "v1", Serving: true, Current: true},
		{ID: "M", Status: "success", TriggeredBy: "move", RestoredFrom: "v2"},
		{ID: "v2", Status: "success"},
	}
	if got, err := rollbackTarget(rows, ""); refusalCode(err) != "nothing_older" || got != nil {
		t.Errorf("rollback with v1's row pruned = %v, %v; want nothing_older", got, err)
	}
	// With the row kept, as the install now keeps it, it goes back from v1.
	rows = append(rows, deployment{ID: "v1", Status: "success"}, deployment{ID: "v0", Status: "success"})
	if got, err := rollbackTarget(rows, ""); err != nil || got.ID != "v0" {
		t.Errorf("rollback with v1's row kept = %v, %v; want v0", got, err)
	}
}
