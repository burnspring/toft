// Package safefetch is the one implementation of "this server is about to
// fetch a URL somebody else chose".
//
// A URL the platform fetches on its own initiative is server-side input
// regardless of who wrote it. A catalog must never be able to make this box
// read http://169.254.169.254/, and neither must an advisory feed endpoint, a
// mirror an operator typed, or whatever the next such feature is. That
// property is worth exactly one implementation: two guards that could disagree
// about what "this network" means is a bug nobody finds until it matters,
// which is the same argument that makes ResolveSourcePolicy and
// ResolveRetention one function each.
//
// It is deliberately not a trust model. Whether the *contents* of what comes
// back may be believed is the caller's question, and each caller answers it
// differently — the gallery's answer is TLS and one team's repository, the
// advisory feed's is TLS and OSV. This package only decides what may be
// dialled, how big the answer may be, and how many times it may be redirected.
package safefetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MaxRedirects caps a redirect chain. Every hop is re-checked, so the cap is
// about loops rather than about safety.
const MaxRedirects = 5

// CheckURL is the hygiene every fetched URL must pass.
//
// The address check that actually matters happens at dial time — a name that
// resolves to a private address is the interesting case, and no amount of
// string inspection catches it. This is the cheap half: scheme, host, and no
// credentials smuggled into the authority.
func CheckURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("not a URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("must be https, not %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("has no host")
	}
	if u.User != nil {
		return errors.New("must not carry credentials")
	}
	return nil
}

// CheckAddress is the save-time half of the address rule: the URL passes
// CheckURL, and its host does not resolve to an address PrivateAddr refuses.
// A form that takes a URL calls it while somebody is looking, so an inward
// address is refused at the form rather than discovered by a send.
//
// A name that does not resolve *yet* passes: an operator standing up a
// receiver may save before DNS has caught up, and the dial guard is still in
// force at send time either way. The error names the host and the address, and
// never the rest of the URL — a query string may be the credential.
func CheckAddress(raw string) error {
	if err := CheckURL(raw); err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("not a URL")
	}
	host := u.Hostname()
	addrs := []net.IP{net.ParseIP(host)}
	where := host
	if addrs[0] == nil {
		if addrs, err = net.LookupIP(host); err != nil {
			return nil
		}
	}
	for _, ip := range addrs {
		if PrivateAddr(ip) {
			if !ip.Equal(net.ParseIP(host)) {
				where = host + ", which resolves to " + ip.String() + ","
			}
			return addressError("points at " + where + " on this network — Toft will " +
				"not be talked into posting inside your own infrastructure, and there " +
				"is no way to switch that off")
		}
	}
	return nil
}

// addressError is a save-time refusal that reads as one sentence and still
// answers errors.Is(err, ErrPrivateAddress).
type addressError string

func (e addressError) Error() string { return string(e) }
func (e addressError) Unwrap() error { return ErrPrivateAddress }

// PrivateAddr reports whether an address is one this server must not be talked
// into reaching: itself, the link-local metadata range, or the private network
// it is deployed inside.
//
// Go's IsPrivate is RFC 1918 and fc00::/7 only, so the special-purpose ranges a
// cloud or cluster fabric also puts inside the network are listed beside it:
// the shared address space (100.64.0.0/10 — carrier-grade NAT, Tailscale, some
// pod and service CIDRs), the IETF protocol block 192.0.0.0/24, and the
// benchmarking range 198.18.0.0/15. Nothing this box should fetch from lives
// in any of them, and neither does anything multicast.
func PrivateAddr(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() {
		return true
	}
	for _, n := range specialRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

var specialRanges = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{"100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15"} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

// dialGuard refuses a connection to a private address *after* resolution,
// which is where DNS rebinding and a hostname that simply points inward both
// land.
//
// **The operator's egress proxy is the one address it lets through, and that
// is a conflict written down rather than resolved away.** Honouring HTTP_PROXY
// means every dial goes to the proxy, so the guard stops seeing the
// destination entirely: it can no longer tell a catalog URL from the metadata
// service, because both arrive at the same socket and the proxy decides the
// rest. What is left is the reason the guard exists at all — *somebody else
// chose the URL* — and that reason does not cover *the operator chose the
// route*. An install behind a proxy has already decided that this box reaches
// the internet through one hop it configured, and the enforcement of what may
// be reached through that hop is the proxy's, which is where such a policy
// belongs and where it can actually be written.
//
// The exemption is exactly the configured proxies' own addresses. A direct
// dial — NO_PROXY, or no proxy configured — is guarded as it always was.
func dialGuard(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("cannot check address %q", address)
	}
	if !PrivateAddr(ip) {
		return nil
	}
	if proxyDialTargets()[address] {
		return nil
	}
	return fmt.Errorf("refusing to fetch from %s — %w", ip, ErrPrivateAddress)
}

