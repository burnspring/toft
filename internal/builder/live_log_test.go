package builder

import (
	"strings"
	"testing"
)

// A reader who arrives mid-build is handed every line the build has printed,
// then each line after it — none twice, none missing. Before the live copy, a
// page that connected at minute four saw what came after minute four.
func TestSubscribeLogsHandsOverTheBacklog(t *testing.T) {
	b := &Builder{listeners: map[string][]chan string{}, live: map[string][]string{"d1": {}}}

	b.broadcast("d1", "one")
	b.broadcast("d1", "two")

	backlog, ch := b.SubscribeLogs("d1")
	defer b.UnsubscribeLogs("d1", ch)
	if len(backlog) != 2 || backlog[0] != "one" || backlog[1] != "two" {
		t.Fatalf("backlog = %q, want [one two]", backlog)
	}

	b.broadcast("d1", "three")
	if got := <-ch; got != "three" {
		t.Errorf("next live line = %q, want three", got)
	}

	// DONE is the socket's framing, not a line of the log.
	b.broadcast("d1", "DONE")
	if live, ok := b.LiveLog("d1"); !ok || live != "one\ntwo\nthree\n" {
		t.Errorf("LiveLog = %q, %v; want the three lines and no DONE", live, ok)
	}
}

// A build that is not running here has no live copy; its row is the log.
func TestSubscribeLogsWithNoRunningBuildHasNoBacklog(t *testing.T) {
	b := &Builder{listeners: map[string][]chan string{}}

	backlog, ch := b.SubscribeLogs("gone")
	defer b.UnsubscribeLogs("gone", ch)
	if backlog != nil {
		t.Errorf("backlog = %q, want nil", backlog)
	}
	if _, ok := b.LiveLog("gone"); ok {
		t.Error("LiveLog claims a build that is not running")
	}
	// A line for a build with no live copy still reaches its listeners.
	b.broadcast("gone", "late")
	if got := <-ch; got != "late" {
		t.Errorf("got %q", got)
	}
}

// LB-8 (security review 2026-11-15): a build that printed without stopping
// grew the stored text, the live lines and their times without bound, in the
// one process that also serves every app. Past the cap all three stop
// together, and the stored log ends with the build's last lines.
func TestABuildLogThatNeverStopsIsBounded(t *testing.T) {
	b := &Builder{listeners: map[string][]chan string{}, live: map[string][]string{"d1": {}}}
	f := &logFunnel{b: b, id: "d1"}
	line := strings.Repeat("x", 1024)
	n := maxBuildLogBytes/len(line) + 5000
	for i := 0; i < n; i++ {
		f.add(line)
	}
	f.add("ERROR: the last line")

	live, ok := b.LiveLog("d1")
	if !ok {
		t.Fatal("no live copy")
	}
	if len(live) > maxBuildLogBytes+len(buildLogCapNotice)+1 {
		t.Errorf("live log is %d bytes, over the cap", len(live))
	}
	if len(b.live["d1"]) != len(b.liveAt["d1"]) {
		t.Errorf("live lines %d and their times %d are not aligned", len(b.live["d1"]), len(b.liveAt["d1"]))
	}
	stored := f.text()
	if len(stored) > maxBuildLogBytes+len(buildLogCapNotice)+100+buildLogTailLines*(buildLogTailLineBytes+1) {
		t.Errorf("stored log is %d bytes, over the cap plus the tail", len(stored))
	}
	if !strings.Contains(stored, buildLogCapNotice) || !strings.Contains(stored, "lines dropped") {
		t.Error("the stored log does not say it was cut")
	}
	if !strings.HasSuffix(stored, "ERROR: the last line\n") {
		t.Errorf("the stored log does not end with the build's last line: …%q", stored[len(stored)-80:])
	}
}

// A log under the cap is stored exactly as before, DONE included, and DONE is
// never a line of the live copy.
func TestABuildLogUnderTheCapIsUnchanged(t *testing.T) {
	b := &Builder{listeners: map[string][]chan string{}, live: map[string][]string{"d1": {}}}
	f := &logFunnel{b: b, id: "d1"}
	f.add("one")
	f.add("bad\x00byte")
	f.add("DONE")
	if got := f.text(); got != "one\nbad�byte\nDONE\n" {
		t.Errorf("text = %q", got)
	}
	if live, _ := b.LiveLog("d1"); live != "one\nbad�byte\n" {
		t.Errorf("LiveLog = %q", live)
	}
}
