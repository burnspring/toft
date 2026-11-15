package runner

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// An app cannot speak in the platform's voice.
//
// This is the failure that would ship quietly: an app prints a line shaped like
// a runner line, the tee writes it to the pod log unchanged, the backend parses
// it back as `platform`, and the Logs tab shows Toft blaming itself for
// the app's own failure — in Toft's voice, with nothing on screen to
// suggest the app wrote it. It fails by producing a plausible-looking line,
// which is the definition of a silent failure.
func TestAppCannotForgeAPlatformLine(t *testing.T) {
	forged := LogMarker(time.Now(), LogSourcePlatform, "stderr") + "ERROR at app_exec: the platform is broken"

	// What the shipper does to a line arriving from app code.
	got := NeutralizeLine(forged)

	_, source, _, line := ParseLogLine(got)
	if source != LogSourceApp {
		t.Errorf("a forged line parsed back as %q; app output must always read as %q", source, LogSourceApp)
	}
	// The app did print this, so it must still be readable. Swallowing output
	// is worse than a mangled bracket.
	if !strings.Contains(line, "ERROR at app_exec: the platform is broken") {
		t.Errorf("the forged text was lost: %q", line)
	}
}

// The neutralization must not touch ordinary output. An app printing brackets,
// timestamps or slashes is ordinary, and mangling those would make the log
// wrong for every app in order to defend against one.
func TestNeutralizeLeavesOrdinaryOutputAlone(t *testing.T) {
	ordinary := []string{
		"",
		"plain output",
		"[info] something happened",
		"[2026-08-19T17:40:06.123Z] a bracketed timestamp of its own",
		"[2026-08-19T17:40:06.123Z app/stdout without the closing bracket",
		"prefixed later: [2026-08-19T17:40:06.123Z platform/stderr] not at the start",
		"[2026-08-19T17:40:06Z platform/stderr] wrong precision",
		"[2026-08-19T17:40:06.123Z runner/stdout] not a known source",
	}
	for _, line := range ordinary {
		if got := NeutralizeLine(line); got != line {
			t.Errorf("NeutralizeLine(%q) = %q, want it untouched", line, got)
		}
	}
}

// The marker round-trips: what the tee writes is what the backend reads.
func TestLogMarkerRoundTrip(t *testing.T) {
	when := time.Date(2026, 8, 19, 17, 40, 6, 123000000, time.UTC)

	for _, source := range []string{LogSourceApp, LogSourcePlatform} {
		for _, stream := range []string{"stdout", "stderr"} {
			text := LogMarker(when, source, stream) + "the line itself"

			at, gotSource, gotStream, line := ParseLogLine(text)
			if !at.Equal(when) {
				t.Errorf("%s/%s: at = %v, want %v", source, stream, at, when)
			}
			if gotSource != source {
				t.Errorf("%s/%s: source = %q", source, stream, gotSource)
			}
			if gotStream != stream {
				t.Errorf("%s/%s: stream = %q", source, stream, gotStream)
			}
			if line != "the line itself" {
				t.Errorf("%s/%s: line = %q", source, stream, line)
			}
		}
	}
}

// An unmarked line is app stdout. This is the whole reason ContractVersion does
// not move: an isolated service deployed by an older runner writes nothing
// marked, and its output must still read correctly rather than requiring every
// such service to be redeployed before its logs work.
func TestUnmarkedLineIsAppStdout(t *testing.T) {
	at, source, stream, line := ParseLogLine("an old runner said this")

	if !at.IsZero() {
		t.Errorf("at = %v, want the zero time: nothing stamped this line", at)
	}
	if source != LogSourceApp {
		t.Errorf("source = %q, want %q", source, LogSourceApp)
	}
	if stream != "stdout" {
		t.Errorf("stream = %q, want stdout", stream)
	}
	if line != "an old runner said this" {
		t.Errorf("line = %q", line)
	}
}

// The runner's own `log.Printf` never reaches the shipper, so the tee cannot
// mark it — and an unmarked line reads as the app's output, which would put the
// platform's diagnostics in the app's voice.
//
// Not hypothetical: the tail of a healthy isolated workload on the k3s box is
// `toft-runner: …` lines, which is what reading a real pod log found and
// reasoning about one had not.
func TestMarkedWriterLabelsTheStandardLogger(t *testing.T) {
	var buf bytes.Buffer
	l := log.New(MarkedWriter(&buf, LogSourcePlatform), "toft-runner: ", 0)

	l.Printf("consumption report: platform answered 401")

	_, source, stream, line := ParseLogLine(strings.TrimSuffix(buf.String(), "\n"))
	if source != LogSourcePlatform {
		t.Errorf("source = %q, want %q", source, LogSourcePlatform)
	}
	if stream != "stderr" {
		t.Errorf("stream = %q, want stderr", stream)
	}
	if line != "toft-runner: consumption report: platform answered 401" {
		t.Errorf("line = %q", line)
	}
}

// A multi-line record is marked per line, or only its first line reads as the
// platform's and the rest look like the app interleaving.
func TestMarkedWriterMarksEveryLine(t *testing.T) {
	var buf bytes.Buffer
	w := MarkedWriter(&buf, LogSourcePlatform)

	record := "first line\nsecond line\nthird line\n"
	n, err := w.Write([]byte(record))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	// The caller's own length, or the logger reads a partial write.
	if n != len(record) {
		t.Errorf("Write returned %d, want %d", n, len(record))
	}

	for i, out := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		_, source, _, line := ParseLogLine(out)
		if source != LogSourcePlatform {
			t.Errorf("line %d source = %q, want %q", i, source, LogSourcePlatform)
		}
		if line == out {
			t.Errorf("line %d was not marked: %q", i, out)
		}
	}
}
