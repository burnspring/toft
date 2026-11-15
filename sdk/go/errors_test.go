package toft

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

// Whether to try again is the platform's call. Every error body it writes
// carries `retryable`, because only the handler knows that a 503 means "this
// install is configured without it" or that a 409 means "wait" — and the status
// rule is the fallback for answers that never reached a handler.
func TestTheBodysRetryableWinsOverTheStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   map[string]any
		want   bool
	}{
		{"a 503 the platform calls permanent", 503, map[string]any{"code": "jobs_unavailable", "retryable": false}, false},
		{"a 409 the platform calls a wait", 409, map[string]any{"code": "queue_full", "retryable": true}, true},
		{"an older install's 503", 503, map[string]any{"error": "x"}, true},
		{"an older install's 409", 409, map[string]any{"error": "x"}, false},
		{"a 429 with no body", 429, map[string]any{}, true},
		{"a transport failure", 0, nil, true},
		{"a retryable that is not a bool", 400, map[string]any{"retryable": "yes"}, false},
	}
	for _, c := range cases {
		e := &Error{Status: c.status, Body: c.body}
		if got := e.Retryable(); got != c.want {
			t.Errorf("%s: Retryable() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHintIsThePlatformsNextStep(t *testing.T) {
	e := &Error{Status: 401, Body: map[string]any{"code": "key_expired", "hint": "mint a new key"}}
	if e.Hint() != "mint a new key" || e.Code() != "key_expired" {
		t.Errorf("Hint() = %q, Code() = %q", e.Hint(), e.Code())
	}
	if (&Error{Status: 404, Body: map[string]any{}}).Hint() != "" {
		t.Error("an absent hint should read as empty")
	}
	var none *Error
	if none.Hint() != "" {
		t.Error("a nil Error's hint should read as empty")
	}
}

// The built-in retry is for an answer that never reached a handler. The
// platform can veto it — a 503 it calls permanent is not asked twice — but
// cannot widen it to a 4xx, whose wait is the caller's to choose.
func TestTheBuiltInRetryHonoursThePlatformsVeto(t *testing.T) {
	var calls atomic.Int32
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(t, w, 503, map[string]any{"error": "not configured", "code": "unavailable",
			"retryable": false, "hint": "an administrator configures it"})
	})
	_, err := App.Self(context.Background())
	e, ok := AsError(err)
	if !ok || e.Retryable() || e.Hint() != "an administrator configures it" {
		t.Fatalf("err = %#v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d requests, want 1 — the platform said a retry cannot help", n)
	}

	calls.Store(0)
	platform(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(t, w, 409, map[string]any{"error": "wait", "code": "busy", "retryable": true})
	})
	_, err = App.Self(context.Background())
	if e, ok := AsError(err); !ok || !e.Retryable() {
		t.Fatalf("err = %#v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d requests, want 1 — a retryable 4xx is the caller's to retry", n)
	}
}
