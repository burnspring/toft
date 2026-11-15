// Package charts tests the Helm chart that delivers Toft to a customer
// (ADR §34).
//
// The chart is the delivery artifact, and `deploy/k8s/*.yaml` plus
// scripts/k3s.sh are this box's own fixture — two paths that can drift. This
// test is the guard on the chart half. It cannot notice that the manifests and
// the chart have come to mean different things; what it does catch is a chart
// that stops rendering, and — the reason it exists at all — a chart that keeps
// rendering after somebody has templated a value that is compiled into the
// binary.
//
// That last failure is invisible in every other way. Derive the platform pod's
// identity label from the release name and the install still comes up: the pod
// runs, the NetworkPolicy is applied, `helm install` reports success, and every
// isolated app is unreachable through the gateway with nothing in any log.
package charts

import (
	"encoding/base64"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"toft/internal/version"
)

// A minimally complete install: two origins, an ingress, and every optional
// capability switched on so the conditional templates are actually exercised.
const testValues = `
image:
  repository: registry.example.com/toft
platform:
  baseUrl: https://lp.example.com
  appsBaseUrl: https://apps.example.com
  trustedProxyCidrs: "10.0.0.0/16"
ingress:
  enabled: true
  className: alb
  inboundCidrs: ["203.0.113.4/32"]
  tls:
    enabled: true
networkPolicy:
  platformSourceCidrs: ["10.0.200.0/24"]
artifactStore:
  enabled: true
  bucket: test-bucket
  region: us-east-1
appStorage:
  efs:
    enabled: true
    fileSystemId: fs-0123456789abcdef0
runnerImages:
  python: example.com/runner-python:1
`

func chartDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("toft")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// render runs `helm template` and returns its output, or the error text when
// the chart deliberately refuses.
func render(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}

	vf := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(vf, []byte(testValues), 0o600); err != nil {
		t.Fatal(err)
	}

	args := append([]string{"template", "toft", chartDir(t), "-f", vf}, extra...)
	out, err := exec.Command("helm", args...).CombinedOutput()
	return string(out), err
}

// renderBare renders with only what is given — no testValues — so the chart's
// own defaults are what is under test.
func renderBare(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	args := append([]string{"template", "toft", chartDir(t), "--set", "image.repository=" + testRepository}, extra...)
	out, err := exec.Command("helm", args...).CombinedOutput()
	return string(out), err
}

// testRepository stands in for the registry an operator names: the chart has
// no default image (nothing is published for it to name).
const testRepository = "registry.example.com/toft"

func mustRender(t *testing.T, extra ...string) string {
	t.Helper()
	out, err := render(t, extra...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return out
}

// TestPlatformLabelIsNotTemplated is the important one.
//
// internal/kube builds every isolated workload's NetworkPolicy with an ingress
// rule admitting pods labelled app.kubernetes.io/name=<managedByValue>, and
// managedByValue is the constant "toft". The chart must therefore emit
// that exact label whatever the release is called and whatever the name is
// overridden to — which is the opposite of the usual Helm convention, and so
// exactly the thing a well-meaning refactor "fixes".
func TestPlatformLabelIsNotTemplated(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--set", "nameOverride=other-name"},
		{"--set", "fullnameOverride=something-else"},
	} {
		out := mustRender(t, args...)

		// The Deployment's pod template must carry the constant.
		if !strings.Contains(out, "app.kubernetes.io/name: toft") {
			t.Errorf("with %v: the platform pod label is not app.kubernetes.io/name=toft; "+
				"every isolated app would become unreachable through the gateway", args)
		}
		// And must never carry a derived one in its place.
		for _, bad := range []string{
			"app.kubernetes.io/name: other-name\n",
			"app.kubernetes.io/name: something-else\n",
		} {
			if strings.Contains(out, bad) {
				t.Errorf("with %v: found a templated platform label %q; it is a compiled-in constant", args, strings.TrimSpace(bad))
			}
		}
	}
}

