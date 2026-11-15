package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"toft/internal/agent"
	"toft/internal/runtime"
)

// agentReadings is the shared runtime when it is the agent: its listing, tier
// and all, and its own figures. Asserted rather than added to the seam, as
// /system/agent and shared_containment do, because no other backend has an
// agent to ask about.
type agentReadings interface {
	AgentList(ctx context.Context) (agent.List, error)
	AgentStats(ctx context.Context) (agent.Stats, error)
}

// containment is how the shared backend holds workloads now, for the
// enforcement word beside every limit: the agent's tier and whether it holds
// CPU. An agent that does not answer — or a shared backend that is not the
// agent — reads as the process tier, so no screen overstates a guarantee.
func (s *Server) containment(ctx context.Context) runtime.Containment {
	if s.rts == nil {
		return runtime.Containment{}
	}
	src, ok := s.rts.Shared().(agentReadings)
	if !ok {
		return runtime.Containment{}
	}
	l, err := src.AgentList(ctx)
	if err != nil {
		return runtime.Containment{}
	}
	return runtime.ContainmentOf(l)
}

// agentFDShare is how much of its descriptor limit the agent may hold before
// the row warns. Past the limit every start fails at opening the workload's
// log, so the warning has to come while there is still room to restart it.
const agentFDShare = 0.8

// checkAgent is the shared-mode agent seen from the platform: how it contains
// workloads, whether each one it placed is held the way the tier says, and
// whether the agent itself is holding more than it should.
//
// Whether it answers at all is shared_backend's question and not asked again:
// an agent that does not answer leaves one failing row there and one warning
// here saying what could not be read.
func (s *Server) checkAgent(ctx context.Context, rp *checkReport) {
	src, ok := s.rts.Shared().(agentReadings)
	if !ok {
		rp.add("shared_containment", "Shared containment", checkSkip,
			"Shared workloads are not run by the agent on this install.")
		return
	}
	l, err := src.AgentList(ctx)
	if err != nil {
		rp.add("shared_containment", "Shared containment", checkWarn,
			"The agent did not answer, so how it holds workloads is unknown: "+err.Error())
		return
	}
	rp.add(containmentRow(l))
	rp.add(cgroupFallbackRow(l))

	st, err := src.AgentStats(ctx)
	switch {
	case errors.Is(err, agent.ErrNotFound):
		rp.add("agent_figures", "Agent", checkSkip,
			"The running agent predates its figures; it reports them after its next re-exec.")
	case err != nil:
		rp.add("agent_figures", "Agent", checkWarn, "Could not read the agent's figures: "+err.Error())
	default:
		rp.add(agentFiguresRow(st))
	}
}

// containmentRow is a fact and never a finding, the at-rest encryption row's
// kind: the tier is what the machine allows (ADR §128), the process tier is the
// expected answer on a container install and on a box run as a user, and a row
// amber on most installs forever would teach an operator to read past amber.
// What it buys is the tier, why not the next one, and what the tier costs,
// written where an operator looks when an app misbehaves. The one exception is
// a namespace tier whose workloads cannot resolve names, which is a fault on
// this machine and not a fact about it.
func containmentRow(l agent.List) (id, name, status, detail string) {
	var d string
	switch l.Tier {
	case agent.TierNamespace:
		d = "Namespace: each workload has its own network, mounts, processes and cgroup"
		if l.Bridge != "" {
			d += ", on bridge toft0 (" + l.Bridge + ")"
		}
		d += "."
	case agent.TierCgroup:
		d = "Cgroup: the kernel enforces each workload's limits; workloads share the host network."
	default:
		d = "Process: the agent's watchdog, not the kernel, enforces memory limits; workloads share the host network, and a process that leaves its app's group survives a stop."
	}
	if l.TierReason != "" {
		d += " Not the next tier up: " + l.TierReason + "."
	}
	// How a namespaced workload resolves names, which is the one part of the
	// tier that can be wrong on a healthy machine: a host whose resolver is a
	// stub on its own loopback, and a forwarder that could not take port 53.
	status = checkPass
	switch l.DNS {
	case agent.DNSHost:
		d += " Names resolve through this host's own resolv.conf."
	case agent.DNSUpstreamFile:
		d += " Names resolve through systemd-resolved's upstream servers: this host's resolv.conf names only its own loopback."
	case agent.DNSForwarder:
		d += " Names resolve through the agent's forwarder on " + l.Gateway + ", which relays to this host's own resolver."
	case agent.DNSUnavailable:
		status = checkWarn
		d += " Names may not resolve inside a workload: this host's resolver is on its own loopback, and the agent " +
			"could not listen on port 53 of " + l.Gateway + " to relay to it. Free that port and restart the agent."
	}
	return "shared_containment", "Shared containment", status, d
}

