package runtime

import (
	"os"
	"strings"
)

// baseEnvKeys is the full set of platform environment variables a child
// process (the shared-mode agent, a deployed app, a build step) is allowed to
// inherit.
// Everything else — JWT_SECRET, ENCRYPTION_KEY, GITHUB_CLIENT_SECRET,
// DATABASE_URL, … — must never reach a child: a deployed app that can read
// JWT_SECRET can forge a system_admin session.
//
// This is an *allow-list*, and that is what makes it hold for variables nobody
// had thought of when it was written. OTEL_EXPORTER_OTLP_HEADERS is the case
// that made the distinction worth restating (ADR §31 D8): it carries the
// operator's collector credential, and an app that inherited it could write
// into the operator's observability stack under the platform's identity,
// forever, without anything about it looking wrong. No OTEL_ strip is needed
// or wanted here — a deny-list would be the thing that eventually misses one —
// but TestBaseEnvStripsOTEL asserts the outcome, because "the mechanism implies
// it" is not the same as "a test says so".
var baseEnvKeys = []string{"PATH", "HOME", "LANG", "TMPDIR", "USER", "SHELL"}

// BaseEnv returns the minimal environment for spawning child processes.
// Use this instead of os.Environ() at every exec site that runs the agent, a
// build tool, or git. App-specific variables are appended by the caller.
//
// LC_* locale variables pass through in addition to baseEnvKeys.
func BaseEnv() []string {
	out := make([]string, 0, len(baseEnvKeys)+4)
	for _, k := range baseEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LC_") {
			out = append(out, kv)
		}
	}
	if os.Getenv("LANG") == "" && os.Getenv("LC_ALL") == "" {
		out = append(out, "LANG="+DefaultLang)
	}
	return out
}

// DefaultLang is the locale a child gets when the platform itself was started
// with none, which the image is. Without it R runs in the C locale and prints
// every non-ASCII character of a message — the platform's em dashes included —
// as <U+2014>. glibc has carried C.UTF-8 built in since 2.35.
const DefaultLang = "C.UTF-8"
