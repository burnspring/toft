package pagerduty

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
	old, oldClient := eventsEndpoint, httpClient
	eventsEndpoint, httpClient = server.URL, server.Client()
	t.Cleanup(func() {
		eventsEndpoint, httpClient = old, oldClient
		server.Close()
	})
}

func target() integrations.Target {
	return integrations.Target{
		Connection: database.IntegrationConnection{
			ID: "iconn-1", Kind: "pagerduty", Name: "Platform", ScopeLabel: "Platform on-call",
		},
		Credentials: map[string]string{"routing_key": strings.Repeat("k", 32)},
	}
}

// **An app can only resolve what it triggered**, and it is true by construction
// rather than by a check: the key is namespaced with the app id before it
// leaves, so the key another app's incident is under is not one this app can
// name. There is nothing here that refuses a cross-app resolve, because there
// is no cross-app resolve to refuse.
func TestTheDedupKeyIsNamespacedWithTheApp(t *testing.T) {
	var keys []string
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var event map[string]any
		_ = json.Unmarshal(body, &event)
		keys = append(keys, event["dedup_key"].(string))
		w.WriteHeader(http.StatusAccepted)
	})

	tr := New()
	for _, appID := range []string{"app-one", "app-two"} {
		if _, err := tr.Send(context.Background(), target(), integrations.Send{
			Action: "trigger", Text: "Ingest is late", DedupKey: "ingest-late",
			AppID: appID, AppName: "Reconciler",
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if keys[0] == keys[1] {
		t.Fatalf("two apps sending the same key produced one incident key: %q", keys[0])
	}
	for i, key := range keys {
		if !strings.HasPrefix(key, []string{"app-one:", "app-two:"}[i]) {
			t.Errorf("key %q is not namespaced with the app id", key)
		}
	}
}

// The id and not the name: a name is a label somebody can change, and renaming
// an app must not orphan the incident it opened this morning.
func TestTheNamespaceIsTheIDNotTheName(t *testing.T) {
	before := DedupKey(target(), integrations.Send{
		DedupKey: "k", AppID: "app-1", AppName: "Reconciler"})
	after := DedupKey(target(), integrations.Send{
		DedupKey: "k", AppID: "app-1", AppName: "Reconciliation Service"})
	if before != after {
		t.Errorf("renaming the app changed the key: %q then %q", before, after)
	}
}

// A resolve carries the key and nothing else — no payload, because there is no
// incident to describe, only one to close.
func TestResolveSendsOnlyTheKey(t *testing.T) {
	var event map[string]any
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &event)
		w.WriteHeader(http.StatusAccepted)
	})

	if _, err := New().Send(context.Background(), target(), integrations.Send{
		Action: "resolve", DedupKey: "ingest-late", AppID: "app-1", Text: "recovered",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if event["event_action"] != "resolve" {
		t.Errorf("event_action = %v", event["event_action"])
	}
	if _, ok := event["payload"]; ok {
		t.Error("a resolve carried a payload — there is no incident to describe, only one to close")
	}
}

// A severity nobody recognises becomes `error` rather than failing the page.
// An app that says "urgent" means something bad, and the worst possible moment
// to be pedantic about a vocabulary is the one where somebody should be woken.
func TestAnUnknownSeverityStillPages(t *testing.T) {
	var event map[string]any
	stub(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &event)
		w.WriteHeader(http.StatusAccepted)
	})
	if _, err := New().Send(context.Background(), target(), integrations.Send{
		Action: "trigger", Text: "down", Severity: "URGENT", DedupKey: "k", AppID: "a",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	payload := event["payload"].(map[string]any)
	if payload["severity"] != "error" {
		t.Errorf("severity = %v, want error", payload["severity"])
	}
}

// A send with no key is refused rather than opening an incident nothing can
// ever close.
func TestATriggerWithNoKeyIsRefused(t *testing.T) {
	_, err := New().Send(context.Background(), target(), integrations.Send{
		Action: "trigger", Text: "down", AppID: "a",
	})
	if err == nil {
		t.Fatal("an incident was opened with no key to close it by")
	}
}

// A rate limit could succeed on a second attempt; a rejected key could not.
func TestOnlyThrottlesAndServerErrorsAreRetryable(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadRequest, false},
	} {
		stub(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"message":"Event object is invalid","errors":["routing_key"]}`))
		})
		_, err := New().Send(context.Background(), target(), integrations.Send{
			Action: "trigger", Text: "down", DedupKey: "k", AppID: "a",
		})
		if err == nil {
			t.Fatalf("%d was treated as success", tc.status)
		}
		if got := appint.IsRetryable(err); got != tc.retry {
			t.Errorf("%d retryable = %v, want %v", tc.status, got, tc.retry)
		}
		if !strings.Contains(err.Error(), "routing_key") {
			t.Errorf("%d lost PagerDuty's own words: %v", tc.status, err)
		}
	}
}
