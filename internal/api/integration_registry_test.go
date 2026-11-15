package api

import (
	"testing"

	"toft/internal/appint"
	"toft/internal/integrations"
)

// The registry half of "the list is the specification".
//
// internal/appint asserts that every kind is completely *described*; a kind can
// pass that and still have nowhere to go, which is the same silence one step
// later — it renders in the admin UI, connects cleanly, attaches, accepts a
// send, and the ledger records "no transport for %q" in a row nobody is
// watching. So this walks the catalog against the registry the wiring layer
// builds.
//
// It builds the registry on a bare Server on purpose. Every transport is a leaf
// with no state of its own, or an adapter that checks for its service at send
// time, so the map is the same map whether or not this install has a relay —
// and a test that needed a whole platform underneath it would be a test nobody
// runs.
func TestEveryKindHasATransport(t *testing.T) {
	registry := (&Server{}).buildTransports()

	for i := range appint.Known {
		kind := &appint.Known[i]
		t.Run(kind.Name, func(t *testing.T) {
			transport, ok := registry[kind.Name]
			if !ok || transport == nil {
				t.Fatalf("%q is in the catalog and has no transport — a send through it "+
					"would be accepted, recorded, queued and then fail with a message "+
					"nobody reads", kind.Name)
			}

			// The descriptor and the transport have to agree about whether
			// there is a menu. A kind that claims to enumerate and cannot would
			// render a picker that always errors; one that enumerates without
			// claiming it hides a menu somebody built.
			_, enumerates := transport.(integrations.Enumerator)
			if kind.Enumerates != enumerates {
				t.Errorf("the descriptor says Enumerates=%v and the transport says %v",
					kind.Enumerates, enumerates)
			}

			// The same for tokens: a token action with no issuer is a switch
			// that turns on a 503, and an issuer with no action is a mint path
			// nothing gates.
			_, issues := transport.(integrations.Issuer)
			if kind.IssuesTokens() != issues {
				t.Errorf("the descriptor says IssuesTokens=%v and the transport says %v",
					kind.IssuesTokens(), issues)
			}
		})
	}

	// And nothing in the registry that is not in the catalog: a transport for a
	// kind nobody can name is dead code that looks like a feature.
	for name := range registry {
		if appint.Lookup(name) == nil {
			t.Errorf("the registry has a transport for %q, which is not in the catalog", name)
		}
	}
}
