//go:build linux

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// The namespace tier's network (ADR §135): a bridge, one address per
// workload, nothing between workloads.
//
// The agent owns bridge toft0 with the gateway at the range's first address.
// Every workload gets a network namespace of its own and a veth pair whose
// host end is a bridge port with **port isolation** on — an isolated port
// forwards only to the bridge itself, never to another isolated port, so no
// workload can reach another at layer 2, which is where two addresses on one
// bridge would otherwise meet without iptables ever seeing the packet. The
// rest is the agent's own chains, flushed and rewritten at every start so it
// never edits a rule it did not write: forward the range's own traffic out and
// its replies back, drop the cloud metadata endpoints when BLOCK_AWS_METADATA
// is set, masquerade the range's egress, and accept from the range on the
// platform's own port and nothing else of the host's.

// The candidate ranges, in order, of which the first that overlaps no route
// and no interface address wins (D13). AGENT_BRIDGE_CIDR overrides the pick.
var bridgeCandidates = []string{"10.88.0.0/16", "172.30.0.0/16", "100.71.0.0/16", "192.168.240.0/20"}

// The cloud metadata endpoints, dropped from the range when the platform blocks
// them (docker/entrypoint.sh holds the same list for the lower tiers).
var metadataAddrs = []string{"169.254.169.254", "169.254.170.2"}

// netConfig is the namespace tier's install-wide half.
type netConfig struct {
	bridge  string // the bridge's name, toft0
	chain   string // the prefix of the agent's own chains, TOFT
	prefix  netip.Prefix
	gateway netip.Addr
	// platformPort is the platform's own port on the gateway, the one thing of
	// the host's a workload may reach.
	platformPort  int
	platformHost  string
	blockMetadata bool
	// plan is how a workload resolves names, decided before the chains are
	// written because the forwarder needs two accepts in TOFT-IN; dns is the
	// mode reached, and resolvConf the file the shim mounts ("" for none).
	plan       resolvPlan
	dns        string
	resolvConf string
}

// pickRange is the bridge's range: the override when one is given, otherwise
// the first candidate clear of every route and address this machine has.
func pickRange(override string) (netip.Prefix, error) {
	if override != "" {
		p, err := netip.ParsePrefix(override)
		if err != nil || !p.Addr().Is4() || p.Bits() > 29 {
			return netip.Prefix{}, fmt.Errorf("AGENT_BRIDGE_CIDR %q is not an IPv4 range of /29 or wider", override)
		}
		return p.Masked(), nil
	}
	taken, err := localPrefixes()
	if err != nil {
		return netip.Prefix{}, err
	}
	for _, c := range bridgeCandidates {
		p := netip.MustParsePrefix(c)
		clear := true
		for _, t := range taken {
			if t.Overlaps(p) {
				clear = false
				break
			}
		}
		if clear {
			return p, nil
		}
	}
	return netip.Prefix{}, errors.New("no free address range; set AGENT_BRIDGE_CIDR")
}

