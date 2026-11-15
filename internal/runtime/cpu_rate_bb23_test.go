//go:build linux

package runtime

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"toft/internal/agent"
)

// BB-23: Admin → Processes read `cpu 0`, or nothing, for an app spinning a
// core. The agent's table is cached for listCacheTTL, and the CPU rate was
// derived between *calls* rather than between *readings* of the counter, so a
// second List() inside the cache window saw the same counter over a gap of
// seconds (0) or a gap under minSampleGap (absent). Every List() reader — the
// apps list, health, processes, compliance — shares the previous sample, which
// made those two windows the common case.
func TestTheCPURateIsDerivedBetweenReadingsNotCalls(t *testing.T) {
	root, err := os.MkdirTemp("", "bb23")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	dir := filepath.Join(root, "agent")
	if err := os.MkdirAll(dir, 0o711); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", agent.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}

	// One clock for both sides: the workload burns one CPU-second per
	// second of it, and the agent reads the counter when it is asked.
	var mu sync.Mutex
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	clock := start
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	fetches := 0

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetches++
		cpu := clock.Sub(start).Seconds()
		mu.Unlock()
		mem := int64(64 << 20)
		w.Header().Set(agent.ProtocolHeader, agent.ProtocolVersion)
		_ = json.NewEncoder(w).Encode(agent.List{
			Protocol: agent.ProtocolVersion, Tier: agent.TierCgroup,
			Workloads: []agent.Status{{
				Name: "spinner", Kind: agent.KindService, State: agent.StateOnline,
				PID: 4242, StartedAt: start, Cgroup: "/toft-apps/spinner",
				CPUSeconds: &cpu, MemoryBytes: &mem,
			}},
		})
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	rt := NewAgentRuntime(Isolation{}, nil, nil, root, dir)
	rt.now = now

	cpuOf := func(step string) *float64 {
		t.Helper()
		l, err := rt.List()
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		m := l["spinner"]
		if m == nil {
			t.Fatalf("%s: no row for spinner", step)
		}
		return m.CPU
	}
	wantCore := func(step string, got *float64) {
		t.Helper()
		if got == nil {
			t.Fatalf("%s: cpu absent after a rate was derived", step)
		}
		if *got < 99 || *got > 101 {
			t.Fatalf("%s: cpu %.2f, want one core (100)", step, *got)
		}
	}

	// The first reading has nothing to be a rate against: absent, honestly.
	if got := cpuOf("first fetch"); got != nil {
		t.Fatalf("first fetch: cpu %v, want absent", *got)
	}
	// Still the same reading inside the window: still nothing to derive.
	advance(1500 * time.Millisecond)
	if got := cpuOf("first window"); got != nil {
		t.Fatalf("first window: cpu %v, want absent", *got)
	}

	// A fresh fetch: the first rate.
	advance(2 * time.Second)
	wantCore("second fetch", cpuOf("second fetch"))
	fetchesAfterSecond := fetches

	// Three readers inside one cache window — one at once, one more than
	// minSampleGap later (the old 0), one less than minSampleGap after that
	// (the old absent).
	wantCore("same instant", cpuOf("same instant"))
	advance(1200 * time.Millisecond)
	wantCore("1.2 s into the window", cpuOf("1.2 s into the window"))
	advance(500 * time.Millisecond)
	wantCore("1.7 s into the window", cpuOf("1.7 s into the window"))
	if fetches != fetchesAfterSecond {
		t.Fatalf("the window was refetched %d times; the test means to stay inside it", fetches-fetchesAfterSecond)
	}

	// Metrics(name) reads the same cached table and agrees.
	m, err := rt.Metrics("spinner")
	if err != nil {
		t.Fatal(err)
	}
	wantCore("Metrics in the window", m.CPU)

	// Past the window: a new reading, a new rate, still one core.
	advance(1500 * time.Millisecond)
	wantCore("third fetch", cpuOf("third fetch"))
	if fetches != fetchesAfterSecond+1 {
		t.Fatalf("want one more fetch past the window, got %d", fetches-fetchesAfterSecond)
	}

	// A refetch forced inside minSampleGap of the last one (a start or stop
	// invalidates the cache) keeps the last rate rather than going absent.
	rt.invalidate()
	advance(300 * time.Millisecond)
	wantCore("refetch under minSampleGap", cpuOf("refetch under minSampleGap"))
}
