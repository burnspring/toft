package api

import (
	"strings"
	"testing"

	"toft/internal/appint"
)

// TestNoCredentialPrefixIsAPrefixOfAnother holds the property every
// classification in this package leans on: a secret is sorted into its class
// by HasPrefix, in whatever order the checks happen to run, so no prefix may be
// the start of another. `toft_` and `toftu_` differ at the fifth character,
// which is what makes `toft_` safe to check first — the same reason `lp_` and
// `lpu_` were, held by a test rather than by luck (ADR §111).
func TestNoCredentialPrefixIsAPrefixOfAnother(t *testing.T) {
	prefixes := map[string]string{
		"retired app key": retiredAppKeyPrefix,
		"personal key":    userKeySecretPrefix,
		"deploy key":      deployKeySecretPrefix,
		"app token":       appTokenSecretPrefix,
	}
	if appTokenSecretPrefix != appint.TokenPrefix {
		t.Errorf("the app token is minted as %q and authenticated as %q", appint.TokenPrefix, appTokenSecretPrefix)
	}
	for a, pa := range prefixes {
		if !strings.HasPrefix(pa, "toft") || !strings.HasSuffix(pa, "_") {
			t.Errorf("%s prefix %q is not of the form toft…_", a, pa)
		}
		for b, pb := range prefixes {
			if a != b && strings.HasPrefix(pb, pa) {
				t.Errorf("%s prefix %q is a prefix of the %s prefix %q: a %s would classify as a %s", a, pa, b, pb, b, a)
			}
		}
	}
}
