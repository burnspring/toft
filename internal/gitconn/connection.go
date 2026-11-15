package gitconn

import (
	"strings"

	"toft/internal/database"
)

// What a connection's credential *is*, in the words the admin page uses.
//
// The weaker auth method gives up three things — provenance, reach and expiry —
// and this file is where two of them are turned into sentences. Stating them on
// the page rather than only in a design document is the condition on which the
// token method exists at all: an administrator choosing it should be able to
// see what they chose.

// ClassifyCredential names the kind of credential a stored secret is, from its
// own prefix.
//
// **The prefix is the whole mechanism, and that is the point.** GitHub spells
// a fine-grained token `github_pat_` and a classic one `ghp_`, and the
// difference is exactly the reach question: a fine-grained token can be scoped
// to selected repositories, a classic one reaches everything its owner can see.
// Asking an administrator to declare which they pasted would be a field they
// could get wrong about a value the platform is holding.
//
// A classic token is described rather than refused (Q10). It is the only kind
// some GitHub Enterprise Server versions can issue, and coverage bounds what
// can be attached either way. What is given up is that an install which would
// rather refuse has no way to say so.
func ClassifyCredential(kind, method, secret string) string {
	if method == database.GitAuthApp {
		return database.GitCredApp
	}
	switch {
	case kind == database.GitConnGitLab:
		return database.GitCredGroup
	case strings.HasPrefix(secret, "github_pat_"):
		return database.GitCredFineGrained
	case strings.HasPrefix(secret, "ghp_"):
		return database.GitCredClassic
	default:
		return ""
	}
}

// ReachSentence is what the admin page says this connection's credential can
// see. Empty when the platform genuinely does not know, which is better than a
// confident sentence about a token it cannot classify.
func ReachSentence(c *database.GitConnection) string {
	switch c.CredentialKind {
	case database.GitCredApp:
		return "The repositories this App is installed on, which GitHub enforces as well as this install does."
	case database.GitCredFineGrained:
		return "Whatever this fine-grained token was scoped to when it was created."
	case database.GitCredClassic:
		return "Everything the account that created this token can see — a classic token cannot be scoped to " +
			"particular repositories. What apps may be attached to is bounded by this connection's coverage instead."
	case database.GitCredGroup:
		return "The group this token belongs to. It is the group's rather than a person's, so it does not stop " +
			"working when somebody leaves."
	default:
		return ""
	}
}
