package runner

import (
	"regexp"
	"slices"
)

// The names an app may give its own environment variables, stated once for
// every writer: the API's create and edit routes, the Connect environment
// route, the gallery's deploy and the builder's .env seeding.
//
// Here because the runner is the one launcher in both modes, so the list sits
// beside the code that actually sets these names and is held against it by a
// test in this package.

// envNameRe accepts what a POSIX environment accepts, which is what a shell can
// export and what os.Getenv in every language this platform runs can read back.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvName reports whether name can be an environment variable at all.
func ValidEnvName(name string) bool { return envNameRe.MatchString(name) }

// PlatformEnvNames are the variables the platform sets in an app's process
// over the app's own, in both execution modes — so a value stored under one of
// them never reaches the app, and accepting it would be accepting nothing.
// **The list is the specification**: `TestPlatformEnvNamesCoverAppEnv` walks
// the launcher and fails when it sets a name that is neither on it nor on its
// short list of names the app's own value still matters for.
//
// That second list is why PATH, HOME, PYTHONPATH, NODE_ENV and FLASK_APP are
// not here. PATH is *prepended to* — the app's own is the base a release's
// interpreter is put in front of. HOME is set only under a per-app account and
// otherwise inherited. The other three are defaults (appEnv): set only when the
// app gave none, so the app's own value is the one that arrives.
var PlatformEnvNames = []string{
	// The address and the mount, the three every service is given.
	"PORT", "HOST", "BASE_PATH",
	// The app's private scratch, wiped on every start.
	"TMPDIR", EnvScratchDir,
	// The environment and the library the release was built against.
	"VIRTUAL_ENV", "R_LIBS_SITE",
	// The app's identity and the platform's address, for the SDK.
	EnvAPIURL, EnvAppToken, EnvAppID, EnvAppSlug, EnvRoleKey, EnvStorage,
	// A job's contract.
	EnvRunToken, EnvRunID, EnvJobName, EnvTrigger, EnvTriggerRole,
	EnvOutputURI, EnvScheduledFor, EnvParams, EnvRerunOf, EnvDeadline,
}

// IsPlatformEnvName reports whether the platform sets name itself.
func IsPlatformEnvName(name string) bool { return slices.Contains(PlatformEnvNames, name) }

// The two refusals' codes, stable on the wire beside the message.
const (
	EnvNameInvalidCode  = "invalid_env_name"
	EnvNameReservedCode = "reserved_env_name"
)

// EnvNameProblem is why name cannot be one of an app's own variables, as a
// stable code and a sentence, or two empty strings when it can.
func EnvNameProblem(name string) (code, message string) {
	switch {
	case !ValidEnvName(name):
		return EnvNameInvalidCode, "\"" + name + "\" is not a valid environment variable name: " +
			"use letters, digits and underscores, not starting with a digit"
	case IsPlatformEnvName(name):
		return EnvNameReservedCode, name + " is set by the platform for every app, " +
			"so a value stored here would never reach the app. Use a different name"
	}
	return "", ""
}
