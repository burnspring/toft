package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/integrations"
)

func stub(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(h)
	old, oldClient := apiBase, httpClient
	apiBase, httpClient = server.URL, server.Client()
	t.Cleanup(func() {
		apiBase, httpClient = old, oldClient
		server.Close()
	})
}

func target(channel string) integrations.Target {
	return integrations.Target{
		Connection: database.IntegrationConnection{
			ID: "iconn-1", Kind: "discord", Name: "Engineering", ScopeID: "GUILD",
		},
		Credentials: map[string]string{"bot_token": "tok"},
		Destination: database.Destination{ID: channel, Label: "builds"},
	}
}

// The attribution is a footer on an embed, which is somewhere the app's own
// text cannot reach: the app supplies a string, this side builds the structure,
// and there is no markdown that deletes a footer.
func TestEveryMessageCarriesAnAttributionTheAppCannotSuppress(t *testing.T) {
	var payload struct {
		Embeds []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Footer      struct {
				Text string `json:"text"`
			} `json:"footer"`
		} `json:"embeds"`
	}
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/channels/C1/messages") {
			t.Errorf("posted to %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bot tok" {
			t.Errorf("authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusOK)
	})

	_, err := New().Send(context.Background(), target("C1"), integrations.Send{
		Action: "post", Text: "**Ingest finished**", AppName: "Reconciler",
		AppURL: "https://toft.example/apps/reconciler",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Embeds[0].Description != "**Ingest finished**" {
		t.Errorf("the app's own text was rewritten: %q", payload.Embeds[0].Description)
	}
	if !strings.Contains(payload.Embeds[0].Footer.Text, "Reconciler") {
		t.Errorf("no attribution: %q", payload.Embeds[0].Footer.Text)
	}
}

// The picker lists text channels. A voice channel, a category or a forum is not
// somewhere a message goes, and offering one is a menu entry that always fails.
func TestDestinationsListsOnlyChannelsAMessageCanReach(t *testing.T) {
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"C1","name":"builds","type":0},
			{"id":"V1","name":"standup","type":2},
			{"id":"A1","name":"announcements","type":5},
			{"id":"K1","name":"Engineering","type":4},
			{"id":"F1","name":"help","type":15}
		]`))
	})
	got, err := New().Destinations(context.Background(), target(""))
	if err != nil {
		t.Fatalf("Destinations: %v", err)
	}
	if len(got) != 2 || got[0].Label != "builds" || got[1].Label != "announcements" {
		t.Errorf("destinations = %+v", got)
	}
}

// A rate limit could succeed on a second attempt; "missing access" could not,
// and retrying it only doubles the log noise for a problem a person must fix.
func TestOnlyThrottlesAndServerErrorsAreRetryable(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
	} {
		stub(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"message":"Missing Access","code":50001}`))
		})
		_, err := New().Send(context.Background(), target("C1"),
			integrations.Send{Action: "post", Text: "x", AppName: "a"})
		if err == nil {
			t.Fatalf("%d was treated as success", tc.status)
		}
		if got := appint.IsRetryable(err); got != tc.retry {
			t.Errorf("%d retryable = %v, want %v", tc.status, got, tc.retry)
		}
		if !strings.Contains(err.Error(), "Missing Access") {
			t.Errorf("%d lost Discord's own words: %v", tc.status, err)
		}
	}
}

// Saving names the half an administrator most often has not done: adding the
// bot to the server.
func TestVerifyNamesTheMissingStep(t *testing.T) {
	stub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Missing Access"}`))
	})
	_, err := New().Verify(context.Background(), target(""))
	if err == nil {
		t.Fatal("a server the bot is not in was accepted")
	}
	if !strings.Contains(err.Error(), "not been added") {
		t.Errorf("the refusal does not name the missing step: %v", err)
	}
}

// A platform notification's link syntax is escaped, so a masked link composed
// from an app name or a log line does not render (RT-10).
func TestAPlatformEmbedCarriesNoMaskedLink(t *testing.T) {
	var payload struct {
		Embeds []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"embeds"`
	}
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.WriteHeader(http.StatusOK)
	})
	_, err := New().Send(context.Background(), target("C1"), integrations.Send{
		Action: "post", Title: "[Payroll](https://evil.example)",
		Text:    "failed: [Sign in again](https://evil.example) <@&123>",
		AppName: "Toft", FromPlatform: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := payload.Embeds[0]
	for _, s := range []string{e.Title, e.Description} {
		if strings.Contains(s, "[Payroll](") || strings.Contains(s, "[Sign in again](") || strings.Contains(s, " <@") {
			t.Errorf("live markdown survived: %q", s)
		}
	}
	if !strings.Contains(e.Description, `\[Sign in again\]`) || !strings.Contains(e.Description, `\<@&123>`) {
		t.Errorf("description = %q, want the link and mention syntax escaped", e.Description)
	}
}