// cgroupFallbackRow finds the workloads the tier does not cover. A cgroup that
// cannot be made starts the workload under the watchdog — said in the agent's
// log, never refused (ADR §128) — so above the process tier a running workload
// with no cgroup is one whose memory limit the kernel is not holding, on an
// install whose every other surface says it is.
func cgroupFallbackRow(l agent.List) (id, name, status, detail string) {
	const rid, rname = "agent_cgroups", "Workload cgroups"
	if l.Tier != agent.TierCgroup && l.Tier != agent.TierNamespace {
		return rid, rname, checkSkip, "At the process tier no workload has a cgroup of its own."
	}
	var running int
	var without []string
	for _, w := range l.Workloads {
		// Online alone: launching is mid-spawn, and every other state has no
		// process to hold.
		if w.State != agent.StateOnline {
			continue
		}
		running++
		if w.Cgroup == "" {
			without = append(without, w.Name)
		}
	}
	if len(without) > 0 {
		sort.Strings(without)
		return rid, rname, checkWarn, fmt.Sprintf(
			"%s of %d running without a cgroup of its own, so the agent's watchdog and not the kernel holds "+
				"its memory: %s. The agent's log has a cgroup fallback line saying why; a restart retries it.",
			countOf(len(without), "workload"), running, list(without))
	}
	return rid, rname, checkPass, fmt.Sprintf("Every running workload has its own cgroup — %s.",
		countOf(running, "workload"))
}

// agentFiguresRow is the two readings that mean something at a single glance.
// The rest of /stats — memory, goroutines — only means something as a trend,
// which is the soak's to watch and not a page that keeps no history.
func agentFiguresRow(st agent.Stats) (id, name, status, detail string) {
	const rid, rname = "agent_figures", "Agent"
	var findings []string
	if st.EndedLogsOpen > 0 {
		holds := "hold"
		if st.EndedLogsOpen == 1 {
			holds = "holds"
		}
		findings = append(findings, fmt.Sprintf(
			"%s still %s its log open, which the agent never does on purpose — on a network "+
				"filesystem that keeps the file and fails the app's delete",
			countOf(st.EndedLogsOpen, "finished run"), holds))
	}
	if st.FDLimit > 0 && float64(st.FDs) >= agentFDShare*float64(st.FDLimit) {
		findings = append(findings, fmt.Sprintf(
			"%d of %d file descriptors open; at the limit every start fails opening its log", st.FDs, st.FDLimit))
	}

	fds := fmt.Sprint(st.FDs)
	if st.FDLimit > 0 {
		fds += fmt.Sprintf(" of %d", st.FDLimit)
	}
	summary := fmt.Sprintf("%s held, %d logs and %s descriptors open; pid %d, protocol %s, up since %s",
		countOf(st.Total, "workload"), st.OpenLogs, fds,
		st.PID, st.Protocol, st.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
	if len(findings) > 0 {
		return rid, rname, checkWarn, sentence(append(findings, summary))
	}
	return rid, rname, checkPass, strings.ToUpper(summary[:1]) + summary[1:] + "."
}
