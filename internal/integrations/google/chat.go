package google

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"toft/internal/database"
	"toft/internal/integrations"
)

// Chat posts an app's messages into a Google Chat space.
//
// The boundary is the same as Slack's and stated the same way: an app may post
// to exactly the spaces somebody added the Chat app to, because those are the
// only ones `spaces.list` can return. There is no way to reach a space nobody
// invited it into, and no configuration here that could widen that.
type Chat struct{}

var (
	_ integrations.Transport  = Chat{}
	_ integrations.Enumerator = Chat{}
	_ integrations.Verifier   = Chat{}
)

// Send posts one message.
//
// The text is the app's, and the attribution line is appended here — composed
// on this side and not reachable from anything app code sent. Chat takes plain
// text with a small markup vocabulary, so unlike Slack there is no block tree
// an app could use to reproduce the platform's own formatting; the attribution
// is a second paragraph rather than a structural element for the same reason it
// is a block there.
func (Chat) Send(ctx context.Context, target integrations.Target,
	msg integrations.Send) (integrations.Result, error) {

	space := target.Destination.ID
	if space == "" {
		space = strings.TrimSpace(target.Connection.DefaultDestination.ID)
	}
	if space == "" {
		return integrations.Result{}, errors.New("this app has no Google Chat space to post to")
	}
	if !strings.HasPrefix(space, "spaces/") {
		space = "spaces/" + space
	}

	accessToken, err := token(ctx, target, ScopeChatBot)
	if err != nil {
		return integrations.Result{}, err
	}

	body, err := json.Marshal(map[string]string{"text": withAttribution(msg)})
	if err != nil {
		return integrations.Result{}, err
	}
	return integrations.Result{}, call(ctx, accessToken, "POST",
		chatHost+"/v1/"+space+"/messages", "application/json", body, nil, "the message")
}

// Destinations lists the spaces the Chat app has been added to.
func (Chat) Destinations(ctx context.Context,
	target integrations.Target) ([]database.Destination, error) {

	accessToken, err := token(ctx, target, ScopeChatBot)
	if err != nil {
		return nil, err
	}
	type space struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		SpaceType   string `json:"spaceType"`
	}
	var spaces []space
	query := url.Values{"pageSize": {"200"}}
	for page := 0; page < listPageCap; page++ {
		var list struct {
			Spaces        []space `json:"spaces"`
			NextPageToken string  `json:"nextPageToken"`
		}
		if err := call(ctx, accessToken, "GET", chatHost+"/v1/spaces?"+query.Encode(), "", nil,
			&list, "the space list"); err != nil {
			return nil, err
		}
		spaces = append(spaces, list.Spaces...)
		if list.NextPageToken == "" {
			break
		}
		query.Set("pageToken", list.NextPageToken)
	}
	out := make([]database.Destination, 0, len(spaces))
	for _, s := range spaces {
		label := s.DisplayName
		if label == "" {
			// A direct message has no display name. Naming it by its id beats
			// an empty row in a menu.
			label = s.Name
		}
		out = append(out, database.Destination{ID: s.Name, Label: label})
	}
	return out, nil
}

// Verify proves the credential. It cannot prove membership of a space — that is
// what the space list is for, and it is the picker rather than the form.
func (Chat) Verify(ctx context.Context, target integrations.Target) (string, error) {
	sa, err := From(target)
	if err != nil {
		return "", err
	}
	if _, err := sa.Token(ctx, ScopeChatBot); err != nil {
		return "", err
	}
	return sa.ClientEmail, nil
}

// withAttribution appends the line that says an application sent this.
//
// A platform notification's title and text are neutralised: Chat parses
// `<https://evil.example|Sign in>` as a link and `<users/all>` as a mention of
// the whole space, and the catalog poster composes that text from an app name,
// a job's stderr or a build-log tail. The name in the attribution loses `<`,
// `>` and `|` always, so it cannot close the link to the app and open another.
func withAttribution(msg integrations.Send) string {
	title, text := msg.Title, msg.Text
	if msg.FromPlatform {
		title, text = neutralise(title), neutralise(text)
	}
	if title != "" {
		text = "*" + title + "*\n" + text
	}
	from := strings.NewReplacer("<", "", ">", "", "|", "").Replace(msg.AppName)
	if from == "" {
		from = "an application"
	}
	if msg.AppURL != "" {
		return text + "\n\n_Sent by " + msg.Attribution("<"+msg.AppURL+"|"+from+">") + "_"
	}
	return text + "\n\n_Sent by " + msg.Attribution(from) + "_"
}

// neutralise replaces the characters Chat's text format reads as a link or a
// mention, `<` and `>`, with look-alikes it reads as nothing. Emphasis
// characters are left: harmless, and replacing `_` or `*` would break the
// install link a notification carries.
var neutralise = strings.NewReplacer("<", "\u2039", ">", "\u203a").Replace
