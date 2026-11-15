package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A refusal the install says will not change ends the watch after one request:
// a 400 used to be retried for ever, in silence (security review 2026-11-14,
// CS-4).
func TestWatchProgressEndsOnARefusalThatWillNotChange(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"no","code":"bad_request","retryable":false}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "test")
	_, err := c.WatchProgress(context.Background(), "a", "d", func(ProgressEvent) {})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("want the 400, got %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("asked %d times, want 1", n)
	}
}

// A terminal frame carrying a field of a type this client does not expect — a
// newer install's — is still the terminal frame.
func TestWatchProgressKeepsAFrameWithAnUnexpectedFieldType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d\",\"status\":\"success\","+
			"\"phase\":\"deploy\",\"step\":\"flip\",\"warnings\":{\"new\":\"shape\"}}\n\n")
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "test")
	done := make(chan struct{})
	var final ProgressEvent
	var err error
	go func() {
		final, err = c.WatchProgress(context.Background(), "a", "d", func(ProgressEvent) {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the terminal frame was dropped and the watch reconnected")
	}
	if err != nil || final.Status != StatusSuccess {
		t.Fatalf("final %+v, err %v; want the success frame", final, err)
	}
}

// An install that cannot be reached ends the watch unreachable once the
// give-up span has passed with no stream opened.
func TestWatchProgressGivesUpOnAnInstallThatIsGone(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	defer func(d time.Duration) { watchGiveUp = d }(watchGiveUp)
	watchGiveUp = 10 * time.Millisecond

	c := New(url, "k", "test")
	done := make(chan error, 1)
	go func() {
		_, err := c.WatchProgress(context.Background(), "a", "d", func(ProgressEvent) {})
		done <- err
	}()
	select {
	case err := <-done:
		var unreachable *UnreachableError
		if !errors.As(err, &unreachable) {
			t.Fatalf("want an UnreachableError, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watch kept retrying an install that is gone")
	}
}

// A stream that worked and dropped is said once to the caller, and the watch
// carries on to the terminal frame.
func TestWatchProgressSaysWhenAStreamDrops(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d\",\"status\":\"building\"}\n\n")
			return // the stream closes with no terminal frame
		}
		fmt.Fprint(w, "event: progress\ndata: {\"deployment_id\":\"d\",\"status\":\"success\"}\n\n")
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "test")
	var said atomic.Int32
	c.OnReconnect = func(error) { said.Add(1) }
	final, err := c.WatchProgress(context.Background(), "a", "d", func(ProgressEvent) {})
	if err != nil || final.Status != StatusSuccess {
		t.Fatalf("final %+v, err %v", final, err)
	}
	if n := said.Load(); n != 1 {
		t.Errorf("OnReconnect called %d times, want 1", n)
	}
}

// A frame longer than the client reads would come back on every reconnect; the
// watch ends, naming where the outcome is.
func TestWatchProgressEndsOnAFrameTooLong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: progress\ndata: {\"message\":\""+strings.Repeat("x", 2<<20)+"\"}\n\n")
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "test")
	_, err := c.WatchProgress(context.Background(), "a", "d", func(ProgressEvent) {})
	var tooLong *FrameTooLongError
	if !errors.As(err, &tooLong) || !strings.Contains(err.Error(), "toft deployments") {
		t.Fatalf("want a FrameTooLongError naming toft deployments, got %v", err)
	}
}
