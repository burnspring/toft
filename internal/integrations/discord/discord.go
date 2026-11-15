// Package discord posts an app's messages into a Discord channel.
//
// Unlike Teams, this one has a boundary an administrator can inspect: a bot
// token plus one server, and the channel list is what the bot can actually see
// there. Adding the bot to a server and giving it access to a channel is a
// decision somebody makes on Discord's side, and removing it takes effect
// immediately without touching Toft — which is the property Slack has and
// the property a webhook URL does not.
//
// The transport is deliberately small: net/http and encoding/json, no SDK. A
// library that maintains a gateway connection is a lot of machinery for a
// product that posts and does not listen — there is no socket here, no command
// handler and no event subscription, for the reason internal/slack has none: a
// bot that can be *talked to* is a remote-control surface for the platform.
package discord

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
	"toft/internal/database"
	"toft/internal/integrations"
)

// Timeout bounds one call.
const Timeout = 30 * time.Second

// apiBase is the only host this package talks to, and it is not configurable.
var (
	apiBase    = "https://discord.com/api/v10"
	httpClient = &http.Client{Timeout: Timeout}
)

// ContentLimit is Discord's own cap on a message. Exceeding it is a refusal at
// the handler rather than a truncation here, which is why the catalog carries
// the number.
const ContentLimit = 2000

// Transport posts to a channel.
type Transport struct{}

var (
	_ integrations.Transport  = Transport{}
	_ integrations.Enumerator = Transport{}
	_ integrations.Verifier   = Transport{}
)

// New builds the transport.
func New() Transport { return Transport{} }

// APIError is a failure Discord reported.
type APIError struct {
	Op      string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Discord refused %s: %s", e.Op, e.Message)
	}
	return fmt.Sprintf("Discord returned HTTP %d from %s", e.Status, e.Op)
}

// Retryable reports whether a second attempt could succeed. A rate limit or a
// server error could; "unknown channel" and "missing access" could not.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status/100 == 5
}

// Send posts one message as an embed.
//
// An embed rather than plain content, because the attribution has to be
// somewhere an app's own text cannot reach: the app supplies a string, this
// side builds the structure, and there is no markdown that deletes a footer.
func (Transport) Send(ctx context.Context, target integrations.Target,
	msg integrations.Send) (integrations.Result, error) {

	channel := target.Destination.ID
	if channel == "" {
		channel = target.Connection.DefaultDestination.ID
	}
	if channel == "" {
		return integrations.Result{}, errors.New("this app has no Discord channel to post to")
	}

	from := msg.AppName
	if from == "" {
		from = "an application"
	}
	// A platform notification's text is composed from an app name, a job's
	// stderr or a build-log tail, so its markdown is escaped: a masked link in
	// an embed description renders, in a message the install appears to have
	// sent. An app's own send keeps its markdown.
	title, text := msg.Title, msg.Text
	if msg.FromPlatform {
		title, text = escapeMarkdown(title), escapeMarkdown(text)
	}
	embed := map[string]any{
		"description": text,
		"footer":      map[string]string{"text": "Sent by " + msg.Attribution(from)},
	}
	if title != "" {
		embed["title"] = title
	}
	if msg.AppURL != "" {
		embed["url"] = msg.AppURL
	}
	body, err := json.Marshal(map[string]any{"embeds": []any{embed}})
	if err != nil {
		return integrations.Result{}, err
	}

	return integrations.Result{}, call(ctx, target, "POST",
		"/channels/"+url.PathEscape(channel)+"/messages", body, nil, "the message")
}

// Destinations lists the text channels the bot can see in the connection's
// server.
func (Transport) Destinations(ctx context.Context,
	target integrations.Target) ([]database.Destination, error) {

	guild := strings.TrimSpace(target.Connection.ScopeID)
	if guild == "" {
		return nil, errors.New("this connection has no server")
	}
	var channels []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type int    `json:"type"`
	}
	if err := call(ctx, target, "GET",
		"/guilds/"+url.PathEscape(guild)+"/channels", nil, &channels,
		"the channel list"); err != nil {
		return nil, err
	}
	out := make([]database.Destination, 0, len(channels))
	for _, c := range channels {
		// 0 is a text channel and 5 is an announcement channel. Everything else
		// — voice, stage, forum, category — is not somewhere a message goes,
		// and offering one is a menu entry that always fails.
		if c.Type != 0 && c.Type != 5 {
			continue
		}
		out = append(out, database.Destination{ID: c.ID, Label: c.Name})
	}
	return out, nil
}

// Verify proves the token and that the bot is in the server named, which is the
// half an administrator most often has not done yet.
func (t Transport) Verify(ctx context.Context, target integrations.Target) (string, error) {
	if strings.TrimSpace(target.Credential("bot_token")) == "" {
		return "", errors.New("a Discord connection needs its bot token")
	}
	guild := strings.TrimSpace(target.Connection.ScopeID)
	if guild == "" {
		return "", errors.New(
			"a Discord connection needs the server id — turn on Developer Mode in Discord, " +
				"right-click the server and Copy Server ID")
	}
	var info struct {
		Name string `json:"name"`
	}
	if err := call(ctx, target, "GET", "/guilds/"+url.PathEscape(guild), nil, &info,
		"that server"); err != nil {
		return "", fmt.Errorf("%w — if the token is right, the bot has not been added to that "+
			"server yet", err)
	}
	return info.Name, nil
}

func call(ctx context.Context, target integrations.Target, method, path string,
	body []byte, out any, op string) error {

	token := strings.TrimSpace(target.Credential("bot_token"))
	if token == "" {
		return errors.New("this connection has no bot token")
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return appint.Retryable(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return appint.Retryable(err)
	}
	if resp.StatusCode/100 != 2 {
		var refusal struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &refusal)
		msg := refusal.Message
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		apiErr := &APIError{Op: op, Status: resp.StatusCode, Message: msg}
		if apiErr.Retryable() {
			return appint.Retryable(apiErr)
		}
		return apiErr
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// escapeMarkdown backslash-escapes the syntax a masked link or a mention needs
// — `[`, `]`, `<` — and the backslash itself, so text cannot undo an escape.
// Emphasis and the rest are left: harmless, and escaping `_` or `(` inside the
// install link a notification carries would break it.
func escapeMarkdown(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\[]<`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
