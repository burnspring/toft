package config

import (
	"net"
	"os"
	"regexp"
	"strings"
	"testing"

	"toft/internal/runner"
)

// setRequired provides the minimum env for Load to succeed.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef") // 32 bytes
	t.Setenv("ENCRYPTION_KEY", strings.Repeat("d0", 32))
	t.Setenv("DATABASE_URL", "postgres://toft:toft@localhost:5432/toft")
}

// A short HS256 secret is brute-forceable offline from one captured cookie,
// and forging system_admin is total compromise. Task 1.4.
func TestLoadRefusesShortJWTSecret(t *testing.T) {
	setRequired(t)
	t.Setenv("JWT_SECRET", "x")

	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted a 1-byte JWT_SECRET")
	}

	t.Setenv("JWT_SECRET", strings.Repeat("a", 31))
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted a 31-byte JWT_SECRET")
	}

	t.Setenv("JWT_SECRET", strings.Repeat("a", 32))
	if _, err := Load(); err != nil {
		t.Fatalf("Load() rejected a 32-byte JWT_SECRET: %v", err)
	}
}

// Retired keys are read-only material, and an install that has never rotated
// sets nothing — so absent must mean an empty list rather than a list holding
// one empty string, which the keyring would refuse to build from.
func TestEncryptionKeyOldParsing(t *testing.T) {
	setRequired(t)

	cases := map[string][]string{
		"":                     nil,
		"   ":                  nil,
		",":                    nil,
		"aaaa":                 {"aaaa"},
		" aaaa , bbbb ":        {"aaaa", "bbbb"},
		"aaaa,,bbbb,":          {"aaaa", "bbbb"},
		"aaaa,bbbb,cccc,dddd,": {"aaaa", "bbbb", "cccc", "dddd"},
	}
	for raw, want := range cases {
		t.Setenv("ENCRYPTION_KEY_OLD", raw)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() with ENCRYPTION_KEY_OLD=%q: %v", raw, err)
		}
		if len(cfg.EncryptionKeyOld) != len(want) {
			t.Errorf("ENCRYPTION_KEY_OLD=%q gave %v, want %v", raw, cfg.EncryptionKeyOld, want)
			continue
		}
		for i := range want {
			if cfg.EncryptionKeyOld[i] != want[i] {
				t.Errorf("ENCRYPTION_KEY_OLD=%q gave %v, want %v", raw, cfg.EncryptionKeyOld, want)
				break
			}
		}
	}

	// Unset is the shape every existing install has.
	os.Unsetenv("ENCRYPTION_KEY_OLD")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.EncryptionKeyOld) != 0 {
		t.Errorf("an install that sets nothing has retired keys %v", cfg.EncryptionKeyOld)
	}
}

// Presence decides the backend, and a typo in the scheme is refused at startup
// rather than at the first decrypt an hour later.
func TestEncryptionKeyURI(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.EncryptionKeyURI != "" {
		t.Errorf("an install that sets no URI has %q", cfg.EncryptionKeyURI)
	}

	for _, uri := range []string{
		"aws-kms://arn:aws:kms:us-east-1:123456789012:alias/toft",
		"hcvault://vault.internal:8200/transit/keys/toft",
		"  aws-kms://arn:aws:kms:eu-west-2:1:key/abc  ",
	} {
		t.Setenv("ENCRYPTION_KEY_URI", uri)
		cfg, err := Load()
		if err != nil {
			t.Errorf("Load() refused %q: %v", uri, err)
			continue
		}
		if cfg.EncryptionKeyURI != strings.TrimSpace(uri) {
			t.Errorf("ENCRYPTION_KEY_URI came back as %q", cfg.EncryptionKeyURI)
		}
	}

	for _, uri := range []string{
		"azure-keyvault://vault.example/keys/toft",
		"gcp-kms://projects/p/locations/l/keyRings/r/cryptoKeys/k",
		"arn:aws:kms:us-east-1:123456789012:alias/toft",
		"https://vault.internal:8200/transit/keys/toft",
	} {
		t.Setenv("ENCRYPTION_KEY_URI", uri)
		if _, err := Load(); err == nil {
			t.Errorf("Load() accepted the unimplemented scheme %q", uri)
		}
	}
}

// There is no unencrypted mode: without a URI, the key is required. With one,
// it is optional — the data key takes over, and a local key that is present is
// there to read what was written before the move.
func TestEncryptionKeyRequiredUnlessAURIIsSet(t *testing.T) {
	setRequired(t)

	os.Unsetenv("ENCRYPTION_KEY")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted an install with neither a key nor a URI")
	}

	t.Setenv("ENCRYPTION_KEY_URI", "aws-kms://arn:aws:kms:us-east-1:1:alias/toft")
	if _, err := Load(); err != nil {
		t.Fatalf("Load() refused a managed install with no local key: %v", err)
	}
}

