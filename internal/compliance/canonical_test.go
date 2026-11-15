package compliance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// RFC 8785 §3.2.2.3's own example, byte for byte: escaping, number
// serialization and key order in one document.
func TestCanonicalMatchesTheRFCExample(t *testing.T) {
	in := `{"numbers":[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001],` +
		`"string":"\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",` +
		`"literals":[null,true,false]}`
	want := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],` +
		`"string":"€$\u000f\nA'B\"\\\\\"/"}`
	got, err := Canonical([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("canonical form:\n got  %s\n want %s", got, want)
	}
}

// §3.2.3: keys sort by UTF-16 code units, which puts an astral character
// before U+FB33 although its code point is larger.
func TestCanonicalSortsKeysByUTF16(t *testing.T) {
	in := `{"\u20ac":1,"\r":2,"\ufb33":3,"1":4,"\ud83d\ude00":5,"\u0080":6,"\u00f6":7}`
	got, err := Canonical([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	dec := json.NewDecoder(strings.NewReader(string(got)))
	_, _ = dec.Token()
	for dec.More() {
		k, _ := dec.Token()
		order = append(order, k.(string))
		_, _ = dec.Token()
	}
	want := []string{"\r", "1", "\u0080", "\u00f6", "\u20ac", "\U0001F600", "\ufb33"}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Errorf("key order %q, want %q", order, want)
	}
}

// The ECMAScript number rule at its edges.
func TestES6Number(t *testing.T) {
	for f, want := range map[float64]string{
		0: "0", 1: "1", -1: "-1", 4.5: "4.5", 0.002: "0.002", 0.000001: "0.000001",
		1e-7: "1e-7", 1e21: "1e+21", 1e23: "1e+23", 1e20: "100000000000000000000",
		9007199254740992:        "9007199254740992",
		295147905179352825856.0: "295147905179352830000",
		5e-324:                  "5e-324",
		1.7976931348623157e308:  "1.7976931348623157e+308",
		-1.5e-9:                 "-1.5e-9",
	} {
		if got := es6Number(f); got != want {
			t.Errorf("es6Number(%v) = %s, want %s", f, got, want)
		}
	}
}

// **The claim the documents make, tested as a stranger would.** A reader with
// the file and Python's standard library — no Go types, no knowledge of how the
// file was escaped — recomputes the digest. The member carries what used to
// break that: `<`, `&` and `>`, which encoding/json escapes, a non-ASCII letter,
// U+2028, and a control character.
func TestTheExportDigestRecomputesWithoutThisBinary(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	member := map[string]any{
		"collected_at": "2026-10-25T00:00:00Z",
		"html":         "<a href=\"x?a=1&b=2\">é</a>",
		"separators":   "line\u2028para\u2029end\x01\ttab",
		"counts":       map[string]int{"ok": 12, "refused": 0},
		"nested":       []any{nil, true, "z"},
	}
	raw, err := freeze("toft-test", member)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `\u003c`) {
		t.Fatal("the fixture no longer exercises encoding/json's HTML escaping")
	}
	script := `import sys, json, hashlib
doc = json.load(sys.stdin)
body = json.dumps(doc["report"], sort_keys=True, separators=(",", ":"), ensure_ascii=False)
print(hashlib.sha256(body.encode()).hexdigest() == doc["sha256"])`
	cmd := exec.Command(py, "-I", "-c", script)
	cmd.Stdin = strings.NewReader(string(raw))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python3: %v", err)
	}
	if strings.TrimSpace(string(out)) != "True" {
		t.Errorf("Python's standard library does not recompute the digest from the file:\n%s", raw)
	}
	var doc struct {
		Digest string `json:"sha256"`
		Rule   string `json:"sha256_rule"`
		Format int    `json:"format"`
	}
	_ = json.Unmarshal(raw, &doc)
	if doc.Rule != DigestRule || doc.Format != ExportFormat {
		t.Errorf("the file must name its rule and format, got %+v", doc)
	}
	canon, _ := Canonical(mustJSON(t, member))
	if sum := sha256.Sum256(canon); hex.EncodeToString(sum[:]) != doc.Digest {
		t.Error("the digest is not over the canonical member")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
