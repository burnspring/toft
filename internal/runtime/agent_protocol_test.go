//go:build linux

package runtime

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"toft/internal/agent"
)

// An agent from before CPU limits is replaced in place, never trusted to
// ignore the field it does not know: it would decode cpu_limit_millicores into
// nothing and start the workload uncapped while every screen said enforced.
func TestAnOlderAgentIsReplacedBeforeAnythingIsPlaced(t *testing.T) {
	root, err := os.MkdirTemp("", "ap")
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

	// An agent at protocol 1 until it is told to re-exec, and the current
	// one after.
	var replaced atomic.Bool
	var placedBefore atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/reexec" {
			replaced.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !replaced.Load() {
			if strings.HasPrefix(r.URL.Path, "/workloads/") {
				placedBefore.Store(true)
			}
			w.Header().Set(agent.ProtocolHeader, "1")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "this agent speaks protocol 1", "protocol": "1"})
			return
		}
		w.Header().Set(agent.ProtocolHeader, agent.ProtocolVersion)
		_ = json.NewEncoder(w).Encode(agent.List{Protocol: agent.ProtocolVersion, Tier: agent.TierCgroup,
			MemoryHeldBy: "cgroup", CPUHeldBy: "cgroup"})
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	rt := NewAgentRuntime(Isolation{}, LocalResolver{}, &stubAllocator{next: freePort(t)}, filepath.Join(root, "apps"), dir)
	if err := rt.EnsureAgent("/nonexistent", AgentOptions{}); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if !replaced.Load() {
		t.Fatal("an agent at protocol 1 was kept")
	}
	if placedBefore.Load() {
		t.Error("a workload was placed on the older agent before it was replaced")
	}
	if agent.ProtocolVersion == "1" {
		t.Error("the CPU limit is a field an agent must honour, so the protocol must have moved past 1")
	}

	// And the field is on the wire, where a current agent reads it.
	b, _ := json.Marshal(agent.Spec{CPULimitMillicores: 1000})
	if !strings.Contains(string(b), `"cpu_limit_millicores":1000`) {
		t.Errorf("the spec does not carry the CPU limit: %s", b)
	}
}