// The key is read by hex.DecodeString and must be 32 bytes. randAlphaNum would
// render a chart that installs cleanly and then fails at startup.
func TestGeneratedEncryptionKeyIsHex(t *testing.T) {
	out := mustRender(t)

	re := regexp.MustCompile(`(?m)^\s*ENCRYPTION_KEY:\s*(\S+)$`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatal("no ENCRYPTION_KEY in the rendered Secret")
	}
	key, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatalf("ENCRYPTION_KEY is not valid base64: %v", err)
	}
	raw, err := hex.DecodeString(string(key))
	if err != nil {
		t.Fatalf("ENCRYPTION_KEY is not hex (internal/crypto reads it with hex.DecodeString): %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("ENCRYPTION_KEY decodes to %d bytes, want 32", len(raw))
	}
}

// No peer is believed about the client by default: loopback was the default,
// and a shared-mode app dialling 127.0.0.1 could then name its own address
// (security review 2026-11-15, HD-5).
func TestChartTrustsNoProxyByDefault(t *testing.T) {
	out, err := renderBare(t, "--set", "platform.baseUrl=https://lp.example.com", "--set", "platform.appsBaseUrl=https://apps.example.com")
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	if got := configMapValue(t, out, "TRUSTED_PROXY_CIDRS"); got != "" {
		t.Errorf("TRUSTED_PROXY_CIDRS = %q by default, want empty", got)
	}
}

// Single origin is a property of the addresses. Setting the URLs equal must
// move the port too, or the install's links point where nothing is listening.
func TestSingleOriginCollapsesTheAppsPort(t *testing.T) {
	out := mustRender(t, "--set", "platform.appsBaseUrl=https://lp.example.com")

	if !strings.Contains(out, `APPS_PORT: "8080"`) {
		t.Error("equal base URLs did not collapse APPS_PORT onto PORT")
	}
	if strings.Contains(out, "containerPort: 8081") {
		t.Error("single-origin install still declares the apps container port; nothing listens there")
	}

	two := mustRender(t)
	if !strings.Contains(two, `APPS_PORT: "8081"`) {
		t.Error("distinct base URLs did not produce a separate apps port")
	}
	if !strings.Contains(two, "containerPort: 8081") {
		t.Error("two-origin install does not declare the apps container port")
	}
}

// Both of these present as application bugs rather than configuration ones,
// which is why the chart fails instead of rendering them.
func TestChartRefusesMisconfigurations(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "ingress with loopback trusted proxies",
			args: []string{"--set", `platform.trustedProxyCidrs=127.0.0.1/32\,::1/128`},
			want: "X-Forwarded-* would be ignored",
		},
		{
			name: "ingress with the default empty trusted proxies",
			args: []string{"--set", `platform.trustedProxyCidrs=`},
			want: "X-Forwarded-* would be ignored",
		},
		{
			name: "internet-facing ingress with no source restriction",
			args: []string{"--set", "ingress.inboundCidrs=null"},
			want: "publish the install to the internet",
		},
		{
			name: "source restriction on a controller the chart cannot translate",
			args: []string{"--set", "ingress.className=traefik"},
			want: "nothing would enforce it",
		},
		{
			name: "source restriction on the cluster's unnamed default class",
			args: []string{"--set", "ingress.className="},
			want: "nothing would enforce it",
		},
		{
			name: "allow-list annotation set by hand as well as from the list",
			args: []string{"--set", `ingress.annotations.alb\.ingress\.kubernetes\.io/inbound-cidrs=0.0.0.0/0`},
			want: "keep the addresses in ingress.inboundCidrs only",
		},
		{
			name: "ALB with the platform's policy and no source range",
			args: []string{"--set", "networkPolicy.platformSourceCidrs=null"},
			want: "the ALB dials the platform pod from its own subnets",
		},
		{
			name: "LoadBalancer Service with the platform's policy and no source range",
			args: []string{"--set", "networkPolicy.platformSourceCidrs=null", "--set", "ingress.enabled=false",
				"--set", "service.type=LoadBalancer"},
			want: "service.type=LoadBalancer with networkPolicy.enabled",
		},
		{
			name: "a runner image with no artifact store",
			args: []string{"--set", "artifactStore.enabled=false"},
			want: "isolated mode needs an artifact store",
		},
		{
			name: "app storage without isolated mode",
			args: []string{"--set", "runnerImages.python="},
			want: "cluster storage is provisioned by the isolated backend",
		},
		{
			name: "external database with no secret to hold its URL",
			args: []string{"--set", "postgres.enabled=false"},
			want: "requires secrets.existingSecret",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := render(t, tc.args...)
			if err == nil {
				t.Fatalf("chart rendered %s instead of refusing it", tc.name)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("refusal does not explain itself: want %q in\n%s", tc.want, out)
			}
		})
	}
}

