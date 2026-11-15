package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"toft/internal/staticserve"
	"toft/internal/toolchain"
	"toft/sdk"
)

// Options are the runner's own inputs, read from its environment — except the
// identity, which is a file.
type Options struct {
	CallbackURL string
	// IdentityTokenPath is the projected ServiceAccount token that proves which
	// pod this is. A path rather than a value because it is read at the moment
	// it is presented: kubelet rotates the file, and a runner that had cached
	// its contents at process start would be the same bug in a new place.
	IdentityTokenPath string
	Mode              string // build | serve | job
	WorkDir           string
	WorkloadName      string
	// ReleaseDir, when set, is a release already on this machine: nothing is
	// fetched and nothing is repaired, because the platform that names it
	// resolved and repaired it before starting this process. Set only by the
	// host backend.
	ReleaseDir string
	// ScratchDir replaces the pod's fixed scratch mount. Empty is the pod's.
	ScratchDir string
	// Port and Bind, when Port is set, are where a service listens, in place
	// of the bootstrap's pod-shaped answer (EnvPort). Set only by the agent.
	Port int
	Bind string
}

// OptionsFromEnv reads the runner's configuration and then *removes* it from
// the process environment.
//
// The removal is what stops the app process inheriting the runner's own inputs;
// it is not what protects the identity, which is a mounted file the runner
// cannot unmount. App code that goes looking can read it, and what it reaches
// with it is bounded and stated: its own environment and its own release, both
// things it already has, as its own pod and no other. That trade is what buys a
// credential nothing in an immutable spec has to outlive.
func OptionsFromEnv() Options {
	o := Options{
		CallbackURL:       os.Getenv(EnvCallbackURL),
		IdentityTokenPath: IdentityTokenPath,
		Mode:              os.Getenv(EnvMode),
		WorkDir:           os.Getenv(EnvWorkDir),
		WorkloadName:      os.Getenv("TOFT_WORKLOAD_NAME"),
		ReleaseDir:        os.Getenv(EnvReleaseDir),
		ScratchDir:        os.Getenv(EnvScratchPath),
		Bind:              os.Getenv(EnvBind),
	}
	if p := os.Getenv(EnvIdentityFile); p != "" {
		o.IdentityTokenPath = p
	}
	if p, err := strconv.Atoi(os.Getenv(EnvPort)); err == nil && p > 0 {
		o.Port = p
	}
	for _, k := range []string{EnvCallbackURL, EnvMode, EnvWorkDir, "TOFT_WORKLOAD_NAME",
		EnvIdentityFile, EnvReleaseDir, EnvScratchPath, EnvPort, EnvBind} {
		os.Unsetenv(k)
	}
	if o.WorkDir == "" {
		o.WorkDir = "/workspace"
	}
	if o.Mode == "" {
		o.Mode = "serve"
	}
	return o
}

