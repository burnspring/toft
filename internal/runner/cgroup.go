package runner

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The runner parents the app process and shares its control group, so it can
// read cumulative CPU time and memory high-water marks directly. That is the
// whole reason self-reporting is possible at all, and why an isolated workload
// needs no cloud metrics service: the numbers are already local.
//
// This is showback-grade, not billing-grade. App code shares a container with
// the runner and could suppress its own figures — but the credential is
// workload-scoped, so an app can only misreport *itself*, never inflate
// another's or read anyone else's. Disputable chargeback would need an
// authoritative cloud-side source, which is a different feature.

// cgroupStat is one reading of the workload's control group.
type cgroupStat struct {
	// CPUSeconds is cumulative since the group was created.
	CPUSeconds float64
	// MemoryBytes is current usage; PeakBytes is the kernel's own high-water
	// mark, which is what explains an out-of-memory kill after the fact.
	MemoryBytes int64
	PeakBytes   int64
	// Throttling, cumulative like CPUSeconds: how many enforcement periods
	// the group ran in, in how many it was held at its CPU limit, and for how
	// long. Throttled is false where the kernel keeps no such counters — no
	// cpu controller on the group — which is absent, never zero.
	Throttled        bool
	Periods          int64
	ThrottledPeriods int64
	ThrottledSeconds float64
}

// readCgroup reads the workload's own control group, preferring cgroup v2.
//
// Both versions are handled because the two isolated backends land on
// different hosts: Fargate has historically exposed v1, and modern Kubernetes
// nodes are v2. Reporting nothing on one of them would make a mixed install
// incomparable, which is the one thing the single metric model exists to
// prevent.
func readCgroup() (cgroupStat, error) {
	if s, err := readCgroupV2(); err == nil {
		return s, nil
	}
	return readCgroupV1()
}

const cgroupV2Root = "/sys/fs/cgroup"

// ownCgroupV2Dir is this process's own cgroup v2 directory, from
// /proc/self/cgroup — in a pod with a cgroup namespace that is "/", the root
// it sees, and on a host run with a cgroup of its own it is that cgroup. The
// root alone was right only in the first case: a host run read the cgroup of
// whatever contained the platform.
func ownCgroupV2Dir() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return cgroupV2Root
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			dir := filepath.Join(cgroupV2Root, rest)
			if _, err := os.Stat(filepath.Join(dir, "cpu.stat")); err == nil {
				return dir
			}
		}
	}
	return cgroupV2Root
}

func readCgroupV2() (cgroupStat, error) {
	var s cgroupStat
	dir := ownCgroupV2Dir()

	// cpu.stat: usage_usec is cumulative CPU time in microseconds.
	usec, err := readKeyedValue(dir+"/cpu.stat", "usage_usec")
	if err != nil {
		return s, err
	}
	s.CPUSeconds = float64(usec) / 1e6
	// nr_periods and the two beside it are written only where the cpu
	// controller is enabled on the group; usage_usec is always there.
	if n, err := readKeyedValue(dir+"/cpu.stat", "nr_periods"); err == nil {
		s.Throttled, s.Periods = true, n
		s.ThrottledPeriods, _ = readKeyedValue(dir+"/cpu.stat", "nr_throttled")
		t, _ := readKeyedValue(dir+"/cpu.stat", "throttled_usec")
		s.ThrottledSeconds = float64(t) / 1e6
	}

	if v, err := readIntFile(dir + "/memory.current"); err == nil {
		s.MemoryBytes = v
	}
	// memory.peak is the kernel's own high-water mark. Where it is absent
	// (older kernels), the caller's running maximum of MemoryBytes stands in —
	// less precise across a spike between samples, but never wrong.
	if v, err := readIntFile(dir + "/memory.peak"); err == nil {
		s.PeakBytes = v
	} else {
		s.PeakBytes = s.MemoryBytes
	}
	return s, nil
}

func readCgroupV1() (cgroupStat, error) {
	var s cgroupStat

	// cpuacct.usage is cumulative CPU time in nanoseconds.
	ns, err := readIntFile("/sys/fs/cgroup/cpuacct/cpuacct.usage")
	if err != nil {
		return s, fmt.Errorf("no readable control group (tried v2 and v1): %w", err)
	}
	s.CPUSeconds = float64(ns) / 1e9
	if n, err := readKeyedValue("/sys/fs/cgroup/cpu/cpu.stat", "nr_periods"); err == nil {
		s.Throttled, s.Periods = true, n
		s.ThrottledPeriods, _ = readKeyedValue("/sys/fs/cgroup/cpu/cpu.stat", "nr_throttled")
		t, _ := readKeyedValue("/sys/fs/cgroup/cpu/cpu.stat", "throttled_time")
		s.ThrottledSeconds = float64(t) / 1e9
	}

	if v, err := readIntFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); err == nil {
		s.MemoryBytes = v
	}
	if v, err := readIntFile("/sys/fs/cgroup/memory/memory.max_usage_in_bytes"); err == nil {
		s.PeakBytes = v
	} else {
		s.PeakBytes = s.MemoryBytes
	}
	return s, nil
}

func readIntFile(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(string(data))
	// cgroup v2 writes "max" for an unset limit; it is not a number and must
	// not be read as one.
	if text == "max" {
		return 0, fmt.Errorf("%s is unlimited", path)
	}
	return strconv.ParseInt(text, 10, 64)
}

// readKeyedValue pulls one "key value" line out of a cgroup stat file.
func readKeyedValue(path, key string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == key {
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s not found in %s", key, path)
}

// cpuLimitMillicores is the CPU limit the kernel holds this workload to, read
// from its own cgroup — cpu.max on v2, the CFS quota on v1 — or 0 where there
// is none. A host run with no cgroup of its own reads nothing: the cgroup it
// could read is the platform's.
var cpuLimitMillicores = readCPULimitMillicores

func readCPULimitMillicores() int {
	if os.Getenv(EnvConsumption) == ConsumptionByHost {
		return 0
	}
	if b, err := os.ReadFile(filepath.Join(ownCgroupV2Dir(), "cpu.max")); err == nil {
		return parseCPUMax(string(b))
	}
	quota, err := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	if err != nil || quota <= 0 {
		return 0
	}
	period, err := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err != nil || period <= 0 {
		return 0
	}
	return int(quota * 1000 / period)
}

// parseCPUMax reads cpu.max's "<quota> <period>" as millicores; "max" is 0.
func parseCPUMax(text string) int {
	f := strings.Fields(text)
	if len(f) != 2 || f[0] == "max" {
		return 0
	}
	quota, err1 := strconv.ParseInt(f[0], 10, 64)
	period, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return int(quota * 1000 / period)
}

// threadHints are the thread-count variables a CPU limit implies: whole cores,
// rounded up, for the runtimes that size a pool from the host's core count —
// Go, OpenMP (numpy, data.table), R's parallel — and libuv's pool, which is
// never set below its own default of 4. None at 0.
func threadHints(millicores int) map[string]string {
	if millicores <= 0 {
		return nil
	}
	cores := (millicores + 999) / 1000
	n := strconv.Itoa(cores)
	return map[string]string{
		"GOMAXPROCS":         n,
		"OMP_NUM_THREADS":    n,
		"MC_CORES":           n,
		"UV_THREADPOOL_SIZE": strconv.Itoa(max(cores, 4)),
	}
}