// A dev-server origin is allowed only on an install that serves itself on
// loopback — getting this wrong would allow :5173 as a request origin on a
// real deployment.
func TestIsLocalDevelopment(t *testing.T) {
	setRequired(t)

	for _, tc := range []struct {
		baseURL string
		want    bool
	}{
		{"http://localhost:8080", true},
		{"http://127.0.0.1:8080", true},
		{"http://[::1]:8080", true},
		{"https://toft.example.com", false},
		{"http://toft.internal:8080", false},
		// Unset falls back to the default base URL, which is loopback.
		{"", true},
	} {
		t.Setenv("BASE_URL", tc.baseURL)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("BASE_URL=%q: %v", tc.baseURL, err)
		}
		if got := cfg.IsLocalDevelopment(); got != tc.want {
			t.Errorf("BASE_URL=%q: IsLocalDevelopment() = %v, want %v", tc.baseURL, got, tc.want)
		}
	}
}

func TestTrustedProxyCIDRParsing(t *testing.T) {
	setRequired(t)

	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 192.168.1.5, ::1")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.TrustedProxyCIDRs) != 3 {
		t.Fatalf("parsed %d CIDRs, want 3", len(cfg.TrustedProxyCIDRs))
	}
	// A bare IP must mean exactly that address.
	if cfg.TrustedProxyCIDRs[1].String() != "192.168.1.5/32" {
		t.Errorf("bare IP parsed as %s, want /32", cfg.TrustedProxyCIDRs[1])
	}

	t.Setenv("TRUSTED_PROXY_CIDRS", "not-a-network")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted an invalid TRUSTED_PROXY_CIDRS")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true,
		"::1":       true,
		"localhost": true,
		"LocalHost": true,
		"0.0.0.0":   false,
		"10.1.2.3":  false,
		"example":   false,
		"":          false,
	}
	for host, want := range cases {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// The default is loopback, not empty. A connection from 127.0.0.1 came from a
// process on this machine — the reverse proxy, or something that already has
// host access — while anything else is a remote client that must not be
// allowed to declare its own address or scheme. Getting this default right is
// what lets the common install (proxy on the same box) run unconfigured.
func TestTrustedProxyCIDRDefaultsToLoopback(t *testing.T) {
	setRequired(t)
	// Through t.Setenv first, so the cleanup it registers restores whatever
	// the environment running the suite had.
	t.Setenv("TRUSTED_PROXY_CIDRS", "placeholder")
	os.Unsetenv("TRUSTED_PROXY_CIDRS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.TrustedProxyCIDRs) != 2 {
		t.Fatalf("parsed %d CIDRs, want the two loopback networks", len(cfg.TrustedProxyCIDRs))
	}
	for _, addr := range []string{"127.0.0.1", "::1"} {
		if !ipInAnyNet(net.ParseIP(addr), cfg.TrustedProxyCIDRs) {
			t.Errorf("%s is not trusted by default", addr)
		}
	}
	// Not RFC1918: on a flat corporate network that would trust every laptop.
	if ipInAnyNet(net.ParseIP("10.1.2.3"), cfg.TrustedProxyCIDRs) {
		t.Error("the default trusts a private-network address")
	}
}

// Present-but-empty means "trust nothing", and must not be quietly replaced by
// the default — an operator who binds this straight to the internet has to be
// able to say so.
func TestTrustedProxyCIDRExplicitEmptyTrustsNothing(t *testing.T) {
	setRequired(t)
	t.Setenv("TRUSTED_PROXY_CIDRS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.TrustedProxyCIDRs) != 0 {
		t.Errorf("TRUSTED_PROXY_CIDRS= parsed %d networks, want none", len(cfg.TrustedProxyCIDRs))
	}
}

func ipInAnyNet(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// An app's SDK reads TOFT_API_URL to reach /api/v1/app/*, and the platform
// sets it from CallbackAddr — never from BaseURL.
//
// The distinction is invisible on one machine, where the two are the same
// string, and decisive in a cluster: BaseURL is a public name that resolves
// outside, so an SDK call made against it leaves through the NAT gateway and
// returns at the load balancer. That is slower and bills egress, and where the
// load balancer restricts source addresses it fails outright, because the NAT's
// address is not on the allowlist. Shared mode used BaseURL until this test.
func TestCallbackAddrPrefersTheInternalAddress(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "cluster: the in-cluster address wins over the public one",
			cfg: Config{
				BaseURL:       "https://toft.example.com",
				CallbackURL:   "http://toft.toft.svc.cluster.local:8080",
				KubeNamespace: "toft-apps",
			},
			want: "http://toft.toft.svc.cluster.local:8080",
		},
		{
			name: "single machine: loopback, never BaseURL and the proxy in front of it",
			cfg:  Config{BaseURL: "https://trytoft.dev/", Host: "0.0.0.0", Port: 8080},
			want: "http://127.0.0.1:8080",
		},
		{
			name: "single machine bound to one address: that address",
			cfg:  Config{BaseURL: "https://toft.example.com", Host: "10.0.0.5", Port: 9000},
			want: "http://10.0.0.5:9000",
		},
		{
			name: "single machine bound to the IPv6 wildcard: IPv4 loopback",
			cfg:  Config{Host: "::", Port: 8080},
			want: "http://127.0.0.1:8080",
		},
		{
			name: "isolated configured with no callback: refused rather than guessed",
			cfg:  Config{BaseURL: "https://toft.example.com", KubeNamespace: "toft-apps"},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.CallbackAddr(); got != tc.want {
				t.Errorf("CallbackAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Two ports on one host are the SAME host. That is the distinction the whole
// handshake turns on: with one host the session cookie reaches the gateway by
// itself, and with two it never does.
func TestTwoHostMode(t *testing.T) {
	tests := []struct {
		name       string
		base, apps string
		want       bool
	}{
		{"same host, two ports", "http://localhost:8080", "http://localhost:8081", false},
		{"same host, one port", "https://lp.example.com", "https://lp.example.com", false},
		{"two hostnames", "https://lp.example.com", "https://apps.example.com", true},
		{"case differs only", "https://LP.example.com", "https://lp.example.com", false},
		{"apps unset", "https://lp.example.com", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{BaseURL: tc.base, AppsBaseURL: tc.apps}
			if got := c.TwoHostMode(); got != tc.want {
				t.Errorf("TwoHostMode() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The platform and the runner speak the same contract version by construction.
//
// This is a regression test with a story. These were two constants in two
// packages — `runner.ContractVersion` and a literal in this file — and they
// drifted to "2" and "1". The result was not subtle and not rare: *every*
// isolated deploy failed at bootstrap with `runner speaks 2, platform speaks 1
// — a partial upgrade; redeploy the mismatched half`, on installs where both
// halves came out of the same `go build`. The check that reported it was
// working perfectly; it was comparing a constant against a hand-copied
// duplicate of itself, so the only thing it could detect was the copy.
//
// The fix is that there is no copy any more, which makes this test cheap and
// almost tautological — deliberately. What it defends is somebody reintroducing
// a literal here "so config has no dependency on runner", which is the change
// that reads as tidying and costs the next person an afternoon on a message
// telling them to redeploy a half that is already correct.
func TestThePlatformSpeaksTheRunnersContractVersion(t *testing.T) {
	if DefaultRunnerVersion != runner.ContractVersion {
		t.Fatalf("the platform defaults to contract version %q and the runner speaks %q. "+
			"Every isolated deploy on an install that has not set RUNNER_VERSION will fail at "+
			"bootstrap, naming a partial upgrade that has not happened. These are one fact",
			DefaultRunnerVersion, runner.ContractVersion)
	}
}

// Both CPU dials are on by default: a request because without one the
// scheduler packs a node until its apps starve, and a limit of one core
// because without one an app in a busy loop takes every core on a shared box.
// Throttling against the limit is said (app_cpu_throttled), not silent.
func TestCPURequestAndLimitAreOnByDefault(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.AppCPURequestMillicores != DefaultAppCPURequestMillicores {
		t.Errorf("default CPU request = %d, want %d",
			cfg.AppCPURequestMillicores, DefaultAppCPURequestMillicores)
	}
	if cfg.AppCPULimitMillicores != DefaultAppCPULimitMillicores {
		t.Errorf("default CPU limit = %d, want one core (%d)",
			cfg.AppCPULimitMillicores, DefaultAppCPULimitMillicores)
	}
	if cfg.JobMemoryLimitMB != 1024 || cfg.JobCPULimitMillicores != 2000 {
		t.Errorf("job defaults = %d MB, %dm; want 1024 MB, 2000m",
			cfg.JobMemoryLimitMB, cfg.JobCPULimitMillicores)
	}
}

// 0 in the environment still uncaps: an operator can choose no CPU limit, in
// the place a pipeline review sees it.
func TestAnExplicitZeroCPULimitUncaps(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_CPU_LIMIT_MILLICORES", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppCPULimitMillicores != 0 {
		t.Errorf("an explicit 0 became %d", cfg.AppCPULimitMillicores)
	}
}

// A request above the defaulted limit is refused, and the refusal says the
// limit now has a default — the operator who set the request never set one.
func TestARequestAboveTheDefaultLimitSaysTheLimitHasADefault(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_CPU_REQUEST_MILLICORES", "2000")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "it defaults to 1000") {
		t.Fatalf("Load() = %v, want a refusal naming the 1000 default", err)
	}
}

// Zero is a value an operator may mean: "reserve nothing", which is what every
// install did before this dial existed. It must survive Load as 0 rather than
// falling back to the default, or there would be no way back to the old
// behaviour.
func TestAnExplicitZeroCPURequestReservesNothing(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_CPU_REQUEST_MILLICORES", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.AppCPURequestMillicores != 0 {
		t.Errorf("an explicit 0 became %d — the escape hatch back to no reservation is gone",
			cfg.AppCPURequestMillicores)
	}
}

// A value that cannot be read is refused rather than silently becoming zero.
// Silently zero is precisely the state this dial exists to end, so a typo that
// produced it would be the bug wearing the fix's clothes.
func TestUnreadableCPUValuesAreRefused(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"APP_CPU_REQUEST_MILLICORES", "100m"}, // the Kubernetes spelling, which is not what this takes
		{"APP_CPU_REQUEST_MILLICORES", "0.5"},
		{"APP_CPU_REQUEST_MILLICORES", "-1"},
		{"APP_CPU_LIMIT_MILLICORES", "one core"},
		{"APP_CPU_LIMIT_MILLICORES", "-250"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted %s=%q", tc.key, tc.val)
			}
		})
	}
}

// A limit below the request is refused here, naming both numbers, rather than
// by the API server at the deploy three weeks later — which is the same bargain
// every other partial configuration in Load makes.
func TestACPULimitBelowTheRequestIsRefused(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_CPU_REQUEST_MILLICORES", "500")
	t.Setenv("APP_CPU_LIMIT_MILLICORES", "250")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted a CPU limit below the request")
	}
	for _, want := range []string{"500", "250"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %s: %v", want, err)
		}
	}
}

// Every seam names a backend, and a misspelled one is refused at startup naming
// the ones that exist.
//
// The refusals matter more than the acceptances here, because the failure they
// prevent is not a startup error — it is a *working* install that signs
// signatures verifying nowhere or writes a PV the driver rejects at attach time,
// inside a pod, on the first deploy that uses it. An operator's readable mistake
// is the cheap version of that, which is why the backing is stated everywhere in
// this product and never derived.
func TestAzureBackendsAreStatedAndRefusedByName(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want []string
	}{
		"artifact store with no account": {
			map[string]string{"ARTIFACT_STORE": "azblob", "ARTIFACT_BUCKET": "releases"},
			[]string{"ARTIFACT_ACCOUNT"},
		},
		"artifact store with no container": {
			map[string]string{"ARTIFACT_STORE": "azblob", "ARTIFACT_ACCOUNT": "acct"},
			[]string{"ARTIFACT_BUCKET"},
		},
		"artifact store misspelled": {
			map[string]string{"ARTIFACT_STORE": "azureblob"},
			[]string{`"local"`, `"s3"`, `"azblob"`},
		},
		"store backend misspelled": {
			map[string]string{"STORE_BACKEND": "azureblob"},
			[]string{`"s3"`, `"azblob"`},
		},
		"store backend with uploads and no account": {
			map[string]string{"STORE_BACKEND": "azblob", "STORE_UPLOADS": "1"},
			[]string{"STORE_ACCOUNT"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %v", tc.env)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %s: %v", want, err)
				}
			}
		})
	}
}

