package toft

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The 2026-11-04 black-box list's second half, Go's side.

// BB-009: a negative Timeout or PollInterval is bad_request, not "no timeout"
// — on a finished run too.
func TestWaitRefusesANegativeTimeout(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{"running", "succeeded"} {
		run := JobRun{ID: "r1", Status: status}
		_, err := run.Wait(ctx, WaitOptions{Timeout: -time.Second})
		hintedRefusal(t, status+" Timeout -1s", err, "bad_request")
		_, err = run.Wait(ctx, WaitOptions{Timeout: time.Second, PollInterval: -time.Second})
		hintedRefusal(t, status+" PollInterval -1s", err, "bad_request")
	}
}

// BB-025: Logs refuses a negative cursor before any request, as Node does.
func TestLogsRefusesANegativeCursor(t *testing.T) {
	_, err := JobRun{ID: "r1"}.Logs(context.Background(), -1)
	hintedRefusal(t, "Logs(-1)", err, "bad_request")
}

// BB-023: TOFT_STORAGE='[]' is not_mapped; only no TOFT_STORAGE at all is
// off_platform.
func TestEmptyStorageListIsNotOffPlatform(t *testing.T) {
	for _, v := range []string{"TOFT_API_URL", "TOFT_APP_ID", "TOFT_RUN_ID", "TOFT_SCRATCH_DIR"} {
		t.Setenv(v, "")
	}
	t.Setenv(EnvStorage, "[]")
	_, err := Storage("x")
	hintedRefusal(t, "Storage with []", err, "not_mapped")
	_, err = Store("x")
	hintedRefusal(t, "Store with []", err, "not_mapped")

	os.Unsetenv(EnvStorage) // t.Setenv above restores it after the test
	_, err = Storage("x")
	hintedRefusal(t, "Storage with no TOFT_STORAGE", err, "off_platform")
}

// BB-008's sweep: Put given a directory as its source is bad_request — the
// source's fault — and leaves nothing on the mount; it was write_failed.
func TestPutOfADirectoryIsTheSourcesFault(t *testing.T) {
	mount := t.TempDir()
	m := Mount{Name: "st", Kind: "store", Path: mount, Access: "full"}
	_, err := m.Put(t.TempDir(), "x/dir.txt")
	hintedRefusal(t, "Put(dir)", err, "bad_request")
	if _, err := os.Stat(filepath.Join(mount, "x", "dir.txt")); err == nil {
		t.Error("Put of a directory left an object behind")
	}
}
