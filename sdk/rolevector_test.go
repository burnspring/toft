package sdk

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"toft/internal/auth"
)

// One signed role assertion, computed once and asserted by the platform and by
// all four SDKs.
//
// This is the only thing that turns a change to Toft's role signing into a
// test failure rather than four apps that quietly stop trusting anything. Five
// implementations of one HMAC exist because a verification that needed a call to
// the platform would be a request per request, which is not a feature anybody
// would turn on — and five implementations of anything drift unless something
// compares them.
//
// **R was absent from this file for the whole of its history, and that absence
// is what let sdk/r ship a caller_role that split a dot-delimited signed blob on
// a space and answered "anonymous" for every request the gateway ever forwarded
// to an R app.** A language family the invariants count is a language family
// this test counts.
//
// **Regenerating the vector to make a test fail go away is the mistake this file
// exists to catch.** If the format genuinely changes, the version prefix changes
// with it and both sides learn to read the old one.
type roleVector struct {
	Key       string `json:"key"`
	AppID     string `json:"app_id"`
	Role      string `json:"role"`
	ExpiresAt int64  `json:"expires_at"`
	Payload   string `json:"payload"`
	Blob      string `json:"blob"`
}

func loadRoleVector(t *testing.T) roleVector {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "role-vector.json"))
	if err != nil {
		t.Fatalf("reading the role vector: %v", err)
	}
	var v roleVector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parsing the role vector: %v", err)
	}
	return v
}

// The platform's mint is the specification, so it goes first: if this fails,
// nothing else in this file means anything.
func TestThePlatformMintsTheCommittedVector(t *testing.T) {
	v := loadRoleVector(t)

	got := auth.MintRoleWith(v.Key, v.Role, v.AppID, time.Unix(v.ExpiresAt, 0))
	if got != v.Blob {
		t.Errorf("the platform now signs a role differently:\n got %s\nwant %s\n\n"+
			"Every deployed app verifies this locally with the key it was given. If this "+
			"change is deliberate, it needs a new version prefix, not a new vector.", got, v.Blob)
	}
	if role := auth.VerifyRole(v.Key, v.Blob, v.AppID, time.Unix(v.ExpiresAt-1, 0)); role != v.Role {
		t.Errorf("the platform's own verifier reads the vector as %q, want %q", role, v.Role)
	}
}

