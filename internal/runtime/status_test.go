package runtime

import "testing"

// The decision every reader of the shared vocabulary makes: is the backend
// running this workload. It used to be spelled `err == nil` in Reconcile — did
// the backend answer at all — which read an `errored` entry and a pid-less
// `online` one as "the process manager owns it from here", and so left them
// unstarted at every boot. It moved here with the constants, because three
// packages now ask the question and one of them is the process list.
//
// Asserted over the predicate rather than through a caller: each of those
// sweeps needs a database, a release tree and a runtime to reach the line, and
// a harness for one branch would be a fixture nobody else uses.
func TestWorkloadIsUpCoversTheSharedVocabulary(t *testing.T) {
	for _, tc := range []struct {
		status string
		up     bool
		why    string
	}{
		{StatusOnline, true, "running; adopt it"},
		{StatusLaunching, true, "the backend is bringing it up and owns it"},
		{StatusErrored, false, "crashed, and nothing else will start it"},
		{StatusStopped, false, "an entry with no process is not a running app"},
		{"", false, "a backend that answered with no word said nothing"},
	} {
		if got := WorkloadIsUp(tc.status); got != tc.up {
			t.Errorf("WorkloadIsUp(%q) = %v, want %v — %s", tc.status, got, tc.up, tc.why)
		}
	}
}
