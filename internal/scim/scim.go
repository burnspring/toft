// Package scim is the wire format of SCIM 2.0 (RFC 7643, RFC 7644), and
// nothing else.
//
// It has no database, no HTTP handlers and no opinion about what a request
// means — it parses what an identity provider sends and renders what one
// expects back. That separation is the point: the protocol has more edge cases
// than the feature does, and every one of them is a place two IdPs disagree.
//
// **What this package supports is a closed list, and the list is the
// specification.** A filter that is not `userName eq "…"`, a PATCH operation
// this product does not implement, a resource it does not serve — each is a
// refusal naming itself, never a best guess. An IdP that receives a clear
// "unsupported" configures around it; one that receives a wrong answer does not
// know to.
package scim

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The schema URNs, which are identifiers rather than URLs and are compared
// exactly. An IdP that sends a different one is talking about a different
// resource.
const (
	SchemaUser       = "urn:ietf:params:scim:schemas:core:2.0:User"
	SchemaGroup      = "urn:ietf:params:scim:schemas:core:2.0:Group"
	SchemaListResp   = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	SchemaPatchOp    = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	SchemaError      = "urn:ietf:params:scim:api:messages:2.0:Error"
	SchemaSPConfig   = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	SchemaResourceTy = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
)

// ContentType is SCIM's own media type. Some IdPs branch on it, and one of them
// treats `application/json` as a protocol violation.
const ContentType = "application/scim+json"

// --- Resources ---

// User is the subset of the SCIM user this product can honestly fill.
//
// Everything here is something the platform already knows. There is no
// `roles`, no `entitlements` and no `groups` — a feed says who exists and
// whether they are active, and what somebody may do stays this product's own.
type User struct {
	Schemas    []string `json:"schemas"`
	ID         string   `json:"id"`
	ExternalID string   `json:"externalId,omitempty"`
	UserName   string   `json:"userName"`
	Name       *Name    `json:"name,omitempty"`
	Emails     []Email  `json:"emails,omitempty"`
	Active     bool     `json:"active"`
	Meta       Meta     `json:"meta"`
}

// Name is SCIM's split name. Only `formatted` is filled: this platform stores
// one display name and splitting it on a space would invent a family name for
// everybody who does not have one where the splitter expects it.
type Name struct {
	Formatted string `json:"formatted,omitempty"`
}

type Email struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary"`
}

// Meta is the resource envelope. `location` is absolute because IdPs follow it.
type Meta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Location     string `json:"location,omitempty"`
}

// ListResponse wraps a query result. `totalResults` of 0 is the correct answer
// to a filter that matched nothing — **not** a 404, which means the *endpoint*
// has nothing at that path. IdPs use the difference to decide whether to create.
type ListResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	ItemsPerPage int      `json:"itemsPerPage"`
	StartIndex   int      `json:"startIndex"`
	Resources    []any    `json:"Resources"`
}

// NewListResponse renders a page. Resources is never null in the JSON: an IdP
// that iterates it without a nil check is common enough that emitting `null`
// is a compatibility bug rather than a purity win.
func NewListResponse(resources []any) ListResponse {
	if resources == nil {
		resources = []any{}
	}
	return ListResponse{
		Schemas:      []string{SchemaListResp},
		TotalResults: len(resources),
		ItemsPerPage: len(resources),
		StartIndex:   1,
		Resources:    resources,
	}
}

// --- Errors ---

