package database

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// FrameAncestors is the list of origins allowed to embed an app.
//
// One column, two shapes, and the type is what keeps them from drifting: on the
// wire and in the UI it is an array, because a list of origins edited by a
// person is a list; in the database and in the header it is the space-separated
// text CSP itself uses, so **the value stored is the value sent**. A conversion
// that happened at the edges instead would give three places to disagree about
// what an empty list is.
type FrameAncestors []string

// Scan reads the stored text. An empty string is an empty list and never a
// list containing one empty origin, which is a distinction `strings.Split`
// gets wrong on its own.
func (f *FrameAncestors) Scan(value any) error {
	*f = nil
	if value == nil {
		return nil
	}
	var text string
	switch v := value.(type) {
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return fmt.Errorf("cannot scan %T into FrameAncestors", value)
	}
	*f = FrameAncestors(strings.Fields(text))
	return nil
}

// Value writes the space-separated form. Never NULL: the column is NOT NULL,
// and "no origins" is a value rather than an absence.
func (f FrameAncestors) Value() (driver.Value, error) { return f.String(), nil }

// String is the CSP source list, and is what the proxy appends to
// `frame-ancestors`.
func (f FrameAncestors) String() string { return strings.Join(f, " ") }

// MarshalJSON emits `[]` rather than `null` for the empty list, because a
// settings form that reads null has to decide what that meant.
func (f FrameAncestors) MarshalJSON() ([]byte, error) {
	if f == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(f))
}

// MaxFrameAncestors bounds the list. Not a security limit — a header of any
// length is enforced the same way — but a settings field that accepts a
// thousand origins is a settings field somebody will paste a log file into.
const MaxFrameAncestors = 20

// ParseFrameAncestors validates and canonicalizes what a person typed.
//
// Exact origins only: a scheme, a host, an optional port. **No paths**, because
// CSP ignores them and a person who typed one believes something false about
// what they configured. **No bare hostnames**, because `example.com` is a
// relative URL to a browser and the difference between http and https is the
// difference between an embed anybody on the network can forge and one they
// cannot. **No `*`**, because "anyone may frame this" is what the default
// exists to refuse and a wildcard hidden in a list is not the way to say it.
//
// A wildcard *label* — `https://*.example.com` — is allowed: it names one
// domain somebody controls, which is a different statement from naming none.
func ParseFrameAncestors(in []string) (FrameAncestors, error) {
	var out FrameAncestors
	seen := map[string]bool{}
	for _, raw := range in {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			continue
		}
		if len(out) >= MaxFrameAncestors {
			return nil, fmt.Errorf("at most %d origins", MaxFrameAncestors)
		}
		if origin == "*" || strings.HasPrefix(origin, "*.") {
			return nil, fmt.Errorf("%q allows any site; name the origins that may embed this app", origin)
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("%q is not an origin — it needs a scheme and a host, like https://intranet.example.com", origin)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("%q must be http or https", origin)
		}
		if u.Path != "" && u.Path != "/" {
			return nil, fmt.Errorf("%q has a path; a frame-ancestors source is an origin and nothing more", origin)
		}
		if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("%q is not an origin — no query, fragment or credentials", origin)
		}
		// The host goes into a CSP header verbatim, and url.Parse is not a
		// hostname grammar: it hands back `a.com;report-uri` as a Host, and a
		// `;` there ends the directive and starts one of the editor's choosing.
		// So nothing outside a host and port's own characters gets through.
		if !originHostChars(u.Host) {
			return nil, fmt.Errorf("%q is not an origin — a host may contain only letters, digits, dots, hyphens and a port", origin)
		}
		// The host, checked for the one wildcard CSP does allow.
		host := u.Host
		if strings.HasPrefix(host, "*.") && len(host) > 2 {
			host = host[2:]
			if strings.Contains(host, "*") {
				return nil, fmt.Errorf("%q has more than one wildcard", origin)
			}
			host = "*." + host
		} else if strings.Contains(host, "*") {
			return nil, fmt.Errorf("%q may only wildcard a leading label", origin)
		}
		canonical := u.Scheme + "://" + host
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return out, nil
}

// originHostChars reports whether host is built only from what a hostname, a
// wildcard label and a port are made of: letters, digits, `.`, `-`, `*` and
// one `:`. Bracketed IPv6 literals are not accepted; nobody frames an app from
// one.
func originHostChars(host string) bool {
	if strings.Count(host, ":") > 1 {
		return false
	}
	for _, c := range host {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '*', c == ':':
		default:
			return false
		}
	}
	return true
}
