package builder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"toft/internal/deploysteps"
	"toft/internal/runner"
	"toft/internal/runtime"
	"toft/internal/toolchain"
)

// Every problem `toft check` names carries a code and a hint (the toft-cli
// contract: "every problem has both"). Most refusals set their own; this file
// is the fallback for the ones that set only a code, or neither, so a program
// can branch on every problem and a person can paste every fix (BB-005 and
// BB-013 of the second 2026-11-03 bash).

// codeHint is the fix a coded refusal implies when the refusal did not say
// one itself, or "".
func codeHint(code string) string {
	switch code {
	case EnvEntryInvalidCode:
		return `write each [env] entry as NAME = "default" or NAME = { sensitive = true }`
	case runner.EnvNameInvalidCode:
		return "rename the variable to letters, digits and underscores, not starting with a digit, like API_URL"
	case runner.EnvNameReservedCode:
		return "rename the variable: the platform sets this one for every app, so a value here never reaches it"
	case StorageEntryInvalid:
		return `write each [[storage]] entry as name = "<resource>" with access = "read", "write" or "full", and an optional path`
	case StorageNameAmbiguous:
		return "name each resource or folder in one [[storage]] entry, and give each mount a folder of its own"
	case StorageUnknown:
		// The same words whether or not the name exists (docs/invariants.md §
		// App storage), and still a way forward for someone told the name by
		// the administrator who created it.
		return "if an administrator created this storage, ask them to delegate it to you or to the app's owner; toft storage ls lists what you can attach now; or remove the [[storage]] entry"
	case StorageNotAttachableByOwner:
		return "ask the resource's administrator to delegate it to the app's owner, or remove the [[storage]] entry"
	case StorageNotAttachableByYou:
		return "ask the resource's administrator for an attach delegation — a store grant browses a resource, it does not attach it — or remove the [[storage]] entry"
	case StorageNotAttachableByNewOwner:
		return "ask the resource's administrator to delegate it to the new owner, or remove the [[storage]] entry before the move"
	case StorageLevelAboveCeiling:
		return "lower access to the level the message names, or ask the resource's administrator to raise the delegation"
	case StorageRequiresPerson:
		return "deploy signed in as a person (toft login), not with an app's or a machine's credential"
	case StorageConflictsConsole:
		return "remove the mapping from the app's Storage tab, or remove this [[storage]] entry"
	case StoragePathTaken:
		return "give each [[storage]] entry a path of its own, none inside another"
	case StorageDeleting:
		return "name another resource: this one is being deleted"
	case StorageNeedsRoot:
		return "name a folder inside the resource, or ask its administrator for a delegation of the whole resource"
	case StorageRevokedDuringDeploy:
		return "deploy again: the delegation changed while this deploy was planning"
	}
	return ""
}

// runtimeWriteLine is the `write "X"` a version-width refusal ends with.
var runtimeWriteLine = regexp.MustCompile(`: write ("(?:[^"\\]|\\.)*")$`)

// runtimeHint is the fix for a refused `[runtime]` key: the exact line the
// message names when it names one, go.mod for a go key, and otherwise an
// example in the key's own language — never a generic python/node line under
// a message that already said "write \"22\"".
func runtimeHint(lang, msg string) string {
	switch {
	case lang == "go":
		// A Go this build host has, not a fixed example: "like go 1.25" on an
		// install offering 1.26 and 1.27 led to go_toolchain_fetched (BB-008).
		line := "1.26"
		if set := toolchain.Available(toolchain.Go); len(set) > 0 && set[0].Series() != "" {
			line = set[0].Series()
		}
		return "remove go from [runtime] and set the go line in go.mod instead, like go " + line
	case lang == "":
		if strings.Contains(msg, "static app") {
			return "remove the [runtime] table: a static app has no interpreter to pin"
		}
		return `pin a release line, like python = "3.14" or node = "24"`
	}
	if m := runtimeWriteLine.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("write %s = %s under [runtime]", lang, m[1])
	}
	return fmt.Sprintf("pin a release line, like %s = %q", lang, exampleLine(lang))
}

func exampleLine(lang string) string {
	switch lang {
	case "node":
		return "24"
	case "r":
		return "4.5"
	}
	return "3.14"
}

