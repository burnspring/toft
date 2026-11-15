package config

import (
	"log/slog"
	"strings"
)

// The operating dial — the third category, beside a fact about the install and
// a capability being offered.
//
// A dial is a number or a word about how much of this machine a workload may
// take, or how eagerly the platform works. It records nothing about how the
// install was stood up and it offers and withdraws nothing, so neither of the
// two categories ADR §14 started with fits it: a memory ceiling is not a fact
// anything else was written down against, and raising it takes no capability
// away and grants none. What sorts a dial from the other two is who is holding
// the question when it arises, and for a quantity that is whoever is watching
// the machine run — an administrator in a browser rather than an operator with
// a deploy pipeline.
//
// **A dial keeps its environment variable.** The variable supplies the
// install's number at boot; the settings row overrides it; an unset or zero
// row means the environment's number, never "off" and never "unlimited". That
// is SettingAppMemoryLimitMB's existing shape promoted to a rule, and it earns
// the promotion three times: a fleet stood up by a pipeline can still state its
// numbers in the pipeline, an install that never opens the settings page keeps
// exactly the behaviour it has, and the dangerous end of every range stays
// where a decision is reviewed before it is applied. A settings page from which
// one administrator can uncap every workload on the box is not a settings page.
//
// **Every dial resolves here and nowhere else.** Not because one place is
// tidier, but because the two callers that read past the memory ceiling — a
// job run's fallback and the Usage tab's figure — were each a boot-time copy of
// a number an administrator could change, and neither looked wrong from any
// screen. The resolution is per-read for the same reason: a value taken from a
// struct assembled at process start can only change on a restart, which is the
// thing the dial exists to end.

// Envelope is this install's operating dials, resolved: the environment's
// numbers with the administrator's overrides applied.
//
// Read at each start, each dispatch and each tick — never held. The cost is one
// settings query against a document that is already read at every start.
type Envelope struct {
	// AppMemoryLimitMB caps a workload's RSS, in mebibytes, and it is the
	// number every backend bakes into the process it launches. It therefore
	// reaches a workload at its next start and never before.
	AppMemoryLimitMB int
	// AppCPURequestMillicores is the scheduler's reservation, honoured in
	// isolated mode only. AppCPULimitMillicores is the ceiling, honoured by a
	// pod and by the shared backend's cgroup (cpu.max). Same timing as the
	// memory ceiling and for the same reason: both are baked into the workload
	// at start.
	AppCPURequestMillicores int
	AppCPULimitMillicores   int
	// JobMemoryLimitMB and JobCPULimitMillicores are a job run's defaults,
	// read at each dispatch. An app's limits do not reach its jobs.
	JobMemoryLimitMB      int
	JobCPULimitMillicores int
	// MaxConcurrentBuilds is how many builds may *start*. It is not the size of
	// the worker pool — that is fixed at construction, because resizing one at
	// runtime means stopping workers, which means abandoning a build or waiting
	// on one, in a package whose whole job is not to abandon builds. It is
	// bounded above by the pool the process was built to.
	MaxConcurrentBuilds int
	// AutoDeployPollSeconds is how often the poller looks. Read at each tick,
	// which resets the timer.
	//
	// **Its zero means something different on each side, and that is stated
	// rather than designed around.** 0 in the environment means the poller is
	// never constructed at all; a stored 0 means "the environment's number",
	// like every other dial. An install that wants auto-deploy polling off
	// turns it off where it has always turned it off — "auto-deploy is offered"
	// is a capability question and would be a different key.
	AutoDeployPollSeconds int
	// RRepos is the binary repository R installs from, read per build. It is a
	// dial rather than configuration because it is the one build parameter
	// whose wrong value is discovered from a deploy log by the person who
	// cannot fix it: P3M publishes trixie binaries for x86_64 only, so every
	// arm64 install's R deploys fail until this names a different mirror.
	//
	// It is unvalidated, here as in the environment. The catalog rule it looks
	// like it breaks — *a URL somebody typed is server-side input, refused at
	// save and at dial* — governs a URL the platform then dials, and nothing
	// dials this one: Rscript is handed a repository option and no socket.
	RRepos string
	// LogLevel is how much the platform says, and it is the one dial that takes
	// effect without waiting for anything to restart — see obs.Init's LevelVar.
	// It reaches no workload at all, which is exactly why it may.
	LogLevel slog.Level
}