// ingress.inboundCidrs is what keeps an install off the open internet, and for
// a while it was read only by the lint and NOTES.txt — the rendered Ingress
// carried nothing, while the notes said "Reachable only from". So the assertion
// is on the rendered object: each class the chart knows gets its controller's
// own allow-list annotation, carrying every address.
func TestIngressCarriesTheInboundAllowList(t *testing.T) {
	two := `ingress.inboundCidrs={203.0.113.4/32,198.51.100.0/24}`
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--set", "ingress.className=alb"}, "alb.ingress.kubernetes.io/inbound-cidrs: 203.0.113.4/32,198.51.100.0/24"},
		{[]string{"--set", "ingress.className=nginx"}, "nginx.ingress.kubernetes.io/whitelist-source-range: 203.0.113.4/32,198.51.100.0/24"},
		{[]string{"--set", "ingress.className=webapprouting.kubernetes.azure.com"}, "nginx.ingress.kubernetes.io/whitelist-source-range: 203.0.113.4/32,198.51.100.0/24"},
		{[]string{"--set", "ingress.className=traefik", "--set", "ingress.inboundCidrsAnnotation=example.com/allow"}, "example.com/allow: 203.0.113.4/32,198.51.100.0/24"},
	} {
		ing := ingressDoc(t, mustRender(t, append(tc.args, "--set", two)...))
		if !strings.Contains(ing, tc.want) {
			t.Errorf("with %v the Ingress does not carry %q — the install would answer the whole internet:\n%s", tc.args, tc.want, ing)
		}
	}

	// Saying it out loud on a controller the chart cannot translate renders,
	// and renders no restriction — NOTES.txt then prints the internet warning.
	ing := ingressDoc(t, mustRender(t, "--set", "ingress.className=traefik", "--set", "ingress.allowPublicInbound=true"))
	if strings.Contains(ing, "203.0.113.4") {
		t.Errorf("an untranslatable class rendered the list somewhere:\n%s", ing)
	}
}

// values-aks.yaml is copied by operators as written, so what it ships is what
// installs. Two things it once got wrong: an inboundCidrs no object carried,
// and the cluster's whole pod CIDR as the trusted proxy — a range holding every
// isolated app, each of which could then name its own client address.
func TestTheAKSValuesRenderRestricted(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	dir := chartDir(t)
	out, err := exec.Command("helm", "template", "toft", dir, "-f", filepath.Join(dir, "values-aks.yaml")).CombinedOutput()
	if err != nil {
		t.Fatalf("values-aks.yaml does not render: %v\n%s", err, out)
	}
	ing := ingressDoc(t, string(out))
	if !strings.Contains(ing, "nginx.ingress.kubernetes.io/whitelist-source-range:") {
		t.Errorf("the AKS Ingress carries no allow-list:\n%s", ing)
	}
	// ingress-nginx refuses a body over 1 MiB by default: every deploy archive
	// and store upload would be a 413 before the platform saw it.
	for _, want := range []string{
		`nginx.ingress.kubernetes.io/proxy-body-size: "0"`,
		`nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"`,
		`nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"`,
	} {
		if !strings.Contains(ing, want) {
			t.Errorf("the AKS Ingress lacks %s:\n%s", want, ing)
		}
	}
	// The operator's own value wins over the default.
	own, err := exec.Command("helm", "template", "toft", dir, "-f", filepath.Join(dir, "values-aks.yaml"),
		"--set-string", `ingress.annotations.nginx\.ingress\.kubernetes\.io/proxy-body-size=200m`).CombinedOutput()
	if err != nil {
		t.Fatalf("an operator's own proxy-body-size does not render: %v\n%s", err, own)
	}
	if ing := ingressDoc(t, string(own)); !strings.Contains(ing, "proxy-body-size: 200m") || strings.Contains(ing, `proxy-body-size: "0"`) {
		t.Errorf("the operator's proxy-body-size is not the one rendered:\n%s", ing)
	}
	// Nothing publishes runner images (ADR §44). Names in the product's own
	// registry namespace look published and fail every isolated deploy with
	// ImagePullBackOff; the file's placeholders must look like placeholders and
	// carry a tag.
	images := regexp.MustCompile(`(?m)^\s*RUNNER_IMAGE_[A-Z]+:\s*"(.*)"\s*$`).FindAllStringSubmatch(string(out), -1)
	if len(images) == 0 {
		t.Error("values-aks.yaml renders no RUNNER_IMAGE_*")
	}
	for _, m := range images {
		img := m[1]
		if !strings.Contains(img, "REPLACE") {
			t.Errorf("values-aks.yaml names %s, which is not a placeholder — nothing publishes runner images", img)
		}
		if i := strings.LastIndex(img, ":"); i < 0 || strings.Contains(img[i:], "/") {
			t.Errorf("values-aks.yaml names %s with no tag", img)
		}
	}
	podCIDR := &net.IPNet{IP: net.IPv4(10, 244, 0, 0), Mask: net.CIDRMask(16, 32)}
	for _, c := range strings.Split(configMapValue(t, string(out), "TRUSTED_PROXY_CIDRS"), ",") {
		_, n, err := net.ParseCIDR(strings.TrimSpace(c))
		if err != nil {
			t.Fatalf("TRUSTED_PROXY_CIDRS entry %q: %v", c, err)
		}
		if n.Contains(podCIDR.IP) || podCIDR.Contains(n.IP) {
			t.Errorf("values-aks.yaml trusts %s, which overlaps AKS's default pod CIDR %s — "+
				"every isolated app could forge its client address", n, podCIDR)
		}
	}
}

