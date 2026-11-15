package deps

import (
	"strings"
	"time"

	"toft/internal/advisory"
	"toft/internal/entitlements"
)

// The gate, and the one place the policy is resolved.
//
// **Enforcement points, and the list is the specification** — the ADR §29 D7
// pattern, because it is the list a sixth path added later has to be checked
// against:
//
//  1. internal/builder, immediately before the framework install step.
//  2. internal/runner, before buildSteps, on the **stored** bootstrap verdict.
//  3. The sweep over serving releases, which is the same rule applied to code
//     already running.
//
// There is deliberately no check at POST /apps. Unlike a repository URL,
// nothing is knowable about an app's dependencies until its code has been
// cloned, so the earliest possible point is the build — which means the
// failure mode of this control is always *a deploy that fails*, never *a form
// that refuses*, and that puts unusual weight on the copy.

// PolicyStore is the settings half of the policy. An interface so that this
// package stays importable from `database`, which implements it.
type PolicyStore interface {
	ResolveDependencyPolicy(licensed bool, licenseReason string) (Policy, error)
}

// Resolver bundles the two halves of "what is the policy in force": the stored
// settings, and the entitlement the settings know nothing about.
//
// One of these, shared by the builder, the API and the sweep, so that three
// call sites cannot disagree about whether the feature is on — which is the
// same argument that makes ResolveSourcePolicy one function.
type Resolver struct {
	Store PolicyStore
	// Ents may be nil, which is the permissive default: an install with no
	// license key is entitled to everything this build offers, and says so.
	Ents entitlements.Source
}

// Policy resolves the whole thing. A nil Resolver answers "off", so a caller
// wired without one needs no branch — the feature is absent rather than
// crashing, which is how every optional capability in this product behaves.
func (r *Resolver) Policy() (Policy, error) {
	if r == nil || r.Store == nil {
		return Policy{Mode: "off"}, nil
	}
	src := r.Ents
	if src == nil {
		src = entitlements.Default()
	}
	status := src.Entitled(entitlements.FeatureDependencyPolicy)
	// A lapsed entitlement inside its grace period still counts as licensed —
	// a license never takes down running code, and it must not start refusing
	// deploys on the day a renewal is late either.
	return r.Store.ResolveDependencyPolicy(status.Entitled, status.Reason)
}

// Decision is one release judged: the verdict to store, and the refusal to
// render when there is one.
type Decision struct {
	Verdict Verdict
	// Refusal is nil unless something Refuses: a blocking finding under an
	// enforcing policy, or a malicious package under `warn` as well. Otherwise
	// the findings are recorded and shown exactly as they would be under
	// `block`.
	Refusal *Refusal
	// Findings is everything derived, blocking or not. `warn` mode's whole
	// value is that this is complete.
	Findings []advisory.Finding
}

// Blocking is the subset that would refuse a deploy under this policy,
// unwaived. Non-empty even in `warn`, which is what lets a tab and a preview
// say "this would block" before anybody switches the mode.
func (d Decision) Blocking() []advisory.Finding {
	var out []advisory.Finding
	for _, f := range d.Findings {
		if f.WouldBlock {
			out = append(out, f)
		}
	}
	return out
}

// Decide turns a finding set into a verdict.
//
// It takes findings rather than fetching them, so that the builder, the sweep
// and the preview all get the same answer from the same function without this
// package needing a database. `fromGit` and the policy's waiver setting are
// the two things that change the *wording* of a refusal rather than whether
// there is one — see Refusal.WayOut.
func Decide(policy Policy, findings []advisory.Finding, fromGit bool,
	scanned bool, packages int, ecosystems []string) Decision {

	d := Decision{Findings: findings}
	verdict := AllowedVerdict(policy.Mode, scanned, packages, ecosystems, policy.FeedAsOf)

	var blocking []advisory.Finding
	for _, f := range d.Blocking() {
		if policy.Refuses(f) {
			blocking = append(blocking, f)
		}
	}
	if len(blocking) == 0 {
		// Either nothing blocks, or the mode is `warn` and nothing is malware,
		// or the license lapsed, or no corpus has ever loaded. Each of those is
		// a reason to record and show and not to refuse, and every one of them
		// is visible on the surfaces rather than inferred from a deploy that
		// went through.
		d.Verdict = verdict
		return d
	}

	refusal := &Refusal{
		Blocking:         blocking,
		Threshold:        policy.BlockSeverity,
		FromGit:          fromGit,
		WaiversAvailable: policy.WaiverMaxDays > 0,
	}
	d.Refusal = refusal
	d.Verdict = Verdict{
		Allowed: false, Reason: refusal.Message(), Mode: policy.Mode,
		Scanned: scanned, CheckedAt: time.Now().UTC(), FeedAsOf: policy.FeedAsOf,
		Packages: packages, Ecosystems: ecosystems,
	}
	return d
}

// UnreadRefusal is the refusal for a release whose own manifest could not be
// read, under an enforcing policy — or "" when the policy is not enforcing, in
// which case the release goes ahead as *not scanned* and says why.
//
// A manifest the app's author chose is not the gate's own failure. Letting it
// through would make committing a lockfile this platform does not read — a
// version 1 package-lock.json that `npm ci` installs from all the same — the
// way past `block`.
func UnreadRefusal(policy Policy, unread *UnreadError, fromGit bool) string {
	if unread == nil || len(unread.Problems) == 0 || !policy.Enforcing() {
		return ""
	}
	again := "push again"
	if !fromGit {
		again = "upload the app again"
	}
	// The lockfile sentence is the way forward for a file this platform does
	// not read; a problem that knows a different one says it instead, and the
	// lockfile sentence stays only for those that do not.
	var remedies []string
	lockfile := false
	for _, p := range unread.Problems {
		if p.Remedy != "" {
			remedies = append(remedies, "for "+p.File+", "+p.Remedy)
		} else {
			lockfile = true
		}
	}
	if lockfile {
		remedies = append(remedies, "commit a lockfile this platform reads — a "+
			"package-lock.json written by npm 7 or later, a renv.lock written by renv")
	}
	// A problem with its own remedy is a file that was read and whose packages
	// could not be listed as the build installs them; only the lockfile case
	// is a file this platform cannot read.
	failed := "could not be checked"
	if lockfile {
		failed = "could not be read"
	}
	var b strings.Builder
	b.WriteString("REFUSED: this install does not deploy packages it could not check, and ")
	b.WriteString(strings.Join(unread.Files(), " and ") + " " + failed + ".")
	for _, p := range unread.Problems {
		b.WriteString("\n\n  " + p.File + ": " + p.Err.Error())
	}
	b.WriteString("\n\nThe way forward: " + strings.Join(remedies, "; and ") + " — then " +
		again + ". Administrators set the policy on Admin → Dependencies.")
	return b.String()
}
