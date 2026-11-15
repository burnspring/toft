package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toft/sdk"
)

// **The runner says when the SDK it is about to install is not the platform's.**
//
// The SDK is embedded in this binary and in the platform's, and the two are
// built and deployed separately — which is the property the family image exists
// to have, and which nothing was comparing. An install upgraded on one side and
// not the other installs one SDK into an app's venv and serves a different one
// from `GET /api/v1/sdk/{lang}`, which is what `toft sdk vendor` hands a
// developer to build against. Both declare the same version, so the only way it
// was ever found was reading `inspect.getsource` inside a running process.
//
// A warning rather than a refusal, and the asymmetry with the runtime-version
// check is the reason: an image that cannot run the resolved interpreter cannot
// run the app at all, while an SDK a version behind still works.
func TestTheRunnerNamesAnSDKSkewAndNeverRefusesOverIt(t *testing.T) {
	mine := sdk.BuildOrEmpty("python")
	if mine == "" {
		t.Fatal("this binary carries no python SDK, so there is nothing to compare")
	}

	for _, tc := range []struct {
		name  string
		boot  *BootstrapResponse
		quiet bool
	}{
		{"the two agree", &BootstrapResponse{SDKBuild: mine}, true},
		{"the platform did not say", &BootstrapResponse{}, true},
		{"there is no bootstrap", nil, true},
		{"they differ", &BootstrapResponse{SDKBuild: "0123456789ab"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sdkSkew(tc.boot, "python")
			if tc.quiet {
				if got != "" {
					t.Errorf("sdkSkew said %q; silence is the answer here — an install "+
						"older than the field has not disagreed, it has not spoken", got)
				}
				return
			}
			if got == "" {
				t.Fatal("two different SDKs produced no warning at all")
			}
			// It has to name **both**, because the number a reader needs is the
			// one they vendored and the whole failure is that the two are
			// indistinguishable without it.
			if !strings.Contains(got, mine) || !strings.Contains(got, "0123456789ab") {
				t.Errorf("the warning names only one copy: %q", got)
			}
			if !strings.Contains(got, "toft sdk vendor") {
				t.Errorf("the warning does not connect to the command that hands over "+
					"the other copy: %q", got)
			}
		})
	}
}

// The skew warning reaches the build: the SDK step carries it. sdkSkew was
// written and tested and never called, so a runner image behind the platform
// installed its older SDK in silence (docs/bugs 2026-10-22-e2e-storage #3).
func TestAPythonBuildCarriesTheSDKSkewOnItsSDKStep(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	boot := &BootstrapResponse{Framework: "flask", SDKBuild: "0123456789ab"}
	steps, cleanup, err := buildSteps(boot, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, s := range steps {
		if strings.HasPrefix(s.say, "Installing the Toft SDK") {
			if !strings.Contains(s.warn, "0123456789ab") {
				t.Errorf("the SDK step carries no skew warning: %+v", s)
			}
			return
		}
	}
	t.Fatalf("no SDK step among %d steps", len(steps))
}

// The repair installs a hashed requirements.txt without --require-hashes,
// which the deploy passes: a release built before the deploy did would
// otherwise fail its start. A well-meant consistency fix fails here, not in a
// pod (ADR §39).
func TestTheRunnersRepairInstallsWithoutRequireHashes(t *testing.T) {
	dir := t.TempDir()
	hashed := "flask==3.1.0 \\\n    --hash=sha256:aaaa\n"
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(hashed), 0o644); err != nil {
		t.Fatal(err)
	}
	steps, cleanup, err := buildSteps(&BootstrapResponse{Framework: "flask"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	want := "pip install -r requirements.txt --python venv/bin/python"
	for _, s := range steps {
		if got := strings.Join(s.args, " "); strings.Contains(got, "requirements.txt") {
			if got != want {
				t.Errorf("the repair's install is %q, want %q", got, want)
			}
			return
		}
	}
	t.Fatalf("no requirements step among %d steps", len(steps))
}

// The runner exits with the app's own code, so a restart read off the
// container names the app's exit rather than the runner's 1 (RUN.12).
func TestAppExitCarriesTheAppsCode(t *testing.T) {
	var exit AppExit
	err := fmt.Errorf("serve: %w", AppExit{Code: 7})
	if !errors.As(err, &exit) || exit.Code != 7 {
		t.Fatalf("errors.As(%v) = %+v", err, exit)
	}
	if got := (AppExit{Code: 7}).Error(); got != "app exited 7" {
		t.Errorf("Error() = %q", got)
	}
}