// Shared apps run inside the platform pod as their own uids, and the
// platform's ServiceAccount token may create pods in the apps namespace. An
// automounted token is 0644 unless the kubelet finds an effective fsUser, which
// this pod had only because of `runAsUser: 0` — so the token is mounted by
// hand with its mode written down, and this holds both halves.
func TestThePlatformTokenIsMountedRootOnly(t *testing.T) {
	out := mustRender(t)
	for _, want := range []string{
		"automountServiceAccountToken: false",
		"mountPath: /var/run/secrets/kubernetes.io/serviceaccount",
		"defaultMode: 0600",
		"- serviceAccountToken:\n                  path: token",
		"path: ca.crt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the platform Deployment lacks %q; the token's mode would be the kubelet's guess", want)
		}
	}
}

// The platform's Role may create any pod spec in the apps namespace; Pod
// Security Admission is what stops that grant being a privileged or hostPath
// pod on the node.
func TestTheAppsNamespaceEnforcesPodSecurity(t *testing.T) {
	out := mustRender(t)
	for _, want := range []string{
		"pod-security.kubernetes.io/enforce: restricted",
		"pod-security.kubernetes.io/warn: restricted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the apps namespace lacks %s", want)
		}
	}
}

// ingressDoc returns the rendered Ingress document out of a helm template stream.
func ingressDoc(t *testing.T, rendered string) string {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---") {
		if strings.Contains(doc, "\nkind: Ingress\n") {
			return doc
		}
	}
	t.Fatal("no Ingress in the rendered chart")
	return ""
}

