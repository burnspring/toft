package entitlements

// Tier is what this install has bought, and it is the input to every gate in
// this package rather than a gate of its own.
//
// Four names and no fifth. `trial` is a plan label on an ordinary license with
// a short term rather than a concept of its own, which is the shape the
// vendor's schema already commits to — `expires_at` is nullable so that a trial
// is an ordinary license that ends.
//
// **It is derived and never stored.** Nothing writes a tier to a table and
// nothing caches one at boot: it is resolved from the stored key and the last
// answer on every call, because a stored tier is a fourth fact that can
// disagree with the three it came from and the disagreement is invisible. An
// install whose key was removed at 10:00 is on the demo tier at 10:00.
type Tier string

const (
	// TierDemo is an install with no license key. It is the only tier that
	// makes no outbound request, and the two facts are the same fact: the demo
	// tier is *defined* by the absence of a key, so "off means no outbound
	// request at all" and "this is the demo tier" cannot come apart.
	TierDemo Tier = "demo"

	// TierFree is a key anybody may ask for. It costs nothing, and what it buys
	// the vendor is the end of blindness: an install with no key is invisible —
	// no version, no platform, no count, and no way to tell anyone about a CVE
	// in the runner image they are running.
	TierFree Tier = "free"

	// TierTrial is a paid license with a short term and a plan label.
	TierTrial Tier = "trial"

	// TierPaid is a license somebody bought.
	TierPaid Tier = "paid"
)

// Tiers is every tier, weakest first. The order is the rank.
var Tiers = []Tier{TierDemo, TierFree, TierTrial, TierPaid}

// rank orders the tiers. Unexported because the ordering is an implementation
// detail of AtLeast and of nothing else: a caller comparing two integers is a
// caller who will one day compare them the wrong way round.
func (t Tier) rank() int {
	for i, name := range Tiers {
		if name == t {
			return i
		}
	}
	// A tier this build cannot name is the weakest thing it knows. It never
	// reaches here from a vendor answer — ParseTier refuses an unrecognised
	// word and the caller resolves to the tier already held — so this is the
	// zero value and a typo, and both are demo.
	return 0
}

// AtLeast reports whether this tier reaches min.
//
// Everything a paid license unlocks declares `TierTrial` rather than
// `TierPaid`, because a trial is an ordinary license and has to unlock what it
// is a trial *of*. The rank still orders trial below paid so that a paid-only
// feature is expressible; none is declared today.
func (t Tier) AtLeast(min Tier) bool { return t.rank() >= min.rank() }

// Describe is the tier in a sentence's voice — "the demo tier", "a paid
// license" — so that a refusal reads as English rather than as an enum.
func (t Tier) Describe() string {
	switch t {
	case TierFree:
		return "the free tier"
	case TierTrial:
		return "a trial license"
	case TierPaid:
		return "a paid license"
	default:
		return "the demo tier"
	}
}

// ParseTier reads a tier name from the vendor's answer.
//
// It refuses rather than approximates: a word this build has never heard of is
// not a tier, and the caller's answer to that is the tier the install already
// held. A new word from the vendor must not be able to withdraw anything from
// an install that cannot even name it.
func ParseTier(s string) (Tier, bool) {
	for _, name := range Tiers {
		if string(name) == s {
			return name, true
		}
	}
	return TierDemo, false
}

// AppLimit is how many apps a tier may have, and whether it has a limit at all.
//
// **The numbers are compiled in and the caps bind on the two unlicensed tiers
// only.** A licensed install's counts are reported, warned on and settled at
// renewal — the vendor states the number in the agreement and the check-in
// carries the count against it, which is where a limit somebody signed for
// belongs. Refusing on a paid customer's own contract number is where licensing
// systems start causing outages in other people's datacentres.
//
// Ten because a demo that cannot hold a realistic estate is not a demo; the
// free number is larger because a small team on a free key is the population
// this exists to make visible, and it is the vendor's to revisit.
func AppLimit(t Tier) (limit int, capped bool) {
	switch t {
	case TierDemo:
		return 10, true
	case TierFree:
		return 25, true
	default:
		return 0, false
	}
}
