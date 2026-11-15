// Package entitlements answers one question: has this install *bought* a
// feature.
//
// It is the first of the four gates a feature passes through, and the only one
// with no table behind it. The other three are an administrator's switch
// (`features`), a per-app grant table owned by whichever capability has one,
// and a check immediately before the transport (appint.Check). Keeping this one separate from the administrator's
// switch is deliberate: somebody must be able to turn off a feature they have
// paid for — during an incident, while a data-egress policy is reviewed, or
// because a budget ran out — without surrendering the entitlement.
//
// # What a license key honestly is
//
// Stated here rather than left for sales material to invent. This runs inside a
// binary the customer executes, so it is **bookkeeping and friction, not a
// security boundary**. Anyone who can run the binary can patch it. What it
// honestly buys: an honest customer knows what they bought, accidental use of
// an unbought feature is prevented, and there is a hook for expiry and renewal.
// Every claim made about it has to stay inside that.
//
// # Why it is an interface, and why the default is still permissive
//
// The seam is here and the one supplier is `Perpetual`, which `main.go` wires
// and `Default()` returns. This package holds the vocabulary — the feature
// catalog and the answer shape — and knows nothing about keys, endpoints or
// terms; the product has none (ADR §32, *Toft is free*).
//
// `Unlicensed` is **the demo tier** rather than "everything is available", and
// `Status.Unlicensed` is what stops a surface rendering it as a green tick that
// means nothing. Tests use it and `AtTier` to drive the gates.
//
// # The tier, and the rule it changed
//
// `Tier` is the input to every gate here, and `Descriptor.MinTier` is what a
// feature declares against it. That reverses one sentence this file used to
// state — *narrowing arrives with the first feature that needs it, rather than
// sitting here empty as a lever anybody could pull in a one-line diff* — and it
// arrives the way that sentence asked for: with the features it gates named one
// by one in `Known`, in the release that can say so.
//
// Seats are deliberately not here, and there is no seat cap on any tier.
// Nothing in this product creates a user — there is no `POST /users` — so a
// seat number would refuse a route that does not exist. A license may *state* a
// number, the license page renders it and the check-in carries the count; it is
// settled at renewal. What a tier does refuse is app **creation**, on the two
// unlicensed tiers only, and that is `AppLimit`.
package entitlements

import (
	"strings"
	"time"
)

// Feature names. Constants rather than free strings so a typo is a compile
// error rather than a feature that is silently never entitled.
const (
	// The value `faq` was app inference — an app asking the platform's model a
	// question. The subsystem is gone and the entitlement with it, and
	// license.Source already answers an uncatalogued name `false` with a
	// reason, so a license still granting it is inert rather than invalid.
	//
	// **The string is retired, not freed.** It is named in every license
	// already issued and in stored rows on installs that switched it on. A
	// future feature reclaiming the value would inherit grants nobody meant to
	// give it, which is the same reasoning that kept the value when the FAQ it
	// was named for went (ADR §81 D9), read in the opposite direction.

	// FeatureDependencyPolicy is which third-party packages an install will
	// run (ADR §39).
	//
	// An entitlement rather than a plain switch because it carries a genuine
	// standing cost — corpus curation, mirror support, and the promise that
	// the data keeps arriving — which is the honest thing for an addon to
	// price. What that must never become is a lapse that reads as a clean
	// bill of health: when the term ends the stored inventory and the recorded
	// findings are **not** deleted, the surfaces say the license lapsed and
	// give the date the figures stop meaning anything, and the gate simply
	// stops gating. An expiry that silently turned forty amber apps green is
	// the failure mode this feature has to be built against.
	FeatureDependencyPolicy = "dependency_policy"

	// The six configuration surfaces a license gates. They are not addons: no
	// app is granted one, nothing is checked before a transport, and what they
	// gate is **writing new configuration** and never honouring configuration
	// already written.
	//
	// They are entries in this catalog rather than a second helper beside it,
	// and that cost is paid deliberately (`AdminSwitch`): two places that answer
	// "may this install use X" is the thing this package exists to prevent, and
	// a surface with no catalog entry cannot be rendered as locked.
	FeatureIdentityProviders = "identity_providers"
	FeatureGroups            = "groups"
	FeatureAppStorage        = "app_storage"
	FeatureTelemetry         = "telemetry"
	FeatureTrustedSources    = "trusted_sources"

	// FeatureIntegrations is connecting this install to somebody else's system —
	// a chat tool, a document library, an on-call service, a webhook receiver.
	//
	// **One entry for the whole catalog, not one per kind.** Eight surfaces
	// gating separately would be a pricing model nobody asked for and eight rows
	// on the license page nobody can tell apart, and it would put this package in
	// the business of having an opinion about which chat tool is worth more.
	FeatureIntegrations = "integrations"

	// FeatureSlack is what FeatureIntegrations used to be called, and it is kept
	// as a superseded alias rather than deleted.
	//
	// It has to be kept. This package refuses to answer `true` by omission — a
	// name absent from Known is not a feature — so a license in the field naming
	// `slack` would lose the whole catalog the day the rename lands, on an
	// install that had paid for it. Resolve() maps it, Known carries it as
	// superseded so nothing renders it as a second row, and the vendor's
	// repository has to grow the new name before this ships. That last part
	// cannot be done from inside this repository, and it is the LP<n> prefix
	// problem again.
	FeatureSlack = "slack"

	// FeaturePublicApps is serving an app to people who are not signed in.
	//
	// It is the only entry in this catalog that gates a property of an *app*
	// rather than a configuration surface of the install, and it is the one
	// whose absence is most visible to people who never see this page — an
	// unauthenticated visitor either gets the app or gets a sign-in form. That
	// is why the gate is on the transition and never on the state: an app that
	// is already public stays public, and what a license withholds is the
	// ability to publish another.
	FeaturePublicApps = "public_apps"
)

