package main

import (
	"errors"
	"strings"
	"testing"
)

// BB-111: under a deploy key the gateway's 401 is not "drop --anonymous" — the
// key never opens an app — and --verify does not advise rolling back a release
// that is fine.
func TestADeployKeysRefusalAtTheGatewaySaysTheKeyNeverOpensAnApp(t *testing.T) {
	err := platformAnswered(&fetchResult{Status: 401, Platform: "sign_in_required", DeployKey: true}, "demo", "")
	if strings.Contains(err.Error(), "--anonymous") || !strings.Contains(err.Error(), "deploy key") {
		t.Errorf("deploy key: %v", err)
	}
	if refusalCodeOf(err) != "sign_in_required" {
		t.Errorf("code = %q", refusalCodeOf(err))
	}
	err = platformAnswered(&fetchResult{Status: 401, Platform: "sign_in_required"}, "demo", "")
	if !strings.Contains(err.Error(), "--anonymous") {
		t.Errorf("anonymous: %v", err)
	}

	verr := verifyFailed("/healthz", "demo", &fetchResult{Status: 401, DeployKey: true}, nil)
	var rf *resultFailedError
	if !errors.As(verr, &rf) || strings.Contains(verr.Error(), "rollback") || !strings.Contains(verr.Error(), "deploy key") {
		t.Errorf("verify under a deploy key: %v", verr)
	}
	if verr := verifyFailed("/healthz", "demo", &fetchResult{Status: 401}, nil); !strings.Contains(verr.Error(), "rollback") {
		t.Errorf("verify under a personal key: %v", verr)
	}
}
