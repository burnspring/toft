package teamsbot

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/integrations"
	"toft/internal/integrations/graph"
)

const (
	tenant  = "8115b923-66c5-4aba-90e1-3f2981afc76a"
	teamID  = "19:team@thread.tacv2"
	general = "19:team@thread.tacv2"
	builds  = "19:builds@thread.tacv2"
	chatID  = "19:chat@thread.v2"
)

type request struct {
	Method, Path string
	Body         map[string]any
	Auth         string
}

// stub aims the package at a recorded Bot Connector and a token function that
// never leaves the process. respond decides the answer per request.
func stub(t *testing.T, respond func(r request) (int, any)) *[]request {
	t.Helper()
	var seen []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := request{Method: r.Method, Path: r.URL.EscapedPath(), Auth: r.Header.Get("Authorization")}
		_ = json.Unmarshal(raw, &req.Body)
		seen = append(seen, req)
		status, body := respond(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	oldBase, oldClient, oldToken := connectorBase, httpClient, tokenFor
	connectorBase, httpClient = server.URL, server.Client()
	tokenFor = func(_ context.Context, c graph.Credentials, scope string) (string, error) {
		if scope != Scope {
			t.Errorf("the token was asked for %q, not the Bot Connector", scope)
		}
		if c.ClientSecret == "" {
			return "", errors.New("no secret")
		}
		return "bot-token", nil
	}
	t.Cleanup(func() { connectorBase, httpClient, tokenFor = oldBase, oldClient, oldToken })
	return &seen
}

func target(dest string, places ...database.IntegrationPlace) integrations.Target {
	return integrations.Target{
		Connection: database.IntegrationConnection{
			ID: "iconn-bot", Kind: "teamsbot", ScopeID: "fb149f69-7608-41ef-8dc4-41992f47b59e",
			Config: map[string]string{"tenant_id": tenant},
		},
		Credentials: map[string]string{"client_secret": "SECRET-VALUE"},
		Destination: database.Destination{ID: dest},
		Places:      places,
	}
}

func teamPlace() database.IntegrationPlace {
	return database.IntegrationPlace{Kind: database.PlaceTeam, ConversationID: teamID, Name: "Toft Test"}
}

func channelsOf(r request) (int, any) {
	if strings.HasSuffix(r.Path, "/conversations") && r.Method == http.MethodGet {
		return 200, map[string]any{"conversations": []map[string]any{
			{"id": general, "name": nil}, {"id": builds, "name": "builds"},
		}}
	}
	return 201, map[string]string{"id": "activity"}
}

// **Silent if it breaks: an app is held to its one channel.** The bot could post
// to any channel of an installed team; the request must name the attachment's
// channel and nothing the app wrote. A regression is a post in the wrong channel
// that every status reports as sent.
func TestASendGoesOnlyToTheAttachmentsChannel(t *testing.T) {
	seen := stub(t, channelsOf)
	_, err := Transport{}.Send(context.Background(), target(ChannelDestination(teamID, builds)),
		integrations.Send{Action: "post", Text: "post to " + general + " instead", AppName: "Reconciler"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0].Path != "/v3/conversations" {
		t.Fatalf("requests = %+v, want one POST /v3/conversations", *seen)
	}
	data, _ := (*seen)[0].Body["channelData"].(map[string]any)
	channel, _ := data["channel"].(map[string]any)
	if channel["id"] != builds {
		t.Errorf("posted to channel %v, want the attached %s", channel["id"], builds)
	}
	tenantData, _ := data["tenant"].(map[string]any)
	if tenantData["id"] != tenant {
		t.Errorf("tenant = %v", tenantData["id"])
	}
	if (*seen)[0].Auth != "Bearer bot-token" {
		t.Errorf("Authorization = %q", (*seen)[0].Auth)
	}
}

// **Silent if it breaks: an uninstalled team's send is failed, not sent, and is
// not retried**.
func TestARosterRefusalIsATerminalFailureThatSaysWhy(t *testing.T) {
	stub(t, func(request) (int, any) {
		return 403, map[string]any{"error": map[string]string{
			"code": "BotNotInConversationRoster", "message": "The bot is not part of the conversation roster."}}
	})
	_, err := Transport{}.Send(context.Background(), target(ChannelDestination(teamID, general)),
		integrations.Send{Action: "post", Text: "x"})
	if err == nil {
		t.Fatal("a send to a team the bot left succeeded")
	}
	if appint.IsRetryable(err) {
		t.Error("a roster refusal was marked retryable; it would be retried forever")
	}
	if !NotInstalled(err) || !strings.Contains(err.Error(), "add the Toft app") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
}

// Throttles and server errors can succeed later, and are retried.
func TestThrottlingIsRetryable(t *testing.T) {
	for _, status := range []int{429, 502} {
		stub(t, func(request) (int, any) { return status, map[string]any{} })
		_, err := Transport{}.Send(context.Background(), target(ChatDestination(chatID)),
			integrations.Send{Action: "post", Text: "x"})
		if !appint.IsRetryable(err) {
			t.Errorf("HTTP %d was not retryable: %v", status, err)
		}
	}
}

// **Silent if it breaks: the prefix decides the call** — a chat posts to
// its activities, a channel creates a conversation, and an unprefixed id posts
// nowhere rather than guessing.
func TestThePrefixDecidesWhichCallASendMakes(t *testing.T) {
	seen := stub(t, channelsOf)
	if _, err := (Transport{}).Send(context.Background(), target(ChatDestination(chatID)),
		integrations.Send{Action: "post", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := (*seen)[0].Path; got != "/v3/conversations/19:chat@thread.v2/activities" &&
		got != "/v3/conversations/19:chat%40thread.v2/activities" {
		t.Errorf("a chat posted to %s", got)
	}
	if _, err := (Transport{}).Send(context.Background(), target(builds),
		integrations.Send{Action: "post", Text: "x"}); err == nil {
		t.Error("an unprefixed destination was posted somewhere")
	}
	if len(*seen) != 1 {
		t.Errorf("an unprefixed destination made a request: %+v", (*seen)[1:])
	}
	if _, err := (Transport{}).Send(context.Background(), target(""),
		integrations.Send{Action: "post", Text: "x"}); err == nil {
		t.Error("a send with no destination succeeded")
	}
}

// The card is the webhook kind's card, attribution last.
func TestTheActivityIsTheSharedCardWithItsAttribution(t *testing.T) {
	seen := stub(t, channelsOf)
	_, _ = Transport{}.Send(context.Background(), target(ChannelDestination(teamID, general)),
		integrations.Send{Action: "post", Text: "Ingest finished", AppName: "Reconciler"})
	raw, _ := json.Marshal((*seen)[0].Body["activity"])
	if !strings.Contains(string(raw), "application/vnd.microsoft.card.adaptive") ||
		!strings.Contains(string(raw), "Sent by Reconciler on Toft") {
		t.Errorf("activity = %s", raw)
	}
}

// Destinations expands each pasted team live and lists pasted chats; a team the
// bot has left drops out of the menu instead of failing it.
func TestDestinationsExpandTeamsAndSkipOneTheBotLeft(t *testing.T) {
	gone := database.IntegrationPlace{Kind: database.PlaceTeam, ConversationID: "19:gone@thread.tacv2", Name: "Gone"}
	chat := database.IntegrationPlace{Kind: database.PlaceChat, ConversationID: chatID, Name: "Release chat"}
	stub(t, func(r request) (int, any) {
		if strings.Contains(r.Path, "gone") {
			return 403, map[string]any{"error": map[string]string{"code": "BotNotInConversationRoster"}}
		}
		return channelsOf(r)
	})
	dests, err := Transport{}.Destinations(context.Background(), target("", teamPlace(), gone, chat))
	if err != nil {
		t.Fatalf("Destinations: %v", err)
	}
	want := map[string]string{
		ChannelDestination(teamID, general): "Toft Test › General",
		ChannelDestination(teamID, builds):  "Toft Test › builds",
		ChatDestination(chatID):             "Release chat",
	}
	if len(dests) != len(want) {
		t.Fatalf("destinations = %+v", dests)
	}
	for _, d := range dests {
		if want[d.ID] != d.Label {
			t.Errorf("%s labelled %q, want %q", d.ID, d.Label, want[d.ID])
		}
	}
	if !strings.HasPrefix(ChannelDestination(teamID, builds), PlacePrefix(teamPlace())) ||
		!strings.HasPrefix(ChatDestination(chatID), PlacePrefix(chat)) {
		t.Error("a destination does not start with its place's prefix; PlaceInUse would miss it")
	}
}

// **Silent if it breaks: one connection is one Teams app.** A fresh manifest id
// per download is a second app in the org catalog that nobody asked for.
func TestTheManifestIDIsStablePerConnection(t *testing.T) {
	if ManifestID("iconn-a") != ManifestID("iconn-a") {
		t.Error("two downloads of one connection have different app ids")
	}
	if ManifestID("iconn-a") == ManifestID("iconn-b") {
		t.Error("two connections share an app id")
	}
	pkg, err := Package(PackageInfo{ConnectionID: "iconn-a", BotID: "bot-id", Name: "Build alerts",
		WebsiteURL: "https://toft.example"})
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]*zip.File{}
	for _, f := range z.File {
		names[f.Name] = f
	}
	for _, want := range []string{"manifest.json", "color.png", "outline.png"} {
		if names[want] == nil {
			t.Errorf("the package has no %s", want)
		}
	}
	rc, _ := names["manifest.json"].Open()
	var m struct {
		ID   string `json:"id"`
		Bots []struct {
			BotID  string   `json:"botId"`
			Scopes []string `json:"scopes"`
		} `json:"bots"`
	}
	_ = json.NewDecoder(rc).Decode(&m)
	if m.ID != ManifestID("iconn-a") || len(m.Bots) != 1 || m.Bots[0].BotID != "bot-id" {
		t.Errorf("manifest = %+v", m)
	}
	for _, s := range m.Bots[0].Scopes {
		if s == "personal" {
			t.Error("the bot is installable in personal chats; Toft posts to teams and chats only")
		}
	}
}

// The links people paste: both hosts, encoded or not; a channel's link and a
// foreign tenant refused by name.
func TestParseLink(t *testing.T) {
	good := map[string]Link{
		"https://teams.microsoft.com/l/team/19%3ADazR0ng%40thread.tacv2/conversations?groupId=g&tenantId=" + tenant: {database.PlaceTeam, "19:DazR0ng@thread.tacv2"},
		"https://teams.cloud.microsoft/l/team/19:abc@thread.tacv2/conversations":                                    {database.PlaceTeam, "19:abc@thread.tacv2"},
		"https://teams.microsoft.com/l/chat/19:meeting_x@thread.v2/conversations?context=%7B%7D":                    {database.PlaceChat, "19:meeting_x@thread.v2"},
	}
	for raw, want := range good {
		got, err := ParseLink(raw, tenant)
		if err != nil || got != want {
			t.Errorf("ParseLink(%q) = %+v, %v; want %+v", raw, got, err, want)
		}
	}
	bad := map[string]string{
		"https://teams.microsoft.com/l/channel/19%3Ac%40thread.tacv2/General?groupId=g": "Get link to team",
		"https://teams.microsoft.com/l/team/19%3Ac%40thread.tacv2/x?tenantId=other":     "another Microsoft 365 tenant",
		"https://evil.example/l/team/19%3Ac%40thread.tacv2/x":                           "paste a link from Teams",
		"http://teams.microsoft.com/l/team/19%3Ac%40thread.tacv2/x":                     "paste a link from Teams",
		"https://teams.microsoft.com/l/team/not-a-thread/x":                             "does not name",
		"19:c@thread.tacv2": "paste a link from Teams",
	}
	for raw, want := range bad {
		if _, err := ParseLink(raw, tenant); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseLink(%q) = %v, want an error naming %q", raw, err, want)
		}
	}
}

// A place is saved only after the connector answers for it: a team by its
// details and channel list, a chat by its members; a team the bot is not in is
// the roster refusal.
func TestVerifyPlace(t *testing.T) {
	stub(t, func(r request) (int, any) {
		switch {
		case strings.Contains(r.Path, "outsider"):
			return 403, map[string]any{"error": map[string]string{"code": "BotNotInConversationRoster"}}
		case strings.HasSuffix(r.Path, "/members"):
			return 200, []map[string]string{{"id": "29:x"}}
		case strings.HasSuffix(r.Path, "/conversations"):
			return channelsOf(r)
		default:
			return 200, map[string]string{"name": "Toft Test"}
		}
	})
	name, err := VerifyPlace(context.Background(), target(""), Link{database.PlaceTeam, teamID}, "")
	if err != nil || name != "Toft Test" {
		t.Errorf("team: %q, %v", name, err)
	}
	if _, err := VerifyPlace(context.Background(), target(""),
		Link{database.PlaceTeam, "19:outsider@thread.tacv2"}, ""); !NotInstalled(err) {
		t.Errorf("a team the bot is not in: %v", err)
	}
	if name, err := VerifyPlace(context.Background(), target(""),
		Link{database.PlaceChat, chatID}, "Release chat"); err != nil || name != "Release chat" {
		t.Errorf("chat: %q, %v", name, err)
	}
	if _, err := VerifyPlace(context.Background(), target(""), Link{database.PlaceChat, chatID}, ""); err == nil {
		t.Error("a chat with no name was accepted")
	}
}

// No error this package returns carries the secret or the bearer token.
func TestNoErrorCarriesTheCredential(t *testing.T) {
	stub(t, func(r request) (int, any) {
		return 401, map[string]any{"error": map[string]string{"code": "Unauthorized",
			"message": "Authorization has been denied for this request."}}
	})
	_, err := Transport{}.Send(context.Background(), target(ChatDestination(chatID)),
		integrations.Send{Action: "post", Text: "x"})
	if err == nil || strings.Contains(err.Error(), "SECRET-VALUE") || strings.Contains(err.Error(), "bot-token") {
		t.Errorf("err = %v", err)
	}
}

// Verify proves the credential with a token and labels the connection by the
// bot; a connection with no app id is refused before anything is asked.
func TestVerify(t *testing.T) {
	stub(t, channelsOf)
	label, err := Transport{}.Verify(context.Background(), target(""))
	if err != nil || label != "bot fb149f69" {
		t.Errorf("Verify = %q, %v", label, err)
	}
	empty := target("")
	empty.Connection.ScopeID = ""
	if _, err := (Transport{}).Verify(context.Background(), empty); err == nil {
		t.Error("a connection with no bot id verified")
	}
	noSecret := target("")
	noSecret.Credentials = map[string]string{}
	if _, err := (Transport{}).Verify(context.Background(), noSecret); err == nil {
		t.Error("a connection with no secret verified")
	}
}
