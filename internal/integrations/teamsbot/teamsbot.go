// Package teamsbot posts an app's messages into Microsoft Teams as a bot.
//
// It is the governed peer of internal/integrations/teams. The webhook's boundary
// is whoever owns a Power Automate flow; this one's is **where the Toft app is
// installed** — a membership the Teams admin center shows and removes in one
// click, which is the boundary every other messaging kind in the catalog has.
//
// # How it reaches Teams
//
// Through the Bot Connector, with a client-credentials token for the bot's own
// Entra application (`graph.TokenFor` with the Bot Connector's scope). No Graph
// permission, no person, and **no inbound endpoint**: a proactive post to a
// conversation the bot is already in needs none, and Microsoft documents a
// global service URL for exactly that case.
//
// # Where it may post
//
// A bot is normally told where it lives by install events on an inbound
// endpoint, which an on-premises install usually cannot expose. So the places
// are pasted: an administrator pastes a team's or a chat's link, and the place
// is saved only after the Bot Connector answers for it (VerifyPlace) — which it
// does only where the bot is installed. Channels are not stored; Destinations
// asks for each pasted team's channels when the menu opens.
//
// Microsoft's boundary is the team: an installed bot could post to any channel
// of it. Holding one app to the one channel its attachment names is this
// transport's job, exactly as a Slack bot invited to five channels is held to
// the one attached. Send posts only to Target.Destination.
//
// ADR §72 is the argument; docs/dev-reference.md § App Integrations the mechanism.
package teamsbot

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
	"toft/internal/integrations/graph"
	"toft/internal/integrations/teams"
)

// Timeout bounds one call.
const Timeout = 30 * time.Second

// Scope is what the bot's token is for.
const Scope = "https://api.botframework.com/.default"

// The one host this package talks to, and how it gets a token.
//
// **Unexported variables, not settings**: a configurable connector base is a
// way to aim a stored bot credential at an arbitrary server — the rule
// graph.loginHost and slack.apiBase keep. Variables only so this package's tests
// can point them at a stub.
var (
	connectorBase = "https://smba.trafficmanager.net/teams"
	httpClient    = &http.Client{Timeout: Timeout}
	tokenFor      = graph.TokenFor
)

// The destination id prefixes. The prefix is ours, never inferred from the
// shape of Microsoft's ids, and it decides which Bot Connector call a send makes.
const (
	channelPrefix = "channel:"
	chatPrefix    = "chat:"
	// teamSep separates a channel destination's team from its channel, so a
	// destination says which pasted place it is in (PlaceInUse).
	teamSep = "|"
)

// ChannelDestination is the destination id of one channel of a pasted team.
func ChannelDestination(teamID, channelID string) string {
	return channelPrefix + teamID + teamSep + channelID
}

// ChatDestination is the destination id of a pasted chat.
func ChatDestination(chatID string) string { return chatPrefix + chatID }

// PlacePrefix is the prefix every destination inside a place starts with —
// what database.PlaceInUse searches for.
func PlacePrefix(p database.IntegrationPlace) string {
	if p.Kind == database.PlaceChat {
		return ChatDestination(p.ConversationID)
	}
	return channelPrefix + p.ConversationID + teamSep
}

// Transport posts as the bot.
type Transport struct{}

var (
	_ integrations.Transport  = Transport{}
	_ integrations.Enumerator = Transport{}
	_ integrations.Verifier   = Transport{}
)

// New builds the transport.
func New() Transport { return Transport{} }

// credentials reads the connection: the bot's app id is the welded scope,
// the tenant is config, the secret is sealed.
func credentials(t integrations.Target) graph.Credentials {
	return graph.Credentials{
		TenantID:     t.Config("tenant_id"),
		ClientID:     strings.TrimSpace(t.Connection.ScopeID),
		ClientSecret: t.Credential("client_secret"),
	}
}

// APIError is a failure the Bot Connector reported.
type APIError struct {
	Op      string
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code == "BotNotInConversationRoster" {
		return "Toft is not in that team or chat any more — add the Toft app to it in " +
			"Teams again, or choose another destination"
	}
	if e.Message != "" {
		return fmt.Sprintf("Teams refused %s: %s", e.Op, e.Message)
	}
	return fmt.Sprintf("Teams returned HTTP %d from %s", e.Status, e.Op)
}

// Retryable: a throttle or a server error could succeed later; a roster
// refusal, a missing conversation or a bad credential could not.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status/100 == 5
}

