package builder

import (
	"strings"
	"testing"

	"toft/internal/toolchain"
)

// An enterprise builds against an internal mirror with a token, so the token has
// to reach the build. What it must never reach is the *app*: the deployed
// workload's environment is the app author's own variables, and a package
// registry credential belongs to whoever installed Toft.
//
// The two environments are assembled in different places — `buildEnvironment`
// for the build commands, `envVars` straight from the database for the workload
// spec — and the property is a consequence of that rather than of a filter, so
// this is what says the consequence is intended.
func TestARegistryCredentialReachesTheBuildAndNotTheApp(t *testing.T) {
	t.Setenv("npm_config_//registry.internal/:_authToken", "npm-secret")
	t.Setenv("NPM_TOKEN", "token-secret")
	t.Setenv("UV_INDEX_INTERNAL_PASSWORD", "uv-secret")

	b := &Builder{}
	appEnv := map[string]string{"DATABASE_URL": "postgres://app"}
	buildEnv := b.buildEnvironment(appEnv, "/apps/demo")

	for _, k := range []string{
		"npm_config_//registry.internal/:_authToken",
		"NPM_TOKEN",
		"UV_INDEX_INTERNAL_PASSWORD",
	} {
		if buildEnv[k] == "" {
			t.Errorf("%s never reached the build; an app on an authenticated mirror cannot build", k)
		}
	}

	// The app's own environment is untouched by any of it. This is the map the
	// workload spec carries, and a credential landing in it would be readable
	// by app code for the life of the deployment.
	for k, v := range appEnv {
		if k != "DATABASE_URL" || v != "postgres://app" {
			t.Fatalf("the app's environment was mutated: %v", appEnv)
		}
	}
	for _, secret := range []string{"npm-secret", "token-secret", "uv-secret"} {
		for k, v := range appEnv {
			if strings.Contains(v, secret) {
				t.Errorf("a build credential reached the app's own environment at %s", k)
			}
		}
	}
}

// The operator's credential beats an app that tries to set the same variable,
// for the reason the whole passthrough exists: where this install fetches from
// is not an app's decision, and neither is what it signs in with.
func TestAnAppCannotOverrideTheOperatorsRegistryCredential(t *testing.T) {
	t.Setenv("NPM_TOKEN", "operators-token")

	b := &Builder{}
	buildEnv := b.buildEnvironment(map[string]string{"NPM_TOKEN": "the-apps-token"}, "/apps/demo")

	if buildEnv["NPM_TOKEN"] != "operators-token" {
		t.Errorf("NPM_TOKEN = %q, want the operator's — an app that can redefine the registry credential "+
			"can point the build at a registry it controls", buildEnv["NPM_TOKEN"])
	}
}

// An app's own variable that configures an installer never reaches the build:
// the scan resolves without it, so an install that honoured it would install a
// set nobody judged (code review 2026-11-11, E-1). The operator's value for the
// same name still passes through, and an ordinary app variable still arrives.
func TestAnAppsInstallerConfigurationIsKeptOutOfTheBuild(t *testing.T) {
	t.Setenv("UV_INDEX_URL", "https://mirror.internal/simple")
	b := &Builder{}
	appEnv := map[string]string{
		"UV_RESOLUTION": "lowest-direct", "UV_OVERRIDE": "overrides.txt", "UV_INDEX_URL": "https://evil.example/simple",
		"PIP_INDEX_URL": "https://evil.example/simple", "npm_config_registry": "https://evil.example/",
		"GOFLAGS": "-modfile=alt.mod", "GOWORK": "go.work", "GOPROXY": "https://evil.example",
		"RENV_PATHS_LOCKFILE": "alt.lock", "NETRC": "/tmp/netrc",
		"DATABASE_URL": "postgres://app", "GOOGLE_MAPS_KEY": "not-an-installer-variable",
	}
	buildEnv := b.buildEnvironment(appEnv, "/apps/demo")
	for k, v := range appEnv {
		switch k {
		case "DATABASE_URL", "GOOGLE_MAPS_KEY":
			if buildEnv[k] != v {
				t.Errorf("%s did not reach the build", k)
			}
		case "UV_INDEX_URL":
			if buildEnv[k] != "https://mirror.internal/simple" {
				t.Errorf("UV_INDEX_URL = %q, want the operator's", buildEnv[k])
			}
		default:
			if _, ok := buildEnv[k]; ok {
				t.Errorf("%s=%q reached the build", k, buildEnv[k])
			}
		}
	}
	if got := toolchain.InstallerConfigNames(appEnv); len(got) != 10 || got[0] != "GOFLAGS" {
		t.Errorf("InstallerConfigNames = %v", got)
	}
}