// The two backend variables are independent, and neither is derived from the
// other. They address different accounts on purpose: the artifact store is the
// platform's own bucket and a store is somebody else's, so an install that
// publishes releases to Blob and signs against a MinIO the finance team already
// runs is a configuration and not a mistake (ADR §82).
func TestArtifactStoreAndStoreBackendAreIndependent(t *testing.T) {
	setRequired(t)
	t.Setenv("ARTIFACT_STORE", "azblob")
	t.Setenv("ARTIFACT_BUCKET", "releases")
	t.Setenv("ARTIFACT_ACCOUNT", "toftartifacts")
	t.Setenv("STORE_UPLOADS", "1")
	t.Setenv("STORE_ENDPOINT", "http://127.0.0.1:9000")
	t.Setenv("STORE_S3_PATH_STYLE", "1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Blob releases with an S3-compatible signer was refused: %v", err)
	}
	if cfg.ArtifactStore != "azblob" {
		t.Errorf("ArtifactStore = %q", cfg.ArtifactStore)
	}
	// Unset means s3, and the artifact store does not reach across to change it.
	if cfg.StoreBackend != "s3" {
		t.Errorf("StoreBackend = %q — ARTIFACT_STORE must not derive it, or an install "+
			"signing against a different cloud becomes a special case", cfg.StoreBackend)
	}
}