// ErrPrivateAddress is what the guard refuses with, and it is a sentinel rather
// than only a sentence because callers have to be able to tell it apart.
//
// A caller that retries a transport failure must never retry this one: an
// address the guard refused is not transient, and a retry loop against the
// metadata service is the exact shape of the thing this package exists to
// prevent. Matching on the message would work until somebody improved the
// wording.
var ErrPrivateAddress = errors.New("it is on this network")

// RefusedAddress reports whether the dial guard is what stopped this, through
// however many layers net/http wrapped it in.
func RefusedAddress(err error) bool { return errors.Is(err, ErrPrivateAddress) }

// proxyEnvKeys are the spellings Go's own ProxyFromEnvironment reads, which is
// the list this has to agree with or the exemption misses the proxy actually
// in use.
var proxyEnvKeys = []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"}

// proxyDialTargets is every "ip:port" a configured proxy may be dialled at.
//
// Resolved once. The environment does not change under a running process, and
// a proxy that moves address is a restart either way — which is the same
// trade the rest of the operator's environment already makes.
var proxyDialTargets func() map[string]bool = sync.OnceValue(resolveProxyTargets)

func resolveProxyTargets() map[string]bool {
	targets := map[string]bool{}
	for _, key := range proxyEnvKeys {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		port := u.Port()
		if port == "" {
			port = "80"
			if strings.EqualFold(u.Scheme, "https") {
				port = "443"
			}
		}
		addrs, err := net.LookupHost(u.Hostname())
		if err != nil {
			continue
		}
		for _, a := range addrs {
			targets[net.JoinHostPort(a, port)] = true
		}
	}
	return targets
}

// Client holds the two HTTP clients this package exists to keep apart.
//
// The strict one refuses to reach anything on this network. The permissive one
// exists for exactly one case: an operator who configured an internal mirror,
// whose own address is the address in question. Being the same host as the
// *configured* endpoint is the whole permission — it is the operator's own
// decision, and it never extends to a URL that arrived inside a document the
// mirror served, nor to one that arrived as a redirect from it: the permissive
// client dials the mirror's host unguarded and every other host through the
// same guard as the strict one, so a mirror answering 302 to 10.0.0.7 is
// refused at the dial exactly as a catalog URL naming it would be.
type Client struct {
	strict     *http.Client
	permissive *http.Client
	mirrorHost string
}

// New builds a client. configuredURL is the endpoint the operator set; when it
// names a host on this network, that one host becomes reachable and nothing
// else does.
func New(configuredURL string, timeout time.Duration) *Client {
	c := &Client{strict: newHTTPClient(true, nil, nil, timeout)}
	c.permissive = newHTTPClient(true, c.MirrorHost, nil, timeout)
	c.SetMirrorHost(configuredURL)
	return c
}

// NewForTesting is New with the transport's TLS configuration replaced and the
// address guard optional, so a test can point a client at an httptest server —
// whose certificate nothing trusts and whose address is on loopback — while
// keeping the redirect cap, the size limits and the timeouts in force.
//
// The address guard is the one rule a test server cannot survive, which is why
// it is the one this constructor lets a caller switch off. It has tests of its
// own, with the guard on.
func NewForTesting(mirrorHost string, tlsCfg *tls.Config, guard bool, timeout time.Duration) *Client {
	c := &Client{
		strict:     newHTTPClient(guard, nil, tlsCfg, timeout),
		mirrorHost: strings.ToLower(mirrorHost),
	}
	c.permissive = newHTTPClient(guard, c.MirrorHost, tlsCfg, timeout)
	return c
}

// NewWithTransport is New with the transport replaced wholesale and the
// address guard given up entirely, for a test that stubs the network rather
// than standing a server on it. Nothing in production builds one.
func NewWithTransport(configuredURL string, transport http.RoundTripper, timeout time.Duration) *Client {
	client := &http.Client{Timeout: timeout, Transport: transport}
	c := &Client{strict: client, permissive: client}
	c.SetMirrorHost(configuredURL)
	return c
}

