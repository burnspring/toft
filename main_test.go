package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// listenOn starts a server on an ephemeral loopback port and returns it with
// its base URL.
func listenOn(t *testing.T, h http.Handler) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	return srv, "http://" + ln.Addr().String()
}

// The behaviour drain exists for: an app request already being served must
// finish across a platform shutdown. Before draining, the listener closed
// outright and every live app connection died on each upgrade — with
// Toft permanently in the data path and running as a single instance,
// that made routine maintenance a user-visible outage.
func TestInFlightRequestsCompleteAcrossShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	srv, base := listenOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, "complete response")
	}))

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Get(base + "/slow")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		done <- result{body: string(body), err: err}
	}()

	<-started // the handler is running when shutdown begins

	drained := make(chan struct{})
	go func() {
		drainServers(5*time.Second, namedServer{"test", srv})
		close(drained)
	}()

	// Give shutdown a moment to take effect, then let the handler finish.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-drained:
		t.Fatal("drain returned while a request was still in flight")
	default:
	}
	close(release)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("in-flight request failed across shutdown: %v", r.err)
		}
		if r.body != "complete response" {
			t.Errorf("truncated response across shutdown: %q", r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never returned")
	}
}

// New connections must be refused as soon as draining starts. A load balancer
// pulling the instance out takes time to notice, and continuing to accept work
// that will be cut off at the deadline is worse than refusing it immediately.
func TestDrainStopsAcceptingNewRequests(t *testing.T) {
	srv, base := listenOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))

	// Prove it was serving first.
	if resp, err := http.Get(base + "/"); err != nil {
		t.Fatalf("server was not serving: %v", err)
	} else {
		resp.Body.Close()
	}

	drainServers(2*time.Second, namedServer{"test", srv})

	client := &http.Client{Timeout: time.Second}
	if resp, err := client.Get(base + "/"); err == nil {
		resp.Body.Close()
		t.Error("server still accepted a request after draining")
	}
}

// A handler that outlives the budget must not hold shutdown open forever: the
// supervisor's SIGKILL is coming either way, and hanging past it turns a
// bounded pause into an abrupt kill with no ordering at all.
func TestDrainIsBoundedByItsTimeout(t *testing.T) {
	block := make(chan struct{})
	defer close(block)

	srv, base := listenOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))

	started := make(chan struct{})
	go func() {
		close(started)
		client := &http.Client{Timeout: 5 * time.Second}
		if resp, err := client.Get(base + "/hang"); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	time.Sleep(100 * time.Millisecond)

	begin := time.Now()
	drainServers(300*time.Millisecond, namedServer{"test", srv})
	elapsed := time.Since(begin)

	if elapsed > 3*time.Second {
		t.Errorf("drain took %s — it is not bounded by its timeout", elapsed)
	}
}

// A single-listener install (APPS_PORT unset) passes a nil gateway. That must
// be a no-op, not a panic on the shutdown path — the one code path where a
// panic is guaranteed to be noticed only in production.
func TestDrainSkipsNilServers(t *testing.T) {
	srv, _ := listenOn(t, http.NotFoundHandler())
	drainServers(time.Second, namedServer{"apps gateway", nil}, namedServer{"platform API", srv})
}

// Both listeners drain under one shared budget rather than serially, so a slow
// gateway cannot eat the whole allowance and leave the API cut off.
func TestBothListenersDrainConcurrently(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(2)

	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, "ok")
	}
	a, baseA := listenOn(t, http.HandlerFunc(slow))
	b, baseB := listenOn(t, http.HandlerFunc(slow))

	for _, base := range []string{baseA, baseB} {
		go func(base string) {
			defer wg.Done()
			if resp, err := http.Get(base + "/"); err == nil {
				io.ReadAll(resp.Body)
				resp.Body.Close()
			}
		}(base)
	}
	time.Sleep(30 * time.Millisecond)

	begin := time.Now()
	drainServers(5*time.Second, namedServer{"a", a}, namedServer{"b", b})
	elapsed := time.Since(begin)
	wg.Wait()

	// Serial draining would cost about 400ms; concurrent, about 200ms.
	if elapsed > 350*time.Millisecond {
		t.Errorf("drain took %s — listeners appear to be draining serially", elapsed)
	}
}

// A host-store write's audit `after` is valid JSON whatever the object's name:
// Go's %q wrote U+E0001 as \U000e0001, which jsonb refused (AS-7).
func TestHostStoreAuditAfterIsJSON(t *testing.T) {
	key := "reports/\U000E0001 \"quoted\" back\\slash \U000F0000.csv"
	got := hostStoreAuditAfter(key, 42)
	if !json.Valid([]byte(got)) {
		t.Fatalf("not JSON: %s", got)
	}
	var back struct {
		Key   string `json:"key"`
		Bytes int64  `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(got), &back); err != nil || back.Key != key || back.Bytes != 42 {
		t.Errorf("round trip = %+v, %v", back, err)
	}
}
