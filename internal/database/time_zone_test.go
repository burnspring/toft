package database

import "testing"

func TestNormalizeTimeZone(t *testing.T) {
	ok := map[string]string{
		"":                 "",
		"  ":               "",
		"UTC":              "UTC",
		"utc":              "UTC",
		"Etc/UTC":          "UTC",
		"America/New_York": "America/New_York",
		" Europe/London ":  "Europe/London",
		"Asia/Kolkata":     "Asia/Kolkata",
	}
	for in, want := range ok {
		got, err := NormalizeTimeZone(in)
		if err != nil || got != want {
			t.Errorf("NormalizeTimeZone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// "Local" is the server's zone — the one a hosted install must never show.
	for _, in := range []string{"Local", "local", "Mars/Olympus", "EST5EDT; DROP", "browser"} {
		if got, err := NormalizeTimeZone(in); err == nil {
			t.Errorf("NormalizeTimeZone(%q) = %q, want a refusal", in, got)
		}
	}
}

func TestNormalizeUserTimeZone_AllowsBrowser(t *testing.T) {
	for _, in := range []string{"browser", "Browser"} {
		if got, err := NormalizeUserTimeZone(in); err != nil || got != UserTimeZoneBrowser {
			t.Errorf("NormalizeUserTimeZone(%q) = %q, %v", in, got, err)
		}
	}
	if got, err := NormalizeUserTimeZone("Europe/Paris"); err != nil || got != "Europe/Paris" {
		t.Errorf("a zone = %q, %v", got, err)
	}
}
