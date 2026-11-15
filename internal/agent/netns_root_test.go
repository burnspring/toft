//go:build linux

package agent

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ADR §135: workloads cannot reach each other at the namespace tier.
//
// Root only, and skipped otherwise: it makes a bridge, veths and iptables
// chains, under names of its own (toftt0, TOFTT-*) and a range of its own, in a
// cgroup parent of its own, and removes every one of them. Run it with
//
//	go test -c -o /tmp/agent.test ./internal/agent && sudo /tmp/agent.test -test.run Namespace -test.v
func TestNamespacedWorkloadsCannotReachEachOther(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: it builds a bridge and iptables chains")
	}
	cg := "/sys/fs/cgroup/toft-agent-test"
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Skipf("no cgroup v2 here: %v", err)
	}
	t.Cleanup(func() { removeCgroupTree(cg) })

	// The platform: something answering on every interface, so the gateway.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("platform\n"))
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	apps := filepath.Join(shortTempDir(t), "apps")
	for _, d := range []string{"app-a", "app-b"} {
		os.MkdirAll(filepath.Join(apps, d), 0o755)
		os.WriteFile(filepath.Join(apps, d, "marker"), []byte(d), 0o644)
	}

	f := newFixture(t, func(c *Config) {
		c.Namespaces, c.PlatformPort, c.BlockMetadata = true, port, true
		c.BridgeName, c.ChainPrefix, c.BridgeCIDR = "toftt0", "TOFTT", "10.249.0.0/24"
		c.CgroupParent = cg
	})
	t.Cleanup(func() { (&netConfig{bridge: "toftt0", chain: "TOFTT"}).teardown() })
	if l, _ := f.c.List(context.Background()); l.Tier != TierNamespace {
		t.Fatalf("the agent is at the %q tier: %s", l.Tier, l.TierReason)
	}

	b := f.spec("b", "ns-serve")
	b.Dir, b.Mask, b.Keep = filepath.Join(apps, "app-b"), []string{apps}, []string{filepath.Join(apps, "app-b")}
	sb := f.start(b)
	if sb.Address == "" {
		t.Fatalf("the namespaced workload has no address: %+v", sb)
	}

	// The host reaches a workload — the proxy's path.
	until(t, 10*time.Second, "the host to reach b", func() bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(sb.Address, fmt.Sprint(WorkloadPort)), time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	})

	a := f.spec("a", "ns-probe")
	a.Dir, a.Mask, a.Keep = filepath.Join(apps, "app-a"), []string{apps}, []string{filepath.Join(apps, "app-a")}
	a.Env["PROBE_PEER"] = net.JoinHostPort(sb.Address, fmt.Sprint(WorkloadPort))
	a.Env["PROBE_GATEWAY"] = net.JoinHostPort("10.249.0.1", fmt.Sprint(port))
	a.Env["PROBE_APPS"] = apps
	a.Env["TOFT_TEST_EXTERNAL"] = os.Getenv("TOFT_TEST_EXTERNAL")
	a.Restart, a.Kind = RestartNever, KindRun
	f.start(a)
	until(t, 20*time.Second, "the probe", func() bool { return f.status("a").State == StateExited })
	out, _ := os.ReadFile(filepath.Join(f.log, "a.log"))
	text := string(out)
	for _, want := range []string{
		"peer=refused", // another workload, on its own address
		"gateway=ok",   // the platform
		"metadata=refused",
		"apps=app-a", // its own tree, and no other app's
		"pid1=true",  // a PID namespace of its own
		"tmp=",       // a private /tmp: empty, or only what is kept under it
		"ipv6=0",     // no address the IPv4 gate does not see
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the probe did not report %s:\n%s", want, text)
		}
	}
	// Nor on the host's side of the link: the bridge and the workload's port.
	for _, dev := range []string{"toftt0", vethName(netip.MustParseAddr(sb.Address))} {
		if b, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + dev + "/disable_ipv6"); err == nil && strings.TrimSpace(string(b)) != "1" {
			t.Errorf("%s carries IPv6", dev)
		}
	}
	if ext := os.Getenv("TOFT_TEST_EXTERNAL"); ext != "" && !strings.Contains(text, "external=ok") {
		t.Errorf("the workload could not reach %s outside:\n%s", ext, text)
	}
}

func removeCgroupTree(dir string) {
	_ = exec.Command("find", dir, "-depth", "-type", "d", "-exec", "rmdir", "{}", ";").Run()
}

