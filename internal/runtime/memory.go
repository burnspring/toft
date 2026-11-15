package runtime

import "fmt"

// MemoryNearLimit is how close to its cap a workload has to have been for a
// death no backend attributed to be put down to memory. Short of 1.0 because
// the reading that would show the crossing is usually never taken: the kill
// happens between samples, and the next one is of a fresh process. The health
// watcher compares its previous sweep's reading; the builder's readiness wait
// compares the ended process's own peak.
const MemoryNearLimit = 0.90

// MemoryPhrase names the numbers where they are known, and says the one thing
// that is always true where they are not. lastMem is a reading the caller took
// earlier, valid only when hadMem.
//
// Both halves are a backend's, and each is separately absent. The isolated
// backend takes no reading at all — it reports the kernel's verdict and the cap
// off the pod spec — while an install that set no cap has a reading and nothing
// to compare it against. A phrase that invented either would put a number in
// front of an administrator that nothing measured.
func MemoryPhrase(m *AppMetrics, lastMem float64, hadMem bool) string {
	// The killed process's own peak where the backend kept it, over the
	// previous sweep's reading: a sweep is a minute apart, and an app that
	// grows into its cap inside one was last seen far below it.
	switch {
	case m.ExitPeakMemoryMB > 0 && m.MemoryLimitMB > 0:
		return fmt.Sprintf("peaked at %d MB against a %d MB limit", m.ExitPeakMemoryMB, m.MemoryLimitMB)
	case m.ExitPeakMemoryMB > 0:
		return fmt.Sprintf("peaked at %d MB", m.ExitPeakMemoryMB)
	case hadMem && m.MemoryLimitMB > 0:
		return fmt.Sprintf("%.0f MB against a %d MB limit", lastMem, m.MemoryLimitMB)
	case m.MemoryLimitMB > 0:
		return fmt.Sprintf("a %d MB limit", m.MemoryLimitMB)
	case hadMem:
		return fmt.Sprintf("%.0f MB", lastMem)
	default:
		return "no reading was taken"
	}
}

// DiedOfMemory reports whether a workload's most recent death is memory's:
// the backend said so, or the ended process's own peak reached
// MemoryNearLimit of the cap it was given. Only meaningful for a workload
// already known to have died — OOMKilled outlives the restart it caused.
func DiedOfMemory(m *AppMetrics) bool {
	if m == nil {
		return false
	}
	return m.OOMKilled ||
		(m.MemoryLimitMB > 0 && m.ExitPeakMemoryMB > 0 &&
			float64(m.ExitPeakMemoryMB) >= float64(m.MemoryLimitMB)*MemoryNearLimit)
}
