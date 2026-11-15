// Package webhook posts an app's message to a URL an administrator typed.
//
// It is the catalog's generic escape hatch, and it is deliberately *one kind
// with one shape* rather than a kind-defining language. A "connector" whose
// URL, method, headers and body template an install could write would make the
// catalog pointless — the descriptor is what the gates, the UI, the ledger and
// the telemetry budget read, and a runtime-defined kind has no refusal copy, no
// destination model and no budget. What an install gets instead is this: a
// signed POST of a documented JSON body, to one welded URL, and whatever
// happens on the far side is the far side's business.
//
// Two things make it safe to hand an administrator.
//
// **The URL is server-side input.** It is dialled through safefetch, which is
// the one implementation of "this server is about to fetch a URL somebody else
// chose": https only, no credentials in the authority, and a private address
// refused at dial. There is no bypass flag, and an operator whose receiver is
// on 10.x cannot use this kind. That is a real cost and it is the right one —
// the alternative is a feature that can be talked into reading the metadata
// service, and "an administrator typed it" is not the property that would make
// that safe, because an administrator typing it is exactly the case.
//
// **The receiver can prove the message came from here.** Every request carries
// an HMAC over the exact bytes sent. A receiver that re-serializes the JSON
// before verifying will fail, and that is said in the documentation rather than
// worked around: the alternative is canonicalization, which is a second
// specification for both sides to disagree about.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"toft/internal/appint"
	"toft/internal/integrations"
	"toft/internal/safefetch"
)

// Timeout bounds one attempt. Shorter than the queue's own DeliverTimeout, so a
// receiver that hangs fails here with a sentence rather than being cut off by
// the worker with a context error.
const Timeout = 20 * time.Second

// SignatureHeader carries the timestamp and the MAC.
const SignatureHeader = "X-Toft-Signature"

// EventHeader names the kind of thing that happened, so a receiver can route
// without parsing the body.
const EventHeader = "X-Toft-Event"

// ToleranceWindow is how old a signature a receiver should accept. It is not
// enforced here — the sender cannot enforce the receiver's replay window — and
// it is documented so both sides pick the same number.
const ToleranceWindow = 5 * time.Minute

// MaxResponse caps what is read back. A receiver's error body is quoted into
// the ledger, and a receiver that answers with a megabyte of HTML should not be
// able to put a megabyte of HTML there.
const MaxResponse = 8 << 10

// Transport is the webhook kind behind the catalog's registry.
//
// It holds one safefetch client, built with no configured host, so every URL
// goes through the strict dial guard. There is deliberately no constructor
// taking a URL: the URL is the connection's, per send, and a client keyed to
// one would be a client per connection to keep in step.
type Transport struct {
	client *safefetch.Client
}

var (
	_ integrations.Transport = Transport{}
	_ integrations.Verifier  = Transport{}
)

// New builds the transport.
func New() Transport {
	return Transport{client: safefetch.New("", Timeout)}
}

// Payload is what a receiver is sent, and it is a **closed list**.
//
// Every field is something the platform already publishes about an app, and
// nothing here is the app's own structured data: an app that wants to send a
// document sends one through a kind that takes files. A body an app could shape
// freely would make this a proxy with a signature on it.
type Payload struct {
	// Event is the action, as the catalog names it: "webhook.post".
	Event string `json:"event"`
	// SentAt is when Toft accepted it, RFC 3339.
	SentAt string `json:"sent_at"`
	// SendID is the ledger row, so a receiver can be idempotent on it — which
	// is the whole answer to this transport's single retry.
	SendID string `json:"send_id"`

	App struct {
		Name string `json:"name"`
		URL  string `json:"url,omitempty"`
	} `json:"app"`

	Text     string `json:"text"`
	Title    string `json:"title,omitempty"`
	Severity string `json:"severity,omitempty"`
	DedupKey string `json:"dedup_key,omitempty"`
}

