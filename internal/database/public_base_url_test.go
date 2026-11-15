package database

import "testing"

// The value's whole purpose is to be pasted into an email and clicked, so what
// it refuses matters as much as what it accepts.
func TestNormalizePublicBaseURL(t *testing.T) {
	accepted := []struct{ in, want string }{
		{"", ""},
		{"https://toft.example", "https://toft.example"},
		{"https://toft.example/", "https://toft.example"},
		{"  https://toft.example  ", "https://toft.example"},
		{"http://toft.example:8080", "http://toft.example:8080"},
		{"HTTPS://toft.example", "https://toft.example"},
	}
	for _, tc := range accepted {
		got, err := NormalizePublicBaseURL(tc.in)
		if err != nil {
			t.Errorf("NormalizePublicBaseURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizePublicBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	refused := []struct{ name, in string }{
		// A javascript: URL in mail the platform sent would be a phishing
		// primitive carrying the platform's name.
		{"javascript scheme", "javascript:alert(1)"},
		{"data scheme", "data:text/html,hi"},
		{"no scheme", "toft.example"},
		{"no host", "https://"},
		// Every caller appends its own path; a stored one would be doubled.
		{"path", "https://toft.example/toft"},
		{"query", "https://toft.example?a=1"},
		{"fragment", "https://toft.example#top"},
	}
	for _, tc := range refused {
		if got, err := NormalizePublicBaseURL(tc.in); err == nil {
			t.Errorf("%s: NormalizePublicBaseURL(%q) = %q, want an error", tc.name, tc.in, got)
		}
	}
}
