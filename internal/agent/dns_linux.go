//go:build linux

package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A namespaced workload's names (ADR §135).
//
// The shim keeps the host's /etc, so a workload reads the host's resolv.conf —
// and on a host whose resolver is a stub on loopback (systemd-resolved's
// 127.0.0.53, Ubuntu's default; Docker's 127.0.0.11; a local dnsmasq), that
// address is the workload's own loopback, where nothing answers. Every
// outbound call by name fails, which looks like a network outage scoped to
// apps. So the agent decides once, at the start of the tier, what a workload
// resolves through, taking the least it can:
//
//   - DNSHost: the host's file names a server a workload can reach. Nothing
//     is mounted; the workload reads what the host reads.
//   - DNSUpstreamFile: it does not, but systemd-resolved's own upstream list
//     does. A file naming those servers is mounted over /etc/resolv.conf.
//   - DNSForwarder: nothing names an upstream. The agent relays queries from
//     the gateway's port 53 to the host's own resolver, verbatim, and the
//     mounted file names the gateway.
//   - DNSUnavailable: the forwarder could not bind. The tier still starts,
//     with the host's file, and the system check says so.
//
// The host's file is read once; a change to it is noticed when the agent next
// starts the tier, which is when the file is regenerated. The four modes are
// in protocol.go, where the platform reads them.

// The host's two resolver files: the one every program reads, and the list of
// real upstreams systemd-resolved keeps behind its stub.
const (
	hostResolvConf     = "/etc/resolv.conf"
	upstreamResolvConf = "/run/systemd/resolve/resolv.conf"
)

// resolvPlan is the tier's decision: the mode, the file to mount (nil for
// DNSHost) and, for the forwarder, where it relays to.
type resolvPlan struct {
	mode     string
	file     []byte
	upstream string // host:port, the forwarder's only
}

// planResolvers decides from the host's file and systemd-resolved's upstream
// file (nil when absent). gateway is the address a forwarder listens on.
//
// A workload's network is IPv4 only, so a server it can reach is an IPv4
// address off loopback; an IPv6 server is as unreachable from inside as
// 127.0.0.53.
func planResolvers(host, upstream []byte, gateway netip.Addr) resolvPlan {
	servers, rest := splitResolvConf(host)
	for _, s := range servers {
		if reachableResolver(s) {
			return resolvPlan{mode: DNSHost}
		}
	}
	if upstream != nil {
		up, _ := splitResolvConf(upstream)
		var usable []string
		for _, s := range up {
			if reachableResolver(s) {
				usable = append(usable, s)
			}
		}
		if len(usable) > 0 {
			return resolvPlan{mode: DNSUpstreamFile, file: resolvFile(usable, rest)}
		}
	}
	// glibc's own default with no nameserver line at all is 127.0.0.1.
	target := "127.0.0.1"
	if len(servers) > 0 {
		target = servers[0]
	}
	return resolvPlan{
		mode:     DNSForwarder,
		file:     resolvFile([]string{gateway.String()}, rest),
		upstream: net.JoinHostPort(target, "53"),
	}
}

// splitResolvConf is a resolv.conf's nameservers, and every other directive
// line — search, domain, options — kept as written.
func splitResolvConf(b []byte) (servers, rest []string) {
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") || strings.HasPrefix(f[0], ";") {
			continue
		}
		if f[0] == "nameserver" {
			if len(f) > 1 {
				servers = append(servers, f[1])
			}
			continue
		}
		rest = append(rest, strings.TrimSpace(line))
	}
	return servers, rest
}

func reachableResolver(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is4() && !a.IsLoopback()
}

func resolvFile(servers, rest []string) []byte {
	var b strings.Builder
	b.WriteString("# Written by the Toft agent for workloads in their own network namespace.\n")
	for _, s := range servers {
		b.WriteString("nameserver " + s + "\n")
	}
	for _, r := range rest {
		b.WriteString(r + "\n")
	}
	return []byte(b.String())
}

// readResolvPlan reads the host's files and decides. A host file that cannot be
// read at all leaves the workload with whatever it reads itself.
func (a *Agent) readResolvPlan(gateway netip.Addr) resolvPlan {
	hostPath, upPath := hostResolvConf, upstreamResolvConf
	if a.cfg.ResolvConf != "" {
		hostPath = a.cfg.ResolvConf
	}
	if a.cfg.UpstreamResolvConf != "" {
		upPath = a.cfg.UpstreamResolvConf
	}
	host, err := os.ReadFile(hostPath)
	if err != nil {
		return resolvPlan{mode: DNSHost}
	}
	up, err := os.ReadFile(upPath)
	if err != nil {
		up = nil
	}
	return planResolvers(host, up, gateway)
}

