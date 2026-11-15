package entitlements

import "strings"

// Plan is what the vendor sold, and it is **descriptive only**.
//
// The tier is what every gate in this package reads; a plan gates nothing, is
// compared against nothing, and appears in no `AtLeast`. That separation is the
// whole reason this type can exist without being a risk: the vendor may add a
// fifth plan tomorrow and every install in the field will render it as a name it
// does not know and keep answering exactly as it did.
//
// # Why there are two words at all
//
// The vendor sells four plans and the install understands four tiers, and the
// two lists do not line up — both enterprise plans are the `paid` tier. The
// mapping is the vendor's and it happens before the response is written, which
// is what lets a new plan ship without a protocol version. What arrives here is
// therefore a tier this build can act on *and* a plan name it may only render.
//
// **Never infer a tier from a plan.** `Response.Tier` is the field; a plan whose
// name looks like it implies more is still just a name, and an install that
// upgraded itself on the strength of one would be granting what nobody sold.
type Plan string

const (
	// PlanNone is a response that names no plan: an install with no key, or a
	// license service older than the field. It is not a plan and it is not a
	// refusal — it means nobody said, and every surface renders the tier alone.
	PlanNone Plan = ""

	PlanFree                Plan = "free"
	PlanTrial               Plan = "trial"
	PlanEnterpriseLimited   Plan = "enterprise_limited"
	PlanEnterpriseUnlimited Plan = "enterprise_unlimited"
)

// Plans is every plan this build can name, cheapest first. **The order is not a
// rank** — nothing compares two plans — it is the order a person reads them in.
var Plans = []Plan{PlanFree, PlanTrial, PlanEnterpriseLimited, PlanEnterpriseUnlimited}

// ParsePlan reads a plan name out of the vendor's answer.
//
// A word this build has never heard of is not a plan, and the answer is
// `PlanNone` with false — the same shape `ParseTier` uses and for a weaker
// version of the same reason. A tier it cannot name must not withdraw anything;
// a plan it cannot name has nothing to withdraw, because nothing reads it.
func ParsePlan(s string) (Plan, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, p := range Plans {
		if string(p) == s {
			return p, true
		}
	}
	return PlanNone, false
}

// Label is the plan's name as a person sees it, and it is the vendor's own
// wording so that the console selling the license and the install holding it
// say the same words.
func (p Plan) Label() string {
	switch p {
	case PlanFree:
		return "Free"
	case PlanTrial:
		return "Trial"
	case PlanEnterpriseLimited:
		return "Enterprise — limited"
	case PlanEnterpriseUnlimited:
		return "Enterprise — unlimited"
	default:
		return ""
	}
}

// Seated reports whether this plan carries a seat number at all.
//
// It is a property of the plan and not a reading of the number, because "no
// ceiling on this plan" and "nobody has agreed a number yet" are both a missing
// figure on the wire and are two completely different sentences to put in front
// of an administrator.
func (p Plan) Seated() bool { return p == PlanFree || p == PlanEnterpriseLimited }

// The two words the vendor uses for whether this install's published URLs are
// probed from outside. `PresenceUnstated` is the third case and is not a word
// on the wire — it is the field being absent.
const (
	PresenceRequired = "required"
	PresenceOff      = "off"
	PresenceUnstated = ""
)
