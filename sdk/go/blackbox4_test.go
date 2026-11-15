package toft

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BB-026 (fourth 2026-11-03 list): a *Run logged with %v, %+v, %#v or
// json.Marshal prints neither the job token nor the presigned output URL — fmt
// prints a struct's unexported fields, and the token is one.
func TestARunPrintsNoSecrets(t *testing.T) {
	run := &Run{ID: "r1", Job: "nightly", Trigger: "ui", TriggerRole: "viewer",
		OutputURI: "https://bucket/out?X-Amz-Credential=AKIA&X-Amz-Security-Token=tok",
		token:     "job-secret-token", base: "http://toft"}
	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, printed := range []string{
		fmt.Sprintf("%v", run), fmt.Sprintf("%+v", run), fmt.Sprintf("%#v", run),
		fmt.Sprintf("%v", *run), fmt.Sprint(run), string(encoded),
	} {
		if strings.Contains(printed, "job-secret-token") || strings.Contains(printed, "X-Amz") {
			t.Errorf("a printed run carries a secret: %s", printed)
		}
		if !strings.Contains(printed, "r1") {
			t.Errorf("a printed run does not name itself: %s", printed)
		}
	}
	if !strings.Contains(string(encoded), `"output_uri":"[redacted]"`) {
		t.Errorf("the encoded run does not say it has an output location: %s", encoded)
	}
}

