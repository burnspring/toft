package toft

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// BB-002: a 5xx is an answer from something that may have done the work, so a
// write without an Idempotency-Key is not sent again. The error stays uncoded
// and retryable — the caller may choose to try again; the SDK does not do it
// behind their back.
func TestAFiveHundredIsNotResentForAWriteWithoutAKey(t *testing.T) {
	calls := map[string]func() error{
		"Jobs.Start": func() error {
			_, err := Jobs.Start(context.Background(), "echo", StartOptions{})
			return err
		},
		"Data.Set": func() error {
			_, err := Data.Set(context.Background(), map[string]any{"a": 1})
			return err
		},
		"Notify.Slack": func() error {
			_, err := Notify.Slack(context.Background(), "x")
			return err
		},
	}
	for name, call := range calls {
		var n atomic.Int32
		platform(t, func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.Header().Set("Retry-After", "0")
			writeJSON(t, w, 500, map[string]any{"error": "boom"})
		})
		err := call()
		e, ok := AsError(err)
		if !ok || e.Status != 500 || e.Code() != "" || !e.Retryable() {
			t.Errorf("%s: err = %#v, want status 500, uncoded, retryable", name, err)
		}
		if got := n.Load(); got != 1 {
			t.Errorf("%s: %d requests, want 1 — the handler may already have acted", name, got)
		}
	}
}

// A GET and a keyed write are harmless to deliver twice, so a 5xx is resent
// once for them.
func TestAFiveHundredIsResentForAReadAndAKeyedWrite(t *testing.T) {
	var n atomic.Int32
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			writeJSON(t, w, 502, map[string]any{"error": "gateway"})
			return
		}
		writeJSON(t, w, 202, map[string]any{"run_id": "run_1", "status": "queued"})
	})
	if _, err := Jobs.Start(context.Background(), "echo", StartOptions{IdempotencyKey: "k1"}); err != nil {
		t.Fatalf("keyed Start: %v", err)
	}
	if got := n.Load(); got != 2 {
		t.Errorf("keyed Start: %d requests, want 2", got)
	}
}

// A 429 is the rate limiter, which answers before any handler: resent once
// whatever the method.
func TestARateLimitIsResentForAnUnkeyedWrite(t *testing.T) {
	var n atomic.Int32
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			writeJSON(t, w, 429, map[string]any{"error": "slow down", "code": "rate_limited", "retryable": true})
			return
		}
		writeJSON(t, w, 200, map[string]any{"version": 1, "data": map[string]any{"a": 1}})
	})
	if _, err := Data.Set(context.Background(), map[string]any{"a": 1}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := n.Load(); got != 2 {
		t.Errorf("%d requests, want 2", got)
	}
}

// TOFT-BB-002: a burst past the limiter is paced, not refused — a 429 is
// waited out for as long as the call's bound allows, not resent once.
func TestARateLimitIsWaitedOutNotResentOnce(t *testing.T) {
	var n atomic.Int32
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 3 {
			w.Header().Set("Retry-After", "0")
			writeJSON(t, w, 429, map[string]any{"error": "slow down", "code": "rate_limited", "retryable": true})
			return
		}
		writeJSON(t, w, 200, map[string]any{"version": 1, "data": map[string]any{"a": 1}})
	})
	if _, err := Data.Get(context.Background()); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := n.Load(); got != 4 {
		t.Errorf("%d requests, want 4", got)
	}
}

// A 5xx after a 429's resend is not waited out: only the limiter is.
func TestARateLimitThenAServerErrorIsReturned(t *testing.T) {
	var n atomic.Int32
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			writeJSON(t, w, 429, map[string]any{"error": "slow down", "code": "rate_limited", "retryable": true})
			return
		}
		writeJSON(t, w, 500, map[string]any{"error": "boom"})
	})
	if _, err := Data.Set(context.Background(), map[string]any{"a": 1}); err == nil {
		t.Fatal("Set succeeded; want the 500")
	}
	if got := n.Load(); got != 2 {
		t.Errorf("%d requests, want 2", got)
	}
}

// BB-003: a timeout is not resent, and the whole call is bounded by the one
// timeout rather than one per attempt.
func TestATimeoutIsNotResentAndBoundsTheWholeCall(t *testing.T) {
	saved := defaultTimeout
	defaultTimeout = 200 * time.Millisecond
	t.Cleanup(func() { defaultTimeout = saved })

	var n atomic.Int32
	release := make(chan struct{})
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		<-release
	})
	// Registered after the server's Close, so it runs first and frees the
	// handlers Close waits on.
	t.Cleanup(func() { close(release) })
	for name, call := range map[string]func() error{
		"Jobs.Start": func() error { _, err := Jobs.Start(context.Background(), "echo", StartOptions{}); return err },
		"App.Self":   func() error { _, err := App.Self(context.Background()); return err },
	} {
		n.Store(0)
		start := time.Now()
		err := call()
		elapsed := time.Since(start)
		e, ok := AsError(err)
		if !ok || e.Code() != "" || !e.Retryable() {
			t.Errorf("%s: err = %#v, want an uncoded retryable timeout", name, err)
		}
		if elapsed > time.Second {
			t.Errorf("%s: took %s, want about the one 200ms bound", name, elapsed)
		}
		if got := n.Load(); got != 1 {
			t.Errorf("%s: %d requests, want 1 — the handler may have received it", name, got)
		}
	}
}

// A connection that never opened delivered nothing, so even an unkeyed write
// is tried once more.
func TestARefusedConnectionIsResentForAnyMethod(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens: every dial is refused

	SetTransport("http://"+addr, "test-token")
	t.Cleanup(func() { SetTransport("", "") })
	saved := defaultTimeout
	defaultTimeout = 5 * time.Second
	t.Cleanup(func() { defaultTimeout = saved })

	tr, _ := client()
	_, got, err := tr.once(context.Background(), http.MethodPost, "/x", []byte("{}"), "application/json", nil, time.Second)
	if err == nil || got.headers {
		t.Fatalf("once: sent=%v err=%v, want an unsent failure", got.headers, err)
	}
	e, _ := AsError(err)
	if !resendable(http.MethodPost, nil, got.headers, e) {
		t.Error("a refused connection was not resent for a POST")
	}
	if resendable(http.MethodPost, nil, true, e) {
		t.Error("a connection lost after sending was resent for an unkeyed POST")
	}
	if !resendable(http.MethodGet, nil, true, e) {
		t.Error("a GET lost after sending was not resent")
	}
}

// BB-014: a 200 that is a web page is not Toft's API. Coded not_toft, not
// retryable, with a hint — never an uncoded parse error.
func TestASuccessThatIsAWebPageIsNotToft(t *testing.T) {
	var n atomic.Int32
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!doctype html><html><body>Sign in</body></html>"))
	})
	_, err := App.Self(context.Background())
	e, ok := AsError(err)
	if !ok || e.Code() != "not_toft" || e.Retryable() || e.Hint() == "" || e.Status != 200 {
		t.Fatalf("err = %#v, want not_toft, not retryable, with a hint, status 200", err)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("%d requests, want 1", got)
	}
}
