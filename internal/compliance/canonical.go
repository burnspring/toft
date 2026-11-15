package compliance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Canonical is RFC 8785, the JSON Canonicalization Scheme: what the export's
// digest is taken over.
//
// **A published rule, so the reader does not need this binary.** The digest
// used to be over `encoding/json`'s own bytes, and reproducing those meant
// knowing that Go escapes `<`, `>` and `&` and writes keys in struct order —
// "recomputable from the file alone" was true only for a reader who already
// knew Go. JCS is independent of whitespace, key order and escaping, has
// libraries in every language, and for a document with ASCII keys and integer
// numbers is exactly Python's `json.dumps(v, sort_keys=True,
// separators=(",", ":"), ensure_ascii=False)`.
func Canonical(doc []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := writeCanonical(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil || math.IsInf(f, 0) {
			return fmt.Errorf("canonical JSON: %q is not an IEEE 754 double", string(v))
		}
		b.WriteString(es6Number(f))
	case string:
		writeCanonicalString(b, v)
	case []any:
		b.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		// UTF-16 code units, not bytes: the two disagree past the BMP.
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, k)
			b.WriteByte(':')
			if err := writeCanonical(b, v[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("canonical JSON: unexpected %T", v)
	}
	return nil
}

func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// writeCanonicalString escapes only what JSON requires: the quote, the
// backslash and the controls, the five with short forms written short.
func writeCanonicalString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// es6Number is ECMAScript's Number.prototype.toString, which RFC 8785 adopts:
// the shortest digits that round-trip, placed by the exponent.
func es6Number(f float64) string {
	if f == 0 {
		return "0" // and -0 too
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// "d.ddde±XX": the shortest round-tripping digits and their exponent.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	x, _ := strconv.Atoi(exp)
	k, n := len(digits), x+1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	es := "+"
	if n-1 < 0 {
		es = "-"
	}
	frac := ""
	if k > 1 {
		frac = "." + digits[1:]
	}
	return sign + digits[:1] + frac + "e" + es + strconv.Itoa(abs(n-1))
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}
