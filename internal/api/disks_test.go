package api

import (
	"errors"
	"testing"
	"time"

	"toft/internal/builder"
)

// Folders on one filesystem are one row, fullest first, with "how full"
// computed as df does — used over used plus available, so root's reserve does
// not read as free space.
func TestDisksAreGroupedByFilesystemFullestFirst(t *testing.T) {
	const gb = 1 << 30
	fs := map[string]builder.DiskUsage{
		"/var/lib/toft/apps": {Device: 1, Total: 200 * gb, Used: 50 * gb, Free: 140 * gb},
		"/var/lib/toft/jobs": {Device: 1, Total: 200 * gb, Used: 50 * gb, Free: 140 * gb},
		"/var/cache/toft":    {Device: 2, Total: 100 * gb, Used: 90 * gb, Free: 5 * gb, Level: builder.DiskSoft},
		"/mnt/efs":           {Device: 3, Total: 8 << 60, Used: 1 * gb, Free: 8<<60 - gb},
	}
	measure := func(p string) (builder.DiskUsage, error) {
		if u, ok := fs[p]; ok {
			return u, nil
		}
		return builder.DiskUsage{}, errors.New("no such file or directory")
	}
	disks, unanswered := disksFrom([]diskFolder{
		{"/var/lib/toft/apps", "App releases"},
		{"/var/lib/toft/jobs", "Job runs"},
		{"/mnt/efs", "Storage drive “shared”"},
		{"/var/cache/toft", "Build cache"},
		{"/gone", "Logs"},
	}, measure)

	if len(disks) != 3 {
		t.Fatalf("got %d disks, want 3: %+v", len(disks), disks)
	}
	if got := disks[0].Folders[0].Path; got != "/var/cache/toft" || disks[0].Level != "low" {
		t.Errorf("first row = %s (%s), want the fuller cache disk, low", got, disks[0].Level)
	}
	if p := disks[0].UsedPercent; p < 94.7 || p > 94.8 {
		t.Errorf("used percent = %.2f, want 90/(90+5) ≈ 94.74", p)
	}
	if n := len(disks[1].Folders); n != 2 {
		t.Errorf("the apps disk carries %d folders, want both on it", n)
	}
	if !disks[2].Unbounded {
		t.Errorf("a filesystem reporting 8 EiB should be unbounded and last: %+v", disks[2])
	}
	if len(unanswered) != 1 || unanswered[0].Path != "/gone" {
		t.Errorf("unanswered = %+v, want /gone", unanswered)
	}
}

// A mount that hangs does not hold the page.
func TestAHungMountIsReportedRatherThanWaitedFor(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	_, err := measureWithin(func(string) (builder.DiskUsage, error) {
		<-block
		return builder.DiskUsage{}, nil
	}, "/mnt/hung", 50*time.Millisecond)
	if !errors.Is(err, errDiskTimeout) {
		t.Errorf("err = %v, want the timeout", err)
	}
	if time.Since(start) > time.Second {
		t.Error("the measurement waited on the hung mount")
	}
}
