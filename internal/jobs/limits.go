package jobs

import (
	"fmt"
	"time"

	"toft/internal/config"
	"toft/internal/database"
)

// Where an effective limit came from. Printed beside the figure on every
// surface — `toft job list`, `toft check`, the Automation tab — because a
// developer who cannot see which limit will stop their work finds out by
// having it stopped.
const (
	SourceDeclared = "declared"
	SourceDefault  = config.ResourceDefault
	// SourceOverride is an administrator's figure for this one job.
	SourceOverride = config.ResourceOverride
	SourceClamped  = "clamped"
	// SourceCeiling is the install's default timeout capped by the timeout
	// ceiling. Nothing was declared, so it is not a clamp: no warning, no
	// event, and no "declared" figure to quote.
	SourceCeiling = "install ceiling"
)

// Limit is one effective figure and where it came from.
type Limit struct {
	Value  int    `json:"value"`
	Source string `json:"source"`
	// Declared is what toft.toml asked for when the figure is not it — set
	// only for a clamped timeout, so a reader can say "15m (clamped by admin,
	// declared 45m)" without a second lookup.
	Declared int `json:"declared,omitempty"`
	// Enforcement is how the limit is held where the job runs —
	// runtime.Enforced, Sampled or NotEnforced — set by a caller that knows
	// the backend. A timeout is the platform's own clock, so the job routes
	// say Enforced for it in every mode.
	Enforcement string `json:"enforcement,omitempty"`
}

// Limits is a job's effective timeout (seconds), memory (MiB) and CPU
// (millicores).
type Limits struct {
	TimeoutSec    Limit `json:"timeout"`
	MemoryMB      Limit `json:"memory"`
	CPUMillicores Limit `json:"cpu"`
}

// Clamped reports whether the timeout is below what the job declared — an
// administrator lowered the ceiling under an existing declaration.
func (l Limits) Clamped() bool {
	return l.TimeoutSec.Source == SourceClamped
}

// Timeout is the effective timeout as a duration.
func (l Limits) Timeout() time.Duration { return time.Duration(l.TimeoutSec.Value) * time.Second }

// EffectiveLimits resolves a definition's limits against the install's.
//
// The developer declares the timeout; the administrator sets its default and
// ceiling. A new declaration above the ceiling never gets this far — check and
// deploy refuse it (CheckCeilings) — so a clamp here is always an
// administrator having lowered the ceiling since: running apps keep working,
// capped, and say so.
//
// Memory and CPU are an administrator's alone: the job's own override, else
// the install's job defaults (config.Envelope.ResourcesForJob). Never the
// app's limits and never the manifest's, which declares none.
func EffectiveLimits(def *database.JobDefinition, settings database.PlatformSettings, env config.Envelope) Limits {
	var l Limits

	switch {
	case def != nil && def.TimeoutSec > 0:
		l.TimeoutSec = Limit{Value: def.TimeoutSec, Source: SourceDeclared}
	default:
		l.TimeoutSec = Limit{Value: settings.DefaultJobTimeoutSecs, Source: SourceDefault}
	}
	if max := settings.MaxJobTimeoutSecs; max > 0 && l.TimeoutSec.Value > max {
		l.TimeoutSec = capped(l.TimeoutSec, max)
	}
	if l.TimeoutSec.Value <= 0 {
		l.TimeoutSec = Limit{Value: 3600, Source: SourceDefault}
	}

	var res config.Resources
	if def != nil {
		res = env.ResourcesForJob(def.OverrideMemoryMB, def.OverrideCPUMillicores)
	} else {
		res = env.ResourcesForJob(0, 0)
	}
	l.MemoryMB = Limit{Value: res.MemoryMB.Value, Source: res.MemoryMB.Source}
	l.CPUMillicores = Limit{Value: res.CPUMillicores.Value, Source: res.CPUMillicores.Source}
	return l
}

// capped is l lowered to the ceiling max: a clamp of what toft.toml declared,
// or SourceCeiling for a figure the job inherited.
func capped(l Limit, max int) Limit {
	if l.Source == SourceDeclared {
		return Limit{Value: max, Source: SourceClamped, Declared: l.Value}
	}
	return Limit{Value: max, Source: SourceCeiling}
}

// CeilingError is a declaration above the install's ceiling: refused at check
// and deploy, never reduced. Key names the toft.toml key, so the refusal can
// be a fix rather than a description.
type CeilingError struct {
	Job      string
	Key      string
	Declared int
	Ceiling  int
	Setting  string
	Unit     string
}