// readIdentityToken reads the projected token, at the moment it is about to be
// presented rather than at process start.
//
// A missing file is a configuration failure and is said plainly: it means the
// pod was created without the projected volume, which is the shape of a
// platform and a runner image that disagree about the contract version — and
// this design would rather fail loudly than half-work.
func readIdentityToken(path string) (string, error) {
	projected := path == "" || path == IdentityTokenPath
	if path == "" {
		path = IdentityTokenPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !projected {
			// A host run: the platform wrote this file into the run's own
			// directory, so a failure is about that file or a parent the app's
			// account cannot traverse — there is no pod to blame.
			return "", fmt.Errorf("reading %s: %w — the run's identity file, written by the platform "+
				"into the run's directory, could not be read by this account", path, err)
		}
		return "", fmt.Errorf("reading %s: %w — this pod has no projected workload identity", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return tok, nil
}

// Run is the runner's whole lifecycle: bootstrap, fetch, execute, report.
//
// Both build mode and run mode live here on purpose. Because they are the same
// image, the build environment *is* the run environment — which dissolves the
// problem that a virtualenv built on one host does not transplant into a
// container, and is what makes an app movable between execution modes without
// a rebuild.
func Run(ctx context.Context, o Options) error {
	// Read here only to fail early and plainly: a pod created without the
	// projected volume has nothing to prove and no channel to say so over —
	// the one we would use is the one this credential opens. Exiting non-zero
	// is what lets the platform's supervisor catch it.
	//
	// The *path* is what the client keeps. kubelet rotates the file, and a
	// service workload outlives more than one of its own run tokens, so every
	// later presentation re-reads it.
	if _, err := readIdentityToken(o.IdentityTokenPath); err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	client := NewClient(o.CallbackURL, o.IdentityTokenPath)

	boot, err := client.Bootstrap(ctx, o.WorkloadName)
	if err != nil {
		// Nothing to report to — the channel we would report over is the one
		// that failed. Exiting non-zero is what lets the reconciler's sweep
		// catch the orphan.
		return fmt.Errorf("%s: %w", BootstrapFailed, err)
	}

	// The placer's address over the bootstrap's (EnvPort): a service on a
	// shared host listens where its backend allocated, and a run listens
	// nowhere whatever it is told.
	if o.Port > 0 && boot.RunID == "" {
		boot.Port, boot.BindAddress = o.Port, o.Bind
	}

	shipper := newLogShipper(client, boot.RunID != "")
	defer shipper.Close()

	// From here every failure is attributable to a stage, and the stage is
	// what tells an operator whose problem it is.
	fail := func(stage FailureStage, cause error) error {
		shipper.Logf("stderr", "ERROR at %s: %v", stage, cause)
		shipper.Flush(ctx)
		reportCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.ReportResult(reportCtx, ResultReport{
			ExitCode: 1, Stage: stage, Reason: cause.Error(),
		})
		return fmt.Errorf("%s: %w", stage, cause)
	}

	// Before the release is fetched, and long before any of it executes: if this
	// image is not what the platform was told it is, nothing downstream can be
	// trusted and every later failure would be a confusing symptom of this one.
	if err := verifyRuntimeVersion(boot, shipper); err != nil {
		return fail(StageRuntimeVersion, err)
	}

	if o.ScratchDir != "" {
		scratchDir = o.ScratchDir
	}
	releaseDir := filepath.Join(o.WorkDir, "release")
	if o.ReleaseDir != "" {
		releaseDir = o.ReleaseDir
	} else {
		shipper.Logf("stdout", "==> Fetching release")
		if err := fetchRelease(ctx, boot, releaseDir); err != nil {
			return fail(StageArtifactFetch, err)
		}
	}

	// Consumption self-reporting starts before app code does, so a workload
	// killed early still has a record. It reports on the same batched interval
	// as logs: at most one interval is ever lost.
	stopMetrics := startConsumptionReporter(client, shipper)
	defer stopMetrics()

	switch o.Mode {
	case "build":
		if err := runBuild(ctx, boot, releaseDir, shipper); err != nil {
			return fail(StageBuild, err)
		}
		shipper.Flush(ctx)
		return client.ReportResult(ctx, ResultReport{ExitCode: 0})

	case "job", "serve":
		// A release built somewhere else may not be runnable here. Repairing it
		// is what makes "the build environment is the run environment" true for
		// a tree that did not come from this image — see repairRelease.
		if o.ReleaseDir == "" {
			if err := repairRelease(ctx, boot, releaseDir, shipper); err != nil {
				return fail(StageBuild, err)
			}
		}
		code, sig, err := execApp(ctx, o.Mode, boot, releaseDir, client, shipper)
		// **A stop the platform asked for is not a failure** (D16): a
		// restart, a rollback, a sleep, a job's timeout or cancel all arrive
		// as the cancelled context. An app that drained and exited 0 on its
		// SIGTERM makes Wait answer ctx.Err() — which used to be logged as
		// "ERROR at app_exec: context canceled" — and one that died of the
		// signal is the platform's stop, not a kill from elsewhere.
		stopped := ctx.Err() != nil
		if err != nil && stopped && errors.Is(err, ctx.Err()) {
			return stoppedAsAsked(o.Mode, client, shipper)
		}
		shipper.Flush(ctx)
		if err != nil {
			return fail(StageAppExec, err)
		}
		reportCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if reportErr := client.ReportResult(reportCtx, ResultReport{ExitCode: code}); reportErr != nil {
			return reportErr
		}
		if code != 0 {
			return AppExit{Code: code, Signal: sig, Stopped: stopped}
		}
		return nil

	default:
		return fmt.Errorf("unknown runner mode %q (want build, serve, or job)", o.Mode)
	}
}

// stoppedAsAsked ends a run whose app exited 0 after the platform asked it to
// stop. A service's stop is said as a progress line, never an ERROR: nothing
// went wrong. A job stopped before it finished is still not a success — its
// row was already moved (timed out, cancelled) by whoever stopped it, and the
// report says failed rather than let a drained exit 0 read as "succeeded"
// should it ever arrive first.
func stoppedAsAsked(mode string, client *Client, shipper *logShipper) error {
	reportCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if mode == "job" {
		shipper.Logf("stdout", "==> Stopped before it finished, as the platform asked")
		shipper.Flush(reportCtx)
		_ = client.ReportResult(reportCtx, ResultReport{
			ExitCode: 1, Stage: StageAppExec, Reason: "stopped before it finished",
		})
		return errors.New("the run was stopped before it finished")
	}
	shipper.Logf("stdout", "==> Stopped")
	shipper.Flush(reportCtx)
	_ = client.ReportResult(reportCtx, ResultReport{ExitCode: 0})
	return nil
}

// AppExit is the app's own non-zero exit, which the runner exits with in
// turn: the cluster reads a restart's exit code off the container, and a
// runner that always exited 1 made every crash read "code 1" whatever the app
// said (RUN.12).
type AppExit struct {
	Code int
	// Signal is the one that ended the app, when one did; Code is then 128+n.
	Signal syscall.Signal
	// Stopped is true when the platform had asked the app to stop: the exit
	// is the answer to that request, not a crash or a kill from elsewhere.
	Stopped bool
}

// Error is the line the runner logs as it ends. The signal is named as an
// operator writes it — "SIGKILL" — never by syscall.Signal's String, which
// made a restart read "app was killed by killed" (D16).
func (e AppExit) Error() string {
	switch {
	case e.Signal != 0 && e.Stopped:
		return fmt.Sprintf("app stopped on %s, as the platform asked", signalName(e.Signal))
	case e.Signal != 0:
		return fmt.Sprintf("app was killed by %s", signalName(e.Signal))
	case e.Stopped:
		return fmt.Sprintf("app exited %d when the platform asked it to stop", e.Code)
	}
	return fmt.Sprintf("app exited %d", e.Code)
}

// fetchRelease downloads and unpacks the release tarball, verifying it against
// the digest the platform recorded. The URL is presigned, so the workload
// holds no object-store credential of its own — app code that leaked one could
// read every other app's releases.
func fetchRelease(ctx context.Context, boot *BootstrapResponse, dest string) error {
	if boot.ArtifactURL == "" {
		return errors.New("bootstrap returned no artifact URL")
	}
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, boot.ArtifactURL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("artifact store answered %d", resp.StatusCode)
	}

	// Stage, verify, then publish: executing a tree assembled from unverified
	// bytes is how a corrupt transfer becomes a mysterious app bug.
	staging := dest + ".staging"
	os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	digest := sha256.New()
	if err := untarGz(io.TeeReader(resp.Body, digest), staging); err != nil {
		return err
	}
	if boot.ArtifactSHA256 != "" {
		if got := hex.EncodeToString(digest.Sum(nil)); got != boot.ArtifactSHA256 {
			return fmt.Errorf("artifact digest %s, expected %s", got, boot.ArtifactSHA256)
		}
	}

	os.RemoveAll(dest)
	return os.Rename(staging, dest)
}

// appEnv assembles the environment app code runs with.
//
// It is the app's own environment plus the job contract, and deliberately not
// the runner's: the runner's own inputs are already unset from this process,
// and nothing here re-adds them or the identity it read. A job gets the *app's*
// environment, never Toft's.
//
// dir is the release, and it is a parameter because one variable here has to be
// an absolute path into it — see R_LIBS_SITE below.
func appEnv(boot *BootstrapResponse, client *Client, mode, dir string) []string {
	env := map[string]string{}
	for k, v := range boot.Env {
		env[k] = v
	}

	// Toft's own values win, so an app cannot redefine its base path or
	// its port out from under the gateway.
	env["BASE_PATH"] = boot.BasePath
	if boot.Port > 0 {
		env["PORT"] = strconv.Itoa(boot.Port)
		bind := boot.BindAddress
		if bind == "" {
			// All interfaces is correct for an isolated workload: the network
			// namespace is the boundary, and the gateway is the only route in.
			bind = "0.0.0.0"
		}
		env["HOST"] = bind
	}

	env[EnvAPIURL] = client.baseURL
	// The run token only where there is a run. It is the runner's own
	// credential on the /runner/* ingest routes, and for a service those
	// routes are the platform log and the consumption figures — which app
	// code holding it could write in the platform's voice. A job's code does
	// hold it, because reporting its own progress and result is what a job's
	// code does; a service's code has its app token and no run to report on,
	// and every SDK reads TOFT_JOB_TOKEN only when TOFT_RUN_ID is set.
	if boot.RunID != "" {
		env[EnvRunToken] = client.RunToken()
	}
	env[EnvAppSlug] = boot.AppSlug
	// The app's own identity, when it was given one — the outbound credential
	// and the id it belongs to, together. Service workloads are; job workloads
	// are not, and the platform decides that by sending a token or not — the
	// runner does not have a rule of its own about which, because two places
	// deciding the same thing is how they come to disagree.
	if boot.AppToken != "" {
		env[EnvAppToken] = boot.AppToken
		env[EnvAppID] = boot.AppID
	}
	// The role-verification key, on its own condition rather than beside the
	// token: the two are produced by different machinery, and an app that could
	// not be given an outbound identity must still be able to refuse a caller.
	if boot.RoleKey != "" {
		env[EnvRoleKey] = boot.RoleKey
	}
	if boot.RunID != "" {
		env[EnvRunID] = boot.RunID
		env[EnvJobName] = boot.JobName
		env[EnvTrigger] = boot.Trigger
		// Set even when empty is not an option: absent means the platform could
		// not resolve who started this, and an SDK reading an absent variable as
		// `unknown` is the same safe answer it gives for an unverifiable header.
		if boot.TriggerRole != "" {
			env[EnvTriggerRole] = boot.TriggerRole
		}
		if boot.RerunOf != "" {
			env[EnvRerunOf] = boot.RerunOf
		}
		if boot.Deadline != "" {
			env[EnvDeadline] = boot.Deadline
		}
		if boot.OutputURL != "" {
			env[EnvOutputURI] = boot.OutputURL
		}
		if boot.ScheduledFor != "" {
			env[EnvScheduledFor] = boot.ScheduledFor
		}
		if len(boot.Params) > 0 {
			env[EnvParams] = string(boot.Params)
			// Per-parameter convenience vars, so a shell job need not parse
			// JSON to read one value.
			for k, v := range flattenParams(boot.Params) {
				env["TOFT_PARAM_"+strings.ToUpper(k)] = v
			}
		}
	}

	// Durable storage, passed through from the runner's own environment rather
	// than recomputed. The backend set it beside the mounts it created, so this
	// is the one description that is guaranteed to match the filesystem the app
	// is about to see. Unset means no storage is mapped, which is the ordinary
	// case and needs no variable.
	if v := os.Getenv(EnvStorage); v != "" {
		env[EnvStorage] = v
	}

	// Scratch: the same contract shared mode offers. TMPDIR is what frameworks
	// pick up automatically, EnvScratchDir is what the SDK reads. Set before
	// the passthrough loop below so the pod's inherited TMPDIR never wins, and
	// as Toft's own values so an app cannot point them somewhere the
	// wipe-on-restart guarantee does not hold.
	env["TMPDIR"] = scratchDir
	env[EnvScratchDir] = scratchDir

	// R's package library, which is the whole of how an R workload finds its
	// packages: renv restored into a series-stamped directory inside the
	// release, and nothing else on the pod points R at it.
	//
	// **This was once the half of the seam that was never written.**
	// serviceCommand says "R_LIBS_SITE is set beside this, not here", and when
	// shared mode had a launcher of its own it set the variable there and
	// nowhere in isolated. An R app therefore worked as a process and failed
	// in a pod with `there is no package called 'shiny'`, which reads as a
	// broken build rather than a missing variable, on a release whose build
	// log lists every package installing cleanly. Both modes now start through
	// this function, so it is set once for both.
	//
	// The series is this image's rather than the deployment's, for the reason
	// `unrunnable` asks the same question the same way: the library has to match
	// the R about to load it, and an image whose R moved under a release is
	// exactly the case the stamp exists to make askable. Unprobeable R sets
	// nothing and lets R use its own defaults, which is what that helper's ""
	// means.
	//
	// Toft's value wins, like BASE_PATH and PORT: an app that set this
	// itself would redirect its own library out from under the platform.
	switch boot.Framework {
	case "shiny", "plumber", "rmarkdown", "quarto", "batch-r":
		if lib := RLibsSiteFor(dir); lib != "" {
			env["R_LIBS_SITE"] = filepath.Join(dir, lib)
		}
	}

	// The Node this release was built against, in front of whatever this image
	// puts on PATH — the same question R_LIBS_SITE answers above, asked for the
	// family whose tree carries no record of its own.
	//
	// A prefix rather than a binary, because `npm` is a Node script and every
	// framework launcher here goes through one: `npm run start`, and
	// `node_modules/next/dist/bin/next`, whose shebang is `/usr/bin/env node`.
	// Naming one and leaving the other is how a release gets installed by one
	// major and executed by another.
	//
	// Empty for a release with no stamp, one whose stamp names this image's
	// default, and one whose series this image does not have — the last of which
	// never reaches here, because `Unrunnable` refuses it by name first.
	//
	// Python's prefix is the release's own venv, and it answers a different
	// question with the same mechanism: not *which* interpreter, but which
	// *environment* — see PythonPathPrefix. VIRTUAL_ENV beside it, because that
	// is what activation means to everything that looks for one, and a job that
	// runs `pip` or `uv` inside its own environment should reach the same place
	// `python` does.
	switch boot.Framework {
	case "node", "nextjs", "", "batch-node":
		if prefix := NodePathPrefix(dir); prefix != "" {
			// The app's own PATH, when it set one, and this image's otherwise.
			// Prepended rather than replaced: which interpreter runs is the
			// platform's decision and where an app finds its own tools is not.
			base := env["PATH"]
			if base == "" {
				base = os.Getenv("PATH")
			}
			env["PATH"] = PrependPath(prefix, base)
		}
	case "flask", "fastapi", "streamlit", "python", "notebook", "marimo", "batch-python":
		// Never during a build: the venv is what the first step is about to
		// create, and pointing uv at a half-built one — or at the stale one a
		// repair is on its way to discard — is how a build resolves against an
		// environment that will not exist when it finishes.
		if mode != "build" {
			if prefix := PythonPathPrefix(dir); prefix != "" {
				base := env["PATH"]
				if base == "" {
					base = os.Getenv("PATH")
				}
				env["PATH"] = PrependPath(prefix, base)
				env["VIRTUAL_ENV"] = filepath.Join(dir, "venv")
			}
		}
	}

	// Defaults the shared launcher always gave and the pod's never did, made
	// the one launcher's so an app does not change behaviour with its mode.
	// Defaults rather than the platform's word: an app's own value wins, which
	// is why these three are not PlatformEnvNames.
	setDefault := func(k, v string) {
		if _, set := env[k]; !set {
			env[k] = v
		}
	}
	// Thread-count hints from the workload's CPU limit (ADR §128): a runtime
	// that sizes a pool from 32 host cores against a one-core quota spends
	// most of each period throttled, and the hint turns a stall into fewer
	// threads. Only where the app's environment does not set its own.
	for k, v := range threadHints(cpuLimitMillicores()) {
		setDefault(k, v)
	}
	switch boot.Framework {
	case "node", "nextjs", "":
		setDefault("NODE_ENV", "production")
	case "flask", "fastapi", "streamlit", "python", "marimo":
		if mode != "build" {
			setDefault("PYTHONPATH", dir)
		}
		if boot.Framework == "flask" {
			setDefault("FLASK_APP", strings.TrimSuffix(defaultEntry(boot.Entrypoint), ".py"))
		}
	}

	// PATH and friends must survive or nothing execs.
	for _, k := range []string{"PATH", "HOME", "LANG", "TMPDIR", "TZ"} {
		if v := os.Getenv(k); v != "" {
			if _, set := env[k]; !set {
				env[k] = v
			}
		}
	}
	if _, set := env["LANG"]; !set && os.Getenv("LC_ALL") == "" {
		env["LANG"] = "C.UTF-8" // runtime.DefaultLang: R in the C locale prints <U+2014>
	}

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out) // deterministic, so two instances of a release match
	return out
}

