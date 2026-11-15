package webhook

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/integrations"
	"toft/internal/safefetch"
)

func target(url, secret string) integrations.Target {
	return integrations.Target{
		Connection: database.IntegrationConnection{
			ID: "iconn-1", Kind: "webhook", Name: "ops-relay", ScopeID: url,
		},
		Credentials: map[string]string{"signing_secret": secret},
	}
}

func message() integrations.Send {
	return integrations.Send{
		Action: "post", Text: "Ingest finished: 1,204 rows",
		AppName: "Reconciler", AppURL: "https://toft.example/apps/reconciler",
		SendID: "send-1",
	}
}

// A receiver can verify the request, and the algorithm the documentation
// describes is the algorithm that is sent.
//
// The signature is over the **exact bytes**, which is why this test reads the
// body once and verifies against those bytes rather than re-marshalling the
// decoded payload: a receiver that re-serializes before verifying fails, and
// that is stated in the docs rather than worked around, because the alternative
// is canonicalization — a second specification for both sides to disagree about.
func TestAReceiverCanVerifyTheSignature(t *testing.T) {
	const secret = "whsec-abc"
	var gotBody []byte
	var gotHeader, gotEvent string

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader = r.Header.Get(SignatureHeader)
		gotEvent = r.Header.Get(EventHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	tr := Transport{client: testClient(server)}
	if _, err := tr.Send(context.Background(), target(server.URL, secret), message()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if err := Verify(secret, gotHeader, gotBody, time.Now()); err != nil {
		t.Errorf("a receiver following the documented algorithm could not verify it: %v", err)
	}
	if gotEvent != "webhook.post" {
		t.Errorf("event header = %q, want webhook.post — a receiver routes on it without parsing", gotEvent)
	}

	var payload Payload
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	// The app is named in the payload and the app cannot suppress it: an app
	// that could post indistinguishably from the platform is one that can
	// impersonate it.
	if payload.App.Name != "Reconciler" || payload.SendID != "send-1" {
		t.Errorf("the payload lost its attribution: %+v", payload)
	}
}

// A wrong secret, a tampered body and a stale timestamp all fail. The last is
// the reason the timestamp is inside the MAC rather than beside it: an attacker
// replaying yesterday's request cannot move the clock forward without
// invalidating the signature.
func TestVerifyRefusesTheThreeWaysItShould(t *testing.T) {
	body := []byte(`{"text":"hello"}`)
	now := time.Now()
	header := Sign("right", now, body)

	if err := Verify("right", header, body, now); err != nil {
		t.Fatalf("the honest case failed: %v", err)
	}
	if err := Verify("wrong", header, body, now); err == nil {
		t.Error("a signature under a different secret verified")
	}
	if err := Verify("right", header, []byte(`{"text":"goodbye"}`), now); err == nil {
		t.Error("a tampered body verified")
	}
	if err := Verify("right", header, body, now.Add(2*ToleranceWindow)); err == nil {
		t.Error("a signature outside the tolerance window verified")
	}
	if err := Verify("right", "nonsense", body, now); err == nil {
		t.Error("a malformed header verified")
	}
}

// **A private address is refused at save, not only at dial.** An operator whose
// receiver is on 10.x learns it at the form rather than at 2am inside a queue
// worker, and there is no bypass flag on either side.
func TestVerifyRefusesAPrivateAddress(t *testing.T) {
	tr := New()
	for _, raw := range []string{
		"https://localhost/hook",
		"https://127.0.0.1/hook",
		"https://[::1]/hook",
	} {
		if _, err := tr.Verify(context.Background(), target(raw, "s")); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
	// And the cheap half, which is the same rule at a different layer.
	for _, raw := range []string{
		"http://example.com/hook",
		"https://user:pass@example.com/hook",
		"not a url at all",
		"",
	} {
		if _, err := tr.Verify(context.Background(), target(raw, "s")); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
}

// The dial guard is still in force at send time — a name that resolved publicly
// when it was saved and resolves inward now is exactly the case save-time
// checking cannot cover.
func TestSendRefusesAPrivateAddressAtDial(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	// The same server, reached through a client with the guard on: httptest
	// listens on loopback, which is precisely what the guard refuses.
	guarded := Transport{client: safefetch.NewForTesting("", &tls.Config{InsecureSkipVerify: true}, true, Timeout)}
	_, err := guarded.Send(context.Background(), target(server.URL, "s"), message())
	if err == nil {
		t.Fatal("a loopback receiver was dialled")
	}
	if appint.IsRetryable(err) {
		t.Error("a refused address was marked retryable — that is a loop against the metadata service")
	}
}

// A 5xx might not repeat and is retried once; a 4xx says the message was wrong,
// and the message is still wrong on the second attempt.
func TestOnlyServerFailuresAreRetryable(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusTooManyRequests, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
	} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "the receiver's own words", tc.status)
		}))
		tr := Transport{client: testClient(server)}
		_, err := tr.Send(context.Background(), target(server.URL, "s"), message())
		server.Close()

		if err == nil {
			t.Fatalf("%d was treated as success", tc.status)
		}
		if got := appint.IsRetryable(err); got != tc.retry {
			t.Errorf("%d retryable = %v, want %v", tc.status, got, tc.retry)
		}
		// The far side's own words reach the ledger, which is the sentence
		// somebody debugging this actually needs.
		if !strings.Contains(err.Error(), "the receiver's own words") {
			t.Errorf("%d lost the receiver's message: %v", tc.status, err)
		}
	}
}

// testClient reaches an httptest server: its certificate is trusted by nothing
// and its address is on loopback, so the guard is the one rule switched off.
// The redirect cap, the size limits and the timeouts all stay in force — and
// TestSendRefusesAPrivateAddressAtDial is the test with the guard on.
func testClient(server *httptest.Server) *safefetch.Client {
	return safefetch.NewForTesting("", &tls.Config{InsecureSkipVerify: true}, false, Timeout)
}
