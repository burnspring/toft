package toft

import (
	"errors"
	"strings"
	"testing"
)

// BB-096: a malformed TOFT_STORAGE is refused in its own terms, never in Go's.
func TestStorageEnvRefusalsSpeakJSON(t *testing.T) {
	for raw, want := range map[string]string{
		`[5]`:          "entry is not an object: 5",
		`[null]`:       "entry is not an object: null",
		`{"name":"x"}`: "is not a JSON array of resources",
		`[{"name":5}]`: "are strings",
	} {
		t.Setenv(EnvStorage, raw)
		_, err := Mounts()
		var e *Error
		if !errors.As(err, &e) || e.Code() != "storage_env_invalid" || !strings.Contains(e.Message, want) ||
			strings.Contains(e.Message, "Go value") || strings.Contains(e.Message, "struct {") {
			t.Errorf("%s: %v, want storage_env_invalid saying %q", raw, err, want)
		}
	}
}