// execApp runs the app, streaming its output home line by line.
func execApp(ctx context.Context, mode string, boot *BootstrapResponse, dir string, client *Client, shipper *logShipper) (int, syscall.Signal, error) {
	var cmd *exec.Cmd
	if mode == "job" {
		switch {
		case len(boot.Args) > 0:
			// `args` runs the release's single built binary — the one a Go
			// app's service would start — with the job's own arguments, and no
			// shell in between.
			name, args, err := jobBinaryArgs(boot)
			if err != nil {
				return 1, 0, err
			}
			cmd = exec.CommandContext(ctx, name, args...)
		case boot.Command != "":
			// A job's command comes from the app's own toft.toml, and runs with
			// the app's identity — the same premise as everything else here:
			// same repo, same environment, same identity.
			cmd = exec.CommandContext(ctx, "/bin/sh", "-c", boot.Command)
		default:
			return 1, 0, errors.New("job has no command")
		}
	} else {
		name, args, err := serviceCommand(boot, dir)
		if err != nil {
			return 1, 0, err
		}
		cmd = exec.CommandContext(ctx, name, args...)
	}

	cmd.Dir = dir
	cmd.Env = appEnv(boot, client, mode, dir)
	// **A stop is SIGTERM, not SIGKILL.** The context is cancelled by the
	// SIGTERM the supervisor sends (signal.NotifyContext in both entrypoints),
	// and exec's default answer to a cancelled context is Process.Kill — which
	// killed every app on every restart before it could drain, and logged it
	// as "killed" (D16). A service must stop on SIGTERM rather than be killed,
	// so a rolling replacement drains rather than drops connections; the
	// deadline is the supervisor's (the agent's stop grace, the pod's
	// terminationGracePeriodSeconds), which kills the whole tree after it.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	// The scratch mount exists on any pod the current backend creates; this
	// covers a pod placed by an older platform, where the env would otherwise
	// point at a path that is not there. Best-effort — a failure surfaces as
	// the app's own error the moment it writes a temp file.
	_ = os.MkdirAll(scratchDir, 0o700)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 1, 0, err
	}
	// **Gated storage needs a group-writable file**, so the app starts under
	// umask 0007: setgid on a gate gives a new file the gate's group, and 0007
	// is what lets the other apps on the gate edit it while every other
	// account is shut out. Set here because Go cannot set a child's umask and
	// this process *is* the app's parent — the runner writes nothing of its
	// own after this point. A pod's files are its own either way.
	setAppUmask()
	if err := cmd.Start(); err != nil {
		return 1, 0, err
	}
	// Whatever the app orphans is the runner's to reap (reap.go): PID 1 in a
	// pod, the subreaper of its tree on a host. The app's own pid is left to
	// cmd.Wait.
	stopReaper := startReaper(cmd.Process.Pid)
	defer stopReaper()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); shipper.Pump("stdout", stdout) }()
	go func() { defer wg.Done(); shipper.Pump("stderr", stderr) }()

	err = awaitApp(cmd, &wg)
	if err == nil {
		return 0, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// A non-zero exit is the app's own failure, reported as a code rather
		// than as a runner error: the distinction is what stops "my code
		// crashed" being presented as "the platform broke". A signal death
		// is reported as the shell's 128+n — the figure a pod's container
		// shows — and carried on beside it, so the runner can end the same way.
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), ws.Signal(), nil
		}
		return exitErr.ExitCode(), 0, nil
	}
	return 1, 0, err
}

