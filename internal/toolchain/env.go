package toolchain

import (
	"os"
	"slices"
	"strings"
)

// The operator's toolchain environment, and the one allow-list that carries it.
//
// Two lists used to exist. `internal/deps`'s resolveEnv passed GOPROXY, the
// module cache variables and all six proxy variables to the commands that
// *read* an app's dependency manifest; the builder passed none of them to the
// commands that *build* the app. Scanning an app's dependencies could traverse
// a corporate proxy and building the same tree, minutes later, could not — a
// build that reached out to proxy.golang.org and hung, on an install where
// every other outbound call was configured correctly.
//
// So the list lives here, in a package with no dependencies of its own, and
// both readers take it from the same place. A variable added for one is added
// for the other by construction, which is the only thing that stops the two
// drifting again.
//
// **What is on it is the operator's, never the app's** (ADR §64 D15). These
// are read from the platform process's own environment — the environment
// whoever installed Toft controls — and an app's declared variables never
// reach this list. An app says which language version it needs; it does not
// say where the module proxy is or what flags a compiler runs under.
//
// **GOOS and GOARCH are deliberately absent, and their absence is enforced
// rather than assumed** (D16). An allow-list already excludes them, but see
// RefuseCrossCompile: an app that sets them in its *own* environment would
// otherwise produce a binary that passes every check in the deploy and dies at
// exec on the host it was built for.
var passthroughKeys = []string{
	// Go's module machinery. GOPATH, GOCACHE and GOMODCACHE are overridden by
	// the builder with platform-owned cache directories; they are here for
	// `internal/deps`, which has no cache dir of its own to impose.
	"GOPATH", "GOCACHE", "GOMODCACHE",
	"GOFLAGS", "GOPROXY", "GOPRIVATE", "GOSUMDB", "GONOSUMDB", "GONOSUMCHECK",
	"GOINSECURE",
	// GOTOOLCHAIN is this feature's own reason for existing on the build side:
	// `auto` lets a go.mod requiring a newer toolchain fetch it from the module
	// proxy, `local` gets Go's own refusal naming both versions. Either is a
	// decision; inheriting whatever the process happened to start with is not.
	"GOTOOLCHAIN",
	// Both spellings, because tools disagree about which they read and an
	// operator should not have to know which one this build shells out to.
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	// npm and uv's own network configuration. Same argument as GOPROXY: an
	// install behind a registry mirror configures it once, in the environment,
	// and every tool that fetches gets it.
	"NPM_CONFIG_REGISTRY", "npm_config_registry",
	"UV_INDEX_URL", "UV_EXTRA_INDEX_URL", "UV_DEFAULT_INDEX",
	// The credential half of the same sentence. A mirror an enterprise runs is
	// usually authenticated, and an install that could name it but not sign in
	// to it could not build those apps at all.
	//
	// NETRC is the one that covers the two languages with no variable of their
	// own: Go reads it for module fetches over HTTPS, and R's libcurl reads it
	// for a package repository. It names a *file*, which is the point — the
	// operator mounts a credential file and names it here, and nothing about
	// its contents passes through this process.
	"NPM_TOKEN", "NETRC",
	// uv's keyring integration, for an index whose credential is not a static
	// token. Off unless the operator asks for it; naming it here is what lets
	// them.
	"UV_KEYRING_PROVIDER",
	// Certificate bundles, which are what a TLS-terminating corporate proxy
	// needs and the single most common reason a build behind one fails.
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE",
	"CURL_CA_BUNDLE", "GIT_SSL_CAINFO",
}

// passthroughPrefixes are carried for the same reason as passthroughKeys and
// cannot be spelled out, because the tools that read them build the variable
// name out of a registry URL or an index's name.
//
// npm's per-registry credential is a config key that *is* a URL —
// `//registry.example.com/:_authToken` — reached from the environment as
// `npm_config_//registry.example.com/:_authToken`. uv's is
// `UV_INDEX_<NAME>_USERNAME` / `_PASSWORD`, where NAME is whatever the operator
// called the index. Neither can appear on a fixed list, and an install that
// could not pass them would be one where the *unauthenticated* half of this
// feature works and the authenticated half does not.
//
// **This widens what the operator may set and not who may set it.** These are
// still read from the platform process's own environment, and an app's declared
// variables still never reach this list (ADR §64 D15) — which is the property
// that matters, because these carry credentials.
//
// The prefixes are deliberately narrow. `UV_` on its own would capture
// UV_CACHE_DIR and let an operator's environment fight the platform for a cache
// directory the platform owns and accounts for.
var passthroughPrefixes = []string{
	"npm_config_", "NPM_CONFIG_",
	"UV_INDEX_",
}

