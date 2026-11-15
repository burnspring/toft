// Package slack posts the platform's notifications into a Slack workspace.
//
// It is the second delivery channel, and it inherits every rule the first one
// established (ADR §14), because none of them was ever about SMTP:
//
//   - Slack is optional. An install with no bot token posts nothing and errors
//     at nothing; every feature that notifies still works, silently.
//   - A post never fails the request that triggered it. PostAsync returns
//     nothing and runs after the response is decided.
//   - The credential goes in and never comes out.
//
// What this package is not: a chat integration. Toft posts and does not
// listen. There is no socket, no command handler, no event subscription, and
// the bot needs exactly one scope to do its job — chat:write — plus the two
// read scopes that let an administrator pick a channel from a menu instead of
// pasting an id. A Slack app that can be *talked to* is a remote-control
// surface for the platform, and nothing here wants one.
//
// The transport is deliberately small: net/http and encoding/json, no SDK. A
// provider's client library would bring a transitive tree to POST one JSON
// document, exactly as it would have for the sending-API mail transport.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"toft/internal/notify"
)

// PostTimeout bounds one attempt. There is no retry queue behind it: an API
// that is down when we call loses the message, and the log says so. A 429 is
// treated the same way — this is not a delivery platform with a queue, and
// pretending otherwise would need the outbox ADR §14 rules out.
const PostTimeout = 30 * time.Second

// apiBase is the only host this package talks to, and it is a constant rather
// than a setting for the reason mail.resendEndpoint is: a configurable API base
// is a way to aim a stored credential at an arbitrary server, which is a worse
// thing to hand an administrator than a provider they cannot change.
const apiBase = "https://slack.com/api/"

// httpClient is shared: one connection pool, and the same timeout the mail
// transports use so a hung Slack cannot outlive a hung relay.
var httpClient = &http.Client{Timeout: PostTimeout}

// Config is a decrypted view of the stored settings. The token is plaintext
// here and nowhere else: it is built at the point of use and dropped when the
// call returns.
type Config struct {
	Token string
	// DefaultChannel is the channel id administrator notifications go to, and
	// the fallback for an attached app whose owner has not chosen one.
	DefaultChannel string
	// baseURL redirects the API call at a test server. Unexported so nothing
	// outside this package — and so nothing an administrator can type — can aim
	// the credential somewhere else.
	baseURL string
}

// Message is one post: a channel and the rendered blocks, plus the plain-text
// fallback Slack shows in notifications and on clients that cannot render
// blocks. Audience is set by Render from the template's own declaration, never
// by the caller, so a new post site cannot forget it and quietly escape the
// administrator's switches.
type Message struct {
	Channel  string
	Text     string
	Blocks   []Block
	Audience string
}

// Validate is deliberately shallow — it catches the blank field, not a wrong
// credential, which only a real post can tell you about. It is the check for a
// configuration that must post to its default channel; listing channels and
// posting to a chosen one need only validateToken.
func (c Config) Validate() error {
	if err := c.validateToken(); err != nil {
		return err
	}
	if strings.TrimSpace(c.DefaultChannel) == "" {
		return errors.New("a default channel is required")
	}
	return nil
}

// validateToken is Validate without the default channel: what every use of a
// connection needs, including the menu whose job is to pick the first channel.
// The places that genuinely post to the default check for one themselves.
func (c Config) validateToken() error {
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("a Slack bot token is required")
	}
	// A token that cannot go in a header is caught here rather than by the
	// transport, whose error text is about header syntax and tells an
	// administrator nothing about what they pasted.
	if strings.ContainsAny(c.Token, " \t\r\n") {
		return errors.New("the bot token contains a space or line break — it was probably pasted with surrounding text")
	}
	if !strings.HasPrefix(c.Token, "xoxb-") {
		// Not a security check — Slack decides what its tokens are. It catches
		// the overwhelmingly common paste error, which is a user token or a
		// signing secret, and says which one was wanted.
		return errors.New("that does not look like a bot token — Toft needs the Bot User OAuth Token, which starts with xoxb-")
	}
	return nil
}

// Post delivers one message synchronously. Callers that must not block use
// Service.PostAsync; this is for the test button, where an administrator is
// waiting for Slack's own error text.
func Post(ctx context.Context, cfg Config, msg Message) error {
	if err := cfg.validateToken(); err != nil {
		return err
	}
	if strings.TrimSpace(msg.Channel) == "" {
		return errors.New("no channel")
	}
	body := map[string]interface{}{
		"channel": msg.Channel,
		// text is the notification and fallback string, not a second copy of
		// the message. Slack shows it in the sidebar, in the desktop toast and
		// on any client that will not render blocks.
		"text": msg.Text,
	}
	if len(msg.Blocks) > 0 {
		body["blocks"] = msg.Blocks
	}
	_, err := call(ctx, cfg, "chat.postMessage", body)
	return err
}

// Channel is one conversation the bot can post to.
type Channel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Private bool   `json:"is_private"`
}

// channelPageLimit is what one users.conversations call asks for. Slack caps it
// well below this on some workspaces and returns a cursor either way, which is
// why the loop below exists.
const channelPageLimit = 200

// channelPageCap bounds the paging. A workspace with more channels than this in
// the bot's membership is not a case this feature needs to serve perfectly, and
// an unbounded loop against someone else's API is how a settings page hangs.
const channelPageCap = 10