// DialOverrides is what an administrator stored, in the units the settings rows
// carry — which is not always the unit the wire carries. A field's zero means
// "the environment's number", never "off": an unset row has to change nothing.
type DialOverrides struct {
	AppMemoryLimitMB        int
	AppCPURequestMillicores int
	AppCPULimitMillicores   int
	JobMemoryLimitMB        int
	JobCPULimitMillicores   int
	MaxConcurrentBuilds     int
	AutoDeployPollSeconds   int
	RRepos                  string
	// LogLevel is the stored word rather than a parsed level, because a word
	// nobody can parse must resolve to the environment's level rather than to
	// whatever slog's zero value happens to be.
	LogLevel string
}

// DialSource hands the resolver the administrator's overrides.
//
// It cannot fail, and that is the rule rather than a convenience. A settings
// document that will not read must not stop a workload starting, so an
// unreadable row resolves to the environment's number exactly as an unset one
// does — a workload starts with the operator's envelope rather than with none.
type DialSource interface {
	Dials() DialOverrides
}

// Resolver is the envelope's two halves held together: the environment's
// numbers, and where the administrator's overrides are read from.
//
// Held rather than the resolved values, because the resolution is per-read.
type Resolver struct {
	cfg *Config
	src DialSource
}

// NewResolver ties this process's configuration to the settings document. A nil
// source is an install with no database yet — the environment's numbers stand.
func NewResolver(cfg *Config, src DialSource) *Resolver {
	return &Resolver{cfg: cfg, src: src}
}

// Envelope resolves the dials as they are now.
//
// A nil resolver, or one with no configuration behind it, answers with the zero
// envelope: that is a caller constructed without one — a test — and never an
// install, because every path that starts a workload is wired from main.
func (r *Resolver) Envelope() Envelope {
	if r == nil || r.cfg == nil {
		return Envelope{}
	}
	env := Envelope{
		AppMemoryLimitMB:        r.cfg.AppMemoryLimitMB,
		AppCPURequestMillicores: r.cfg.AppCPURequestMillicores,
		AppCPULimitMillicores:   r.cfg.AppCPULimitMillicores,
		JobMemoryLimitMB:        r.cfg.JobMemoryLimitMB,
		JobCPULimitMillicores:   r.cfg.JobCPULimitMillicores,
		MaxConcurrentBuilds:     r.cfg.MaxConcurrentBuilds,
		AutoDeployPollSeconds:   r.cfg.AutoDeployPollSeconds,
		RRepos:                  r.cfg.RRepos,
		LogLevel:                r.cfg.LogLevel,
	}
	if r.src == nil {
		return env
	}
	o := r.src.Dials()
	// Each dial, one line, and each line is the same rule: a set row wins,
	// anything else hands back to the environment. The list is the
	// specification — TestTheDialListIsTheSpecification walks it.
	if o.AppMemoryLimitMB > 0 {
		env.AppMemoryLimitMB = o.AppMemoryLimitMB
	}
	if o.AppCPURequestMillicores > 0 {
		env.AppCPURequestMillicores = o.AppCPURequestMillicores
	}
	if o.AppCPULimitMillicores > 0 {
		env.AppCPULimitMillicores = o.AppCPULimitMillicores
	}
	if o.JobMemoryLimitMB > 0 {
		env.JobMemoryLimitMB = o.JobMemoryLimitMB
	}
	if o.JobCPULimitMillicores > 0 {
		env.JobCPULimitMillicores = o.JobCPULimitMillicores
	}
	if o.MaxConcurrentBuilds > 0 {
		env.MaxConcurrentBuilds = o.MaxConcurrentBuilds
	}
	if o.AutoDeployPollSeconds > 0 {
		env.AutoDeployPollSeconds = o.AutoDeployPollSeconds
	}
	if v := strings.TrimSpace(o.RRepos); v != "" {
		env.RRepos = v
	}
	if lvl, ok := ParseLogLevel(o.LogLevel); ok {
		env.LogLevel = lvl
	}
	return env
}