// unsupportedHint is the fix for an unsupported_framework refusal, by the same
// branches UnsupportedMessage takes, so the hint answers the sentence beside it.
func unsupportedHint(srcDir string) string {
	if BatchRefusal(srcDir) != "" {
		return `fix [batch] as the message says: language = "python", "node", "r" or "go", with that language's files in the tree`
	}
	if _, err := ResolveNotebook(srcDir); err != nil {
		return notebookHint(err)
	}
	if severalRmdMessage(srcDir) != "" {
		return `name the document to serve in toft.toml: [notebook] file = "<name>.Rmd"`
	}
	if fileExists(filepath.Join(srcDir, "go.mod")) && goMainPackage(srcDir) == "" {
		return "add a main package — package main with func main — at the module root or under cmd/<name>"
	}
	if repositoryLooksLikeR(srcDir) {
		return "add app.R for Shiny, plumber.R (or #* @get annotations) for an API, or an .Rmd or .qmd document"
	}
	if decl := pythonProjectFile(srcDir); decl != "" && detectPythonEntrypoint(srcDir) == "" {
		return fmt.Sprintf("add %s (or one of %s) that starts the app", pythonEntrypoints[0], strings.Join(pythonEntrypoints[1:], ", "))
	}
	if name := disqualifier(srcDir); name == "package.json" {
		// A Node app starts from its start script, not from a file of a known
		// name — the fix the SDK guide states for a frontend-only repo (D-42).
		return `add a server and a "start" script to package.json, or commit the build output and declare it: [static] root = "dist"`
	} else if name != "" {
		return fmt.Sprintf("add the entry file a %s app starts from, or declare the built folder: [static] root = \"dist\"", name)
	}
	// The same two branches UnsupportedMessage takes before its static-site
	// sentence: the manifest the program is missing (BB-19).
	if entry := nodeEntryWithoutManifest(srcDir); entry != "" {
		return fmt.Sprintf(`add a package.json with a "start" script, like "scripts": {"start": "node %s"}`, entry)
	}
	if goMainWithoutModule(srcDir) {
		return "add a go.mod: run go mod init <name> in this directory"
	}
	if entries, err := os.ReadDir(srcDir); err == nil && len(visibleEntries(entries)) == 0 {
		return "deploy the directory that holds the app: this one is empty"
	}
	// Only the starting files this tree does not already hold: a hint naming a
	// file the author has is the dead end it exists to prevent. Node and Go
	// start from their manifests, not from a file of a known name, so those are
	// named instead — server.js and main.go offered alone sent each to the
	// other (BB-19).
	var starts []string
	for _, name := range []string{"app.py", "app.R"} {
		if !fileExists(filepath.Join(srcDir, name)) {
			starts = append(starts, name)
		}
	}
	starts = append(starts, `a package.json with a "start" script`, "a go.mod with a main package")
	return fmt.Sprintf("add what an app starts from (%s) or an index.html, or declare [batch] for an app that only runs jobs", strings.Join(starts, ", "))
}

// notebookHint is the fix for a notebook that cannot be served.
func notebookHint(err error) string {
	var ne NotebookError
	isNE := errors.As(err, &ne)
	if isNE && ne.Field == "engine" && ne.Framework == FrameworkRMarkdown {
		return `write engine = "knitr" under [notebook] — an .Rmd is knitted — or remove the key`
	}
	if isNE && ne.Field == "engine" {
		return `write engine = "nbconvert", "quarto", "knitr" or "jupyter" under [notebook], or remove it to take the file's default`
	}
	if isNE && ne.Framework == FrameworkMarimo {
		return "keep one marimo notebook at the repository root and move the others out"
	}
	if strings.Contains(err.Error(), "marimo") {
		return "deploy the marimo notebook and the other documents as two apps"
	}
	return `name the document to serve in toft.toml: [notebook] file = "<name>"`
}

func visibleEntries(entries []os.DirEntry) []os.DirEntry {
	var out []os.DirEntry
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") && e.Name() != ManifestFile {
			out = append(out, e)
		}
	}
	return out
}

// reasonHint is the last fallback: a hint for a problem's reason, when
// neither the refusal nor its code named one.
func reasonHint(reason string) string {
	switch deploysteps.Reason(reason) {
	case deploysteps.ReasonStorageRefused:
		return "fix the [[storage]] entry the message names, or remove it"
	case deploysteps.ReasonRuntimeVersion:
		return "pin a release line this install has — the message names them"
	}
	return "fix what the message names in " + ManifestFile + " or the tree"
}

// completeProblems gives every problem a code (its reason, when it set none)
// and a hint.
func (p *Preflight) completeProblems() {
	for i := range p.Problems {
		pp := &p.Problems[i]
		if pp.Code == "" {
			pp.Code = pp.Reason
		}
		if pp.Hint == "" {
			pp.Hint = codeHint(pp.Code)
		}
		if pp.Hint == "" {
			pp.Hint = reasonHint(pp.Reason)
		}
	}
}

// runtimeVersionHint is the fix when no interpreter here satisfies what the
// release asks for. "Remove the pin" is only offered for a [runtime] pin that
// exists: a requires-python with no [runtime] table is no pin to remove.
func runtimeVersionHint(srcDir, lang string, mode runtime.Mode) string {
	// One line this install has, written out to paste — "the message names
	// them" sent the reader back to the sentence beside it (BB-013).
	line := exampleLine(lang)
	if set := EffectiveVersions(lang, mode); len(set) > 0 && set[0].Series() != "" {
		line = set[0].Series()
	}
	if pins, err := ParseRuntimePins(srcDir); err == nil && pins[lang] != "" {
		return fmt.Sprintf("pin a release line this install has, like %s = %q under [runtime], or remove the pin to take the default", lang, line)
	}
	if lang == "python" {
		return fmt.Sprintf(`widen requires-python in pyproject.toml to include a version this install has, or pin one under [runtime], like python = %q`, line)
	}
	return fmt.Sprintf("pin a release line this install has under [runtime], like %s = %q", lang, line)
}
