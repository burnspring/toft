package builder

import (
	"errors"
	"fmt"

	"toft/internal/staticserve"
)

// describeStaticRelease is the whole of a static app's build step.
//
// There is nothing to install and nothing to compile, so what a static build
// does is *settle and state* what it is about to publish: the document root,
// the entry document, and how many files the two of them add up to. That last
// number is the one an operator wants, because "the release is the site" — a
// repo whose `dist/` was not committed and whose root is therefore the checkout
// publishes everything in the checkout, and the refusal list is the only thing
// standing between a `.git/` and the internet.
//
// It is also the last point at which a declaration that is *right about intent
// and wrong about this release* can be refused cheaply. `[static] root = "dist"`
// on a repo whose dist/ is produced by a build the platform is not running is
// a deploy failure naming the missing directory, rather than an app whose front
// page is a 404 forever.
func describeStaticRelease(releaseDir string, appendLog func(string)) error {
	cfg, err := staticserve.Resolve(releaseDir)
	if err != nil {
		// A wrongly typed key said as toft check says it, not in the
		// decoder's Go terms (D-27, fourth 2026-11-03 black-box list).
		var de staticserve.DeclarationError
		if errors.As(err, &de) {
			return staticDeclarationProblem(releaseDir, de)
		}
		return err
	}

	root := cfg.RootRel
	if root == "." {
		root = "the repository root"
		appendLog("==> Serving " + root)
		// Worth saying once, in the place somebody is looking: this is the case
		// where the refusal list is load-bearing.
		appendLog("    Dotfiles (.git, .env), " + staticserve.ManifestFile +
			" and the process-manager entry are never served.")
	} else {
		appendLog("==> Serving " + root + "/")
	}
	appendLog(fmt.Sprintf("    %s, entry document %s%s",
		plural(cfg.Files, "1 file", fmt.Sprintf("%d files", cfg.Files)),
		cfg.Entry, staticFallbackNote(cfg.Fallback)))

	for _, line := range staticserve.Lint(releaseDir, cfg.RootRel) {
		appendLog(line)
	}
	return nil
}

func staticFallbackNote(on bool) string {
	if on {
		return ", with SPA fallback on"
	}
	return ""
}