// Known is every feature this binary knows how to gate, in the order the admin
// surface lists them. A name absent from here is not a feature; asking about one
// is answered `false` with a reason saying so, rather than `true` by omission.
var Known = []Descriptor{
	{
		Name:  FeatureDependencyPolicy,
		Title: "Dependency policy",
		Summary: "Checks the packages each app installs against published advisories, and can " +
			"refuse a malicious or vulnerable one at deploy. Only names and versions are read.",
		MinTier:     TierTrial,
		AdminSwitch: true,
	},
	{
		Name:  FeatureIdentityProviders,
		Title: "Identity providers",
		Summary: "Sign in with your organization's OIDC or GitHub identity. The built-in " +
			"administrator always works without one.",
		// Free, and this is the free tier's actual value proposition. Nothing
		// in this product creates a user, so on an install with no provider
		// there is exactly one account forever: a provider is not an enterprise
		// nicety here, it is the only way to get a second person in.
		MinTier: TierFree,
		// No switch. An administrator turning this off would be switching off
		// the way their colleagues sign in, and there is nothing to switch —
		// what the license decides is whether *new* configuration may be
		// written, never whether existing configuration is honoured.
		AdminSwitch: false,
	},
	{
		Name:        FeatureGroups,
		Title:       "Groups",
		Summary:     "Own apps as a team, so access survives somebody leaving.",
		MinTier:     TierTrial,
		AdminSwitch: false,
	},
	{
		Name:  FeatureAppStorage,
		Title: "App storage",
		Summary: "Give an app a bucket or a shared filesystem, with per-person grants. " +
			"Storage already attached keeps working without it.",
		MinTier:     TierTrial,
		AdminSwitch: false,
	},
	{
		Name:  FeatureIntegrations,
		Title: "Integrations",
		Summary: "Let apps send to chat, document libraries, on-call or a signed webhook. " +
			"An app has none until an administrator attaches one.",
		MinTier:     TierTrial,
		AdminSwitch: true,
	},
	{
		Name:  FeatureSlack,
		Title: "Slack",
		Summary: "Superseded by Integrations. Kept so a license naming it keeps working; " +
			"it grants exactly what Integrations grants.",
		MinTier: TierTrial,
		// No switch, and it is not rendered as a row of its own: it is the same
		// entitlement under an older name, and two toggles for one decision is
		// the thing this catalog exists to prevent.
		AdminSwitch: false,
		Superseded:  FeatureIntegrations,
	},
	{
		Name:        FeatureTelemetry,
		Title:       "Telemetry export",
		Summary:     "Export this install's traces and metrics to your own collector.",
		MinTier:     TierTrial,
		AdminSwitch: true,
	},
	{
		Name:  FeaturePublicApps,
		Title: "Public apps",
		Summary: "Serve an app to anyone with the link, with no sign-in. Without it, public " +
			"apps keep serving but no new one can be made public.",
		MinTier: TierTrial,
		// No switch. An administrator turning this off would withdraw anonymous
		// access from every public app at once, which is a thing the platform
		// only ever does one app at a time and only on evidence.
		AdminSwitch: false,
	},
	{
		Name:  FeatureTrustedSources,
		Title: "Trusted app sources",
		Summary: "Limit which repositories this install deploys from. Changing the policy " +
			"never stops a running app.",
		MinTier:     TierTrial,
		AdminSwitch: false,
	},
}

