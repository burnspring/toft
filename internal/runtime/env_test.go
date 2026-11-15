package runtime

import (
	"slices"
	"strings"
	"testing"
)

// Platform secrets must never reach a child process. This is the regression
// test for hardening task 1.1: with secrets set in this process, BaseEnv()
// (used at every pm2/build/git exec site) must not carry them.
func TestBaseEnvExcludesPlatformSecrets(t *testing.T) {
	t.Setenv("JWT_SECRET", "super-secret-jwt-value")
	t.Setenv("ENCRYPTION_KEY", "super-secret-encryption-value")
	// A retired key opens every record written before the rotation, so it is
	// exactly as much of a secret as the live one and is asserted beside it.
	t.Setenv("ENCRYPTION_KEY_OLD", "super-secret-retired-encryption-value")
	// The URI carries no credential by construction, but it names this
	// install's KMS key and a workload has no business knowing it either.
	t.Setenv("ENCRYPTION_KEY_URI", "aws-kms://arn:aws:kms:us-east-1:123456789012:alias/toft")
	t.Setenv("GITHUB_CLIENT_SECRET", "super-secret-oauth-value")
	t.Setenv("DATABASE_URL", "postgres://toft:pw@localhost/toft")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/toft")
	t.Setenv("LC_ALL", "en_US.UTF-8")

	env := BaseEnv()

	for _, kv := range env {
		key := strings.SplitN(kv, "=", 2)[0]
		switch key {
		case "JWT_SECRET", "ENCRYPTION_KEY", "ENCRYPTION_KEY_OLD", "ENCRYPTION_KEY_URI",
			"GITHUB_CLIENT_SECRET", "DATABASE_URL":
			t.Errorf("BaseEnv() leaked platform secret %s", key)
		}
		if strings.Contains(kv, "super-secret") || strings.Contains(kv, "postgres://") {
			t.Errorf("BaseEnv() leaked a secret value in %q", kv)
		}
	}

	want := map[string]string{
		"PATH":   "/usr/bin:/bin",
		"HOME":   "/home/toft",
		"LC_ALL": "en_US.UTF-8",
	}
	got := map[string]string{}
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		got[parts[0]] = parts[1]
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("BaseEnv() missing allowlisted %s: got %q, want %q", k, got[k], v)
		}
	}
}

// TestBaseEnvStripsOTEL is ADR §31 D8, and it is the highest-value test in the
// telemetry work.
//
// The platform's environment now holds the operator's collector credential. A
// deployed app that inherited OTEL_EXPORTER_OTLP_HEADERS could write into the
// operator's observability stack under the platform's identity, indefinitely,
// and nothing about that would look wrong from either side — which makes it a
// strictly worse outcome than most secrets leaking.
//
// The allow-list in env.go is what prevents it, so this test would pass on the
// day it was written whatever anyone did. It is here for the day somebody adds
// a pass-through loop beside the LC_ one.
func TestBaseEnvStripsOTEL(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.internal:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer collector-credential")
	t.Setenv("OTEL_SERVICE_NAME", "toft")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "team=platform")
	t.Setenv("METRICS_TOKEN", "scrape-token")

	for _, kv := range BaseEnv() {
		key := strings.SplitN(kv, "=", 2)[0]
		if strings.HasPrefix(key, "OTEL_") {
			t.Errorf("BaseEnv() carried %s into a child process", key)
		}
		if key == "METRICS_TOKEN" {
			t.Error("BaseEnv() carried the scrape token into a child process")
		}
		if strings.Contains(kv, "collector-credential") {
			t.Errorf("BaseEnv() leaked the collector credential in %q", kv)
		}
	}
}

// A platform started with no locale still hands its children UTF-8: R in the C
// locale printed the platform's em dashes as <U+2014>.
func TestBaseEnvDefaultsLangToUTF8(t *testing.T) {
	t.Setenv("LANG", "")
	t.Setenv("LC_ALL", "")
	if !slices.Contains(BaseEnv(), "LANG=C.UTF-8") {
		t.Errorf("BaseEnv() = %v, want LANG=C.UTF-8", BaseEnv())
	}
	t.Setenv("LANG", "en_GB.UTF-8")
	if slices.Contains(BaseEnv(), "LANG=C.UTF-8") || !slices.Contains(BaseEnv(), "LANG=en_GB.UTF-8") {
		t.Errorf("an operator's LANG was not kept: %v", BaseEnv())
	}
}
