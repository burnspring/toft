package integrations

import "testing"

func TestAttribution(t *testing.T) {
	for _, tc := range []struct {
		send  Send
		shown string
		want  string
	}{
		{Send{AppName: "Payroll"}, "Payroll", "Payroll on Toft"},
		{Send{AppName: "Payroll", Platform: "Staging"}, "Payroll", "Payroll on Staging"},
		{Send{AppName: "Payroll", Platform: "Staging"}, "[Payroll](https://x)", "[Payroll](https://x) on Staging"},
		// A platform notification names the install once.
		{Send{AppName: "Toft", FromPlatform: true}, "[Toft](https://x)", "[Toft](https://x)"},
		{Send{AppName: "Staging", Platform: "Staging", FromPlatform: true}, "Staging", "Staging"},
		// An app named after the install still speaks as an app (security
		// review 2026-11-10, F-3).
		{Send{AppName: "Staging", Platform: "Staging"}, "Staging", "Staging on Staging"},
		{Send{AppName: "Toft"}, "Toft", "Toft on Toft"},
	} {
		if got := tc.send.Attribution(tc.shown); got != tc.want {
			t.Errorf("%+v / %q: %q, want %q", tc.send, tc.shown, got, tc.want)
		}
	}
}
