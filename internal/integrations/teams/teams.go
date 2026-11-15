// Package teams posts into a Microsoft Teams channel through a Workflows
// incoming webhook.
//
// **It is a webhook, and the product says so in three places.** Posting to a
// channel as an *application* requires either delegated authority — a person,
// which is what a platform-held credential is definitionally not — or Teams'
// protected APIs, which are gated behind Microsoft's own approval process and
// their own billing. Neither is a thing an install can simply configure, so
// what this kind actually is is a Workflows URL: welded, unenumerable, and
// bounded by whoever created the workflow rather than by a membership an
// administrator can inspect.
//
// That difference matters and it is not cosmetic. Everywhere else in this
// catalog, "where may an app post" is a boundary somebody can see and change —
// the bot's membership, the site's grant. Here it is a URL, and the person who
// can change it is whoever owns the workflow in Power Automate. **A name that
// implies a Graph integration while delivering a webhook is the one thing this
// kind can get wrong**, and copy is the only place to fix it: the connection
// form says it, and the attach dialog says it again.
//
// The transport itself is small because the far side is: one POST of an
// Adaptive Card. It goes through safefetch like every other URL somebody typed.
//
// **The URL's query string is the credential.** Its `sig` is a signature the
// workflow checks, and anybody holding the URL posts. So the URL is split at
// save (SplitScope): scheme, host and path are the scope — welded, shown and
// audited — and the query is sealed with the connection's secret fields, put
// back together only here, at the point of use. No error this transport
// returns carries the query.
package teams

import (
	"bytes"
	"context"
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

// Timeout bounds one post.
const Timeout = 20 * time.Second

// MaxResponse caps what is read back into the ledger.
const MaxResponse = 8 << 10

// Transport posts to a Workflows webhook.
type Transport struct{ client *safefetch.Client }

var (
	_ integrations.Transport     = Transport{}
	_ integrations.Verifier      = Transport{}
	_ integrations.ScopeSplitter = Transport{}
)

// QueryKey is the sealed credential field holding the URL's query string.
const QueryKey = "query"

// New builds the transport.
func New() Transport { return Transport{client: safefetch.New("", Timeout)} }

// Send posts one message as an Adaptive Card.
//
// The card carries the app's text and, below it, a line naming the app and
// linking to it — composed here and not reachable from anything app code sent.
// An app never speaks in the platform's voice, and on this transport that is
// enforced by the app supplying a string rather than a card.
func (t Transport) Send(ctx context.Context, target integrations.Target,
	msg integrations.Send) (integrations.Result, error) {

	endpoint := Endpoint(target)
	if err := safefetch.CheckURL(endpoint); err != nil {
		return integrations.Result{}, fmt.Errorf("this connection's URL %s", err)
	}

	body, err := json.Marshal(Card(msg))
	if err != nil {
		return integrations.Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return integrations.Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := t.client
	if client == nil {
		client = safefetch.New("", Timeout)
	}
	resp, err := client.For(endpoint).Do(req)
	if err != nil {
		if safefetch.RefusedAddress(err) {
			return integrations.Result{}, errors.New(
				"this workflow URL resolves to an address on this network, which Toft will not dial")
		}
		return integrations.Result{}, appint.Retryable(redact(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 == 2 {
		return integrations.Result{}, nil
	}
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponse))
	failure := fmt.Errorf("the Teams workflow answered %d: %s",
		resp.StatusCode, strings.TrimSpace(string(detail)))
	if resp.StatusCode/100 == 5 || resp.StatusCode == http.StatusTooManyRequests {
		return integrations.Result{}, appint.Retryable(failure)
	}
	return integrations.Result{}, failure
}

// Verify checks the URL at save time — its shape and the address its host
// resolves to. It sends nothing: a "verification" that posted into somebody's
// channel is a test button wearing a disguise.
//
// The label is the workflow's id, read off the path: it is what Power Automate
// shows, so it is how an administrator finds the flow — and its owner — again.
func (t Transport) Verify(_ context.Context, target integrations.Target) (string, error) {
	endpoint := Endpoint(target)
	if strings.TrimSpace(target.Connection.ScopeID) == "" {
		return "", errors.New(
			"a Teams connection needs the Workflows webhook URL for the channel it posts to")
	}
	if err := safefetch.CheckAddress(endpoint); err != nil {
		return "", fmt.Errorf("that URL %w", err)
	}
	return Label(target.Connection.ScopeID), nil
}

// SplitScope keeps the query string — the signature — out of the scope.
func (Transport) SplitScope(raw string) (string, map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", nil, errors.New("that is not a URL")
	}
	query := u.RawQuery
	u.RawQuery, u.Fragment = "", ""
	if query == "" {
		return u.String(), nil, nil
	}
	return u.String(), map[string]string{QueryKey: query}, nil
}

// Endpoint is the URL the transport posts to: the scope with its sealed query
// put back. A connection saved before the split holds its whole URL as the
// scope and no sealed query, and posts to exactly what it holds.
func Endpoint(target integrations.Target) string {
	scope := strings.TrimSpace(target.Connection.ScopeID)
	if q := target.Credential(QueryKey); q != "" && !strings.Contains(scope, "?") {
		return scope + "?" + q
	}
	return scope
}

// Label names a connection by its workflow: "workflow 1663…92f4", from the
// `/workflows/<id>/` segment Power Automate puts in every such URL. A URL of
// another shape is labelled by its host.
func Label(scope string) string {
	u, err := url.Parse(strings.TrimSpace(scope))
	if err != nil {
		return "a Teams workflow"
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "workflows" && parts[i+1] != "" {
			id := parts[i+1]
			if len(id) > 12 {
				id = id[:4] + "…" + id[len(id)-4:]
			}
			return "workflow " + id
		}
	}
	if u.Hostname() != "" {
		return "a Teams workflow on " + u.Hostname()
	}
	return "a Teams workflow"
}

// scopeForDisplay is a URL with its query string and fragment removed.
func scopeForDisplay(scope string) string {
	if i := strings.IndexAny(scope, "?#"); i >= 0 {
		return scope[:i]
	}
	return scope
}

// redact strips the query from the URL a transport error names: net/http's
// *url.Error prints the whole request URL, signature and all, into what
// becomes the ledger's error column and the log.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return &url.Error{Op: ue.Op, URL: scopeForDisplay(ue.URL), Err: ue.Err}
	}
	return err
}

