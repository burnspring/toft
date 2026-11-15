package scim

import (
	"encoding/json"
	"errors"
	"testing"
)

// The tests that matter here are the interop ones. Every failure below is a
// real identity provider sending something a strict reading rejects, and the
// symptom in each case is the same: deprovisioning that silently never happens,
// on an install where somebody believes it does.

// Entra sends `active` as a capitalised *string*. A strict bool decode fails
// the operation, which presents as the feed working for reads and never
// deactivating anybody — on the IdP most likely to be sending it.
func TestActiveAcceptsEveryShapeAProviderSends(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"okta pathless object", `{"Operations":[{"op":"replace","value":{"active":false}}]}`, false},
		{"rfc example", `{"Operations":[{"op":"replace","path":"active","value":false}]}`, false},
		{"entra string", `{"Operations":[{"op":"Replace","path":"active","value":"False"}]}`, false},
		{"entra string true", `{"Operations":[{"op":"Replace","path":"active","value":"True"}]}`, true},
		{"lowercase string", `{"Operations":[{"op":"replace","path":"active","value":"true"}]}`, true},
		{"reactivate", `{"Operations":[{"op":"replace","value":{"active":true}}]}`, true},
		{"add rather than replace", `{"Operations":[{"op":"add","path":"active","value":true}]}`, true},
		{"lowercase operations key", `{"operations":[{"op":"replace","path":"active","value":false}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req PatchRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			got, err := ActiveFromPatch(req)
			if err != nil {
				t.Fatalf("ActiveFromPatch: %v", err)
			}
			if got != tc.want {
				t.Errorf("active = %v, want %v", got, tc.want)
			}
		})
	}
}

// An unrecognised value is an error, never a false. Reading something nobody
// understood as "deactivate" is how a feed locks people out over a typo in
// another product.
func TestAnUnreadableActiveIsRefusedRatherThanReadAsFalse(t *testing.T) {
	for _, body := range []string{
		`{"Operations":[{"op":"replace","path":"active","value":"yes"}]}`,
		`{"Operations":[{"op":"replace","path":"active","value":0}]}`,
		`{"Operations":[{"op":"replace","path":"active","value":null}]}`,
	} {
		var req PatchRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decoding %s: %v", body, err)
		}
		if _, err := ActiveFromPatch(req); err == nil {
			t.Errorf("%s was accepted; an unparseable active must refuse, not deactivate", body)
		}
	}
}

// A PATCH this endpoint does not implement is told so, rather than silently
// doing nothing and answering 200.
func TestAPatchWithNothingSupportedSaysSo(t *testing.T) {
	var req PatchRequest
	_ = json.Unmarshal([]byte(`{"Operations":[{"op":"replace","path":"displayName","value":"New"}]}`), &req)
	if _, err := ActiveFromPatch(req); !errors.Is(err, ErrNoSupportedOperation) {
		t.Errorf("err = %v, want ErrNoSupportedOperation", err)
	}
}

// Okta puts the subject of a member removal inside the *path*, as a filter,
// with no value at all. An implementation that reads only `value` produces a
// group that can be added to and never removed from — which is a permission
// that only ever grows.
func TestMemberRemovalCarriedInThePathIsRead(t *testing.T) {
	var req PatchRequest
	body := `{"Operations":[{"op":"remove","path":"members[value eq \"user-123\"]"}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	changes, err := MembersFromPatch(req)
	if err != nil {
		t.Fatalf("MembersFromPatch: %v", err)
	}
	if len(changes) != 1 || changes[0].UserID != "user-123" || changes[0].Add {
		t.Errorf("changes = %+v, want one removal of user-123", changes)
	}
}

func TestMemberOperationShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []MemberChange
	}{
		{
			"add an array",
			`{"Operations":[{"op":"add","path":"members","value":[{"value":"a"},{"value":"b"}]}]}`,
			[]MemberChange{{UserID: "a", Add: true}, {UserID: "b", Add: true}},
		},
		{
			"add a bare object",
			`{"Operations":[{"op":"add","path":"members","value":{"value":"a"}}]}`,
			[]MemberChange{{UserID: "a", Add: true}},
		},
		{
			"remove with a value array",
			`{"Operations":[{"op":"remove","path":"members","value":[{"value":"a"}]}]}`,
			[]MemberChange{{UserID: "a", Add: false}},
		},
		{
			"add and remove in one request",
			`{"Operations":[{"op":"add","path":"members","value":[{"value":"a"}]},` +
				`{"op":"remove","path":"members[value eq \"b\"]"}]}`,
			[]MemberChange{{UserID: "a", Add: true}, {UserID: "b", Add: false}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req PatchRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			got, err := MembersFromPatch(req)
			if err != nil {
				t.Fatalf("MembersFromPatch: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("changes = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("changes[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// `replace` on members means "these are now the members", which would let a
// feed remove memberships an administrator wrote by hand. It is refused by
// there being nothing that matches it, and this says that is deliberate.
func TestReplacingTheWholeMemberListIsNotSupported(t *testing.T) {
	var req PatchRequest
	_ = json.Unmarshal([]byte(`{"Operations":[{"op":"replace","path":"members","value":[{"value":"a"}]}]}`), &req)
	if _, err := MembersFromPatch(req); !errors.Is(err, ErrNoSupportedOperation) {
		t.Errorf("err = %v, want ErrNoSupportedOperation — a wholesale replace could remove a manual membership", err)
	}
}

// One filter is implemented and everything else is refused by name. A parser
// that took the first term of a compound expression would answer a *wider*
// question than the caller asked, and return somebody they excluded.
func TestOnlyTheUserNameEqualsFilterIsAccepted(t *testing.T) {
	ok := map[string]string{
		`userName eq "alice"`:             "alice",
		`userName Eq "alice@example.com"`: "alice@example.com",
		`  username   eq   "alice"  `:     "alice",
		`userName eq "with space"`:        "with space",
	}
	for filter, want := range ok {
		got, err := ParseUserNameFilter(filter)
		if err != nil {
			t.Errorf("ParseUserNameFilter(%q): %v", filter, err)
			continue
		}
		if got != want {
			t.Errorf("ParseUserNameFilter(%q) = %q, want %q", filter, got, want)
		}
	}

	for _, bad := range []string{
		"",
		`userName eq alice`,                  // unquoted
		`userName co "ali"`,                  // contains
		`externalId eq "x"`,                  // another attribute
		`userName eq "a" and active eq true`, // compound — narrower than we would answer
		`userName eq "a" or userName eq "b"`, // compound
		`userName eq ""`,                     // empty subject
		`userName eq "unterminated`,          // malformed
	} {
		if got, err := ParseUserNameFilter(bad); err == nil {
			t.Errorf("ParseUserNameFilter(%q) = %q, want a refusal", bad, got)
		}
	}
}

// A filter that matched nothing is an empty list, not a 404 — IdPs use the
// difference to decide whether to create an account, and a 404 there reads as
// "this endpoint is broken" rather than "this person is not here".
func TestAnEmptyListIsWellFormed(t *testing.T) {
	resp := NewListResponse(nil)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back["totalResults"] != float64(0) {
		t.Errorf("totalResults = %v, want 0", back["totalResults"])
	}
	res, ok := back["Resources"].([]any)
	if !ok {
		t.Fatalf("Resources = %v (%T), want an empty array and never null", back["Resources"], back["Resources"])
	}
	if len(res) != 0 {
		t.Errorf("Resources = %v, want empty", res)
	}
}
