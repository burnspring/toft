package builder

import (
	"strings"
	"testing"

	"toft/internal/gitconn"
)

// Test 6. **The three combinations in D6 are indistinguishable below
// `Credential`.**
//
// A GitHub App that mints, a GitHub token that is held, and a GitLab token that
// is held are three different stories above the struct and one story below it.
// The claim is worth paying for rather than asserting, because the `mint` path
// lands beside the `hold` path with no abstraction above either — nothing in the
// type system stops a later change making the builder branch on how the
// credential was obtained, and the first symptom would be two different
// sentences for one failure.
//
// This test lives in `internal/builder` deliberately: it is the *consumer* that
// must not be able to tell, so the consumer is where the property is stated.
func TestNothingBelowCredentialKnowsHowItWasObtained(t *testing.T) {
	repo := "https://git.example.com/acme/reports"
	const conn = "Acme"

	// All three arrive here as one struct with one source. The only field that
	// differs is the token, which is a secret and reaches nothing but
	// GitAuthEnv.
	combinations := []struct {
		name string
		cred gitconn.Credential
	}{
		{"github app, minted", gitconn.Credential{
			Token: "ghs_minted_for_one_repo", Source: gitconn.SourceConnection, Connection: conn}},
		{"github token, held", gitconn.Credential{
			Token: "github_pat_held", Source: gitconn.SourceConnection, Connection: conn}},
		{"gitlab group token, held", gitconn.Credential{
			Token: "glpat_held", Source: gitconn.SourceConnection, Connection: conn}},
	}

	for _, class := range []GitFailure{
		GitFailureAuth, GitFailureNotFound, GitFailureBranch, GitFailureNetwork, GitFailureUnknown,
	} {
		var first string
		for i, c := range combinations {
			got := class.Explain(repo, "main", c.cred)
			if i == 0 {
				first = got
				continue
			}
			if got != first {
				t.Errorf("%s: %q explains differently from %q:\n\t%s\n\t%s",
					class, c.name, combinations[0].name, first, got)
			}
		}
		// And the sentence is the connection's, not a token's — which is the
		// thing all three share and the reason they can share it.
		if class == GitFailureAuth && !strings.Contains(first, conn) {
			t.Errorf("the auth sentence does not name the connection: %q", first)
		}
	}

	// The environment git actually gets differs only in the credential, and the
	// credential reaches only git's own config channel.
	for _, c := range combinations {
		env := gitEnv(c.cred.Token)
		if env["GIT_CONFIG_COUNT"] != "2" {
			t.Errorf("%s: GIT_CONFIG_COUNT = %q", c.name, env["GIT_CONFIG_COUNT"])
		}
		if env["GIT_TERMINAL_PROMPT"] != "0" {
			t.Errorf("%s: a minted credential changed the prompt setting", c.name)
		}
	}
}
