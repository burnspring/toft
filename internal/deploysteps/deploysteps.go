// Package deploysteps is the vocabulary of a deploy: the steps it passes
// through, the order they happen in, and which of the three phases each one
// belongs to.
//
// It is a leaf package on purpose. The builder walks these steps, the database
// stamps the current one on the deployment row, and the API reports both the
// step and its phase to whatever is watching — a browser or a CLI. Any of those
// three owning the definition would force the other two to either import it or
// restate it, and a restated step→phase table is one that will eventually
// disagree with itself about what "scan" means.
//
// **The keys are the contract.** They are stored in `deployments.current_step`
// and in `step_timings`, they are the fingerprint that makes two failed builds
// "the same problem" (`builder.recordDeployFailure`), and they go out on the
// wire to clients this repository does not contain. Renaming one is a breaking
// change to every deployment row already written; adding one is not. Human
// labels deliberately live nowhere near here — they are copy, they belong to
// the surface that renders them, and a CLI printing `build` is printing the
// word its user already uses.
package deploysteps

// Phase is the coarse answer to "where is my deploy" — the three stages a
// person publishing an app thinks in, and the unit a progress display is built
// on. Steps are the fine answer, and there are a dozen of them.
type Phase string

const (
	// PhaseUpload is getting the code onto the platform and deciding where it
	// will run. For a bundle that is a genuine upload; for a git-backed app it
	// is a clone, which the UI labels accordingly — the phase key stays the
	// same either way so that one progress model covers both sources.
	//
	// `Setup` sits here rather than in PhaseDeploy, where its meaning ("decide
	// where this release will run, and prove it can") might suggest it
	// belongs. Phases have to be contiguous in Order: a deploy that went
	// deploy → upload → scan → deploy could not be drawn as a progress bar or
	// reported as a stage. Setup is also, honestly, part of getting the code
	// here — it is what makes the fetch that follows it worth doing.
	PhaseUpload Phase = "upload"
	// PhaseScan is the dependency gate: what this release wants to install,
	// judged before anything installs it. It is one step today and stays its
	// own phase anyway, because it is the one stage that can refuse a deploy
	// on grounds that have nothing to do with whether the code works.
	PhaseScan Phase = "scan"
	// PhaseDeploy is everything from working out how to build it to moving
	// traffic onto it.
	PhaseDeploy Phase = "deploy"
)

// Step names one phase of a deploy.
type Step string

const (
	Receive   Step = "receive"
	Unpack    Step = "unpack"
	Setup     Step = "setup"
	Fetch     Step = "fetch"
	Scan      Step = "scan"
	Detect    Step = "detect"
	Configure Step = "configure"
	Build     Step = "build"
	Publish   Step = "publish"
	Start     Step = "start"
	Ready     Step = "ready"
	Flip      Step = "flip"
)

// Order is the specification. A timeline drawn in a different order from the
// one a deploy runs in would be worse than no timeline, and a phase whose
// steps are not contiguous here is not a phase.
var Order = []Step{
	Receive, Unpack, Setup, Fetch,
	Scan,
	Detect, Configure, Build, Publish, Start, Ready, Flip,
}

var phaseOf = map[Step]Phase{
	Receive:   PhaseUpload,
	Unpack:    PhaseUpload,
	Setup:     PhaseUpload,
	Fetch:     PhaseUpload,
	Scan:      PhaseScan,
	Detect:    PhaseDeploy,
	Configure: PhaseDeploy,
	Build:     PhaseDeploy,
	Publish:   PhaseDeploy,
	Start:     PhaseDeploy,
	Ready:     PhaseDeploy,
	Flip:      PhaseDeploy,
}

// conditional steps happen on some deploys and not others, so a later step
// does not imply they were skipped over — it may mean they never applied.
// Only the two that receive an uploaded bundle: a git-backed deploy has no
// bytes to receive and nothing to unpack, and drawing those as "done" because
// the build has moved past them would be a small lie told on every timeline.
var conditional = map[Step]bool{
	Receive: true,
	Unpack:  true,
	// A batch app (ADR §107) starts nothing and waits on nothing; the flip is
	// the whole of its going live.
	Start: true,
	Ready: true,
}

