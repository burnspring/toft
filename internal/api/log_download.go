package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"toft/internal/builder"
	"toft/internal/database"
	"toft/internal/runtime"
)

// A log's download is the **whole** log, read on the server.
//
// The pane used to build its file from whatever the browser was holding: the
// lines one socket had received, or the tail one request had asked for. So the
// Download beside a build that had printed four thousand lines saved the few
// dozen this tab happened to see, and nothing on it said so. A file someone is
// going to send to somebody else is the one place a partial copy is worst — the
// reader of it has no screen to compare it against (ADR §87).
//
// `?download=1` on the two read routes is the form, rather than a route each:
// the authorization is the read's, the IDOR pin on a deployment is the read's,
// and the bytes are the read's. What changes is the framing — plain text, the
// terminal escapes removed, and a filename that says which log and which build.

// maxDownloadLines bounds a run-log download. Ten times the tail's ceiling: a
// download is one request somebody asked for, not a poll, and the backends hold
// less than this for most apps anyway. It still reaches the backend's tail —
// the agent's file read from its end, the pod log's tailLines — so it is still
// a bound.
const maxDownloadLines = 50000

// ansiEscapes are the terminal's colour and cursor sequences, which the pane
// turns into colour and which are noise in a file, a chat message and an email.
// The same two patterns `stripLogNoise` in web/src/lib/ansi.tsx applies.
var ansiEscapes = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b[^\[]`)

func stripANSI(s string) string {
	return ansiEscapes.ReplaceAllString(s, "")
}

// wantsDownload reports whether the caller asked for the log as text rather
// than the JSON read: `?download=1` as a file, `?raw=1` shown in the tab.
func wantsDownload(r *http.Request) bool {
	return flag(r, "download") || wantsRaw(r)
}

// wantsRaw is *View raw*: the same text, inline, so the browser's own find and
// save work on the whole log.
func wantsRaw(r *http.Request) bool {
	return flag(r, "raw")
}

func flag(r *http.Request, name string) bool {
	v := r.URL.Query().Get(name)
	return v == "1" || v == "true"
}

// writeLogFile answers with text, as an attachment named filename or — for
// `?raw=1` — inline. Inline is safe to serve from the platform's origin because
// it is text/plain under the global `nosniff`, and the sandbox below refuses it
// a script context even if a browser disagreed.
func writeLogFile(w http.ResponseWriter, r *http.Request, filename, text string) {
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	disposition := "attachment"
	if wantsRaw(r) {
		disposition = "inline"
		w.Header().Set("Content-Security-Policy", "sandbox")
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disposition, filename))
	// A build log is the raw output of a build run with the app's secrets in
	// its environment; no shared cache may keep a copy.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(stripANSI(text)))
}

// buildLogFilename names one build's log the way the pane always has:
// `<slug>-build-<commit or id, seven characters>.log`. Four `logs.txt` in one
// downloads folder are four files nobody can tell apart.
func buildLogFilename(app *database.App, deploy *database.Deployment) string {
	ref := deploy.CommitSHA
	if ref == "" {
		ref = deploy.ID
	}
	if len(ref) > 7 {
		ref = ref[:7]
	}
	return fmt.Sprintf("%s-build-%s.log", app.Slug, ref)
}

// buildLogText is a build log as a file: each line with the time it was
// written, in runLogText's shape, where the build recorded one (ADR §155). A
// build that recorded none is its text exactly as stored.
func buildLogText(text string, lt *builder.LineTimes) string {
	if lt == nil || text == "" {
		return text
	}
	lines, ms := storedStamps(text, lt)
	var sb strings.Builder
	for i, l := range lines {
		if ms[i] >= 0 {
			sb.WriteString(time.UnixMilli(ms[i]).UTC().Format(time.RFC3339Nano))
			sb.WriteByte(' ')
		}
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// runLogText is the run log as a file: one line per line, stamped where the
// backend stamped it. The pane's gutter shows a clock; a file read tomorrow
// needs the date as well.
func runLogText(lines []runtime.LogLine) string {
	var sb strings.Builder
	for _, l := range lines {
		if l.At != nil {
			sb.WriteString(l.At.UTC().Format(time.RFC3339Nano))
			sb.WriteByte(' ')
		}
		sb.WriteString(l.Line)
		sb.WriteByte('\n')
	}
	return sb.String()
}