// Channels lists the conversations the bot is a member of.
//
// This one call is both the channel menu and the validator, and that is what
// makes the boundary a sentence an administrator can act on: an app may post to
// exactly the channels somebody invited the bot into. Adding a channel to the
// menu is `/invite @toft` in that channel; removing one is `/kick`. There
// is no allow-list to maintain in Toft, and no way for an app owner to
// name a channel the workspace has not consented to.
//
// im and mpim are excluded: Toft posts to channels and does not DM people.
func Channels(ctx context.Context, cfg Config) ([]Channel, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("a Slack bot token is required")
	}
	out, err := listChannels(ctx, cfg, "public_channel,private_channel")
	var api *APIError
	if errors.As(err, &api) && api.Code == "missing_scope" {
		// A bot holding channels:read but not groups:read is refused the
		// two-type list outright. It keeps the public half of its menu rather
		// than losing the whole of it.
		out, err = listChannels(ctx, cfg, "public_channel")
	}
	if err != nil {
		if errors.As(err, &api) {
			if api.Code != "" {
				return nil, apiError(api.Code)
			}
			if api.Status == http.StatusTooManyRequests {
				return nil, apiError("ratelimited")
			}
		}
		return nil, err
	}
	return out, nil
}

// listChannels pages users.conversations for the given types. The method takes
// form-encoded arguments only — a JSON body is ignored, and Slack then answers
// with its defaults: public channels, archived included, 100 a page, the cursor
// unread.
func listChannels(ctx context.Context, cfg Config, types string) ([]Channel, error) {
	var out []Channel
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < channelPageCap; page++ {
		form := url.Values{
			"types":            {types},
			"exclude_archived": {"true"},
			"limit":            {strconv.Itoa(channelPageLimit)},
		}
		if cursor != "" {
			form.Set("cursor", cursor)
		}
		raw, err := callForm(ctx, cfg, "users.conversations", form)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Channels []Channel `json:"channels"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("reading Slack's channel list: %w", err)
		}
		// De-duplicated by id, so a far side that ignores the cursor cannot
		// produce a menu of repeats.
		for _, c := range parsed.Channels {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			out = append(out, c)
		}
		cursor = parsed.Meta.NextCursor
		if cursor == "" {
			break
		}
	}
	return out, nil
}

// call makes one Slack Web API request and returns the raw response body.
//
// The one thing about Slack's API that must not be got wrong: a refusal comes
// back as **HTTP 200 with `"ok": false`**. Checking the status code alone would
// report every rejected token, every missing scope and every channel the bot
// was kicked out of as a successful post.
func call(ctx context.Context, cfg Config, method string, body map[string]interface{}) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("building the Slack request: %w", err)
	}
	base := apiBase
	if cfg.baseURL != "" {
		base = cfg.baseURL
	}
	endpoint, err := url.JoinPath(base, method)
	if err != nil {
		return nil, fmt.Errorf("building the Slack request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building the Slack request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := httpClient.Do(req)
	if err != nil {
		// The URL is in here; the credential is not — it travelled in a header,
		// and net/http does not echo header values into transport errors.
		return nil, fmt.Errorf("Slack: %w", err)
	}
	defer resp.Body.Close()
	// Read even on success: the connection is only reusable if the body is
	// drained, and these bodies are small.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, errors.New("Slack is rate-limiting this workspace (HTTP 429) — the message was dropped")
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Slack returned HTTP %d", resp.StatusCode)
	}

	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("Slack returned something that is not JSON (HTTP %d)", resp.StatusCode)
	}
	if !envelope.OK {
		return nil, apiError(envelope.Error)
	}
	return raw, nil
}

// apiError turns Slack's error slug into one line an administrator can act on.
// The slugs are precise and useless — "not_in_channel" is exactly right and
// tells someone who has never read Slack's API docs nothing about what to do.
func apiError(slug string) error {
	switch slug {
	case "invalid_auth", "token_revoked", "account_inactive":
		return errors.New("Slack rejected the bot token — re-install the app to your workspace and paste the new Bot User OAuth Token")
	case "not_authed":
		return errors.New("no bot token was sent to Slack")
	case "missing_scope":
		return errors.New("the Slack app is missing a scope — add chat:write (and channels:read, groups:read for the channel menu), then re-install it to the workspace")
	case "not_in_channel":
		return errors.New("the bot is not in that channel — run /invite for the Toft app in it")
	case "channel_not_found":
		return errors.New("that channel no longer exists, or the bot cannot see it — for a private channel the bot has to be invited before it is visible")
	case "is_archived":
		return errors.New("that channel is archived")
	case "ratelimited", "rate_limited":
		return errors.New("Slack is rate-limiting this workspace — the message was dropped")
	case "msg_too_long":
		return errors.New("the message was too long for Slack")
	case "":
		return errors.New("Slack refused the request without saying why")
	default:
		return fmt.Errorf("Slack refused the request: %s", slug)
	}
}

// audienceEnabled maps a message's audience onto the administrator's switches.
// An unrecognised audience — including the empty one — is refused rather than
// waved through, so a message that escapes both switches cannot be posted by
// omission. The mirror of mail's gate, and deliberately identical in shape.
func audienceEnabled(notifyAdmin, notifyApp bool, audience string) bool {
	switch audience {
	case notify.AudienceAdmin:
		return notifyAdmin
	case notify.AudienceApp:
		return notifyApp
	case notify.AudienceTest:
		// Reached only if a test message were handed to PostAsync; PostTest
		// does not consult this. Treated as a notification with no switch of
		// its own, which means: not posted this way.
		return false
	default:
		return false
	}
}
