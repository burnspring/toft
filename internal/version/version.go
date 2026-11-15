// Package version records the published version of this Toft build.
//
// It is deliberately a compiled-in constant rather than something derived from
// VCS metadata at build time: a binary built from a source tarball, or inside
// a Docker context that excludes .git, has no revision to stamp and would
// otherwise have nothing to report. The revision, when there is one, is
// additional detail — never the answer to "what version is this?".
package version

import "runtime/debug"

// Version is the published version of this binary. Bump it here when cutting
// a release: it is the single source of truth, and the five other places that
// carry the number derive from it and are guarded against it — the chart's
// appVersion, the VERSION_DEFAULT in both published installers, the
// TOFT_VERSION in the compose bundle's .env.example, and the tag
// scripts/publish-image.sh pushes. They move in the same commit, because a
// release that disagrees with itself is what those guards exist to stop.
//
// A build that is not a release — a nightly, a vendor rebuild, a patched
// fork — may override it without editing source:
//
//	go build -ldflags="-X toft/internal/version.Version=0.3.0-rc1" .
//
// which is why this is a var and not a const.
var Version = "0.10.0"

// Revision reports the VCS commit this binary was built from, suffixed
// "-dirty" when the tree had uncommitted changes. It is "" when the build
// carried no VCS stamp (`go run`, tests, a build from an exported tree), which
// is a normal build, not a broken one — callers render it as absent rather
// than substituting a placeholder.
func Revision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// String is the version with the revision appended when one is known —
// "0.1.0 (a1b2c3d4e5f6)" — for logs and anywhere one string has to carry both.
func String() string {
	if rev := Revision(); rev != "" {
		return Version + " (" + rev + ")"
	}
	return Version
}
