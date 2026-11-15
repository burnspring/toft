package main

import (
	"encoding/json"
	"testing"
)

// The 2026-11-04 black-box list, fourth round.

// BB-007: go_work_dir_excluded has one shape wherever it is said — the
// install's {code, table, path, message}, the check's own beside it, and
// create's and deploy's — so a program reads the directory from path.
func TestGoWorkDirExcludedHasOneShape(t *testing.T) {
	want := func(where string, d map[string]any) {
		t.Helper()
		if d["code"] != "go_work_dir_excluded" || d["table"] != "go.work" || d["path"] != ".toft/sdk-go" {
			t.Errorf("%s: detail = %v, want code, table go.work and path .toft/sdk-go", where, d)
		}
	}
	var doc struct {
		Details []map[string]any `json:"warning_details"`
	}
	if err := json.Unmarshal(withBundleNotices(json.RawMessage(`{"ok":true}`), nil, nil, ".toft/sdk-go"), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Details) != 1 {
		t.Fatalf("check details = %v", doc.Details)
	}
	want("check", doc.Details[0])

	_, details := clientWarnings(&env{bundleWorkspaceLeftOut: []string{".toft/sdk-go"}})
	if len(details) != 1 {
		t.Fatalf("deploy details = %v", details)
	}
	want("create/deploy", details[0].(map[string]any))

	// The install's own, now carrying path, is not said a second time.
	raw := json.RawMessage(`{"ok":true,"warning_details":[{"code":"go_work_dir_excluded","table":"go.work",` +
		`"path":".toft/sdk-go","message":"go.work uses .toft/sdk-go, which this tree does not contain"}]}`)
	doc.Details = nil
	if err := json.Unmarshal(withBundleNotices(raw, nil, nil, ".toft/sdk-go"), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Details) != 1 {
		t.Errorf("details = %v, want the install's alone", doc.Details)
	}
}