// Card builds the Adaptive Card message both Teams kinds post: a Workflows
// webhook takes it as the request body, and the Teams bot sends it as the
// activity. One function, so the attribution an app cannot suppress is one
// implementation (ADR §72).
//
// **A platform notification's text is never markdown.** The catalog poster
// composes it from an app name, a job's last stderr line or a build-log tail,
// any of which can carry `[Sign in again](https://evil.example)`, and a
// TextBlock renders that as a working link in a card the install appears to
// have sent. It goes out as TextRuns inside a RichTextBlock, which parse
// nothing; an app's own send keeps its markdown.
func Card(msg integrations.Send) map[string]any {
	blocks := []map[string]any{}
	if msg.Title != "" {
		if msg.FromPlatform {
			blocks = append(blocks, richText(msg.Title, true))
		} else {
			blocks = append(blocks, map[string]any{
				"type": "TextBlock", "text": msg.Title, "weight": "Bolder", "wrap": true,
			})
		}
	}
	if msg.FromPlatform {
		blocks = append(blocks, richText(msg.Text, false))
	} else {
		blocks = append(blocks, map[string]any{
			"type": "TextBlock", "text": msg.Text, "wrap": true,
		})
	}
	// The attribution, always, and last. Composed on this side. The name has
	// the link syntax taken out of it, so an app named `Pay](https://evil) [x`
	// cannot add a second link beside the one to the app (ADR §72).
	from := AttributionName(msg.AppName)
	if from == "" {
		from = "an application"
	}
	attribution := map[string]any{
		"type": "TextBlock", "wrap": true, "isSubtle": true, "spacing": "Small",
		"text": "Sent by " + msg.Attribution(from),
	}
	if msg.AppURL != "" {
		attribution["text"] = "Sent by " + msg.Attribution("["+from+"]("+msg.AppURL+")")
	}
	blocks = append(blocks, attribution)

	return map[string]any{
		"type": "message",
		"attachments": []map[string]any{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"type":    "AdaptiveCard",
				"version": "1.4",
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"body":    blocks,
			},
		}},
	}
}

// richText is one paragraph of literal text: a RichTextBlock's TextRun is never
// parsed as markdown.
func richText(text string, bold bool) map[string]any {
	run := map[string]any{"type": "TextRun", "text": text}
	if bold {
		run["weight"] = "Bolder"
	}
	return map[string]any{"type": "RichTextBlock", "inlines": []map[string]any{run}}
}

// AttributionName is an app's name as the Teams attribution may show it: the
// characters of markdown's link syntax removed.
func AttributionName(name string) string {
	return strings.NewReplacer("[", "", "]", "", "(", "", ")", "").Replace(name)
}
