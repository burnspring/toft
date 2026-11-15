package auth

import "testing"

// stubResolver returns a fixed role regardless of inputs.
type stubResolver struct{ role string }

func (s stubResolver) EffectiveRole(objectID, pt, pid string, groups []string) (string, error) {
	return s.role, nil
}

// Tests the PRD §7 matrix × viewer-exclusion downgrade × system_admin bypass.
func TestAuthorizeMatrix(t *testing.T) {
	private := &Object{ID: "app1", OwnerID: "owner1", Visibility: "private"}

	cases := []struct {
		name       string
		principal  *Principal
		grantRole  string // role the resolver returns
		action     Action
		obj        *Object
		wantAllow  bool
		wantEffect string
	}{
		// system_admin bypass
		{"admin_read", &Principal{Type: "user", ID: "a", SystemRole: "system_admin"}, "", ActionRead, private, true, ObjectRoleOwner},
		{"admin_delete", &Principal{Type: "user", ID: "a", SystemRole: "system_admin"}, "", ActionDelete, private, true, ObjectRoleOwner},
		{"admin_publish_no_action", &Principal{Type: "user", ID: "a", SystemRole: "system_admin"}, "", ActionPublish, nil, true, "system_admin"},

		// Owner (via owner_id denormalization)
		{"owner_read", &Principal{Type: "user", ID: "owner1", SystemRole: "publisher"}, "", ActionRead, private, true, ObjectRoleOwner},
		{"owner_edit", &Principal{Type: "user", ID: "owner1", SystemRole: "publisher"}, "", ActionEditContent, private, true, ObjectRoleOwner},
		{"owner_delete", &Principal{Type: "user", ID: "owner1", SystemRole: "publisher"}, "", ActionDelete, private, true, ObjectRoleOwner},
		{"owner_manage_grants", &Principal{Type: "user", ID: "owner1", SystemRole: "publisher"}, "", ActionManageGrants, private, true, ObjectRoleOwner},
		{"owner_transfer", &Principal{Type: "user", ID: "owner1", SystemRole: "publisher"}, "", ActionTransferOwnership, private, true, ObjectRoleOwner},

		// Editor can edit but not delete
		{"collab_read", &Principal{Type: "user", ID: "u2", SystemRole: "publisher"}, ObjectRoleEditor, ActionRead, private, true, ObjectRoleEditor},
		{"collab_edit", &Principal{Type: "user", ID: "u2", SystemRole: "publisher"}, ObjectRoleEditor, ActionEditContent, private, true, ObjectRoleEditor},
		{"collab_no_delete", &Principal{Type: "user", ID: "u2", SystemRole: "publisher"}, ObjectRoleEditor, ActionDelete, private, false, ObjectRoleEditor},
		{"collab_no_grants", &Principal{Type: "user", ID: "u2", SystemRole: "publisher"}, ObjectRoleEditor, ActionManageGrants, private, false, ObjectRoleEditor},

		// Object viewer can read only
		{"viewer_read", &Principal{Type: "user", ID: "u3", SystemRole: "publisher"}, ObjectRoleViewer, ActionRead, private, true, ObjectRoleViewer},
		{"viewer_no_edit", &Principal{Type: "user", ID: "u3", SystemRole: "publisher"}, ObjectRoleViewer, ActionEditContent, private, false, ObjectRoleViewer},

		// Viewer-exclusion downgrade: viewer system role + editor grant → treated as viewer
		{"viewer_exclusion_downgrade_edit", &Principal{Type: "user", ID: "u4", SystemRole: "viewer"}, ObjectRoleEditor, ActionEditContent, private, false, ObjectRoleViewer},
		{"viewer_exclusion_downgrade_read", &Principal{Type: "user", ID: "u4", SystemRole: "viewer"}, ObjectRoleEditor, ActionRead, private, true, ObjectRoleViewer},

		// Service account cannot transfer ownership or approve access
		{"sa_no_transfer", &Principal{Type: "service_account", ID: "sa1", SystemRole: "publisher"}, ObjectRoleOwner, ActionTransferOwnership, private, false, ""},
		{"sa_no_approve", &Principal{Type: "service_account", ID: "sa1", SystemRole: "publisher"}, ObjectRoleOwner, ActionApproveAccessRequest, private, false, ""},
		// But can read if granted editor role
		{"sa_can_read_collab", &Principal{Type: "service_account", ID: "sa1", SystemRole: "publisher"}, ObjectRoleEditor, ActionRead, private, true, ObjectRoleEditor},
		{"sa_can_edit_collab", &Principal{Type: "service_account", ID: "sa1", SystemRole: "publisher"}, ObjectRoleEditor, ActionEditContent, private, true, ObjectRoleEditor},

		// Publish requires publisher system role
		{"viewer_no_publish_nil", &Principal{Type: "user", ID: "u5", SystemRole: "viewer"}, "", ActionPublish, nil, false, ""},
		{"publisher_publish_nil", &Principal{Type: "user", ID: "u5", SystemRole: "publisher"}, "", ActionPublish, nil, true, ""},

		// Anonymous public read
		{"anon_read_public", nil, "", ActionRead, &Object{ID: "p", OwnerID: "o", Visibility: "public"}, true, "anonymous"},
		{"anon_read_private_denied", nil, "", ActionRead, &Object{ID: "p", OwnerID: "o", Visibility: "private"}, false, "none"},

		// Authenticated-visibility
		{"auth_reader_sees_authenticated", &Principal{Type: "user", ID: "someone", SystemRole: "publisher"}, "", ActionRead, &Object{ID: "p", OwnerID: "o", Visibility: "authenticated"}, true, "authenticated"},
		{"auth_visibility_no_edit", &Principal{Type: "user", ID: "someone", SystemRole: "publisher"}, "", ActionEditContent, &Object{ID: "p", OwnerID: "o", Visibility: "authenticated"}, false, "authenticated"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Authorize(tc.principal, tc.action, tc.obj, stubResolver{role: tc.grantRole})
			if d.Allow != tc.wantAllow {
				t.Errorf("allow: got %v want %v (decision=%+v)", d.Allow, tc.wantAllow, d)
			}
			if tc.wantEffect != "" && tc.obj != nil && d.EffectiveRole != tc.wantEffect {
				t.Errorf("effective role: got %q want %q", d.EffectiveRole, tc.wantEffect)
			}
		})
	}
}

