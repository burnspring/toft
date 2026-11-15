//go:build !linux

package agent

import (
	"context"
	"errors"
)

// The agent places workloads with Linux's own primitives — pidfds, FIFOs,
// cgroups — and has no other implementation.
var errLinuxOnly = errors.New("the shared-mode agent runs on Linux only")

func Main([]string) error { return errLinuxOnly }

func Ensure(context.Context, string, string, []string, ...string) (*Client, error) {
	return nil, errLinuxOnly
}

func ShimMain() error { return errLinuxOnly }

// ShimCommand is the namespace-tier shim's subcommand.
const ShimCommand = "agent-shim"
