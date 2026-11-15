package builder

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"toft/internal/runtime"
)

// A batch app is an app with no service (ADR §107): a release that installs its
// dependencies like the language family it is written in and starts nothing.
// Everything it does is a job.
//
// **Declared, never inferred.** A Go `main`, a Python `main.py` and a Node
// `start` script are exactly what the service detectors read as a web app, so a
// batch program in those shapes cannot be told apart from one by looking — and a
// guess that is wrong is a green deploy and a crash loop. The declaration is a
// `[batch]` table in toft.toml, read here in detection for the reason
// `[static]` is: `Builder.Start` re-runs detection against the release at every
// wake, restore and boot, and a declaration only the build read would be
// overwritten by the first of them.
//
// **Four framework values rather than one plus a language column**, because the
// image a workload runs in (`kube.languageFamily`) and the toolchain a build
// resolves (`LanguageOf`) are both pure functions of the framework string, and
// one column read beside it would put that decision in two places.
const (
	FrameworkBatchNode   Framework = "batch-node"
	FrameworkBatchPython Framework = "batch-python"
	FrameworkBatchGo     Framework = "batch-go"
	FrameworkBatchR      Framework = "batch-r"
)

// ErrNoService is the refusal to start, stop or restart a batch app's service,
// which it does not have.
var ErrNoService = errors.New("this is a batch app: it runs jobs and serves nothing, so there is no service to start, stop or restart")

// BatchFrameworks is every batch value, in the order the tag lists them.
var BatchFrameworks = []Framework{FrameworkBatchNode, FrameworkBatchPython, FrameworkBatchGo, FrameworkBatchR}

// IsBatch reports whether f serves nothing. Answered from the string so that a
// caller holding only `apps.framework` asks the same question.
func (f Framework) IsBatch() bool { return IsBatchFramework(string(f)) }

// IsBatchFramework is IsBatch for a framework that arrived as a string.
func IsBatchFramework(f string) bool { return strings.HasPrefix(f, "batch-") }

// BatchLanguages maps a `[batch] language` value to its framework. The list is
// the specification: these are the families both execution modes carry.
var BatchLanguages = map[string]Framework{
	"node":   FrameworkBatchNode,
	"python": FrameworkBatchPython,
	"go":     FrameworkBatchGo,
	"r":      FrameworkBatchR,
}

// batchDeclaration is what `[batch]` says.
type batchDeclaration struct {
	Present  bool
	Language string
}

func readBatchDeclaration(srcDir string) batchDeclaration {
	data, err := readInRelease(srcDir, ManifestFile)
	if err != nil {
		return batchDeclaration{}
	}
	var m struct {
		Batch struct {
			Language string `toml:"language"`
		} `toml:"batch"`
	}
	md, err := toml.Decode(string(data), &m)
	if err != nil || !md.IsDefined("batch") {
		return batchDeclaration{}
	}
	return batchDeclaration{Present: true, Language: strings.ToLower(strings.TrimSpace(m.Batch.Language))}
}

// batchLanguagesIn names the families this release carries a manifest for, in a
// fixed order — what an undeclared `language` is inferred from.
func batchLanguagesIn(srcDir string) []string {
	var found []string
	if fileExists(filepath.Join(srcDir, "package.json")) {
		found = append(found, "node")
	}
	if fileExists(filepath.Join(srcDir, "go.mod")) {
		found = append(found, "go")
	}
	if pythonProjectFile(srcDir) != "" {
		found = append(found, "python")
	}
	if fileExists(filepath.Join(srcDir, "renv.lock")) || repositoryLooksLikeR(srcDir) {
		found = append(found, "r")
	}
	return found
}

// batchLanguage resolves which family a batch release is, or says why it
// cannot: a declared language the release has no manifest for, two languages
// and no declaration, or none at all.
func batchLanguage(srcDir string, decl batchDeclaration) (Framework, error) {
	found := batchLanguagesIn(srcDir)
	if decl.Language != "" {
		fw, ok := BatchLanguages[decl.Language]
		if !ok {
			names := make([]string, 0, len(BatchLanguages))
			for k := range BatchLanguages {
				names = append(names, k)
			}
			sort.Strings(names)
			return FrameworkUnsupported, fmt.Errorf("[batch] language %q is not one of %s",
				decl.Language, strings.Join(names, ", "))
		}
		for _, l := range found {
			if l == decl.Language {
				return fw, nil
			}
		}
		return FrameworkUnsupported, fmt.Errorf("[batch] language is %q, and this release carries no "+
			"%s project file (%s)", decl.Language, decl.Language, batchProjectFiles[decl.Language])
	}
	switch len(found) {
	case 1:
		return BatchLanguages[found[0]], nil
	case 0:
		return FrameworkUnsupported, fmt.Errorf("[batch] found no project to install: add a " +
			"package.json, go.mod, requirements.txt or pyproject.toml, or renv.lock")
	default:
		return FrameworkUnsupported, fmt.Errorf("[batch] found %s projects here; say which this is "+
			"with language = %q (or another of them)", strings.Join(found, " and "), found[0])
	}
}

var batchProjectFiles = map[string]string{
	"node":   "package.json",
	"go":     "go.mod",
	"python": "requirements.txt or pyproject.toml",
	"r":      "renv.lock or R files",
}

// detectBatch is detection's answer for a declared batch release. No entrypoint
// is demanded — that is the whole difference from the service detectors — and Go
// is the one family that names one, because a Go job runs the binary the build
// produced.
func detectBatch(srcDir string, decl batchDeclaration) DetectionResult {
	fw, err := batchLanguage(srcDir, decl)
	if err != nil {
		return DetectionResult{Framework: FrameworkUnsupported}
	}
	switch fw {
	case FrameworkBatchNode:
		return DetectionResult{Framework: fw, HasBuild: hasBuildScript(srcDir)}
	case FrameworkBatchGo:
		if pkg := goMainPackage(srcDir); pkg != "" {
			return DetectionResult{Framework: fw, Entrypoint: pkg}
		}
		return DetectionResult{Framework: fw}
	}
	return DetectionResult{Framework: fw}
}

// BatchRefusal is why a release that declares `[batch]` cannot be built, or "".
// Detection cannot refuse — it answers a framework — so the deploy asks this
// beside UnsupportedMessage and says the batch-specific sentence instead.
func BatchRefusal(srcDir string) string {
	decl := readBatchDeclaration(srcDir)
	if !decl.Present {
		return ""
	}
	if _, err := batchLanguage(srcDir, decl); err != nil {
		return err.Error()
	}
	return ""
}

// ErrAppLocked is the refusal to start a workload for a locked app. A lock
// withholds starting, deploying and changing, and it is checked where a
// workload is started as well as where a request is admitted: a deploy queued
// before the lock, or a boot reconcile, would otherwise run the app under it.
var ErrAppLocked = runtime.ErrAppLocked

// ErrStartSuperseded is a start asked for a release that is no longer the
// app's current one: a deploy flipped between the request and the start lock.
// Nothing is started, and nothing is wrong (code review 2026-11-11, F-5).
var ErrStartSuperseded = runtime.ErrStartSuperseded
