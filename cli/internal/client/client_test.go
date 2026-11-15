package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A proxy's own 503 page while the install redeploys is the install not
// answering — exit 4 — and its HTML is never the message.
func TestAProxysErrorPageIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("<html><head><title>503 Service Temporarily Unavailable</title></head></html>"))
	}))
	defer srv.Close()

	err := New(srv.URL, "", "test").Do(http.MethodGet, "/api/v1/me", nil, nil)
	var u *UnreachableError
	if !errors.As(err, &u) {
		t.Fatalf("got %T %v, want *UnreachableError", err, err)
	}
	if strings.Contains(err.Error(), "<html") || !strings.Contains(err.Error(), "503") {
		t.Errorf("message = %q, want the status and no HTML", err.Error())
	}
	if !u.ShouldRetry() {
		t.Error("a restarting install is worth retrying")
	}
}

// The install's own 503, in its envelope, is still its refusal.
func TestTheInstallsOwn503StaysARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"this install cannot run jobs","code":"jobs_unavailable"}`))
	}))
	defer srv.Close()

	err := New(srv.URL, "", "test").Do(http.MethodGet, "/api/v1/x", nil, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "jobs_unavailable" {
		t.Fatalf("got %T %v, want the install's refusal", err, err)
	}
}

// Every request carries the run's context: a cancelled run ends an in-flight
// Do and Upload at once, where they used to run to completion and the install
// started the deploy the person had just tried to abort (code review
// 2026-11-11, L-1).
func TestACancelledContextEndsDoAndUpload(t *testing.T) {
	started := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		// Slow, not blocked on the client's context: the server learns of a
		// hang-up only on its next read, and the point here is the client's
		// side of the cut.
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()
	defer srv.CloseClientConnections()
	ctx, cancel := context.WithCancel(context.Background())
	c := New(srv.URL, "", "test")
	c.Context = ctx
	go func() { <-started; cancel() }()
	done := make(chan error, 1)
	go func() { done <- c.Do(http.MethodGet, "/api/v1/me", nil, nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Do returned nil after the context was cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Do did not return after the context was cancelled")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	c.Context = ctx2
	go func() { <-started; cancel2() }()
	go func() {
		done <- c.Upload("/api/v1/apps/upload", "a.zip", strings.NewReader("zip"), 3, nil, nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Upload returned nil after the context was cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Upload did not return after the context was cancelled")
	}
}

// The credential follows a redirect only within the install's origin: another
// port of the same host is another origin (security review 2026-11-14, CS-8).
func TestACredentialDoesNotLeaveTheOrigin(t *testing.T) {
	for _, cookie := range []bool{false, true} {
		var got http.Header
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			w.Write([]byte(`{}`))
		}))
		var within http.Header
		var install *httptest.Server
		install = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/x":
				http.Redirect(w, r, other.URL+"/landed", http.StatusFound)
			case "/api/v1/here":
				http.Redirect(w, r, "/api/v1/landed", http.StatusFound)
			case "/api/v1/landed":
				within = r.Header.Clone()
				w.Write([]byte(`{}`))
			}
		}))

		c := New(install.URL, "toftu_secret", "test")
		c.Cookie = cookie
		if err := c.Do(http.MethodGet, "/api/v1/x", nil, nil); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if got == nil {
			t.Fatal("the redirect was not followed")
		}
		if got.Get("Authorization") != "" || got.Get("Cookie") != "" {
			t.Errorf("cookie=%v: the credential reached another port: %v", cookie, got)
		}
		if err := c.Do(http.MethodGet, "/api/v1/here", nil, nil); err != nil {
			t.Fatalf("Do: %v", err)
		}
		if within.Get("Authorization") == "" && within.Get("Cookie") == "" {
			t.Errorf("cookie=%v: a redirect within the install lost the credential", cookie)
		}
		install.Close()
		other.Close()
	}
}

func TestCredentialMayFollowTheInstallsOwnUpgrade(t *testing.T) {
	parse := func(s string) *url.URL { u, _ := url.Parse(s); return u }
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"https://toft.example", "https://toft.example/x", true},
		{"http://toft.example", "https://toft.example/x", true},
		{"http://toft.example:80", "https://toft.example:443/x", true},
		{"https://toft.example", "http://toft.example/x", false},
		{"https://toft.example", "https://toft.example:8443/x", false},
		{"https://toft.example", "https://apps.toft.example/x", false},
		{"http://toft.example:8080", "https://toft.example/x", false},
	} {
		if got := credentialMayFollow(parse(c.from), parse(c.to)); got != c.want {
			t.Errorf("%s -> %s: %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

// Under --session the build-log follower and the progress stream share one
// client, and a renewal on either answer writes the token the other reads
// (security review 2026-11-14, CS-12). Run under -race.
func TestRenewalIsSafeAcrossGoroutines(t *testing.T) {
	var n atomic.Int64
	issued := sync.Map{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := fmt.Sprintf("session-%d", n.Add(1))
		issued.Store(v, true)
		http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: v, MaxAge: 60})
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "session-0", "test")
	c.Cookie = true
	var renewed atomic.Int64
	c.OnRenew = func(string, time.Time) { renewed.Add(1) }
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Do(http.MethodGet, "/api/v1/x", nil, nil); err != nil {
				t.Errorf("Do: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, ok := issued.Load(c.CurrentToken()); !ok {
		t.Errorf("the token %q is not one the server issued", c.CurrentToken())
	}
	if renewed.Load() == 0 {
		t.Error("OnRenew was never called")
	}
}
