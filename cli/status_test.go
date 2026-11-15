package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// **`since` is when the release became current, and the build has its own
// line.** After a rollback the serving row's finished_at is the day it was
// built, and `toft status` used to print it as "since"; after a stop it sat
// beside the word `stopped`. Each clock now has a label that is true of it.
func TestStatusLabelsEachClock(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprintf(w, `{"app":{"id":"app-1","slug":"demo","status":"stopped",`+
				`"serving_since":%q,"status_changed_at":%q}}`, at(5*time.Minute), at(2*time.Minute))
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprintf(w, `[{"id":"dep-1","status":"success","serving":true,"current":true,`+
				`"branch":"main","finished_at":%q}]`, at(50*time.Minute))
		case r.URL.Path == "/api/v1/config":
			fmt.Fprint(w, `{"apps_base_url":""}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	})
	if err := cmdStatus(e, []string{"--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"stopped for 2 minutes",
		"since       5 minutes ago",
		"built       50 minutes ago",
		"release     main",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// An install older than the two clocks sends neither, and the build time is
// never passed off as the serving one.
func TestStatusWithoutClocksSaysBuilt(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("running"))
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprintf(w, `[{"id":"dep-1","status":"success","serving":true,"branch":"main","finished_at":%q}]`,
				time.Now().UTC().Add(-3*time.Hour).Format(time.RFC3339))
		case r.URL.Path == "/api/v1/config":
			fmt.Fprint(w, `{"apps_base_url":""}`)
		}
	})
	if err := cmdStatus(e, []string{"--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "since") {
		t.Errorf("a `since` line with no serving_since to read:\n%s", out)
	}
	if !strings.Contains(out, "built       3 hours ago") || !strings.Contains(out, "serving     main") {
		t.Errorf("want the build's own line and `serving`:\n%s", out)
	}
}

// A locked app is stopped *because* it is locked, and the gateway's 503 sends
// people to `toft status` to read why — so the lock is said, not left as
// "stopped", and `--json` carries it.
func TestStatusSaysTheAppIsLocked(t *testing.T) {
	at := time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339)
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprintf(w, `{"app":{"id":"app-1","slug":"demo","status":"stopped",`+
				`"locked_at":%q,"locked_source":"admin","locked_message":"retired in Q3"}}`, at)
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprint(w, `[]`)
		case r.URL.Path == "/api/v1/config":
			fmt.Fprint(w, `{"apps_base_url":""}`)
		}
	}
	e, buf := fakeInstall(t, handler)
	if err := cmdStatus(e, []string{"--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"locked      by an administrator, 3 minutes ago", "retired in Q3", "until an administrator unlocks it"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	e, buf = fakeInstall(t, handler)
	if err := cmdStatus(e, []string{"--app", "demo", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"lock":{"at":`) || !strings.Contains(buf.String(), `"source":"admin"`) {
		t.Errorf("--json has no lock: %s", buf.String())
	}
}

// --mine is the install's own `mine`: an app a group of yours owns is yours,
// and --json carries owner_group_id beside owner_id (drills 2026-10-28 evening
// #3 — comparing owner_id with your id dropped every group-owned app).
func TestAppsMineIncludesAGroupsApps(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":"a","slug":"teams","owner_id":"","owner_group_id":"g1","mine":true},`+
			`{"id":"b","slug":"theirs","owner_id":"u2","mine":false}]`)
	})
	if err := cmdApps(e, []string{"--mine", "--json"}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, `"slug":"teams"`) || strings.Contains(got, `"slug":"theirs"`) ||
		!strings.Contains(got, `"owner_group_id":"g1"`) {
		t.Errorf("toft apps --mine --json = %s", got)
	}
}