// BB-030: Respond's 202 body and json.Marshal(run) carry rerun_of,
// scheduled_for, deadline, exit_code and failure_reason as null when empty.
func TestAnEncodedRunCarriesItsEmptyFieldsAsNull(t *testing.T) {
	encoded, err := json.Marshal(JobRun{ID: "r1", Job: "j", Status: RunQueued})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"rerun_of", "scheduled_for", "deadline", "exit_code", "failure_reason"} {
		value, ok := body[key]
		if !ok || value != nil {
			t.Errorf("%s: present %v, value %v — want present as null in %s", key, ok, value, encoded)
		}
	}
	if body["id"] != "r1" || body["deduplicated"] != false {
		t.Errorf("the run's own fields went missing: %s", encoded)
	}

	exit := 3
	encoded, _ = json.Marshal(JobRun{ID: "r2", RerunOf: "r1", FailureReason: "exit 3", ExitCode: &exit})
	json.Unmarshal(encoded, &body)
	if body["rerun_of"] != "r1" || body["failure_reason"] != "exit 3" || body["exit_code"] != float64(3) {
		t.Errorf("set fields did not survive: %s", encoded)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/start", nil)
	if err := Jobs.Respond(w, req, JobRun{ID: "r1", Status: RunQueued, StatusURL: "/apps/a/__toft/runs/r1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), `"rerun_of":null`) || !strings.Contains(w.Body.String(), `"failure_reason":null`) {
		t.Errorf("Respond's 202 body leaves the empty fields out: %s", w.Body.String())
	}
}

// BB-043: the value Store returns beside not_a_store answers not_a_store when
// used anyway, rather than not_mapped for a name that is mapped; a folder with
// a ".." segment is invalid_name; and a TOFT_STORAGE entry missing its kind or
// access is storage_env_invalid rather than a mount of no kind.
func TestStoreEdgeCasesAreCodedForWhatIsWrong(t *testing.T) {
	t.Setenv(EnvStorage, `[{"name":"vol","kind":"volume","path":"/data/vol","access":"full"}]`)
	mount, err := Store("vol")
	if e, ok := AsError(err); !ok || e.Code() != "not_a_store" {
		t.Fatalf("Store on a volume: %v", err)
	}
	_, err = mount.Upload(context.Background(), "x.csv", "")
	if e, ok := AsError(err); !ok || e.Code() != "not_a_store" {
		t.Errorf("Upload on what Store returned for a volume: %v — want not_a_store", err)
	}

	for _, folder := range []string{"..", "a/../b", "/abs", "a/"} {
		_, err := StorageIn("vol", folder)
		if e, ok := AsError(err); !ok || e.Code() != "invalid_name" || e.Hint() == "" {
			t.Errorf("StorageIn(vol, %q): %v — want invalid_name with a hint", folder, err)
		}
	}

	for _, env := range []string{`[{"name":"x"}]`, `[{"name":"x","kind":"store"}]`, `[{"kind":"store","access":"read"}]`} {
		t.Setenv(EnvStorage, env)
		_, err := Mounts()
		if e, ok := AsError(err); !ok || e.Code() != "storage_env_invalid" || e.Hint() == "" {
			t.Errorf("Mounts with %s: %v — want storage_env_invalid with a hint", env, err)
		}
	}
}

// BB-044: an encoded Mount has the documented field names, read_only and
// mounted included.
func TestAnEncodedMountHasTheDocumentedFields(t *testing.T) {
	encoded, err := json.Marshal(Mount{Name: "drop", Kind: "store", Access: "read", Folder: "in"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"drop","kind":"store","path":"","access":"read","folder":"in","read_only":true,"mounted":false}`
	if string(encoded) != want {
		t.Errorf("encoded Mount\n got %s\nwant %s", encoded, want)
	}
}

// BB-047: NaN in app data, or in a job's params, is bad_request off the
// platform too — before the off_platform the transport would raise.
func TestNaNIsBadRequestBeforeOffPlatform(t *testing.T) {
	for _, name := range []string{EnvAPIURL, EnvAppToken, "TOFT_JOB_TOKEN"} {
		t.Setenv(name, "")
	}
	SetTransport("", "")
	_, err := Data.Set(context.Background(), map[string]any{"x": math.NaN()})
	if e, ok := AsError(err); !ok || e.Code() != "bad_request" {
		t.Errorf("Data.Set with NaN off-platform: %v — want bad_request", err)
	}
	_, err = Jobs.Start(context.Background(), "j", StartOptions{Params: map[string]any{"x": math.Inf(1)}})
	if e, ok := AsError(err); !ok || e.Code() != "bad_request" {
		t.Errorf("Jobs.Start with Inf off-platform: %v — want bad_request", err)
	}
}

// BB-042: refusals the SDK makes itself carry a hint — a wait that ran out, a
// blank Slack text or email subject, a version conflict, an object store's
// refusal.
func TestSDKRefusalsCarryAHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/app/data"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPreconditionFailed)
			w.Write([]byte(`{"code":"version_conflict","error":"stale","version":4}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/app/job-runs/"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"r1","job":"j","status":"running"}`))
		case r.URL.Path == "/store":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv(EnvAPIURL, srv.URL)
	t.Setenv(EnvAppToken, "tok")
	SetTransport("", "")
	t.Cleanup(func() { SetTransport("", "") })

	hinted := func(label string, err error, code string) {
		t.Helper()
		e, ok := AsError(err)
		if !ok || e.Code() != code || e.Hint() == "" {
			t.Errorf("%s: %v — want %s with a hint", label, err, code)
		}
	}
	_, err := Notify.Slack(context.Background(), "   ")
	hinted("blank Slack text", err, "bad_request")
	_, err = Notify.Email(context.Background(), EmailMessage{To: []string{"a@example.com"}, Subject: " ", Text: "x"})
	hinted("blank email subject", err, "bad_request")
	_, err = Data.SetIf(context.Background(), map[string]any{"a": 1}, 3)
	hinted("version conflict", err, "version_conflict")
	err = (&Run{OutputURI: srv.URL + "/store"}).WriteOutput(context.Background(), []byte("x"))
	hinted("output unwritable", err, "output_unwritable")
	_, err = (JobRun{ID: "r1", Status: RunRunning}).Wait(context.Background(), WaitOptions{Timeout: 1, PollInterval: 1})
	hinted("wait timeout", err, "wait_timeout")
}
