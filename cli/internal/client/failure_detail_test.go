package client

import "testing"

// BB-074: a failed deploy's terminal frame without the why takes it from the
// frame that said it, so a --json result built from the terminal frame carries
// reason and message; the terminal frame's own fields win.
func TestTheTerminalFrameKeepsTheFailuresWhy(t *testing.T) {
	explained := ProgressEvent{Status: StatusFailed, Phase: "build", Step: "render",
		Reason: "build_failed", Code: "render_failed", Message: "there is no package called 'rmarkdown'"}

	got := withFailureDetail(ProgressEvent{Status: StatusFailed}, explained)
	if got.Reason != "build_failed" || got.Code != "render_failed" || got.Message != explained.Message ||
		got.Phase != "build" || got.Step != "render" {
		t.Errorf("filled = %+v", got)
	}

	own := withFailureDetail(ProgressEvent{Status: StatusFailed, Reason: "runtime_crashed", Message: "own"}, explained)
	if own.Reason != "runtime_crashed" || own.Message != "own" {
		t.Errorf("the terminal frame's own fields lost: %+v", own)
	}

	ok := withFailureDetail(ProgressEvent{Status: StatusSuccess}, explained)
	if ok.Reason != "" || ok.Message != "" {
		t.Errorf("a success took a failure's why: %+v", ok)
	}
}
