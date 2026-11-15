package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client is the runner's side of the callback channel.
//
// It holds two credentials and keeps them apart on purpose: the workload's
// projected identity, presented to fetch the app's decrypted environment, and
// the narrowed run token used for everything afterwards. Bootstrap happens
// once and the environment is fetched once — the runner does not re-fetch
// secrets, and nothing that compromises it later inherits the ability.
//
// The identity is a *path* rather than a value, for the reason run.go states
// about reading it at the moment it is presented: kubelet rotates the file, so
// a cached copy would expire while the pod it names goes on living. That is
// also what lets a service workload exchange it for a fresh run token when the
// current one runs out, which is a narrower thing than bootstrapping again —
// see TokenResponse for what a refresh must not do.
//
// The file is still mounted, and app code in the pod can read it. That is the
// recorded trade: it reaches this app's own environment and its own release, as
// this pod and no other, and nothing on the cluster.
type Client struct {
	baseURL      string
	http         *http.Client
	identityPath string

	mu           sync.Mutex
	bootstrapped bool
	runToken     string
	// workloadName and isRun are what the refresh path needs to know, and both
	// are settled by bootstrap. A *run* never refreshes: its token expires with
	// the run, deliberately, and the platform refuses to exchange one.
	workloadName string
	isRun        bool

	// refreshMu serialises the exchange. Logs and consumption report from
	// different goroutines and expire at the same instant; without this they
	// would each mint a token and the second would replace a credential the
	// first had just installed.
	refreshMu sync.Mutex
}