func (e *CeilingError) Error() string {
	return fmt.Sprintf("toft.toml [jobs.%s] %s = %s is above this install's ceiling of %s (%s)",
		e.Job, e.Key, e.format(e.Declared), e.format(e.Ceiling), e.Setting)
}

// Describe is the refusal as the rest of a sentence that starts with the key:
// "is 45m, above this install's ceiling of 15m (max_job_timeout_seconds)".
func (e *CeilingError) Describe() string {
	return fmt.Sprintf("is %s, above this install's ceiling of %s (%s)",
		e.format(e.Declared), e.format(e.Ceiling), e.Setting)
}

// Hint is the pasteable fix: the key at the ceiling, or the setting to raise.
func (e *CeilingError) Hint() string {
	return fmt.Sprintf("set %s = %s in [jobs.%s], or ask an admin to raise %s",
		e.Key, e.tomlValue(e.Ceiling), e.Job, e.Setting)
}

func (e *CeilingError) format(v int) string {
	if e.Unit == "s" {
		return HumanSeconds(v)
	}
	return fmt.Sprintf("%d MB", v)
}

func (e *CeilingError) tomlValue(v int) string {
	if e.Unit == "s" {
		return `"` + HumanSeconds(v) + `"`
	}
	return fmt.Sprintf("%d", v)
}

// CheckCeilings refuses a declaration above the install's ceilings. Every
// definition is checked, and the first offender is returned.
func CheckCeilings(defs []database.JobDefinition, settings database.PlatformSettings) error {
	return CheckCeilingsAgainst(defs, nil, settings)
}

// CheckCeilingsAgainst is CheckCeilings for a deploy of an app that already has
// definitions: a declaration that is unchanged from what is deployed is not
// refused for a ceiling an administrator lowered since — the app keeps
// running, capped, and ClampWarnings says so. Only a new or changed
// declaration above the ceiling is refused.
func CheckCeilingsAgainst(defs, existing []database.JobDefinition, settings database.PlatformSettings) error {
	if refused := CeilingRefusals(defs, existing, settings); len(refused) > 0 {
		return refused[0]
	}
	return nil
}

// CeilingRefusals is every job CheckCeilingsAgainst would refuse, in the order
// the definitions came — so a check can tell a refused declaration from a
// clamped one. A new or changed declaration above a ceiling is refused, never
// clamped; `clamped` is a ceiling lowered under one already deployed.
func CeilingRefusals(defs, existing []database.JobDefinition, settings database.PlatformSettings) []*CeilingError {
	before := map[string]database.JobDefinition{}
	for _, d := range existing {
		before[d.Name] = d
	}
	var out []*CeilingError
	for i := range defs {
		d := &defs[i]
		if old, ok := before[d.Name]; ok && old.TimeoutSec == d.TimeoutSec {
			continue
		}
		if max := settings.MaxJobTimeoutSecs; max > 0 && d.TimeoutSec > max {
			out = append(out, &CeilingError{Job: d.Name, Key: "timeout", Declared: d.TimeoutSec, Ceiling: max,
				Setting: database.SettingMaxJobTimeout, Unit: "s"})
		}
	}
	return out
}

// HumanSeconds writes a duration the way toft.toml accepts it and a person
// reads it: 45m, 1h, 1h30m, 90s.
func HumanSeconds(sec int) string {
	d := time.Duration(sec) * time.Second
	switch {
	case sec <= 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0 && d > time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// ClampWarnings are the sentences a deploy and `toft check` print for jobs
// running below the timeout they declare, because an administrator lowered
// the ceiling after the declaration was made.
func ClampWarnings(defs []database.JobDefinition, settings database.PlatformSettings) []string {
	var out []string
	for i := range defs {
		l := EffectiveLimits(&defs[i], settings, config.Envelope{})
		if l.TimeoutSec.Source == SourceClamped {
			out = append(out, fmt.Sprintf("[jobs.%s] timeout %s is clamped to %s by this install's ceiling "+
				"(max_job_timeout_seconds); set timeout = \"%s\" to match", defs[i].Name,
				HumanSeconds(l.TimeoutSec.Declared), HumanSeconds(l.TimeoutSec.Value), HumanSeconds(l.TimeoutSec.Value)))
		}
	}
	return out
}
