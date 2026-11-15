package api

import (
	"testing"

	"toft/internal/builder"
)

// ADR §155: the timed socket's framing, and a stored log longer than its times.
func TestStampedMessage(t *testing.T) {
	got := stampedMessage([]string{"a", "b"}, []int64{1000, -1})
	if got != "1000\ta\n-\tb\n" {
		t.Fatalf("stampedMessage = %q", got)
	}
}

func TestStoredStampsLeavesTheTailUnmarked(t *testing.T) {
	// The restart sweep appends after the build: those lines have no time.
	lines, ms := storedStamps("one\ntwo\n\nERROR: interrupted\n", &builder.LineTimes{StartMS: 5000, OffsetsMS: []int64{0, 250}})
	if len(lines) != 4 {
		t.Fatalf("lines = %q", lines)
	}
	want := []int64{5000, 5250, -1, -1}
	for i := range want {
		if ms[i] != want[i] {
			t.Errorf("line %d = %d, want %d", i, ms[i], want[i])
		}
	}
	if _, ms := storedStamps("x\n", nil); ms[0] != -1 {
		t.Error("a log with no times must stamp nothing")
	}
}

// The download stamps each line in the run log's shape; a line with no time,
// and a build with none at all, stay as stored.
func TestBuildLogTextStampsTheDownload(t *testing.T) {
	lt := &builder.LineTimes{StartMS: 1762542202000, OffsetsMS: []int64{0, 1500}}
	got := buildLogText("==> Fetch\ncloned\nERROR: interrupted\n", lt)
	want := "2025-11-07T19:03:22Z ==> Fetch\n2025-11-07T19:03:23.5Z cloned\nERROR: interrupted\n"
	if got != want {
		t.Fatalf("buildLogText =\n%q\nwant\n%q", got, want)
	}
	if got := buildLogText("plain\n", nil); got != "plain\n" {
		t.Fatalf("no times = %q", got)
	}
}
