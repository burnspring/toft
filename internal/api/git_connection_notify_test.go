package api

import (
	"strings"
	"testing"

	"toft/internal/database"
	"toft/internal/sources"
)

// Only a *widening* is announced. Announcing a narrowing too would train an
// administrator to ignore the message, which is the one failure a notification
// like this has.
func TestOnlyAWideningIsAnnounced(t *testing.T) {
	policy := func(mode string, uploads bool) database.SourcePolicy {
		return database.SourcePolicy{
			Policy: sources.Policy{Mode: mode}, UploadsEnabled: uploads,
		}
	}

	for _, c := range []struct {
		name          string
		before, after database.SourcePolicy
		bp, ap        []string
		want          bool
	}{
		{"allowlist to any", policy(sources.ModeAllowlist, false), policy(sources.ModeAny, false), nil, nil, true},
		{"none to allowlist", policy(sources.ModeNone, false), policy(sources.ModeAllowlist, false), nil, nil, true},
		{"any to allowlist", policy(sources.ModeAny, false), policy(sources.ModeAllowlist, false), nil, nil, false},
		{"allowlist to none", policy(sources.ModeAllowlist, false), policy(sources.ModeNone, false), nil, nil, false},
		{"a pattern added", policy(sources.ModeAllowlist, false), policy(sources.ModeAllowlist, false),
			[]string{"github.com/acme/*"}, []string{"github.com/acme/*", "github.com/labs/*"}, true},
		{"a pattern removed", policy(sources.ModeAllowlist, false), policy(sources.ModeAllowlist, false),
			[]string{"github.com/acme/*", "github.com/labs/*"}, []string{"github.com/acme/*"}, false},
		{"uploads on", policy(sources.ModeAny, false), policy(sources.ModeAny, true), nil, nil, true},
		{"uploads off", policy(sources.ModeAny, true), policy(sources.ModeAny, false), nil, nil, false},
		{"nothing changed", policy(sources.ModeAllowlist, true), policy(sources.ModeAllowlist, true),
			[]string{"github.com/acme/*"}, []string{"github.com/acme/*"}, false},
	} {
		got := widenedPolicy(c.before, c.after, c.bp, c.ap)
		if (got != "") != c.want {
			t.Errorf("%s: widenedPolicy = %q, want announced=%v", c.name, got, c.want)
		}
	}

	// And when it does announce, it says what changed rather than what the
	// policy now is — which is the difference between a notice somebody reads
	// and one they filter.
	detail := widenedPolicy(
		policy(sources.ModeAllowlist, false), policy(sources.ModeAllowlist, false),
		[]string{"github.com/acme/*"}, []string{"github.com/acme/*", "github.com/labs/*"})
	if !strings.Contains(detail, "github.com/labs/*") {
		t.Errorf("the detail does not name what was added: %q", detail)
	}
	if strings.Contains(detail, "github.com/acme/*") {
		t.Errorf("the detail restates what did not change: %q", detail)
	}
}

// The widening notice is addressed to the administrators, as every other
// administrator notice is. Unaddressed, it reached nobody on an install without
// an admin group address.
func TestSourceReachWidenedMailsTheAdministrators(t *testing.T) {
	srv, _, fake := newMailTestServer(t)
	seedUser(t, srv.db, "root", database.SystemRoleAdmin)

	srv.notifySourceReachWidened("t", "d")

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages, want 1", len(msgs))
	}
	if len(msgs[0].To) != 1 || msgs[0].To[0] != "root@example.com" {
		t.Errorf("To = %v, want [root@example.com]", msgs[0].To)
	}
}

// With no administrator address on file and no group address, nothing is
// handed to the mailer (the send site warns instead). The built-in
// administrator's placeholder address does not count as one.
func TestSourceReachWidenedWithNobodyToTellSendsNothing(t *testing.T) {
	srv, _, fake := newMailTestServer(t)

	srv.notifySourceReachWidened("t", "d")

	if msgs := fake.messages(); len(msgs) != 0 {
		t.Fatalf("sent %d messages (%v), want none", len(msgs), msgs[0].To)
	}
}
