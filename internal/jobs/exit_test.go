package jobs

import (
	"testing"

	"toft/internal/runtime"
)

// A run stopped for its memory limit has no exit code: the host backend's 0 is
// stored as none, a real one (Kubernetes' 137) is kept, and an ordinary exit
// is its own (drills 2026-10-28 #2).
func TestAnOutOfMemoryRunIsStoredWithNoExitCode(t *testing.T) {
	if got := recordedExit(runtime.JobStatus{State: runtime.JobResourceExceeded}); got != nil {
		t.Errorf("out of memory, no code = %d, want none", *got)
	}
	if got := recordedExit(runtime.JobStatus{State: runtime.JobResourceExceeded, ExitCode: 137}); got == nil || *got != 137 {
		t.Errorf("out of memory, 137 = %v, want 137", got)
	}
	for _, st := range []runtime.JobStatus{{State: runtime.JobSucceeded}, {State: runtime.JobFailed, ExitCode: 3}} {
		if got := recordedExit(st); got == nil || *got != st.ExitCode {
			t.Errorf("%v = %v, want %d", st.State, got, st.ExitCode)
		}
	}
}
