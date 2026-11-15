// Package pagerduty triggers and resolves incidents from an app.
//
// # Why resolve is here
//
// It would have been easy to ship the trigger half alone, and it would have
// been wrong. An app that may wake somebody up and may not put them back to bed
// leaves a page nobody on this side can silence — which is the residue rule
// applied to the one residue that wakes people up. So both, and the shape of
// the pair is what makes it safe.
//
// # The dedup key is namespaced, and that is the whole boundary
//
// Every key an app sends is prefixed with its app id before it leaves. An app
// can therefore only resolve what it triggered: not because a check refuses a
// cross-app resolve, but because the key it would have to name is not a key it
// can construct. That is the same shape as scoping a query by `app_id` inside
// the query rather than checking ownership afterwards, and it is the reason
// this route needs no ownership check of its own.
//
// # What this does not do
//
// **The platform seam does not resolve incidents.** Toft closing an
// incident in somebody's on-call system because an `app_events` row was marked
// resolved is either this feature's best day or a boundary this product should
// not cross, and it is a decision worth taking on its own with an install that
// has run the trigger half for a while. Not in this change.
package pagerduty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"toft/internal/appint"
	"toft/internal/integrations"
)

// Timeout bounds one call.
const Timeout = 30 * time.Second

// eventsEndpoint is the only host this package talks to, and it is not
// configurable.
var (
	eventsEndpoint = "https://events.pagerduty.com/v2/enqueue"
	httpClient     = &http.Client{Timeout: Timeout}
)

// Severities the Events API accepts. Anything else is normalised to `error`
// rather than refused: an app that says "urgent" means something bad, and
// failing the page over a vocabulary mismatch is the worst possible moment to
// be pedantic.
var severities = map[string]bool{
	"critical": true, "error": true, "warning": true, "info": true,
}

// Transport is PagerDuty behind the catalog's registry.
type Transport struct{}

var (
	_ integrations.Transport = Transport{}
	_ integrations.Verifier  = Transport{}
)

// New builds the transport.
func New() Transport { return Transport{} }

// Send triggers or resolves, by the action the catalog named.
func (Transport) Send(ctx context.Context, target integrations.Target,
	msg integrations.Send) (integrations.Result, error) {

	routingKey := strings.TrimSpace(target.Credential("routing_key"))
	if routingKey == "" {
		return integrations.Result{}, errors.New("this connection has no integration key")
	}

	action := msg.Action
	if action != "trigger" && action != "resolve" {
		return integrations.Result{}, fmt.Errorf("%q is not something PagerDuty does", action)
	}
	key := DedupKey(target, msg)
	if key == "" {
		return integrations.Result{}, errors.New(
			"PagerDuty needs a dedup_key — it is what says which incident this is, and " +
				"what closes it later")
	}

	event := map[string]any{
		"routing_key":  routingKey,
		"event_action": action,
		"dedup_key":    key,
	}
	if action == "trigger" {
		severity := strings.ToLower(strings.TrimSpace(msg.Severity))
		if !severities[severity] {
			severity = "error"
		}
		summary := msg.Text
		if msg.Title != "" {
			summary = msg.Title + " — " + summary
		}
		// PagerDuty's own cap on a summary. Trimmed rather than refused: this
		// is the one transport where a refusal means nobody is woken up, and a
		// shortened page beats no page.
		if len(summary) > 1024 {
			summary = summary[:1021] + "…"
		}
		source := msg.AppName
		if source == "" {
			source = msg.PlatformName()
		}
		payload := map[string]any{
			"summary":  summary,
			"severity": severity,
			// The app is the source, always, and it is not something app code
			// can set: an app that could page as another app is an app that can
			// send somebody to the wrong runbook at 3am.
			"source":    source,
			"component": msg.PlatformName(),
		}
		event["payload"] = payload
		if msg.AppURL != "" {
			event["links"] = []map[string]string{
				{"href": msg.AppURL, "text": msg.Attribution(source)},
			}
		}
	}

	body, err := json.Marshal(event)
	if err != nil {
		return integrations.Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, eventsEndpoint,
		bytes.NewReader(body))
	if err != nil {
		return integrations.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return integrations.Result{}, appint.Retryable(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode/100 == 2 {
		// The ledger records the key rather than the destination: "which
		// incident was this" is the question somebody asks afterwards, and the
		// service is the connection's.
		return integrations.Result{Destination: key}, nil
	}

	var refusal struct {
		Message string   `json:"message"`
		Errors  []string `json:"errors"`
	}
	_ = json.Unmarshal(raw, &refusal)
	msgText := refusal.Message
	if len(refusal.Errors) > 0 {
		msgText += ": " + strings.Join(refusal.Errors, ", ")
	}
	if strings.TrimSpace(msgText) == "" {
		msgText = strings.TrimSpace(string(raw))
	}
	failure := fmt.Errorf("PagerDuty refused the event: %s", msgText)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5 {
		return integrations.Result{}, appint.Retryable(failure)
	}
	return integrations.Result{}, failure
}

// Verify checks that a key is present. There is nothing else to prove without
// sending an event, and an event is a page — a "verification" that woke
// somebody up would be a test button wearing a disguise, and this feature has a
// test button that says what it is.
func (Transport) Verify(_ context.Context, target integrations.Target) (string, error) {
	key := strings.TrimSpace(target.Credential("routing_key"))
	if key == "" {
		return "", errors.New(
			"a PagerDuty connection needs the integration key from an Events API v2 " +
				"integration on the service it should page")
	}
	if len(key) < 20 {
		return "", errors.New(
			"that does not look like an Events API v2 integration key — it is the 32-character " +
				"key on the service's Integrations tab, not an API token")
	}
	label := strings.TrimSpace(target.Connection.ScopeLabel)
	if label == "" {
		label = "a PagerDuty service"
	}
	return label, nil
}

// DedupKey is the key that leaves, namespaced with the app.
//
// One function, exported, because the boundary this feature turns on is that an
// app can only resolve what it triggered — and that is true only if the trigger
// path and the resolve path namespace identically. Two call sites computing it
// separately is exactly how they come to disagree.
func DedupKey(target integrations.Target, msg integrations.Send) string {
	key := strings.TrimSpace(msg.DedupKey)
	if key == "" {
		return ""
	}
	// The **app id**, not its name: a name is a label somebody can change, and
	// renaming an app must not orphan the incident it opened this morning.
	app := strings.TrimSpace(msg.AppID)
	if app == "" {
		app = target.Connection.ID
	}
	return app + ":" + key
}