// crossCompileKeys are refused rather than passed. See RefuseCrossCompile.
var crossCompileKeys = []string{"GOOS", "GOARCH"}

// Passthrough returns the operator's toolchain and proxy environment.
//
// Only variables that are actually set are returned, so an unset one stays
// unset in the child rather than becoming an empty string — which is a
// different thing to every tool on this list. GOPROXY="" means "no proxy, fail
// offline"; GOPROXY absent means "the default".
func Passthrough() map[string]string {
	out := make(map[string]string, len(passthroughKeys))
	for _, k := range passthroughKeys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			out[k] = v
		}
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" || !hasPassthroughPrefix(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// hasPassthroughPrefix reports whether a variable is carried by a prefix rule.
func hasPassthroughPrefix(key string) bool {
	for _, p := range passthroughPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// PassthroughSlice is Passthrough in the KEY=VALUE form exec wants, appended to
// base. Deduplication is left to exec: for a repeated key the last wins, so a
// caller may append its own overrides after this.
func PassthroughSlice(base []string) []string {
	for k, v := range Passthrough() {
		base = append(base, k+"="+v)
	}
	return base
}

// RefuseCrossCompile reports the cross-compilation variables present in an
// app's own environment, if any.
//
// A Go app that sets GOOS=windows builds a binary that satisfies every check
// this platform makes — the build exits zero, the artifact is published, the
// release directory holds a file with the right name — and then dies at exec
// with a message about an exec format error, on a workload nobody is watching.
// ADR §44 records the same failure from the other direction, where an image
// built for one architecture was pulled onto a node of another.
//
// The variables are stripped from every build regardless (they are not on the
// allow-list and an app's own variables are merged after it). This exists so a
// Go build *says so* rather than quietly ignoring a variable somebody set on
// purpose.
func RefuseCrossCompile(env map[string]string) []string {
	var found []string
	for _, k := range crossCompileKeys {
		if _, ok := env[k]; ok {
			found = append(found, k)
		}
	}
	return found
}

// installerConfigPrefixes name the variables by which uv, pip, npm, go and
// renv take their configuration from the environment: where to resolve from,
// which resolution strategy, which overrides and constraints, which lockfile,
// which workspace.
var installerConfigPrefixes = []string{
	"UV_", "PIP_", "npm_config_", "NPM_CONFIG_", "RENV_", "R_LIBS",
}

// installerConfigKeys are the installer-configuration names without a prefix.
// Go's are spelled out — `GO` as a prefix would take GOOGLE_MAPS_KEY with it.
var installerConfigKeys = []string{
	"GOFLAGS", "GOWORK", "GOPROXY", "GONOPROXY", "GOPRIVATE", "GOSUMDB", "GONOSUMDB",
	"GONOSUMCHECK", "GOINSECURE", "GOVCS", "GOPATH", "GOCACHE", "GOMODCACHE", "GOENV",
	"GOROOT", "GOTOOLCHAIN", "GOEXPERIMENT",
	"NETRC", "PYTHONPATH", "PYTHONUSERBASE",
}

// IsInstallerConfig reports a variable name that configures an installer.
//
// **The judged set must be the installed set** (ADR §64). The dependency scan
// resolves under the operator's environment and none of the app's; the build's
// `uv pip install`, `npm ci`, `go build` and `renv::restore()` used to run
// under the app's own variables too, so `UV_RESOLUTION=lowest-direct`,
// `UV_OVERRIDE=…`, `GOFLAGS=-modfile=…` or `GOWORK` declared as an app
// variable changed what was installed without changing what was judged or
// inventoried (code review 2026-11-11, E-1). An app's declared variable under
// one of these names is therefore kept out of the build environment — the
// operator's own value for it still passes through (Passthrough), and the
// app's process still receives it at run time, where no installer reads it.
func IsInstallerConfig(name string) bool {
	if slices.Contains(installerConfigKeys, name) {
		return true
	}
	for _, p := range installerConfigPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// InstallerConfigNames are the names in an app's environment that
// IsInstallerConfig keeps out of a build, sorted, for the build log.
func InstallerConfigNames(env map[string]string) []string {
	var out []string
	for k := range env {
		if IsInstallerConfig(k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// StripCrossCompile removes the cross-compilation variables from a build
// environment. Called for every framework, because GOOS in a Python app's
// environment is merely meaningless rather than harmful and there is no reason
// to carry it into a child.
func StripCrossCompile(env map[string]string) {
	for _, k := range crossCompileKeys {
		delete(env, k)
	}
}