// Send posts one message.
func (t Transport) Send(ctx context.Context, target integrations.Target,
	msg integrations.Send) (integrations.Result, error) {

	endpoint := strings.TrimSpace(target.Connection.ScopeID)
	if err := safefetch.CheckURL(endpoint); err != nil {
		return integrations.Result{}, fmt.Errorf("this connection's URL %s", err)
	}
	secret := target.Credential("signing_secret")
	if secret == "" {
		return integrations.Result{}, errors.New("this connection has no signing secret")
	}

	payload := Payload{
		Event:    "webhook." + msg.Action,
		SentAt:   time.Now().UTC().Format(time.RFC3339),
		SendID:   msg.SendID,
		Text:     msg.Text,
		Title:    msg.Title,
		Severity: msg.Severity,
		DedupKey: msg.DedupKey,
	}
	payload.App.Name = msg.AppName
	payload.App.URL = msg.AppURL

	body, err := json.Marshal(payload)
	if err != nil {
		return integrations.Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return integrations.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(EventHeader, payload.Event)
	req.Header.Set(SignatureHeader, Sign(secret, time.Now().UTC(), body))

	client := t.client
	if client == nil {
		client = safefetch.New("", Timeout)
	}
	resp, err := client.For(endpoint).Do(req)
	if err != nil {
		// A dial the guard refused is not a transient failure and must never be
		// retried into a loop against the metadata service.
		if safefetch.RefusedAddress(err) {
			return integrations.Result{}, fmt.Errorf(
				"this receiver resolves to an address on this network, which Toft will not dial")
		}
		return integrations.Result{}, appint.Retryable(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		return integrations.Result{}, nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponse))
	err = fmt.Errorf("the receiver answered %d: %s",
		resp.StatusCode, strings.TrimSpace(string(detail)))
	// A 5xx might not repeat; a 4xx says the message was wrong, and the message
	// is still wrong on the second attempt.
	if resp.StatusCode/100 == 5 || resp.StatusCode == http.StatusTooManyRequests {
		return integrations.Result{}, appint.Retryable(err)
	}
	return integrations.Result{}, err
}

// Verify proves the URL at *save* time and returns the host as the scope's
// label.
//
// It resolves the name and refuses a private address here rather than only at
// dial, so an administrator whose receiver is inside the network learns it at
// the form instead of at 2am in a queue worker. It deliberately does **not**
// send anything: a "verification" that posted a message into somebody's
// receiver is a test button wearing a disguise, and this feature has a test
// button.
func (t Transport) Verify(_ context.Context, target integrations.Target) (string, error) {
	endpoint := strings.TrimSpace(target.Connection.ScopeID)
	if endpoint == "" {
		return "", errors.New("a webhook connection needs the URL Toft should post to")
	}
	if err := safefetch.CheckAddress(endpoint); err != nil {
		return "", fmt.Errorf("that URL %w", err)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("that is not a URL")
	}
	return u.Hostname(), nil
}

// Sign builds the header value: `t=<unix>,v1=<hex>` over `t.body`.
//
// The timestamp is *inside* the MAC, which is what makes it a replay bound
// rather than a suggestion: an attacker replaying yesterday's request cannot
// move the clock forward without invalidating the signature.
func Sign(secret string, at time.Time, body []byte) string {
	ts := fmt.Sprintf("%d", at.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature the way a receiver should, and exists so the
// documented algorithm has one executable definition on this side too.
func Verify(secret, header string, body []byte, now time.Time) error {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	if ts == "" || sig == "" {
		return errors.New("the signature header is not in the documented form")
	}
	var unix int64
	if _, err := fmt.Sscanf(ts, "%d", &unix); err != nil {
		return errors.New("the signature's timestamp is not a number")
	}
	age := now.Sub(time.Unix(unix, 0))
	if age < -ToleranceWindow || age > ToleranceWindow {
		return errors.New("the signature is outside the tolerance window")
	}
	want := Sign(secret, time.Unix(unix, 0), body)
	_, wantSig, _ := strings.Cut(want, "v1=")
	if !hmac.Equal([]byte(sig), []byte(wantSig)) {
		return errors.New("the signature does not match")
	}
	return nil
}
