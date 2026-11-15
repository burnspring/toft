package sdk_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// **Every SDK reads the platform's error envelope.**
//
// `internal/api/error_envelope.go` completes every error body the platform
// writes with `code`, `retryable` and, where there is a next step, `hint`.
// The point of `retryable` is that the handler decides: a 503 that means "this
// install is configured without it" says false, and a 409 that means "wait for
// a run to finish" says true — the two answers a status rule gets backwards.
// An SDK that goes on computing it from the status alone is still right for
// most refusals, which is exactly how it would go unnoticed: the app retries a
// permanent refusal forever, or pages a person over a wait.
//
// The comparison is textual, like the route check beside it: each key has to
// appear in the SDK's error type spelled as a lookup into the decoded body, and
// the platform has to spell the same key the same way. A rename on either half
// fails here rather than in an app.
var envelopeKeys = []string{"code", "retryable", "hint"}

// envelopeReaders is where each SDK decodes a refusal, and how that language
// spells "this key of the body".
var envelopeReaders = map[string]func(key string) *regexp.Regexp{
	filepath.Join("go", "errors.go"): func(k string) *regexp.Regexp {
		return regexp.MustCompile(`\.Body\["` + k + `"\]`)
	},
	filepath.Join("python", "toft", "errors.py"): func(k string) *regexp.Regexp {
		return regexp.MustCompile(`self\.body\.get\("` + k + `"`)
	},
	filepath.Join("node", "transport.js"): func(k string) *regexp.Regexp {
		return regexp.MustCompile(`this\.body\.` + k + `\b`)
	},
	filepath.Join("r", "R", "transport.R"): func(k string) *regexp.Regexp {
		return regexp.MustCompile(`body\$` + k + `\b`)
	},
}

func TestEverySDKReadsThePlatformsErrorEnvelope(t *testing.T) {
	platform, err := os.ReadFile(filepath.Join("..", "internal", "api", "error_envelope.go"))
	if err != nil {
		t.Fatalf("reading the platform's envelope: %v", err)
	}
	for _, key := range envelopeKeys {
		if !regexp.MustCompile(`body\["` + key + `"\]`).Match(platform) {
			t.Errorf("internal/api/error_envelope.go no longer writes %q as a body key — "+
				"every SDK reads it by that name", key)
		}
	}
	for file, lookup := range envelopeReaders {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, key := range envelopeKeys {
			if pattern := lookup(key); !pattern.Match(raw) {
				t.Errorf("%s does not read the envelope's %q (no match for %s) — the "+
					"platform sends it on every refusal, and an SDK that ignores it "+
					"decides for itself what only the handler knows", file, key, pattern)
			}
		}
	}
}
