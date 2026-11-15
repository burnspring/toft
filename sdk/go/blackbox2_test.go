package toft

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// BB-002 (second 2026-11-03 report): output that is not JSON is a coded
// not_json, not a bare *json.SyntaxError.
func TestOutputJSONOnBytesThatAreNotJSONIsNotJSON(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	var v any
	ok, err := JobRun{ID: "run_nj"}.OutputJSON(context.Background(), &v)
	if ok { // BB-056 (fifth list): false beside the error — v was not filled
		t.Error("OutputJSON reported true for output it could not decode")
	}
	refusedLocally(t, "OutputJSON(not json)", err, "not_json")
	if e, _ := AsError(err); e != nil && e.Hint() != "read the bytes with Output" {
		t.Errorf("hint = %q", e.Hint())
	}
}

// BB-003 and BB-004: an empty kind or capability is refused before any request.
func TestEmptyKindAndCapabilityAreRefusedLocally(t *testing.T) {
	called := false
	platform(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	_, err := AccessToken(context.Background(), "")
	refusedLocally(t, `AccessToken("")`, err, "bad_request")
	ok, err := App.Can(context.Background(), " ")
	refusedLocally(t, `Can(" ")`, err, "bad_request")
	if ok {
		t.Error(`Can(" ") answered true`)
	}
	if called {
		t.Error("a request reached the platform")
	}
}

// BB-019: off-platform scratch is a private toft-scratch-<uid>/<app>, never the
// shared temp directory.
func TestOffPlatformScratchIsPrivate(t *testing.T) {
	t.Setenv(EnvScratchDir, "")
	t.Setenv("TMPDIR", t.TempDir())
	localScratch.Lock()
	localScratch.path = ""
	localScratch.Unlock()
	dir, err := ScratchDir("u")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(dir)
	if !strings.HasPrefix(filepath.Base(filepath.Dir(root)), "toft-scratch-") {
		t.Errorf("ScratchDir(u) = %s, want it under toft-scratch-<uid>/<app>", dir)
	}
	for _, d := range []string{dir, root, filepath.Dir(root)} {
		fi, err := os.Stat(d)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v, %v — want 0700", d, fi.Mode().Perm(), err)
		}
	}
}

// drills 2026-11-03: a Viewer for an anonymous visitor says so when printed —
// it called them "a signed-in person" while every start in their name was
// refused viewer_anonymous. Python's repr already told the three apart.
func TestAViewerPrintsWhoTheRequestCarries(t *testing.T) {
	key := "test-role-key"
	t.Setenv(EnvRoleKey, key)
	for role, want := range map[string]string{
		RoleAnonymous: "an anonymous visitor",
		RoleViewer:    "a signed-in person",
		"":            "this request's viewer;",
	} {
		h := http.Header{}
		h.Set(ActForHeader, "blob")
		if role != "" {
			h.Set(RoleHeader, mintForTest(key, role, time.Now().Add(time.Minute)))
		}
		v, err := ViewerFor(h)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); !strings.Contains(got, want) || strings.Contains(got, "blob") {
			t.Errorf("role %q printed %q, want %q", role, got, want)
		}
	}
}

// drills 2026-11-03: a value JSON cannot hold is refused with a hint, as in
// Python, not with an empty one.
func TestAnUnencodableBodyCarriesAHint(t *testing.T) {
	e, _ := AsError(encodeError(errors.New("json: unsupported value: NaN")))
	if e == nil || e.Code() != "bad_request" || !strings.Contains(e.Hint(), "NaN") {
		t.Fatalf("encodeError = %+v", e)
	}
}
