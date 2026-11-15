// Command toft-runner is the process that runs inside a runner image.
//
// One binary, two modes. Build mode fetches source, installs dependencies, and
// emits a ready-to-run tree; run mode fetches that tree and execs it as a
// service or a one-shot job. Because both are the same image, the build
// environment *is* the run environment — which dissolves the problem that a
// virtualenv built on one host does not transplant into a container, and is
// what makes an app movable between execution modes without a rebuild.
//
// It is a static binary with no platform code inside it: the image contains a
// language runtime and this, and nothing else of Toft's. What holds the
// two halves together is the versioned callback contract in internal/runner,
// not a shared deployment.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"toft/internal/runner"
	"toft/internal/staticserve"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("toft-runner: ")

	// A static app's workload: this same binary, re-executed through
	// /proc/self/exe by the launcher above it. Dispatched before anything reads
	// the environment because it needs none of it — which is also what lets the
	// static family image be an empty base with this binary in it and nothing
	// else. The serving code is shared with the platform binary; an app that
	// behaved differently across execution modes would make the mode a one-way
	// door.
	if len(os.Args) > 1 && os.Args[1] == staticserve.CommandName {
		// Labeled `app`, and not because it is more accurate — this is
		// Toft's own code. The runner starts this subcommand as the app in
		// both modes and pumps its output as the app's, so a static app reads
		// the same wherever it runs: the outcome that must not happen is the
		// two modes disagreeing about the same binary.
		log.SetOutput(runner.MarkedWriter(os.Stderr, runner.LogSourceApp))
		if err := staticserve.Serve(os.Args[2:]); err != nil {
			log.Fatalf("%s: %v", staticserve.CommandName, err)
		}
		return
	}

	// Everything this binary says on its own behalf is the platform speaking.
	// These lines never touch the shipper — they go straight to stderr — so the
	// tee cannot mark them, and an unmarked line reads as the app's output.
	log.SetOutput(runner.MarkedWriter(os.Stderr, runner.LogSourcePlatform))

	// The supervisor's SIGTERM has to reach the app process, not just this
	// one, so that a rolling replacement drains rather than drops connections.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opts := runner.OptionsFromEnv()
	if opts.CallbackURL == "" {
		// Nothing to report to, so say it on stderr and exit non-zero: the
		// platform's supervisor is what catches a workload that never called
		// home. The identity is a mounted file and is checked where it is read,
		// which is the one place that can say what was missing.
		log.Fatalf("no callback URL in the environment — " +
			"this binary is started by Toft, not by hand")
	}

	if err := runner.Run(ctx, opts); err != nil {
		log.Printf("%v", err)
		var exit runner.AppExit
		if errors.As(err, &exit) && exit.Code > 0 && exit.Code < 256 {
			runner.ExitAs(exit)
		}
		os.Exit(1)
	}
}
