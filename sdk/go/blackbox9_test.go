package toft

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// BB-004: a reused connection the platform closes without answering is
// resent exactly once. net/http resends a GET or a keyed write on its own
// there, and the SDK's resend on top of it made three requests.
func TestADroppedReusedConnectionIsResentExactlyOnce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int32
	var mute atomic.Bool
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					if _, err := http.ReadRequest(br); err != nil {
						return
					}
					if mute.Load() {
						n.Add(1)
						return // accepted, read, closed: no answer
					}
					c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"))
				}
			}(c)
		}
	}()
	SetTransport("http://"+ln.Addr().String(), "test-token")
	t.Cleanup(func() { SetTransport("", "") })

	for name, call := range map[string]func() error{
		"Data.Get": func() error { _, err := Data.Get(context.Background()); return err },
		"Jobs.Start": func() error {
			_, err := Jobs.Start(context.Background(), "echo", StartOptions{IdempotencyKey: "k9"})
			return err
		},
	} {
		mute.Store(false)
		if _, err := Data.Get(context.Background()); err != nil { // leaves a keep-alive connection idle
			t.Fatalf("warm-up: %v", err)
		}
		mute.Store(true)
		n.Store(0)
		if err := call(); err == nil {
			t.Errorf("%s: no error from a platform that never answered", name)
		}
		if got := n.Load(); got != 2 {
			t.Errorf("%s: %d requests, want 2 — the request and one resend", name, got)
		}
	}
}

// BB-005: Update given a nil function is a coded refusal, not a panic.
func TestUpdateWithANilFunctionIsBadRequest(t *testing.T) {
	SetTransport("", "")
	_, err := Data.Update(context.Background(), nil)
	e, ok := AsError(err)
	if !ok || e.Code() != "bad_request" || e.Retryable() || e.Hint() == "" {
		t.Fatalf("err = %#v, want bad_request with a hint", err)
	}
}

// BB-006: a 200 with an empty body is not Toft's API — every app route answers
// a body, and the ones with nothing to say answer 204.
func TestAnEmptySuccessIsNotToft(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
	})
	for name, call := range map[string]func() error{
		"Jobs.Start": func() error { _, err := Jobs.Start(context.Background(), "echo", StartOptions{}); return err },
		"Data.Get":   func() error { _, err := Data.Get(context.Background()); return err },
		"App.Self":   func() error { _, err := App.Self(context.Background()); return err },
	} {
		err := call()
		e, ok := AsError(err)
		if !ok || e.Code() != "not_toft" || e.Retryable() || e.Hint() == "" || e.Status != 200 {
			t.Errorf("%s: err = %#v, want not_toft, status 200, with a hint", name, err)
		}
	}
}

// A 204 is still an answer: no viewer.
func TestANoContentViewerIsStillNobody(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	got, err := fetchIdentity(context.Background(), "blob")
	if err != nil || got != nil {
		t.Fatalf("fetchIdentity = %v, %v; want nil, nil", got, err)
	}
}

// BB-008: AccessToken's unknown_capability hint names kinds, which is what it
// takes — the platform's names capabilities, and following that asked for
// "sharepoint.token.token".
func TestAccessTokenUnknownKindHintNamesKinds(t *testing.T) {
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, 400, map[string]any{
			"error": `"bogus.token" is not a capability this platform offers`, "code": "unknown_capability",
			"retryable": false,
			"hint": "name one of the capabilities GET /api/v1/app/self lists for this app — the integration, " +
				"a dot, the action: slack.post, email.send, sharepoint.upload; a file integration's access token is " +
				"sharepoint.token, onedrive.token, gdrive.token",
		})
	})
	_, err := AccessToken(context.Background(), "bogus")
	e, ok := AsError(err)
	if !ok || e.Code() != "unknown_capability" || e.Status != 400 {
		t.Fatalf("err = %#v, want unknown_capability 400", err)
	}
	hint := e.Hint()
	for _, want := range []string{`"sharepoint"`, `"onedrive"`, `"gdrive"`} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q does not name the kind %s", hint, want)
		}
	}
	if strings.Contains(hint, "onedrive.token") {
		t.Errorf("hint %q still names capabilities", hint)
	}
}