// PhaseOf answers which phase a step belongs to, and returns "" for the empty
// step and for any name it does not know.
//
// Both cases are real and neither is an error. Every deployment row written
// before the step columns existed has no step at all, and a row written by a
// newer binary during a rolling upgrade may name a step this one has never
// heard of. "No phase" is the honest answer to both; inventing one would put a
// confident stage on a deploy nobody measured.
func PhaseOf(step string) Phase {
	return phaseOf[Step(step)]
}

// Phases lists the three, in the order a deploy passes through them.
func Phases() []Phase {
	return []Phase{PhaseUpload, PhaseScan, PhaseDeploy}
}

// StepsIn lists one phase's steps, in Order.
func StepsIn(p Phase) []Step {
	var out []Step
	for _, s := range Order {
		if phaseOf[s] == p {
			out = append(out, s)
		}
	}
	return out
}

// Index is a step's position in Order, or -1 for an unknown one. It is what
// tells a progress display which steps are behind the current one.
func Index(step string) int {
	for i, s := range Order {
		if s == Step(step) {
			return i
		}
	}
	return -1
}

// Conditional reports whether a step only happens on some deploys. A display
// showing a step that is behind the current one but conditional should check
// whether it actually ran — a recorded timing is the proof — before calling it
// done.
func Conditional(step string) bool {
	return conditional[Step(step)]
}

// Reason is why a deploy failed, as a key rather than a sentence.
//
// **It exists for the reader that cannot read English.** A failed deploy has
// always said where it died — the step — and why, in a paragraph written for a
// person. An agent driving `toft` gets the paragraph and has to decide what to do
// next from it, which means matching on prose that every one of ~30 call sites
// is free to reword. The reason is the branchable half of that answer, and the
// question it is shaped to answer is **what do I do next**: fix the code, read
// a different log, change a manifest, or go and ask an administrator.
//
// Like the steps, these keys go out on the wire to clients this repository does
// not contain, they are stored on the deployment row, and **an unknown one must
// render rather than vanish**: a client older than the install it is talking to
// is the normal case, not the exception. Adding a reason is safe; renaming one
// is a breaking change to every row already written.
//
// Empty is a real value and means *unclassified* — a row written before these
// columns existed. It is never rendered as `internal_error`, which is a claim
// about the platform, and no surface may read absence as any particular cause.
type Reason string