// singleBinary is the file a Go build leaves in the release — the web app's
// and the batch app's alike.
const singleBinary = "./toft-app"

// jobBinaryArgs is the binary and the arguments a job's `args` runs.
//
// The binary is the release's single build output whatever the framework
// is called: a `[batch]` Go app has no service, so asking the service
// launcher for it refused every run ("unsupported framework batch-go").
//
// **An argv that names the binary first is read as an argv.** `args` is
// documented as the binary's argv, and a reader who knows os.Args writes
// ["./toft-app", "job", "free"]; passing that through whole gave the app
// os.Args[1] == "./toft-app", so a dispatch on os.Args[1] fell through to
// the web server and the run never ended. The binary's own name in first
// place is dropped; anything else is an argument.
func jobBinaryArgs(boot *BootstrapResponse) (string, []string, error) {
	switch boot.Framework {
	case "go", "batch-go":
	default:
		return "", nil, fmt.Errorf("args runs a Go build's single binary, and a %q release has none", boot.Framework)
	}
	args := boot.Args
	if len(args) > 0 && namesSingleBinary(args[0]) {
		args = args[1:]
	}
	return singleBinary, args, nil
}

// namesSingleBinary reports whether an argument is the release's binary by
// any of the spellings a person writes it: toft-app, ./toft-app, or a path
// ending in /toft-app.
func namesSingleBinary(a string) bool {
	a = strings.TrimSpace(a)
	return a == "toft-app" || a == singleBinary || strings.HasSuffix(a, "/toft-app")
}

// awaitApp ends a started app's run: its exit status, once its output is
// drained and nothing it started is left running.
//
// **The app's exit, not its output's EOF, is the end of the run.** A
// descendant that left the app's process group still holds a copy of stdout,
// so EOF can wait on it forever — a crash never reported, a stop ended only by
// the agent's SIGKILL of a group the daemon is not in. So the runner waits on
// the exit itself, then kills whatever the app left behind (reap.go), and only
// then drains the output. One path for a crash, a clean exit and a stop: on
// SIGTERM the app is signalled first and drains, and the sweep follows its exit.
func awaitApp(cmd *exec.Cmd, pumps *sync.WaitGroup) error {
	if waitExited(cmd.Process.Pid) {
		killChildren(cmd.Process.Pid, sweepGrace)
		pumped := make(chan struct{})
		go func() { pumps.Wait(); close(pumped) }()
		select {
		case <-pumped:
		case <-time.After(2 * time.Second):
			// A writer outside the tree still holds the pipe; cmd.Wait
			// closes the read ends, which ends the pumps.
		}
	} else {
		pumps.Wait()
	}
	err := cmd.Wait()
	pumps.Wait()
	return err
}

// serviceCommand is the per-framework start invocation, in both modes: the
// shared-mode agent places this runner as a pod's entrypoint is, so the
// port/host/base-path contract is identical by construction — an app that
// behaves differently across modes makes the mode a one-way door.
// dir is the release directory, which only the static case needs — a file
// server is the one workload whose *input* is the tree rather than a file
// inside it.
func serviceCommand(boot *BootstrapResponse, dir string) (string, []string, error) {
	port := strconv.Itoa(boot.Port)
	bind := boot.BindAddress
	if bind == "" {
		bind = "0.0.0.0"
	}
	entry := defaultEntry(boot.Entrypoint)
	module := strings.TrimSuffix(entry, ".py")

	switch boot.Framework {
	case "streamlit":
		// --server.enableCORS=false because Streamlit's own WebSocket origin
		// check compares Origin against the Host it was reached on, which a
		// proxy necessarily rewrites, so it refuses every upgrade and the page
		// renders blank. Toft's own origin check still applies. This is the
		// launcher in both modes, so an app cannot work in shared mode and
		// render blank once promoted to isolated — the worst version of this
		// bug, and the one a second launcher used to make possible.
		return "venv/bin/python", []string{"-m", "streamlit", "run", entry,
			"--server.port", port, "--server.address", bind,
			"--server.headless", "true", "--server.enableCORS", "false",
			"--server.baseUrlPath", boot.BasePath + "/"}, nil
	case "fastapi":
		attr := boot.StartCmd
		if attr == "" {
			attr = "app"
		}
		return "venv/bin/python", []string{"-m", "uvicorn", module + ":" + attr,
			"--host", bind, "--port", port, "--root-path", boot.BasePath}, nil
	case "flask":
		// One definition, in flasklaunch.go.
		return "venv/bin/python", FlaskArgs(dir, module, boot.StartCmd, bind, boot.Port), nil
	case "python":
		return "venv/bin/python", []string{entry}, nil
	case "go":
		return singleBinary, nil, nil
	case "notebook", "rmarkdown", "quarto":
		// The rendered document, served by the same file server a static app
		// gets — and rooted at the directory the platform wrote it to, never
		// at the release. `staticserve` publishes everything under its root
		// that is not dot-prefixed and its refusal list is not configurable,
		// so a root of the release directory would publish requirements.txt,
		// the notebook source and the whole of venv/.
		//
		// The name is builder.RenderDir's, written out rather than imported:
		// the runner binary must not import the builder. `notebook_test.go` in
		// internal/builder reads this file and fails the build if it stops
		// agreeing with it.
		return "/proc/self/exe", []string{staticserve.CommandName,
			"--dir", filepath.Join(dir, "toft-render"), "--port", port, "--host", bind}, nil

	case "shiny", "plumber":
		// One definition, in rlaunch.go, which the builder's R steps share.
		//
		// R_LIBS_SITE is set beside this, not here: serviceCommand returns an
		// argv and the library path is environment. See rEnviron.
		//
		// The interpreter is the one matching the library in the release, not
		// this image's default. They are the same binary in an image carrying
		// one R — which every runner image does — so this is the isolated half
		// of a shared-mode fix, taken for the reason the file's opening
		// paragraph gives: both launchers ask the question in one place, or
		// they eventually answer it differently.
		return RLaunchBinary(dir), RLaunchArgs(boot.Framework, boot.Entrypoint, bind, boot.Port), nil

	case "marimo":
		// Streamlit's shape, with marimo's own flags — and unlike every other
		// Python framework here the bind invariant needs no correction, because
		// --host already defaults to loopback. It is still passed, because a
		// default is not a promise and this one is load-bearing.
		//
		// --no-token because the platform authorizes before the proxy is
		// reached and a second token would be a credential nobody issued. It is
		// also marimo's default; saying it is a statement rather than a change.
		//
		// **--include-code is deliberately absent.** Code is not sent to the
		// browser unless that flag is given, and there is no negative form of
		// it — so an app is not a source listing by omission, which is the
		// correct direction for a flag to have no opposite in.
		//
		// **--allow-origins is deliberately absent too, and that is a
		// measurement rather than an oversight.** Streamlit needs its own
		// origin check disabled here, because it compares Origin against the
		// Host it was reached on and a proxy necessarily rewrites Host —
		// leaving the page blank with the reason only in the app's log. marimo
		// 0.24.0 does not make that comparison: a websocket upgrade carrying a
		// foreign Origin against a loopback Host is accepted with the flag and
		// without it, tested both ways. Passing it anyway would be a wildcard
		// CORS allowance bought for a problem this framework does not have. If
		// a future marimo starts refusing the upgrade, this is where the fix
		// goes, and Toft gives nothing up by making it: its own origin
		// check runs on every app request before the proxy is reached.
		return "venv/bin/python", []string{"-m", "marimo", "run", entry,
			"--host", bind, "--port", port, "--base-url", boot.BasePath,
			"--headless", "--no-token"}, nil

	case "static":
		// This same binary, re-executed as a file server — /proc/self/exe rather
		// than a path, so nothing here assumes anything about the image. That is
		// what lets the static family image be an empty base with the runner in
		// it and no filesystem layout to agree on.
		//
		// Root, entry and fallback are resolved from the release directory, by
		// the same code the shared launcher's subcommand runs. Neither launcher
		// holds serving logic of its own: an app that behaved differently across
		// execution modes would make the mode a one-way door.
		return "/proc/self/exe", []string{staticserve.CommandName,
			"--dir", dir, "--port", port, "--host", bind}, nil

	case "node":
		start := boot.StartCmd
		if start == "" {
			start = "start"
		}
		// The npm beside the release's own Node. `next` below needs no such
		// correction: it is a path inside the release, and its shebang reads
		// the PATH appEnv already put the right interpreter on.
		return ToolBeside(NodePathPrefix(dir), "npm"), []string{"run", start}, nil
	case "nextjs", "":
		return "node_modules/next/dist/bin/next", []string{"start",
			"--port", port, "--hostname", bind}, nil
	default:
		return "", nil, fmt.Errorf("unsupported framework %q", boot.Framework)
	}
}