// --- the boolean switches ----------------------------------------------------
//
// Every one of these was read as `os.Getenv(NAME) != ""` — on for `0`, for
// `false`, for `off`, for a stray space. Three of the nine failed *open* that
// way: DEV_INSECURE_COOKIES=0 dropped the Secure attribute from both session
// cookies, BUILD_ALLOW_SCRIPTS=0 ran npm lifecycle scripts, and
// ECS_ASSIGN_PUBLIC_IP=0 put a public address on a task. The line that did it
// had never had a test, in a file whose other fields all do, and its failure
// mode is an install that starts, serves, passes every health check and is
// insecure.
//
// switches is the list, and the list is the specification.
var switches = []struct {
	name string
	read func(*Config) bool
	// derived marks the two whose *absence* is not their off position: the
	// platform works the posture out from its own uid rather than being told it
	// four times. Everything else defaults off, so absent and empty are one
	// answer and telling them apart would buy nothing.
	derived bool
}{
	{name: "ARTIFACT_S3_PATH_STYLE", read: func(c *Config) bool { return c.ArtifactS3PathStyle }},
	{name: "STORE_UPLOADS", read: func(c *Config) bool { return c.StoreUploads }},
	{name: "STORE_S3_PATH_STYLE", read: func(c *Config) bool { return c.StoreS3PathStyle }},
	{name: "ECS_ASSIGN_PUBLIC_IP", read: func(c *Config) bool { return c.ECSAssignPublicIP }},
	{name: "BUILD_ALLOW_SCRIPTS", read: func(c *Config) bool { return c.AllowBuildScripts }},
	{name: "APP_USER_PER_APP", read: func(c *Config) bool { return c.AppUserPerApp }, derived: true},
	{name: "APP_USER_AUTOCREATE", read: func(c *Config) bool { return c.AppUserAutoCreate }, derived: true},
	{name: "BLOCK_AWS_METADATA", read: func(c *Config) bool { return c.BlockAWSMetadata }},
	{name: "DEV_INSECURE_COOKIES", read: func(c *Config) bool { return c.DevInsecureCookies }},
}