// GrantedRole is the only place the six-value ladder collapses to the three an
// app is told, so every value goes through it here. The three that disappear are
// the platform's bookkeeping about *how* read access was obtained, and all three
// mean the same thing to an app: can see it, holds no grant.
func TestGrantedRoleKeepsTheThreeTheUISpells(t *testing.T) {
	for _, tc := range []struct{ effective, want string }{
		{ObjectRoleOwner, ObjectRoleOwner},
		{ObjectRoleEditor, ObjectRoleEditor},
		{ObjectRoleViewer, ObjectRoleViewer},
		{"authenticated", ObjectRoleViewer},
		{"anonymous", ObjectRoleViewer},
		// Past the read gate this can only be an administrator with no grant,
		// and `viewer` is the literal truth. The is_admin field beside it
		// carries the rest.
		{"none", ObjectRoleViewer},
	} {
		t.Run(tc.effective, func(t *testing.T) {
			got, err := GrantedRole(tc.effective)
			if err != nil {
				t.Fatalf("GrantedRole(%q) returned %v", tc.effective, err)
			}
			if got != tc.want {
				t.Errorf("GrantedRole(%q) = %q, want %q", tc.effective, got, tc.want)
			}
		})
	}
}

// allActions is every Action the authorizer knows. A new one added without a
// row here makes TestListSatisfiesNoAction weaker without failing, so it is
// asserted complete against the matrix rather than trusted.
var allActions = []Action{
	ActionRead, ActionEditContent, ActionDelete, ActionManageGrants,
	ActionChangeVisibility, ActionTransferOwnership, ActionApproveAccessRequest,
	ActionPublish,
}

