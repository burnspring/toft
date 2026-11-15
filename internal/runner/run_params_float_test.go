package runner

import "testing"

// DOC-3 (second 2026-11-03 bash): a float param arrives in TOFT_PARAMS as 2.0
// and its TOFT_PARAM_<NAME> said "2". Numbers are written as they arrived.
func TestAFloatParamKeepsItsDecimalPoint(t *testing.T) {
	got := flattenParams([]byte(`{"ratio":2.0,"n":3,"big":12345678901234567890,"s":"x","b":true}`))
	want := map[string]string{"ratio": "2.0", "n": "3", "big": "12345678901234567890", "s": "x", "b": "true"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