// Descriptor is what the admin surface renders for a feature that is not
// installed yet. It carries no state — state is the `features` row.
type Descriptor struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Summary string `json:"summary"`

	// MinTier is the narrowing lever, and it is a tier rather than a boolean so
	// that "which license" is one fact instead of a boolean plus a lookup
	// somewhere else. The zero value is TierDemo, which is every install: an
	// entry that declares nothing gates nothing.
	MinTier Tier `json:"min_tier"`

	// Superseded names the entry this one is an older name for.
	//
	// A license in the field is a document somebody bought; renaming a feature
	// must not invalidate it, and this package's refusal to answer `true` by
	// omission means a dropped name is a feature that silently goes away. So an
	// old name stays in the catalog and points at the new one, the admin surface
	// does not render it as a second row, and `Resolve` is the one place the
	// mapping happens.
	Superseded string `json:"superseded,omitempty"`

	// AdminSwitch is whether gate 2 exists for this feature.
	//
	// It is set explicitly on every entry rather than defaulted, because the
	// cost of a catalog entry is exactly this: `featureDTO` merges `Known` with
	// the `features` row, so joining the catalog would otherwise hand an
	// administrator a switch that can break their own install — turning off the
	// providers their colleagues sign in with. False means the row renders with
	// its license state and no toggle, and `handleUpdateFeature` refuses it by
	// name.
	AdminSwitch bool `json:"admin_switch"`
}

// RequiresLicense reports whether this feature is narrowed by a license at all.
//
// A method rather than a second field, deliberately: a boolean beside MinTier
// is a fourth fact that can disagree with the third, and this file's own
// argument against a stored tier is the argument against it.
func (d Descriptor) RequiresLicense() bool { return d.MinTier != TierDemo }

// Describe returns the descriptor for a feature name, or nil. A superseded name
// resolves to the entry that supersedes it, so a caller never has to know which
// of two names a license used.
func Describe(name string) *Descriptor {
	for i := range Known {
		if Known[i].Name != name {
			continue
		}
		if to := Known[i].Superseded; to != "" {
			return Describe(to)
		}
		return &Known[i]
	}
	return nil
}

// Current is every feature the admin surface renders, which is Known without
// the superseded aliases. One list is the catalog; the other is the page.
func Current() []Descriptor {
	out := make([]Descriptor, 0, len(Known))
	for _, d := range Known {
		if d.Superseded == "" {
			out = append(out, d)
		}
	}
	return out
}

// Status is the answer, and it carries the words a person reads.
//
// A refusal that says only "not licensed" makes an operator open a ticket to
// learn whether they never bought it, their term lapsed, or they are inside a
// grace period that is about to end. Those are three different actions.
type Status struct {
	// Entitled is the gate. False means inference is refused for this feature.
	Entitled bool `json:"entitled"`
	// Reason is empty when entitled without qualification. When entitled inside
	// a grace period it carries the warning; when not entitled it carries why.
	Reason string `json:"reason,omitempty"`
	// Unlicensed reports that this install has no license key at all, which is
	// the permissive default rather than a grant somebody was given. The admin
	// surface says so plainly instead of rendering a green tick that means
	// nothing.
	Unlicensed bool `json:"unlicensed"`
	// ExpiresAt is when the term ends, when there is one. Nil under the
	// permissive default.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// InGrace reports that the term has ended and the install is inside the
	// window that warns rather than refusing. A license never takes down running
	// code: an app whose feature has lapsed keeps serving and says why.
	InGrace bool `json:"in_grace"`
	// Tier is the tier the answer was decided against, and MinTier is what the
	// feature asked for. Carried on the Status rather than fetched again by the
	// surface, so that the words on the screen and the decision behind them
	// cannot be two different reads.
	Tier    Tier `json:"tier"`
	MinTier Tier `json:"min_tier"`
}

// Source is the seam. **One seam and two methods**, and the second is not a
// second gate.
//
// This interface used to have one method on the argument that a second would be
// a second place a gate could be decided. Tier is not that: it is the *input*
// to every gate, including Entitled, which is answered in terms of it. The
// alternative — folding the tier into Status and reading it off some arbitrary
// feature — was rejected because the app cap has no feature name to ask about.
type Source interface {
	// Entitled reports whether this install may use the named feature right now.
	Entitled(feature string) Status
	// Tier is what this install has bought, resolved fresh. Every gate reads
	// this one function, and nothing stores what it returns.
	Tier() Tier
}

