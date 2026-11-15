package toft

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BB-077: Respond refuses a run Start did not return, writing nothing, as
// Python, Node and R do.
func TestRespondRefusesARunStartDidNotReturn(t *testing.T) {
	for _, run := range []JobRun{{}, {ID: "real"}, {Job: "probe"}} {
		w := httptest.NewRecorder()
		err := Jobs.Respond(w, httptest.NewRequest(http.MethodPost, "/x", nil), run)
		var e *Error
		if !errors.As(err, &e) || e.Code() != "bad_request" || e.Hint() == "" {
			t.Errorf("Respond(%+v) = %v, want a hinted bad_request", run, err)
		}
		if w.Header().Get("Location") != "" || w.Body.Len() != 0 {
			t.Errorf("Respond(%+v) wrote a response: %q", run, w.Body.String())
		}
	}
}

// BB-076: the run carries cpu_throttled_seconds, execution_mode and
// definition_id as the platform sends them.
func TestJobRunCarriesModeDefinitionAndThrottle(t *testing.T) {
	var run JobRun
	if err := json.Unmarshal([]byte(`{"id":"r","cpu_throttled_seconds":1.5,"execution_mode":"shared","definition_id":"d1"}`), &run); err != nil {
		t.Fatal(err)
	}
	if run.CPUThrottledSeconds == nil || *run.CPUThrottledSeconds != 1.5 || run.ExecutionMode != "shared" || run.DefinitionID != "d1" {
		t.Errorf("run = %+v", run)
	}
}

// BB-083: StoreIn on a volume's folder names StorageIn, because Storage on a
// name attached at two folders is ambiguous_mount.
func TestStoreInOnAVolumeFolderNamesStorageIn(t *testing.T) {
	t.Setenv(EnvStorage, `[{"name":"vol","kind":"volume","folder":"f1","path":"/d/f1","access":"full"},`+
		`{"name":"vol","kind":"volume","folder":"f2","path":"/d/f2","access":"full"}]`)
	_, err := StoreIn("vol", "f2")
	var e *Error
	if !errors.As(err, &e) || e.Code() != "not_a_store" || !strings.Contains(e.Hint(), `toft.StorageIn("vol", "f2")`) {
		t.Errorf("StoreIn = %v (hint %q)", err, hintOf(err))
	}
}

func hintOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Hint()
	}
	return ""
}
