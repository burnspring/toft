package database

import "testing"

// The validator's refusals are the specification. The parent-and-child rows
// are the ones that matter: an app under the platform's host could set
// Domain= cookies the platform reads.
func TestParseAppHostname(t *testing.T) {
	reserved := []string{"toft.example.com", "apps.example.com"}
	extra := []string{"localhost", "tunnel.example.net"}
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"reports.example.com", "reports.example.com", true},
		{"  Reports.Example.COM. ", "reports.example.com", true},
		{"a-b.c-d.example", "a-b.c-d.example", true},

		{"", "", false},
		{"https://reports.example.com", "", false},
		{"reports.example.com:8443", "", false},
		{"reports.example.com/x", "", false},
		{"*.example.com", "", false},
		{"10.0.0.1", "", false},
		{"::1", "", false},
		{"reports", "", false},
		{"-bad.example.com", "", false},
		{"bad-.example.com", "", false},
		{"under_score.example.com", "", false},
		{"a..example.com", "", false},

		// The install's own hosts, their children and their parents.
		{"toft.example.com", "", false},
		{"apps.example.com", "", false},
		{"x.toft.example.com", "", false},
		{"deep.x.apps.example.com", "", false},
		{"example.com", "", false},
		// A sibling is not a parent.
		{"other.example.com", "other.example.com", true},
		// EXTRA_ORIGINS hosts exactly; their neighbours are fine.
		{"tunnel.example.net", "", false},
		{"other.example.net", "other.example.net", true},
	} {
		got, err := ParseAppHostname(tc.in, reserved, extra)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("ParseAppHostname(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("ParseAppHostname(%q) = %q, accepted; want a refusal", tc.in, got)
		}
	}
}
