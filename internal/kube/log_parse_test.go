package kube

import (
	"testing"

	"toft/internal/runtime"
)

// The runner's marker, parsed back from a real pod log.
//
// Captured with `kubectl logs` from a python workload on the k3s box after the
// tee shipped — a serve pod whose venv needed repairing, so it exercises the
// platform's narration, a tool's output pumped through the shipper, and the
// app's own stderr in one capture. Reasoning about the format produced it;
// reading it back is what says it works.
func TestParseRunnerLineAgainstARealPodLog(t *testing.T) {
	cases := []struct {
		raw        string
		wantSource string
		wantStream string
		wantLine   string
		wantStamp  bool
	}{
		{
			raw:        "[2026-08-19T18:34:40.260Z platform/stdout] ==> Python 3.14.7, as declared",
			wantSource: runtime.LogSourcePlatform,
			wantStream: runtime.LogStreamStdout,
			wantLine:   "==> Python 3.14.7, as declared",
			wantStamp:  true,
		},
		{
			raw:        "[2026-08-19T18:34:40.718Z platform/stdout] ==> Repairing the release: its virtual environment was built against an interpreter this image does not have",
			wantSource: runtime.LogSourcePlatform,
			wantStream: runtime.LogStreamStdout,
			wantLine:   "==> Repairing the release: its virtual environment was built against an interpreter this image does not have",
			wantStamp:  true,
		},
		{
			raw:        "[2026-08-19T18:34:41.421Z app/stdout] Using CPython 3.14.7 interpreter at: /usr/local/bin/python",
			wantSource: runtime.LogSourceApp,
			wantStream: runtime.LogStreamStdout,
			wantLine:   "Using CPython 3.14.7 interpreter at: /usr/local/bin/python",
			wantStamp:  true,
		},
		{
			// The split a pull would otherwise lose: kubelet merges the two
			// before anything can read them back, so this is only recoverable
			// because the marker carried it.
			raw:        "[2026-08-19T18:34:50.801Z app/stderr] INFO:     Started server process [71]",
			wantSource: runtime.LogSourceApp,
			wantStream: runtime.LogStreamStderr,
			wantLine:   "INFO:     Started server process [71]",
			wantStamp:  true,
		},
		{
			// The previous generation of the same pod, written by a runner
			// from before the tee. Every line of it degrades to app stdout,
			// which is what it actually is, and is why ContractVersion did not
			// have to move.
			raw:        "toft-runner: runner: consumption report: platform answered 401",
			wantSource: runtime.LogSourceApp,
			wantStream: runtime.LogStreamStdout,
			wantLine:   "toft-runner: runner: consumption report: platform answered 401",
			wantStamp:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.wantLine, func(t *testing.T) {
			got := parseRunnerLine(tc.raw)
			if got.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", got.Source, tc.wantSource)
			}
			if got.Stream != tc.wantStream {
				t.Errorf("stream = %q, want %q", got.Stream, tc.wantStream)
			}
			if got.Line != tc.wantLine {
				t.Errorf("line = %q, want %q", got.Line, tc.wantLine)
			}
			if tc.wantStamp && got.At == nil {
				t.Error("no timestamp parsed from a marked line")
			}
			if !tc.wantStamp && got.At != nil {
				t.Errorf("a timestamp appeared from nowhere: %v", got.At)
			}
		})
	}
}