// A deploy that failed in the background while the previous release serves is
// named, with its reason and the command that explains it — in the line and
// under --json — and a failure that is not the newest is not (docs/bugs
// 2026-10-28 #5).
func TestStatusNamesANewerFailedDeploy(t *testing.T) {
	for _, tc := range []struct {
		name, deployments string
		want              bool
	}{
		{"newest failed", `[{"id":"dep-new-1234567","status":"failed","failure_reason":"not_ready"},` +
			`{"id":"dep-old","status":"success","serving":true,"current":true,"branch":"main"}]`, true},
		{"an older failure", `[{"id":"dep-old","status":"success","serving":true,"current":true,"branch":"main"},` +
			`{"id":"dep-new-1234567","status":"failed","failure_reason":"not_ready"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
					fmt.Fprint(w, `{"app":{"id":"app-1","slug":"demo","status":"running"}}`)
				case strings.HasSuffix(r.URL.Path, "/deployments"):
					fmt.Fprint(w, tc.deployments)
				default:
					fmt.Fprint(w, `{}`)
				}
			}
			e, buf := fakeInstall(t, handler)
			if err := cmdStatus(e, []string{"--app", "demo"}); err != nil {
				t.Fatal(err)
			}
			named := strings.Contains(buf.String(), "failed — not_ready") &&
				strings.Contains(buf.String(), "toft logs --deploy")
			if named != tc.want {
				t.Errorf("named the failure = %v, want %v:\n%s", named, tc.want, buf.String())
			}

			e, buf = fakeInstall(t, handler)
			if err := cmdStatus(e, []string{"--app", "demo", "--json"}); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(buf.String(), `"latest"`); got != tc.want {
				t.Errorf("--json latest = %v, want %v: %s", got, tc.want, buf.String())
			}
		})
	}
}

// An app whose every deploy failed has no "release before it": status says
// nothing serves, and does not also claim an earlier release is serving
// (drills on trytoft 2026-11-02, marimo refused by the dependency policy).
func TestStatusOfAnAppThatNeverDeployed(t *testing.T) {
	e, buf := fakeInstall(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/by-slug/demo"):
			fmt.Fprint(w, appRow("failed"))
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprintf(w, `[{"id":"dep-1","status":"failed","failure_reason":"dependency_blocked","finished_at":%q}]`,
				time.Now().UTC().Add(-time.Minute).Format(time.RFC3339))
		case r.URL.Path == "/api/v1/config":
			fmt.Fprint(w, `{"apps_base_url":""}`)
		}
	})
	if err := cmdStatus(e, []string{"--app", "demo"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "release before it") {
		t.Errorf("status names a release before it on an app that never deployed:\n%s", out)
	}
	if !strings.Contains(out, "nothing deployed yet") || !strings.Contains(out, "nothing serves yet") {
		t.Errorf("want nothing serving, said once each way:\n%s", out)
	}
}

// A refusal with no advisory — the scan could not resolve the tree — is not
// sent to the waiver route, and its reason is printed.
func TestDepsRefusalWithoutFindingsPointsAtTheTree(t *testing.T) {
	var state dependencyState
	if err := json.Unmarshal([]byte(`{"available":true,"refused":{"deployment_id":"dep-1",`+
		`"reason":"REFUSED: pyyaml publishes no wheel for this Python","findings":[]},"can_waive":true}`), &state); err != nil {
		t.Fatal(err)
	}
	if got := wayOut(&state); strings.Contains(got, "waiver is per advisory") || !strings.Contains(got, "nothing to waive") {
		t.Errorf("wayOut = %q", got)
	}
}

// The feed's time is said in UTC because it is labelled UTC; the install
// answers it in its own zone (security review 2026-11-15, CL-8).
func TestDepsFeedTimeIsUTC(t *testing.T) {
	var state dependencyState
	if err := json.Unmarshal([]byte(`{"available":true,"has_data":true,"feed_as_of":"2026-11-15T09:00:00-05:00"}`), &state); err != nil {
		t.Fatal(err)
	}
	e, buf := testEnv()
	printDependencyState(e, &appSummary{Slug: "demo"}, &state)
	if !strings.Contains(buf.String(), "2026-11-15 14:00 UTC") {
		t.Errorf("printed %q, want 14:00 UTC", buf.String())
	}
}
