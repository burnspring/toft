//go:build linux

package agent

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"
)

// Main is `toft agent`. It answers until it is told to stop, and leaves the
// estate running when it does: the next agent adopts it.
func Main(args []string) error {
	fs := flag.NewFlagSet(Command, flag.ContinueOnError)
	dir := fs.String("dir", DefaultDir, "the agent's directory: socket, state and identity files")
	resumed := fs.Bool("resume", false, "this process is an agent re-executed in place; its workloads are its children")
	namespaces := fs.Bool("namespaces", false, "reach for the namespace tier where this machine allows it")
	platformPort := fs.Int("platform-port", 0, "the platform's own port, the one a namespaced workload may reach on the gateway")
	platformHost := fs.String("platform-host", "", "the address the platform listens on; a namespaced workload reaches it only on every interface")
	bridgeCIDR := fs.String("bridge-cidr", "", "the bridge's range: the platform's remembered one, or AGENT_BRIDGE_CIDR")
	blockMetadata := fs.Bool("block-metadata", false, "drop the cloud metadata endpoints from the bridge's range")
	cgroupParent := fs.String("cgroup-parent", "", "the cgroup v2 directory workloads' cgroups go under; empty is the agent's own")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	if err := useLog(LogPath(*dir)); err != nil {
		return fmt.Errorf("opening the agent's log: %w", err)
	}
	// Its stdout and stderr are a log file today, but a reader that has gone
	// away must never be what stops the agent.
	signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)

	a, err := New(Config{Dir: *dir, CgroupParent: *cgroupParent, Namespaces: *namespaces,
		PlatformPort: *platformPort, PlatformHost: *platformHost, BridgeCIDR: *bridgeCIDR, BlockMetadata: *blockMetadata})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *resumed {
		logf("re-executed in place: adopted %d workload(s) as children", len(a.list().Workloads))
	}
	logf("answering on %s (protocol %s), running %s", SocketPath(*dir), ProtocolVersion, a.cfg.Exe)
	err = a.ServeUntilDone(ctx)
	a.Close(false)
	return err
}
