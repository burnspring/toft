package toft

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusedLocally asserts err is a coded, non-retryable SDK refusal.
func refusedLocally(t *testing.T, name string, err error, code string) {
	t.Helper()
	e, ok := AsError(err)
	if !ok || e.Code() != code || e.Retryable() {
		t.Errorf("%s: %v — want code %q and not retryable", name, err, code)
	}
}

// BB-006: a part that climbs out of the scratch root, or an absolute one, is
// refused rather than joined into the app's own tree.
func TestScratchDirRefusesAPathOutsideTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scratch")
	t.Setenv(EnvScratchDir, root)
	for _, parts := range [][]string{
		{"../escape"}, {"a", "../../escape"}, {"/etc"}, {"ok", "/abs"},
	} {
		dir, err := ScratchDir(parts...)
		refusedLocally(t, "ScratchDir"+strings.Join(parts, ","), err, "bad_request")
		if dir != "" {
			t.Errorf("ScratchDir(%q) = %q, want no path", parts, dir)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); err == nil {
		t.Error("the refused path was created anyway")
	}
	// Staying inside is still fine, ".." that does not climb out included.
	dir, err := ScratchDir("a", "../b")
	if err != nil || dir != filepath.Join(root, "b") {
		t.Errorf("ScratchDir(a, ../b) = %q, %v", dir, err)
	}
	// A session's parts are held to the session's own directory.
	_, err = SessionScratchDir("sid", "../other-session")
	refusedLocally(t, "SessionScratchDir(sid, ../other)", err, "bad_request")
}

// A whitespace-only session id collapses sessions as surely as an empty one.
func TestABlankSessionIdIsRefused(t *testing.T) {
	t.Setenv(EnvScratchDir, t.TempDir())
	_, err := SessionScratchDir("   ")
	refusedLocally(t, "SessionScratchDir(blank)", err, "bad_request")
}

// BB-007: local failures are coded and never retryable.
func TestLocalFileFailuresAreCoded(t *testing.T) {
	_, err := AttachFile("/nonexistent/file.xlsx")
	refusedLocally(t, "AttachFile(missing)", err, "not_found")
	_, err = AttachFile(t.TempDir())
	refusedLocally(t, "AttachFile(directory)", err, "bad_request")

	m := Mount{Name: "reports", Kind: "store", Path: t.TempDir(), Access: "write"}
	_, err = m.Put("/nonexistent/x.csv", "x.csv")
	refusedLocally(t, "Put(missing source)", err, "not_found")

	newStoreFake(t)
	_, err = unmountedStore().Upload(context.Background(), "/nonexistent/x.csv", "")
	refusedLocally(t, "Upload(missing source)", err, "not_found")

	t.Setenv(EnvStorage, "{not json")
	_, err = Mounts()
	refusedLocally(t, "Mounts(bad env)", err, "storage_env_invalid")
}

// BB-033: every operation on an unmapped store answers not_mapped, as
// Store("nope") did — the zero Mount locally, and a hand-built one through the
// platform's store_not_mapped.
func TestEveryOperationOnAnUnmappedStoreIsNotMapped(t *testing.T) {
	t.Setenv(EnvStorage, `[{"name":"uploads","kind":"store","path":"","access":"write"}]`)
	newStoreFake(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "x.csv")
	_ = os.WriteFile(src, []byte("x"), 0o644)

	zero, err := Store("nope")
	refusedLocally(t, "Store(nope)", err, "not_mapped")
	ops := map[string]func(Mount) error{
		"List":     func(m Mount) error { _, err := m.List(ctx, "", ""); return err },
		"Upload":   func(m Mount) error { _, err := m.Upload(ctx, src, ""); return err },
		"Download": func(m Mount) error { _, err := m.Download(ctx, "x.csv"); return err },
		"URL":      func(m Mount) error { _, err := m.URL(ctx, "x.csv", false); return err },
		"Remove":   func(m Mount) error { return m.Remove(ctx, "x.csv") },
		"Put":      func(m Mount) error { _, err := m.Put(src, ""); return err },
	}
	for name, op := range ops {
		refusedLocally(t, "zero Mount "+name, op(zero), "not_mapped")
	}

	// A Mount naming a store the platform does not map: its store_not_mapped
	// is reported under the same code.
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 404, map[string]any{"error": "no store named \"gone\"", "code": "store_not_mapped", "retryable": false})
	})
	stale := Mount{Name: "gone", Kind: "store"}
	for name, op := range ops {
		if name == "Put" {
			continue // local: nothing mounted, no platform to ask
		}
		err := op(stale)
		refusedLocally(t, "stale Mount "+name, err, "not_mapped")
		if e, _ := AsError(err); e != nil && e.Status != 404 {
			t.Errorf("stale Mount %s: status %d, want the platform's 404", name, e.Status)
		}
	}
}

