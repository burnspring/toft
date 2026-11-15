package toft

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hintedRefusal asserts a coded, final refusal that names a next step.
func hintedRefusal(t *testing.T, label string, err error, code string) {
	t.Helper()
	e, ok := AsError(err)
	if !ok || e.Code() != code || e.Retryable() || strings.TrimSpace(e.Hint()) == "" {
		t.Errorf("%s: %v — want %s, not retryable, with a hint", label, err, code)
	}
}

// The volume Mount that Store returns beside not_a_store answers not_a_store
// to every signed-URL method — Upload, List, URL, Download, Remove — while Put,
// a copy onto the mount, copies into a mounted volume as it does in Python,
// Node and R (BB-055 refused it; reversed 2026-11-04 for parity).
func TestEveryStoreMethodOnAVolumeIsNotAStore(t *testing.T) {
	root := t.TempDir()
	volume := filepath.Join(root, "vol")
	source := filepath.Join(root, "x")
	os.WriteFile(source, []byte("a"), 0o644)
	t.Setenv(EnvStorage, `[{"name":"blackbox-vol","kind":"volume","path":"`+volume+`","access":"full"}]`)

	m, err := Store("blackbox-vol")
	if e, ok := AsError(err); !ok || e.Code() != "not_a_store" {
		t.Fatalf("Store on a volume: %v", err)
	}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Upload":   func() error { _, err := m.Upload(ctx, source, "a.txt"); return err },
		"List":     func() error { _, err := m.List(ctx, "", ""); return err },
		"URL":      func() error { _, err := m.URL(ctx, "a.txt", false); return err },
		"Download": func() error { _, err := m.Download(ctx, "a.txt"); return err },
		"Remove":   func() error { return m.Remove(ctx, "a.txt") },
	} {
		hintedRefusal(t, name+" on a volume", call(), "not_a_store")
	}
	if _, err := m.Put(source, "a.txt"); err != nil {
		t.Fatalf("Put onto a mounted volume: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(volume, "a.txt")); err != nil || string(got) != "a" {
		t.Errorf("Put did not land in the volume: %q, %v", got, err)
	}
}

// BB-056: OutputJSON reports false beside the error when the output is empty
// or not JSON — v was not filled — and true only when it decoded.
func TestOutputJSONIsFalseWhenItCannotDecode(t *testing.T) {
	body := ""
	platform(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
	for _, b := range []string{"", "not json", "{"} {
		body = b
		var v map[string]any
		ok, err := JobRun{ID: "r1"}.OutputJSON(context.Background(), &v)
		if ok {
			t.Errorf("output %q: ok = true beside %v", b, err)
		}
		hintedRefusal(t, "output "+b, err, "not_json")
	}
	body = `{"rows":3}`
	var v map[string]any
	if ok, err := (JobRun{ID: "r1"}).OutputJSON(context.Background(), &v); !ok || err != nil || v["rows"] != float64(3) {
		t.Errorf("JSON output: ok %v, err %v, v %v", ok, err, v)
	}
}

// BB-053: the refusals the SDK makes itself carry a hint.
func TestBB5LocalRefusalsCarryAHint(t *testing.T) {
	ctx := context.Background()
	_, err := Jobs.Start(ctx, "", StartOptions{})
	hintedRefusal(t, `Jobs.Start("")`, err, "bad_request")
	_, err = Jobs.List(ctx, ListOptions{Status: "bogus"})
	hintedRefusal(t, `Jobs.List(status bogus)`, err, "bad_request")
	_, err = Jobs.List(ctx, ListOptions{Limit: -1})
	hintedRefusal(t, `Jobs.List(limit -1)`, err, "bad_request")
	_, err = Jobs.Get(ctx, "")
	hintedRefusal(t, `Jobs.Get("")`, err, "bad_request")
	_, err = Data.Set(ctx, nil)
	hintedRefusal(t, "Data.Set(nil)", err, "bad_request")
	_, err = SessionScratchDir("  ")
	hintedRefusal(t, `SessionScratchDir("")`, err, "bad_request")
	_, err = AttachFile(filepath.Join(t.TempDir(), "missing.xlsx"))
	hintedRefusal(t, "AttachFile(missing)", err, "not_found")
	_, err = AttachFile(t.TempDir())
	hintedRefusal(t, "AttachFile(a directory)", err, "bad_request")
	err = (&Run{ID: "r1"}).Progress(ctx, 140, "")
	hintedRefusal(t, "Progress(140)", err, "bad_request")

	for _, env := range []string{`not json`, `{"name":"x"}`, `"x"`, `[1]`} {
		t.Setenv(EnvStorage, env)
		_, err := Mounts()
		hintedRefusal(t, "Mounts with "+env, err, "storage_env_invalid")
	}
}

// BB-061: the storage_env_invalid hint does not call path required — a store
// reached by signed URL has none — and an entry without one is accepted.
func TestTheStorageEnvHintCallsPathOptional(t *testing.T) {
	t.Setenv(EnvStorage, `[{"name":"x"}]`)
	_, err := Mounts()
	e, _ := AsError(err)
	if e == nil || !strings.Contains(e.Hint(), "signed URL") || strings.Contains(e.Hint(), "path and access") {
		t.Errorf("hint = %q — want path described as optional", e.Hint())
	}
	t.Setenv(EnvStorage, `[{"name":"drop","kind":"store","access":"read"}]`)
	if mounts, err := Mounts(); err != nil || len(mounts) != 1 || mounts[0].Mounted() {
		t.Errorf("a store with no path: %v, %v", mounts, err)
	}
}

// D-32: a ctx whose deadline passes is wait_timeout (retryable); a ctx
// cancelled outright is wait_cancelled (final), with a hint.
func TestWaitTellsACancelFromADeadline(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"id": "r1", "job": "j", "status": "running"})
	})
	run := JobRun{ID: "r1", Status: RunRunning}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := run.Wait(ctx, WaitOptions{PollInterval: 5 * time.Millisecond})
	if e, ok := AsError(err); !ok || e.Code() != "wait_timeout" || !e.Retryable() {
		t.Errorf("deadline: %v — want wait_timeout, retryable", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err = run.Wait(ctx, WaitOptions{PollInterval: 5 * time.Millisecond})
	hintedRefusal(t, "cancelled", err, "wait_cancelled")
}

// BB-050: a run's ScheduledFor keeps the offset the platform sent — a firing in
// America/Bogota reads 12:34-05:00, not 17:34Z.
func TestScheduledForKeepsThePlatformsOffset(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"id": "r1", "job": "j", "status": "queued",
			"scheduled_for": "2026-11-03T12:34:00-05:00"})
	})
	run, err := Jobs.Get(context.Background(), "r1")
	if err != nil || run.ScheduledFor == nil {
		t.Fatalf("Get: %v, %v", run, err)
	}
	if got := run.ScheduledFor.Format(time.RFC3339); got != "2026-11-03T12:34:00-05:00" {
		t.Errorf("ScheduledFor = %s — want the platform's offset kept", got)
	}
	if !run.ScheduledFor.Equal(time.Date(2026, 11, 3, 17, 34, 0, 0, time.UTC)) {
		t.Errorf("ScheduledFor names a different instant: %s", run.ScheduledFor)
	}

	t.Setenv(EnvScheduledFor, "2026-11-03T12:34:00-05:00")
	if at, ok := (&Run{}).ScheduledFor(); !ok || at.Format(time.RFC3339) != "2026-11-03T12:34:00-05:00" {
		t.Errorf("Run.ScheduledFor = %s, %v", at, ok)
	}
}