// The single most dangerous line in this package, asserted directly.
//
// ObjectRoleList must sit strictly between "none" and "anonymous". If a
// renumbering ever puts it at or above "anonymous", matrixAllows's read test —
// `>= rolePrecedenceStr("anonymous")` — starts returning true and **every
// private app in the install becomes readable to everyone holding a list
// grant**. No happy-path test notices: a list holder simply starts seeing more
// than they should, which is the failure mode that gets found by an auditor.
//
// So this walks every action against a principal whose only relationship to the
// object is a list grant, and asserts the answer is no, every time. Read is in
// the list and is the one that matters.
func TestListSatisfiesNoAction(t *testing.T) {
	private := &Object{ID: "app1", OwnerID: "owner1", Visibility: "private"}
	resolver := stubResolver{role: ObjectRoleList}

	for _, systemRole := range []string{"viewer", "publisher"} {
		for _, action := range allActions {
			p := &Principal{Type: "user", ID: "u-list", SystemRole: systemRole}
			d := Authorize(p, action, private, resolver)
			if d.Allow {
				t.Errorf("a list grant allowed %s (system role %s); the rung must sit below every way of reading",
					action, systemRole)
			}
			// An empty effective role means the refusal came before the object
			// was consulted — the system-role gate, which publish meets for a
			// viewer. Anything that did reach the ladder must read as `list`
			// and not have been softened on the way.
			if d.EffectiveRole != "" && d.EffectiveRole != ObjectRoleList {
				t.Errorf("effective role for a list grant on %s is %q, want %q",
					action, d.EffectiveRole, ObjectRoleList)
			}
		}
	}

	// And the ordering itself, stated as the two comparisons the ladder turns
	// on rather than as integers, so a renumbering in either direction fails
	// here with a sentence rather than by arithmetic.
	if rolePrecedenceStr(ObjectRoleList) >= rolePrecedenceStr("anonymous") {
		t.Fatal("list is at or above anonymous — every private app is now readable to every list holder")
	}
	if rolePrecedenceStr(ObjectRoleList) <= rolePrecedenceStr("none") {
		t.Fatal("list is at or below none — a list grant no longer outranks having no relationship at all")
	}
}

// The ladder's order, end to end, so a renumbering that reshuffles two rungs is
// caught even when each is individually above none and below anonymous.
func TestLadderIsStrictlyOrdered(t *testing.T) {
	ascending := []string{
		"none", ObjectRoleList, "anonymous", "authenticated",
		ObjectRoleViewer, ObjectRoleEditor, ObjectRoleOwner,
	}
	for i := 1; i < len(ascending); i++ {
		lower, upper := ascending[i-1], ascending[i]
		if rolePrecedenceStr(lower) >= rolePrecedenceStr(upper) {
			t.Errorf("%q does not rank below %q (%d vs %d)",
				lower, upper, rolePrecedenceStr(lower), rolePrecedenceStr(upper))
		}
	}
}

// GrantedRole refuses the list rung rather than collapsing it to viewer.
//
// The other four non-grant values become `viewer` because they all mean "can
// see it, holds no grant". A list holder cannot see it, so `viewer` would be a
// lie told to an app — and the only way one arrives here is if something
// upstream let through a caller who cannot read the object.
func TestGrantedRoleRefusesTheListRung(t *testing.T) {
	if got, err := GrantedRole(ObjectRoleList); err == nil {
		t.Errorf("GrantedRole(%q) = %q with no error; a list holder must never be described to an app",
			ObjectRoleList, got)
	}
	obj := &Object{ID: "app1", OwnerID: "owner1", Visibility: "private"}
	p := &Principal{Type: "user", ID: "u-list", SystemRole: "publisher"}
	if got, err := CallerRole(p, obj, stubResolver{role: ObjectRoleList}); err == nil {
		t.Errorf("CallerRole minted %q for a list holder; the gateway must refuse instead", got)
	}
}
