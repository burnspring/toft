package config

import (
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The dial list is the specification.
//
// Two directions, because either one alone is a sample rather than a list: every
// name in Dials resolves through the envelope, and every field the envelope
// resolves is named in Dials. An eighth dial added the old way — a Config field
// read straight at a call site — compiles, runs and works, right up until an
// administrator changes it and nothing happens, which is exactly the failure the
// two callers that read past the memory ceiling had.
func TestTheDialListIsTheSpecification(t *testing.T) {
	// One install, with every dial set to something distinguishable from both
	// the zero value and the environment's.
	cfg := &Config{
		AppMemoryLimitMB:        512,
		AppCPURequestMillicores: 100,
		AppCPULimitMillicores:   0,
		JobMemoryLimitMB:        1024,
		JobCPULimitMillicores:   2000,
		MaxConcurrentBuilds:     1,
		AutoDeployPollSeconds:   60,
		RRepos:                  "https://p3m.example/cran",
		LogLevel:                slog.LevelInfo,
	}
	overrides := DialOverrides{
		AppMemoryLimitMB:        4096,
		AppCPURequestMillicores: 250,
		AppCPULimitMillicores:   750,
		JobMemoryLimitMB:        3072,
		JobCPULimitMillicores:   500,
		MaxConcurrentBuilds:     4,
		AutoDeployPollSeconds:   30,
		RRepos:                  "https://mirror.internal/cran",
		LogLevel:                "debug",
	}

	// Nothing stored: the environment stands, whole.
	bare := NewResolver(cfg, fixedDials{}).Envelope()
	if bare.AppMemoryLimitMB != 512 || bare.AppCPURequestMillicores != 100 ||
		bare.MaxConcurrentBuilds != 1 || bare.AutoDeployPollSeconds != 60 ||
		bare.RRepos != "https://p3m.example/cran" || bare.LogLevel != slog.LevelInfo {
		t.Fatalf("an install that stored nothing resolved to %+v, want the environment's numbers", bare)
	}

	// Everything stored: every dial moves. A dial that did not move is one
	// nobody can change from the settings page.
	got := NewResolver(cfg, fixedDials{o: overrides}).Envelope()
	for _, c := range []struct {
		key  string
		got  any
		want any
	}{
		{"app_memory_limit_mb", got.AppMemoryLimitMB, 4096},
		{"app_cpu_request_millicores", got.AppCPURequestMillicores, 250},
		{"app_cpu_limit_millicores", got.AppCPULimitMillicores, 750},
		{"job_memory_limit_mb", got.JobMemoryLimitMB, 3072},
		{"job_cpu_limit_millicores", got.JobCPULimitMillicores, 500},
		{"max_concurrent_builds", got.MaxConcurrentBuilds, 4},
		{"auto_deploy_poll_seconds", got.AutoDeployPollSeconds, 30},
		{"r_package_repository", got.RRepos, "https://mirror.internal/cran"},
		{"log_level", got.LogLevel, slog.LevelDebug},
	} {
		if c.got != c.want {
			t.Errorf("%s resolved to %v, want %v — a stored dial that does not move is one "+
				"nobody can change from the settings page", c.key, c.got, c.want)
		}
		found := false
		for _, name := range Dials {
			found = found || name == c.key
		}
		if !found {
			t.Errorf("%s is resolved by the envelope and config.Dials does not name it; add it "+
				"there, so both directions stay tested", c.key)
		}
	}
	if len(Dials) != 9 {
		t.Errorf("config.Dials names %d dials and this test walks 9: %v — the list is the "+
			"specification, so a new one is added deliberately in both places", len(Dials), Dials)
	}
}

// A zero row is the environment's number, never "off" and never "unlimited".
// This is the rule that makes an install which never opens the settings page
// keep exactly the behaviour it has, and the one a settings page must never be
// able to break.
func TestAZeroRowIsTheEnvironmentsNumber(t *testing.T) {
	cfg := &Config{
		AppMemoryLimitMB:        512,
		AppCPURequestMillicores: 100,
		MaxConcurrentBuilds:     2,
		AutoDeployPollSeconds:   60,
		RRepos:                  "https://p3m.example/cran",
		LogLevel:                slog.LevelWarn,
	}
	// Every field zero, and a log level nobody can parse — which must not
	// silence the platform either.
	got := NewResolver(cfg, fixedDials{o: DialOverrides{LogLevel: "verbose"}}).Envelope()

	if got.AppMemoryLimitMB != 512 {
		t.Errorf("memory = %d, want the environment's 512 — a zero row must not uncap a workload",
			got.AppMemoryLimitMB)
	}
	if got.AppCPURequestMillicores != 100 {
		t.Errorf("cpu request = %d, want 100", got.AppCPURequestMillicores)
	}
	if got.MaxConcurrentBuilds != 2 {
		t.Errorf("concurrency = %d, want 2", got.MaxConcurrentBuilds)
	}
	if got.AutoDeployPollSeconds != 60 {
		t.Errorf("poll = %d, want 60", got.AutoDeployPollSeconds)
	}
	if got.RRepos != "https://p3m.example/cran" {
		t.Errorf("repository = %q, want the environment's", got.RRepos)
	}
	if got.LogLevel != slog.LevelWarn {
		t.Errorf("level = %v, want the environment's warn — a stored word nobody can parse must "+
			"not silence the platform or flood its log", got.LogLevel)
	}
}

// A settings document that will not read is not an error a start can carry: the
// operator's envelope stands, and a workload starts with it rather than with
// none.
func TestAnUnreadableDocumentLeavesTheOperatorsEnvelope(t *testing.T) {
	cfg := &Config{AppMemoryLimitMB: 512}
	if got := NewResolver(cfg, nil).Envelope().AppMemoryLimitMB; got != 512 {
		t.Errorf("memory = %d, want 512", got)
	}
}

// Nothing outside this package reads a dial off Config. The test is textual
// because the failure is not: a call site that reads cfg.MaxConcurrentBuilds
// compiles, runs, and quietly ignores the settings page forever.
//
// `main.go` is exempt for the two it is *allowed* to read — the builder's pool
// size and whether a poller exists at all, both of which are construction-time
// facts rather than dials — and `internal/api` is exempt because reporting the
// environment's own figure beside the effective one is the whole point of the
// `env_*` fields.
func TestNoOneOutsideConfigReadsADialOffConfig(t *testing.T) {
	fields := []string{
		"AppMemoryLimitMB", "AppCPURequestMillicores", "AppCPULimitMillicores",
		"JobMemoryLimitMB", "JobCPULimitMillicores", "MaxConcurrentBuilds", "AutoDeployPollSeconds", "RRepos", "LogLevel",
	}
	exempt := map[string]bool{
		"main.go":                           true,
		"internal/api/settings_handlers.go": true,
	}

	var offenders []string
	err := walkGoFiles("../..", func(path string, body string) {
		if exempt[strings.TrimPrefix(path, "../../")] || strings.HasSuffix(path, "_test.go") ||
			strings.Contains(path, "/config/") {
			return
		}
		for _, f := range fields {
			if regexp.MustCompile(`cfg\.` + f + `\b|Config\)\.` + f + `\b`).MatchString(body) {
				offenders = append(offenders, strings.TrimPrefix(path, "../../")+": cfg."+f)
			}
		}
	})
	if err != nil {
		t.Skipf("walking the tree: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("these read a dial straight off Config rather than through config.Envelope: %v — "+
			"a value taken from a struct assembled at process start can only change on a restart, "+
			"which is the thing the dial exists to end", offenders)
	}
}

func walkGoFiles(root string, fn func(path, body string)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := root + "/" + e.Name()
		if e.IsDir() {
			// A dot-directory is never the product: .git, and .claude, which
			// holds whole checkouts (agent worktrees) of other trees.
			if strings.HasPrefix(e.Name(), ".") || e.Name() == "web" || e.Name() == "node_modules" ||
				e.Name() == "data" || e.Name() == "tests" {
				continue
			}
			if err := walkGoFiles(p, fn); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fn(p, string(b))
	}
	return nil
}

type fixedDials struct{ o DialOverrides }

func (f fixedDials) Dials() DialOverrides { return f.o }

// Every level of both resolutions, and every 0: an override wins, a 0 override
// is the install's figure, and a job never inherits the app's.
func TestResourcesResolveOverrideThenInstall(t *testing.T) {
	env := Envelope{AppMemoryLimitMB: 512, AppCPULimitMillicores: 1000,
		JobMemoryLimitMB: 1024, JobCPULimitMillicores: 2000}
	for _, c := range []struct {
		name string
		got  Resources
		want Resources
	}{
		{"app, nothing set", env.ResourcesForApp(0, 0),
			Resources{Resource{512, ResourceDefault}, Resource{1000, ResourceDefault}}},
		{"app, both overridden", env.ResourcesForApp(2048, 250),
			Resources{Resource{2048, ResourceOverride}, Resource{250, ResourceOverride}}},
		{"app, memory only", env.ResourcesForApp(2048, 0),
			Resources{Resource{2048, ResourceOverride}, Resource{1000, ResourceDefault}}},
		{"job, nothing set", env.ResourcesForJob(0, 0),
			Resources{Resource{1024, ResourceDefault}, Resource{2000, ResourceDefault}}},
		{"job, both overridden", env.ResourcesForJob(4096, 500),
			Resources{Resource{4096, ResourceOverride}, Resource{500, ResourceOverride}}},
		{"job, cpu only", env.ResourcesForJob(0, 500),
			Resources{Resource{1024, ResourceDefault}, Resource{500, ResourceOverride}}},
		// An uncapped install CPU stays uncapped; memory never does.
		{"app, install uncapped", Envelope{AppMemoryLimitMB: 512}.ResourcesForApp(0, 0),
			Resources{Resource{512, ResourceDefault}, Resource{0, ResourceDefault}}},
		{"job, empty envelope", Envelope{}.ResourcesForJob(0, 0),
			Resources{Resource{DefaultJobMemoryLimitMB, ResourceDefault}, Resource{0, ResourceDefault}}},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, c.got, c.want)
		}
	}
}
