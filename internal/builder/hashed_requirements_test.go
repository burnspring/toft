package builder

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"toft/internal/deps"
)

func requirementsRelease(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const hashedRequirements = "flask==3.1.0 \\\n    --hash=sha256:aaaa\n    # via -r requirements.in\n" +
	"werkzeug==3.1.9 \\\n    --hash=sha256:bbbb\n    # via flask\n"

// **The scan reads a hashed file as complete only because the install refuses
// anything it does not list.** One fixture through both halves: if either
// changes without the other, the SBOM claims a set the build does not install
// (ADR §39) — or the build refuses what the scan
// still calls unlocked.
func TestAHashedFileIsCompleteOnlyBecauseTheInstallRequiresHashes(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no uv: nothing is resolved
	for _, tc := range []struct {
		name, body string
		locked     bool
	}{
		{"hashed", hashedRequirements, true},
		{"partly hashed", "flask==3.1.0 --hash=sha256:aaaa\nwerkzeug==3.1.9\n", false},
		{"pinned, no hashes", "flask==3.1.0\nwerkzeug==3.1.9\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := requirementsRelease(t, tc.body)
			res, _ := deps.Resolve(context.Background(), dir, nil)
			args, hashed, err := requirementsInstallArgs(dir, true)
			if err != nil {
				t.Fatal(err)
			}
			flag := slices.Contains(args, "--require-hashes")
			read := res.HashedRequirements && len(res.Gaps) == 0
			if flag != tc.locked || hashed != tc.locked || read != tc.locked {
				t.Errorf("install flag %v (args %q), scan read as the lock %v (gaps %+v); want both %v",
					flag, args, read, res.Gaps, tc.locked)
			}
		})
	}
}

// A repair installs a release already built, and one built before the flag
// existed must still start: no --require-hashes off the deploy path (D5).
func TestARepairInstallsAHashedFileWithoutTheFlag(t *testing.T) {
	dir := requirementsRelease(t, hashedRequirements)
	args, hashed, err := requirementsInstallArgs(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pip", "install", "-r", "requirements.txt", "--python", "venv/bin/python"}
	if hashed || !slices.Equal(args, want) {
		t.Errorf("repair args = %q (hashed %v), want %q", args, hashed, want)
	}
}

// DG-1 (security review 2026-11-14): the install reads no uv configuration,
// because the scan reads none — the app's `[tool.uv]` / uv.toml would
// otherwise shape one half and not the other. Set over anything an app or the
// operator's environment says.
func TestTheInstallReadsNoUVConfig(t *testing.T) {
	t.Setenv("UV_NO_CONFIG", "0")
	b := &Builder{}
	env := b.buildEnvironment(map[string]string{"UV_NO_CONFIG": "0"}, "/")
	if env["UV_NO_CONFIG"] != "1" {
		t.Errorf("UV_NO_CONFIG = %q, want 1 on every build command", env["UV_NO_CONFIG"])
	}
}