// NewClient takes the *path* to the workload's projected identity, not its
// contents. See Client.
func NewClient(baseURL, identityPath string) *Client {
	return &Client{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		identityPath: identityPath,
		// Generous, because a cold platform behind a load balancer may be slow
		// to answer, but bounded: a runner that hangs forever on a callback is
		// an orphan consuming capacity that nothing knows about.
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// Bootstrap fetches everything the workload needs and narrows the runner's own
// credentials in the same step.
func (c *Client) Bootstrap(ctx context.Context, workloadName string) (*BootstrapResponse, error) {
	c.mu.Lock()
	done := c.bootstrapped
	c.mu.Unlock()

	// One bootstrap per process, still. The guard is not about the credential
	// — the identity file is re-readable and app code can read it too — it is
	// about the *response*: bootstrapping again would fetch the app's secrets a
	// second time and supersede the app token the running process holds.
	if done {
		return nil, fmt.Errorf("this runner has already bootstrapped")
	}

	token, err := c.identity()
	if err != nil {
		return nil, err
	}

	var out BootstrapResponse
	if err := c.bootstrapCall(ctx, token, workloadName, &out); err != nil {
		return nil, err
	}
	if err := CheckVersion(ContractVersion, out.Version); err != nil {
		return nil, err
	}

	// Narrow. From here the runner presents only the token app code may also
	// hold, so a compromise of the runner is no worse than a compromise of the
	// app it is running.
	c.mu.Lock()
	c.bootstrapped = true
	c.runToken = out.RunToken
	c.workloadName = workloadName
	c.isRun = out.RunID != ""
	c.mu.Unlock()

	return &out, nil
}

// bootstrapDialWindow is how long a bootstrap keeps redialling a platform that
// refused the connection. A variable so the tests need not wait it out.
var bootstrapDialWindow = 30 * time.Second

// bootstrapCall is the bootstrap request, redialled while the connection
// itself is refused. A pod's first moments are when a NetworkPolicy has not yet
// admitted its address — kube-router adds a new pod to the source set on its
// next sync — and a job pod, unlike a service, is not restarted to try again,
// so one refused dial was a failed run. Only a dial is retried: a request that
// never connected cannot have bootstrapped, while one that timed out after
// sending might have, and a second bootstrap supersedes the first's token.
func (c *Client) bootstrapCall(ctx context.Context, token, workloadName string, out *BootstrapResponse) error {
	deadline := time.Now().Add(bootstrapDialWindow)
	wait := 250 * time.Millisecond
	for {
		err := c.call(ctx, http.MethodPost, "/api/v1/runner/bootstrap", token,
			BootstrapRequest{Version: ContractVersion, WorkloadName: workloadName}, out)
		var op *net.OpError
		if err == nil || !errors.As(err, &op) || op.Op != "dial" || time.Now().Add(wait).After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(2*wait, 4*time.Second)
	}
}

// identity reads the projected token at the moment it is about to be
// presented. Never cached: kubelet rotates the file, and a runner holding what
// it read at process start is the bug this whole design removed from the pod
// spec.
func (c *Client) identity() (string, error) {
	return readIdentityToken(c.identityPath)
}

// refresh exchanges the workload's cluster-issued identity for a fresh run
// token, and is the whole answer to a service workload outliving its
// credential.
//
// `stale` is the token whose rejection prompted this. If it is no longer the
// one held, another goroutine has already exchanged and this call is a no-op —
// two reporters expiring at the same tick must not mint two tokens.
func (c *Client) refresh(ctx context.Context, stale string) error {
	c.mu.Lock()
	isRun, workload := c.isRun, c.workloadName
	c.mu.Unlock()

	// A run's token expires with the run, and that is what makes a leaked one
	// inert. The platform refuses to exchange it; not asking is the same
	// answer without the round trip.
	if isRun {
		return fmt.Errorf("a run's token is not refreshed")
	}

	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.RunToken() != stale {
		return nil
	}

	token, err := c.identity()
	if err != nil {
		return err
	}
	var out TokenResponse
	if err := c.call(ctx, http.MethodPost, "/api/v1/runner/token", token,
		TokenRequest{Version: ContractVersion, WorkloadName: workload}, &out); err != nil {
		return err
	}
	if out.RunToken == "" {
		return fmt.Errorf("/api/v1/runner/token returned no token")
	}

	c.mu.Lock()
	c.runToken = out.RunToken
	c.mu.Unlock()
	return nil
}

// RunToken is the narrowed credential, for export into app code.
func (c *Client) RunToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runToken
}

func (c *Client) ShipLogs(ctx context.Context, batch LogBatch) error {
	batch.Version = ContractVersion
	batch.Sent = time.Now().UTC()
	return c.report(ctx, "/api/v1/runner/logs", batch)
}

func (c *Client) ReportConsumption(ctx context.Context, r ConsumptionReport) error {
	r.Version = ContractVersion
	return c.report(ctx, "/api/v1/runner/consumption", r)
}

func (c *Client) ReportProgress(ctx context.Context, percent int) error {
	return c.report(ctx, "/api/v1/runner/progress",
		ProgressReport{Version: ContractVersion, Percent: percent})
}

func (c *Client) ReportResult(ctx context.Context, r ResultReport) error {
	r.Version = ContractVersion
	return c.report(ctx, "/api/v1/runner/result", r)
}

// report is every callback a workload makes after bootstrap: presented with
// the run token, and retried exactly once against a freshly exchanged one when
// the platform says that token is no longer good.
//
// Once, not until it works. A refusal that survives a new credential is not a
// credential problem, and a workload that retried a rejected report forever
// would be the flood this path was written to end rather than the fix for it.
func (c *Client) report(ctx context.Context, path string, body any) error {
	token := c.RunToken()
	err := c.call(ctx, http.MethodPost, path, token, body, nil)
	if !errors.Is(err, ErrUnauthorized) {
		return err
	}
	if rerr := c.refresh(ctx, token); rerr != nil {
		return fmt.Errorf("%w (refreshing it failed: %v)", err, rerr)
	}
	return c.call(ctx, http.MethodPost, path, c.RunToken(), body, nil)
}

// ReportStage tells the platform which stage the workload reached, so a
// workload that dies without a result is still attributable.
func (c *Client) ReportStage(ctx context.Context, stage FailureStage) error {
	return c.ShipLogs(ctx, LogBatch{Stage: &StageMark{Stage: stage}})
}

func (c *Client) call(ctx context.Context, method, path, token string, body, out any) error {
	if token == "" {
		return fmt.Errorf("no credential for %s", path)
	}

	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(VersionHeader, ContractVersion)

	resp, err := c.http.Do(req)
	if err != nil {
		// A runner that cannot reach the platform must fail loudly rather than
		// run on silently: the reconciler's sweep is what catches the orphan,
		// and it can only do that if the workload stops rather than quietly
		// continuing to consume capacity.
		return fmt.Errorf("%w: %s: %w", ErrCallbackUnreachable, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		body := strings.TrimSpace(string(snippet))
		// An HTML body is a proxy's error page, not the platform's answer: six
		// lines of markup in the app's log that say only the status again.
		if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") ||
			strings.HasPrefix(strings.ToLower(body), "<html") || strings.HasPrefix(strings.ToLower(body), "<!doctype") {
			body = http.StatusText(resp.StatusCode) + " (an HTML page, from whatever is in front of the platform)"
		}
		err := fmt.Errorf("%s: platform answered %d: %s", path, resp.StatusCode, body)
		// 401 is the one status a caller can act on rather than only report:
		// it is what an expired run token looks like, and the credential is
		// exchangeable. Every other refusal is about the request.
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%w: %w", ErrUnauthorized, err)
		}
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ErrUnauthorized is the platform rejecting the credential presented, as
// distinct from rejecting what was sent with it. A service workload's run
// token is short-lived by design, so this is a routine event with a routine
// answer — exchange the identity for a new one — rather than a failure.
var ErrUnauthorized = errUnauthorized{}

type errUnauthorized struct{}

func (errUnauthorized) Error() string { return "credential rejected" }

// ErrCallbackUnreachable is the platform being unreachable, as distinct from
// it answering with an error. Only the first justifies giving up on the
// workload entirely.
var ErrCallbackUnreachable = errCallbackUnreachable{}

type errCallbackUnreachable struct{}

func (errCallbackUnreachable) Error() string { return "platform callback unreachable" }