// defaultEntry is a Python release's entrypoint, app.py when detection named
// none.
func defaultEntry(entry string) string {
	if entry == "" {
		return "app.py"
	}
	return entry
}

// buildToolEnv is what every build step runs with beyond the app's own
// environment — internal/builder's cacheEnv, for the part of it that applies in
// a pod. uv's cache is under HOME and the release is a mounted volume, so its
// hardlink attempt fails across the two filesystems and prints "Failed to
// hardlink files" into the build log before copying anyway; copy is what it
// ends up doing, and saying so up front removes the warning, as on the host.
//
// UV_NO_CONFIG is the host's rule too (internal/builder buildEnvironment): no
// uv.toml or `[tool.uv]` table shapes an install, because none shapes what the
// scan judged. Appended after appEnv, so an app's own value never wins.
// GOENV=off is the same rule for Go's environment file (LB-10).
var buildToolEnv = []string{"UV_LINK_MODE=copy", "UV_NO_CONFIG=1", "GOENV=off"}

// runBuild installs dependencies and emits a ready-to-run tree.
//
// Dependencies are resolved *here*, at build time, and never at workload
// start: doing it at start would make cold start equal install time on every
// restart and crashloop iteration, would require every workload to have
// runtime egress to public package registries, and would let two instances of
// one release resolve different dependency trees.
// buildOutputGrace is how long a build step's Wait waits, after the step has
// exited, for whatever it started to let go of its output.
const buildOutputGrace = 2 * time.Second

func runBuild(ctx context.Context, boot *BootstrapResponse, dir string, shipper *logShipper) error {
	// Enforcement point 2 of three, and the reason it is *here* rather than at
	// the top of the pod's life: buildSteps is what fetches and installs the
	// packages, and a refusal after that point is a report. It is also reached
	// by repairRelease — a workload rebuilding a tree it cannot run — which is
	// exactly the path a gate placed only at first bootstrap would miss.
	//
	// The verdict travels in the bootstrap response. The pod does not query
	// the feed, does not hold the feed credential, and does not carry a second
	// copy of the matcher that could disagree with the platform's.
	if refusal := strings.TrimSpace(boot.DependencyRefusal); refusal != "" {
		shipper.Logf("stderr", "%s", refusal)
		return fmt.Errorf("this release was refused by the install's dependency policy")
	}

	steps, cleanup, err := buildSteps(boot, dir)
	if err != nil {
		return err
	}
	defer cleanup()

	// Go's SDK is a workspace rather than a command, and it has to come *before*
	// the build, which links it in. Never fatal, matching the native builder and
	// the Node case below: an app that does not import it should not fail over a
	// convenience. Removed again by the cleanup, so the tree this pod hands on is
	// the tree it was given.
	if boot.Framework == "go" || boot.Framework == "batch-go" {
		switch prep, err := sdk.PrepareGo(dir); {
		case err != nil:
			shipper.Logf("stdout", "    Warning: unpacking the Toft SDK failed: %v", err)
		case prep.Installed:
			defer prep.Done()
			if w := sdkSkew(boot, "go"); w != "" {
				shipper.Logf("stdout", "==> %s", w)
			}
			shipper.Logf("stdout", "==> Installed the Toft SDK (%s) %s", sdk.GoModule, sdk.Describe("go"))
		default:
			shipper.Logf("stdout", "%s", sdk.GoSkippedLine(dir, prep.Skipped))
		}
	}
	for _, step := range steps {
		if step.warn != "" {
			shipper.Logf("stdout", "==> %s", step.warn)
		}
		if step.say != "" {
			shipper.Logf("stdout", "==> %s", step.say)
		} else {
			shipper.Logf("stdout", "==> %s %s", step.name, strings.Join(step.args, " "))
		}
		cmd := exec.CommandContext(ctx, step.name, step.args...)
		cmd.Dir = dir
		cmd.Env = append(append(appEnv(boot, &Client{baseURL: ""}, "build", dir), buildToolEnv...), step.env...)

		// Not read to EOF before Wait: a build tool that leaves a daemon
		// holding the output (a compiler server, a package manager's cache
		// daemon) hung the build after the step itself had finished. Through
		// io.Pipe with WaitDelay, Wait returns at the step's exit plus at most
		// buildOutputGrace, and closing the writer ends the pump.
		out, outW := io.Pipe()
		cmd.Stdout, cmd.Stderr = outW, outW
		cmd.WaitDelay = buildOutputGrace
		if err := cmd.Start(); err != nil {
			return err
		}
		pumped := make(chan struct{})
		go func() { defer close(pumped); shipper.Pump("stdout", out) }()
		err := cmd.Wait()
		outW.Close()
		<-pumped
		if errors.Is(err, exec.ErrWaitDelay) {
			err = nil // the step exited 0; something it started held the output
		}
		if err != nil {
			// The app's actual build error, not a generic backend failure: the
			// developer must debug their code, not the platform.
			return fmt.Errorf("%s failed: %w", step.name, err)
		}
	}

	// Node's SDK is a directory copy rather than a command, and it has to come
	// after `npm ci`, which empties node_modules. Never fatal, matching the
	// native builder: an app that does not import it should not fail over a
	// convenience.
	switch boot.Framework {
	case "node", "nextjs", "", "batch-node":
		switch installed, err := sdk.EnsureNode(dir); {
		case err != nil:
			shipper.Logf("stdout", "    Warning: installing the Toft SDK failed: %v", err)
		case installed:
			if w := sdkSkew(boot, "node"); w != "" {
				shipper.Logf("stdout", "==> %s", w)
			}
			shipper.Logf("stdout", "==> Installed the Toft SDK (%s) %s",
				sdk.NodePackage, sdk.Describe("node"))
			shipper.Logf("stdout", "%s", sdk.NodeDeprecatedLine)
		}
	}
	return nil
}

// repairRelease rebuilds a release's dependencies when the tree it fetched
// cannot run in this image, and does nothing at all when it can.
//
// The platform builds every release natively, on the host, with the host's
// toolchain — and then an isolated workload runs that tree inside this image.
// For Python the two do not agree: `uv venv` records the absolute path of the
// interpreter it was built against (`/usr/bin/python3.11` on the platform host),
// the family image is `python:3.12-slim`, and so `venv/bin/python` dangles here.
// A job whose command is `venv/bin/python …` fails with *not found*; one that
// uses the container's own `python3` sees none of the app's dependencies. The
// same is true of an isolated Python app's service workload — this was never
// about jobs.
//
// The repair is deliberately conditional rather than an unconditional rebuild.
// A tree that already runs here — the ordinary case once a release is built by
// this image — is left exactly as it was fetched, so nothing pays for a
// dependency resolution it does not need. Only a workload that would otherwise
// fail does the work, which is why this reads as a repair and not a build step.
//
// The stale venv is removed rather than rebuilt over: its `site-packages` holds
// wheels compiled for the other interpreter's ABI (numpy, pandas, pyarrow), and
// a fresh interpreter pointed at those is a subtler failure than a missing one.
//
// The two steps this is made of are exported, because the *shared* start path
// needs the same repair for the same reason and must not carry a second opinion
// about what "unusable" means — see internal/builder's Repair. What differs
// between the two is only how the rebuild happens: `runBuild` here, the builder
// there. What must not differ is the question.
func repairRelease(ctx context.Context, boot *BootstrapResponse, dir string, shipper *logShipper) error {
	broken, what := Unrunnable(boot.Framework, dir)
	if !broken {
		return nil
	}
	shipper.Logf("stdout", "==> Repairing the release: %s", what)

	if err := DiscardUnusable(boot.Framework, dir); err != nil {
		return err
	}
	if err := runBuild(ctx, boot, dir, shipper); err != nil {
		return err
	}
	restamp(dir, boot.Framework, shipper)
	return nil
}