// The bundled Postgres is a ClusterIP Service in the release namespace, and an
// isolated app's egress is open to everything but the metadata addresses — so
// without a policy one app can hold every connection slot and take the
// platform down. The rule admits the compiled-in platform label and nothing
// else.
func TestTheBundledPostgresAdmitsOnlyThePlatform(t *testing.T) {
	var np string
	for _, doc := range strings.Split(mustRender(t), "\n---") {
		if strings.Contains(doc, "\nkind: NetworkPolicy\n") && strings.Contains(doc, "name: toft-postgres\n") {
			np = doc
		}
	}
	if np == "" {
		t.Fatal("no NetworkPolicy fences the bundled Postgres; every isolated app can reach it")
	}
	for _, want := range []string{
		"app.kubernetes.io/name: toft-postgres", // selects the database pod
		"policyTypes: [Ingress]",
		"- podSelector:\n            matchLabels:\n              app.kubernetes.io/name: toft\n", // admits the platform label
		"port: 5432",
	} {
		if !strings.Contains(np, want) {
			t.Errorf("the Postgres NetworkPolicy lacks %q:\n%s", want, np)
		}
	}
	if strings.Contains(np, "namespaceSelector") || strings.Contains(np, "ipBlock") {
		t.Errorf("the Postgres NetworkPolicy admits more than the platform pod:\n%s", np)
	}

	// The managed-database path has no in-cluster Postgres to fence, and takes
	// its URL from the key external.databaseUrlKey names — a knob that once
	// reached no template, so a Secret exporting PGURL started a pod with no
	// DATABASE_URL at all.
	ext := mustRender(t, "--set", "postgres.enabled=false", "--set", "secrets.existingSecret=s",
		"--set", "external.databaseUrlKey=PGURL")
	if !strings.Contains(ext, "- name: DATABASE_URL\n              valueFrom:\n                secretKeyRef:\n                  name: s\n                  key: \"PGURL\"") {
		t.Error("external.databaseUrlKey does not map the Secret's key onto DATABASE_URL")
	}
	if strings.Contains(mustRender(t), "key: \"DATABASE_URL\"") {
		t.Error("the bundled Postgres install maps an external DATABASE_URL it does not have")
	}
	if strings.Contains(mustRender(t, "--set", "postgres.enabled=false", "--set", "secrets.existingSecret=s"), "name: toft-postgres\n") {
		t.Error("postgres.enabled=false still renders Postgres objects")
	}
}

// The chart defaults the image tag to .Chart.AppVersion, and
// scripts/publish-image.sh tags the published image with version.Version. Two
// version numbers that must be equal, edited in different files, in different
// languages, at different times.
//
// Drift here renders a chart that pulls a tag nobody published: `helm install`
// succeeds, and the failure arrives minutes later as an ImagePullBackOff on a
// customer's cluster.
func TestChartAppVersionMatchesTheBinary(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir(t), "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^appVersion:\s*"?([^"\s]+)"?`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("Chart.yaml has no appVersion")
	}
	if got := string(m[1]); got != version.Version {
		t.Errorf("Chart.yaml appVersion is %q, internal/version.Version is %q — "+
			"the chart would pull an image tag that publish-image.sh never pushes", got, version.Version)
	}
}

// No image is published, so the chart names none: image.repository is empty
// by default and the render refuses until the operator names their registry,
// with the build command in the message.
func TestChartRequiresAnImageRepository(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	raw, err := os.ReadFile(filepath.Join(chartDir(t), "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^  repository: ""$`).Match(raw) {
		t.Error(`values.yaml's image.repository is not "" — the chart would name an image nothing publishes`)
	}
	out, err := exec.Command("helm", "template", "toft", chartDir(t),
		"--set", "platform.baseUrl=https://lp.example.com",
		"--set", "platform.appsBaseUrl=https://apps.example.com").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "set image.repository to the registry holding your Toft image") {
		t.Errorf("a render with no image.repository did not refuse with the build hint: %v\n%s", err, out)
	}
}

