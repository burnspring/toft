package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A declared interpreter that never answers fails the probe — and so the boot —
// at probeTimeout and names the path, rather than hanging the platform.
func TestAWedgedProbeTimesOutAndNamesThePath(t *testing.T) {
	old := probeTimeout
	probeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { probeTimeout = old })

	wedged := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(wedged, []byte("#!/bin/sh\nsleep 60 &\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := runVersion(wedged, []string{"--version"})
	if err == nil {
		t.Fatal("a wedged probe returned no error")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the probe returned after %s", took)
	}
	if !strings.Contains(err.Error(), wedged) || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %v, want it to name the path that hung", err)
	}
}
