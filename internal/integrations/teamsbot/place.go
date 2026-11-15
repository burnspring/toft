package teamsbot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"toft/internal/database"
	"toft/internal/integrations"
)

// The Teams hosts a pasted link may come from.
var linkHosts = map[string]bool{
	"teams.microsoft.com":   true,
	"teams.cloud.microsoft": true,
}

// Link is what a pasted Teams link names.
type Link struct {
	Kind           string // database.PlaceTeam or database.PlaceChat
	ConversationID string
}

// ParseLink reads a pasted team or chat link. It refuses anything it cannot
// read rather than guessing — Microsoft's link formats are observations, not a
// contract — and a channel's link, which names the channel and not the team the
// Bot Connector lists channels by. tenantID, when the link carries one, has to
// be the connection's.
func ParseLink(raw, tenantID string) (Link, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !linkHosts[strings.ToLower(u.Hostname())] {
		return Link{}, errors.New("paste a link from Teams — on the team, ⋯ › Get link to team; " +
			"in a chat, ⋯ › Copy link")
	}
	if t := u.Query().Get("tenantId"); t != "" && tenantID != "" && !strings.EqualFold(t, tenantID) {
		return Link{}, fmt.Errorf("that link is from another Microsoft 365 tenant (%s), not "+
			"this connection's (%s)", t, tenantID)
	}
	// /l/{kind}/{19:…@thread.…}/…, the id percent-encoded or not.
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(parts) < 3 || parts[0] != "l" {
		return Link{}, errors.New("that is not a link to a team or a chat")
	}
	id, err := url.PathUnescape(parts[2])
	if err != nil || !strings.HasPrefix(id, "19:") || !strings.Contains(id, "@thread.") {
		return Link{}, errors.New("that link does not name a Teams team or chat")
	}
	switch parts[1] {
	case "team":
		return Link{Kind: database.PlaceTeam, ConversationID: id}, nil
	case "chat":
		return Link{Kind: database.PlaceChat, ConversationID: id}, nil
	case "channel":
		return Link{}, errors.New("that is a channel's link — use ⋯ › Get link to team on the " +
			"team instead; its channels are listed from there")
	default:
		return Link{}, errors.New("that link does not name a Teams team or chat")
	}
}

// VerifyPlace asks the Bot Connector for a place and returns its name. It
// answers only where the bot is installed, so a refusal here is the membership
// answer: NotInstalled reports it. A chat's name is not something the connector
// returns; chatName is the administrator's.
func VerifyPlace(ctx context.Context, target integrations.Target, l Link,
	chatName string) (string, error) {

	switch l.Kind {
	case database.PlaceTeam:
		var team struct {
			Name string `json:"name"`
		}
		if err := call(ctx, target, http.MethodGet,
			"/v3/teams/"+url.PathEscape(l.ConversationID), nil, &team, "the team"); err != nil {
			return "", err
		}
		if _, err := Channels(ctx, target, l.ConversationID); err != nil {
			return "", err
		}
		if team.Name == "" {
			team.Name = "Team"
		}
		return team.Name, nil
	case database.PlaceChat:
		if err := call(ctx, target, http.MethodGet,
			"/v3/conversations/"+url.PathEscape(l.ConversationID)+"/members", nil, nil,
			"the chat"); err != nil {
			return "", err
		}
		name := strings.TrimSpace(chatName)
		if name == "" {
			return "", errors.New("give this chat a name — Teams does not say what a chat is called")
		}
		return name, nil
	}
	return "", fmt.Errorf("%q is not a place kind", l.Kind)
}
