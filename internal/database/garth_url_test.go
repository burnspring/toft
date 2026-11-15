package database

import "testing"

func TestNormalizeGarthURL(t *testing.T) {
	ok := map[string]string{
		"":                            "",
		"  https://Garth.Acme.com/  ": "https://garth.acme.com",
		"https://acme.com/garth/":     "https://acme.com/garth",
		"https://garth.acme.com:8443": "https://garth.acme.com:8443",
	}
	for in, want := range ok {
		got, err := NormalizeGarthURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeGarthURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"garth.acme.com", "http://garth.acme.com", "javascript:alert(1)",
		"https://", "https://u:p@garth.acme.com", "https://garth.acme.com/?x=1",
		"https://garth.acme.com/#top",
	} {
		if got, err := NormalizeGarthURL(bad); err == nil {
			t.Errorf("NormalizeGarthURL(%q) = %q, want an error", bad, got)
		}
	}
}
