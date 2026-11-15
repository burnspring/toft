package procstat

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// The /proc field offsets are pure arithmetic over a positional format, and a
// wrong offset yields confident nonsense rather than an error. This checks them
// against a process whose CPU and memory we can move deliberately.
func TestReadAgainstThisProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs is Linux-only")
	}
	pid := os.Getpid()

	before, err := Read(pid)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if before.RSSBytes < 1<<20 {
		t.Errorf("rss = %d bytes; a live Go process uses more than 1 MiB", before.RSSBytes)
	}

	// Burn measurable CPU and hold memory.
	ballast := make([]byte, 64<<20)
	for i := range ballast {
		ballast[i] = byte(i)
	}
	deadline := time.Now().Add(400 * time.Millisecond)
	x := 0
	for time.Now().Before(deadline) {
		x++
	}
	_ = x

	after, err := Read(pid)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if after.CPUSeconds <= before.CPUSeconds {
		t.Errorf("cpuSeconds did not advance: %v -> %v", before.CPUSeconds, after.CPUSeconds)
	}
	if after.RSSBytes <= before.RSSBytes {
		t.Errorf("rss did not grow after allocating 64 MiB: %d -> %d", before.RSSBytes, after.RSSBytes)
	}
	runtime.KeepAlive(ballast)
}
