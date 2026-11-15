package api

import (
	"net/http/httptest"
	"testing"

	"toft/internal/auth"
	"toft/internal/database"
)

// Who deployed is what toft.toml's storage changes are judged against, so each
// kind of caller has to land as itself — a deploy key must never read as the
// person who created it. A service account is recorded by its owning user,
// whose delegations it is judged through, under its own type.
func TestStampDeployerRecordsEachKindOfCaller(t *testing.T) {
	for _, tc := range []struct {
		name    string
		p       *auth.Principal
		typ, id string
	}{
		{"nobody", nil, "", ""},
		{"a person", &auth.Principal{Type: "user", ID: "u1"}, database.DeployerUser, "u1"},
		{"a person through a personal key", &auth.Principal{Type: "user", ID: "u1", APIKeyID: "k1"}, database.DeployerUser, "u1"},
		{"a service account", &auth.Principal{Type: "service_account", ID: "sa1", OwningUserID: "u1"}, database.DeployerServiceAccount, "u1"},
		{"a deploy key", &auth.Principal{Type: auth.PrincipalTypeDeployKey, DeployKeyID: "dk1"}, database.DeployerDeployKey, "dk1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			if tc.p != nil {
				r = r.WithContext(auth.WithPrincipal(r.Context(), tc.p))
			}
			d := &database.Deployment{}
			stampDeployer(r, d)
			if d.DeployedByType != tc.typ || d.DeployedByID != tc.id {
				t.Errorf("stamped %q/%q, want %q/%q", d.DeployedByType, d.DeployedByID, tc.typ, tc.id)
			}
		})
	}
}
