package auth

import "testing"

// A group-owned object has no owning user, and the member arrives as an owner
// through the resolver. That is the whole of what group ownership changes in
// this package: nothing.
//
// Stated as a test anyway, because "needs no change" is a claim about behaviour
// and the denormalised owner check above it is exactly the sort of shortcut
// that would quietly answer the wrong question if OwnerID's meaning drifted.
func TestGroupOwnerResolvesToOwnerWithNoOwningUser(t *testing.T) {
	groupOwned := &Object{ID: "app1", OwnerID: "", Visibility: "private"}
	member := &Principal{Type: "user", ID: "member", SystemRole: "publisher", Groups: []string{"g1"}}

	got := EffectiveObjectRole(member, groupOwned, stubResolver{role: ObjectRoleOwner})
	if got != ObjectRoleOwner {
		t.Fatalf("effective role = %q, want owner", got)
	}
	if d := Authorize(member, ActionTransferOwnership, groupOwned, stubResolver{role: ObjectRoleOwner}); !d.Allow {
		t.Fatalf("a member of the owning group may not transfer: %+v", d)
	}
	if d := Authorize(member, ActionDelete, groupOwned, stubResolver{role: ObjectRoleOwner}); !d.Allow {
		t.Fatalf("a member of the owning group may not delete: %+v", d)
	}
}

// The clamp that survived the removal of the group clamp.
//
// database.EffectiveRole no longer holds a group-derived role down to viewer —
// a group has to be able to confer owner now. The viewer-exclusion rule is a
// *different* rule that happened to sit next to it: a user whose system role is
// viewer never holds owner or editor, whatever any grant says. It applies
// to direct grants too, and after ADR §51 it protects more than it used to,
// because a group can now hand out the role it is protecting against.
func TestSystemViewerInOwningGroupIsStillAViewer(t *testing.T) {
	groupOwned := &Object{ID: "app1", OwnerID: "", Visibility: "private"}
	viewer := &Principal{Type: "user", ID: "v", SystemRole: "viewer", Groups: []string{"g1"}}

	for _, granted := range []string{ObjectRoleOwner, ObjectRoleEditor} {
		got := EffectiveObjectRole(viewer, groupOwned, stubResolver{role: granted})
		if got != ObjectRoleViewer {
			t.Errorf("a system viewer granted %q through a group resolves to %q, want viewer", granted, got)
		}
	}

	// And the downgrade is not cosmetic — it has to actually refuse the write.
	if d := Authorize(viewer, ActionEditContent, groupOwned, stubResolver{role: ObjectRoleOwner}); d.Allow {
		t.Fatal("a system viewer in the owning group was allowed to edit")
	}
	if d := Authorize(viewer, ActionManageGrants, groupOwned, stubResolver{role: ObjectRoleOwner}); d.Allow {
		t.Fatal("a system viewer in the owning group was allowed to manage grants")
	}
	if d := Authorize(viewer, ActionRead, groupOwned, stubResolver{role: ObjectRoleOwner}); !d.Allow {
		t.Fatal("a system viewer in the owning group cannot read the app at all")
	}
}