// Dials names every operating dial, once, so the set is checkable rather than
// aspirational — the way appint.Known makes the integration list checkable. An
// eighth variable that answers "quantity, pace, ceiling or verbosity" and is not
// here is a test failure.
//
// The names are the settings keys, because that is the half a person types.
var Dials = []string{
	"app_memory_limit_mb",
	"app_cpu_request_millicores",
	"app_cpu_limit_millicores",
	"job_memory_limit_mb",
	"job_cpu_limit_millicores",
	"max_concurrent_builds",
	"auto_deploy_poll_seconds",
	"r_package_repository",
	"log_level",
}

// ParseLogLevel reads the one dial that is not a number.
//
// Unset, or a word this platform does not know, is *not* a level: the caller
// falls back to the environment's, because a stored value nobody can parse must
// not silence the platform or flood its log.
func ParseLogLevel(raw string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// Where a resolved limit came from, printed beside the figure on every
// surface, because a limit nobody can trace is one nobody can raise.
const (
	// ResourceOverride is an administrator's figure for this one app or job.
	ResourceOverride = "override"
	// ResourceDefault is the install's: the settings row, else the
	// environment.
	ResourceDefault = "install default"
)

// Resource is one resolved limit and where it came from. A Value of 0 is no
// limit — reachable for CPU only, by an operator who set 0 in the
// environment.
type Resource struct {
	Value  int
	Source string
}

// Resources is one workload's two limits: memory in MiB, CPU in millicores.
type Resources struct {
	MemoryMB      Resource
	CPUMillicores Resource
}

// ResourcesForApp is what one app's workload is started with: the
// administrator's per-app override where there is one, and the install's
// otherwise. Every start and every screen reads them through here, so an
// app's limits cannot be answered differently by two callers.
//
// **Zero on the app means the install's number, never "uncapped"**, exactly as
// zero in the settings row means the environment's. The install's figure is
// arithmetic about a machine; the app's is about one piece of work, and
// raising the install's number to suit the one report that genuinely needs
// 4 GB raises it for every app on the box.
func (e Envelope) ResourcesForApp(appMemoryMB, appCPUMillicores int) Resources {
	return Resources{
		MemoryMB:      resolveResource(appMemoryMB, e.AppMemoryLimitMB),
		CPUMillicores: resolveResource(appCPUMillicores, e.AppCPULimitMillicores),
	}
}

// ResourcesForJob is what one job's run is started with: the administrator's
// override on that job where there is one, and the install's job defaults
// otherwise. Never the app's — a report's nightly rebuild and the dashboard
// that serves it are two pieces of work — and never the manifest's, which
// declares none.
func (e Envelope) ResourcesForJob(overrideMemoryMB, overrideCPUMillicores int) Resources {
	mem := resolveResource(overrideMemoryMB, e.JobMemoryLimitMB)
	if mem.Value <= 0 {
		// A job is never started uncapped for memory: an envelope assembled
		// without a configuration (a test) still gets the default.
		mem.Value = DefaultJobMemoryLimitMB
	}
	return Resources{
		MemoryMB:      mem,
		CPUMillicores: resolveResource(overrideCPUMillicores, e.JobCPULimitMillicores),
	}
}

func resolveResource(override, install int) Resource {
	if override > 0 {
		return Resource{Value: override, Source: ResourceOverride}
	}
	return Resource{Value: install, Source: ResourceDefault}
}