// Progress outside 0–100 is refused before any request.
func TestProgressOutOfRangeIsRefused(t *testing.T) {
	run := &Run{ID: "r", base: "http://127.0.0.1:1", token: "t", client: http.DefaultClient}
	for _, p := range []int{-1, 101, 150} {
		refusedLocally(t, "Progress", run.Progress(context.Background(), p, ""), "bad_request")
	}
}

// A status that is not one of the eight words, or a negative limit, is refused
// rather than answered with an empty list.
func TestListRefusesABogusStatusAndANegativeLimit(t *testing.T) {
	asked := false
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		asked = true
		writeJSON(t, w, 200, map[string]any{"runs": []any{}})
	})
	ctx := context.Background()
	_, err := Jobs.List(ctx, ListOptions{Status: "bogus"})
	refusedLocally(t, "List(bogus)", err, "bad_request")
	_, err = Jobs.List(ctx, ListOptions{Limit: -1})
	refusedLocally(t, "List(-1)", err, "bad_request")
	if asked {
		t.Error("a refused list reached the platform")
	}
	if _, err := Jobs.List(ctx, ListOptions{Status: RunTimedOut}); err != nil || !asked {
		t.Errorf("List(timed_out) = %v", err)
	}
}

// BB-019: nil is "the run wrote no output"; an empty, non-nil slice is "it
// wrote an empty one". And a 404 that is not no_output is a refusal.
func TestNoOutputAndAnEmptyOutputAreDifferent(t *testing.T) {
	ctx := context.Background()
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/none/"):
			writeJSON(t, w, 404, map[string]any{"error": "this run produced no output", "code": "no_output"})
		case strings.Contains(r.URL.Path, "/missing/"):
			writeJSON(t, w, 404, map[string]any{"error": "no such run", "code": "run_not_found", "retryable": false})
		default:
			w.WriteHeader(200) // an output object of zero bytes
		}
	})
	none, err := JobRun{ID: "none"}.Output(ctx)
	if err != nil || none != nil {
		t.Errorf("no output: %v, %v — want nil, nil", none, err)
	}
	empty, err := JobRun{ID: "empty"}.Output(ctx)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("empty output: %#v, %v — want a non-nil empty slice", empty, err)
	}
	_, err = JobRun{ID: "missing"}.Output(ctx)
	if e, ok := AsError(err); !ok || e.Code() != "run_not_found" {
		t.Errorf("an unknown run's output: %v — want its refusal, not nothing", err)
	}
}

// BB-032: a write and the read after it agree, and Decode keeps big integers
// exact.
func TestAWriteAndTheReadAfterItAgree(t *testing.T) {
	var stored json.RawMessage
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			var body json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			stored = body
			writeJSON(t, w, 200, map[string]any{"version": 1, "bytes": len(body)})
		case http.MethodGet:
			w.Write([]byte(`{"data":` + string(stored) + `,"version":1}`))
		}
	})
	ctx := context.Background()
	const big = int64(9007199254740993)
	saved, err := Data.Set(ctx, map[string]any{"n": big})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Data.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Data["n"] != got["n"] {
		t.Errorf("Set answered %v (%T), Get %v (%T) — they must agree", saved.Data["n"], saved.Data["n"], got["n"], got["n"])
	}
	read, _ := Data.Read(ctx)
	for name, doc := range map[string]Document{"Set": saved, "Read": read} {
		var typed struct{ N int64 }
		if ok, err := doc.Decode(&typed); !ok || err != nil || typed.N != big {
			t.Errorf("%s Decode into a struct = %v, %v, %d — want %d", name, ok, err, typed.N, big)
		}
		var loose map[string]any
		if _, err := doc.Decode(&loose); err != nil || loose["n"] != json.Number("9007199254740993") {
			t.Errorf("%s Decode into a map = %v (%T)", name, loose["n"], loose["n"])
		}
	}
	if ok, err := (Document{}).Decode(&struct{}{}); ok || err != nil {
		t.Errorf("Decode of a never-written document = %v, %v — want false", ok, err)
	}
}
