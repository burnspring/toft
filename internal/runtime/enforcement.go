package runtime

import "toft/internal/agent"

// How a limit is held, said beside every limit on every screen (ADR §128): a
// kernel guarantee, a watchdog's sample, or nothing at all. A 24 GB memory
// limit that a watchdog sampled once a second once read exactly like one the
// kernel held; these three words are what tell them apart.
const (
	// Enforced is the kernel's: a pod's limit, or the workload's own cgroup.
	Enforced = "enforced"
	// Sampled is the agent's watchdog, reading the process tree and stopping
	// it past the limit — after it has gone past.
	Sampled = "sampled"
	// NotEnforced is a limit nothing holds on this install.
	NotEnforced = "not_enforced"
)

// The two limits a workload has.
const (
	DialMemory = "memory"
	DialCPU    = "cpu"
)

// Containment is how the shared backend holds its workloads, as the agent
// reports it: its tier, and whether the cpu controller reached the workloads'
// cgroups — asked for best effort, so a cgroup-tier hierarchy without it holds
// memory and not CPU.
type Containment struct {
	Tier    string
	CPUHeld bool
}

// ContainmentOf reads an agent's listing.
func ContainmentOf(l agent.List) Containment {
	return Containment{Tier: l.Tier, CPUHeld: l.CPUHeldBy == "cgroup"}
}

// Enforcement is how one limit of a workload running in mode is held. The
// namespace tier counts as the cgroup tier: the workload's cgroup is the same.
// An unknown tier — an agent that did not answer — is read as the process
// tier, because a word that overstates a guarantee is the failure this exists
// to end.
func Enforcement(mode Mode, c Containment, dial string) string {
	if mode == ModeIsolated {
		return Enforced
	}
	switch c.Tier {
	case agent.TierCgroup, agent.TierNamespace:
		if dial == DialCPU && !c.CPUHeld {
			return NotEnforced
		}
		return Enforced
	}
	if dial == DialMemory {
		return Sampled
	}
	return NotEnforced
}