// Unlicensed is the answer for an install holding no license key: **the demo
// tier**, and honest about it.
//
// It used to mean "everything this binary knows about is entitled". It now
// means the tier an install with no key is on, which is the same fact stated
// once instead of twice — the demo tier is *defined* by the absence of a key,
// which is also why it is the only tier that makes no outbound request.
// Status.Unlicensed is still set, so no surface can render "licensed" over an
// install that was never asked for a key.
//
// It is also what a Server or a Resolver constructed without a source falls
// back to, and that now costs something it did not before: a consumer added
// without being wired resolves to demo rather than to everything-available. The
// answer is not to make the fallback generous — that would be a license you get
// by forgetting to wire one — it is that the number of consumers is written
// down in `main.go` and the list is the specification. A test that needs a
// licensed install asks for one by name with AtTier.
type Unlicensed struct{}

// Entitled implements Source.
func (Unlicensed) Entitled(feature string) Status {
	d := Describe(feature)
	if d == nil {
		return Status{
			Reason: "this build does not offer a feature called " + feature,
		}
	}
	st := Status{Unlicensed: true, Tier: TierDemo, MinTier: d.MinTier}
	if !d.RequiresLicense() {
		st.Entitled = true
		st.Reason = "No license is needed for " + d.Title +
			", and this install has none"
		return st
	}
	// The tier leads the sentence, never the feature: a title may be plural and
	// a tier never is.
	st.Reason = describeUpper(d.MinTier) + " unlocks " + d.Title +
		". This install has no license key, so it is on " + TierDemo.Describe() + "."
	return st
}

// Tier implements Source.
func (Unlicensed) Tier() Tier { return TierDemo }

// AtTier is a source fixed at one tier, for a caller that already knows the
// answer — a test that needs a licensed install, and nothing in production.
//
// Named rather than hand-rolled at each call site so that "this fixture is
// deliberately licensed" is greppable, and so that a test never has to build a
// cached vendor response to say something that simple.
type AtTier Tier

// Entitled implements Source.
func (a AtTier) Entitled(feature string) Status {
	d := Describe(feature)
	if d == nil {
		return Status{Reason: "this build does not offer a feature called " + feature}
	}
	tier := Tier(a)
	st := Status{Tier: tier, MinTier: d.MinTier, Entitled: tier.AtLeast(d.MinTier)}
	if st.Entitled {
		st.Reason = describeUpper(tier) + " includes " + d.Title
	} else {
		st.Reason = describeUpper(d.MinTier) + " unlocks " + d.Title +
			", and this install is on " + tier.Describe() + "."
	}
	return st
}

// Tier implements Source.
func (a AtTier) Tier() Tier { return Tier(a) }

// describeUpper is a tier's description with a capital, for the start of a
// sentence. It exists so that no sentence in this package has a feature title
// as its subject: a title may be plural and a tier never is, and *Groups needs
// a trial license* reads as a bug in the product.
func describeUpper(t Tier) string {
	s := t.Describe()
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Perpetual is the answer this build gives: **a perpetual enterprise license,
// held by every install, with no key and no check-in.**
//
// Toft is free (ADR §32, *Toft is free*), so there is nothing to buy and
// nothing to lock. The tier is `paid`, the plan enterprise unlimited, there is
// no term and no grace window, and every feature in `Known` is entitled — so
// no app cap binds, no configuration surface is locked, and nothing an
// administrator sees mentions a license. `Unlicensed` and `AtTier` are kept,
// unused in production, because the gates' tests drive them.
//
// One strictness survives: a name absent from the catalog is still answered
// `false` with a reason, never `true` by omission.
type Perpetual struct{}

// PerpetualPlan is the plan Perpetual describes itself as.
const PerpetualPlan = PlanEnterpriseUnlimited

// Entitled implements Source.
func (Perpetual) Entitled(feature string) Status {
	if Describe(feature) == nil {
		return Status{Reason: "this build does not offer a feature called " + feature}
	}
	return Status{
		Entitled: true,
		Reason:   "Included — Toft is free, and every feature is on.",
		Tier:     TierPaid,
		MinTier:  Describe(feature).MinTier,
	}
}

// Tier implements Source.
func (Perpetual) Tier() Tier { return TierPaid }

// Default is what a Server uses when no Source was supplied, and what `main.go`
// wires: Perpetual. Named rather than constructed inline so the one place the
// fallback is chosen is greppable.
func Default() Source { return Perpetual{} }