// localPrefixes is every IPv4 route and interface address on this machine.
func localPrefixes() ([]netip.Prefix, error) {
	var out []netip.Prefix
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
			out = append(out, p.Masked())
		}
	}
	// /proc/net/route: destination and mask in little-endian hex.
	raw, err := os.ReadFile("/proc/net/route")
	if err == nil {
		for i, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if i == 0 || len(f) < 8 {
				continue
			}
			dst, err1 := strconv.ParseUint(f[1], 16, 32)
			mask, err2 := strconv.ParseUint(f[7], 16, 32)
			if err1 != nil || err2 != nil || mask == 0 {
				continue
			}
			d := netip.AddrFrom4([4]byte{byte(dst), byte(dst >> 8), byte(dst >> 16), byte(dst >> 24)})
			bits := 0
			for m := uint32(mask); m != 0; m >>= 1 {
				bits += int(m & 1)
			}
			out = append(out, netip.PrefixFrom(d, bits).Masked())
		}
	}
	return out, nil
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setupBridge makes the bridge and the agent's chains. Idempotent: an agent
// that restarts finds the bridge it left and keeps it, so a workload adopted
// across the restart keeps its address.
func (n *netConfig) setupBridge() error {
	if _, err := net.InterfaceByName(n.bridge); err != nil {
		if err := run("ip", "link", "add", n.bridge, "type", "bridge"); err != nil {
			return err
		}
	}
	if err := disableIPv6(n.bridge); err != nil {
		return err
	}
	gw := netip.PrefixFrom(n.gateway, n.prefix.Bits()).String()
	_ = run("ip", "addr", "replace", gw, "dev", n.bridge)
	if err := run("ip", "link", "set", n.bridge, "up"); err != nil {
		return err
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0); err != nil {
		return fmt.Errorf("enabling IPv4 forwarding: %w", err)
	}
	return n.setupChains()
}

