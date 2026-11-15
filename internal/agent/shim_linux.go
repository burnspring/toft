//go:build linux

package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ShimCommand is the platform binary's subcommand that is a namespace-tier
// workload's first process (ADR §136). It is PID 1 of a PID namespace of its
// own, in a mount and a network namespace of its own, and root inside them for
// exactly as long as it takes to build the workload's view of the machine:
//
//   - it waits for the agent to plug its end of the veth pair, then names it
//     eth0 and gives it the workload's address and the gateway;
//   - it covers every directory the workload must not see with an empty tmpfs
//     — the whole of APPS_DIR, LOGS_DIR, CACHE_DIR and the agent's own
//     directory — and puts back the few it must: its own app's tree and its own
//     identity file. What is put back is cloned *before* the cover goes on,
//     because once it is on there is nothing underneath to reach;
//   - it mounts a private /tmp and a fresh /proc;
//   - then it drops to the app's account and execs the runner, which keeps
//     PID 1 and reaps for the rest of the workload's life (internal/runner's
//     reap.go).
//
// It is the platform's own binary rather than a helper in the image for the
// runner's reason: one binary, and a host never holds two versions of the
// launcher.
const ShimCommand = "agent-shim"

// envShim carries the shim's configuration. Unset before the runner starts.
const envShim = "TOFT_AGENT_SHIM"

type shimConfig struct {
	Address string   `json:"address"` // CIDR, the workload's own
	Gateway string   `json:"gateway"`
	Peer    string   `json:"peer"` // the veth end, renamed eth0
	Mask    []string `json:"mask"`
	Keep    []string `json:"keep"`
	// ResolvConf is a file the agent wrote under its own (masked) directory,
	// mounted over /etc/resolv.conf when the host's names a resolver the
	// workload cannot reach (dns_linux.go). Empty leaves the host's.
	ResolvConf string   `json:"resolv_conf,omitempty"`
	UID        int      `json:"uid"`
	GID        int      `json:"gid"`
	Groups     []int    `json:"groups"`
	Exe        string   `json:"exe"`
	Args       []string `json:"args"`
}

// ShimMain is `toft agent-shim`. It returns only on failure, which the agent
// reads as the start failing.
func ShimMain() error {
	var cfg shimConfig
	if err := json.Unmarshal([]byte(os.Getenv(envShim)), &cfg); err != nil {
		return fmt.Errorf("unreadable configuration: %w", err)
	}
	os.Unsetenv(envShim)

	// Wait for the agent: it plugs the veth once this process exists, and
	// writes one line on fd 3 when it has.
	ready := os.NewFile(3, "ready")
	line, err := bufio.NewReader(ready).ReadString('\n')
	ready.Close()
	if err != nil || strings.TrimSpace(line) != "go" {
		return fmt.Errorf("the agent did not finish plugging this workload in: %q %v", line, err)
	}

	if err := shimMounts(cfg); err != nil {
		return fmt.Errorf("building the mount namespace: %w", err)
	}
	if err := shimNetwork(cfg); err != nil {
		return fmt.Errorf("configuring the network namespace: %w", err)
	}

	groups := cfg.Groups
	if cfg.UID >= 0 {
		if err := syscall.Setgroups(groups); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := syscall.Setgid(cfg.GID); err != nil {
			return fmt.Errorf("setgid: %w", err)
		}
		if err := syscall.Setuid(cfg.UID); err != nil {
			return fmt.Errorf("setuid: %w", err)
		}
	}
	return syscall.Exec(cfg.Exe, append([]string{cfg.Exe}, cfg.Args...), os.Environ())
}

