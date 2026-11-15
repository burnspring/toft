package jobs

import (
	"strings"
	"testing"
)

// BB-024: a declared float reaches the job as a float, from a string, a whole
// JSON number or a default.
func TestAFloatParamStaysAFloat(t *testing.T) {
	spec := []byte(`{"ratio":{"type":"float"},"scale":{"type":"float","default":3},"n":{"type":"int"}}`)
	for _, in := range []string{`{"ratio":"2","n":"4"}`, `{"ratio":2,"n":4}`} {
		out, err := checkParams(spec, []byte(in))
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		if !strings.Contains(s, `"ratio":2.0`) || !strings.Contains(s, `"scale":3.0`) || !strings.Contains(s, `"n":4`) || strings.Contains(s, `"n":4.0`) {
			t.Errorf("%s → %s", in, s)
		}
	}
}