func TestChartLints(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	vf := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(vf, []byte(testValues), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("helm", "lint", chartDir(t), "-f", vf).CombinedOutput()
	if err != nil {
		t.Fatalf("helm lint failed: %v\n%s", err, out)
	}
}

// Per-app accounts are the shipped posture, and the chart is where a customer
// reads that. Two things have to hold and only one of them is the default.
//
// The default is on. And because the platform *derives* the posture when
// nothing says otherwise — root, running the agent, which is a chart install exactly —
// "off" has to be *expressible*: a ConfigMap that omitted the key would leave
// that derivation standing, so `appAccounts.perApp: false` would render a chart
// that says one thing and installs another. An empty value is how off is
// spelled, and this asserts both directions, because only the second one can
// fail silently.
//
// This assertion got *more* load-bearing when the derivation landed, not less:
// before it, an omitted key left the image's `APP_USER_PER_APP=1` standing, and
// now it leaves the code's own answer standing. Same failure, one layer down.
func TestChartShipsPerAppAccountsAndCanTurnThemOff(t *testing.T) {
	on := mustRender(t)
	for _, want := range []string{
		`APP_USER_PER_APP: "1"`,
		`APP_USER_AUTOCREATE: "1"`,
		`APP_USER_GROUP: "toft-apps"`,
	} {
		if !strings.Contains(on, want) {
			t.Errorf("the chart's default install does not carry %s; shared apps would run "+
				"as the platform unless the image happened to say otherwise", want)
		}
	}

	off := mustRender(t,
		"--set", "appAccounts.perApp=false",
		"--set", "appAccounts.autoCreate=false")
	for _, want := range []string{
		`APP_USER_PER_APP: ""`,
		`APP_USER_AUTOCREATE: ""`,
	} {
		if !strings.Contains(off, want) {
			t.Errorf("with appAccounts off, the ConfigMap does not carry %s — an omitted key "+
				"leaves the image's own value standing and the switch does nothing", want)
		}
	}
}

// The per-app group is supplementary; the accounts' shared primary group is
// what the metadata-egress rule matches. Naming a per-app group here would be
// the version of that change that gates the directories just as well and
// silently stops every app process matching the firewall rule.
func TestChartsAppGroupIsTheSharedOne(t *testing.T) {
	out := mustRender(t, "--set", "appAccounts.group=my-apps")
	if !strings.Contains(out, `APP_USER_GROUP: "my-apps"`) {
		t.Error("APP_USER_GROUP is not settable")
	}
	if strings.Contains(out, "APP_GROUP_PREFIX") {
		t.Error("a per-app group prefix reached the chart; the per-app group takes the " +
			"account's own name and is never configured")
	}
}

// The guard on the defect that shipped in chart 0.1.1.
//
// DEV_INSECURE_COOKIES drops the Secure attribute from both session cookies.
// The helper that emits it read correctly — "derived from the scheme unless
// set" — and rendered `0` for an HTTPS install, which the binary read as *on*.
// So the assertion is on the **rendered ConfigMap**, not on the template text:
// a guard that grepped _helpers.tpl would have passed on the version that
// shipped this.
func TestChartNeverWeakensCookiesByDefault(t *testing.T) {
	// testValues carries an https:// baseUrl and says nothing about cookies,
	// which is the install the defect was found on.
	//
	// Off is the empty string and nothing else. The binary read this name as
	// "set to anything at all" when the defect shipped, and reads it as a
	// boolean now; empty is the one spelling both of those agree on, so this
	// assertion means the same thing on either side of that change.
	if got := configMapValue(t, mustRender(t), "DEV_INSECURE_COOKIES"); got != "" {
		t.Errorf("an HTTPS install at default values renders DEV_INSECURE_COOKIES=%q — off is the "+
			"empty string, and %q was read as ON by the binary this chart shipped against, "+
			"minting every session cookie without Secure", got, got)
	}

	// And off has to stay expressible in the other direction, because an
	// omitted key would leave whatever the image says standing.
	if got := configMapValue(t, mustRender(t, "--set", "platform.insecureCookies=true"), "DEV_INSECURE_COOKIES"); got != "1" {
		t.Errorf("insecureCookies=true renders DEV_INSECURE_COOKIES=%q, want \"1\"", got)
	}

	// A plain-http install gets it whatever the values say: a Secure cookie
	// over http is dropped by the browser, and that is a sign-in that appears
	// to work and then does not.
	http := mustRender(t,
		"--set", "platform.baseUrl=http://lp.example.com",
		"--set", "platform.appsBaseUrl=http://apps.example.com",
		"--set", "ingress.tls.enabled=false")
	if got := configMapValue(t, http, "DEV_INSECURE_COOKIES"); got != "1" {
		t.Errorf("a plain-http install renders DEV_INSECURE_COOKIES=%q, want \"1\"", got)
	}
}

// The old typing was a string, and a string in a boolean's place is read as
// true by every non-empty value — which is how `insecureCookies: "0"` would
// carry the defect through an upgrade with the values file still saying off.
func TestChartRefusesAStringInsecureCookies(t *testing.T) {
	out, err := render(t, "--set-string", "platform.insecureCookies=0")
	if err == nil {
		t.Fatalf("the chart rendered insecureCookies=\"0\" instead of refusing it:\n%s", out)
	}
	if !strings.Contains(out, "must be true or false") {
		t.Errorf("the refusal does not say what to write instead:\n%s", out)
	}
}

// configMapValue reads one key out of the rendered ConfigMap. Deliberately not
// a YAML parse of the whole stream: helm template emits several documents and
// the point here is the one line a customer's install would carry.
func configMapValue(t *testing.T, rendered, key string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `:\s*"(.*)"\s*$`)
	m := re.FindStringSubmatch(rendered)
	if m == nil {
		t.Fatalf("no %s in the rendered ConfigMap; an omitted key leaves the image's own value standing", key)
	}
	return m[1]
}