func TestEverySwitchReadsEverySpelling(t *testing.T) {
	on := []string{"1", "true", "TRUE", "yes", "on", " 1 "}
	off := []string{"", "0", "false", "FALSE", "no", "off", " 0 "}

	for _, sw := range switches {
		t.Run(sw.name, func(t *testing.T) {
			for _, v := range on {
				setRequired(t)
				t.Setenv(sw.name, v)
				cfg, err := Load()
				if err != nil {
					t.Fatalf("%s=%q: Load: %v", sw.name, v, err)
				}
				if !sw.read(cfg) {
					t.Errorf("%s=%q is off; it means on", sw.name, v)
				}
			}
			for _, v := range off {
				setRequired(t)
				t.Setenv(sw.name, v)
				cfg, err := Load()
				if err != nil {
					t.Fatalf("%s=%q: Load: %v", sw.name, v, err)
				}
				if sw.read(cfg) {
					t.Errorf("%s=%q is ON — this is the defect: a word meaning off that turns the switch on", sw.name, v)
				}
			}
		})
	}
}

// Unset is off, and that is the compatibility promise: an install that never
// mentioned a switch keeps the behaviour it has.
func TestAnUnsetSwitchIsOff(t *testing.T) {
	setRequired(t)
	for _, sw := range switches {
		os.Unsetenv(sw.name)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, sw := range switches {
		if sw.read(cfg) {
			t.Errorf("%s is on with nothing set", sw.name)
		}
	}
}

// A value that means nothing is refused rather than guessed at, because these
// nine have no common safe direction to fall in — BLOCK_AWS_METADATA=yse read
// leniently would leave apps able to read the instance's IAM credentials on an
// install whose operator wrote the opposite.
func TestAnUnreadableSwitchIsRefusedByName(t *testing.T) {
	for _, sw := range switches {
		t.Run(sw.name, func(t *testing.T) {
			setRequired(t)
			t.Setenv(sw.name, "maybe")
			_, err := Load()
			if err == nil {
				t.Fatalf("%s=maybe started the platform", sw.name)
			}
			if !strings.Contains(err.Error(), sw.name) {
				t.Errorf("the refusal does not name the variable: %v", err)
			}
			for _, spelling := range []string{"true", "false"} {
				if !strings.Contains(err.Error(), spelling) {
					t.Errorf("the refusal does not say %q is accepted: %v", spelling, err)
				}
			}
		})
	}
}

// Two typos are two lines, not two restarts.
func TestEveryUnreadableSwitchIsReportedAtOnce(t *testing.T) {
	setRequired(t)
	t.Setenv("BLOCK_AWS_METADATA", "yse")
	t.Setenv("DEV_INSECURE_COOKIES", "nope")

	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted two unreadable switches")
	}
	for _, name := range []string{"BLOCK_AWS_METADATA", "DEV_INSECURE_COOKIES"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("only one refusal was reported; %s is missing from %v", name, err)
		}
	}
}

// The guard that stops a tenth switch joining them the old way.
//
// The nine above were not a convention this codebase lacked — telemetry.go's
// isTrue has read 1/true/yes/on with an argued comment the whole time, and one
// of its three callers is a Toft-owned name. The convention existed and
// nine call sites did not get it, which is the failure a list can prevent and a
// comment cannot.
func TestEveryBooleanSwitchIsReadStrictly(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	if m := regexp.MustCompile(`os\.Getenv\("[A-Z0-9_]+"\) != ""`).FindAllString(body, -1); len(m) > 0 {
		t.Errorf("config.go still reads a switch by presence: %v — every boolean Toft owns the "+
			"name of goes through envFlag, which refuses a value that means nothing. See the "+
			"comment on envFlag for why lenient is not an option here", m)
	}

	for _, sw := range switches {
		// Two readers, one strictness. `readDefault` differs from `read` in
		// exactly one place — what an *absent* name means — and parses a present
		// value through the same parseFlag, so a switch on either is read
		// strictly. The list below says which is which, because a switch that
		// silently moved from one to the other would change what an install with
		// nothing set gets.
		reader := `flags.read("` + sw.name + `")`
		if sw.derived {
			reader = `flags.readDefault("` + sw.name + `"`
		}
		if !strings.Contains(body, reader) {
			t.Errorf("%s is in the switch list but config.go does not read it through %s", sw.name, reader)
		}
	}
	// And nothing is read through either reader that the list does not know
	// about, so the table above stays the specification rather than a sample.
	for _, m := range regexp.MustCompile(`flags\.read(?:Default)?\("([A-Z0-9_]+)"`).FindAllStringSubmatch(body, -1) {
		found := false
		for _, sw := range switches {
			found = found || sw.name == m[1]
		}
		if !found {
			t.Errorf("config.go reads %s through envFlag and the switch list does not name it; "+
				"add it there with the field it sets, so both directions stay tested", m[1])
		}
	}
}

