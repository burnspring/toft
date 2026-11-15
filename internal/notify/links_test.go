package notify

import "testing"

// The whole point of the function: an app's page is addressed by id. A slug
// here is the bug it was written to end — the SPA resolves `/apps/:id` by id,
// and a real slug is intercepted before the SPA is ever served.
func TestAppPageURL(t *testing.T) {
	cases := []struct {
		name, base, appID, want string
	}{
		{"plain", "https://toft.example.com", "app_1a2b", "https://toft.example.com/apps/app_1a2b"},
		{"trailing slash on the base", "https://toft.example.com/", "app_1a2b", "https://toft.example.com/apps/app_1a2b"},
		// Both halves are required, and a missing one is "no address to give"
		// rather than a URL with a hole in it — every caller renders the link
		// only when this is non-empty.
		{"no base", "", "app_1a2b", ""},
		{"no app", "https://toft.example.com", "", ""},
		{"neither", "", "", ""},
		// Ids are generated and have never contained one of these, which is
		// exactly why the escape is here rather than in a caller.
		{"an id needing escaping", "https://toft.example.com", "a b/c", "https://toft.example.com/apps/a%20b%2Fc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AppPageURL(c.base, c.appID); got != c.want {
				t.Errorf("AppPageURL(%q, %q) = %q, want %q", c.base, c.appID, got, c.want)
			}
		})
	}
}