// newHTTPClient builds one of the two clients. exempt, when set, names the one
// host whose dial skips the guard — the operator's mirror — and is consulted
// per dial, so a redirect to any other host is guarded like the strict client.
func newHTTPClient(guard bool, exempt func() string, tlsCfg *tls.Config, timeout time.Duration) *http.Client {
	plain := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: dialGuard}
	dial := plain.DialContext
	if guard {
		dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			if exempt != nil {
				if host, _, err := net.SplitHostPort(address); err == nil &&
					exempt() != "" && strings.EqualFold(host, exempt()) {
					return plain.DialContext(ctx, network, address)
				}
			}
			return guarded.DialContext(ctx, network, address)
		}
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// http.Transport's zero value is *no proxy*, not the
			// environment's, so until this line every safefetch caller
			// ignored an operator's egress proxy and simply failed on an
			// install with no direct route out. See dialGuard for what
			// honouring it costs.
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dial,
			TLSClientConfig:       tlsCfg,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", MaxRedirects)
			}
			// Every hop is re-checked. A redirect that leaves https, or that
			// hands the request to a host the first check would have refused,
			// is exactly the move this exists to stop.
			if err := CheckURL(req.URL.String()); err != nil {
				return err
			}
			// A hop to another host carries none of the caller's headers but
			// the harmless ones. net/http strips Authorization and Cookie only
			// across registrable domains, and nothing at all of a header a
			// caller named itself — a mirror credential such as X-Api-Key
			// would otherwise be re-sent to wherever the mirror pointed.
			if !strings.EqualFold(req.URL.Hostname(), via[0].URL.Hostname()) {
				for k := range req.Header {
					if !redirectSafeHeaders[k] {
						req.Header.Del(k)
					}
				}
			}
			return nil
		},
	}
}

// redirectSafeHeaders survive a hop to another host: nothing in them is a
// credential. Keys are canonical.
var redirectSafeHeaders = map[string]bool{
	"Accept": true, "Accept-Encoding": true, "Accept-Language": true,
	"User-Agent": true, "Content-Type": true,
}

// SetMirrorHost records which host the operator configured, from a URL.
func (c *Client) SetMirrorHost(configuredURL string) {
	c.mirrorHost = ""
	if u, err := url.Parse(configuredURL); err == nil {
		c.mirrorHost = strings.ToLower(u.Hostname())
	}
}

// MirrorHost is the configured host, for a caller that needs to say so.
func (c *Client) MirrorHost() string { return c.mirrorHost }

// For picks the client a URL is allowed: the permissive one only for the
// operator's own configured host, and the strict one for everything else.
func (c *Client) For(rawURL string) *http.Client {
	if c.mirrorHost == "" {
		return c.strict
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return c.strict
	}
	if strings.EqualFold(u.Hostname(), c.mirrorHost) {
		return c.permissive
	}
	return c.strict
}

// CheckConfigured applies the rules to the configured endpoint itself, with
// the mirror exemption the operator asked for by configuring it.
//
// The scheme rule still holds. A document fetched over http could be rewritten
// in flight, and an operator who wants a mirror can put a certificate on it.
func (c *Client) CheckConfigured(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return errors.New("the configured URL is not a URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("the configured URL must be https, not %q", u.Scheme)
	}
	if c.mirrorHost != "" && strings.EqualFold(u.Hostname(), c.mirrorHost) {
		return nil
	}
	return CheckURL(rawURL)
}

// ReadLimited reads at most max bytes and refuses anything longer.
//
// The limit is applied to the stream, not to Content-Length, because a
// response streamed without one is exactly the case a header check misses.
// `what` names the thing in the refusal, so an operator reading a log learns
// which document was too big.
func ReadLimited(r io.Reader, max int64, what string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("%s is larger than the %d byte limit", what, max)
	}
	return body, nil
}

// CopyLimited streams into w, refusing anything past max. Used where the
// payload belongs in a temporary file rather than in memory.
func CopyLimited(w io.Writer, r io.Reader, max int64, what string) (int64, error) {
	n, err := io.Copy(w, io.LimitReader(r, max+1))
	if err != nil {
		return n, err
	}
	if n > max {
		return n, fmt.Errorf("%s is larger than the %d byte limit", what, max)
	}
	return n, nil
}
