package runtime

import (
	"errors"
	"fmt"
	"testing"
)

// The mock used to be a third answer beside the two real backends' — PM2
// returning success with an empty tail, the cluster returning an error — and it
// is the answer every API test sees. A mock that errors on a workload it does
// not hold is a mock that teaches the suite the wrong contract.
func TestMockReportsAbsenceRatherThanFailure(t *testing.T) {
	m := NewMockRuntime(nil)

	logs, err := m.Logs("never-registered", 10)
	if got := AbsenceReason(err); got != LogAbsentNoWorkload {
		t.Errorf("Logs absence reason = %q (err %v), want %q", got, err, LogAbsentNoWorkload)
	}
	if len(logs) != 0 {
		t.Errorf("Logs returned %d lines alongside an absence", len(logs))
	}

	stream, err := m.LogStream("never-registered")
	if got := AbsenceReason(err); got != LogAbsentNoWorkload {
		t.Errorf("LogStream absence reason = %q (err %v), want %q", got, err, LogAbsentNoWorkload)
	}
	if stream != nil {
		stream.Close()
		t.Error("LogStream returned a stream for a workload that does not exist")
	}
}

// AbsenceReason is the single reader of an absence, so that "is this an
// absence?" and "which absence?" cannot be answered differently. It has to see
// through a wrap, because a backend adding context to an error is ordinary.
func TestAbsenceReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain failure", errors.New("connection refused"), ""},
		{"absence", Absent(LogAbsentStopped), LogAbsentStopped},
		{"wrapped absence", fmt.Errorf("kube: reading logs: %w", Absent(LogAbsentLaunching)), LogAbsentLaunching},
		{"wrapped failure", fmt.Errorf("kube: reading logs: %w", errors.New("boom")), ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AbsenceReason(tc.err); got != tc.want {
				t.Errorf("AbsenceReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// An absence still satisfies error, so every caller that has not been taught
// about it keeps treating a missing workload as a failure — which is the safe
// direction for the mistake to fall.
func TestAbsentErrorIsAnError(t *testing.T) {
	var err error = Absent(LogAbsentLocked)
	if err == nil {
		t.Fatal("Absent returned a nil error")
	}
	if err.Error() == "" {
		t.Error("an absence carries no message")
	}
}
