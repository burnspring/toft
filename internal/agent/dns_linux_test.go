//go:build linux

package agent

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// The resolver a namespaced workload is given, from the host's two files. The
// six hosts a workload meets: an upstream already named, Ubuntu's stub with
// systemd-resolved's upstream list behind it, the stub without one, Docker's
// embedded resolver, a local dnsmasq, and a file naming both kinds.
func TestPlanResolvers(t *testing.T) {
	gw := netip.MustParseAddr("10.88.0.1")
	stub := "# managed by systemd-resolved\nnameserver 127.0.0.53\noptions edns0 trust-ad\nsearch corp.example\n"
	for _, tc := range []struct {
		name           string
		host, upstream string // upstream "" is absent
		mode           string
		file           []string // lines the mounted file must hold
		notInFile      []string
		upstreamAddr   string
	}{
		{name: "upstream already named", host: "nameserver 10.0.0.2\nsearch ec2.internal\n", mode: DNSHost},
		{name: "systemd stub with its upstream list", host: stub,
			upstream: "nameserver 10.0.0.2\nnameserver 127.0.0.1\nnameserver 1.1.1.1\nsearch other.example\n",
			mode:     DNSUpstreamFile,
			file:     []string{"nameserver 10.0.0.2", "nameserver 1.1.1.1", "options edns0 trust-ad", "search corp.example"},
			notInFile: []string{"127.0.0.53", "127.0.0.1", "other.example"}},
		{name: "systemd stub, no upstream list", host: stub, mode: DNSForwarder,
			file: []string{"nameserver 10.88.0.1", "search corp.example"}, notInFile: []string{"127.0.0.53"},
			upstreamAddr: "127.0.0.53:53"},
		{name: "docker's embedded resolver", host: "nameserver 127.0.0.11\noptions ndots:0\n", mode: DNSForwarder,
			file: []string{"nameserver 10.88.0.1", "options ndots:0"}, upstreamAddr: "127.0.0.11:53"},
		{name: "local dnsmasq, an upstream list of loopback only", host: "nameserver 127.0.0.1\n",
			upstream: "nameserver 127.0.0.1\n", mode: DNSForwarder, upstreamAddr: "127.0.0.1:53"},
		{name: "mixed: one reachable server is enough", host: "nameserver 127.0.0.53\nnameserver 10.0.0.2\n", mode: DNSHost},
		{name: "IPv6 only is as unreachable as loopback", host: "nameserver fd00::2\n", mode: DNSForwarder,
			upstreamAddr: "[fd00::2]:53"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var up []byte
			if tc.upstream != "" {
				up = []byte(tc.upstream)
			}
			p := planResolvers([]byte(tc.host), up, gw)
			if p.mode != tc.mode {
				t.Fatalf("mode = %s, want %s", p.mode, tc.mode)
			}
			if tc.mode == DNSHost && p.file != nil {
				t.Errorf("a file for a host whose own file works:\n%s", p.file)
			}
			for _, l := range tc.file {
				if !strings.Contains(string(p.file), l+"\n") {
					t.Errorf("the file lacks %q:\n%s", l, p.file)
				}
			}
			for _, s := range tc.notInFile {
				if strings.Contains(string(p.file), s) {
					t.Errorf("the file holds %q:\n%s", s, p.file)
				}
			}
			if p.upstream != tc.upstreamAddr {
				t.Errorf("forwarder upstream = %q, want %q", p.upstream, tc.upstreamAddr)
			}
		})
	}
}

// The forwarder relays a query verbatim, over UDP and TCP, and the answer back.
func TestTheForwarderRelaysVerbatim(t *testing.T) {
	// An upstream that answers every query with "answer:" and the query.
	up, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := up.ReadFrom(buf)
			if err != nil {
				return
			}
			up.WriteTo(append([]byte("answer:"), buf[:n]...), from)
		}
	}()
	upTCP, err := net.Listen("tcp4", up.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer upTCP.Close()
	go func() {
		for {
			c, err := upTCP.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 512)
			n, _ := c.Read(buf)
			c.Write(append([]byte("answer:"), buf[:n]...))
			c.Close()
		}
	}()

	f, err := startForwarder("127.0.0.1:0", up.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()

	for _, network := range []string{"udp", "tcp"} {
		addr := f.udp.LocalAddr().String()
		if network == "tcp" {
			addr = f.tcp.Addr().String()
		}
		c, err := net.Dial(network, addr)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write([]byte("query"))
		buf := make([]byte, 512)
		n, err := c.Read(buf)
		c.Close()
		if err != nil || string(buf[:n]) != "answer:query" {
			t.Errorf("%s: got %q, %v", network, buf[:n], err)
		}
	}
}

// One workload cannot take every slot: a source holding more than its share
// in flight is refused, and another source's query is still relayed (RT-8).
func TestOneSourceCannotTakeEveryForwarderSlot(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	up, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := up.ReadFrom(buf)
			if err != nil {
				return
			}
			up.WriteTo(append([]byte("answer:"), buf[:n]...), from)
		}
	}()
	// The TCP upstream never answers: every connection relayed to it holds its
	// slot until the test ends.
	upTCP, err := net.Listen("tcp4", up.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer upTCP.Close()
	go func() {
		for {
			c, err := upTCP.Accept()
			if err != nil {
				return
			}
			go func() { <-done; c.Close() }()
		}
	}()

	f, err := startForwarder("127.0.0.1:0", up.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()

	flooder := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}, Timeout: time.Second}
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < dnsInFlight+10; i++ {
		c, err := flooder.Dial("tcp4", f.tcp.Addr().String())
		if err != nil {
			continue
		}
		c.Write([]byte("hang"))
		held = append(held, c)
	}
	time.Sleep(300 * time.Millisecond)

	other := &net.Dialer{LocalAddr: &net.UDPAddr{IP: net.ParseIP("127.0.0.3")}}
	c, err := other.Dial("udp4", f.udp.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("query"))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "answer:query" {
		t.Errorf("another source's query was not relayed while one source flooded: %q, %v", buf[:n], err)
	}
}
