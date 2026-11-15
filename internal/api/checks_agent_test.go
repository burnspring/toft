package api

import (
	"strings"
	"testing"
	"time"

	"toft/internal/agent"
)

// The tier is a fact at every tier, the process one included, and says why it
// is not the next.
func TestContainmentRowIsAFact(t *testing.T) {
	for _, tc := range []struct {
		l    agent.List
		want []string
	}{
		{agent.List{Tier: agent.TierProcess, TierReason: "no cgroup v2 subtree this agent may write: read-only"},
			[]string{"Process:", "watchdog", "Not the next tier up: no cgroup v2 subtree"}},
		{agent.List{Tier: agent.TierCgroup, TierReason: "it needs CAP_SYS_ADMIN"},
			[]string{"Cgroup:", "Not the next tier up: it needs CAP_SYS_ADMIN."}},
		{agent.List{Tier: agent.TierNamespace, Bridge: "10.88.0.0/16"},
			[]string{"Namespace:", "10.88.0.0/16"}},
	} {
		id, _, status, detail := containmentRow(tc.l)
		if id != "shared_containment" || status != checkPass {
			t.Errorf("%s: %s %s", tc.l.Tier, id, status)
		}
		for _, w := range tc.want {
			if !strings.Contains(detail, w) {
				t.Errorf("%s: %q lacks %q", tc.l.Tier, detail, w)
			}
		}
		if tc.l.TierReason == "" && strings.Contains(detail, "Not the next") {
			t.Errorf("%s: a reason invented: %q", tc.l.Tier, detail)
		}
	}
}

// At the namespace tier the row says how a workload resolves names, and warns
// only when it may not: the forwarder a loopback-only host needs could not bind.
func TestContainmentRowNamesTheDNSPosture(t *testing.T) {
	for _, tc := range []struct {
		dns, want, status string
	}{
		{agent.DNSHost, "this host's own resolv.conf", checkPass},
		{agent.DNSUpstreamFile, "systemd-resolved's upstream servers", checkPass},
		{agent.DNSForwarder, "forwarder on 10.88.0.1", checkPass},
		{agent.DNSUnavailable, "could not listen on port 53 of 10.88.0.1", checkWarn},
	} {
		_, _, status, detail := containmentRow(agent.List{Tier: agent.TierNamespace, Gateway: "10.88.0.1", DNS: tc.dns})
		if status != tc.status || !strings.Contains(detail, tc.want) {
			t.Errorf("%s: %s %q, want %s and %q", tc.dns, status, detail, tc.status, tc.want)
		}
	}
	if _, _, _, detail := containmentRow(agent.List{Tier: agent.TierCgroup}); strings.Contains(detail, "resolve") {
		t.Errorf("below the namespace tier the row speaks of DNS: %q", detail)
	}
}

// Above the process tier a running workload with no cgroup is named; one that
// is not running is not counted, and the process tier has nothing to find.
func TestCgroupFallbackRow(t *testing.T) {
	workloads := []agent.Status{
		{Name: "app-b", State: agent.StateOnline, Cgroup: ""},
		{Name: "app-a", State: agent.StateOnline, Cgroup: "/sys/fs/cgroup/toft-workloads/app-a.1"},
		{Name: "run-1", State: agent.StateExited},
		{Name: "app-c", State: agent.StateLaunching},
	}

	_, _, status, detail := cgroupFallbackRow(agent.List{Tier: agent.TierCgroup, Workloads: workloads})
	if status != checkWarn || !strings.Contains(detail, "1 workload of 2 running") || !strings.Contains(detail, ": app-b.") {
		t.Errorf("fallback: %s %q", status, detail)
	}

	_, _, status, detail = cgroupFallbackRow(agent.List{Tier: agent.TierNamespace, Workloads: workloads[1:]})
	if status != checkPass || !strings.Contains(detail, "1 workload") {
		t.Errorf("all held: %s %q", status, detail)
	}

	if _, _, status, _ = cgroupFallbackRow(agent.List{Tier: agent.TierProcess, Workloads: workloads}); status != checkSkip {
		t.Errorf("process tier: %s", status)
	}
}

// A finished run holding its log is a leak at one; descriptors warn at 80% of
// the limit, from both sides, and an agent that reports no limit is not judged
// on it.
func TestAgentFiguresRow(t *testing.T) {
	base := agent.Stats{PID: 42, Protocol: "1", StartedAt: time.Now(), Total: 3, OpenLogs: 2, FDs: 20, FDLimit: 1024}

	if _, _, status, detail := agentFiguresRow(base); status != checkPass ||
		!strings.Contains(detail, "pid 42") || !strings.Contains(detail, "20 of 1024 descriptors open") {
		t.Errorf("healthy: %s %q", status, detail)
	}

	leak := base
	leak.EndedLogsOpen = 1
	if _, _, status, detail := agentFiguresRow(leak); status != checkWarn || !strings.Contains(detail, "1 finished run still holds") {
		t.Errorf("leak: %s %q", status, detail)
	}

	near := base
	near.FDs = 819 // 80% of 1024 is 819.2
	if _, _, status, _ := agentFiguresRow(near); status != checkPass {
		t.Errorf("just under: %s", status)
	}
	near.FDs = 820
	if _, _, status, detail := agentFiguresRow(near); status != checkWarn || !strings.Contains(detail, "820 of 1024") {
		t.Errorf("at the share: %s %q", status, detail)
	}

	unknown := base
	unknown.FDLimit, unknown.FDs = 0, 100000
	if _, _, status, _ := agentFiguresRow(unknown); status != checkPass {
		t.Errorf("no limit reported: %s", status)
	}
}
