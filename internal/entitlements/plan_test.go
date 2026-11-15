package entitlements

import "testing"

// A plan name this build has never heard of is not a plan, and it is not an
// error either — it is `PlanNone`, and every surface renders the tier alone.
//
// This is the weaker half of `ParseTier`'s rule and it is weaker for a reason:
// a tier it cannot name must not *withdraw* anything, whereas a plan it cannot
// name has nothing to withdraw, because nothing reads it.
func TestParsePlanRefusesRatherThanApproximates(t *testing.T) {
	for _, want := range Plans {
		got, ok := ParsePlan(string(want))
		if !ok || got != want {
			t.Errorf("ParsePlan(%q) = %q, %v", want, got, ok)
		}
	}
	// Case and space, because a wire field is somebody else's string.
	if got, ok := ParsePlan("  Enterprise_Limited "); !ok || got != PlanEnterpriseLimited {
		t.Errorf("ParsePlan of a padded, capitalised name = %q, %v", got, ok)
	}
	for _, word := range []string{"", "enterprise", "enterprise_platinum", "paid", "demo"} {
		if got, ok := ParsePlan(word); ok || got != PlanNone {
			t.Errorf("ParsePlan(%q) = %q, %v — want PlanNone, false", word, got, ok)
		}
	}
}

// Every plan has a label and no two share one, because the label is the only
// thing a person sees and two plans rendering the same words is a page that
// cannot be read.
func TestEveryPlanHasADistinctLabel(t *testing.T) {
	seen := map[string]Plan{}
	for _, p := range Plans {
		l := p.Label()
		if l == "" {
			t.Errorf("plan %q has no label", p)
			continue
		}
		if other, dup := seen[l]; dup {
			t.Errorf("plans %q and %q both render as %q", other, p, l)
		}
		seen[l] = p
	}
	// PlanNone is the absence of a plan and must render as nothing, so that a
	// surface testing the label for emptiness is testing the right thing.
	if PlanNone.Label() != "" {
		t.Errorf("PlanNone.Label() = %q, want empty", PlanNone.Label())
	}
}

// **Seated is a property of the plan, not a reading of the number.**
//
// A missing seat figure means two completely different things — this plan has
// no ceiling, and nobody has agreed a number yet — and the wire cannot tell
// them apart. Without this the page would have to guess, and it would guess
// "unlimited" for a limited enterprise customer whose contract had not been
// filled in, which is the one wrong answer that reads as good news.
func TestSeatedSeparatesNoCeilingFromNoNumberYet(t *testing.T) {
	for _, p := range []Plan{PlanFree, PlanEnterpriseLimited} {
		if !p.Seated() {
			t.Errorf("%q carries a seat number and reports Seated() false", p)
		}
	}
	for _, p := range []Plan{PlanTrial, PlanEnterpriseUnlimited, PlanNone} {
		if p.Seated() {
			t.Errorf("%q has no seat ceiling and reports Seated() true", p)
		}
	}
}

// **A plan is not a tier and must never be usable as one.**
//
// The two vocabularies overlap by two words — `free` and `trial` are both a
// plan and a tier — and the temptation this guards against is a caller that
// takes one and passes it to the other because the string happens to parse.
// `enterprise_limited` and `enterprise_unlimited` are not tiers at all, and
// `paid` and `demo` are not plans; a build where either crossed over would be
// granting what nobody sold.
func TestThePlanAndTierVocabulariesDoNotCross(t *testing.T) {
	for _, p := range Plans {
		if _, ok := ParseTier(string(p)); ok && p != PlanFree && p != PlanTrial {
			t.Errorf("plan %q parses as a tier, so a caller could pass one for the other", p)
		}
	}
	for _, word := range []string{"paid", "demo"} {
		if _, ok := ParsePlan(word); ok {
			t.Errorf("tier %q parses as a plan", word)
		}
	}
	// The two enterprise plans are the paid tier at the vendor, and this build
	// must learn that from the `tier` field rather than from the plan's name.
	for _, p := range []Plan{PlanEnterpriseLimited, PlanEnterpriseUnlimited} {
		if _, ok := ParseTier(string(p)); ok {
			t.Errorf("%q is a tier this build can name, which it must not be", p)
		}
	}
}