// restamp records the interpreter the repair just used, and it is not
// bookkeeping — it is what stops a repair from being a loop.
//
// A tree stamped for a Node this image does not have is `Unrunnable` by that
// stamp, so it is discarded and rebuilt against the default. Leave the stamp
// alone and the next start asks the same question, gets the same answer, and
// rebuilds again: not a wrong verdict once but forever, which is the shape
// `Unrunnable`'s R case already warns about in the other direction.
//
// The same value `RepairInterpreter` handed the build, read back from a stamp
// the build did not touch, so the two cannot disagree.
func restamp(dir, framework string, shipper *logShipper) {
	lang := stampedLanguage(framework)
	if lang == "" {
		return
	}
	v := RepairInterpreter(lang, dir)
	if v.Version == "" {
		return
	}
	if err := WriteRuntimeStamp(dir, lang, v.Version); err != nil {
		shipper.Logf("stdout", "    Warning: the rebuilt tree could not be recorded as %s %s: %v",
			toolchain.Label(lang), v.Version, err)
	}
}

// DiscardUnusable removes what a repair must not build *over*, and nothing else.
//
// Python is the only family whose artifact is bound to an absolute path outside
// the tree, and so the only one with anything to discard: a fresh interpreter
// pointed at `site-packages` full of wheels compiled for another ABI is a
// subtler failure than a missing one. `npm ci` and `go build` replace their own
// output, so there is nothing here for them.
func DiscardUnusable(framework, dir string) error {
	switch framework {
	case "flask", "fastapi", "streamlit", "python", "notebook", "marimo", "batch-python":
		if err := os.RemoveAll(filepath.Join(dir, "venv")); err != nil {
			return fmt.Errorf("removing the unusable virtual environment: %w", err)
		}
	case "shiny", "plumber", "rmarkdown", "quarto", "batch-r":
		// The whole library root, not just the series directory this image
		// wanted. A release that arrived carrying R-4.4 is about to be rebuilt
		// for R-4.5, and leaving 4.4 behind would keep a tree of compiled
		// objects nothing can load and no later step will ever remove — growing
		// the artifact on every repair.
		if err := os.RemoveAll(filepath.Join(dir, RenvLibraryRoot)); err != nil {
			return fmt.Errorf("removing the unusable R package library: %w", err)
		}
	}
	return nil
}

// Unrunnable reports whether this release is missing the artifact its framework
// needs in order to start here, and names it for the log.
//
// Each check is the cheapest question that distinguishes "built elsewhere and
// unusable" from "ready": os.Stat follows symlinks, so a venv whose interpreter
// link dangles fails it while a working one passes. Where a family's artifact
// cannot be told apart cheaply — a node_modules holding a native module built
// against another libc — the check stays conservative and answers "runnable",
// leaving behaviour exactly as it was rather than guessing. That conservatism is
// also the reason a Node major is a fleet event nothing here can detect.
//
// Exported and taking a framework string rather than a BootstrapResponse,
// because both start paths ask it: an isolated workload about a tree that
// arrived from another machine, and a shared workload about one whose
// interpreter moved underneath it. One definition, so the two answers cannot
// diverge.
func Unrunnable(framework, dir string) (bool, string) {
	resolves := func(rel string) bool {
		_, err := os.Stat(filepath.Join(dir, rel))
		return err == nil
	}

	switch framework {
	case "flask", "fastapi", "streamlit", "python", "marimo", "notebook", "batch-python":
		// `notebook` is the row worth pausing on. Its *workload* is a file
		// server that opens no interpreter — but its release carries a venv
		// all the same, because the build executed the notebook's cells in
		// one, and a venv-carrying release in an image with no Python is
		// exactly the failure ADR §44 exists to catch. So it is asked the
		// Python question, and it is why the image family is python rather
		// than the tiny static one.
		if !resolves("venv/bin/python") {
			return true, "its virtual environment was built against an interpreter this image does not have"
		}
	case "node", "nextjs", "", "batch-node":
		if !resolves("node_modules") {
			return true, "its dependencies are not installed"
		}
		// The same question R's case asks, and Node earned the right to be
		// asked it the day an install could carry more than one. A native
		// module is compiled against one major's ABI and fails at `require`
		// naming a `.node` file rather than a version — and unlike a venv,
		// nothing in `node_modules` dangles when the interpreter is gone, so
		// there is no cheap existence check that would notice.
		//
		// Only a release that *says* which Node it was built against. One that
		// says nothing was built when this install had a single Node, and
		// starting it on the default is exactly what it has always done.
		if series := ReleaseSeries(toolchain.Node, dir); series != "" &&
			!HasSeries(toolchain.Node, series) {
			return true, "its dependencies were built for " + toolchain.Label(toolchain.Node) +
				" " + series + ", which this host no longer has"
		}
	case "go", "batch-go":
		if !resolves("toft-app") {
			return true, "it carries no compiled binary"
		}
	case "shiny", "plumber", "rmarkdown", "quarto", "batch-r":
		// R's check is the path itself rather than the existence of one thing,
		// and the reason is stronger than Python's. A venv dangles when the
		// interpreter *moves*; an renv library holds compiled objects that are
		// wrong when the interpreter's **minor changes**, and the binaries
		// TOFT_R_REPOS serves are built per distribution too — so an image
		// whose base moves invalidates them without anything moving here.
		//
		// So the question is *is there a library for the R this image actually
		// has*, which is what the series-stamped directory exists to make
		// askable. A Python-shaped existence check would pass a library built
		// against 4.4 in an image carrying 4.5, and that release starts, loads
		// far enough to fail at dyn.load, and reports a missing shared object
		// rather than a version.
		//
		// An unprobeable R is not a verdict: the release is left alone, because
		// discarding a working library on the strength of a failed probe is the
		// worse error.
		if toolchain.DefaultSeries(toolchain.R) == "" {
			break
		}
		// The question is asked of the *release's* series rather than the
		// default's, and on a host carrying one R those are the same binary —
		// which is why this read the default for as long as it did. List a
		// second path in TOFT_R_PATH and they come apart: a release built
		// for 4.3 is judged against 4.5, declared broken, has its library
		// discarded whole, and is rebuilt by a repair that resolves 4.3 again.
		// The next start asks the same question and gets the same answer, so it
		// is not a wrong verdict once; it is a loop.
		series := RSeriesInRelease(dir)
		if series == "" {
			return true, "its R package library is not installed"
		}
		if RExecutableForSeries(series) == "" {
			return true, "its R package library was built for R " + series +
				", which this host no longer has"
		}
	case "static":
		// Nothing to be missing. A static release is the files themselves —
		// there is no venv bound to an absolute interpreter path, no
		// node_modules, and no binary — which is the property that lets its
		// family image be an empty base.
	}
	return false, ""
}

type buildStep struct {
	name string
	args []string
	// say replaces the command echo with a sentence, for a step whose command
	// line is not what the reader needs. The SDK install is the case: its
	// argument is a temporary directory with a random name, and what a reader
	// wants is *which* SDK went in.
	say string
	// env is appended to the step's environment, for a step whose behaviour is
	// configured by variables rather than flags. Only the R steps use it: renv
	// takes its library path, its cache setting and its repository override
	// from the environment and has no command-line equivalent for any of them.
	env []string
	// warn is a line printed before the step, when there is something the
	// reader has to know about what is about to happen. Empty is the ordinary
	// case; it is not an error, and it never stops the step.
	warn string
}