// Error is SCIM's error body. `scimType` is the machine-readable half and is
// what an IdP's console renders; `detail` is for the person reading that
// console, so it is a sentence rather than a code.
type Error struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	ScimType string   `json:"scimType,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// The scimType values this product uses. RFC 7644 §3.12 defines more; these are
// the ones a refusal here can honestly be.
const (
	// TypeInvalidFilter — a filter this endpoint does not implement.
	TypeInvalidFilter = "invalidFilter"
	// TypeInvalidValue — a well-formed request whose value is wrong.
	TypeInvalidValue = "invalidValue"
	// TypeInvalidSyntax — the body is not what the operation requires.
	TypeInvalidSyntax = "invalidSyntax"
	// TypeMutability — the change is refused by policy. The last administrator.
	TypeMutability = "mutability"
)

// NewError builds an error body. Status is a string in SCIM, which is not an
// oversight in the spec and is not worth arguing with.
func NewError(status int, scimType, detail string) Error {
	return Error{
		Schemas:  []string{SchemaError},
		Status:   fmt.Sprint(status),
		ScimType: scimType,
		Detail:   detail,
	}
}

// --- Filters ---

// ErrUnsupportedFilter is every filter that is not `userName eq "…"`.
var ErrUnsupportedFilter = errors.New(`only 'userName eq "value"' is supported`)

// ParseUserNameFilter reads the one filter this endpoint implements.
//
// SCIM's filter grammar is large and this product implements a single
// production of it, because a single production is what every IdP actually
// sends before deciding whether to create an account. Supporting a subset and
// **saying so** is the honest shape; the dishonest one is a parser that
// silently ignores the half of an expression it did not understand and returns
// a confidently wrong result.
//
// Accepts, case-insensitively on the operator and the attribute name:
//
//	userName eq "alice@example.com"
//	userName Eq "alice"
//
// Everything else, including `and`, `or`, `co`, `sw` and a bare value with no
// quotes, is ErrUnsupportedFilter.
func ParseUserNameFilter(filter string) (string, error) {
	f := strings.TrimSpace(filter)
	if f == "" {
		return "", ErrUnsupportedFilter
	}

	// Split into three parts on whitespace, keeping the value intact — it may
	// contain spaces inside its quotes.
	attr, rest, ok := strings.Cut(f, " ")
	if !ok || !strings.EqualFold(strings.TrimSpace(attr), "userName") {
		return "", ErrUnsupportedFilter
	}
	op, value, ok := strings.Cut(strings.TrimSpace(rest), " ")
	if !ok || !strings.EqualFold(strings.TrimSpace(op), "eq") {
		return "", ErrUnsupportedFilter
	}

	value = strings.TrimSpace(value)
	// A compound expression reaches here as a value with a trailing clause, and
	// it must be refused rather than truncated to its first term: `userName eq
	// "a" and active eq true` is a narrower question than `userName eq "a"`,
	// and answering the wider one is how a filter silently returns somebody the
	// caller excluded.
	if !strings.HasPrefix(value, `"`) {
		return "", ErrUnsupportedFilter
	}
	closing := strings.Index(value[1:], `"`)
	if closing < 0 {
		return "", ErrUnsupportedFilter
	}
	if strings.TrimSpace(value[closing+2:]) != "" {
		return "", ErrUnsupportedFilter
	}
	inner := value[1 : closing+1]
	if inner == "" {
		return "", ErrUnsupportedFilter
	}
	return inner, nil
}

// --- PATCH ---

// PatchRequest is RFC 7644 §3.5.2's body.
//
// `Operations` is capitalised in the wire format and Go's decoder is
// case-insensitive on field names, so both spellings arrive here — which is
// just as well, because IdPs send both.
type PatchRequest struct {
	Schemas    []string         `json:"schemas"`
	Operations []PatchOperation `json:"Operations"`
}

type PatchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// ErrNoSupportedOperation is a PATCH whose operations are all things this
// endpoint does not implement. Distinct from a malformed body: the request was
// understood and there was nothing in it to do.
var ErrNoSupportedOperation = errors.New("no supported operation in this PATCH")

// ActiveFromPatch finds the `active` value a PATCH sets, if any.
//
// Three shapes reach this, and all three are things real providers send:
//
//	{"op":"replace","value":{"active":false}}      // Okta
//	{"op":"replace","path":"active","value":false} // the RFC's example
//	{"op":"Replace","path":"active","value":"False"} // Microsoft Entra
//
// **The third is why this cannot be a plain bool decode.** Entra sends the
// value as a *string*, capitalised, and a strict decode fails the whole
// operation — which presents as deprovisioning silently never working, on the
// IdP most likely to be sending it. `truthyJSON` handles both, strictly: only
// an affirmative value is true, and anything unrecognised is an error rather
// than a false, because reading an unparseable value as "deactivate" would lock
// accounts on a misunderstanding.
func ActiveFromPatch(req PatchRequest) (bool, error) {
	for _, op := range req.Operations {
		if !strings.EqualFold(op.Op, "replace") && !strings.EqualFold(op.Op, "add") {
			continue
		}
		path := strings.TrimSpace(strings.ToLower(op.Path))

		if path == "active" {
			return truthyJSON(op.Value)
		}
		if path == "" {
			// A pathless operation carries an object of attributes. Only
			// `active` is read; anything else in it is ignored in silence,
			// because this endpoint does not implement those attributes and
			// half-applying an operation is worse than not applying it.
			var attrs map[string]json.RawMessage
			if err := json.Unmarshal(op.Value, &attrs); err != nil {
				continue
			}
			for k, v := range attrs {
				if strings.EqualFold(k, "active") {
					return truthyJSON(v)
				}
			}
		}
	}
	return false, ErrNoSupportedOperation
}

