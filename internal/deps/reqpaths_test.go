package deps

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A requirements file — or one it includes — that names a path outside the
// release is refused before uv runs as the platform and reads it (security
// review 2026-11-10, E-2). The walk tokenises the way uv does: an option that
// ends its line takes the next line's first token, and a line the walk cannot
// hold is a refusal, not a pass (code review 2026-11-11, G-1).
func TestARequirementsPathOutsideTheReleaseIsFound(t *testing.T) {
	long := strings.Repeat("x", 2<<20)
	for _, tc := range []struct{ body, nested, want string }{
		{"flask==3.0\n", "", ""},
		{"-r /etc/shadow\n", "", "/etc/shadow"},
		{"--requirement=../../x.txt\n", "", "../../x.txt"},
		{"-r/etc/passwd\n", "", "/etc/passwd"},
		{"-c sub/c.txt\n", "-r ../../outside.txt\n", "../../outside.txt"},
		{"-r sub/c.txt\n", "requests\n", ""},
		{"-e file:///opt/pkg\n", "", "file:///opt/pkg"},
		{"/etc/passwd\n", "", "/etc/passwd"},
		{"./localpkg\n", "", ""},
		{"-f https://example.com/wheels\n", "", ""},
		{"requests # -r /etc/shadow\n", "", ""},
		// G-1: the value on the next line, as uv reads it.
		{"-r\nsub/../../../../../../etc/hostname\n", "", "sub/../../../../../../etc/hostname"},
		{"--requirement\n/etc/shadow\n", "", "/etc/shadow"},
		{"-r\n# a comment first\n../x.txt\n", "", "../x.txt"},
		{"-r\nsub/c.txt\n", "flask\n", ""},
		// A backslash continuation is joined.
		{"-r \\\n  ../../outside.txt\n", "", "../../outside.txt"},
		// A line over the scanner's old 1 MiB buffer no longer ends the walk early.
		{"requests==1.0 # " + long + "\n-r /etc/shadow\n", "", "/etc/shadow"},
		{long + "\n-r /etc/shadow\n", "", "/etc/shadow"},
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(tc.body), 0o644)
		if tc.nested != "" {
			os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
			os.WriteFile(filepath.Join(dir, "sub", "c.txt"), []byte(tc.nested), 0o644)
		}
		err := outsideRequirementPath(dir, "requirements.txt")
		label := tc.body
		if len(label) > 60 {
			label = label[:60] + "…"
		}
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%q: refused: %v", label, err)
		case tc.want != "" && err == nil:
			t.Errorf("%q: passed, want a refusal naming %q", label, tc.want)
		case tc.want != "" && !strings.Contains(err.Error(), `"`+tc.want+`"`):
			t.Errorf("%q: refusal %q does not name %q", label, err, tc.want)
		}
	}
}

// A requirements file the walk cannot read the way uv will — a FIFO, or one
// over the manifest cap — is refused rather than passed.
func TestAnUnreadableRequirementsFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := mkfifo(filepath.Join(dir, "requirements.txt")); err != nil {
		t.Skip("no FIFO here:", err)
	}
	err := outsideRequirementPath(dir, "requirements.txt")
	if err == nil || !strings.Contains(err.Error(), "could not be checked") {
		t.Fatalf("FIFO: got %v", err)
	}
}

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o644) }