// The two readers accept the same spellings and differ in exactly one place.
// Stated as a test because the difference is the whole design: a name Toft
// owns can be refused, a name it borrows has to be tolerated.
func TestTheLenientReaderAgreesExceptOnWhatItCannotRead(t *testing.T) {
	for _, v := range []string{"1", "true", "YES", " on "} {
		if !isTrue(v) {
			t.Errorf("isTrue(%q) is false; envFlag reads it as on", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "OFF"} {
		if isTrue(v) {
			t.Errorf("isTrue(%q) is true; envFlag reads it as off", v)
		}
	}
	if isTrue("maybe") {
		t.Error(`isTrue("maybe") is true; the lenient reader's whole job is that an unrecognised value is off`)
	}
	if _, err := parseFlag("maybe"); err == nil {
		t.Error(`parseFlag("maybe") did not refuse; the strict reader's whole job is that it does`)
	}
}

// --- The units an operator states a machine in ---

// Each quantity has two spellings, neither of them the old one: megabytes is
// the right unit for a 256 MB cap and gigabytes for a 16 GB one. What an
// install may not do is write both, because that is two numbers with no way to
// tell which was meant.
func TestAQuantityHasTwoSpellingsAndBothSetIsRefused(t *testing.T) {
	pairs := []struct {
		bigKey, smallKey string
		field            func(*Config) int
		// perBigUnit is the large spelling's unit expressed in the stored one.
		perBigUnit int
	}{
		{"APP_MEMORY_LIMIT_GB", "APP_MEMORY_LIMIT_MB",
			func(c *Config) int { return c.AppMemoryLimitMB }, 1024},
		{"APP_CPU_REQUEST_CORES", "APP_CPU_REQUEST_MILLICORES",
			func(c *Config) int { return c.AppCPURequestMillicores }, 1000},
		{"APP_CPU_LIMIT_CORES", "APP_CPU_LIMIT_MILLICORES",
			func(c *Config) int { return c.AppCPULimitMillicores }, 1000},
		{"JOB_MEMORY_LIMIT_GB", "JOB_MEMORY_LIMIT_MB",
			func(c *Config) int { return c.JobMemoryLimitMB }, 1024},
		{"JOB_CPU_LIMIT_CORES", "JOB_CPU_LIMIT_MILLICORES",
			func(c *Config) int { return c.JobCPULimitMillicores }, 1000},
	}

	for _, p := range pairs {
		t.Run(p.bigKey, func(t *testing.T) {
			setRequired(t)
			// The CPU limit defaults to one core, so a request of 1.5 cores
			// would be refused for being above it — not what this tests.
			if p.smallKey == "APP_CPU_REQUEST_MILLICORES" {
				t.Setenv("APP_CPU_LIMIT_MILLICORES", "0")
			}

			// Both: refused, naming the two, rather than resolved by precedence.
			t.Setenv(p.bigKey, "2")
			t.Setenv(p.smallKey, "1000")
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted both %s and %s; an operator with both has two numbers "+
					"and the platform cannot know which they meant", p.bigKey, p.smallKey)
			}
			for _, want := range []string{p.bigKey, p.smallKey} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal should name %s, got: %v", want, err)
				}
			}

			// The large spelling alone, with a decimal.
			t.Setenv(p.smallKey, "")
			t.Setenv(p.bigKey, "1.5")
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if got, want := p.field(cfg), p.perBigUnit*3/2; got != want {
				t.Errorf("%s=1.5 resolved to %d, want %d", p.bigKey, got, want)
			}

			// The small spelling alone, and it is a peer rather than a
			// tolerated leftover: an install that states a cap in megabytes is
			// stating it in the unit that fits the number.
			t.Setenv(p.bigKey, "")
			t.Setenv(p.smallKey, "2000")
			cfg, err = Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if got := p.field(cfg); got != 2000 {
				t.Errorf("%s=2000 resolved to %d", p.smallKey, got)
			}

			// And a value neither reader can make sense of is refused, never
			// approximated — the rule the strict boolean reader exists for.
			t.Setenv(p.smallKey, "")
			t.Setenv(p.bigKey, "half")
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted %s=half", p.bigKey)
			}
		})
	}
}

// A gigabyte is 1024 mebibytes, and that is the decision rather than an
// accident: the field says GB and means what both backends count in, so an
// operator who types 0.5 gets the 512 that has been in .env.example since the
// beginning.
func TestAGigabyteIsWhatBothBackendsCountIn(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_MEMORY_LIMIT_GB", "0.5")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppMemoryLimitMB != 512 {
		t.Errorf("APP_MEMORY_LIMIT_GB=0.5 resolved to %d MB, want 512", cfg.AppMemoryLimitMB)
	}
}

