package api

import (
	"net/http/httptest"
	"testing"
)

func TestIsPageLoad(t *testing.T) {
	cases := []struct {
		name, method, dest, accept string
		want                       bool
	}{
		{"document", "GET", "document", "text/html", true},
		{"document, any Accept", "GET", "document", "*/*", true},
		{"iframe is the Preview tab", "GET", "iframe", "text/html", false},
		{"empty is a fetch", "GET", "empty", "text/html", false},
		{"script", "GET", "script", "*/*", false},
		{"no header, html first", "GET", "", "text/html,application/xhtml+xml;q=0.9", true},
		{"no header, html with params", "GET", "", "text/html;q=1.0", true},
		{"no header, html not first", "GET", "", "application/json, text/html", false},
		{"no header, no Accept", "GET", "", "", false},
		{"POST document", "POST", "document", "text/html", false},
		{"HEAD", "HEAD", "document", "text/html", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/apps/x/", nil)
		if c.dest != "" {
			r.Header.Set("Sec-Fetch-Dest", c.dest)
		}
		if c.accept != "" {
			r.Header.Set("Accept", c.accept)
		}
		if got := isPageLoad(r); got != c.want {
			t.Errorf("%s: isPageLoad = %v, want %v", c.name, got, c.want)
		}
	}
}
