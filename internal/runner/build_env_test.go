package runner

import (
	"slices"
	"testing"
)

// The pod's build is held to the host's on uv's link mode: without it every
// isolated Python build printed "Failed to hardlink files" twice, which the
// host path had already stopped doing (internal/builder cacheEnv).
func TestPodBuildsCopyRatherThanHardlink(t *testing.T) {
	if !slices.Contains(buildToolEnv, "UV_LINK_MODE=copy") {
		t.Errorf("buildToolEnv = %v, want UV_LINK_MODE=copy", buildToolEnv)
	}
}

// DG-1 (security review 2026-11-14): a pod's build reads no uv configuration,
// as the host's does not, because the scan reads none.
func TestPodBuildsReadNoUVConfig(t *testing.T) {
	if !slices.Contains(buildToolEnv, "UV_NO_CONFIG=1") {
		t.Errorf("buildToolEnv = %v, want UV_NO_CONFIG=1", buildToolEnv)
	}
}

// LB-10 (security review 2026-11-15): a pod's build reads no go environment
// file, as the host's does not.
func TestPodBuildsReadNoGoEnvFile(t *testing.T) {
	if !slices.Contains(buildToolEnv, "GOENV=off") {
		t.Errorf("buildToolEnv = %v, want GOENV=off", buildToolEnv)
	}
}