// setupChains rewrites the agent's own chains from nothing, and hooks each in
// once. A rule it did not write is never touched.
func (n *netConfig) setupChains() error {
	fwd, in, nat := n.chain+"-FWD", n.chain+"-IN", n.chain+"-NAT"
	rangeS := n.prefix.String()
	for _, c := range []struct{ table, chain, hook string }{
		{"filter", fwd, "FORWARD"}, {"filter", in, "INPUT"}, {"nat", nat, "POSTROUTING"},
	} {
		_ = run("iptables", "-t", c.table, "-N", c.chain)
		if err := run("iptables", "-t", c.table, "-F", c.chain); err != nil {
			return err
		}
		if run("iptables", "-t", c.table, "-C", c.hook, "-j", c.chain) != nil {
			if err := run("iptables", "-t", c.table, "-I", c.hook, "1", "-j", c.chain); err != nil {
				return err
			}
		}
	}
	rules := [][]string{}
	if n.blockMetadata {
		for _, a := range metadataAddrs {
			rules = append(rules, []string{"-A", fwd, "-s", rangeS, "-d", a, "-j", "DROP"},
				[]string{"-A", in, "-s", rangeS, "-d", a, "-j", "DROP"})
		}
	}
	rules = append(rules,
		// Between two workloads, should anything route rather than bridge.
		[]string{"-A", fwd, "-s", rangeS, "-d", rangeS, "-j", "DROP"},
		// The range's own traffic out, and the replies back in.
		[]string{"-A", fwd, "-s", rangeS, "!", "-d", rangeS, "-j", "ACCEPT"},
		[]string{"-A", fwd, "-d", rangeS, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		// Of the host's own sockets, the platform's port and replies only.
		[]string{"-A", in, "-s", rangeS, "-p", "tcp", "--dport", strconv.Itoa(n.platformPort), "-j", "ACCEPT"},
		[]string{"-A", in, "-s", rangeS, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		[]string{"-A", in, "-s", rangeS, "-p", "icmp", "-j", "ACCEPT"},
	)
	if n.plan.mode == DNSForwarder {
		// The DNS forwarder on the gateway, for a host whose resolver is
		// on its own loopback (dns_linux.go).
		gw := n.gateway.String()
		rules = append(rules,
			[]string{"-A", in, "-s", rangeS, "-d", gw, "-p", "udp", "--dport", "53", "-j", "ACCEPT"},
			[]string{"-A", in, "-s", rangeS, "-d", gw, "-p", "tcp", "--dport", "53", "-j", "ACCEPT"})
	}
	rules = append(rules, []string{"-A", in, "-s", rangeS, "-j", "DROP"})
	for _, r := range rules {
		if err := run("iptables", append([]string{"-t", "filter"}, r...)...); err != nil {
			return err
		}
	}
	return run("iptables", "-t", "nat", "-A", nat, "-s", rangeS, "!", "-o", n.bridge, "-j", "MASQUERADE")
}

// teardown removes the bridge and the agent's chains — when the tier was
// refused partway, so a machine is never left half-configured.
func (n *netConfig) teardown() {
	for _, c := range []struct{ table, chain, hook string }{
		{"filter", n.chain + "-FWD", "FORWARD"}, {"filter", n.chain + "-IN", "INPUT"}, {"nat", n.chain + "-NAT", "POSTROUTING"},
	} {
		_ = run("iptables", "-t", c.table, "-D", c.hook, "-j", c.chain)
		_ = run("iptables", "-t", c.table, "-F", c.chain)
		_ = run("iptables", "-t", c.table, "-X", c.chain)
	}
	_ = run("ip", "link", "del", n.bridge)
}

// reachable reports whether a namespaced workload will reach the platform on
// the gateway: only a platform bound to every interface answers there, and one
// bound to loopback alone cannot be reached from inside a namespace — the tier
// is refused rather than every runner failing its bootstrap. Decided from the
// bind address the platform was started with, never by dialling: the agent
// starts before the platform's listeners do.
func (n *netConfig) reachable() error {
	switch n.platformHost {
	case "", "0.0.0.0", "::", n.gateway.String():
		return nil
	}
	return fmt.Errorf("the platform listens on %s only, which a workload in its own namespace cannot reach: bind it to every interface (HOST=0.0.0.0)", n.platformHost)
}

// allocate gives name an address in the range, the one it already holds when
// it has one: an address is kept by name for the workload's life, as a port is
// at the lower tiers. Called with a.mu held.
func (a *Agent) allocate(name string) (netip.Addr, error) {
	if addr, ok := a.addrs[name]; ok {
		return addr, nil
	}
	used := map[netip.Addr]bool{}
	for _, v := range a.addrs {
		used[v] = true
	}
	n := a.net
	for addr := n.gateway.Next(); n.prefix.Contains(addr); addr = addr.Next() {
		if used[addr] || addr == lastAddr(n.prefix) {
			continue
		}
		a.addrs[name] = addr
		return addr, nil
	}
	return netip.Addr{}, fmt.Errorf("the range %s has no address left", n.prefix)
}

// lastAddr is a range's broadcast address, never given out.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// vethName is the host end of a workload's pair, from its address: unique while
// the address is held, and short enough for the 15-byte interface name.
func vethName(addr netip.Addr) string {
	b := addr.As4()
	return fmt.Sprintf("tw%02x%02x%02x", b[1], b[2], b[3])
}

// plug gives a process started in a namespace of its own its end of a veth
// pair: the host end a port of the bridge with isolation on, the other end in
// the process's namespace, where the shim names it eth0.
func (n *netConfig) plug(pid int, addr netip.Addr) (peer string, err error) {
	host := vethName(addr)
	peer = "tp" + host[2:]
	_ = run("ip", "link", "del", host) // a pair left by a process that died unplugged
	if err := run("ip", "link", "add", host, "type", "veth", "peer", "name", peer); err != nil {
		return "", err
	}
	if err := disableIPv6(host); err != nil {
		_ = run("ip", "link", "del", host)
		return "", err
	}
	steps := [][]string{
		{"link", "set", peer, "netns", strconv.Itoa(pid)},
		{"link", "set", host, "master", n.bridge},
		{"link", "set", host, "type", "bridge_slave", "isolated", "on"},
		{"link", "set", host, "up"},
	}
	for _, s := range steps {
		if err := run("ip", s...); err != nil {
			_ = run("ip", "link", "del", host)
			return "", err
		}
	}
	return peer, nil
}

// disableIPv6 takes IPv6 off one interface of the current network namespace,
// before it is up, so it never autoconfigures a link-local address. The
// namespace tier's gate is IPv4 (TOFT-IN, TOFT-FWD): with fe80:: on the bridge
// and on eth0 a workload reached every host socket bound to `::` past it — a
// metrics listener, a dual-stack Postgres, anything on the box. There is no
// IPv6 on the workload's link, which is what keeps the gate whole; a kernel
// without IPv6 has nothing to take off.
func disableIPv6(dev string) error {
	err := os.WriteFile("/proc/sys/net/ipv6/conf/"+dev+"/disable_ipv6", []byte("1"), 0)
	if err == nil || (errors.Is(err, fs.ErrNotExist) && !ipv6Present()) {
		return nil
	}
	return fmt.Errorf("disabling IPv6 on %s: %w", dev, err)
}

// ipv6Present reports whether this namespace has IPv6 at all.
func ipv6Present() bool {
	_, err := os.Stat("/proc/sys/net/ipv6")
	return err == nil
}

// hasCaps reports whether this process holds CAP_SYS_ADMIN and CAP_NET_ADMIN,
// which the namespace tier needs to make namespaces and a bridge.
func hasCaps() bool {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
			eff, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			if err != nil {
				return false
			}
			const netAdmin, sysAdmin = 12, 21
			return eff&(1<<netAdmin) != 0 && eff&(1<<sysAdmin) != 0
		}
	}
	return false
}