// truthyJSON reads a boolean that may have arrived as a string.
//
// Strict in the direction that matters: an unrecognised value is an error, not
// a false. False deactivates, and deactivating on a value nobody understood is
// how a feed locks somebody out over a typo in another product.
func truthyJSON(raw json.RawMessage) (bool, error) {
	// `null` first, and it is the case a test caught rather than a case that
	// was designed for: Go treats a JSON null as a no-op for most targets, so
	// unmarshalling it into a bool *succeeds* and leaves the zero value. A null
	// `active` would therefore have deactivated the account, silently, on a
	// value that says nothing at all.
	if string(raw) == "null" || len(raw) == 0 {
		return false, errors.New("`active` was null, which says nothing — refusing rather than deactivating")
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("`active` must be true or false, got %s", string(raw))
}

// MemberChange is one add or remove of a member, resolved out of a group PATCH.
type MemberChange struct {
	UserID string
	Add    bool
}

// MembersFromPatch reads the member operations out of a group PATCH.
//
// Two shapes, and the second is the one that surprises:
//
//	{"op":"add","path":"members","value":[{"value":"user-id"}]}
//	{"op":"remove","path":"members[value eq \"user-id\"]"}
//
// **The remove carries its subject inside the path**, as a filter, with no
// value at all — which is what Okta sends and what a naive implementation drops
// on the floor, producing a group that can be added to and never removed from.
// A remove with a `value` array is also accepted, because Entra sends that one.
//
// A `replace` on `members` is deliberately not supported: it means "these are
// now the members", which would let a feed remove memberships an administrator
// wrote by hand — the one thing the provenance rules exist to prevent. It is
// refused by absence rather than by a check, since nothing here matches it.
func MembersFromPatch(req PatchRequest) ([]MemberChange, error) {
	var out []MemberChange
	for _, op := range req.Operations {
		add := strings.EqualFold(op.Op, "add")
		remove := strings.EqualFold(op.Op, "remove")
		if !add && !remove {
			continue
		}
		path := strings.TrimSpace(op.Path)
		lower := strings.ToLower(path)

		// The filtered-path form: members[value eq "id"].
		if strings.HasPrefix(lower, "members[") && strings.HasSuffix(path, "]") {
			inner := path[len("members[") : len(path)-1]
			id, err := valueFromMemberFilter(inner)
			if err != nil {
				return nil, err
			}
			out = append(out, MemberChange{UserID: id, Add: add})
			continue
		}

		if lower != "members" {
			continue
		}
		var members []struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(op.Value, &members); err != nil {
			// A single member sent as an object rather than an array of one.
			var single struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(op.Value, &single); err != nil || single.Value == "" {
				return nil, fmt.Errorf("`members` must be an array of {\"value\": id}")
			}
			members = append(members, single)
		}
		for _, m := range members {
			if m.Value == "" {
				continue
			}
			out = append(out, MemberChange{UserID: m.Value, Add: add})
		}
	}
	if len(out) == 0 {
		return nil, ErrNoSupportedOperation
	}
	return out, nil
}

// valueFromMemberFilter reads the id out of `value eq "user-id"`.
func valueFromMemberFilter(expr string) (string, error) {
	attr, rest, ok := strings.Cut(strings.TrimSpace(expr), " ")
	if !ok || !strings.EqualFold(attr, "value") {
		return "", fmt.Errorf("unsupported member filter %q", expr)
	}
	op, value, ok := strings.Cut(strings.TrimSpace(rest), " ")
	if !ok || !strings.EqualFold(op, "eq") {
		return "", fmt.Errorf("unsupported member filter %q", expr)
	}
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"`)
	if value == "" {
		return "", fmt.Errorf("unsupported member filter %q", expr)
	}
	return value, nil
}