// NotInstalled reports the Bot Connector's refusal for a conversation the bot is
// not in — the membership answer every place check turns on.
func NotInstalled(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == "BotNotInConversationRoster"
}

// Verify proves the tenant, the bot's app id and the secret together by
// fetching a token. It cannot prove the Bot Framework registration without
// a conversation; the first pasted place does that.
func (Transport) Verify(ctx context.Context, target integrations.Target) (string, error) {
	creds := credentials(target)
	if creds.ClientID == "" {
		return "", errors.New("a Teams bot connection needs the bot's application (client) id")
	}
	if _, err := tokenFor(ctx, creds, Scope); err != nil {
		return "", err
	}
	id := creds.ClientID
	if len(id) > 8 {
		id = id[:8]
	}
	return "bot " + id, nil
}

// Send posts one card to the attachment's channel or chat, and nowhere else.
func (Transport) Send(ctx context.Context, target integrations.Target,
	msg integrations.Send) (integrations.Result, error) {

	dest := target.Destination.ID
	if dest == "" {
		return integrations.Result{}, errors.New(
			"no Teams channel or chat is chosen for this — an administrator picks one on the " +
				"app's Integrations tab, or a default on the connection")
	}
	activity := teams.Card(msg)
	switch {
	case strings.HasPrefix(dest, channelPrefix):
		channel := dest[len(channelPrefix):]
		if i := strings.Index(channel, teamSep); i >= 0 {
			channel = channel[i+len(teamSep):]
		}
		body := map[string]any{
			"isGroup": true,
			"channelData": map[string]any{
				"channel": map[string]string{"id": channel},
				"tenant":  map[string]string{"id": target.Config("tenant_id")},
			},
			"activity": activity,
		}
		return integrations.Result{}, call(ctx, target, http.MethodPost, "/v3/conversations",
			body, nil, "the post")
	case strings.HasPrefix(dest, chatPrefix):
		chat := dest[len(chatPrefix):]
		return integrations.Result{}, call(ctx, target, http.MethodPost,
			"/v3/conversations/"+url.PathEscape(chat)+"/activities", activity, nil, "the post")
	default:
		// Never defaulted to one call or the other: an id that is neither is a
		// bug upstream, and guessing would post somewhere nobody chose.
		return integrations.Result{}, fmt.Errorf("%q is not a Teams bot destination", dest)
	}
}

// Destinations is every channel of every pasted team, asked of the Bot Connector
// now, and every pasted chat. A team the bot has left is left out of the
// menu rather than failing it; the connection's place list says which.
func (Transport) Destinations(ctx context.Context,
	target integrations.Target) ([]database.Destination, error) {

	out := []database.Destination{}
	var firstErr error
	for _, p := range target.Places {
		if p.Kind == database.PlaceChat {
			out = append(out, database.Destination{ID: ChatDestination(p.ConversationID), Label: p.Name})
			continue
		}
		channels, err := Channels(ctx, target, p.ConversationID)
		if err != nil {
			if !NotInstalled(err) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, c := range channels {
			out = append(out, database.Destination{
				ID:    ChannelDestination(p.ConversationID, c.ID),
				Label: p.Name + " › " + c.Name,
			})
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// Channel is one channel of a team.
type Channel struct {
	ID   string
	Name string
}

// Channels lists a team's channels. It answers only where the bot is installed.
func Channels(ctx context.Context, target integrations.Target, teamID string) ([]Channel, error) {
	var resp struct {
		Conversations []struct {
			ID   string  `json:"id"`
			Name *string `json:"name"`
		} `json:"conversations"`
	}
	if err := call(ctx, target, http.MethodGet,
		"/v3/teams/"+url.PathEscape(teamID)+"/conversations", nil, &resp, "the channel list"); err != nil {
		return nil, err
	}
	out := make([]Channel, 0, len(resp.Conversations))
	for _, c := range resp.Conversations {
		// The General channel's name arrives null, to be localised by the
		// client; its id is the team's.
		name := "General"
		if c.Name != nil && *c.Name != "" {
			name = *c.Name
		}
		out = append(out, Channel{ID: c.ID, Name: name})
	}
	return out, nil
}

func call(ctx context.Context, target integrations.Target, method, path string,
	body any, out any, op string) error {

	token, err := tokenFor(ctx, credentials(target), Scope)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, connectorBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &refusal)
		apiErr := &APIError{Op: op, Status: resp.StatusCode,
			Code: refusal.Error.Code, Message: refusal.Error.Message}
		if apiErr.Message == "" && apiErr.Code == "" {
			apiErr.Message = strings.TrimSpace(string(raw))
		}
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