// An isolated app reaches the platform at the callback listener and nowhere
// else: CALLBACK_URL names the callback port, the listener is configured, and
// the platform pod's NetworkPolicy admits the apps namespace to that port alone
// while every other namespace keeps every port. An app that could dial :8080
// could forge X-Forwarded-* from inside the trusted-proxy range (DEPLOY-3).
func TestAppsReachThePlatformOnlyAtTheCallbackPort(t *testing.T) {
	out := mustRender(t)
	for _, want := range []string{
		"- name: CALLBACK_ADDR\n              value: \":8082\"",
		"- name: CALLBACK_URL\n              value: http://toft.default.svc.cluster.local:8082\n",
		"- name: callback\n              containerPort: 8082",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the Deployment lacks %q", want)
		}
	}

	var svc, external, np string
	for _, doc := range strings.Split(out, "\n---") {
		switch {
		case strings.Contains(doc, "\nkind: Service\n") && strings.Contains(doc, "\n  name: toft\n"):
			svc = doc
		case strings.Contains(doc, "\nkind: Service\n") && strings.Contains(doc, "\n  name: toft-external\n"):
			external = doc
		case strings.Contains(doc, "\nkind: NetworkPolicy\n") && strings.Contains(doc, "\n  name: toft\n"):
			np = doc
		}
	}
	if !strings.Contains(svc, "- name: callback\n      port: 8082\n      targetPort: callback") {
		t.Errorf("the platform Service does not publish the callback port:\n%s", svc)
	}
	if strings.Contains(external, "callback") {
		t.Errorf("the external Service publishes the callback port:\n%s", external)
	}
	if np == "" {
		t.Fatal("no NetworkPolicy fences the platform pod; every isolated app can reach :8080")
	}
	rules := strings.SplitN(np, "\n    - from:", 3)
	if len(rules) != 3 {
		t.Fatalf("want two ingress rules on the platform's NetworkPolicy:\n%s", np)
	}
	others, apps := rules[1], rules[2]
	for _, want := range []string{
		"operator: NotIn\n                values: [\"toft-apps\"]",
		"- ipBlock:\n            cidr: \"10.0.200.0/24\"",
	} {
		if !strings.Contains(others, want) {
			t.Errorf("the non-apps rule lacks %q:\n%s", want, others)
		}
	}
	if strings.Contains(others, "ports:") {
		t.Errorf("the non-apps rule lists ports, which would close a scrape port:\n%s", others)
	}
	for _, want := range []string{
		"kubernetes.io/metadata.name: \"toft-apps\"",
		"ports:\n        - port: 8082\n",
	} {
		if !strings.Contains(apps, want) {
			t.Errorf("the apps rule lacks %q:\n%s", want, apps)
		}
	}
	if strings.Contains(apps, "8080") || strings.Contains(apps, "8081") || strings.Contains(apps, "ipBlock") {
		t.Errorf("the apps rule admits more than the callback port:\n%s", apps)
	}

	if strings.Contains(mustRender(t, "--set", "networkPolicy.enabled=false"), "values: [\"toft-apps\"]") {
		t.Error("networkPolicy.enabled=false still renders the platform's NetworkPolicy")
	}
}

