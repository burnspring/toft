package api

import (
	"strings"
	"testing"

	"toft/internal/config"
	"toft/internal/database"
)

// The default is the one that matters: an app nobody configured must still be
// framable by the install itself, or the live preview on every app page is a
// blank box — and must be framable by nothing else, or the setting is decoration.
func TestFrameAncestorsDefaultsToTheInstall(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example.com", AppsPort: 8081, Port: 8080}}
	got := s.frameAncestorsPolicy(&database.App{})

	if !strings.HasPrefix(got, "frame-ancestors ") {
		t.Fatalf("not a frame-ancestors policy: %q", got)
	}
	if !strings.Contains(got, "'self'") {
		t.Errorf("the app cannot frame itself: %q", got)
	}
	if !strings.Contains(got, "https://toft.example.com") {
		t.Errorf("the platform cannot frame the app it is previewing: %q", got)
	}
	if strings.Contains(got, "*") {
		t.Errorf("the default allows any site: %q", got)
	}
}

// A single-origin install frames its apps from its own origin, so 'self' is
// already the answer and naming the platform again would be noise.
func TestFrameAncestorsSingleOrigin(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example.com", Port: 8080, AppsPort: 8080}}
	got := s.frameAncestorsPolicy(&database.App{})
	if got != "frame-ancestors 'self'" {
		t.Errorf("policy = %q, want just 'self'", got)
	}
}

func TestFrameAncestorsCarriesTheAppsList(t *testing.T) {
	s := &Server{cfg: &config.Config{BaseURL: "https://toft.example.com", Port: 8080, AppsPort: 8080}}
	app := &database.App{FrameAncestors: database.FrameAncestors{"https://intranet.example.com"}}
	got := s.frameAncestorsPolicy(app)
	if got != "frame-ancestors 'self' https://intranet.example.com" {
		t.Errorf("policy = %q", got)
	}
}

// What a person types, and what the platform will not accept from them. Each
// refusal here is a way of believing something false about what was configured.
func TestParseFrameAncestors(t *testing.T) {
	ok, err := database.ParseFrameAncestors([]string{
		"https://intranet.example.com",
		" http://tools.internal:8080 ",
		"https://*.example.com",
		"https://intranet.example.com", // a duplicate is not an error
		"",
	})
	if err != nil {
		t.Fatalf("valid list refused: %v", err)
	}
	want := "https://intranet.example.com http://tools.internal:8080 https://*.example.com"
	if ok.String() != want {
		t.Errorf("canonical form = %q, want %q", ok.String(), want)
	}

	for _, bad := range []string{
		"*",                           // anyone
		"*.example.com",               // anyone, spelled hopefully
		"example.com",                 // no scheme: a browser reads this as a path
		"https://example.com/embed",   // a path CSP ignores
		"ftp://example.com",           // not a browser origin
		"https://a.*.example.com",     // a wildcard CSP does not allow
		"https://user:pw@example.com", // credentials
		"https://example.com?x=1",     // a query
		"https://a.com;report-uri",    // ends the directive and starts another (url.Parse calls it a host)
		"https://a.com;",              // an empty directive after it
		"https://a.com,b.com",         // a second source smuggled into one entry
		"https://a.com:80:81",         // two ports
		"https://a_b.example.com",     // not a hostname character
	} {
		if _, err := database.ParseFrameAncestors([]string{bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}

	if _, err := database.ParseFrameAncestors(make([]string, database.MaxFrameAncestors+1)); err != nil {
		t.Errorf("a list of empty strings is empty, not an overflow: %v", err)
	}
	many := make([]string, 0, database.MaxFrameAncestors+1)
	for i := 0; i <= database.MaxFrameAncestors; i++ {
		many = append(many, "https://h"+string(rune('a'+i))+".example.com")
	}
	if _, err := database.ParseFrameAncestors(many); err == nil {
		t.Error("no bound on the list")
	}
}

// The empty list and a never-configured app are the same policy, and neither is
// null on the wire: a settings form that reads null has to guess what it meant.
func TestFrameAncestorsJSONIsAlwaysAList(t *testing.T) {
	b, err := database.FrameAncestors(nil).MarshalJSON()
	if err != nil || string(b) != "[]" {
		t.Errorf("empty list marshalled as %s (%v)", b, err)
	}
}