func shimMounts(cfg shimConfig) error {
	// Nothing done here may reach the host's mounts.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making / private: %w", err)
	}
	type held struct {
		fd     int
		target string
	}
	// Everything kept is cloned first, before any cover goes on — /tmp's
	// included: a release or a run directory under /tmp (JOB_RUN_DIR=/tmp)
	// would otherwise be hidden by the private /tmp below.
	covers := append([]string{"/tmp"}, cfg.Mask...)
	var keep []held
	for _, k := range cfg.Keep {
		if !underAny(k, covers) {
			continue
		}
		fd, err := holdBeneath(k, covers)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		keep = append(keep, held{fd, k})
	}
	// The resolver file lives in the agent's directory, which is masked below,
	// so it too is held before the covers go on.
	resolv := -1
	if cfg.ResolvConf != "" {
		fd, err := unix.OpenTree(unix.AT_FDCWD, cfg.ResolvConf, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
		if err != nil {
			return fmt.Errorf("holding %s: %w", cfg.ResolvConf, err)
		}
		resolv = fd
	}
	// A private /tmp: what one app writes to a literal /tmp is no other's.
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777"); err != nil {
		return fmt.Errorf("a private /tmp: %w", err)
	}
	for _, m := range cfg.Mask {
		if _, err := os.Stat(m); err != nil {
			continue // absent, or already under a cover
		}
		if err := unix.Mount("tmpfs", m, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=1m,mode=0711"); err != nil {
			return fmt.Errorf("covering %s: %w", m, err)
		}
	}
	for _, k := range keep {
		if err := os.MkdirAll(k.target, 0o711); err != nil {
			return fmt.Errorf("making room for %s: %w", k.target, err)
		}
		if err := unix.MoveMount(k.fd, "", unix.AT_FDCWD, k.target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
			return fmt.Errorf("putting back %s: %w", k.target, err)
		}
		unix.Close(k.fd)
	}
	if resolv >= 0 {
		// Over the file /etc/resolv.conf resolves to: on Ubuntu it is a
		// link into /run, and a mount does not go on a link.
		target, err := filepath.EvalSymlinks("/etc/resolv.conf")
		if err != nil {
			return fmt.Errorf("finding /etc/resolv.conf: %w", err)
		}
		if err := unix.MoveMount(resolv, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
			return fmt.Errorf("mounting the workload's resolv.conf over %s: %w", target, err)
		}
		unix.Close(resolv)
	}
	// A /proc that shows this PID namespace and nothing else.
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mounting /proc: %w", err)
	}
	return nil
}

// holdBeneath clones the tree at keep, resolved beneath the innermost cover it
// lies under and never out of it.
//
// As root, and before any cover goes on. A Keep path can be app-writable — a
// folder of a non-partitioned storage resource sits in a 2770 directory
// another app may write — so a plain open followed whatever link was planted
// there: `ln -s / F` and the shim cloned the host's root, uncovered, into the
// other app's workload at its next respawn (security review 2026-11-14, RT-9).
// RESOLVE_BENEATH refuses an absolute link and any link that climbs out of the
// cover, while the release's own relative `current -> releases/<id>` still
// resolves; refusing every link would break every namespace-tier start.
func holdBeneath(keep string, covers []string) (int, error) {
	fd, err := openBeneath(keep, covers)
	if err != nil {
		return -1, err
	}
	defer unix.Close(fd)
	tree, err := unix.OpenTree(fd, "", unix.AT_EMPTY_PATH|unix.OPEN_TREE_CLONE|unix.AT_RECURSIVE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return -1, fmt.Errorf("holding %s: %w", keep, err)
	}
	return tree, nil
}

// openBeneath opens keep as an O_PATH descriptor, resolved beneath the
// innermost of covers it lies under. ENOENT is returned bare, for a Keep that
// is simply absent; every other refusal is a sentence that fails the start.
func openBeneath(keep string, covers []string) (int, error) {
	cover := ""
	for _, c := range covers {
		if underAny(keep, []string{c}) && len(c) > len(cover) {
			cover = c
		}
	}
	dfd, err := unix.Open(cover, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	rel, _ := filepath.Rel(cover, keep)
	if rel == "." {
		return dfd, nil
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat2(dfd, rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH,
	})
	if errors.Is(err, unix.ENOENT) {
		return -1, err
	}
	if err != nil {
		return -1, fmt.Errorf("%s does not resolve inside %s (a link out of it?), so the workload cannot be given it: %w",
			keep, cover, err)
	}
	return fd, nil
}

// underAny reports whether path is one of dirs or below one.
func underAny(path string, dirs []string) bool {
	for _, d := range dirs {
		rel, err := filepath.Rel(d, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

// shimNetwork names the workload's end eth0 and gives it its address — and no
// IPv6: a fresh namespace starts with the kernel's default, not the host's, so
// eth0 would autoconfigure fe80:: and talk past the agent's IPv4 gate. Taken
// off eth0 before it is up, and off the namespace's default for any interface
// after it; loopback keeps ::1, which reaches nothing but the namespace. The
// sysctls read are this namespace's own through the /proc shimMounts mounted.
func shimNetwork(cfg shimConfig) error {
	ip := func(s ...string) error {
		if out, err := exec.Command("ip", s...).CombinedOutput(); err != nil {
			return fmt.Errorf("ip %s: %v: %s", strings.Join(s, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := ip("link", "set", "lo", "up"); err != nil {
		return err
	}
	if err := ip("link", "set", cfg.Peer, "name", "eth0"); err != nil {
		return err
	}
	for _, dev := range []string{"default", "eth0"} {
		if err := disableIPv6(dev); err != nil {
			return err
		}
	}
	for _, s := range [][]string{
		{"addr", "add", cfg.Address, "dev", "eth0"},
		{"link", "set", "eth0", "up"},
		{"route", "add", "default", "via", cfg.Gateway},
	} {
		if err := ip(s...); err != nil {
			return err
		}
	}
	return nil
}
