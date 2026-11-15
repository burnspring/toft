package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/internal/toolchain"
)

// A Node version stated where Toft does not look is said, once, with what the
// build got instead — and nothing is said when [runtime] node is the statement,
// or when nothing was stated at all.
func TestANodeVersionToftDoesNotReadIsSaid(t *testing.T) {
	rt := &resolvedRuntime{Language: toolchain.Node, Chosen: toolchain.Version{Version: "24.22.0"}}
	cases := []struct {
		name  string
		files map[string]string
		pin   string
		want  []string
	}{
		{"engines", map[string]string{"package.json": `{"engines":{"node":"22.x"}}`}, "", []string{`engines.node ("22.x")`, "24.22.0", "[runtime] node"}},
		{"nvmrc", map[string]string{"package.json": `{}`, ".nvmrc": "22\n"}, "", []string{`.nvmrc ("22")`}},
		{"both", map[string]string{"package.json": `{"engines":{"node":"22"}}`, ".nvmrc": "22\n"}, "", []string{`and .nvmrc ("22") are not read`}},
		{"pinned", map[string]string{"package.json": `{"engines":{"node":"22.x"}}`}, "22", nil},
		{"nothing stated", map[string]string{"package.json": `{"scripts":{"start":"node s.js"}}`}, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r := *rt
			r.Pin = tc.pin
			w, ok := nodeVersionUnread(dir, &r)
			if ok != (tc.want != nil) {
				t.Fatalf("warned = %v, want %v (%q)", ok, tc.want != nil, w.Message)
			}
			if ok && w.Code != WarningNodeVersionUnread {
				t.Errorf("code = %q", w.Code)
			}
			for _, part := range tc.want {
				if !strings.Contains(w.Message, part) {
					t.Errorf("message %q lacks %q", w.Message, part)
				}
			}
		})
	}
}
