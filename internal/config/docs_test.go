package config

import (
	"strings"
	"testing"
)

func TestParseDocsURL(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                 DefaultDocsURL,
		"   ":                              DefaultDocsURL,
		"off":                              "",
		"OFF":                              "",
		" Off ":                            "",
		"https://docs.internal.example/":   "https://docs.internal.example",
		"http://10.0.0.5:8080/toft-docs//": "http://10.0.0.5:8080/toft-docs",
	} {
		got, err := ParseDocsURL(raw)
		if err != nil || got != want {
			t.Errorf("ParseDocsURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"docs.internal.example",
		"ftp://docs.internal.example",
		"https://user:pass@docs.internal.example",
		"https://docs.internal.example/?v=1",
		"https://docs.internal.example/#top",
		"https:///nohost",
		"none",
	} {
		if _, err := ParseDocsURL(raw); err == nil || !strings.Contains(err.Error(), "DOCS_URL") {
			t.Errorf("ParseDocsURL(%q) = %v, want a refusal naming DOCS_URL", raw, err)
		}
	}
}
