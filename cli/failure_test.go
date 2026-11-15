package main

import (
	"strings"
	"testing"
)

// A deploy killed for its memory limit names the limit, not a log that has
// nothing to say about it.
func TestNextStepForOutOfMemoryNamesTheMemoryLimit(t *testing.T) {
	if got := whereTheDetailIs("out_of_memory"); got != detailMemory {
		t.Fatalf("whereTheDetailIs(out_of_memory) = %q, want %q", got, detailMemory)
	}
	line := nextStep("out_of_memory", "dep-1")
	if !strings.Contains(line, "memory limit") || strings.Contains(line, "toft logs") {
		t.Errorf("nextStep(out_of_memory) = %q", line)
	}
	if line := nextStep("not_ready", "dep-1"); !strings.Contains(line, "toft logs") {
		t.Errorf("not_ready lost its pointer to the runtime log: %q", line)
	}
}
