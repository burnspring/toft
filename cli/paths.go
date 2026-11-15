package main

import (
	"regexp"
	"strings"
)

// Paths a caller types, and the one thing that reliably rewrites them before
// this binary ever sees them.
//
// **A POSIX-looking argument does not survive Git Bash on Windows.** MSYS2 —
// which is what `toft.exe` runs under when an agent shells out through Git Bash,
// the default shell Claude Code and most CI images use there — rewrites an
// argument that looks like a Unix path into a Windows one before exec:
//
//	toft api GET /api/v1/apps        →  toft.exe api GET C:/Program Files/Git/api/v1/apps
//	toft task create --path /healthz →  --path C:/Program Files/Git/healthz
//
// Nothing in the conversion is visible to the program. The value arrives
// looking like something the caller typed, so every refusal downstream is a
// refusal of a string nobody wrote — and the two the 0.6.0 audit hit were the
// two worst available: `toft api` sent it to the install, which resolved no route
// and answered as though the *credential* were the problem, and `toft task
// create` sent it to the platform's path validator, which correctly refused a
// path with a drive letter in it and said "path must be app-relative, e.g.
// /jobs/cleanup" — while the caller was staring at exactly that.
//
// So it is named here, before the request. The alternative — a better message
// from the platform — cannot work: by then the leading slash is gone and the
// value is indistinguishable from a caller who meant it.
//
// **Detected rather than undone.** Reversing the conversion means guessing
// which prefix the shell used and which of its segments were the caller's,
// which is a guess that would silently send the wrong request; naming it is a
// refusal the caller can act on in one edit. MSYS_NO_PATHCONV=1 is the fix on
// their side, and a relative path is the fix that needs nothing.

// windowsAbsolute matches a native Windows path: a drive letter and a
// separator, in either slash. That is the shape the conversion produces and a
// shape no path on this platform ever legitimately has.
var windowsAbsolute = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// rewrittenByTheShell reports whether a path arrived as a native Windows path
// rather than as the one the caller typed.
//
// A backslash counts on its own: every path this client sends is a URL path,
// and a URL path has no backslashes in it, so one is either this conversion or
// a caller who needs telling the same thing.
func rewrittenByTheShell(path string) bool {
	return windowsAbsolute.MatchString(path) || strings.Contains(path, `\`)
}

// shellRewroteIt is the refusal, in the words of the fix rather than of the
// diagnosis. It names what arrived, because the caller's own command line does
// not contain it and comparing the two is the whole "oh" of this failure.
func shellRewroteIt(what, got, relative string) error {
	return usageCodef("invalid_path", "%s arrived as %q, which is a Windows path and not a path on your "+
		"install — a POSIX-looking argument is rewritten by Git Bash and MSYS shells "+
		"before %s ever sees it.\n"+
		"    Use the relative form, which is never rewritten: %s\n"+
		"    Or run the shell with MSYS_NO_PATHCONV=1 to keep the leading slash.",
		what, got, "toft", relative)
}