// nsFlags are the namespaces every workload gets at this tier.
const nsFlags = unix.CLONE_NEWNET | unix.CLONE_NEWNS | unix.CLONE_NEWPID

// trialUnshare starts a process in all three namespaces and requires it to
// succeed: the capability check is necessary and not sufficient — a seccomp
// profile or a user-namespace sandbox refuses one anyway.
func trialUnshare() error {
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: nsFlags}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("a trial process in its own network, mount and PID namespace failed: %v", err)
	}
	return nil
}

// detectNamespaces establishes the namespace tier, or says why not. Called
// once, at start, with the cgroup tier already held.
func (a *Agent) detectNamespaces() (*netConfig, error) {
	if !a.cfg.Namespaces {
		return nil, errors.New("not asked for")
	}
	if os.Geteuid() != 0 || !hasCaps() {
		return nil, errors.New("this agent does not hold CAP_SYS_ADMIN and CAP_NET_ADMIN")
	}
	for _, tool := range []string{"iptables", "ip"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("%s is not on PATH", tool)
		}
	}
	if err := trialUnshare(); err != nil {
		return nil, err
	}
	if a.cfg.PlatformPort <= 0 {
		return nil, errors.New("the platform's port is not known")
	}
	prefix, err := a.bridgeRange()
	if err != nil {
		return nil, err
	}
	n := &netConfig{
		bridge: a.cfg.BridgeName, chain: a.cfg.ChainPrefix, prefix: prefix,
		gateway: prefix.Addr().Next(), platformPort: a.cfg.PlatformPort,
		platformHost: a.cfg.PlatformHost, blockMetadata: a.cfg.BlockMetadata,
	}
	n.plan = a.readResolvPlan(n.gateway)
	if err := n.setupBridge(); err != nil {
		n.teardown()
		return nil, err
	}
	if err := n.reachable(); err != nil {
		n.teardown()
		return nil, err
	}
	return n, nil
}

// bridgeRange is the range this agent uses: the one its state recorded (an
// agent restart), then the platform's remembered or configured one (a reboot,
// or AGENT_BRIDGE_CIDR), then a fresh pick. A range is picked once and kept.
func (a *Agent) bridgeRange() (netip.Prefix, error) {
	if a.stateBridge != "" {
		if p, err := netip.ParsePrefix(a.stateBridge); err == nil {
			return p, nil
		}
	}
	return pickRange(a.cfg.BridgeCIDR)
}

// sortedAddrs is the allocation table, for the state file.
func (a *Agent) sortedAddrs() map[string]string {
	out := map[string]string{}
	names := make([]string, 0, len(a.addrs))
	for n := range a.addrs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out[n] = a.addrs[n].String()
	}
	return out
}
