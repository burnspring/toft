package api

import (
	"context"
	"fmt"
	"time"

	"toft/internal/database"
)

// checkGitConnections is about the credential rather than the connection: a
// GitHub App installation token or a PAT expires, auto-deploy stops, and the
// first evidence anybody gets is a developer's failed deploy. The row that
// knows — LastError, CredentialExpiresAt — is on an admin page nobody visits
// weekly.
//
// **One row for all of them.** An install with six connections would otherwise
// own six rows on a page that has to stay scannable; the count is the row and
// the names are in the detail. The same reasoning as the storage resources and
// the scheduled tasks.
//
// **A disabled connection is excluded, not reported healthy.** Disable is
// immediate and unrefusable, so a disabled connection holding an expired token
// is not a finding about anything.
//
// Nothing in the detail is a credential: an id, a display name, an app count
// and a date, plus the git host's own words in LastError. The connection struct
// holds three ciphertexts and three *Set booleans, and none of the six is read
// here.
func (s *Server) checkGitConnections(_ context.Context, rp *checkReport) {
	conns, err := s.db.ListGitConnections()
	if err != nil {
		rp.add("git_connections", "Clone credentials", checkWarn, dbReadFailed("git_connections", "the git connections", err))
		return
	}

	var enabled []database.GitConnection
	for _, c := range conns {
		if c.Enabled {
			enabled = append(enabled, c)
		}
	}
	if len(enabled) == 0 {
		rp.add("git_connections", "Clone credentials", checkSkip,
			"No connected git host is enabled.")
		return
	}

	now := time.Now()
	apps := 0
	var expired, expiring, failing []string
	for _, c := range enabled {
		apps += c.AppCount
		if c.CredentialExpiresAt != nil {
			when := c.CredentialExpiresAt.UTC().Format("2 Jan 2006")
			switch until := c.CredentialExpiresAt.Sub(now); {
			case until < 0:
				expired = append(expired, fmt.Sprintf("%s (%d app(s), expired %s)", c.DisplayName, c.AppCount, when))
			case until < credentialExpiryWarning:
				expiring = append(expiring, fmt.Sprintf("%s (%d app(s), expires %s)", c.DisplayName, c.AppCount, when))
			}
		}
		// A connection with no recorded expiry is not a finding: a GitHub App's
		// mint is short-lived by construction and there is no date to count
		// down to. Its last error still is.
		if c.LastErrorAt != nil && (c.LastOKAt == nil || c.LastErrorAt.After(*c.LastOKAt)) {
			failing = append(failing, fmt.Sprintf("%s: %s", c.DisplayName, c.LastError))
		}
	}

	switch {
	case len(expired) > 0:
		// `fail`, and the app count is the number that matters: nothing deploys
		// through an expired credential, and the question an administrator has
		// is how much of the estate that is.
		rp.add("git_connections", "Clone credentials", checkFail,
			"Expired, and nothing deploys through it: "+list(expired)+".")
	case len(expiring) > 0 || len(failing) > 0:
		var parts []string
		if len(expiring) > 0 {
			parts = append(parts, "expiring within "+humanDuration(credentialExpiryWarning)+": "+list(expiring))
		}
		if len(failing) > 0 {
			parts = append(parts, "last attempt failed — "+list(failing))
		}
		rp.add("git_connections", "Clone credentials", checkWarn, sentence(parts))
	default:
		rp.add("git_connections", "Clone credentials", checkPass,
			fmt.Sprintf("%d connection(s), %d app(s), all minting.", len(enabled), apps))
	}
}