// An install that says nothing keeps exactly the envelope it has always had.
func TestAnInstallThatSaysNothingKeepsItsEnvelope(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppMemoryLimitMB != DefaultAppMemoryLimitMB {
		t.Errorf("memory ceiling = %d, want %d", cfg.AppMemoryLimitMB, DefaultAppMemoryLimitMB)
	}
	if cfg.AppCPURequestMillicores != DefaultAppCPURequestMillicores {
		t.Errorf("cpu request = %d, want %d", cfg.AppCPURequestMillicores, DefaultAppCPURequestMillicores)
	}
	if cfg.AppCPULimitMillicores != DefaultAppCPULimitMillicores {
		t.Errorf("cpu limit = %d, want %d", cfg.AppCPULimitMillicores, DefaultAppCPULimitMillicores)
	}
}

// --- The posture the platform can work out for itself ---

// Absent, empty and a word are three answers for the two account switches, and
// that is the whole point of the second reader.
//
// The off direction is the one that can fail silently, which is why
// chart_test.go asserts the configmap emits `""` rather than omitting the key,
// and why this is its config-side peer: a customer who wrote `perApp: false` in
// their values must get the platform account posture, not the derived default.
func TestAbsentEmptyAndAWordAreThreeAnswers(t *testing.T) {
	for _, name := range []string{"APP_USER_PER_APP", "APP_USER_AUTOCREATE"} {
		read := func(c *Config) bool { return c.AppUserPerApp }
		if name == "APP_USER_AUTOCREATE" {
			read = func(c *Config) bool { return c.AppUserAutoCreate }
		}
		t.Run(name, func(t *testing.T) {
			// Absent: the derivation. On this machine that is whatever euid the
			// test runs as, and asserting the *derivation* rather than a fixed
			// answer is what keeps this test true on both.
			setRequired(t)
			os.Unsetenv(name)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if got, want := read(cfg), perAppAccountsByDefault(); got != want {
				t.Errorf("absent %s = %v, want the derived %v — an install that says nothing "+
					"gets the posture its own uid allows, not the least safe of three", name, got, want)
			}

			// Present and empty: off. A delivery artifact spells off as empty,
			// and the Helm chart's configmap always emits the key.
			setRequired(t)
			t.Setenv(name, "")
			cfg, err = Load()
			if err != nil {
				t.Fatal(err)
			}
			if read(cfg) {
				t.Errorf("%s=\"\" is on — empty is how the chart spells off, and a customer who "+
					"wrote false in their values would silently get the derived default", name)
			}

			// Present and a word: the strict reader's answer, either way.
			for _, c := range []struct {
				value string
				want  bool
			}{{"1", true}, {"true", true}, {"false", false}, {"off", false}} {
				setRequired(t)
				t.Setenv(name, c.value)
				cfg, err := Load()
				if err != nil {
					t.Fatalf("%s=%s: %v", name, c.value, err)
				}
				if read(cfg) != c.want {
					t.Errorf("%s=%s = %v, want %v", name, c.value, read(cfg), c.want)
				}
			}

			// And a word that is not a boolean is still refused. The second
			// reader differs from the first in exactly one place.
			setRequired(t)
			t.Setenv(name, "yse")
			if _, err := Load(); err == nil {
				t.Errorf("%s=yse was accepted; the derived default must not have relaxed the "+
					"strict parse", name)
			}
		})
	}
}

// The runtime an install that names none gets. `mock` is a test double and the
// failure it produced was quiet: an install that accepts a deploy and starts
// nothing.
func TestTheDefaultRuntimeIsTheOneEveryArtifactSets(t *testing.T) {
	setRequired(t)
	os.Unsetenv("RUNTIME")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime != "agent" {
		t.Errorf("runtime = %q, want agent — a default nothing outside a test uses is a default "+
			"in the wrong place", cfg.Runtime)
	}
}

func TestDefaultConcurrentBuildsIsHalfTheCPUsBetweenOneAndFour(t *testing.T) {
	for cpus, want := range map[int]int{0: 1, 1: 1, 2: 1, 3: 1, 4: 2, 6: 3, 8: 4, 64: 4} {
		if got := DefaultConcurrentBuilds(cpus); got != want {
			t.Errorf("DefaultConcurrentBuilds(%d) = %d, want %d", cpus, got, want)
		}
	}
}

// RUNTIME is one of three words, and anything else is refused at start rather
// than read as the mock that starts nothing.
func TestAnUnknownRuntimeIsRefused(t *testing.T) {
	setRequired(t)
	for _, ok := range []string{"agent", "mock"} {
		t.Setenv("RUNTIME", ok)
		if _, err := Load(); err != nil {
			t.Errorf("RUNTIME=%s: %v", ok, err)
		}
	}
	t.Setenv("RUNTIME", "docker")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "RUNTIME") {
		t.Errorf("RUNTIME=docker: %v, want a refusal naming it", err)
	}
}