// sdkSkew is the sentence for an image whose SDK is not the one the platform
// serves, and "" when they agree or when the platform did not say.
//
// **A warning rather than a refusal**, and the asymmetry with the runtime
// version check is the reason: an image that cannot run the interpreter a
// deploy resolved to cannot run the app at all, while an SDK a version behind
// still works, and an app that never imports it is unaffected. What was
// missing was never a stricter rule — it was any way at all to notice.
//
// It names both copies, because the number a reader needs is the one they
// vendored, and the whole failure is that the two are indistinguishable
// without it.
func sdkSkew(boot *BootstrapResponse, lang string) string {
	mine := sdk.BuildOrEmpty(lang)
	if boot == nil || boot.SDKBuild == "" || mine == "" || boot.SDKBuild == mine {
		return ""
	}
	return fmt.Sprintf(
		"Warning: this install's runner image carries a different app SDK from the "+
			"one the platform serves (%s here, %s there). `toft sdk vendor` gives you "+
			"the platform's copy, so it is not the code this app was built against — "+
			"an operator upgrades the runner image to close the gap.",
		sdk.Describe(lang), boot.SDKBuild)
}

// buildSteps returns the commands that install a release's dependencies, and a
// cleanup for anything they were handed. Kept in step with internal/builder:
// the two paths must produce the same tree, or an app behaves differently
// depending on where it happened to be built.
func buildSteps(boot *BootstrapResponse, dir string) ([]buildStep, func(), error) {
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(dir, rel))
		return err == nil
	}
	cleanup := func() {}

	switch boot.Framework {
	case "flask", "fastapi", "streamlit", "python", "notebook", "marimo", "batch-python":
		// `--python`, from the release's own stamp, for the reason
		// internal/builder passes it on every build even when the answer is the
		// default: a bare `uv venv` is uv's *discovery order* choosing a version
		// somebody pinned, and on an image carrying three interpreters it will
		// choose. The venv that recorded the answer is the thing being
		// discarded, which is why the stamp is a file beside it rather than a
		// line inside it.
		venv := []string{"venv", "venv"}
		if v := RepairInterpreter(toolchain.Python, dir); v.Path != "" {
			venv = append(venv, "--python", v.Path)
		}
		steps := []buildStep{{name: "uv", args: venv}}
		// Before the app's own dependencies, so a version it declares itself
		// replaces this one — the platform's copy is a floor, not a ceiling.
		// Kept in step with internal/builder, which installs it the same way at
		// the same point; the two build paths must not disagree about what is
		// present in an app's environment.
		if sdkDir, err := sdk.Materialize("", "python"); err == nil {
			// The `say` line is not decoration. **This binary and the
			// platform's each carry their own copy of the SDK**, they are built
			// and deployed separately on purpose, and an install that upgraded
			// one and not the other installs a different SDK here from the one
			// `GET /api/v1/sdk/python` serves to `toft sdk vendor` — both
			// declaring the same version, because a version is a declaration
			// and both trees declared it. sdk.Describe carries the digest that
			// tells them apart, and the build log is where a developer can
			// compare it against what they vendored.
			steps = append(steps, buildStep{
				name: "uv",
				args: []string{"pip", "install", sdkDir, "--python", "venv/bin/python"},
				say:  "Installing the Toft SDK " + sdk.Describe("python"),
				// The skew warning was written and tested and never called, so
				// an image behind the platform said nothing (docs/bugs
				// 2026-10-22-e2e-storage #3).
				warn: sdkSkew(boot, "python"),
			})
			cleanup = func() { os.RemoveAll(sdkDir) }
		}
		// The platform's own Python packages for the two notebook frameworks,
		// before the app's own and for the same reason the SDK is: a version
		// the app declares itself replaces this one. Kept in step with
		// internal/builder's renderPackages — the two build paths must produce
		// the same tree, and this is the one list that is written twice
		// because the runner binary must not import the builder.
		//
		// **Nothing here re-renders.** A rebuild in a pod is a repair, and a
		// repair restores what dangles: a venv is bound to an absolute
		// interpreter path and the rendered document is not. The document is
		// already in this release, and executing every cell again outside a
		// deploy would produce output the next start throws away.
		switch boot.Framework {
		case "notebook":
			// pyyaml is Quarto's, and it is here for a release whose engine
			// this pod cannot know: the engine is a property of the release's
			// manifest and the bootstrap response does not carry it. One list
			// for both engines is what lets the two build paths produce the
			// same tree at all.
			steps = append(steps, buildStep{name: "uv", args: []string{"pip", "install",
				"nbconvert", "ipykernel", "pyyaml", "--python", "venv/bin/python"}})
		case "marimo":
			steps = append(steps, buildStep{name: "uv", args: []string{"pip", "install",
				"marimo", "--python", "venv/bin/python"}})
		}
		switch {
		case exists("requirements.txt"):
			// **No --require-hashes, on purpose**, where the deploy passes it
			// for a hashed file (internal/builder's requirementsInstallArgs).
			// A repair is a start, not a deploy: every release built since that
			// rule has a complete file, because an incomplete one failed its
			// build, and uv still checks the hashes listed without the flag. For
			// a release built before it, the flag would turn this repair into a
			// failed start. TestTheRunnersRepairInstallsWithoutRequireHashes.
			steps = append(steps, buildStep{name: "uv", args: []string{"pip", "install", "-r", "requirements.txt", "--python", "venv/bin/python"}})
		case exists("pyproject.toml"):
			steps = append(steps, buildStep{name: "uv", args: []string{"pip", "install", ".", "--python", "venv/bin/python"}})
		}
		return steps, cleanup, nil

	case "node", "nextjs", "", "batch-node":
		install := []string{"install", "--ignore-scripts"}
		if exists("package-lock.json") {
			install = []string{"ci", "--ignore-scripts"}
		}
		// The npm beside the Node this release names, by absolute path, for the
		// reason ToolBeside exists: the PATH prefix appEnv writes decides what
		// npm's shebang resolves `node` to and cannot decide which npm runs.
		npm := ToolBeside(NodePathPrefix(dir), "npm")
		steps := []buildStep{{name: npm, args: install}}
		if (boot.Framework != "node" && boot.Framework != "batch-node") || exists("next.config.js") {
			steps = append(steps, buildStep{name: npm, args: []string{"run", "build"}})
		}
		return steps, cleanup, nil

	case "go", "batch-go":
		entry := boot.Entrypoint
		if entry == "" {
			entry = "."
		}
		return []buildStep{{name: "go", args: []string{"build", "-o", "toft-app", entry}}}, cleanup, nil

	case "shiny", "plumber", "rmarkdown", "quarto", "batch-r":
		// The R half of "both build paths must produce the same tree". This is
		// the *repair* path — a release that arrived carrying a library built
		// against another R, discarded by DiscardUnusable — so it restores from
		// the lockfile exactly as internal/builder does at deploy time, with
		// the same environment and in the same order.
		//
		// The series is this image's, not the one the release was built for.
		// That is the whole point of a repair: the library is being rebuilt
		// because the R here is not the R there.
		series := toolchain.DefaultSeries(toolchain.R)
		if series == "" {
			return nil, cleanup, fmt.Errorf("this image has no R to rebuild the package library with")
		}
		lib := filepath.Join(dir, RLibsSite(series))
		// No cache: a pod has no volume to keep one on, and a repair is rare.
		env := RenvEnv(lib, boot.RRepos, "")
		if mv, err := WriteRMakevars(dir); err == nil {
			env = append(env, "R_MAKEVARS_USER="+mv)
		}

		steps := []buildStep{{name: rExecutable(), args: []string{"-e", RInstallRenvExpr, lib, boot.RRepos}, env: env}}

		// The SDK, on the same terms as the Python and Node cases above: before
		// the app's own packages, and kept in step with internal/builder
		// because the two build paths must produce the same tree.
		if sdkDir, err := sdk.Materialize("", "r"); err == nil {
			args := append([]string{"-e", RInstallSDKExpr, lib, boot.RRepos, sdkDir},
				sdk.RSDKDependencies...)
			steps = append(steps, buildStep{
				name: rExecutable(), args: args, env: env,
				say:  "Installing the Toft SDK " + sdk.Describe("r"),
				warn: sdkSkew(boot, "r"),
			})
			cleanup = func() { os.RemoveAll(sdkDir) }
		}

		if !exists("renv.lock") {
			// Nothing names what to install, so nothing is installed — the same
			// answer internal/builder gives, and not an error. A release with
			// no lockfile has an empty library by design and fails at its first
			// library() call with the app's own error.
			return steps, cleanup, nil
		}
		steps = append(steps, buildStep{name: rExecutable(), args: []string{"-e", RRestoreExpr, lib, dir}, env: env})
		return steps, cleanup, nil

	case "static":
		// No steps at all: no package manager, no interpreter, no toolchain.
		// This is the first of the four facts that make an empty base image
		// sufficient for the static family.
		return nil, cleanup, nil

	default:
		return nil, cleanup, fmt.Errorf("unsupported framework %q", boot.Framework)
	}
}

