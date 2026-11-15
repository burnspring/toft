package runner

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

// The address a host service listens on is its placer's, not the bootstrap's
// (ADR §131). The bootstrap answers a pod's 0.0.0.0:8080; a
// shared workload must bind the port its backend allocated, on loopback, or it
// is reachable without the gateway.
func TestThePlacersAddressOverridesTheBootstraps(t *testing.T) {
	body, digest := tarball(t, map[string]string{
		"dump.sh": "#!/bin/sh\nenv\numask\n",
	}, map[string]int64{"dump.sh": 0o755})
	svc := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", AppSlug: "demo", Framework: "python",
		ArtifactURL: artifactServer(t, body), ArtifactSHA256: digest,
		RunToken: "narrowed", Command: "sh dump.sh",
		Port: 8080, BindAddress: "0.0.0.0",
	})
	var tee strings.Builder
	restore := logTee
	logTee = &tee
	t.Cleanup(func() { logTee = restore })

	if err := Run(context.Background(), Options{
		CallbackURL: svc.server.URL, IdentityTokenPath: identityFile(t, "boot"),
		Mode: "job", WorkDir: t.TempDir(), WorkloadName: "demo-abc123",
		Port: 4123, Bind: "127.0.0.1",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := tee.String()
	if !strings.Contains(out, "PORT=4123") || !strings.Contains(out, "HOST=127.0.0.1") {
		t.Errorf("the app was not told the placer's address; its environment:\n%s", out)
	}
	if strings.Contains(out, "PORT=8080") {
		t.Error("the bootstrap's pod-shaped port reached a host service")
	}
	// Gated storage needs a group-writable file: the app starts under 0007.
	if !strings.Contains(out, "] 0007") {
		t.Errorf("the app did not start under umask 0007:\n%s", out)
	}
}

// A run listens nowhere, whatever the placer's environment says.
func TestARunIsGivenNoAddressByThePlacer(t *testing.T) {
	body, digest := tarball(t, map[string]string{"dump.sh": "#!/bin/sh\nenv\n"},
		map[string]int64{"dump.sh": 0o755})
	job := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", AppSlug: "demo", Framework: "python",
		ArtifactURL: artifactServer(t, body), ArtifactSHA256: digest,
		RunToken: "narrowed", RunID: "run-1", JobName: "dump", Command: "sh dump.sh",
	})
	if err := Run(context.Background(), Options{
		CallbackURL: job.server.URL, IdentityTokenPath: identityFile(t, "boot"),
		Mode: "job", WorkDir: t.TempDir(), WorkloadName: "run-1", Port: 4123, Bind: "127.0.0.1",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if env := strings.Join(job.allLines(), "\n"); strings.Contains(env, "PORT=") {
		t.Error("a run was handed a port")
	}
}

// The placer's two variables are the runner's inputs, never the app's: read,
// then unset, like every other one.
func TestThePlacersAddressIsReadAndUnset(t *testing.T) {
	t.Setenv(EnvPort, "4123")
	t.Setenv(EnvBind, "127.0.0.1")
	o := OptionsFromEnv()
	if o.Port != 4123 || o.Bind != "127.0.0.1" {
		t.Errorf("options = port %d bind %q", o.Port, o.Bind)
	}
	for _, k := range []string{EnvPort, EnvBind} {
		if _, set := os.LookupEnv(k); set {
			t.Errorf("%s survived into the runner's environment, which the app inherits", k)
		}
	}
}

// The shared launcher's three defaults are the one launcher's now, and an
// app's own value still wins over each.
func TestTheLauncherDefaultsYieldToTheApps(t *testing.T) {
	c := &Client{baseURL: "http://platform"}
	dir := t.TempDir()
	get := func(env []string, k string) string {
		for _, kv := range env {
			if name, v, _ := strings.Cut(kv, "="); name == k {
				return v
			}
		}
		return "<unset>"
	}
	node := appEnv(&BootstrapResponse{Framework: "node"}, c, "serve", dir)
	if got := get(node, "NODE_ENV"); got != "production" {
		t.Errorf("NODE_ENV = %q, want production", got)
	}
	own := appEnv(&BootstrapResponse{Framework: "node", Env: map[string]string{"NODE_ENV": "staging"}}, c, "serve", dir)
	if got := get(own, "NODE_ENV"); got != "staging" {
		t.Errorf("the app's own NODE_ENV was overridden: %q", got)
	}
	flask := appEnv(&BootstrapResponse{Framework: "flask", Entrypoint: "server.py"}, c, "serve", dir)
	if get(flask, "PYTHONPATH") != dir || get(flask, "FLASK_APP") != "server" {
		t.Errorf("flask PYTHONPATH=%q FLASK_APP=%q", get(flask, "PYTHONPATH"), get(flask, "FLASK_APP"))
	}
	if got := get(appEnv(&BootstrapResponse{Framework: "flask"}, c, "build", dir), "PYTHONPATH"); got != "<unset>" {
		t.Errorf("a build was given PYTHONPATH=%q", got)
	}
}

// The app's end is the runner's (RUN.12, RUN.36): a signal death comes back
// as the signal and the shell's 128+n beside it, so the runner can die of the
// same signal and whatever reaps it reads what the app did.
func TestAnAppsSignalDeathIsCarriedToTheRunner(t *testing.T) {
	body, digest := tarball(t, map[string]string{"die.sh": "#!/bin/sh\nkill -SEGV $$\n"},
		map[string]int64{"die.sh": 0o755})
	p := newStubPlatform(t, BootstrapResponse{
		AppID: "app-1", AppSlug: "demo", Framework: "python",
		ArtifactURL: artifactServer(t, body), ArtifactSHA256: digest,
		RunToken: "narrowed", RunID: "run-1", JobName: "die", Command: "sh die.sh",
	})
	err := Run(context.Background(), Options{
		CallbackURL: p.server.URL, IdentityTokenPath: identityFile(t, "boot"),
		Mode: "job", WorkDir: t.TempDir(), WorkloadName: "run-1",
	})
	var exit AppExit
	if !errors.As(err, &exit) || exit.Signal != syscall.SIGSEGV || exit.Code != 128+int(syscall.SIGSEGV) {
		t.Errorf("Run = %v (%+v), want the app's SIGSEGV and code 139", err, exit)
	}
}