// Each SDK is asserted in its own runtime rather than inferred from one of them.
// The four cases are the whole contract: the vector verifies, a wrong key does
// not, an expired blob does not, and an absent key does not — and every failure
// is one word, because an app author debugging a refusal does not need four
// branches.
func TestEverySDKReadsTheSameVectorAndRefusesTheSameWay(t *testing.T) {
	v := loadRoleVector(t)
	valid := v.ExpiresAt - 1
	expired := v.ExpiresAt + 1

	t.Run("go", func(t *testing.T) {
		// The Go SDK is a package in this module, but it is *the shipped copy*
		// rather than an import here: sdk/go has no go.mod and is materialized
		// into an app's tree. Running its own suite is what asserts it; this
		// asserts the platform's reference verifier agrees with the vector at
		// the same four points, so a divergence has one place to show up.
		cases := map[string]struct {
			key, blob string
			now       int64
			want      string
		}{
			"the vector":  {v.Key, v.Blob, valid, v.Role},
			"wrong key":   {"some-other-key", v.Blob, valid, "unknown"},
			"expired":     {v.Key, v.Blob, expired, "unknown"},
			"absent key":  {"", v.Blob, valid, "unknown"},
			"absent blob": {v.Key, "", valid, "unknown"},
		}
		for name, tc := range cases {
			if got := auth.VerifyRole(tc.key, tc.blob, v.AppID, time.Unix(tc.now, 0)); got != tc.want {
				t.Errorf("%s = %q, want %q", name, got, tc.want)
			}
		}
	})

	t.Run("node", func(t *testing.T) {
		if _, err := exec.LookPath("node"); err != nil {
			t.Skip("no node on PATH")
		}
		script := `
import { verifyRole } from "./caller.js";
const [key, blob, nowSec] = [process.argv[2], process.argv[3], Number(process.argv[4])];
process.stdout.write(verifyRole(key, blob, nowSec * 1000));
`
		run := func(key, blob string, now int64) string {
			out, err := runNodeScript(t, script, key, blob, itoa(now))
			if err != nil {
				t.Fatalf("running the node verifier: %v", err)
			}
			return out
		}
		assertFour(t, "node", v, run, valid, expired)
	})

	t.Run("r", func(t *testing.T) {
		if _, err := exec.LookPath("Rscript"); err != nil {
			t.Skip("no Rscript on PATH")
		}
		// openssl supplies the HMAC: R has none in base, and the runner image
		// carries the package for exactly this reason. Absent, the SDK cannot
		// verify at all, so skipping is the honest answer — but it is skipped
		// on the *package*, never on the assertion.
		if !rHasOpenSSL() {
			t.Skip("no openssl package in this R")
		}
		script := `
args <- commandArgs(trailingOnly = TRUE)
source("R/viewer.R")
cat(verify_role(args[1], args[2], as.numeric(args[3])))
`
		run := func(key, blob string, now int64) string {
			cmd := exec.Command("Rscript", "-e", script, key, blob, itoa(now))
			cmd.Dir = "r"
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("running the r verifier: %v", err)
			}
			return string(out)
		}
		assertFour(t, "r", v, run, valid, expired)
	})

	t.Run("python", func(t *testing.T) {
		python := pythonOnPath()
		if python == "" {
			t.Skip("no python3 on PATH")
		}
		script := `
import sys
sys.path.insert(0, "python")
from toft.caller import verify_role
sys.stdout.write(verify_role(sys.argv[1], sys.argv[2], float(sys.argv[3])))
`
		run := func(key, blob string, now int64) string {
			cmd := exec.Command(python, "-c", script, key, blob, itoa(now))
			cmd.Dir = "."
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("running the python verifier: %v", err)
			}
			return string(out)
		}
		assertFour(t, "python", v, run, valid, expired)
	})
}

func assertFour(t *testing.T, lang string, v roleVector, run func(key, blob string, now int64) string, valid, expired int64) {
	t.Helper()
	if got := run(v.Key, v.Blob, valid); got != v.Role {
		t.Errorf("%s reads the vector as %q, want %q — the platform and this SDK "+
			"no longer compute the same MAC", lang, got, v.Role)
	}
	for name, got := range map[string]string{
		"wrong key":   run("some-other-key", v.Blob, valid),
		"expired":     run(v.Key, v.Blob, expired),
		"absent key":  run("", v.Blob, valid),
		"absent blob": run(v.Key, "", valid),
	} {
		if got != "unknown" {
			t.Errorf("%s: %s = %q, want unknown — a caller it cannot verify must "+
				"refuse, because the one deployment where the header is missing is "+
				"the one nobody configured", lang, name, got)
		}
	}
}

func runNodeScript(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	// Written beside the SDK rather than into a temp directory: the script
	// imports ./caller.js, and the package is ESM with no build step, so the
	// import has to resolve against the real files an app would receive.
	path := filepath.Join("node", "role_vector_check.mjs")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		return "", err
	}
	t.Cleanup(func() { os.Remove(path) })

	cmd := exec.Command("node", append([]string{"role_vector_check.mjs"}, args...)...)
	cmd.Dir = "node"
	out, err := cmd.Output()
	return string(out), err
}

func pythonOnPath() string {
	for _, name := range []string{"python3", "python"} {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return ""
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// rHasOpenSSL reports whether this R can compute an HMAC at all.
//
// R has none in base. The runner image installs the openssl package, and an SDK
// whose availability depended on the app's own lockfile would be an SDK that
// fails at the moment the app is already deployed.
func rHasOpenSSL() bool {
	cmd := exec.Command("Rscript", "-e", `cat(requireNamespace("openssl", quietly = TRUE))`)
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "TRUE"
}