// Apps on hostnames of their own reach the install only through names the
// operator lists: each a rule to the apps listener and a TLS host, and none
// at all by default — and the isolated apps' NetworkPolicy is not touched.
func TestIngressRoutesListedAppHostsToTheAppsListener(t *testing.T) {
	plain := mustRender(t, "--set", "ingress.className=alb", "--set", "ingress.allowPublicInbound=true",
		"--set", "ingress.tls.enabled=true")
	ing := ingressDoc(t, plain)
	if strings.Contains(ing, "reports.example.com") {
		t.Fatalf("an app host rendered with none listed:\n%s", ing)
	}

	with := mustRender(t, "--set", "ingress.className=alb", "--set", "ingress.allowPublicInbound=true",
		"--set", "ingress.tls.enabled=true", "--set", "ingress.appHosts={reports.example.com,*.apps.example.org}")
	ing = ingressDoc(t, with)
	for _, want := range []string{
		`- host: "reports.example.com"`,
		`- host: "*.apps.example.org"`,
		`- "reports.example.com"`,
	} {
		if !strings.Contains(ing, want) {
			t.Errorf("the Ingress does not carry %q:\n%s", want, ing)
		}
	}
	rule := ing[strings.Index(ing, `- host: "reports.example.com"`):]
	if !strings.Contains(rule[:strings.Index(rule+"\n    - host", "\n    - host")], "name: apps") {
		t.Errorf("the app host is not routed to the apps port:\n%s", rule)
	}

	// Everything but the Ingress is the same chart — the Secret aside, whose
	// generated values (and so the pod's checksum of it) differ on every render.
	checksum := regexp.MustCompile(`checksum/secret: [0-9a-f]+`)
	strip := func(s string) string {
		var keep []string
		for _, doc := range strings.Split(s, "\n---") {
			if !strings.Contains(doc, "\nkind: Ingress\n") && !strings.Contains(doc, "\nkind: Secret\n") {
				keep = append(keep, doc)
			}
		}
		return checksum.ReplaceAllString(strings.Join(keep, "\n---"), "")
	}
	if strip(plain) != strip(with) {
		t.Error("listing app hosts changed something other than the Ingress")
	}
}

// The binary refuses to start with KUBE_NAMESPACE set and no artifact store or
// no runner image (config.validateIsolated). The chart used to emit the
// namespace unconditionally, so an install at the documented default values —
// the two URLs and nothing else — rendered, installed, printed its sign-in
// instructions and crash-looped.
func TestTheDefaultValuesRenderAnInstallTheBinaryStarts(t *testing.T) {
	urls := []string{"--set", "platform.baseUrl=https://a.example", "--set", "platform.appsBaseUrl=https://b.example"}
	out, err := renderBare(t, urls...)
	if err != nil {
		t.Fatalf("the default values do not render: %v\n%s", err, out)
	}
	if regexp.MustCompile(`(?m)^\s*KUBE_NAMESPACE:`).MatchString(out) {
		t.Error("the default values emit KUBE_NAMESPACE with no artifact store and no runner image; " +
			"the binary refuses to start in that shape")
	}
	if regexp.MustCompile(`(?m)^\s*RUNNER_`).MatchString(out) {
		t.Error("the default values emit a RUNNER_* line outside isolated mode")
	}
	// `helm template` does not render NOTES.txt, so its condition is read.
	if b, err := os.ReadFile(filepath.Join(chartDir(t), "templates", "NOTES.txt")); err != nil ||
		!strings.Contains(string(b), `if not (include "toft.isolatedEnabled" .)`) {
		t.Error("NOTES.txt does not list isolated mode as absent when toft.isolatedEnabled is false")
	}
	// A store and no runner is shared mode with a store, and legal.
	out, err = renderBare(t, append(urls, "--set", "artifactStore.enabled=true", "--set", "artifactStore.bucket=b")...)
	if err != nil {
		t.Fatalf("an artifact store with no runner image was refused: %v\n%s", err, out)
	}
	if regexp.MustCompile(`(?m)^\s*KUBE_NAMESPACE:`).MatchString(out) {
		t.Error("an artifact store with no runner image emits KUBE_NAMESPACE")
	}

	isolated := mustRender(t)
	if got := configMapValue(t, isolated, "KUBE_NAMESPACE"); got != "toft-apps" {
		t.Errorf("with a store and a runner image KUBE_NAMESPACE = %q, want toft-apps", got)
	}
	if got := configMapValue(t, isolated, "RUNNER_IMAGE_PYTHON"); got == "" {
		t.Error("with a store and a runner image the runner image is not emitted")
	}
}