// flattenParams turns a flat JSON object into string values for the
// per-parameter environment variables. Nested values are left to
// TOFT_PARAMS rather than flattened into something ambiguous.
func flattenParams(raw []byte) map[string]string {
	out := map[string]string{}
	// Numbers as written: TOFT_PARAMS carries a float param as 2.0, and a
	// float64 round trip wrote TOFT_PARAM_X as "2" — an int, read back by a
	// shell job (DOC-3, second 2026-11-03 bash).
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return out
	}
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = t
		case json.Number:
			out[k] = t.String()
		case bool:
			out[k] = strconv.FormatBool(t)
		}
	}
	return out
}

// scanLineMax is the longest line scanLines hands on whole; the rest of a
// longer one is dropped, and the line is marked. record() clips to maxLineBytes
// anyway, so nothing past this was ever kept.
const scanLineMax = 1024 * 1024

// scanLines is the shared line reader for piped output: a line at a time, the
// longest kept whole up to scanLineMax and truncated past it, **and the reader
// is read to its end whatever it holds.** It used to be a bufio.Scanner with a
// 1 MiB token cap and no Err check: one line over that — a tqdm bar on stderr
// that only ever writes \r, a base64 blob, a minified bundle — made Scan
// return false, the pump return, and nothing read the pipe again, so the app
// blocked in write(2) once the 64 KiB pipe filled and awaitApp waited on an
// exit that never came; a build step hung the same way (code review
// 2026-11-11, Q-1 and Q2-3). The agent's output.read has the same shape.
func scanLines(r io.Reader, fn func(string)) {
	br := bufio.NewReaderSize(r, 64*1024)
	var line []byte
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			complete := chunk[len(chunk)-1] == '\n'
			if complete {
				chunk = chunk[:len(chunk)-1]
			}
			if !over {
				if room := scanLineMax - len(line); len(chunk) > room {
					line = append(line, chunk[:room]...)
					line = append(line, " …[line truncated]"...)
					over = true
				} else {
					line = append(line, chunk...)
				}
			}
			if complete {
				fn(string(line))
				line, over = line[:0], false
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if len(line) > 0 {
				fn(string(line))
			}
			return
		}
	}
}

// verifyRuntimeVersion is the confirm half of decide-then-confirm.
//
// The platform decided which interpreter this app builds against from a
// *declaration* — RUNNER_PYTHON_VERSION / RUNNER_NODE_VERSION, set beside the
// RUNNER_IMAGE_* that names this image — because the decision has to be made
// before any pod exists: the build produces the venv and node_modules on the
// platform host, and by the time this process is running the artifact is
// already committed to an interpreter (ADR §64).
//
// So this runner's job is not to decide anything. It is to check the platform's
// belief against its own filesystem and refuse loudly when the two disagree,
// which is the only thing that turns a wrong RUNNER_*_VERSION from a silent
// mismatch into a configuration error somebody can read.
//
// The last time these drifted, the platform resolved Python 3.11 on bookworm
// while the runner's family-python image was already on 3.12: the same app got
// a different Python depending on which execution mode it landed in, and
// because a venv records its interpreter's absolute path, the mismatch also
// dangled venv/bin/python inside the workload. Nothing held the two in step but
// a comment in each Dockerfile.
//
// A workload with nothing declared is not refused. An install that has not set
// the variable is an install that has said nothing, and absent is not a claim
// this can contradict — the platform's build log says so in those words on
// every deploy of such an app. Static workloads and Go workloads reach here with
// nothing declared by construction: one has no interpreter and the other ships
// a binary and leaves its toolchain on the build host.
func verifyRuntimeVersion(boot *BootstrapResponse, shipper *logShipper) error {
	if boot.RuntimeLanguage == "" || boot.RuntimeVersion == "" {
		return nil
	}

	have := toolchain.Available(boot.RuntimeLanguage)
	if len(have) == 0 {
		return fmt.Errorf("the platform expects this image to carry %s %s, and it has no %s at "+
			"all. RUNNER_%s_VERSION and RUNNER_IMAGE_%s do not describe the same image",
			toolchain.Label(boot.RuntimeLanguage), boot.RuntimeVersion,
			toolchain.Label(boot.RuntimeLanguage),
			strings.ToUpper(boot.RuntimeLanguage), strings.ToUpper(boot.RuntimeLanguage))
	}

	confirmed, missing := matchDeclaredSeries(boot.RuntimeLanguage, boot.RuntimeVersion, have)

	if len(missing) == 0 {
		shipper.Logf("stdout", "==> %s %s, as declared",
			toolchain.Label(boot.RuntimeLanguage), strings.Join(confirmed, ", "))
		return nil
	}

	found := make([]string, 0, len(have))
	for _, v := range have {
		found = append(found, v.Version)
	}
	return fmt.Errorf("the platform expects this image to carry %s %s; it has %s, so %s is "+
		"missing. A release built against a declared version has a dependency tree belonging "+
		"to an interpreter that is not here. Set RUNNER_%s_VERSION to what RUNNER_IMAGE_%s "+
		"actually contains, or point RUNNER_IMAGE_%s at an image that matches",
		toolchain.Label(boot.RuntimeLanguage), boot.RuntimeVersion, strings.Join(found, ", "),
		strings.Join(missing, ", "),
		strings.ToUpper(boot.RuntimeLanguage), strings.ToUpper(boot.RuntimeLanguage),
		strings.ToUpper(boot.RuntimeLanguage))
}

// matchDeclaredSeries walks a RUNNER_*_VERSION declaration against what this
// image actually has, and reports both halves: the versions found, and the
// declared series with nothing behind them.
//
// **Every declared series, not the first that matches** (ADR §64 D19). The
// declaration is a list because an image may carry several interpreters, and
// the failure this catches is precisely the one a list makes possible: an image
// that has the default and none of the others, against which the platform will
// happily resolve an app to a series no pod can run. Checking only the default
// would confirm the one version that was never in doubt.
//
// Split out from verifyRuntimeVersion so the policy can be tested without the
// probe. internal/toolchain answers once per process — which is right for a
// runner that starts, checks itself and execs an app, and means a test that
// reaches the probe is a test that depends on what else in the package asked
// first, and on which interpreters the machine running it happens to carry.
func matchDeclaredSeries(lang, declared string, have []toolchain.Version) (confirmed, missing []string) {
	for _, want := range strings.Split(declared, ",") {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		found := ""
		for _, v := range have {
			if toolchain.MatchSeries(lang, v.Version, want) {
				found = v.Version
				break
			}
		}
		if found == "" {
			missing = append(missing, want)
			continue
		}
		confirmed = append(confirmed, found)
	}
	return confirmed, missing
}

// scratchDir is where TMPDIR and TOFT_SCRATCH_DIR point: the pod's fixed
// mount, or the run's own directory on the host. Set once by Run, before any
// app code.
var scratchDir = ScratchMountPath
