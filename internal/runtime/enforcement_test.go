package runtime

import (
	"testing"

	"toft/internal/agent"
)

// Every (mode, tier, dial), including CPU at the process tier and CPU on a
// cgroup hierarchy without the cpu controller: a wrong word here is a
// guarantee nobody has.
func TestTheEnforcementWordForEveryModeTierAndDial(t *testing.T) {
	cgroup := Containment{Tier: agent.TierCgroup, CPUHeld: true}
	noCPU := Containment{Tier: agent.TierCgroup}
	ns := Containment{Tier: agent.TierNamespace, CPUHeld: true}
	proc := Containment{Tier: agent.TierProcess}
	for _, c := range []struct {
		mode Mode
		c    Containment
		dial string
		want string
	}{
		{ModeIsolated, Containment{}, DialMemory, Enforced},
		{ModeIsolated, Containment{}, DialCPU, Enforced},
		{ModeShared, cgroup, DialMemory, Enforced},
		{ModeShared, cgroup, DialCPU, Enforced},
		{ModeShared, ns, DialMemory, Enforced},
		{ModeShared, ns, DialCPU, Enforced},
		{ModeShared, noCPU, DialMemory, Enforced},
		{ModeShared, noCPU, DialCPU, NotEnforced},
		{ModeShared, proc, DialMemory, Sampled},
		{ModeShared, proc, DialCPU, NotEnforced},
		{ModeShared, Containment{}, DialMemory, Sampled},
		{ModeShared, Containment{}, DialCPU, NotEnforced},
	} {
		if got := Enforcement(c.mode, c.c, c.dial); got != c.want {
			t.Errorf("Enforcement(%s, %+v, %s) = %s, want %s", c.mode, c.c, c.dial, got, c.want)
		}
	}
	if !ContainmentOf(agent.List{Tier: agent.TierCgroup, CPUHeldBy: "cgroup"}).CPUHeld ||
		ContainmentOf(agent.List{Tier: agent.TierCgroup, CPUHeldBy: "none"}).CPUHeld {
		t.Error("ContainmentOf misread cpu_held_by")
	}
}
