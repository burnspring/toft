package entitlements

import "testing"

// The catalog is a specification, and these are the properties a reader of
// `Known` is entitled to assume. This package had no test file at all until the
// tier arrived; an entitlement seam that has never withdrawn anything has not
// been proven to be able to.

// **Every entry declares what it costs and whether it can be switched off.**
//
// AdminSwitch is the cost of joining this catalog: `featureDTO` merges Known
// with the `features` row, so an entry that gains a switch it should not have
// hands an administrator a way to break their own install. It is set explicitly
// on every entry rather than defaulted, and this is what says so.
func TestEveryEntryDeclaresItsTierAndItsSwitch(t *testing.T) {
	if len(Known) == 0 {
		t.Fatal("the catalog is empty")
	}
	seen := map[string]bool{}
	for _, d := range Known {
		if seen[d.Name] {
			t.Errorf("%s appears twice", d.Name)
		}
		seen[d.Name] = true

		if d.Title == "" || d.Summary == "" {
			t.Errorf("%s: a feature with no words cannot be rendered as locked", d.Name)
		}
		if _, ok := ParseTier(string(d.MinTier)); !ok {
			t.Errorf("%s: MinTier %q is not a tier", d.Name, d.MinTier)
		}
		if d.MinTier == TierPaid {
			t.Errorf("%s: MinTier is paid, so a trial license would not unlock what it is a "+
				"trial of. Everything a paid license unlocks declares TierTrial", d.Name)
		}
	}
}

// The three surfaces an administrator must not be able to switch off: turning
// them off would be turning off the way colleagues sign in, who owns which app,
// or which repositories may be deployed from — and there is nothing to switch,
// because what a license gates is writing *new* configuration and never
// honouring what is already written.
func TestTheSurfacesThatMayNotBeSwitchedOff(t *testing.T) {
	for _, name := range []string{FeatureIdentityProviders, FeatureGroups,
		FeatureAppStorage, FeatureTrustedSources} {
		d := Describe(name)
		if d == nil {
			t.Fatalf("%s is not in the catalog", name)
		}
		if d.AdminSwitch {
			t.Errorf("%s has an administrator's switch", name)
		}
	}
}

// The rank is the whole of the ordering, and a trial reaches everything a paid
// license unlocks.
func TestTierOrdering(t *testing.T) {
	for _, tc := range []struct {
		tier, min Tier
		want      bool
	}{
		{TierDemo, TierDemo, true},
		{TierDemo, TierFree, false},
		{TierFree, TierFree, true},
		{TierFree, TierTrial, false},
		{TierTrial, TierTrial, true},
		{TierTrial, TierFree, true},
		{TierPaid, TierTrial, true},
		{TierPaid, TierFree, true},
		{"reticulating", TierFree, false},
	} {
		if got := tc.tier.AtLeast(tc.min); got != tc.want {
			t.Errorf("%q.AtLeast(%q) = %v, want %v", tc.tier, tc.min, got, tc.want)
		}
	}
}

// A word this build has never heard of is not a tier. It is refused rather than
// approximated, and the caller's answer to that is the tier already held.
func TestParseTierRefusesRatherThanApproximating(t *testing.T) {
	for _, word := range []string{"", "Demo", "enterprise", "paid "} {
		if _, ok := ParseTier(word); ok {
			t.Errorf("%q was read as a tier", word)
		}
	}
	for _, want := range Tiers {
		got, ok := ParseTier(string(want))
		if !ok || got != want {
			t.Errorf("%q did not round-trip", want)
		}
	}
}

// **The cap binds on the two unlicensed tiers only.** A licensed install's
// counts are reported and settled at renewal; refusing on a paid customer's own
// contract number is where licensing systems start causing outages in other
// people's datacentres.
func TestOnlyTheUnlicensedTiersAreCapped(t *testing.T) {
	demo, ok := AppLimit(TierDemo)
	if !ok || demo <= 0 {
		t.Fatalf("the demo tier has no app cap: %d %v", demo, ok)
	}
	free, ok := AppLimit(TierFree)
	if !ok || free <= demo {
		t.Fatalf("the free tier's cap is %d against the demo tier's %d", free, demo)
	}
	for _, tier := range []Tier{TierTrial, TierPaid} {
		if _, capped := AppLimit(tier); capped {
			t.Errorf("%q refuses on a count", tier)
		}
	}
}

// The demo tier is what an install with no key is on, and it is entitled to
// exactly the features that declare no tier.
func TestUnlicensedIsTheDemoTier(t *testing.T) {
	u := Unlicensed{}
	if u.Tier() != TierDemo {
		t.Fatalf("Unlicensed.Tier() = %q", u.Tier())
	}
	for _, d := range Known {
		got := u.Entitled(d.Name)
		if got.Entitled != !d.RequiresLicense() {
			t.Errorf("%s: entitled = %v on the demo tier", d.Name, got.Entitled)
		}
		if !got.Unlicensed {
			t.Errorf("%s: an install with no key did not report itself unlicensed", d.Name)
		}
		if got.Reason == "" {
			t.Errorf("%s: no reason", d.Name)
		}
	}
	// A name absent from the catalog is answered false with a reason, never
	// true by omission.
	if got := u.Entitled("teleportation"); got.Entitled || got.Reason == "" {
		t.Fatalf("an unknown feature: %+v", got)
	}
}

// AtTier is the fixture, and it answers exactly the rank.
func TestAtTier(t *testing.T) {
	paid := AtTier(TierPaid)
	if paid.Tier() != TierPaid {
		t.Fatal("AtTier does not report its own tier")
	}
	for _, d := range Known {
		if !paid.Entitled(d.Name).Entitled {
			t.Errorf("%s is not entitled at a paid license", d.Name)
		}
	}
	free := AtTier(TierFree)
	if free.Entitled(FeatureDependencyPolicy).Entitled {
		t.Error("the free tier reached dependency policy, which needs a trial")
	}
	if !free.Entitled(FeatureIdentityProviders).Entitled {
		t.Error("the free tier does not reach identity providers — which is what free is for")
	}
	if free.Entitled(FeatureGroups).Entitled {
		t.Error("the free tier reached groups")
	}
}

// Perpetual is what this build runs on: every catalogued feature entitled, the
// paid tier with no cap and no term, and nothing saying "no license key".
func TestPerpetualEntitlesEveryFeature(t *testing.T) {
	p := Perpetual{}
	if p.Tier() != TierPaid {
		t.Fatalf("Perpetual.Tier() = %q", p.Tier())
	}
	if _, capped := AppLimit(p.Tier()); capped {
		t.Fatal("the perpetual tier refuses on a count")
	}
	for _, d := range Known {
		got := p.Entitled(d.Name)
		if !got.Entitled || got.Unlicensed || got.InGrace || got.ExpiresAt != nil || got.Reason == "" {
			t.Errorf("%s: %+v", d.Name, got)
		}
	}
	if got := p.Entitled("teleportation"); got.Entitled || got.Reason == "" {
		t.Fatalf("an unknown feature: %+v", got)
	}
	if _, ok := Default().(Perpetual); !ok {
		t.Fatalf("Default() is %T, not Perpetual", Default())
	}
}
