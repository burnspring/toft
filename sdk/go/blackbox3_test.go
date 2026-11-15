package toft

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BB-008 (third 2026-11-03 list): an object store's refusal of a run's output
// is coded output_unwritable — not retryable for a 403, retryable for a 5xx —
// and only a store that cannot be reached is uncoded.
func TestWriteOutputRefusalsAreCoded(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/forbidden":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer store.Close()
	for _, tc := range []struct {
		uri, code string
		retryable bool
	}{
		{store.URL + "/forbidden", "output_unwritable", false},
		{store.URL + "/down", "output_unwritable", true},
		{"http://127.0.0.1:1/out", "", true},
	} {
		err := (&Run{OutputURI: tc.uri}).WriteOutput(context.Background(), []byte("x"))
		e, ok := AsError(err)
		if !ok || e.Code() != tc.code || e.Retryable() != tc.retryable {
			t.Errorf("WriteOutput to %s: %v — want code %q, retryable %v", tc.uri, err, tc.code, tc.retryable)
		}
	}
}

// BB-008: the same for reading an output back and for a signed store request.
func TestStoreFetchRefusalsAreCoded(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer store.Close()
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, store.URL+"/obj", http.StatusFound)
	})
	_, err := JobRun{ID: "r1"}.Output(context.Background())
	refusedLocally(t, "Output from a store answering 403", err, "output_unreadable")
	if e, _ := AsError(err); e == nil || e.Status != http.StatusForbidden {
		t.Errorf("Output: %v — want the store's 403 beside the code", err)
	}
	_, err = sendSigned(context.Background(), signedRequest{Method: http.MethodGet, URL: store.URL + "/obj"}, nil)
	refusedLocally(t, "a signed request the store refused", err, "store_refused")
}

// BB-022: an output written empty is not JSON, and not "no output" either.
func TestOutputJSONOnAnEmptyOutputIsNotJSON(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	var v any
	ok, err := JobRun{ID: "r_empty"}.OutputJSON(context.Background(), &v)
	if ok { // BB-056 (fifth list): false beside the error — v was not filled
		t.Error("OutputJSON reported true for an empty output")
	}
	refusedLocally(t, "OutputJSON(empty)", err, "not_json")
}

// BB-022: Respond's 202 body always carries deduplicated, and Mounts() with
// nothing mapped encodes as [], not null.
func TestRespondAlwaysCarriesDeduplicatedAndMountsIsAList(t *testing.T) {
	rec := httptest.NewRecorder()
	Jobs.Respond(rec, httptest.NewRequest(http.MethodPost, "/x", nil), JobRun{ID: "r1", StatusURL: "/s"})
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got, ok := body["deduplicated"]; !ok || got != false {
		t.Errorf("Respond body deduplicated = %v (present %v), want false", got, ok)
	}

	t.Setenv(EnvStorage, "")
	mounts, err := Mounts()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(mounts)
	if string(encoded) != "[]" {
		t.Errorf("Mounts() with nothing mapped encodes as %s, want []", encoded)
	}
}

// BB-024: a word that is not a role is bad_request, not a silent false.
func TestAtLeastRefusesAWordThatIsNotARole(t *testing.T) {
	for _, role := range []string{"superuser", "", "Editor"} {
		func() {
			defer func() {
				err, _ := recover().(error)
				if e, ok := AsError(err); !ok || e.Code() != "bad_request" || !strings.Contains(e.Error(), "not a role") {
					t.Errorf("AtLeast(%q) panicked with %v, want a bad_request *Error", role, err)
				}
			}()
			Caller{Role: RoleOwner}.AtLeast(role)
		}()
	}
	for _, role := range []string{RoleSystem, RoleApp, RoleUnknown} {
		if (Caller{Role: RoleOwner}).AtLeast(role) {
			t.Errorf("AtLeast(%q) = true", role)
		}
	}
}

// D-13 (third 2026-11-03 list): off the platform, Storage and Store are
// off_platform, as Node's are, rather than not_mapped.
func TestStorageOffPlatformIsOffPlatform(t *testing.T) {
	for _, v := range []string{EnvStorage, "TOFT_API_URL", "TOFT_APP_ID", "TOFT_RUN_ID", "TOFT_SCRATCH_DIR"} {
		t.Setenv(v, "")
	}
	_, err := Storage("reports")
	refusedLocally(t, "Storage off-platform", err, "off_platform")
	_, err = Store("reports")
	refusedLocally(t, "Store off-platform", err, "off_platform")
}
