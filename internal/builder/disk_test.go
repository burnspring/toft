package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"toft/internal/artifact"
	"toft/internal/database"
)

func TestDiskLevelReadsBothWatermarksAndTheFloor(t *testing.T) {
	b := &Builder{opts: Options{DiskSoftFreePercent: 15, DiskHardFreePercent: 5}}
	const gib = 1 << 30
	for _, c := range []struct {
		free, total uint64
		want        DiskLevel
	}{
		{50 * gib, 100 * gib, DiskOK},
		{14 * gib, 100 * gib, DiskSoft},
		{4 * gib, 100 * gib, DiskHard},
		// 10% of a 5 GB disk is above both percentages and under the floor.
		{gib / 2, 5 * gib, DiskHard},
		{0, 0, DiskOK}, // unmeasurable is never a refusal
		// EFS: 8 EiB, nearly all free. free*100 used to overflow into "hard".
		{1<<63 - 1<<40, 1 << 63, DiskOK},
	} {
		if got := b.levelOf(c.free, c.total); got != c.want {
			t.Errorf("levelOf(%d, %d) = %s, want %s", c.free, c.total, got, c.want)
		}
	}
}

// A build is refused before it writes anything, naming what is free and what
// it needs; a disk that cannot be measured admits.
func TestAdmitTreeRefusesPastTheHardWatermark(t *testing.T) {
	full := &Builder{opts: Options{AppsDir: t.TempDir(), DiskSoftFreePercent: 100, DiskHardFreePercent: 99}}
	err := full.admitTree(1 << 20)
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("admitTree = %v, want ErrDiskFull", err)
	}
	roomy := &Builder{opts: Options{AppsDir: t.TempDir(), DiskSoftFreePercent: 2, DiskHardFreePercent: 1}}
	if err := roomy.admitTree(0); err != nil && !errors.Is(err, ErrDiskFull) {
		t.Fatalf("admitTree: %v", err)
	}
	missing := &Builder{opts: Options{AppsDir: filepath.Join(t.TempDir(), "absent"), DiskHardFreePercent: 99, DiskSoftFreePercent: 100}}
	if err := missing.admitTree(1); err != nil {
		t.Errorf("an unmeasurable disk refused: %v", err)
	}
}

func TestBuildEstimateIsTheLastMeasuredRelease(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	got := buildEstimate([]database.Deployment{
		{Status: database.DeployStatusFailed, DiskBytes: n(999)},
		{Status: database.DeployStatusSuccess, SourceDeployID: "x", DiskBytes: n(888)},
		{Status: database.DeployStatusSuccess, DiskBytes: n(100), ArtifactBytes: n(40)},
		{Status: database.DeployStatusSuccess, DiskBytes: n(5)},
	})
	if got != 140 {
		t.Errorf("buildEstimate = %d, want 140", got)
	}
}

// Shared keeps the live release and REL.2's rollback target; isolated keeps
// none of its own trees; pinned and unfinished releases stay either way.
func TestDiskKeep(t *testing.T) {
	deployments := []database.Deployment{
		{ID: "building", Status: database.DeployStatusBuilding, StartedAt: at(6)},
		{ID: "live", Status: database.DeployStatusSuccess, StartedAt: at(5), ExecutionMode: "shared"},
		{ID: "rollback-row", Status: database.DeployStatusSuccess, SourceDeployID: "v1", StartedAt: at(4)},
		{ID: "v3", Status: database.DeployStatusSuccess, StartedAt: at(3)},
		{ID: "v2", Status: database.DeployStatusSuccess, StartedAt: at(2)},
		{ID: "v1", Status: database.DeployStatusSuccess, StartedAt: at(1)},
	}
	pinned := map[string]bool{"v1": true}
	keep := diskKeep(&database.App{}, deployments, "live", pinned)
	for _, id := range []string{"live", "v3", "v1", "building"} {
		if !keep[id] {
			t.Errorf("shared: %s left the disk", id)
		}
	}
	if keep["v2"] {
		t.Error("shared: v2 is neither live nor the rollback target, and stayed")
	}

	deployments[1].ExecutionMode = "isolated"
	keep = diskKeep(&database.App{}, deployments, "live", pinned)
	if keep["live"] || keep["v3"] {
		t.Errorf("isolated: kept %v; its pod reads the store, not this disk", keep)
	}
	if !keep["v1"] || !keep["building"] {
		t.Error("isolated: a pinned or unfinished release left the disk")
	}
}

// A tree leaves the disk only when the store confirms it holds the release: a
// release built before the store existed is the only copy.
func TestEvictTreesNeedsTheStoresCopy(t *testing.T) {
	store := newStore(t)
	appsDir := t.TempDir()
	b := &Builder{opts: Options{AppsDir: appsDir}, artifacts: store}
	app := &database.App{ID: "app", Slug: "app"}

	for _, id := range []string{"live", "stored", "local-only"} {
		dir := ReleaseDir(appsDir, app.ID, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if id != "local-only" {
			if _, err := store.Put(context.Background(), artifact.ReleaseKey(app.ID, id), dir); err != nil {
				t.Fatal(err)
			}
		}
	}
	evicted := b.evictTrees(app, map[string]bool{"live": true})
	if len(evicted) != 1 || evicted[0] != "stored" {
		t.Errorf("evicted = %v, want [stored]", evicted)
	}
	for id, want := range map[string]bool{"live": true, "stored": false, "local-only": true} {
		if got := fileExists(ReleaseDir(appsDir, app.ID, id)); got != want {
			t.Errorf("%s on disk = %v, want %v", id, got, want)
		}
	}
}
