package database_test

import (
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
)

// An email-mode exchange resolves an address only where somebody verified it,
// or an administrator set it on an account with no external identity — never
// to an account whose address came from an unverified assertion (security
// review 2026-11-10, A-2).
func TestAnEmailExchangeMatchesOnlyAVerifiedAddress(t *testing.T) {
	db := dbtest.New(t)
	now := time.Now()
	claimant := mkUser(t, db, "claimant", "viewer", now, nil, nil, false)
	verified := mkUser(t, db, "verified", "viewer", now, nil, nil, false)
	admSet := mkUser(t, db, "adminset", "viewer", now, nil, nil, false)
	for _, row := range []struct {
		user, email string
		ok          bool
	}{
		{claimant, "claimant@example.com", false},
		{verified, "verified@example.com", true},
	} {
		if _, err := db.Conn().Exec(`INSERT INTO user_identities (provider_id, subject, user_id, email, email_verified)
			VALUES ('okta', ?, ?, ?, ?)`, "sub-"+row.user, row.user, row.email, row.ok); err != nil {
			t.Fatal(err)
		}
	}
	issuer := &database.TrustedIssuer{SubjectClaim: database.SubjectClaimEmail}
	for email, want := range map[string]string{
		"claimant@example.com": "",
		"VERIFIED@example.com": verified,
		"adminset@example.com": admSet,
	} {
		got, err := db.FindUserForIssuer(issuer, email)
		if err != nil || got != want {
			t.Errorf("%s resolved to %q (%v), want %q", email, got, err, want)
		}
	}
}