const (
	// ReasonSetupFailed is the deploy never getting as far as the code:
	// nowhere to run it, or no backend that could.
	ReasonSetupFailed Reason = "setup_failed"
	// ReasonSourceUnavailable is the code not being obtainable — a clone that
	// failed, a credential that could not be read, a rollback target that has
	// been pruned. Nothing is wrong with the app; something is wrong between
	// here and where the app lives.
	ReasonSourceUnavailable Reason = "source_unavailable"
	// ReasonSourceBlocked is this install refusing the origin: the source
	// policy, a URL the validator will not accept, a branch name it will not.
	// A decision, not a fault, and **the developer cannot fix it by editing
	// code** — which is the whole reason it is not folded into the one above.
	ReasonSourceBlocked Reason = "source_blocked"
	// ReasonDependencyBlocked is the dependency policy refusing named packages.
	// Also a decision rather than a fault, and the one with a structured answer
	// waiting behind it: `GET /apps/{id}/dependencies` names the packages, the
	// advisories and the fixed versions.
	ReasonDependencyBlocked Reason = "dependency_blocked"
	// ReasonUnsupportedFramework is a tree nothing here knows how to serve.
	ReasonUnsupportedFramework Reason = "unsupported_framework"
	// ReasonManifestInvalid is toft.toml refused. Deliberately apart from
	// the one above: the platform understood the app and rejected what it
	// declared, which is a one-file fix rather than a question about whether
	// this app can be hosted at all.
	ReasonManifestInvalid Reason = "manifest_invalid"
	// ReasonStorageRefused is toft.toml's [[storage]] refused: an entry the
	// app's owner or the deployer may not attach, or one the console would
	// refuse too. Apart from ReasonManifestInvalid because the file parsed and
	// the fix is often a permission — an administrator's — not an edit.
	ReasonStorageRefused Reason = "storage_refused"
	// ReasonStorageRevokedDuringDeploy is a delegation the plan relied on
	// revoked or lowered between the plan and the flip. The release was healthy
	// and is abandoned; the previous one keeps serving.
	ReasonStorageRevokedDuringDeploy Reason = "storage_revoked_during_deploy"
	// ReasonRuntimeVersion is a declared language version this install cannot
	// honour.
	ReasonRuntimeVersion Reason = "runtime_version"
	// ReasonBuildFailed is the app's own build or dependency install failing —
	// the ordinary red. **The detail is in the build log**, and nowhere else.
	ReasonBuildFailed Reason = "build_failed"
	// ReasonPublishFailed is the artifact store refusing the built release.
	ReasonPublishFailed Reason = "publish_failed"
	// ReasonStartFailed is the workload not starting.
	ReasonStartFailed Reason = "start_failed"
	// ReasonNotReady is the workload starting and never answering. Apart from
	// ReasonStartFailed because the next thing to read differs: a process that
	// started and then failed has said why in the **runtime** log, which the
	// build log does not contain.
	ReasonNotReady Reason = "not_ready"
	// ReasonOutOfMemory is the workload killed for its memory limit before it
	// answered — a not_ready whose cause is known. Apart from it because the
	// next action is not in any log: the app was sent a signal and printed
	// nothing, and the fix is a larger memory limit, which an administrator
	// may have to allow.
	ReasonOutOfMemory Reason = "out_of_memory"
	// ReasonDiskFull is this install's release disk too full to take another
	// tree: below DISK_HARD_FREE_PERCENT, or short of what this app's last
	// release needed. Not a fault in the app; an administrator frees or grows
	// the disk. Refused before the build starts, never discovered half way.
	ReasonDiskFull Reason = "disk_full"
	// ReasonInternal is the platform itself failing — a directory it could not
	// create, a row it could not write. Never a statement about the app, and
	// the one reason whose right next action is to retry or to tell somebody.
	ReasonInternal Reason = "internal_error"
)

// reasonOf is the default classification of a failure at each step.
//
// **A default the builder may sharpen, never a mapping it must accept.** Most
// steps have exactly one way to fail and the step is the whole answer; the
// handful that do not — `detect`, which refuses three different things, and
// `fetch`, where a policy refusal and a broken clone land at the same step —
// name their reason at the call site. Deriving the default here rather than
// passing one at every `fail()` is the same argument the step itself makes:
// a new failure site cannot forget to be classified.
var reasonOf = map[Step]Reason{
	Receive:   ReasonSourceUnavailable,
	Unpack:    ReasonSourceUnavailable,
	Setup:     ReasonSetupFailed,
	Fetch:     ReasonSourceUnavailable,
	Scan:      ReasonDependencyBlocked,
	Detect:    ReasonUnsupportedFramework,
	Configure: ReasonInternal,
	Build:     ReasonBuildFailed,
	Publish:   ReasonPublishFailed,
	Start:     ReasonStartFailed,
	Ready:     ReasonNotReady,
	Flip:      ReasonInternal,
}

// ReasonOf is the reason a failure at this step gets unless the caller says
// otherwise. An unknown step classifies as nothing at all rather than as an
// internal error: a step this binary has never heard of is a newer install, not
// a broken one.
func ReasonOf(step string) Reason {
	return reasonOf[Step(step)]
}

// Reasons lists the vocabulary, for a test and for anything that has to
// enumerate it. Order is the order a deploy would meet them.
func Reasons() []Reason {
	return []Reason{
		ReasonSetupFailed, ReasonSourceUnavailable, ReasonSourceBlocked,
		ReasonDependencyBlocked, ReasonUnsupportedFramework, ReasonManifestInvalid,
		ReasonStorageRefused, ReasonRuntimeVersion, ReasonBuildFailed, ReasonPublishFailed,
		ReasonStartFailed, ReasonNotReady, ReasonOutOfMemory, ReasonStorageRevokedDuringDeploy, ReasonDiskFull, ReasonInternal,
	}
}

// KnownReason reports whether this binary has heard of a reason. A client uses
// it to decide whether it can say anything more than the key itself.
func KnownReason(reason string) bool {
	for _, r := range Reasons() {
		if string(r) == reason {
			return true
		}
	}
	return false
}
