package runner

import (
	"strings"
	"testing"
)

func TestCPUMaxIsReadAsMillicores(t *testing.T) {
	for in, want := range map[string]int{
		"max 100000\n":    0,
		"100000 100000\n": 1000,
		"50000 100000":    500,
		"250000 100000":   2500,
		"":                0,
		"junk":            0,
	} {
		if got := parseCPUMax(in); got != want {
			t.Errorf("parseCPUMax(%q) = %d, want %d", in, got, want)
		}
	}
}

// The hints follow the limit rounded up to whole cores, libuv's never below
// its own 4, and none at all with no limit.
func TestThreadHintsFollowTheLimit(t *testing.T) {
	if h := threadHints(0); len(h) != 0 {
		t.Errorf("no limit gave hints %v", h)
	}
	h := threadHints(1500)
	if h["GOMAXPROCS"] != "2" || h["OMP_NUM_THREADS"] != "2" || h["MC_CORES"] != "2" || h["UV_THREADPOOL_SIZE"] != "4" {
		t.Errorf("1.5 cores gave %v", h)
	}
	if h := threadHints(8000); h["UV_THREADPOOL_SIZE"] != "8" {
		t.Errorf("8 cores gave UV_THREADPOOL_SIZE %q", h["UV_THREADPOOL_SIZE"])
	}
}

// Set from the limit, and never over an app's own value.
func TestThreadHintsReachTheAppUnlessItSetsItsOwn(t *testing.T) {
	old := cpuLimitMillicores
	cpuLimitMillicores = func() int { return 1000 }
	t.Cleanup(func() { cpuLimitMillicores = old })

	get := func(env []string, k string) string {
		for _, kv := range env {
			if name, v, _ := strings.Cut(kv, "="); name == k {
				return v
			}
		}
		return "<unset>"
	}
	c := &Client{baseURL: "http://platform"}
	env := appEnv(&BootstrapResponse{Framework: "python",
		Env: map[string]string{"OMP_NUM_THREADS": "8"}}, c, "serve", t.TempDir())
	if got := get(env, "GOMAXPROCS"); got != "1" {
		t.Errorf("GOMAXPROCS = %q, want 1", got)
	}
	if got := get(env, "OMP_NUM_THREADS"); got != "8" {
		t.Errorf("the app's own OMP_NUM_THREADS was overridden: %q", got)
	}
}