// A namespaced workload resolves a name when the host's resolver is on its own
// loopback — systemd-resolved's stub, Docker's 127.0.0.11, a local dnsmasq —
// through the agent's forwarder on the gateway. Without it every outbound
// call by name fails, and only inside apps. Root only, as above.
func TestANamespacedWorkloadResolvesThroughALoopbackResolver(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: it builds a bridge and iptables chains")
	}
	cg := "/sys/fs/cgroup/toft-agent-test-dns"
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Skipf("no cgroup v2 here: %v", err)
	}
	t.Cleanup(func() { removeCgroupTree(cg) })

	// The host's resolver: a stub on loopback that knows one name.
	stub, err := net.ListenPacket("udp4", "127.0.0.54:53")
	if err != nil {
		t.Skipf("cannot stand a stub resolver on 127.0.0.54:53: %v", err)
	}
	defer stub.Close()
	go serveOneName(stub, "toft-dns-test.example.", [4]byte{192, 0, 2, 7})

	conf := filepath.Join(shortTempDir(t), "resolv.conf")
	os.WriteFile(conf, []byte("nameserver 127.0.0.54\nsearch corp.example\n"), 0o644)

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	f := newFixture(t, func(c *Config) {
		c.Namespaces, c.PlatformPort = true, ln.Addr().(*net.TCPAddr).Port
		c.BridgeName, c.ChainPrefix, c.BridgeCIDR = "toftt1", "TOFTU", "10.249.1.0/24"
		c.CgroupParent = cg
		c.ResolvConf, c.UpstreamResolvConf = conf, filepath.Join(filepath.Dir(conf), "absent")
	})
	t.Cleanup(func() { (&netConfig{bridge: "toftt1", chain: "TOFTU"}).teardown() })
	l, _ := f.c.List(context.Background())
	if l.Tier != TierNamespace {
		t.Fatalf("the agent is at the %q tier: %s", l.Tier, l.TierReason)
	}
	if l.DNS != DNSForwarder {
		t.Fatalf("DNS = %q, want the forwarder", l.DNS)
	}

	apps := filepath.Join(shortTempDir(t), "apps")
	os.MkdirAll(filepath.Join(apps, "app-r"), 0o755)
	r := f.spec("r", "ns-resolve")
	r.Dir, r.Mask, r.Keep = filepath.Join(apps, "app-r"), []string{apps}, []string{filepath.Join(apps, "app-r")}
	r.Env["PROBE_NAME"] = "toft-dns-test.example"
	r.Restart, r.Kind = RestartNever, KindRun
	f.start(r)
	until(t, 20*time.Second, "the lookup", func() bool { return f.status("r").State == StateExited })
	out, _ := os.ReadFile(filepath.Join(f.log, "r.log"))
	if !strings.Contains(string(out), "resolved=[192.0.2.7]") {
		t.Errorf("the workload did not resolve through the forwarder:\n%s", out)
	}
	if !strings.Contains(string(out), "nameserver 10.249.1.1") || !strings.Contains(string(out), "search corp.example") {
		t.Errorf("the workload's resolv.conf is not the generated one:\n%s", out)
	}
}

// serveOneName answers A queries for name with addr, and every other query
// with no answer: enough DNS for a resolver to be satisfied.
func serveOneName(pc net.PacketConn, name string, addr [4]byte) {
	buf := make([]byte, 512)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		q := buf[:n]
		if n < 12 {
			continue
		}
		// The question: labels from byte 12, then type and class.
		i, labels := 12, []string{}
		for i < n && q[i] != 0 {
			l := int(q[i])
			if i+1+l > n {
				break
			}
			labels = append(labels, string(q[i+1:i+1+l]))
			i += 1 + l
		}
		end := i + 5 // the zero label, type, class
		if end > n {
			continue
		}
		qtype := int(q[i+1])<<8 | int(q[i+2])
		resp := append([]byte{}, q[:end]...)
		resp[2] = 0x81 // a response, recursion desired
		resp[3] = 0x80 // recursion available, no error
		resp[10], resp[11] = 0, 0
		resp[8], resp[9] = 0, 0
		if strings.EqualFold(strings.Join(labels, ".")+".", name) && qtype == 1 {
			resp[6], resp[7] = 0, 1
			resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, addr[0], addr[1], addr[2], addr[3])
		} else {
			resp[6], resp[7] = 0, 0
		}
		pc.WriteTo(resp, from)
	}
}
