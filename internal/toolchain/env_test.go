package toolchain

import (
	"os"
	"strings"
	"testing"
)

// The defect this package's env half exists to close: the commands that read an
// app's dependency manifest could traverse a corporate proxy and the commands
// that build the same tree could not. One list, so the two cannot drift again.
func TestPassthroughCarriesTheProxyAndModuleEnvironment(t *testing.T) {
	for k, v := range map[string]string{
		"GOPROXY":     "https://artifactory.example/api/go/go",
		"GOTOOLCHAIN": "local",
		"HTTPS_PROXY": "http://proxy.example:3128",
		"no_proxy":    "example.internal",
	} {
		t.Setenv(k, v)
	}

	env := Passthrough()
	for _, k := range []string{"GOPROXY", "GOTOOLCHAIN", "HTTPS_PROXY", "no_proxy"} {
		if env[k] == "" {
			t.Errorf("%s did not survive Passthrough — a build behind a proxy cannot reach anything without it", k)
		}
	}
}

// An unset variable must stay unset rather than arrive empty: to every tool on
// this list the two mean different things. GOPROXY="" is "fail offline";
// GOPROXY absent is "use the default".
func TestPassthroughOmitsWhatIsNotSet(t *testing.T) {
	t.Setenv("GOPRIVATE", "")
	if _, ok := Passthrough()["GOPRIVATE"]; ok {
		t.Error("an empty GOPRIVATE was passed through; absent and empty are different instructions")
	}
}

// D16, at the level a list can hold it: the allow-list itself must not name the
// cross-compilation variables, whatever else it grows.
func TestCrossCompileIsNotOnTheAllowList(t *testing.T) {
	t.Setenv("GOOS", "windows")
	t.Setenv("GOARCH", "arm64")
	env := Passthrough()
	for _, k := range []string{"GOOS", "GOARCH"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s is on the passthrough allow-list; a cross-compiled binary passes "+
				"every check in a deploy and dies at exec", k)
		}
	}
}

func TestRefuseAndStripCrossCompile(t *testing.T) {
	env := map[string]string{"GOOS": "windows", "DATABASE_URL": "x"}
	found := RefuseCrossCompile(env)
	if len(found) != 1 || found[0] != "GOOS" {
		t.Fatalf("RefuseCrossCompile = %v, want [GOOS]", found)
	}
	StripCrossCompile(env)
	if _, ok := env["GOOS"]; ok {
		t.Error("GOOS survived StripCrossCompile")
	}
	if env["DATABASE_URL"] != "x" {
		t.Error("StripCrossCompile removed something that was not its business")
	}
}

func TestPassthroughSliceAppends(t *testing.T) {
	t.Setenv("GOFLAGS", "-mod=mod")
	got := PassthroughSlice([]string{"PATH=" + os.Getenv("PATH")})
	var seen bool
	for _, kv := range got {
		if kv == "GOFLAGS=-mod=mod" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("GOFLAGS missing from %v", got)
	}
	if !strings.HasPrefix(got[0], "PATH=") {
		t.Error("PassthroughSlice did not preserve the base it was given, in order")
	}
}

// A registry mirror an enterprise runs is usually authenticated, and the
// credential's variable name cannot be spelled out in advance: npm builds it
// out of the registry URL and uv out of the index's name. Both must survive.
func TestPassthroughCarriesRegistryCredentials(t *testing.T) {
	t.Setenv("npm_config_//registry.internal/:_authToken", "npm-secret")
	t.Setenv("UV_INDEX_INTERNAL_USERNAME", "svc-build")
	t.Setenv("UV_INDEX_INTERNAL_PASSWORD", "uv-secret")
	t.Setenv("NPM_TOKEN", "token-secret")
	t.Setenv("NETRC", "/etc/toft/netrc")

	env := Passthrough()
	for k, want := range map[string]string{
		"npm_config_//registry.internal/:_authToken": "npm-secret",
		"UV_INDEX_INTERNAL_USERNAME":                 "svc-build",
		"UV_INDEX_INTERNAL_PASSWORD":                 "uv-secret",
		"NPM_TOKEN":                                  "token-secret",
		"NETRC":                                      "/etc/toft/netrc",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q — an app built against an authenticated mirror cannot build without it", k, env[k], want)
		}
	}
}

// The prefixes are narrow on purpose. A bare `UV_` would carry UV_CACHE_DIR,
// and the platform owns the cache directories because it accounts for them.
func TestPassthroughPrefixesDoNotReachThePlatformsOwnCaches(t *testing.T) {
	t.Setenv("UV_CACHE_DIR", "/home/someone/.cache/uv")
	t.Setenv("UV_LINK_MODE", "copy")

	env := Passthrough()
	for _, k := range []string{"UV_CACHE_DIR", "UV_LINK_MODE"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s was carried; the prefix rule is meant to reach index credentials, not everything uv reads", k)
		}
	}
}

// PassthroughSlice and Passthrough are one list read twice. They drifted once
// already — that is why this package exists — and the prefix rule is a second
// chance to make the same mistake.
func TestBothReadersAgreeOnThePrefixRule(t *testing.T) {
	t.Setenv("npm_config_//registry.internal/:_authToken", "npm-secret")

	slice := PassthroughSlice(nil)
	found := false
	for _, kv := range slice {
		if kv == "npm_config_//registry.internal/:_authToken=npm-secret" {
			found = true
		}
	}
	if !found {
		t.Errorf("PassthroughSlice dropped what Passthrough carried: %v", slice)
	}
}
