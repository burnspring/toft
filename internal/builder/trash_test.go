package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"toft/internal/artifact"
)

// waitForTrash swaps the background removal for one the test can wait on.
func waitForTrash(t *testing.T) <-chan string {
	t.Helper()
	done := make(chan string, 8)
	orig := removeTrash
	removeTrash = func(path string) {
		orig(path)
		done <- path
	}
	t.Cleanup(func() { removeTrash = orig })
	return done
}

func writeTrashTree(t *testing.T, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		p := filepath.Join(root, "releases", "d1", "venv", "lib", string(rune('a'+i%26)), "f")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The app's path is free when the call returns, and its bytes go afterwards,
// from the trash -- which is what lets a delete answer before a network
// filesystem has walked a virtualenv.
func TestDeletingAnAppFreesItsPathAtOnceAndEmptiesTheTrashAfter(t *testing.T) {
	done := waitForTrash(t)
	apps := t.TempDir()
	writeTrashTree(t, AppDir(apps, "app-1"), 40)

	if err := DeleteAppStorage(apps, "", "", nil, "app-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(AppDir(apps, "app-1")); !os.IsNotExist(err) {
		t.Fatalf("the app directory is still there after the call: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the trash was never emptied")
	}
	left, _ := os.ReadDir(filepath.Join(apps, trashName))
	if len(left) != 0 {
		t.Errorf("the trash still holds %d entries", len(left))
	}

	// Idempotent, as a retry needs: an absent app is success.
	if err := DeleteAppStorage(apps, "", "", nil, "app-1"); err != nil {
		t.Errorf("deleting an app that is already gone: %v", err)
	}
}

func TestDeletingAReleaseUsesTheTrashToo(t *testing.T) {
	done := waitForTrash(t)
	apps := t.TempDir()
	writeTrashTree(t, AppDir(apps, "app-1"), 5)

	if err := DeleteRelease(apps, nil, "app-1", "d1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(ReleaseDir(apps, "app-1", "d1")); !os.IsNotExist(err) {
		t.Fatalf("the release directory is still there: %v", err)
	}
	if _, err := os.Lstat(AppDir(apps, "app-1")); err != nil {
		t.Errorf("the app's own directory went with its release: %v", err)
	}
	<-done
}

// What a restart interrupted is finished at the next start.
func TestSweepTrashRemovesWhatAPreviousProcessLeft(t *testing.T) {
	done := waitForTrash(t)
	apps := t.TempDir()
	writeTrashTree(t, filepath.Join(apps, trashName, "app-1-123", "tree"), 3)

	SweepTrash(apps)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep removed nothing")
	}
	left, _ := os.ReadDir(filepath.Join(apps, trashName))
	if len(left) != 0 {
		t.Errorf("the trash still holds %d entries after the sweep", len(left))
	}
}

// Deleting an app removes its runs' outputs from the artifact store, not only
// its releases: the job_runs rows holding their keys cascade away with the
// app, so nothing could find them afterwards (AS-3).
func TestDeleteAppStorageRemovesTheAppsRunOutputs(t *testing.T) {
	store, err := artifact.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "out"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mine := artifact.RunOutputKey("app-1", "r1", "output")
	theirs := artifact.RunOutputKey("app-2", "r1", "output")
	for _, k := range []string{mine, theirs} {
		if _, err := store.Put(ctx, k, src); err != nil {
			t.Fatal(err)
		}
	}

	if err := DeleteAppStorage(t.TempDir(), "", "", store, "app-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(ctx, mine); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("the deleted app's run output survived: %v", err)
	}
	if _, err := store.Stat(ctx, theirs); err != nil {
		t.Errorf("another app's run output was removed: %v", err)
	}
}
