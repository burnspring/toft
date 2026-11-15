package main

import (
	"fmt"
	"strings"

	"toft/internal/deploysteps"
)

// What a failed deploy is told to do next.
//
// **The reason is a key, and this is the only place it becomes English.** The
// key is what a program branches on; a person needs the sentence, and a person
// reading a CI log needs it too. Keeping the translation in one file is what
// stops the CLI growing three wordings of one rule — the argument
// `internal/deps.Refusal` already makes for the refusal it renders in five
// places.
//
// The half that earns its place for both readers is **which log holds the
// detail**. "The build failed" and "it built and would not answer" send you to
// two different streams, and an agent that reads the wrong one finds nothing
// and concludes there is nothing to find.

// detailSource is where the rest of the story is, for a given failure.
type detailSource string

const (
	// detailBuildLog is the build's own output: `toft logs --deploy <id>`.
	detailBuildLog detailSource = "build"
	// detailRuntimeLog is the app process's output: `toft logs`. The build
	// succeeded, so the build log ends with "Ready." and says nothing at all
	// about why the app then fell over.
	detailRuntimeLog detailSource = "runtime"
	// detailDependencies is the structured answer behind a policy refusal:
	// `toft deps`. The packages, the advisories and the versions that fix them.
	detailDependencies detailSource = "dependencies"
	// detailMemory is a workload killed for its memory limit: no log has the
	// rest, because the app was sent a signal and printed nothing, and the
	// next action is a larger limit — an administrator's to set.
	detailMemory detailSource = "memory"
	// detailNone is a failure whose message is the whole story — a refusal, a
	// manifest the platform would not read. Sending somebody to a log that
	// repeats one line is worse than sending them nowhere.
	detailNone detailSource = ""
)

// whereTheDetailIs answers the only question a failed deploy leaves open.
//
// An unknown reason — an install newer than this binary — answers
// detailBuildLog rather than nothing. It is the right guess for most failures
// and the cheap one to be wrong about; answering "nowhere" would tell somebody
// there is nothing more to read when there almost certainly is.
func whereTheDetailIs(reason string) detailSource {
	switch deploysteps.Reason(reason) {
	case deploysteps.ReasonDependencyBlocked:
		return detailDependencies
	case deploysteps.ReasonSourceBlocked, deploysteps.ReasonManifestInvalid,
		deploysteps.ReasonRuntimeVersion, deploysteps.ReasonUnsupportedFramework,
		deploysteps.ReasonStorageRefused, deploysteps.ReasonStorageRevokedDuringDeploy:
		return detailNone
	case deploysteps.ReasonOutOfMemory:
		return detailMemory
	case deploysteps.ReasonStartFailed, deploysteps.ReasonNotReady:
		return detailRuntimeLog
	case deploysteps.ReasonInternal, deploysteps.ReasonDiskFull:
		return detailNone
	case "":
		// Unclassified: an install older than the vocabulary. The step is all
		// anybody has, and the build log is where a step's output lives.
		return detailBuildLog
	}
	return detailBuildLog
}

// nextStep is the one line printed under a failure, naming the command that
// has the rest. Empty when the message already was the rest.
func nextStep(reason, deployID string) string {
	switch whereTheDetailIs(reason) {
	case detailDependencies:
		return "The packages, the advisories and the versions that fix them: toft deps"
	case detailMemory:
		return "It ran out of memory, so its own output will not say why. It needs a larger memory " +
			"limit: an administrator sets it in the app's Runtime settings (memory_limit_mb), " +
			"within the ceiling set in Admin → Settings"
	case detailRuntimeLog:
		return "It built and then would not answer — the app's own output is in: toft logs"
	case detailBuildLog:
		return "The whole build log: toft logs --deploy " + deployID
	}
	return ""
}

// failureSentence says where it died, because "failed during scan" and "failed
// during deploy" are two different next actions.
//
// The reason is named in parentheses when this binary knows it, and omitted
// when it does not — an install newer than this `toft` can send a key that means
// nothing here, and printing it raw beside a sentence it does not match reads
// as a bug rather than as a newer platform.
func failureSentence(phase, step, reason, message string) string {
	where := "the deploy failed"
	if phase != "" {
		where = fmt.Sprintf("failed during %s", phase)
		if step != "" {
			where += " (" + step + ")"
		}
	}
	if deploysteps.KnownReason(reason) {
		where += " — " + reason
	}
	if message != "" {
		return where + ": " + plainText(message)
	}
	return where
}

// plainText is s with every escape sequence removed, colour included: what
// the CLI says in its own voice — a failure sentence, a progress line — may
// quote what an app wrote, and no app's bytes drive the terminal from there
// (docs/invariants.md § The CLI; security review 2026-11-10, G-1).
//
// Every other control character goes too (dropControls): a `\r` or `\b`
// overwrites what the CLI printed before it, and U+009B/U+009D are CSI and OSC
// without an ESC (security review 2026-11-14, CS-6).
func plainText(s string) string {
	if !hasControl(s) {
		return s
	}
	return dropControls(ansiEscape.ReplaceAllString(crLines(s), ""))
}

// tailOf keeps the last n lines of a log, which is what a failure is entitled
// to carry without becoming the log itself.
func tailOf(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