// startDNS carries out the plan once the bridge is up: the forwarder bound on
// the gateway, and the file written for the shim to mount. It returns the
// mode reached, which is the plan's unless the forwarder could not bind.
func (a *Agent) startDNS(n *netConfig, plan resolvPlan) string {
	if plan.mode == DNSHost {
		return DNSHost
	}
	if plan.mode == DNSForwarder {
		f, err := startForwarder(net.JoinHostPort(n.gateway.String(), "53"), plan.upstream)
		if err != nil {
			logf("namespace tier: no DNS forwarder on %s: %v; workloads keep the host's resolv.conf", n.gateway, err)
			return DNSUnavailable
		}
		a.fwd = f
	}
	path := filepath.Join(a.cfg.Dir, "resolv.conf")
	if err := os.WriteFile(path, plan.file, 0o644); err != nil {
		logf("namespace tier: writing %s: %v; workloads keep the host's resolv.conf", path, err)
		if a.fwd != nil {
			a.fwd.close()
			a.fwd = nil
		}
		return DNSUnavailable
	}
	// WriteFile keeps an existing file's mode; the workload reads this as its
	// own account.
	_ = os.Chmod(path, 0o644)
	n.resolvConf = path
	return plan.mode
}

// dnsForwarder relays DNS from the gateway to the host's resolver: each query
// as it came, each answer as it went, no parsing and no cache. It listens
// where only the range reaches — the gateway, through TOFT-IN's two port-53
// accepts.
type dnsForwarder struct {
	udp      *net.UDPConn
	tcp      *net.TCPListener
	upstream string
	slots    chan struct{}
	wg       sync.WaitGroup

	// perSource counts each source address's queries in flight, so one
	// workload cannot take every slot and cut name resolution for every other
	// namespaced app on the box. A workload's source address is its own veth's
	// and cannot be spoofed as another's.
	mu        sync.Mutex
	perSource map[string]int
}

const (
	dnsTimeout  = 5 * time.Second
	dnsInFlight = 256
	// dnsPerSource is how many of the slots one source may hold at once.
	dnsPerSource = 16
)

// acquire takes a slot for one query from src: its own share first, then the
// global one. False means drop the query; release undoes a true.
func (f *dnsForwarder) acquire(src string) bool {
	f.mu.Lock()
	if f.perSource[src] >= dnsPerSource {
		f.mu.Unlock()
		return false
	}
	f.perSource[src]++
	f.mu.Unlock()
	select {
	case f.slots <- struct{}{}:
		return true
	default:
		f.releaseSource(src)
		return false
	}
}

func (f *dnsForwarder) release(src string) {
	<-f.slots
	f.releaseSource(src)
}

func (f *dnsForwarder) releaseSource(src string) {
	f.mu.Lock()
	if f.perSource[src]--; f.perSource[src] <= 0 {
		delete(f.perSource, src)
	}
	f.mu.Unlock()
}

func startForwarder(listen, upstream string) (*dnsForwarder, error) {
	ua, err := net.ResolveUDPAddr("udp4", listen)
	if err != nil {
		return nil, err
	}
	udp, err := net.ListenUDP("udp4", ua)
	if err != nil {
		return nil, err
	}
	ta, _ := net.ResolveTCPAddr("tcp4", listen)
	tcp, err := net.ListenTCP("tcp4", ta)
	if err != nil {
		udp.Close()
		return nil, err
	}
	f := &dnsForwarder{udp: udp, tcp: tcp, upstream: upstream, slots: make(chan struct{}, dnsInFlight),
		perSource: map[string]int{}}
	f.wg.Add(2)
	go f.serveUDP()
	go f.serveTCP()
	return f, nil
}

func (f *dnsForwarder) close() {
	f.udp.Close()
	f.tcp.Close()
	f.wg.Wait()
}

func (f *dnsForwarder) serveUDP() {
	defer f.wg.Done()
	for {
		buf := make([]byte, 65535)
		n, from, err := f.udp.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		src := from.IP.String()
		if !f.acquire(src) {
			continue // a client retries; a flood is not relayed
		}
		go func(q []byte, from *net.UDPAddr) {
			defer f.release(src)
			if answer, err := exchangeUDP(f.upstream, q); err == nil {
				_, _ = f.udp.WriteToUDP(answer, from)
			}
		}(buf[:n], from)
	}
}

func exchangeUDP(upstream string, q []byte) ([]byte, error) {
	c, err := net.DialTimeout("udp", upstream, dnsTimeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(dnsTimeout))
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (f *dnsForwarder) serveTCP() {
	defer f.wg.Done()
	for {
		c, err := f.tcp.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		src := ""
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			src = a.IP.String()
		}
		if !f.acquire(src) {
			c.Close()
			continue
		}
		go func() {
			defer f.release(src)
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
			defer cancel()
			up, err := (&net.Dialer{}).DialContext(ctx, "tcp", f.upstream)
			if err != nil {
				return
			}
			defer up.Close()
			deadline := time.Now().Add(4 * dnsTimeout)
			_ = c.SetDeadline(deadline)
			_ = up.SetDeadline(deadline)
			go func() { _, _ = io.Copy(up, c) }()
			_, _ = io.Copy(c, up)
		}()
	}
}