// The agent and PM2 both run shared workloads as processes on this machine,
// which is what the boot reconcile, the health watcher and the per-app account
// posture key on.
func TestTheAgentIsASharedHostRuntime(t *testing.T) {
	for rt, want := range map[string]bool{"agent": true, "mock": false} {
		if got := SharedHostRuntime(rt); got != want {
			t.Errorf("SharedHostRuntime(%q) = %v", rt, got)
		}
	}
}

// ADR §129: the two names the agent retired are refused by
// name, each pointing at what replaced it — never read as something else.
func TestTheRetiredRuntimeNamesAreRefusedByName(t *testing.T) {
	setRequired(t)
	t.Setenv("RUNTIME", "pm2")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "RUNTIME=pm2 is no longer supported") ||
		!strings.Contains(err.Error(), "RUNTIME=agent") {
		t.Errorf("RUNTIME=pm2: %v", err)
	}
	t.Setenv("RUNTIME", "agent")
	t.Setenv("JOB_CGROUP_PARENT", "/sys/fs/cgroup/toft")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "AGENT_CGROUP_PARENT") {
		t.Errorf("JOB_CGROUP_PARENT: %v", err)
	}
}

// KUBE-3: an integer setting that is not one used to read as 0, and
// APPS_PORT=0 silently collapsed the two-origin split. Each is refused at
// start, by name, the way an unreadable boolean is.
func TestUnreadableIntegersRefuseTheStart(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"APPS_PORT", "8081/tcp"},
		{"APPS_PORT", "0"},
		{"PORT", "abc"},
		{"PORT", "70000"},
		{"PORT_RANGE_START", "abc"},
		{"PORT_RANGE_END", "-1"},
		{"MAX_CONCURRENT_BUILDS", "two"},
		{"APP_REQUEST_DEADLINE_SECONDS", "5m"},
		{"AUTO_DEPLOY_POLL_INTERVAL", "1m"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Errorf("Load() = %v, want a refusal naming %s", err, tc.key)
			}
		})
	}

	// A readable value, whitespace around it included, is taken as written;
	// unset is the default; and 0 still means "no poller".
	setRequired(t)
	t.Setenv("APPS_PORT", " 9091 ")
	t.Setenv("AUTO_DEPLOY_POLL_INTERVAL", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppsPort != 9091 || cfg.Port != 8080 || cfg.AutoDeployPollSeconds != 0 || !cfg.SeparateAppsListener() {
		t.Errorf("apps port %d, port %d, poll %d", cfg.AppsPort, cfg.Port, cfg.AutoDeployPollSeconds)
	}
}

// CALLBACK_ADDR is absent unless set, and a port apart from the others when it
// is: on the platform's own port it would be the listener it exists to keep
// apps away from (ADR §31 D4).
func TestCallbackListener(t *testing.T) {
	setRequired(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CallbackListenAddr != "" {
		t.Errorf("unset CALLBACK_ADDR = %q, want absent", cfg.CallbackListenAddr)
	}

	t.Setenv("CALLBACK_ADDR", " :8082 ")
	if cfg, err = Load(); err != nil || cfg.CallbackListenAddr != ":8082" {
		t.Fatalf("CALLBACK_ADDR=:8082: %q, %v", cfg.CallbackListenAddr, err)
	}

	for addr, want := range map[string]string{
		"8082":         "not host:port",
		":8080":        "PORT",
		"0.0.0.0:8081": "APPS_PORT",
		"[::]:9090":    "METRICS_ADDR",
	} {
		t.Setenv("CALLBACK_ADDR", addr)
		t.Setenv("METRICS_ADDR", "127.0.0.1:9090")
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CALLBACK_ADDR=%s: %v, want a refusal naming %s", addr, err, want)
		}
	}
}

// JOB_CPU_LIMIT_MILLICORES below the app request is refused like the app's own
// limit: a job pod pairs the two (code review 2026-11-11, H-5).
func TestAJobCPULimitBelowTheRequestIsRefused(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_CPU_REQUEST_MILLICORES", "500")
	t.Setenv("JOB_CPU_LIMIT_MILLICORES", "250")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JOB_CPU_LIMIT_MILLICORES") {
		t.Fatalf("Load() = %v, want a refusal naming JOB_CPU_LIMIT_MILLICORES", err)
	}
}

// Every object-store backend presigns, so isolated mode takes any of them —
// Azure Blob included, once AzBlobStore gained its presigner (AS-2).
func TestIsolatedModeTakesEveryObjectStore(t *testing.T) {
	for _, store := range []string{"s3", "azblob"} {
		cfg := &Config{
			KubeNamespace: "toft-apps",
			ArtifactStore: store,
			RunnerImages:  map[string]string{"python": "runner:py"},
			CallbackURL:   "http://toft.toft.svc.cluster.local:8080",
		}
		if err := cfg.validateIsolated(); err != nil {
			t.Errorf("isolated mode on %s was refused: %v", store, err)
		}
	}
}
