package runtime

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func getSystemMetrics() (*SystemMetrics, error) {
	m := &SystemMetrics{CPUCount: runtime.NumCPU()}

	// Memory from /proc/meminfo (Linux)
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/meminfo")
		if err == nil {
			lines := strings.Split(string(data), "\n")
			memInfo := make(map[string]uint64)
			for _, line := range lines {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					key := strings.TrimSuffix(parts[0], ":")
					val, _ := strconv.ParseUint(parts[1], 10, 64)
					memInfo[key] = val * 1024 // convert from kB to bytes
				}
			}
			m.MemoryTotal = memInfo["MemTotal"]
			m.MemoryUsed = memInfo["MemTotal"] - memInfo["MemAvailable"]
			if m.MemoryTotal > 0 {
				m.MemoryPercent = float64(m.MemoryUsed) / float64(m.MemoryTotal) * 100
			}
		}
	}

	// CPU from /proc/stat
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/loadavg")
		if err == nil {
			parts := strings.Fields(string(data))
			if len(parts) > 0 {
				load, _ := strconv.ParseFloat(parts[0], 64)
				cpus := float64(runtime.NumCPU())
				m.CPUPercent = (load / cpus) * 100
				if m.CPUPercent > 100 {
					m.CPUPercent = 100
				}
			}
		}
	}

	// Disk usage
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err == nil {
		m.DiskTotal = stat.Blocks * uint64(stat.Bsize)
		m.DiskUsed = (stat.Blocks - stat.Bfree) * uint64(stat.Bsize)
		if m.DiskTotal > 0 {
			m.DiskPercent = float64(m.DiskUsed) / float64(m.DiskTotal) * 100
		}
	}

	return m, nil
}
